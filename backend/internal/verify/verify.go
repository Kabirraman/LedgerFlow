// Package verify closes LEDGERFLOW's loop: it establishes whether an
// intervention actually produced money, and it is the only place recovered
// revenue is banked (SRS 6.1 stage 8, FR-049, FR-050, FR-051).
//
// Two independent signals feed it, because either one alone loses money:
//
//   - An inbound payment webhook, attributed to the action that caused it. This
//     is exact and fast, and covers the ordinary case.
//   - A poller that re-reads the resource from Razorpay for actions that never
//     produced a webhook. A dropped delivery or a misconfigured endpoint must
//     not turn a real recovery into a permanent "not recovered" (SRS 20.3).
//
// Every recovery records how it was established, and only exact action-level
// attribution credits a strategy. A payment that arrives on a case with no
// attributable action closes that case honestly but earns no strategy credit —
// crediting it would let the system look effective at having done nothing, which
// is precisely the measurement dishonesty SRS 25.2 forbids.
package verify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ledgerflow/ledgerflow/internal/domain"
	"github.com/ledgerflow/ledgerflow/internal/events"
	"github.com/ledgerflow/ledgerflow/internal/razorpay"
)

// Store is the persistence surface verification needs. It is deliberately narrow:
// the verifier can read cases and actions and write outcomes, and can do nothing
// else to a case (NFR-007).
type Store interface {
	GetCase(ctx context.Context, id string) (*domain.RiskCase, error)
	GetCustomer(ctx context.Context, id string) (*domain.Customer, error)
	GetAction(ctx context.Context, id string) (*domain.RecoveryAction, error)
	GetActionByIdempotencyKey(ctx context.Context, key string) (*domain.RecoveryAction, error)
	FindActionByExternalID(ctx context.Context, externalID string) (*domain.RecoveryAction, error)
	FindOpenCaseByPaymentOrder(ctx context.Context, orderID string) (*domain.RiskCase, error)
	ListActionsAwaitingVerification(ctx context.Context, olderThan time.Duration, limit int) ([]domain.RecoveryAction, error)
	SettleRecovery(ctx context.Context, o *domain.Outcome, segment domain.Segment,
		sourceType domain.SourceType, actionType domain.ActionType) (banked bool, err error)
	RecordOutcome(ctx context.Context, o *domain.Outcome) (created bool, err error)
	UpdateCaseStatus(ctx context.Context, caseID string, to domain.CaseStatus, stopReason string) error
	MarkInvoicePaid(ctx context.Context, id string, amountPaid domain.Money) error
	Audit(ctx context.Context, actor, entityType, entityID, caseID, eventType string, detail any) error
	IncrCounter(ctx context.Context, name string) error
	AddCounter(ctx context.Context, name string, n, sum int64) error
}

// Counter names for the operational metrics endpoint (SRS 22.5). Kept as a local
// list rather than imported, so this package does not depend on the store package
// for a plain string.
const (
	counterRecoveriesVerified  = "recoveries_verified"
	counterRecoveriesOrganic   = "recoveries_organic"
	counterRecoveriesDuplicate = "duplicate_recoveries"
	counterRecoveriesFailed    = "recoveries_not_recovered"
	counterTimeToRecovery      = "time_to_recovery_seconds"
	counterVerifyPollFailures  = "verify_poll_failures"
)

// Verification sources, recorded on every outcome so analytics can separate what
// LEDGERFLOW caused from what merely happened.
const (
	// SourceLinkReference is the strongest attribution: the money arrived on a
	// resource carrying the idempotency key of a specific action.
	SourceLinkReference = "webhook:reference_id"
	// SourceResourceID attributes by Razorpay resource id.
	SourceResourceID = "webhook:resource_id"
	// SourceOrganic means the case's money arrived without any recovery action of
	// ours being traceable to it.
	SourceOrganic = "organic"
	// SourcePollLink and SourcePollInvoice mean the gateway was asked directly
	// because no webhook ever arrived.
	SourcePollLink    = "poll:payment_link"
	SourcePollInvoice = "poll:invoice"
	// SourcePollExpired means the resource reached a terminal unpaid state.
	SourcePollExpired = "poll:expired"
	// SourceDeadline means the recovery window closed with no payment.
	SourceDeadline = "poll:deadline"
)

