package events

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ledgerflow/ledgerflow/internal/domain"
	"github.com/ledgerflow/ledgerflow/internal/idem"
	"github.com/ledgerflow/ledgerflow/internal/razorpay"
	"github.com/ledgerflow/ledgerflow/internal/risk"
)

// Store is the persistence surface ingestion needs (SRS NFR-007).
type Store interface {
	RecordEvent(ctx context.Context, e *domain.Event) (bool, error)
	MarkEventProcessed(ctx context.Context, id string) error
	LatestEntityTimestamp(ctx context.Context, entityID string) (*time.Time, error)

	FindOrCreateCustomerByEmail(ctx context.Context, email, contact, name string, seg domain.Segment) (*domain.Customer, error)
	UpsertCustomer(ctx context.Context, c *domain.Customer) error

	UpsertTransaction(ctx context.Context, t *domain.Transaction) error
	FindTransactionByRazorpayID(ctx context.Context, rzpID string) (*domain.Transaction, error)
	CountCustomerAttempts(ctx context.Context, customerID, orderID string) (int, error)

	UpsertInvoice(ctx context.Context, inv *domain.Invoice) error
	FindInvoiceByRazorpayID(ctx context.Context, rzpID string) (*domain.Invoice, error)
	MarkInvoicePaid(ctx context.Context, id string, amountPaid domain.Money) error

	UpsertSubscription(ctx context.Context, sub *domain.Subscription) error
	FindSubscriptionByRazorpayID(ctx context.Context, rzpID string) (*domain.Subscription, error)

	CreateCase(ctx context.Context, c *domain.RiskCase) error
	FindOpenCaseBySource(ctx context.Context, st domain.SourceType, sourceID string) (*domain.RiskCase, error)

	FindActionByExternalID(ctx context.Context, externalID string) (*domain.RecoveryAction, error)
	GetActionByIdempotencyKey(ctx context.Context, key string) (*domain.RecoveryAction, error)

	Audit(ctx context.Context, actor, entityType, entityID, caseID, eventType string, detail any) error
	IncrCounter(ctx context.Context, name string) error
}

// Settler receives confirmed inbound money so the verification service can close
// the loop. Ingestion does not settle recoveries itself: attributing money to a
// case, deciding whether it counts as recovered and banking the amount are the
// verifier's job, and splitting them keeps a webhook from writing revenue
// totals directly (SRS 6.1 stage 8, FR-050).
type Settler interface {
	OnPaymentConfirmed(ctx context.Context, sig PaymentSignal) error
}

// PaymentSignal describes money that arrived.
type PaymentSignal struct {
	// ReferenceID is the idempotency key LEDGERFLOW put on the resource, and the
	// strongest possible attribution: it names the exact action that caused this
	// payment.
	ReferenceID string
	// ResourceID is the payment link / invoice / subscription the money came
	// through; PaymentID is the payment itself.
	ResourceID     string
	PaymentID      string
	OrderID        string
	Amount         domain.Money
	At             time.Time
	SourceType     domain.SourceType
	AttributedCase string
}

// Config tunes ingestion.
type Config struct {
	// WebhookSecret is the Razorpay dashboard webhook secret, never the API key
	// secret.
	WebhookSecret string
	// MaxBodyBytes bounds a webhook body. The HTTP layer enforces this too; the
	// duplicate check here means a non-HTTP caller cannot bypass it.
	MaxBodyBytes int
	// MaxClockSkew rejects events timestamped implausibly far in the future,
	// which is a replay or a misconfigured sender rather than a real event.
	MaxClockSkew time.Duration
}

func (c Config) withDefaults() Config {
	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = 1 << 20 // 1 MiB
	}
	if c.MaxClockSkew <= 0 {
		c.MaxClockSkew = 10 * time.Minute
	}
	return c
}

// Ingestor turns inbound facts into cases.
type Ingestor struct {
	store   Store
	settler Settler
	cfg     Config
	now     func() time.Time
}

