package httpapi

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// The demonstration checkout (SRS 11.2).
//
// Razorpay emits no "cart abandoned" event, and inferring one from the absence of a
// payment would be guessing. So abandonment is a first-party signal: this checkout
// belongs to LEDGERFLOW, it records its own session state, and it tells the system
// when a shopper left.
//
// These routes write checkout intent only. They never create a transaction, never
// record a payment and never call Razorpay — a demo that could mint payment records
// would make every recovery figure derived from it worthless (SRS 25.2).

// startCheckoutRequest opens a session. The amount is in paise, matching every
// other amount in the system, so there is no unit conversion anywhere in the path.
type startCheckoutRequest struct {
	Email      string `json:"email"`
	Name       string `json:"name"`
	Contact    string `json:"contact"`
	CartAmount int64  `json:"cart_amount"`
	ItemCount  int    `json:"item_count"`
}

// maxDemoCartAmount bounds the demo cart at ₹5,00,000. High enough to exercise the
// human-approval threshold, low enough that a typo cannot produce a headline figure.
const maxDemoCartAmount = int64(50000000)

// startCheckout creates a demo checkout session.
func (s *Server) startCheckout(c *gin.Context) {
	var req startCheckoutRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		failWith(c, http.StatusBadRequest, "invalid_body", "expected a JSON object with email and cart_amount")
		return
	}

	email := strings.ToLower(strings.TrimSpace(req.Email))
	details := map[string]string{}
	if email == "" || !strings.Contains(email, "@") {
		details["email"] = "a valid email address is required"
	}
	if req.CartAmount <= 0 {
		details["cart_amount"] = "the cart amount must be a positive number of paise"
	}
	if req.CartAmount > maxDemoCartAmount {
		details["cart_amount"] = "the demo cart is capped at 50000000 paise (₹5,00,000)"
	}
	if req.ItemCount < 0 {
		details["item_count"] = "the item count cannot be negative"
	}
	if len(details) > 0 {
		failValidation(c, details)
		return
	}

	ctx := c.Request.Context()
	// A new email becomes a new customer in the "new" segment. Claiming a segment we
	// have no history for would feed the risk score a fact we invented.
	cust, err := s.deps.Store.FindOrCreateCustomerByEmail(ctx, email,
		strings.TrimSpace(req.Contact), strings.TrimSpace(req.Name), domain.SegmentNew)
	if err != nil {
		fail(c, err)
		return
	}

	session := &domain.CheckoutSession{
		CustomerID:  cust.ID,
		CartAmount:  domain.Money(req.CartAmount),
		ItemCount:   req.ItemCount,
		PageViews:   1,
		StartedAt:   s.now(),
		Status:      "active",
		Environment: domain.EnvTest,
	}
	if err := s.deps.Store.UpsertCheckoutSession(ctx, session); err != nil {
		fail(c, err)
		return
	}
	_ = s.deps.Store.Audit(ctx, actorOf(c), "checkout_session", session.ID, "", "demo_checkout_started",
		map[string]any{"customer_id": cust.ID, "cart_amount": session.CartAmount})

	created(c, gin.H{"session": session, "customer": cust})
}

// checkoutActivity records continued browsing, which raises page views and pushes
// the idle clock forward. Page views feed the customer-intent term of the risk
// score, so a shopper who lingered scores differently from one who bounced
// (SRS 9.1).
func (s *Server) checkoutActivity(c *gin.Context) {
	id, valid := pathID(c, "id")
	if !valid {
		return
	}
	ctx := c.Request.Context()

	session, err := s.deps.Store.GetCheckoutSession(ctx, id)
	if err != nil {
		fail(c, err)
		return
	}
	if session.Status != "active" {
		failWith(c, http.StatusConflict, "session_closed",
			"this checkout session is "+session.Status+" and no longer accepts activity")
		return
	}

	session.PageViews++
	session.LastActivityAt = s.now()
	if err := s.deps.Store.UpsertCheckoutSession(ctx, session); err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"session": session})
}

// abandonCheckout is the shopper leaving, and it opens a recovery case at once
// rather than waiting out the idle timer (SRS 11.2).
//
// The case is scored by the same code path as the timed sweep. What differs is only
// who noticed the abandonment, and that is not an input to the score.
func (s *Server) abandonCheckout(c *gin.Context) {
	id, valid := pathID(c, "id")
	if !valid {
		return
	}
	if s.deps.Scanner == nil {
		notConfigured(c, "checkout abandonment detection")
		return
	}
	ctx := c.Request.Context()

	session, err := s.deps.Store.GetCheckoutSession(ctx, id)
	if err != nil {
		fail(c, err)
		return
	}
	switch session.Status {
	case "converted":
		failWith(c, http.StatusConflict, "already_converted",
			"this checkout was completed and cannot be abandoned")
		return
	case "abandoned":
		// Idempotent by intent: a double-clicked "leave" button must not produce a
		// second case, and the unique index on the session pointer would refuse it
		// anyway. Reporting the existing state is more useful than an error.
		ok(c, gin.H{"session": session, "already_abandoned": true})
		return
	}

	if session.LastActivityAt.IsZero() {
		session.LastActivityAt = session.StartedAt
	}
	res, err := s.deps.Scanner.AbandonCheckout(ctx, *session)
	if err != nil {
		fail(c, err)
		return
	}
	if res.CaseID != "" {
		c.Set(caseIDKey, res.CaseID)
	}
	_ = s.deps.Store.Audit(ctx, actorOf(c), "checkout_session", id, res.CaseID, "demo_checkout_abandoned",
		map[string]any{"cart_amount": session.CartAmount, "case_created": res.CaseCreated})

	accepted(c, gin.H{
		"session_id":     id,
		"case_id":        res.CaseID,
		"case_reference": res.CaseReference,
		"case_created":   res.CaseCreated,
		"reason":         res.Reason,
	})
}

// convertCheckout is the shopper completing the purchase.
//
// It marks the session converted and nothing more. In particular it does not bank a
// recovery: whether a completed checkout counts as recovered revenue is the
// verifier's judgement, made from a real payment record with attribution to a
// specific action (SRS FR-050, 12.4). A demo button that credited itself with the
// recovery would be manufacturing the headline number.
func (s *Server) convertCheckout(c *gin.Context) {
	id, valid := pathID(c, "id")
	if !valid {
		return
	}
	ctx := c.Request.Context()

	session, err := s.deps.Store.GetCheckoutSession(ctx, id)
	if err != nil {
		fail(c, err)
		return
	}
	if session.Status == "converted" {
		ok(c, gin.H{"session": session, "already_converted": true})
		return
	}
	if err := s.deps.Store.MarkCheckoutStatus(ctx, id, "converted"); err != nil {
		fail(c, err)
		return
	}
	session.Status = "converted"
	_ = s.deps.Store.Audit(ctx, actorOf(c), "checkout_session", id, "", "demo_checkout_converted",
		map[string]any{"cart_amount": session.CartAmount})

	ok(c, gin.H{
		"session": session,
		// Stated explicitly so the demo cannot be read as having recovered money.
		"note": "the session is marked converted; recovery is only banked by the verifier from a real payment",
	})
}