// Config tunes verification timing.
type Config struct {
	// VerifyAfter is how long an executed action is given to produce a webhook
	// before the poller asks the gateway directly.
	VerifyAfter time.Duration
	// GiveUpAfter bounds how long an action may stay unresolved. Past this the
	// outcome is recorded as not recovered so the orchestrator can retry,
	// escalate or stop, rather than leaving the case in limbo forever
	// (SRS 14.2, 20.3).
	GiveUpAfter time.Duration
	// BatchLimit bounds one pass so a backlog cannot monopolise a worker.
	BatchLimit int
}

func (c Config) withDefaults() Config {
	if c.VerifyAfter <= 0 {
		c.VerifyAfter = 5 * time.Minute
	}
	if c.GiveUpAfter <= 0 {
		// Longer than the default 48h payment link expiry, so a link that is
		// still live is never written off as a failure.
		c.GiveUpAfter = 72 * time.Hour
	}
	if c.BatchLimit <= 0 {
		c.BatchLimit = 50
	}
	return c
}

// Verifier implements events.Settler and drives the verification poller.
type Verifier struct {
	store   Store
	gateway razorpay.Gateway
	cfg     Config
	now     func() time.Time
}

// New builds a verifier. The gateway may be nil, in which case webhook-driven
// verification still works and polling reports every action as pending rather
// than guessing.
func New(s Store, g razorpay.Gateway, cfg Config) *Verifier {
	return &Verifier{
		store:   s,
		gateway: g,
		cfg:     cfg.withDefaults(),
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// SetClock overrides the clock for deterministic tests.
func (v *Verifier) SetClock(fn func() time.Time) { v.now = fn }

// Compile-time proof that the verifier is the settler ingestion expects, so the
// two halves of the loop cannot drift apart silently.
var _ events.Settler = (*Verifier)(nil)

// attribution is the resolved answer to "what did this money pay for".
type attribution struct {
	caseID     string
	actionID   string
	actionType domain.ActionType
	source     string
}

// OnPaymentConfirmed banks a confirmed inbound payment against the case it
// resolves, if any.
//
// A payment that resolves to no case is not an error: most payments on a live
// account have nothing to do with a recovery, and treating them as failures would
// fill the log with noise and stall webhook processing.
func (v *Verifier) OnPaymentConfirmed(ctx context.Context, sig events.PaymentSignal) error {
	if sig.Amount <= 0 {
		return nil
	}

	att, err := v.attribute(ctx, sig)
	if err != nil {
		return err
	}
	if att.caseID == "" {
		return nil
	}
	return v.bank(ctx, att, sig.Amount, v.paidAt(sig))
}

func (v *Verifier) paidAt(sig events.PaymentSignal) time.Time {
	if !sig.At.IsZero() {
		return sig.At
	}
	return v.now()
}

// attribute resolves a payment to a case and, where possible, to the exact action
// that produced it.
//
// The order of attempts is the order of evidential strength, and it stops at the
// first exact match. Nothing here searches for a plausible case: an unattributable
// payment returns an empty attribution rather than being assigned to the nearest
// candidate.
func (v *Verifier) attribute(ctx context.Context, sig events.PaymentSignal) (attribution, error) {
	// 1. The reference id we put on the resource names the action outright.
	if sig.ReferenceID != "" {
		a, err := v.store.GetActionByIdempotencyKey(ctx, sig.ReferenceID)
		switch {
		case err == nil:
			return attribution{caseID: a.CaseID, actionID: a.ID, actionType: a.ActionType,
				source: SourceLinkReference}, nil
		case !errors.Is(err, domain.ErrNotFound):
			return attribution{}, fmt.Errorf("lookup action by reference: %w", err)
		}
	}

	// 2. The Razorpay resource id maps back to the action that created it.
	if sig.ResourceID != "" {
		a, err := v.store.FindActionByExternalID(ctx, sig.ResourceID)
		switch {
		case err == nil:
			return attribution{caseID: a.CaseID, actionID: a.ID, actionType: a.ActionType,
				source: SourceResourceID}, nil
		case !errors.Is(err, domain.ErrNotFound):
			return attribution{}, fmt.Errorf("lookup action by resource id: %w", err)
		}
	}

	// 3. Ingestion may already have resolved a case without an action.
	if sig.AttributedCase != "" {
		return attribution{caseID: sig.AttributedCase, source: SourceOrganic}, nil
	}

	// 4. The customer paid the original order rather than anything we sent. The
	//    case is genuinely recovered, but no action of ours earned it.
	if sig.OrderID != "" {
		c, err := v.store.FindOpenCaseByPaymentOrder(ctx, sig.OrderID)
		switch {
		case err == nil:
			return attribution{caseID: c.ID, source: SourceOrganic}, nil
		case !errors.Is(err, domain.ErrNotFound):
			return attribution{}, fmt.Errorf("lookup case by order: %w", err)
		}
	}

	return attribution{}, nil
}

// bank records a verified recovery.
//
// The recovered amount is the smaller of what arrived and what was at risk. An
// overpayment is not extra recovery: banking it would let the dashboard claim
// more recovered revenue than the leak ever contained (SRS 19.2, 25.2).
func (v *Verifier) bank(ctx context.Context, att attribution, paid domain.Money, at time.Time) error {
	if at.IsZero() {
		// The poller learns that a resource is paid but not when, because the
		// normalized resource carries no settlement timestamp. Verification time is
		// the honest stand-in, and it is what time-to-recovery is defined against
		// anyway (SRS FR-051).
		at = v.now()
	}

	c, err := v.store.GetCase(ctx, att.caseID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("load case %s: %w", att.caseID, err)
	}
	if c.Status == domain.StatusRecovered {
		// Already banked. The store's write is idempotent anyway, but stopping
		// here avoids a redundant outcome row for a redelivered webhook.
		_ = v.store.IncrCounter(ctx, counterRecoveriesDuplicate)
		return nil
	}

	amount := paid
	capped := false
	if c.RevenueAtRisk > 0 && amount > c.RevenueAtRisk {
		amount, capped = c.RevenueAtRisk, true
	}

	cust, err := v.store.GetCustomer(ctx, c.CustomerID)
	if err != nil {
		// Without the segment the strategy metric would be filed under the wrong
		// bucket, so the settlement is deferred instead. The webhook stays
		// unprocessed and is replayed; the poller would also pick this up.
		return fmt.Errorf("load customer for case %s: %w", c.ID, err)
	}

	recoveredAt := at
	out := &domain.Outcome{
		CaseID:                c.ID,
		ActionID:              att.actionID,
		Outcome:               domain.OutcomeRecovered,
		RecoveredAmount:       amount,
		RecoveredAt:           &recoveredAt,
		TimeToRecoverySeconds: secondsBetween(c.CreatedAt, at),
		VerificationSource:    att.source,
		Notes:                 recoveryNote(paid, amount, capped),
	}

	banked, err := v.store.SettleRecovery(ctx, out, cust.Segment, c.SourceType, att.actionType)
	if err != nil {
		return fmt.Errorf("settle recovery for case %s: %w", c.ID, err)
	}
	if !banked {
		_ = v.store.IncrCounter(ctx, counterRecoveriesDuplicate)
		return nil
	}

	if att.actionID != "" {
		_ = v.store.IncrCounter(ctx, counterRecoveriesVerified)
	} else {
		_ = v.store.IncrCounter(ctx, counterRecoveriesOrganic)
	}
	_ = v.store.AddCounter(ctx, counterTimeToRecovery, 1, out.TimeToRecoverySeconds)

	// Keep the local receivable consistent with the money that arrived, so the
	// overdue sweep does not chase an invoice that has been paid.
	if c.SourceType == domain.SourceInvoiceOverdue && c.InvoiceID != "" {
		if err := v.store.MarkInvoicePaid(ctx, c.InvoiceID, paid); err != nil {
			return fmt.Errorf("mark invoice %s paid: %w", c.InvoiceID, err)
		}
	}

	_ = v.store.Audit(ctx, "verifier", "case", c.ID, c.ID, "recovery_verified", map[string]any{
		"action_id":                att.actionID,
		"action_type":              string(att.actionType),
		"verification_source":      att.source,
		"amount_paid":              int64(paid),
		"amount_banked":            int64(amount),
		"revenue_at_risk":          int64(c.RevenueAtRisk),
		"time_to_recovery_seconds": out.TimeToRecoverySeconds,
	})
	return nil
}

func recoveryNote(paid, banked domain.Money, capped bool) string {
	if !capped {
		return ""
	}
	return fmt.Sprintf("payment of %d paise exceeded the %d paise at risk; banked the amount at risk",
		int64(paid), int64(banked))
}

// secondsBetween is clamped at zero. A negative interval means the gateway's
// timestamp precedes our case creation, which is a clock disagreement rather than
// a recovery that happened before the leak.
func secondsBetween(from, to time.Time) int64 {
	if from.IsZero() || to.IsZero() || !to.After(from) {
		return 0
	}
	return int64(to.Sub(from).Seconds())
}

// Report summarises one polling pass.
type Report struct {
	Examined     int      `json:"examined"`
	Recovered    int      `json:"recovered"`
	NotRecovered int      `json:"not_recovered"`
	StillPending int      `json:"still_pending"`
	Errors       []string `json:"errors,omitempty"`
}

// RunOnce polls the gateway for executed actions that never produced a webhook
// (SRS FR-049).
//
// Simulated actions can never appear here: the store's query restricts itself to
// live_test mode, and the gateway is checked for externality besides. A simulation
// run therefore cannot reach a Razorpay endpoint through the verifier (SRS 23.4).
func (v *Verifier) RunOnce(ctx context.Context) (Report, error) {
	var rep Report

	actions, err := v.store.ListActionsAwaitingVerification(ctx, v.cfg.VerifyAfter, v.cfg.BatchLimit)
	if err != nil {
		return rep, fmt.Errorf("list actions awaiting verification: %w", err)
	}

	for i := range actions {
		a := actions[i]
		rep.Examined++

		if v.gateway == nil || !v.gateway.External() {
			rep.StillPending++
			continue
		}

		state, err := v.pollResource(ctx, a)
		if err != nil {
			_ = v.store.IncrCounter(ctx, counterVerifyPollFailures)
			rep.Errors = append(rep.Errors, fmt.Sprintf("action %s: %v", a.ID, err))
			_ = v.store.Audit(ctx, "verifier", "action", a.ID, a.CaseID, "verify_deferred",
				map[string]any{"error": err.Error()})
			rep.StillPending++
			continue
		}

		switch {
		case state.paid:
			att := attribution{caseID: a.CaseID, actionID: a.ID, actionType: a.ActionType, source: state.source}
			if err := v.bank(ctx, att, state.amount, state.at); err != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("action %s: %v", a.ID, err))
				continue
			}
			rep.Recovered++

		case state.dead:
			if err := v.recordNotRecovered(ctx, a, state.source, state.reason); err != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("action %s: %v", a.ID, err))
				continue
			}
			rep.NotRecovered++

		case v.expired(a):
			if err := v.recordNotRecovered(ctx, a, SourceDeadline,
				"recovery window closed with no payment"); err != nil {
				rep.Errors = append(rep.Errors, fmt.Sprintf("action %s: %v", a.ID, err))
				continue
			}
			rep.NotRecovered++

		default:
			rep.StillPending++
		}
	}
	return rep, nil
}

