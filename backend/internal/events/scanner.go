package events

import (
	"context"
	"fmt"
	"time"

	"github.com/ledgerflow/ledgerflow/internal/domain"
	"github.com/ledgerflow/ledgerflow/internal/risk"
)

// ScanStore is the surface the source scanners need.
type ScanStore interface {
	Store
	GetCustomer(ctx context.Context, id string) (*domain.Customer, error)
	FindAbandonedCheckouts(ctx context.Context, idleFor time.Duration, limit int) ([]domain.CheckoutSession, error)
	MarkCheckoutStatus(ctx context.Context, id, status string) error
	FindOverdueInvoices(ctx context.Context, limit int) ([]domain.Invoice, error)
}

// Scanner opens cases for revenue leaks that produce no webhook.
//
// Two of the four SRS workflows are silent: a customer who abandons a checkout
// generates no Razorpay event, and an invoice becoming overdue is the *absence*
// of a payment event rather than the presence of one. Waiting for a webhook
// would mean never detecting either, so these are found by sweeping records
// against the clock (SRS 11.2, 11.3).
type Scanner struct {
	store ScanStore
	ing   *Ingestor
	cfg   ScanConfig
	now   func() time.Time
}

// ScanConfig tunes the sweeps.
type ScanConfig struct {
	// AbandonAfter is how long a checkout session may sit idle before it counts
	// as abandoned.
	AbandonAfter time.Duration
	// GraceDays is how long past its due date an invoice is left alone. Chasing
	// an invoice the morning it falls due annoys customers who pay on terms.
	GraceDays int
	// BatchLimit bounds one sweep so a backlog cannot monopolise a worker.
	BatchLimit int
}

func (c ScanConfig) withDefaults() ScanConfig {
	if c.AbandonAfter <= 0 {
		c.AbandonAfter = 30 * time.Minute
	}
	if c.GraceDays < 0 {
		c.GraceDays = 0
	}
	if c.BatchLimit <= 0 {
		c.BatchLimit = 50
	}
	return c
}

// NewScanner builds a scanner that reuses the ingestor's case-opening path, so
// a swept case and a webhook case are scored and created identically.
func NewScanner(s ScanStore, ing *Ingestor, cfg ScanConfig) *Scanner {
	return &Scanner{
		store: s,
		ing:   ing,
		cfg:   cfg.withDefaults(),
		now:   func() time.Time { return time.Now().UTC() },
	}
}

// SetClock overrides the clock for deterministic tests.
func (s *Scanner) SetClock(fn func() time.Time) { s.now = fn }

// ScanReport summarises one sweep.
type ScanReport struct {
	Examined    int      `json:"examined"`
	CasesOpened int      `json:"cases_opened"`
	Skipped     int      `json:"skipped"`
	Errors      []string `json:"errors,omitempty"`
}

// ScanAbandonedCheckouts opens cases for idle checkout sessions.
func (s *Scanner) ScanAbandonedCheckouts(ctx context.Context) (ScanReport, error) {
	var rep ScanReport
	sessions, err := s.store.FindAbandonedCheckouts(ctx, s.cfg.AbandonAfter, s.cfg.BatchLimit)
	if err != nil {
		return rep, fmt.Errorf("find abandoned checkouts: %w", err)
	}

	for idx := range sessions {
		rep.Examined++
		res, err := s.abandon(ctx, sessions[idx])
		switch {
		case err != nil:
			rep.Errors = append(rep.Errors, fmt.Sprintf("session %s: %v", sessions[idx].ID, err))
		case res.CaseCreated:
			rep.CasesOpened++
		default:
			rep.Skipped++
		}
	}
	return rep, nil
}

// AbandonCheckout marks one session abandoned and opens its case immediately.
//
// This is what the demonstration checkout calls when the shopper leaves, so a demo
// does not have to wait out the idle timer to show the workflow (SRS 11.2). It runs
// the identical scoring path as the timed sweep — the only difference is what
// decided the session was abandoned, and that difference is recorded in the source
// of the event rather than in how the case is scored.
func (s *Scanner) AbandonCheckout(ctx context.Context, cs domain.CheckoutSession) (Result, error) {
	return s.abandon(ctx, cs)
}

