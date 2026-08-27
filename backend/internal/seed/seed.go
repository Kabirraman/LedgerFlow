// Package seed loads a small test-mode dataset on first boot.
//
// A fresh deployment with an empty database shows an empty dashboard, which makes
// the system look broken rather than idle. This package fixes that by writing a
// handful of records that exercise the SRS workflows (SRS 24.2, 24.3).
//
// Two rules govern everything here:
//
//   - It seeds *records*, not outcomes. Cases are opened by the same detection and
//     scoring path a real event would take: failed payments through the ingestor's
//     backfill, abandoned checkouts and overdue invoices through the scanner. No
//     case, action, decision or recovered amount is written directly. Fabricating
//     those would put numbers on the dashboard that no part of the system actually
//     produced (SRS 25.2).
//   - It is idempotent and non-destructive. Seeding runs only when the database
//     holds no customers, so restarting the stack never duplicates data and never
//     overwrites records an operator has been working with.
//
// Everything written is tagged domain.EnvTest and every amount is synthetic. The
// API labels data from this dataset accordingly, so a demo cannot present it as
// live merchant revenue.
package seed

import (
	"context"
	"fmt"
	"time"

	"github.com/ledgerflow/ledgerflow/internal/domain"
	"github.com/ledgerflow/ledgerflow/internal/events"
	"github.com/ledgerflow/ledgerflow/internal/razorpay"
)

// Store is the persistence surface seeding needs (SRS NFR-007).
type Store interface {
	ListCustomers(ctx context.Context, limit int) ([]domain.Customer, error)
	UpsertCustomer(ctx context.Context, c *domain.Customer) error
	UpsertCheckoutSession(ctx context.Context, cs *domain.CheckoutSession) error
	UpsertInvoice(ctx context.Context, inv *domain.Invoice) error
	UpsertSubscription(ctx context.Context, sub *domain.Subscription) error
	Audit(ctx context.Context, actor, entityType, entityID, caseID, eventType string, detail any) error
}

// Ingestor is the case-opening path seeding reuses for failed payments.
type Ingestor interface {
	BackfillPayment(ctx context.Context, p razorpay.Payment) (events.Result, error)
}

// Report summarises what seeding wrote.
type Report struct {
	Skipped bool `json:"skipped"`
	// Reason explains a skip, so "nothing happened" is never silent.
	Reason        string   `json:"reason,omitempty"`
	Customers     int      `json:"customers"`
	Payments      int      `json:"failed_payments"`
	Checkouts     int      `json:"abandoned_checkouts"`
	Invoices      int      `json:"overdue_invoices"`
	Subscriptions int      `json:"subscriptions"`
	CasesOpened   int      `json:"cases_opened"`
	Errors        []string `json:"errors,omitempty"`
}

// Seeder writes the dataset.
type Seeder struct {
	store Store
	ing   Ingestor
	now   func() time.Time
}

// New builds a seeder. ing may be nil, in which case failed payments are skipped
// rather than written by some other route: there is no second way to open a
// payment-failure case, and inventing one for the demo is exactly what this
// package refuses to do.
func New(s Store, ing Ingestor) *Seeder {
	return &Seeder{store: s, ing: ing, now: func() time.Time { return time.Now().UTC() }}
}

// SetClock overrides the clock for deterministic tests.
func (s *Seeder) SetClock(fn func() time.Time) { s.now = fn }

// person is one synthetic customer and the records that hang off them.
//
// The mix is deliberate rather than random: it spans all four segments and both
// ends of the payment-reliability range, so the risk score, the diagnosis and the
// planner each see meaningfully different inputs on a fresh install. A dataset
// where every customer looked alike would demonstrate nothing.
type person struct {
	name        string
	email       string
	contact     string
	segment     domain.Segment
	successRate float64
	payments    int
	lifetime    domain.Money
}

