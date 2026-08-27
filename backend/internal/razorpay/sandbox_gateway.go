package razorpay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// SandboxGateway is a deterministic in-process stand-in for the Razorpay API.
//
// It exists so the full recovery loop — action creation, verification, outcome
// recording — is demonstrable on a machine with no Razorpay credentials. It is
// NOT a simulation of recovery *outcomes*: links stay unpaid until something
// marks them paid (a webhook, or MarkPaid from the demo checkout), exactly as a
// real link would.
//
// Name() returns "sandbox" so no audit record or dashboard number can present
// sandbox activity as Razorpay test-mode activity (SRS 25.2).
type SandboxGateway struct {
	mu    sync.RWMutex
	links map[string]*PaymentLink
	// byReference maps reference_id (our idempotency key) to link id, which is
	// what makes a duplicate create request return the original resource
	// instead of a second one (SRS 20.1).
	byReference   map[string]string
	payments      map[string]*Payment
	invoices      map[string]*Invoice
	invoiceRefs   map[string]string
	subscriptions map[string]*Subscription
	notifications []Notification
	seq           int
	now           func() time.Time
}

// Notification records an outbound reminder so the UI and tests can assert it
// happened without an external messaging provider (SRS 5.2 out-of-scope:
// production omnichannel messaging).
type Notification struct {
	ResourceID string    `json:"resource_id"`
	Kind       string    `json:"kind"` // payment_link | invoice
	Medium     string    `json:"medium"`
	At         time.Time `json:"at"`
}

// NewSandboxGateway builds an empty sandbox gateway.
func NewSandboxGateway() *SandboxGateway {
	return &SandboxGateway{
		links:         map[string]*PaymentLink{},
		byReference:   map[string]string{},
		payments:      map[string]*Payment{},
		invoices:      map[string]*Invoice{},
		invoiceRefs:   map[string]string{},
		subscriptions: map[string]*Subscription{},
		now:           func() time.Time { return time.Now().UTC() },
	}
}

// SetClock overrides the time source for deterministic tests.
func (s *SandboxGateway) SetClock(fn func() time.Time) { s.now = fn }

func (s *SandboxGateway) Name() string   { return "sandbox" }
func (s *SandboxGateway) External() bool { return false }

// id builds a stable Razorpay-shaped identifier from the reference key, so the
// same logical action always yields the same id across restarts.
func (s *SandboxGateway) id(prefix, seed string) string {
	sum := sha256.Sum256([]byte(prefix + ":" + seed))
	return prefix + hex.EncodeToString(sum[:7])
}