// abandon marks the session and scores the case.
//
// The session is marked abandoned before the case is scored. That ordering matters:
// if case creation fails the session is still flagged, so the next sweep does not
// treat it as freshly active and start the clock again.
func (s *Scanner) abandon(ctx context.Context, cs domain.CheckoutSession) (Result, error) {
	res := Result{EventType: "checkout.abandoned", SourceType: domain.SourceCheckoutAbandonment}

	if err := s.store.MarkCheckoutStatus(ctx, cs.ID, "abandoned"); err != nil {
		return res, err
	}
	cust, err := s.store.GetCustomer(ctx, cs.CustomerID)
	if err != nil {
		return res, err
	}

	seed := caseSeed{
		SourceType: domain.SourceCheckoutAbandonment,
		Customer:   cust,
		SourceID:   cs.ID,
		Amount:     cs.CartAmount,
		Features: risk.Features{
			SourceType:          domain.SourceCheckoutAbandonment,
			Amount:              cs.CartAmount,
			CheckoutViews:       cs.PageViews,
			MinutesSinceAbandon: minutesSince(s.now(), cs.LastActivityAt),
			AgeMinutes:          minutesSince(s.now(), cs.StartedAt),
			Segment:             cust.Segment,
			CustomerSuccessRate: cust.SuccessRate,
			LifetimeValue:       cust.LifetimeValue,
			TotalPayments:       cust.TotalPayments,
		},
	}
	if err := s.ing.openCase(ctx, seed, &res); err != nil {
		return res, err
	}
	return res, nil
}

// minutesSince clamps to zero. A session whose last activity is a few milliseconds
// in the future — the demo checkout abandons a cart it just created — must not
// produce a negative age and invert the time-sensitivity term of the risk score.
func minutesSince(now, then time.Time) int {
	if then.IsZero() {
		return 0
	}
	mins := int(now.Sub(then).Minutes())
	if mins < 0 {
		return 0
	}
	return mins
}

// ScanOverdueInvoices opens cases for receivables past their due date.
func (s *Scanner) ScanOverdueInvoices(ctx context.Context) (ScanReport, error) {
	var rep ScanReport
	invoices, err := s.store.FindOverdueInvoices(ctx, s.cfg.BatchLimit)
	if err != nil {
		return rep, fmt.Errorf("find overdue invoices: %w", err)
	}

	cutoff := s.now().AddDate(0, 0, -s.cfg.GraceDays)
	for idx := range invoices {
		inv := invoices[idx]
		rep.Examined++

		if inv.DueDate.After(cutoff) {
			rep.Skipped++
			continue
		}

		cust, err := s.store.GetCustomer(ctx, inv.CustomerID)
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("invoice %s: %v", inv.ID, err))
			continue
		}

		// Only the unpaid balance is at risk. Using the full invoice amount would
		// overstate recovery for a partially paid invoice and let the dashboard
		// claim money that already arrived (SRS 19.2).
		outstanding := inv.Amount - inv.AmountPaid
		if outstanding <= 0 {
			rep.Skipped++
			continue
		}

		res := Result{}
		seed := caseSeed{
			SourceType: domain.SourceInvoiceOverdue,
			Customer:   cust,
			SourceID:   inv.ID,
			Amount:     outstanding,
			Features: risk.Features{
				SourceType:          domain.SourceInvoiceOverdue,
				Amount:              outstanding,
				DaysOverdue:         int(s.now().Sub(inv.DueDate).Hours() / 24),
				ReminderCount:       inv.ReminderCount,
				Segment:             cust.Segment,
				CustomerSuccessRate: cust.SuccessRate,
				LifetimeValue:       cust.LifetimeValue,
				TotalPayments:       cust.TotalPayments,
			},
		}
		if err := s.ing.openCase(ctx, seed, &res); err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("invoice %s: %v", inv.ID, err))
			continue
		}
		if res.CaseCreated {
			rep.CasesOpened++
		} else {
			rep.Skipped++
		}
	}
	return rep, nil
}

// RunOnce performs both sweeps and merges the reports.
func (s *Scanner) RunOnce(ctx context.Context) (ScanReport, error) {
	var merged ScanReport
	checkouts, err1 := s.ScanAbandonedCheckouts(ctx)
	invoices, err2 := s.ScanOverdueInvoices(ctx)

	for _, r := range []ScanReport{checkouts, invoices} {
		merged.Examined += r.Examined
		merged.CasesOpened += r.CasesOpened
		merged.Skipped += r.Skipped
		merged.Errors = append(merged.Errors, r.Errors...)
	}
	// Both sweeps always run: an error in one must not hide leaks found by the
	// other.
	if err1 != nil {
		return merged, err1
	}
	return merged, err2
}

// Start runs RunOnce on a ticker until ctx is cancelled.
func (s *Scanner) Start(ctx context.Context, every time.Duration, onError func(error)) {
	if every <= 0 {
		every = 2 * time.Minute
	}
	ticker := time.NewTicker(every)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := s.RunOnce(ctx); err != nil && onError != nil {
					onError(err)
				}
			}
		}
	}()
}