var people = []person{
	{"Aarav Mehta", "aarav.mehta@example.com", "+919810000001", domain.SegmentHighValue, 0.94, 48, 4_85_000_00},
	{"Priya Nair", "priya.nair@example.com", "+919810000002", domain.SegmentRepeat, 0.81, 14, 62_400_00},
	{"Rohan Gupta", "rohan.gupta@example.com", "+919810000003", domain.SegmentNew, 0.00, 0, 0},
	{"Kavya Iyer", "kavya.iyer@example.com", "+919810000004", domain.SegmentRepeat, 0.55, 9, 28_900_00},
	{"Northwind Logistics Pvt Ltd", "accounts@northwind.example.com", "+919810000005", domain.SegmentB2B, 0.32, 22, 1_14_500_00},
	{"Ananya Desai", "ananya.desai@example.com", "+919810000006", domain.SegmentSubscription, 0.97, 63, 7_20_000_00},
}

// failedPayment describes one synthetic failure.
//
// The error codes are the real Razorpay vocabulary, and they are spread across
// transient and non-transient causes on purpose: a dataset of nothing but
// insufficient-funds failures would make the retry strategy look far better than
// it is, and one of nothing but hard declines would make it look useless.
type failedPayment struct {
	personIdx   int
	amount      domain.Money
	method      string
	errorCode   string
	errorReason string
	description string
	minutesAgo  int
}

var failures = []failedPayment{
	{0, 12_499_00, "card", "GATEWAY_ERROR", "payment_failed", "Order #LF-4471 — annual plan", 12},
	{1, 2_899_00, "upi", "BAD_REQUEST_ERROR", "payment_failed", "Order #LF-4472 — starter kit", 34},
	{3, 8_750_00, "netbanking", "GATEWAY_ERROR", "insufficient_funds", "Order #LF-4473 — bundle", 58},
	{4, 1_499_00, "card", "BAD_REQUEST_ERROR", "payment_declined_by_bank", "Order #LF-4474 — refill", 96},
	{5, 45_000_00, "card", "GATEWAY_ERROR", "payment_failed", "Order #LF-4475 — enterprise seat pack", 7},
}

// abandonedCart describes one idle checkout.
//
// LastActivityAt is set far enough back that the scanner's first pass finds it, so
// a fresh install shows checkout-abandonment cases without an operator having to
// wait out the idle timer. Page views vary because they drive the customer-intent
// term of the risk score (SRS 9.1).
type abandonedCart struct {
	personIdx  int
	amount     domain.Money
	items      int
	views      int
	minutesAgo int
}

var carts = []abandonedCart{
	{2, 3_299_00, 2, 7, 41},
	{1, 18_900_00, 5, 12, 73},
	{5, 96_500_00, 3, 4, 52},
}

// overdueInvoice describes one receivable past its due date.
type overdueInvoice struct {
	personIdx int
	number    string
	amount    domain.Money
	paid      domain.Money
	daysLate  int
	reminders int
}

var invoices = []overdueInvoice{
	{0, "LF-INV-2041", 1_25_000_00, 0, 9, 0},
	{4, "LF-INV-2042", 34_500_00, 10_000_00, 23, 2},
	{1, "LF-INV-2043", 7_800_00, 0, 3, 0},
}