// NewIngestor builds an ingestor. settler may be nil, in which case recovery
// events update records but no outcome is banked.
func NewIngestor(s Store, settler Settler, cfg Config) *Ingestor {
	return &Ingestor{
		store:   s,
		settler: settler,
		cfg:     cfg.withDefaults(),
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// SetClock overrides the clock for deterministic tests.
func (i *Ingestor) SetClock(fn func() time.Time) { i.now = fn }

// Result reports what ingestion did with one event.
type Result struct {
	EventID       string            `json:"event_id"`
	EventType     string            `json:"event_type"`
	Accepted      bool              `json:"accepted"`
	Duplicate     bool              `json:"duplicate"`
	Stale         bool              `json:"stale"`
	Ignored       bool              `json:"ignored"`
	Reason        string            `json:"reason,omitempty"`
	Intent        Intent            `json:"intent,omitempty"`
	CaseID        string            `json:"case_id,omitempty"`
	CaseCreated   bool              `json:"case_created"`
	CaseReference string            `json:"case_reference,omitempty"`
	SourceType    domain.SourceType `json:"source_type,omitempty"`
	Signal        *PaymentSignal    `json:"signal,omitempty"`
}

// IngestWebhook is the webhook entry point. body must be the exact bytes read
// from the socket.
//
// A signature failure is persisted, not just refused: an attack or a
// misconfigured secret must be visible in the event log and the counters rather
// than disappearing into an HTTP 401 (SRS FR-002).
func (i *Ingestor) IngestWebhook(ctx context.Context, body []byte, signature, eventIDHeader string) (Result, error) {
	res := Result{}
	_ = i.store.IncrCounter(ctx, "webhooks_received")

	if len(body) == 0 {
		return res, fmt.Errorf("%w: empty webhook body", domain.ErrValidation)
	}
	if len(body) > i.cfg.MaxBodyBytes {
		return res, fmt.Errorf("%w: webhook body exceeds %d bytes", domain.ErrValidation, i.cfg.MaxBodyBytes)
	}

	if err := razorpay.VerifyWebhookSignature(body, signature, i.cfg.WebhookSecret); err != nil {
		i.recordRejected(ctx, body, eventIDHeader, err)
		return res, err
	}

	env, err := ParseEnvelope(body)
	if err != nil {
		i.recordRejected(ctx, body, eventIDHeader, err)
		return res, err
	}
	return i.ingest(ctx, env, body, eventIDHeader)
}

// recordRejected persists an event that failed verification.
//
// The external event id is namespaced with an "invalid:" prefix so a forged
// event can never occupy the id of a legitimate one and cause the real webhook
// to be discarded as a duplicate later.
func (i *Ingestor) recordRejected(ctx context.Context, body []byte, eventIDHeader string, cause error) {
	_ = i.store.IncrCounter(ctx, "webhook_signature_failures")
	ext := "invalid:" + idem.EventKey("razorpay", eventIDHeader, "unverified", "", body)
	ev := &domain.Event{
		Source:          "razorpay",
		ExternalEventID: ext,
		EventType:       "unverified",
		PayloadJSON:     nil, // deliberately not stored: unverified bytes are not treated as a payload
		SignatureValid:  false,
		RejectionReason: cause.Error(),
		ReceivedAt:      i.now(),
	}
	_, _ = i.store.RecordEvent(ctx, ev)
	_ = i.store.Audit(ctx, "webhook", "event", ev.ID, "", "webhook_rejected",
		map[string]any{"reason": cause.Error(), "bytes": len(body)})
}

// ingest handles a verified envelope.
func (i *Ingestor) ingest(ctx context.Context, env *Envelope, body []byte, eventIDHeader string) (Result, error) {
	res := Result{EventType: env.Event}
	entityID := env.EntityID()
	occurred := env.OccurredAt()

	if !occurred.IsZero() && occurred.After(i.now().Add(i.cfg.MaxClockSkew)) {
		return res, fmt.Errorf("%w: event timestamp %s is too far in the future",
			domain.ErrValidation, occurred.Format(time.RFC3339))
	}

	ev := &domain.Event{
		Source:          "razorpay",
		ExternalEventID: idem.EventKey("razorpay", eventIDHeader, env.Event, entityID, body),
		EventType:       env.Event,
		PayloadJSON:     body,
		SignatureValid:  true,
		EntityID:        entityID,
		ReceivedAt:      i.now(),
	}
	if !occurred.IsZero() {
		ev.EntityTimestamp = &occurred
	}

	created, err := i.store.RecordEvent(ctx, ev)
	if err != nil {
		return res, fmt.Errorf("record event: %w", err)
	}
	res.EventID = ev.ID
	if !created {
		// Redelivery. The unique index caught it, so no handler runs twice.
		_ = i.store.IncrCounter(ctx, "duplicate_events")
		res.Duplicate = true
		res.Reason = "event already processed"
		return res, nil
	}

	// Ordering: an event describing an older state than one already applied is
	// recorded but not acted on. Equal timestamps are allowed through, because
	// two real state changes can share a second and losing one of those is worse
	// than processing a redundant idempotent update.
	if ev.EntityTimestamp != nil && entityID != "" {
		latest, tsErr := i.store.LatestEntityTimestamp(ctx, entityID)
		if tsErr == nil && latest != nil && ev.EntityTimestamp.Before(*latest) {
			_ = i.store.MarkEventProcessed(ctx, ev.ID)
			res.Stale = true
			res.Reason = fmt.Sprintf("event is older than the last applied state (%s)",
				latest.Format(time.RFC3339))
			_ = i.store.Audit(ctx, "webhook", "event", ev.ID, "", "event_out_of_order",
				map[string]any{"entity_id": entityID, "event_at": ev.EntityTimestamp, "latest": latest})
			return res, nil
		}
	}

	class := Classify(env)
	res.Intent = class.Intent
	res.SourceType = class.SourceType

	switch class.Intent {
	case IntentIgnore:
		res.Ignored = true
		res.Reason = class.Reason
	case IntentUpdate:
		err = i.applyUpdate(ctx, env)
	case IntentRisk:
		err = i.applyRisk(ctx, env, class, &res)
	case IntentRecovery:
		err = i.applyRecovery(ctx, env, class, &res)
	}
	if err != nil {
		// The event stays unprocessed so a later replay can retry it. Marking it
		// processed here would silently drop the fact.
		return res, err
	}

	if markErr := i.store.MarkEventProcessed(ctx, ev.ID); markErr != nil {
		return res, markErr
	}
	res.Accepted = true
	return res, nil
}

// applyRisk normalizes the source record and opens a case for it.
func (i *Ingestor) applyRisk(ctx context.Context, env *Envelope, class Classification, res *Result) error {
	switch class.SourceType {
	case domain.SourcePaymentFailure:
		p := env.Payment()
		if p == nil {
			return fmt.Errorf("%w: %s carried no payment entity", domain.ErrValidation, env.Event)
		}
		cust, err := i.customerForPayment(ctx, p, class.SourceType)
		if err != nil {
			return err
		}
		txn, err := i.upsertPayment(ctx, p, cust.ID)
		if err != nil {
			return err
		}
		// A payment that actually succeeded is not at risk, whatever the event
		// name says. Trusting the status over the event type means a reordered
		// pair of webhooks cannot open a case against captured money.
		if isSuccessfulStatus(p.Status) {
			res.Ignored = true
			res.Reason = "payment status is " + p.Status
			return nil
		}
		return i.openCase(ctx, caseSeed{
			SourceType: domain.SourcePaymentFailure,
			Customer:   cust,
			SourceID:   txn.ID,
			Amount:     txn.Amount,
			Features: risk.Features{
				SourceType:          domain.SourcePaymentFailure,
				Amount:              txn.Amount,
				ErrorCode:           txn.ErrorCode,
				FailureReason:       txn.FailureReason,
				AttemptCount:        txn.AttemptCount,
				Segment:             cust.Segment,
				CustomerSuccessRate: cust.SuccessRate,
				LifetimeValue:       cust.LifetimeValue,
				TotalPayments:       cust.TotalPayments,
			},
		}, res)

	case domain.SourceInvoiceOverdue:
		inv := env.Invoice()
		if inv == nil {
			return fmt.Errorf("%w: %s carried no invoice entity", domain.ErrValidation, env.Event)
		}
		cust, err := i.store.FindOrCreateCustomerByEmail(ctx,
			inv.CustomerDetails.Email, inv.CustomerDetails.Contact, inv.CustomerDetails.Name,
			domain.SegmentB2B)
		if err != nil {
			return err
		}
		local, err := i.upsertInvoice(ctx, inv, cust.ID)
		if err != nil {
			return err
		}
		if strings.EqualFold(local.Status, "paid") {
			res.Ignored = true
			res.Reason = "invoice is already paid"
			return nil
		}
		daysOverdue := 0
		if !local.DueDate.IsZero() {
			daysOverdue = int(i.now().Sub(local.DueDate).Hours() / 24)
		}
		outstanding := local.Amount - local.AmountPaid
		if outstanding <= 0 {
			outstanding = local.Amount
		}
		return i.openCase(ctx, caseSeed{
			SourceType: domain.SourceInvoiceOverdue,
			Customer:   cust,
			SourceID:   local.ID,
			Amount:     outstanding,
			Features: risk.Features{
				SourceType:          domain.SourceInvoiceOverdue,
				Amount:              outstanding,
				DaysOverdue:         daysOverdue,
				ReminderCount:       local.ReminderCount,
				Segment:             cust.Segment,
				CustomerSuccessRate: cust.SuccessRate,
				LifetimeValue:       cust.LifetimeValue,
				TotalPayments:       cust.TotalPayments,
			},
		}, res)

	case domain.SourceSubscriptionFailure:
		sub := env.Subscription()
		if sub == nil {
			return fmt.Errorf("%w: %s carried no subscription entity", domain.ErrValidation, env.Event)
		}
		local, cust, err := i.upsertSubscription(ctx, sub, env.Payment())
		if err != nil {
			return err
		}
		return i.openCase(ctx, caseSeed{
			SourceType: domain.SourceSubscriptionFailure,
			Customer:   cust,
			SourceID:   local.ID,
			Amount:     local.Amount,
			Features: risk.Features{
				SourceType:          domain.SourceSubscriptionFailure,
				Amount:              local.Amount,
				AttemptCount:        local.FailedChargeCount,
				Segment:             cust.Segment,
				CustomerSuccessRate: cust.SuccessRate,
				LifetimeValue:       cust.LifetimeValue,
				TotalPayments:       cust.TotalPayments,
			},
		}, res)
	}
	return nil
}

// applyRecovery records inbound money and hands attribution to the settler.
func (i *Ingestor) applyRecovery(ctx context.Context, env *Envelope, class Classification, res *Result) error {
	sig := PaymentSignal{At: env.OccurredAt(), SourceType: class.SourceType}
	if sig.At.IsZero() {
		sig.At = i.now()
	}

	if link := env.PaymentLink(); link != nil {
		sig.ResourceID = link.ID
		sig.ReferenceID = link.ReferenceID
		sig.Amount = domain.Money(link.AmountPaid)
		if sig.Amount == 0 {
			sig.Amount = domain.Money(link.Amount)
		}
	}
	if inv := env.Invoice(); inv != nil {
		sig.ResourceID = firstNonEmpty(sig.ResourceID, inv.ID)
		sig.ReferenceID = firstNonEmpty(sig.ReferenceID, inv.ReferenceID)
		if sig.Amount == 0 {
			sig.Amount = domain.Money(inv.AmountPaid)
		}
		if local, err := i.store.FindInvoiceByRazorpayID(ctx, inv.ID); err == nil {
			paid := domain.Money(inv.AmountPaid)
			if paid == 0 {
				paid = domain.Money(inv.Amount)
			}
			if strings.EqualFold(inv.Status, "paid") {
				if err := i.store.MarkInvoicePaid(ctx, local.ID, paid); err != nil {
					return err
				}
			}
			sig.SourceType = domain.SourceInvoiceOverdue
		}
	}
	if sub := env.Subscription(); sub != nil {
		sig.ResourceID = firstNonEmpty(sig.ResourceID, sub.ID)
		if _, _, err := i.upsertSubscription(ctx, sub, env.Payment()); err != nil {
			return err
		}
		sig.SourceType = domain.SourceSubscriptionFailure
	}
	if p := env.Payment(); p != nil {
		sig.PaymentID = p.ID
		sig.OrderID = p.OrderID
		if sig.Amount == 0 {
			sig.Amount = domain.Money(p.Amount)
		}
		if sig.ReferenceID == "" {
			// A payment created by one of our links carries the case and key in
			// its notes, which is the fallback attribution path when the link
			// entity is not in the payload.
			sig.ReferenceID = p.Notes["idempotency_key"]
		}
		if cust, err := i.customerForPayment(ctx, p, class.SourceType); err == nil {
			if _, err := i.upsertPayment(ctx, p, cust.ID); err != nil {
				return err
			}
		}
	}

	// Attribution: the reference id names the action, the action names the case.
	// Falling back to the external resource id covers links created before a
	// restart, where the key is known only to the stored action row.
	if sig.ReferenceID != "" {
		if a, err := i.store.GetActionByIdempotencyKey(ctx, sig.ReferenceID); err == nil && a != nil {
			sig.AttributedCase = a.CaseID
		}
	}
	if sig.AttributedCase == "" && sig.ResourceID != "" {
		if a, err := i.store.FindActionByExternalID(ctx, sig.ResourceID); err == nil && a != nil {
			sig.AttributedCase = a.CaseID
		}
	}

	res.CaseID = sig.AttributedCase
	res.Signal = &sig

	if i.settler == nil {
		res.Reason = "no settler configured; payment recorded without settlement"
		return nil
	}
	if err := i.settler.OnPaymentConfirmed(ctx, sig); err != nil {
		return fmt.Errorf("settle payment %s: %w", sig.PaymentID, err)
	}
	return nil
}

// applyUpdate normalizes a record without opening or closing a case.
func (i *Ingestor) applyUpdate(ctx context.Context, env *Envelope) error {
	if inv := env.Invoice(); inv != nil {
		if local, err := i.store.FindInvoiceByRazorpayID(ctx, inv.ID); err == nil {
			local.Status = inv.Status
			local.AmountPaid = domain.Money(inv.AmountPaid)
			return i.store.UpsertInvoice(ctx, local)
		}
	}
	if sub := env.Subscription(); sub != nil {
		_, _, err := i.upsertSubscription(ctx, sub, env.Payment())
		return err
	}
	if p := env.Payment(); p != nil {
		cust, err := i.customerForPayment(ctx, p, domain.SourcePaymentFailure)
		if err != nil {
			return err
		}
		_, err = i.upsertPayment(ctx, p, cust.ID)
		return err
	}
	return nil
}

// --- normalization helpers ---

func (i *Ingestor) customerForPayment(ctx context.Context, p *PaymentEntity, st domain.SourceType) (*domain.Customer, error) {
	seg := risk.Classify(risk.SegmentInput{SourceType: st, Email: p.Email})
	cust, err := i.store.FindOrCreateCustomerByEmail(ctx, p.Email, p.Contact, nameFromPayment(p), seg)
	if err != nil {
		return nil, err
	}
	if cust.RazorpayCustomerID == "" && p.CustomerID != "" {
		cust.RazorpayCustomerID = p.CustomerID
		if err := i.store.UpsertCustomer(ctx, cust); err != nil {
			return nil, err
		}
	}
	return cust, nil
}

// upsertPayment writes the transaction record.
//
// Status regression is refused: once a payment is captured, a later-arriving
// failure event cannot mark it failed again. Ordering by timestamp already
// filters most of this, but two events inside the same second would pass that
// check, and un-capturing money is not a recoverable mistake (SRS FR-004).
func (i *Ingestor) upsertPayment(ctx context.Context, p *PaymentEntity, customerID string) (*domain.Transaction, error) {
	txn := &domain.Transaction{
		RazorpayPaymentID: p.ID,
		RazorpayOrderID:   p.OrderID,
		CustomerID:        customerID,
		Amount:            domain.Money(p.Amount),
		Currency:          defaultStr(p.Currency, "INR"),
		Status:            p.Status,
		Method:            p.Method,
		FailureReason:     firstNonEmpty(p.ErrorDescription, p.ErrorReason),
		ErrorCode:         p.ErrorCode,
		AttemptCount:      1,
		CreatedAt:         unixTime(p.CreatedAt),
	}

	if existing, err := i.store.FindTransactionByRazorpayID(ctx, p.ID); err == nil && existing != nil {
		txn.ID = existing.ID
		txn.AttemptCount = existing.AttemptCount
		if isSuccessfulStatus(existing.Status) && !isSuccessfulStatus(p.Status) {
			return existing, nil
		}
	} else if !errors.Is(err, domain.ErrNotFound) && err != nil {
		return nil, err
	} else if n, cErr := i.store.CountCustomerAttempts(ctx, customerID, p.OrderID); cErr == nil {
		// A repeat failure on the same order is a stronger risk signal than a
		// first attempt, so the attempt count comes from persisted history rather
		// than from the webhook.
		txn.AttemptCount = n + 1
	}

	if err := i.store.UpsertTransaction(ctx, txn); err != nil {
		return nil, err
	}
	return txn, nil
}

func (i *Ingestor) upsertInvoice(ctx context.Context, inv *InvoiceEntity, customerID string) (*domain.Invoice, error) {
	local := &domain.Invoice{
		RazorpayInvoiceID: inv.ID,
		CustomerID:        customerID,
		InvoiceNumber:     defaultStr(inv.InvoiceNumber, inv.ID),
		Amount:            domain.Money(inv.Amount),
		AmountPaid:        domain.Money(inv.AmountPaid),
		Status:            inv.Status,
		DueDate:           unixTime(inv.DueBy),
		CreatedAt:         unixTime(inv.CreatedAt),
	}
	if existing, err := i.store.FindInvoiceByRazorpayID(ctx, inv.ID); err == nil && existing != nil {
		local.ID = existing.ID
		local.ReminderCount = existing.ReminderCount
	}
	if local.DueDate.IsZero() {
		local.DueDate = i.now()
	}
	if err := i.store.UpsertInvoice(ctx, local); err != nil {
		return nil, err
	}
	return local, nil
}

// upsertSubscription writes the subscription and resolves its customer.
//
// The subscription entity carries no amount — that lives on the plan — so the
// charge amount is taken from the accompanying payment when present, and
// otherwise from what we already stored. An unknown amount stays zero rather
// than being estimated, which makes the planner refuse to act on the case: a
// guessed amount is worse than a stalled case (SRS 19.2).
func (i *Ingestor) upsertSubscription(ctx context.Context, sub *SubscriptionEntity,
	p *PaymentEntity) (*domain.Subscription, *domain.Customer, error) {

	local := &domain.Subscription{
		RazorpaySubscriptionID: sub.ID,
		PlanID:                 sub.PlanID,
		Status:                 sub.Status,
		CurrentEnd:             unixTime(sub.CurrentEnd),
	}

	existing, findErr := i.store.FindSubscriptionByRazorpayID(ctx, sub.ID)
	if findErr == nil && existing != nil {
		local.ID = existing.ID
		local.CustomerID = existing.CustomerID
		local.Amount = existing.Amount
		local.FailedChargeCount = existing.FailedChargeCount
	}
	if p != nil && p.Amount > 0 {
		local.Amount = domain.Money(p.Amount)
	}
	if isFailedSubscriptionStatus(sub.Status) {
		local.FailedChargeCount++
	}

	var cust *domain.Customer
	switch {
	case local.CustomerID != "":
		// Already linked; reuse the stored customer id without a lookup.
	case p != nil && (p.Email != "" || p.Contact != ""):
		c, err := i.store.FindOrCreateCustomerByEmail(ctx, p.Email, p.Contact, nameFromPayment(p),
			domain.SegmentSubscription)
		if err != nil {
			return nil, nil, err
		}
		cust = c
		local.CustomerID = c.ID
	default:
		// A subscription webhook with no payment and no local record gives us no
		// contact details. Recording it with a synthetic placeholder customer
		// would put a fake identity in front of an operator, so the event is
		// refused and stays replayable once the customer is known.
		return nil, nil, fmt.Errorf("%w: subscription %s has no resolvable customer",
			domain.ErrValidation, sub.ID)
	}

	if err := i.store.UpsertSubscription(ctx, local); err != nil {
		return nil, nil, err
	}
	if cust == nil {
		var err error
		if cust, err = i.customerByID(ctx, local.CustomerID); err != nil {
			return nil, nil, err
		}
	}
	return local, cust, nil
}

func (i *Ingestor) customerByID(ctx context.Context, id string) (*domain.Customer, error) {
	// FindOrCreateCustomerByEmail cannot look up by id, so a stored customer is
	// resolved through the record that referenced it. Any store implementing
	// GetCustomer satisfies this; the assertion keeps the narrow interface from
	// growing a method only this path needs.
	type getter interface {
		GetCustomer(ctx context.Context, id string) (*domain.Customer, error)
	}
	if g, ok := i.store.(getter); ok {
		return g.GetCustomer(ctx, id)
	}
	return &domain.Customer{ID: id, Segment: domain.SegmentSubscription}, nil
}

// --- case creation ---

type caseSeed struct {
	SourceType domain.SourceType
	Customer   *domain.Customer
	SourceID   string
	Amount     domain.Money
	Features   risk.Features
}

// openCase scores the seed and creates a case if it is genuinely at risk.
//
// Scoring happens here, deterministically, before any agent sees the case. The
// Detection Agent later annotates and may escalate this assessment, but the
// number that decides whether a case exists at all is the SRS 9.1 formula.
func (i *Ingestor) openCase(ctx context.Context, seed caseSeed, res *Result) error {
	if seed.SourceID == "" {
		return fmt.Errorf("%w: cannot open a case without a source record", domain.ErrValidation)
	}

	// An existing case for the same source record wins. The partial unique index
	// would reject the duplicate anyway; checking first keeps the audit log free
	// of constraint-violation noise for an ordinary webhook retry.
	if existing, err := i.store.FindOpenCaseBySource(ctx, seed.SourceType, seed.SourceID); err == nil && existing != nil {
		res.CaseID = existing.ID
		res.CaseReference = existing.Reference
		res.Reason = "case already open for this record"
		return nil
	}

	assessment := risk.Score(seed.Features)
	if !assessment.IsAtRisk {
		res.Ignored = true
		res.Reason = fmt.Sprintf("risk score %.3f is below the at-risk threshold", assessment.RiskScore)
		return nil
	}

	c := &domain.RiskCase{
		SourceType:    seed.SourceType,
		CustomerID:    seed.Customer.ID,
		RevenueAtRisk: assessment.RevenueAtRisk,
		RiskScore:     assessment.RiskScore,
		Urgency:       assessment.Urgency,
		Status:        domain.StatusNew,
		ReasonCodes:   assessment.ReasonCodes,
		EvidenceRefs:  assessment.EvidenceRefs,
		Mode:          domain.ModeLiveTest,
	}
	switch seed.SourceType {
	case domain.SourcePaymentFailure:
		c.TransactionID = seed.SourceID
	case domain.SourceCheckoutAbandonment:
		c.CheckoutSessionID = seed.SourceID
	case domain.SourceInvoiceOverdue:
		c.InvoiceID = seed.SourceID
	case domain.SourceSubscriptionFailure:
		c.SubscriptionID = seed.SourceID
	}

	if err := i.store.CreateCase(ctx, c); err != nil {
		if errors.Is(err, domain.ErrDuplicateEvent) {
			// Two ingests raced. The constraint decided the winner; this one
			// attaches to the existing case.
			if existing, findErr := i.store.FindOpenCaseBySource(ctx, seed.SourceType, seed.SourceID); findErr == nil {
				res.CaseID = existing.ID
				res.CaseReference = existing.Reference
				res.Reason = "case already open for this record"
				return nil
			}
		}
		return err
	}

	res.CaseID = c.ID
	res.CaseReference = c.Reference
	res.CaseCreated = true
	_ = i.store.Audit(ctx, "ingestor", "case", c.ID, c.ID, "case_opened", map[string]any{
		"source_type":     string(seed.SourceType),
		"revenue_at_risk": int64(c.RevenueAtRisk),
		"risk_score":      c.RiskScore,
		"urgency":         string(c.Urgency),
		"reason_codes":    c.ReasonCodes,
	})
	return nil
}

// --- small helpers ---

func isSuccessfulStatus(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "captured", "authorized", "paid", "refunded":
		return true
	}
	return false
}

func isFailedSubscriptionStatus(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "halted", "pending":
		return true
	}
	return false
}

func nameFromPayment(p *PaymentEntity) string {
	if n := strings.TrimSpace(p.Notes["name"]); n != "" {
		return n
	}
	if p.Email != "" {
		if at := strings.Index(p.Email, "@"); at > 0 {
			return p.Email[:at]
		}
	}
	if p.Contact != "" {
		return "Customer " + p.Contact
	}
	return "Unknown customer"
}

func defaultStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