// resourceState is what the gateway says about one action's resource.
type resourceState struct {
	paid   bool
	amount domain.Money
	at     time.Time
	// dead means the resource reached a terminal unpaid state, so waiting longer
	// cannot change the answer.
	dead   bool
	source string
	reason string
}

// pollResource reads the resource an action created.
//
// A lookup failure is returned as an error rather than being interpreted. Leaving
// the action pending costs one more pass; reading a failed lookup as "not paid"
// would write off a recovery that may already have happened.
func (v *Verifier) pollResource(ctx context.Context, a domain.RecoveryAction) (resourceState, error) {
	if a.ExternalID == "" {
		return resourceState{dead: true, source: SourcePollExpired,
			reason: "action has no external resource to verify"}, nil
	}

	if strings.HasPrefix(a.ExternalID, "inv_") {
		inv, err := v.gateway.FetchInvoice(ctx, a.ExternalID)
		if err != nil {
			return resourceState{}, err
		}
		return invoiceState(inv), nil
	}

	link, err := v.gateway.FetchPaymentLink(ctx, a.ExternalID)
	if err != nil {
		return resourceState{}, err
	}
	return linkState(link), nil
}

func linkState(l *razorpay.PaymentLink) resourceState {
	if l == nil {
		return resourceState{dead: true, source: SourcePollExpired, reason: "payment link no longer exists"}
	}
	switch l.Status {
	case "paid":
		return resourceState{paid: true, amount: settledAmount(l.AmountPaid, l.Amount), source: SourcePollLink}
	case "cancelled", "expired":
		return resourceState{dead: true, source: SourcePollExpired,
			reason: "payment link " + l.Status + " unpaid"}
	default:
		// "created" and "partially_paid" are both still open. A partial payment is
		// not a recovery: the leak is only closed when the amount at risk arrives.
		return resourceState{}
	}
}

