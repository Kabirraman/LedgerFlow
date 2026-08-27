// Package events ingests the facts LEDGERFLOW reacts to: Razorpay webhooks,
// first-party checkout activity, and periodic sweeps of records that go stale
// without any event at all (SRS 6.1, 11.1-11.4).
//
// Three rules govern everything in this package:
//
//   - The signature is verified against the raw request bytes before the body is
//     parsed. Re-serialising JSON changes the bytes the HMAC covers, so parsing
//     first would make verification meaningless (SRS 19.3).
//   - Deduplication is a database constraint, not a cache. A redelivered webhook
//     collides on events.external_event_id and is discarded (SRS FR-003).
//   - Normalization only ever advances state. An out-of-order or regressive
//     event is recorded and ignored rather than allowed to un-capture a payment
//     (SRS FR-004).
package events

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// Envelope is the Razorpay webhook wrapper (SRS 12.6).
type Envelope struct {
	Entity    string              `json:"entity"`
	AccountID string              `json:"account_id"`
	Event     string              `json:"event"`
	Contains  []string            `json:"contains"`
	CreatedAt int64               `json:"created_at"`
	Payload   map[string]entityOf `json:"payload"`
}

// entityOf unwraps the {"entity": {...}} nesting Razorpay uses for each
// resource in a webhook payload.
type entityOf struct {
	Entity json.RawMessage `json:"entity"`
}

// PaymentEntity is the subset of Razorpay's payment object LEDGERFLOW stores.
//
// Deliberately absent: card fields, VPA, bank account details. Storing a payment
// instrument would put the application database in scope for card-data handling
// for no recovery benefit (SRS 19.1 data minimization).
type PaymentEntity struct {
	ID               string            `json:"id"`
	OrderID          string            `json:"order_id"`
	InvoiceID        string            `json:"invoice_id"`
	Amount           int64             `json:"amount"`
	Currency         string            `json:"currency"`
	Status           string            `json:"status"`
	Method           string            `json:"method"`
	Captured         bool              `json:"captured"`
	Description      string            `json:"description"`
	Email            string            `json:"email"`
	Contact          string            `json:"contact"`
	CustomerID       string            `json:"customer_id"`
	ErrorCode        string            `json:"error_code"`
	ErrorDescription string            `json:"error_description"`
	ErrorReason      string            `json:"error_reason"`
	ErrorSource      string            `json:"error_source"`
	ErrorStep        string            `json:"error_step"`
	CreatedAt        int64             `json:"created_at"`
	Notes            map[string]string `json:"notes"`
}

// PaymentLinkEntity is the payment link object. ReferenceID carries the
// idempotency key LEDGERFLOW sent, which is how a paid link is traced back to
// the action that created it (SRS 20.1).
type PaymentLinkEntity struct {
	ID          string            `json:"id"`
	ShortURL    string            `json:"short_url"`
	Status      string            `json:"status"`
	Amount      int64             `json:"amount"`
	AmountPaid  int64             `json:"amount_paid"`
	Currency    string            `json:"currency"`
	ReferenceID string            `json:"reference_id"`
	Description string            `json:"description"`
	CreatedAt   int64             `json:"created_at"`
	Notes       map[string]string `json:"notes"`
}

// InvoiceEntity is the invoice object.
type InvoiceEntity struct {
	ID              string `json:"id"`
	InvoiceNumber   string `json:"invoice_number"`
	Status          string `json:"status"`
	Amount          int64  `json:"amount"`
	AmountPaid      int64  `json:"amount_paid"`
	AmountDue       int64  `json:"amount_due"`
	Currency        string `json:"currency"`
	ReferenceID     string `json:"reference_id"`
	DueBy           int64  `json:"due_by"`
	CreatedAt       int64  `json:"created_at"`
	CustomerDetails struct {
		Name    string `json:"name"`
		Email   string `json:"email"`
		Contact string `json:"contact"`
	} `json:"customer_details"`
	Notes map[string]string `json:"notes"`
}

// SubscriptionEntity is the subscription object.
type SubscriptionEntity struct {
	ID             string            `json:"id"`
	PlanID         string            `json:"plan_id"`
	CustomerID     string            `json:"customer_id"`
	Status         string            `json:"status"`
	PaidCount      int               `json:"paid_count"`
	RemainingCount int               `json:"remaining_count"`
	CurrentStart   int64             `json:"current_start"`
	CurrentEnd     int64             `json:"current_end"`
	ChargeAt       int64             `json:"charge_at"`
	Notes          map[string]string `json:"notes"`
}

// ParseEnvelope decodes a verified webhook body.
//
// It is only called after the signature check passes. An unverified body is
// stored as opaque bytes and never parsed, because parsing attacker-controlled
// structure is exactly the step a forged webhook is trying to reach.
func ParseEnvelope(body []byte) (*Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("%w: webhook body is not valid JSON: %v", domain.ErrValidation, err)
	}
	if strings.TrimSpace(env.Event) == "" {
		return nil, fmt.Errorf("%w: webhook has no event type", domain.ErrValidation)
	}
	return &env, nil
}