func (s *SandboxGateway) CreatePaymentLink(ctx context.Context, req PaymentLinkRequest) (*PaymentLink, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.Amount <= 0 {
		return nil, &APIError{StatusCode: 400, Code: "BAD_REQUEST_ERROR", Description: "amount must be greater than zero", Field: "amount"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Idempotent create: an existing reference_id returns the original link.
	if req.ReferenceID != "" {
		if existing, ok := s.byReference[req.ReferenceID]; ok {
			return clone(s.links[existing]), nil
		}
	}

	s.seq++
	linkID := s.id("plink_", firstNonEmpty(req.ReferenceID, fmt.Sprintf("seq%d", s.seq)))
	link := &PaymentLink{
		ID:          linkID,
		ShortURL:    "https://rzp.io/i/" + strings.TrimPrefix(linkID, "plink_")[:8],
		Status:      "created",
		Amount:      req.Amount,
		Currency:    defaultStr(req.Currency, "INR"),
		ReferenceID: req.ReferenceID,
		Description: req.Description,
		CreatedAt:   s.now(),
		Notes:       req.Notes,
	}
	s.links[linkID] = link
	if req.ReferenceID != "" {
		s.byReference[req.ReferenceID] = linkID
	}
	if req.NotifyEmail {
		s.notifications = append(s.notifications, Notification{ResourceID: linkID, Kind: "payment_link", Medium: "email", At: s.now()})
	}
	if req.NotifySMS {
		s.notifications = append(s.notifications, Notification{ResourceID: linkID, Kind: "payment_link", Medium: "sms", At: s.now()})
	}
	return clone(link), nil
}

func (s *SandboxGateway) FetchPaymentLink(ctx context.Context, id string) (*PaymentLink, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	link, ok := s.links[id]
	if !ok {
		return nil, &APIError{StatusCode: 404, Code: "BAD_REQUEST_ERROR", Description: "payment link not found"}
	}
	return clone(link), nil
}

// FindPaymentLinkByReference mirrors the live lookup used to resolve an
// ambiguous create.
func (s *SandboxGateway) FindPaymentLinkByReference(ctx context.Context, referenceID string) (*PaymentLink, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byReference[referenceID]
	if !ok {
		return nil, nil
	}
	return clone(s.links[id]), nil
}

func (s *SandboxGateway) NotifyPaymentLink(ctx context.Context, id, medium string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.links[id]; !ok {
		return &APIError{StatusCode: 404, Code: "BAD_REQUEST_ERROR", Description: "payment link not found"}
	}
	s.notifications = append(s.notifications, Notification{ResourceID: id, Kind: "payment_link", Medium: medium, At: s.now()})
	return nil
}

func (s *SandboxGateway) CancelPaymentLink(ctx context.Context, id string) (*PaymentLink, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	link, ok := s.links[id]
	if !ok {
		return nil, &APIError{StatusCode: 404, Code: "BAD_REQUEST_ERROR", Description: "payment link not found"}
	}
	if link.Status == "paid" {
		return nil, &APIError{StatusCode: 400, Code: "BAD_REQUEST_ERROR", Description: "a paid payment link cannot be cancelled"}
	}
	link.Status = "cancelled"
	return clone(link), nil
}

// MarkPaymentLinkPaid settles a sandbox link. The demo checkout and the webhook
// simulator call this; the recovery engine never does, because a real link is
// only paid by the customer.
func (s *SandboxGateway) MarkPaymentLinkPaid(id string) (*PaymentLink, *Payment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	link, ok := s.links[id]
	if !ok {
		return nil, nil, fmt.Errorf("%w: payment link %s", domain.ErrNotFound, id)
	}
	if link.Status == "paid" {
		// Idempotent: a duplicate settlement must not double-count revenue.
		return clone(link), clone(s.payments[s.id("pay_", id)]), nil
	}
	link.Status = "paid"
	link.AmountPaid = link.Amount
	payment := &Payment{
		ID: s.id("pay_", id), Amount: link.Amount, Currency: link.Currency,
		Status: "captured", Method: "upi", Captured: true,
		Description: "LEDGERFLOW recovery via payment link", CreatedAt: s.now(),
	}
	s.payments[payment.ID] = payment
	return clone(link), clone(payment), nil
}

func (s *SandboxGateway) FetchPayment(ctx context.Context, id string) (*Payment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.payments[id]
	if !ok {
		return nil, &APIError{StatusCode: 404, Code: "BAD_REQUEST_ERROR", Description: "payment not found"}
	}
	return clone(p), nil
}

// PutPayment seeds a payment record, used by the demo data loader.
func (s *SandboxGateway) PutPayment(p Payment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.payments[p.ID] = &p
}

func (s *SandboxGateway) ListPayments(ctx context.Context, from, to time.Time, count int) ([]Payment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Payment, 0, len(s.payments))
	for _, p := range s.payments {
		if !from.IsZero() && p.CreatedAt.Before(from) {
			continue
		}
		if !to.IsZero() && p.CreatedAt.After(to) {
			continue
		}
		out = append(out, *p)
	}
	return out, nil
}

func (s *SandboxGateway) CreateInvoice(ctx context.Context, req InvoiceRequest) (*Invoice, error) {
	if req.Amount <= 0 {
		return nil, &APIError{StatusCode: 400, Code: "BAD_REQUEST_ERROR", Description: "amount must be greater than zero", Field: "amount"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.ReferenceID != "" {
		if existing, ok := s.invoiceRefs[req.ReferenceID]; ok {
			return clone(s.invoices[existing]), nil
		}
	}
	s.seq++
	invID := s.id("inv_", firstNonEmpty(req.ReferenceID, fmt.Sprintf("seq%d", s.seq)))
	inv := &Invoice{
		ID: invID, InvoiceNumber: defaultStr(req.InvoiceNumber, "INV-"+strings.TrimPrefix(invID, "inv_")[:6]),
		Status: "issued", Amount: req.Amount, AmountDue: req.Amount,
		Currency:    defaultStr(req.Currency, "INR"),
		ShortURL:    "https://rzp.io/i/" + strings.TrimPrefix(invID, "inv_")[:8],
		ReferenceID: req.ReferenceID, DueBy: req.ExpireBy, CreatedAt: s.now(),
	}
	s.invoices[invID] = inv
	if req.ReferenceID != "" {
		s.invoiceRefs[req.ReferenceID] = invID
	}
	return clone(inv), nil
}

func (s *SandboxGateway) FetchInvoice(ctx context.Context, id string) (*Invoice, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	inv, ok := s.invoices[id]
	if !ok {
		return nil, &APIError{StatusCode: 404, Code: "BAD_REQUEST_ERROR", Description: "invoice not found"}
	}
	return clone(inv), nil
}

func (s *SandboxGateway) IssueInvoice(ctx context.Context, id string) (*Invoice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv, ok := s.invoices[id]
	if !ok {
		return nil, &APIError{StatusCode: 404, Code: "BAD_REQUEST_ERROR", Description: "invoice not found"}
	}
	if inv.Status == "draft" {
		inv.Status = "issued"
	}
	return clone(inv), nil
}

func (s *SandboxGateway) NotifyInvoice(ctx context.Context, id, medium string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.invoices[id]; !ok {
		return &APIError{StatusCode: 404, Code: "BAD_REQUEST_ERROR", Description: "invoice not found"}
	}
	s.notifications = append(s.notifications, Notification{ResourceID: id, Kind: "invoice", Medium: medium, At: s.now()})
	return nil
}

// PutInvoice seeds an invoice, used by the demo data loader.
func (s *SandboxGateway) PutInvoice(inv Invoice) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invoices[inv.ID] = &inv
}

// MarkInvoicePaid settles a sandbox invoice idempotently.
func (s *SandboxGateway) MarkInvoicePaid(id string) (*Invoice, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv, ok := s.invoices[id]
	if !ok {
		return nil, fmt.Errorf("%w: invoice %s", domain.ErrNotFound, id)
	}
	if inv.Status != "paid" {
		inv.Status = "paid"
		inv.AmountPaid = inv.Amount
		inv.AmountDue = 0
	}
	return clone(inv), nil
}

func (s *SandboxGateway) FetchSubscription(ctx context.Context, id string) (*Subscription, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sub, ok := s.subscriptions[id]
	if !ok {
		return nil, &APIError{StatusCode: 404, Code: "BAD_REQUEST_ERROR", Description: "subscription not found"}
	}
	return clone(sub), nil
}

// PutSubscription seeds a subscription, used by the demo data loader.
func (s *SandboxGateway) PutSubscription(sub Subscription) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscriptions[sub.ID] = &sub
}

func (s *SandboxGateway) CancelSubscription(ctx context.Context, id string, atCycleEnd bool) (*Subscription, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sub, ok := s.subscriptions[id]
	if !ok {
		return nil, &APIError{StatusCode: 404, Code: "BAD_REQUEST_ERROR", Description: "subscription not found"}
	}
	sub.Status = "cancelled"
	return clone(sub), nil
}

// Notifications returns a copy of the outbound notification log.
func (s *SandboxGateway) Notifications() []Notification {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Notification, len(s.notifications))
	copy(out, s.notifications)
	return out
}