func invoiceState(inv *razorpay.Invoice) resourceState {
	if inv == nil {
		return resourceState{dead: true, source: SourcePollExpired, reason: "invoice no longer exists"}
	}
	switch inv.Status {
	case "paid":
		return resourceState{paid: true, amount: settledAmount(inv.AmountPaid, inv.Amount), source: SourcePollInvoice}
	case "cancelled", "expired":
		return resourceState{dead: true, source: SourcePollExpired,
			reason: "invoice " + inv.Status + " unpaid"}
	default:
		return resourceState{}
	}
}

// settledAmount prefers the reported paid amount and falls back to the resource's
// face value. A resource in state "paid" was settled in full, so a zero paid
// amount is a gap in the response rather than a free recovery — and banking zero
// would record a recovery worth nothing against a case that is now closed.
func settledAmount(paid, face domain.Money) domain.Money {
	if paid > 0 {
		return paid
	}
	return face
}

// expired reports whether an action has been open past the recovery window.
func (v *Verifier) expired(a domain.RecoveryAction) bool {
	if a.ExecutedAt == nil {
		return false
	}
	return v.now().Sub(*a.ExecutedAt) > v.cfg.GiveUpAfter
}

// recordNotRecovered writes the negative outcome and hands the case back to the
// workflow.
//
// The case moves to FAILED rather than to a terminal state, because "this attempt
// did not work" is not the same as "this money is unrecoverable": the orchestrator
// decides whether to retry, escalate or stop, under the retry limits in SRS 10.1.
func (v *Verifier) recordNotRecovered(ctx context.Context, a domain.RecoveryAction, source, reason string) error {
	out := &domain.Outcome{
		CaseID:             a.CaseID,
		ActionID:           a.ID,
		Outcome:            domain.OutcomeNotRecovered,
		VerificationSource: source,
		Notes:              reason,
	}
	if _, err := v.store.RecordOutcome(ctx, out); err != nil {
		return fmt.Errorf("record not-recovered outcome: %w", err)
	}
	_ = v.store.IncrCounter(ctx, counterRecoveriesFailed)

	if err := v.store.UpdateCaseStatus(ctx, a.CaseID, domain.StatusFailed, reason); err != nil {
		if !errors.Is(err, domain.ErrInvalidTransition) {
			return fmt.Errorf("mark case %s failed: %w", a.CaseID, err)
		}
		// The case has already moved on — a human closed it, or a later action is
		// mid-flight. The outcome is recorded either way; overriding the case
		// state from here would discard a decision made with more context.
		_ = v.store.Audit(ctx, "verifier", "case", a.CaseID, a.CaseID, "verify_transition_skipped",
			map[string]any{"attempted": string(domain.StatusFailed), "reason": err.Error()})
	}

	_ = v.store.Audit(ctx, "verifier", "action", a.ID, a.CaseID, "recovery_not_verified",
		map[string]any{"verification_source": source, "reason": reason})
	return nil
}

