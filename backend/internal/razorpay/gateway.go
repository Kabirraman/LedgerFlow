package razorpay

import (
	"context"
	"time"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// Gateway is the payment-operations surface LEDGERFLOW depends on.
//
// Only the Action Executor holds a Gateway (SRS FR-042). The agent layer has no
// reference to it, so no model output can reach a money-moving endpoint even if
// the model asks for one.
type Gateway interface {
	// Name identifies the transport in audit records so a demo can never
	// present sandbox results as Razorpay test-mode results (SRS 25.2).
	Name() string

	// External reports whether this gateway performs network calls.
	External() bool

	// CreatePaymentLink is the primary recovery action (SRS 12.3).
	CreatePaymentLink(ctx context.Context, req PaymentLinkRequest) (*PaymentLink, error)
	FetchPaymentLink(ctx context.Context, id string) (*PaymentLink, error)

	// FindPaymentLinkByReference resolves an ambiguous create. When a create call
	// times out we hold no resource id, only the reference_id we sent — so this
	// is the only way to learn whether the link exists before attempting again
	// (SRS 20.2). A missing link returns (nil, nil), not an error: "it was never
	// created" is a legitimate answer, distinct from "the lookup failed".
	FindPaymentLinkByReference(ctx context.Context, referenceID string) (*PaymentLink, error)
	NotifyPaymentLink(ctx context.Context, id, medium string) error
	CancelPaymentLink(ctx context.Context, id string) (*PaymentLink, error)

	// FetchPayment is used for diagnosis and verification, never as a
	// collection endpoint (SRS 12.2).
	FetchPayment(ctx context.Context, id string) (*Payment, error)
	ListPayments(ctx context.Context, from, to time.Time, count int) ([]Payment, error)

	// Invoices back the B2B receivable workflow (SRS 12.4).
	CreateInvoice(ctx context.Context, req InvoiceRequest) (*Invoice, error)
	FetchInvoice(ctx context.Context, id string) (*Invoice, error)
	IssueInvoice(ctx context.Context, id string) (*Invoice, error)
	NotifyInvoice(ctx context.Context, id, medium string) error

	// Subscriptions back the recurring workflow (SRS 12.5).
	FetchSubscription(ctx context.Context, id string) (*Subscription, error)
	CancelSubscription(ctx context.Context, id string, atCycleEnd bool) (*Subscription, error)
}

// PaymentLinkRequest creates a Payment Link. Amount is in paise and is always
// taken from a trusted database record, never from model output (SRS 19.2).
type PaymentLinkRequest struct {
	Amount          domain.Money
	Currency        string
	Description     string
	ReferenceID     string // carries the idempotency key so replays collide
	CustomerName    string
	CustomerEmail   string
	CustomerContact string
	NotifyEmail     bool
	NotifySMS       bool
	// ReminderEnable asks Razorpay to send its own follow-up reminders.
	ReminderEnable bool
	ExpireBy       time.Time
	CallbackURL    string
	Notes          map[string]string
}

// PaymentLink is the normalized Payment Link resource.
type PaymentLink struct {
	ID          string            `json:"id"`
	ShortURL    string            `json:"short_url"`
	Status      string            `json:"status"` // created | partially_paid | paid | cancelled | expired
	Amount      domain.Money      `json:"amount"`
	AmountPaid  domain.Money      `json:"amount_paid"`
	Currency    string            `json:"currency"`
	ReferenceID string            `json:"reference_id"`
	Description string            `json:"description"`
	CreatedAt   time.Time         `json:"created_at"`
	Notes       map[string]string `json:"notes,omitempty"`
}

// Paid reports whether the link has been settled in full.
func (p *PaymentLink) Paid() bool { return p != nil && p.Status == "paid" }

// Payment is the normalized payment resource.
type Payment struct {
	ID               string       `json:"id"`
	OrderID          string       `json:"order_id"`
	Amount           domain.Money `json:"amount"`
	Currency         string       `json:"currency"`
	Status           string       `json:"status"` // created | authorized | captured | refunded | failed
	Method           string       `json:"method"`
	Captured         bool         `json:"captured"`
	Description      string       `json:"description"`
	Email            string       `json:"email"`
	Contact          string       `json:"contact"`
	ErrorCode        string       `json:"error_code"`
	ErrorDescription string       `json:"error_description"`
	ErrorReason      string       `json:"error_reason"`
	CustomerID       string       `json:"customer_id"`
	CreatedAt        time.Time    `json:"created_at"`
}

// Succeeded reports whether money actually arrived.
func (p *Payment) Succeeded() bool {
	return p != nil && (p.Status == "captured" || p.Status == "authorized")
}

// InvoiceRequest creates or reissues an invoice.
type InvoiceRequest struct {
	Amount          domain.Money
	Currency        string
	Description     string
	ReferenceID     string
	InvoiceNumber   string
	CustomerName    string
	CustomerEmail   string
	CustomerContact string
	ExpireBy        time.Time
	SMSNotify       bool
	EmailNotify     bool
	Notes           map[string]string
}

// Invoice is the normalized invoice resource.
type Invoice struct {
	ID            string       `json:"id"`
	InvoiceNumber string       `json:"invoice_number"`
	Status        string       `json:"status"` // draft | issued | partially_paid | paid | cancelled | expired
	Amount        domain.Money `json:"amount"`
	AmountPaid    domain.Money `json:"amount_paid"`
	AmountDue     domain.Money `json:"amount_due"`
	Currency      string       `json:"currency"`
	ShortURL      string       `json:"short_url"`
	ReferenceID   string       `json:"reference_id"`
	DueBy         time.Time    `json:"due_by"`
	CreatedAt     time.Time    `json:"created_at"`
}

// Paid reports whether the invoice is settled.
func (i *Invoice) Paid() bool { return i != nil && i.Status == "paid" }

// Subscription is the normalized subscription resource.
type Subscription struct {
	ID             string       `json:"id"`
	PlanID         string       `json:"plan_id"`
	Status         string       `json:"status"` // created | authenticated | active | pending | halted | cancelled | completed
	CustomerID     string       `json:"customer_id"`
	ShortURL       string       `json:"short_url"`
	PaidCount      int          `json:"paid_count"`
	TotalCount     int          `json:"total_count"`
	RemainingCount int          `json:"remaining_count"`
	CurrentStart   time.Time    `json:"current_start"`
	CurrentEnd     time.Time    `json:"current_end"`
	ChargeAt       time.Time    `json:"charge_at"`
	Amount         domain.Money `json:"amount"`
}

// APIError is a structured Razorpay error response. Callers distinguish 4xx
// validation failures (do not retry) from 5xx transient failures (bounded
// retry) using Retryable (SRS 20.3).
type APIError struct {
	StatusCode  int    `json:"status_code"`
	Code        string `json:"code"`
	Description string `json:"description"`
	Source      string `json:"source,omitempty"`
	Step        string `json:"step,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Field       string `json:"field,omitempty"`
	// Ambiguous marks a failure where the request may have succeeded despite
	// the error (timeout, connection reset). The caller must reconcile external
	// state before acting again (SRS 20.2).
	Ambiguous bool `json:"ambiguous"`
}

func (e *APIError) Error() string {
	if e.Description != "" {
		return "razorpay: " + e.Code + ": " + e.Description
	}
	return "razorpay: request failed with status " + itoa(e.StatusCode)
}

// Retryable reports whether the call may be retried with the same idempotency
// key. 5xx and transport errors are retryable; 4xx validation errors are not
// (SRS 20.3).
func (e *APIError) Retryable() bool {
	if e.Ambiguous {
		return true
	}
	return e.StatusCode == 0 || e.StatusCode == 429 || e.StatusCode >= 500
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