// SimulationGateway is the gateway installed on the simulation path. Every
// method returns ErrSimulationBoundary, so SRS AC-009 ("simulation mode cannot
// call Razorpay APIs") is enforced by the type system rather than by a runtime
// flag that could be forgotten. The simulator resolves outcomes from the
// dataset's ground-truth response curve instead.
type SimulationGateway struct{}

// NewSimulationGateway returns the boundary-enforcing gateway.
func NewSimulationGateway() *SimulationGateway { return &SimulationGateway{} }

func (SimulationGateway) Name() string   { return "simulation_boundary" }
func (SimulationGateway) External() bool { return false }

func (SimulationGateway) CreatePaymentLink(context.Context, PaymentLinkRequest) (*PaymentLink, error) {
	return nil, domain.ErrSimulationBoundary
}
func (SimulationGateway) FetchPaymentLink(context.Context, string) (*PaymentLink, error) {
	return nil, domain.ErrSimulationBoundary
}
func (SimulationGateway) FindPaymentLinkByReference(context.Context, string) (*PaymentLink, error) {
	return nil, domain.ErrSimulationBoundary
}
func (SimulationGateway) NotifyPaymentLink(context.Context, string, string) error {
	return domain.ErrSimulationBoundary
}
func (SimulationGateway) CancelPaymentLink(context.Context, string) (*PaymentLink, error) {
	return nil, domain.ErrSimulationBoundary
}
func (SimulationGateway) FetchPayment(context.Context, string) (*Payment, error) {
	return nil, domain.ErrSimulationBoundary
}
func (SimulationGateway) ListPayments(context.Context, time.Time, time.Time, int) ([]Payment, error) {
	return nil, domain.ErrSimulationBoundary
}
func (SimulationGateway) CreateInvoice(context.Context, InvoiceRequest) (*Invoice, error) {
	return nil, domain.ErrSimulationBoundary
}
func (SimulationGateway) FetchInvoice(context.Context, string) (*Invoice, error) {
	return nil, domain.ErrSimulationBoundary
}
func (SimulationGateway) IssueInvoice(context.Context, string) (*Invoice, error) {
	return nil, domain.ErrSimulationBoundary
}
func (SimulationGateway) NotifyInvoice(context.Context, string, string) error {
	return domain.ErrSimulationBoundary
}
func (SimulationGateway) FetchSubscription(context.Context, string) (*Subscription, error) {
	return nil, domain.ErrSimulationBoundary
}
func (SimulationGateway) CancelSubscription(context.Context, string, bool) (*Subscription, error) {
	return nil, domain.ErrSimulationBoundary
}

// clone returns a shallow copy so callers cannot mutate sandbox state through
// a returned pointer.
func clone[T any](in *T) *T {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