// VerifyCase forces verification of a single case's actions, so an operator can
// resolve one case from the UI without waiting for the next poll (SRS 15.2).
func (v *Verifier) VerifyCase(ctx context.Context, actionID string) (Report, error) {
	var rep Report
	a, err := v.store.GetAction(ctx, actionID)
	if err != nil {
		return rep, err
	}
	rep.Examined = 1

	if a.Status != domain.ActionStatusExecuted {
		rep.StillPending = 1
		return rep, nil
	}
	if v.gateway == nil || !v.gateway.External() {
		rep.StillPending = 1
		return rep, nil
	}

	state, err := v.pollResource(ctx, *a)
	if err != nil {
		_ = v.store.IncrCounter(ctx, counterVerifyPollFailures)
		return rep, err
	}
	switch {
	case state.paid:
		att := attribution{caseID: a.CaseID, actionID: a.ID, actionType: a.ActionType, source: state.source}
		if err := v.bank(ctx, att, state.amount, state.at); err != nil {
			return rep, err
		}
		rep.Recovered = 1
	case state.dead:
		if err := v.recordNotRecovered(ctx, *a, state.source, state.reason); err != nil {
			return rep, err
		}
		rep.NotRecovered = 1
	default:
		rep.StillPending = 1
	}
	return rep, nil
}

// Start runs RunOnce on a ticker until ctx is cancelled. Errors are reported
// through onError rather than stopping the loop: a transient gateway problem must
// not disable verification for the rest of the process lifetime.
func (v *Verifier) Start(ctx context.Context, every time.Duration, onError func(error)) {
	if every <= 0 {
		every = time.Minute
	}
	ticker := time.NewTicker(every)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := v.RunOnce(ctx); err != nil && onError != nil {
					onError(err)
				}
			}
		}
	}()
}