// Run seeds the dataset if the database is empty.
func (s *Seeder) Run(ctx context.Context) (Report, error) {
	var rep Report

	existing, err := s.store.ListCustomers(ctx, 1)
	if err != nil {
		return rep, fmt.Errorf("check for existing data: %w", err)
	}
	if len(existing) > 0 {
		rep.Skipped = true
		rep.Reason = "the database already holds customer records"
		return rep, nil
	}

	now := s.now()

	// Customers first: every other record references one.
	ids := make([]string, 0, len(people))
	for _, p := range people {
		c := &domain.Customer{
			Name:          p.name,
			Email:         p.email,
			Contact:       p.contact,
			Segment:       p.segment,
			LifetimeValue: p.lifetime,
			SuccessRate:   p.successRate,
			TotalPayments: p.payments,
			Environment:   domain.EnvTest,
			CreatedAt:     now.AddDate(0, 0, -90),
		}
		if err := s.store.UpsertCustomer(ctx, c); err != nil {
			return rep, fmt.Errorf("seed customer %s: %w", p.email, err)
		}
		ids = append(ids, c.ID)
		rep.Customers++
	}

	// Failed payments, through the ingestor's backfill path. The payment ids carry
	// an "rzp_test" shape so nothing in the dataset can be mistaken for a live
	// Razorpay record, and the ingestor scores and opens each case exactly as it
	// would for one fetched from the API.
	if s.ing != nil {
		for idx, f := range failures {
			p := people[f.personIdx]
			pay := razorpay.Payment{
				ID:               fmt.Sprintf("pay_TESTSEED%04d", idx+1),
				OrderID:          fmt.Sprintf("order_TESTSEED%04d", idx+1),
				Amount:           f.amount,
				Currency:         "INR",
				Status:           "failed",
				Method:           f.method,
				Description:      f.description,
				Email:            p.email,
				Contact:          p.contact,
				ErrorCode:        f.errorCode,
				ErrorReason:      f.errorReason,
				ErrorDescription: "seeded demonstration failure (synthetic, test mode)",
				CreatedAt:        now.Add(-time.Duration(f.minutesAgo) * time.Minute),
			}
			res, err := s.ing.BackfillPayment(ctx, pay)
			if err != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("payment %s: %v", pay.ID, err))
				continue
			}
			rep.Payments++
			if res.CaseCreated {
				rep.CasesOpened++
			}
		}
	} else {
		rep.Errors = append(rep.Errors,
			"failed-payment cases were not seeded: no ingestor was supplied")
	}

	// Abandoned checkouts. Left in "active" status with a stale last-activity time
	// so the scanner discovers them, rather than pre-marking them abandoned: the
	// detection step is part of what the demo is meant to show.
	for _, cart := range carts {
		started := now.Add(-time.Duration(cart.minutesAgo+15) * time.Minute)
		cs := &domain.CheckoutSession{
			CustomerID:     ids[cart.personIdx],
			CartAmount:     cart.amount,
			ItemCount:      cart.items,
			PageViews:      cart.views,
			StartedAt:      started,
			LastActivityAt: now.Add(-time.Duration(cart.minutesAgo) * time.Minute),
			Status:         "active",
			Environment:    domain.EnvTest,
		}
		if err := s.store.UpsertCheckoutSession(ctx, cs); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("checkout for %s: %v", cs.CustomerID, err))
			continue
		}
		rep.Checkouts++
	}

	// Overdue receivables, likewise found by the scanner. One is partly paid, which
	// is the case that catches an implementation billing the full invoice amount
	// instead of the outstanding balance (SRS 11.3).
	for _, inv := range invoices {
		record := &domain.Invoice{
			CustomerID:    ids[inv.personIdx],
			InvoiceNumber: inv.number,
			Amount:        inv.amount,
			AmountPaid:    inv.paid,
			Status:        "issued",
			DueDate:       now.AddDate(0, 0, -inv.daysLate),
			ReminderCount: inv.reminders,
			Environment:   domain.EnvTest,
			CreatedAt:     now.AddDate(0, 0, -inv.daysLate-30),
		}
		if err := s.store.UpsertInvoice(ctx, record); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("invoice %s: %v", inv.number, err))
			continue
		}
		rep.Invoices++
	}

	// One subscription with a failed charge (SRS 11.4).
	//
	// No case is opened for it here. A subscription case originates from a
	// subscription.charged failure webhook, and there is no polling path that could
	// find one — so seeding a case would mean writing a case no detector produced.
	// The record exists so the workflow has data the moment a test webhook arrives.
	sub := &domain.Subscription{
		CustomerID:        ids[5],
		PlanID:            "plan_TESTSEED0001",
		Amount:            4_999_00,
		Status:            "halted",
		FailedChargeCount: 1,
		CurrentEnd:        now.AddDate(0, 0, 6),
		Environment:       domain.EnvTest,
		CreatedAt:         now.AddDate(0, -7, 0),
	}
	if err := s.store.UpsertSubscription(ctx, sub); err != nil {
		rep.Errors = append(rep.Errors, fmt.Sprintf("subscription: %v", err))
	} else {
		rep.Subscriptions++
	}

	_ = s.store.Audit(ctx, "system", "seed", "", "", "demo_data_seeded",
		map[string]any{"report": rep, "label": "synthetic test-mode dataset"})

	return rep, nil
}