// Payment extracts the payment entity, if the payload carries one.
func (e *Envelope) Payment() *PaymentEntity {
	var p PaymentEntity
	if !e.decode("payment", &p) || p.ID == "" {
		return nil
	}
	return &p
}

// PaymentLink extracts the payment link entity, if present.
func (e *Envelope) PaymentLink() *PaymentLinkEntity {
	var l PaymentLinkEntity
	if !e.decode("payment_link", &l) || l.ID == "" {
		return nil
	}
	return &l
}

// Invoice extracts the invoice entity, if present.
func (e *Envelope) Invoice() *InvoiceEntity {
	var inv InvoiceEntity
	if !e.decode("invoice", &inv) || inv.ID == "" {
		return nil
	}
	return &inv
}

// Subscription extracts the subscription entity, if present.
func (e *Envelope) Subscription() *SubscriptionEntity {
	var sub SubscriptionEntity
	if !e.decode("subscription", &sub) || sub.ID == "" {
		return nil
	}
	return &sub
}

func (e *Envelope) decode(key string, out any) bool {
	if e == nil || e.Payload == nil {
		return false
	}
	wrapper, ok := e.Payload[key]
	if !ok || len(wrapper.Entity) == 0 {
		return false
	}
	return json.Unmarshal(wrapper.Entity, out) == nil
}

// EntityID returns the primary resource id the event concerns, which is the key
// ordering is enforced against.
func (e *Envelope) EntityID() string {
	if l := e.PaymentLink(); l != nil {
		return l.ID
	}
	if inv := e.Invoice(); inv != nil {
		return inv.ID
	}
	if sub := e.Subscription(); sub != nil {
		return sub.ID
	}
	if p := e.Payment(); p != nil {
		return p.ID
	}
	return ""
}

// OccurredAt is the moment the state change happened.
//
// The *event* timestamp is used rather than the entity's created_at, because a
// payment's created_at never changes: payment.authorized and payment.captured
// for the same payment carry an identical entity timestamp, so ordering on it
// would discard the capture (SRS FR-004).
func (e *Envelope) OccurredAt() time.Time {
	if e.CreatedAt > 0 {
		return time.Unix(e.CreatedAt, 0).UTC()
	}
	return time.Time{}
}

// Intent is what ingestion should do with an event.
type Intent string

const (
	// IntentRisk means the event reveals revenue at risk and should open a case.
	IntentRisk Intent = "risk"
	// IntentRecovery means money arrived and an open case may be recoverable.
	IntentRecovery Intent = "recovery"
	// IntentUpdate means the event changes a record but neither opens nor closes
	// a case (a link expiring, an invoice being issued).
	IntentUpdate Intent = "update"
	// IntentIgnore means the event is not part of any LEDGERFLOW workflow.
	IntentIgnore Intent = "ignore"
)

// Classification is the routing decision for one event.
type Classification struct {
	Intent     Intent
	SourceType domain.SourceType
	// Reason explains an ignore decision in the audit trail.
	Reason string
}

// Classify maps an event type onto a workflow.
//
// The mapping is explicit and closed: an unrecognised event type is ignored
// rather than guessed at. Razorpay adds event types over time, and an
// unrecognised one must be inert rather than partially handled.
func Classify(env *Envelope) Classification {
	switch env.Event {
	// --- risk-revealing events ---
	case "payment.failed":
		return Classification{Intent: IntentRisk, SourceType: domain.SourcePaymentFailure}
	case "subscription.halted", "subscription.pending":
		return Classification{Intent: IntentRisk, SourceType: domain.SourceSubscriptionFailure}
	case "invoice.expired":
		return Classification{Intent: IntentRisk, SourceType: domain.SourceInvoiceOverdue}

	// --- recovery events ---
	case "payment.captured", "payment.authorized", "order.paid":
		return Classification{Intent: IntentRecovery, SourceType: domain.SourcePaymentFailure}
	case "payment_link.paid":
		return Classification{Intent: IntentRecovery}
	case "invoice.paid":
		return Classification{Intent: IntentRecovery, SourceType: domain.SourceInvoiceOverdue}
	case "subscription.charged":
		return Classification{Intent: IntentRecovery, SourceType: domain.SourceSubscriptionFailure}

	// --- record updates with no case effect ---
	case "payment_link.partially_paid", "payment_link.expired", "payment_link.cancelled",
		"invoice.partially_paid", "invoice.issued",
		"subscription.activated", "subscription.cancelled", "subscription.completed",
		"subscription.updated", "subscription.authenticated":
		return Classification{Intent: IntentUpdate}

	default:
		return Classification{Intent: IntentIgnore, Reason: "event type is not part of a recovery workflow"}
	}
}

func unixTime(sec int64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}
