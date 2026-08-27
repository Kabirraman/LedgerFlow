package simulation

import (
	"fmt"
	"strings"
	"time"

	"github.com/ledgerflow/ledgerflow/internal/agents"
	"github.com/ledgerflow/ledgerflow/internal/domain"
	"github.com/ledgerflow/ledgerflow/internal/risk"
)

// Scenario is one benchmark case expressed as the domain records the production
// code expects, so the real Detection, Diagnosis and Planner agents and the real
// policy engine run against it unchanged. The benchmark measures shipped code,
// not a parallel implementation of it (SRS 22.3).
//
// What is absent matters as much as what is present: no Recoverable flag, no
// benchmark-best action, no response curve. Ground truth stays in the
// domain.BenchmarkCase the runner holds, and a Scenario is all a strategy ever
// receives, so a strategy cannot consult the answer key even by accident
// (SRS 17.2).
type Scenario struct {
	Case     domain.RiskCase
	Customer *domain.Customer

	// Exactly one source record is set, matching Case.SourceType.
	Transaction  *domain.Transaction
	Checkout     *domain.CheckoutSession
	Invoice      *domain.Invoice
	Subscription *domain.Subscription

	// TrustedAmount is the amount the source record vouches for. Every monetary
	// check downstream compares against this, never against model output.
	TrustedAmount domain.Money

	// Features is the SRS 9.1 feature vector for the risk scorer.
	Features risk.Features

	// HasContact reports whether the customer can be reached at all.
	HasContact bool

	// AlreadyPaid mirrors the external-state fact the policy engine's
	// already-recovered stopping rule reads.
	AlreadyPaid bool
}

// NewScenario materialises a benchmark case as of now.
//
// Ages in the dataset are relative — "twelve minutes since abandonment", "fifty
// days overdue" — so timestamps are derived here rather than stored. That is what
// makes a dataset generated once replayable indefinitely without its cases
// quietly ageing into different risk scores (SRS NFR-008).
func NewScenario(b domain.BenchmarkCase, now time.Time) *Scenario {
	created := now.Add(-time.Duration(b.AgeMinutes) * time.Minute)
	sc := &Scenario{AlreadyPaid: b.AlreadyPaid}

	sc.Customer = &domain.Customer{
		ID:            "cus_" + strings.ToLower(strings.ReplaceAll(b.ID, "-", "_")),
		Name:          "Benchmark " + b.ID,
		Segment:       b.Segment,
		LifetimeValue: b.LifetimeValue,
		SuccessRate:   b.CustomerSuccessRate,
		TotalPayments: b.TotalPayments,
		Environment:   domain.EnvTest,
		CreatedAt:     created.Add(-time.Duration(maxInt(b.RecencyDays, 0)) * 24 * time.Hour),
	}
	// A case with no contact details is a real operational condition — a
	// card-on-file customer whose email bounced, a guest checkout with a typo —
	// and the pipeline must reach "cannot deliver this" rather than sending
	// somewhere. The scenario expresses it as absent fields, not a flag, so the
	// production code path is the one being exercised.
	if !b.NoContact {
		sc.Customer.Email = strings.ToLower(strings.ReplaceAll(b.ID, "-", ".")) + "@example.test"
		sc.Customer.Contact = fmt.Sprintf("+9198%08d", hashDigits(b.ID))
	}
	sc.HasContact = sc.Customer.Email != "" || sc.Customer.Contact != ""

	sc.Case = domain.RiskCase{
		ID:         b.ID,
		Reference:  "REV-" + strings.TrimPrefix(b.ID, "SIM-"),
		SourceType: b.SourceType,
		CustomerID: sc.Customer.ID,
		// POLICY_REVIEW is the only status the policy engine reads, and it is the
		// status a case is in when the engine runs. Setting it here keeps the
		// harness from having to mutate the case between stages.
		Status:      domain.StatusPolicyReview,
		Mode:        domain.ModeSimulation,
		Environment: domain.EnvTest,
		CreatedAt:   created,
		UpdatedAt:   created,
	}

	switch b.SourceType {
	case domain.SourcePaymentFailure:
		sc.Transaction = &domain.Transaction{
			ID:                "txn_" + b.ID,
			RazorpayPaymentID: "pay_sim" + strings.TrimPrefix(b.ID, "SIM-"),
			RazorpayOrderID:   "order_sim" + strings.TrimPrefix(b.ID, "SIM-"),
			CustomerID:        sc.Customer.ID,
			Amount:            b.Amount,
			Currency:          "INR",
			Status:            orDefault(b.SourceStatus, "failed"),
			Method:            b.Method,
			FailureReason:     b.FailureReason,
			ErrorCode:         b.ErrorCode,
			AttemptCount:      b.AttemptCount,
			Environment:       domain.EnvTest,
			CreatedAt:         created,
		}
		sc.Case.TransactionID = sc.Transaction.ID
		sc.TrustedAmount = b.Amount

	case domain.SourceCheckoutAbandonment:
		abandoned := now.Add(-time.Duration(b.MinutesSinceAbandon) * time.Minute)
		sc.Checkout = &domain.CheckoutSession{
			ID:             "cko_" + b.ID,
			CustomerID:     sc.Customer.ID,
			CartAmount:     b.Amount,
			ItemCount:      1 + b.CheckoutViews/3,
			PageViews:      b.CheckoutViews,
			StartedAt:      abandoned.Add(-12 * time.Minute),
			LastActivityAt: abandoned,
			Status:         orDefault(b.SourceStatus, "abandoned"),
			Environment:    domain.EnvTest,
		}
		sc.Case.CheckoutSessionID = sc.Checkout.ID
		sc.TrustedAmount = b.Amount

	case domain.SourceInvoiceOverdue:
		sc.Invoice = &domain.Invoice{
			ID:                "inv_" + b.ID,
			RazorpayInvoiceID: "inv_sim" + strings.TrimPrefix(b.ID, "SIM-"),
			CustomerID:        sc.Customer.ID,
			InvoiceNumber:     "BENCH-" + strings.TrimPrefix(b.ID, "SIM-"),
			Amount:            b.Amount,
			AmountPaid:        b.AmountPaid,
			Status:            orDefault(b.SourceStatus, "issued"),
			DueDate:           now.Add(-time.Duration(b.DaysOverdue) * 24 * time.Hour),
			ReminderCount:     b.ReminderCount,
			Environment:       domain.EnvTest,
			CreatedAt:         created,
		}
		sc.Case.InvoiceID = sc.Invoice.ID
		// Only the unpaid balance is collectable. This mirrors the orchestrator's
		// rule exactly, and it is what stops a partially paid invoice from being
		// chased for its gross amount.
		sc.TrustedAmount = b.Amount - b.AmountPaid

	case domain.SourceSubscriptionFailure:
		sc.Subscription = &domain.Subscription{
			ID:                     "sub_" + b.ID,
			RazorpaySubscriptionID: "sub_sim" + strings.TrimPrefix(b.ID, "SIM-"),
			CustomerID:             sc.Customer.ID,
			PlanID:                 "plan_bench",
			Amount:                 b.Amount,
			Status:                 orDefault(b.SourceStatus, "halted"),
			FailedChargeCount:      b.AttemptCount,
			CurrentEnd:             now.Add(14 * 24 * time.Hour),
			Environment:            domain.EnvTest,
			CreatedAt:              created,
		}
		sc.Case.SubscriptionID = sc.Subscription.ID
		sc.TrustedAmount = b.Amount
	}

	if sc.TrustedAmount < 0 {
		sc.TrustedAmount = 0
	}
	sc.Features = features(b, sc.TrustedAmount)
	return sc
}

// features builds the SRS 9.1 feature vector from a benchmark case.
//
// The benchmark case's observable fields were defined to correspond field for
// field with risk.Features, so this is a copy rather than a derivation. That is
// deliberate: the dataset states the facts the scorer consumes, which means a
// benchmark result can be traced back to specific inputs instead of to a
// transformation nobody reads.
func features(b domain.BenchmarkCase, trusted domain.Money) risk.Features {
	f := risk.Features{
		SourceType:          b.SourceType,
		Amount:              trusted,
		ErrorCode:           b.ErrorCode,
		FailureReason:       b.FailureReason,
		AttemptCount:        b.AttemptCount,
		Segment:             b.Segment,
		CustomerSuccessRate: b.CustomerSuccessRate,
		LifetimeValue:       b.LifetimeValue,
		RecencyDays:         b.RecencyDays,
		TotalPayments:       b.TotalPayments,
		CheckoutViews:       b.CheckoutViews,
		MinutesSinceAbandon: b.MinutesSinceAbandon,
		DaysOverdue:         b.DaysOverdue,
		AgeMinutes:          b.AgeMinutes,
		PriorRecoveries:     b.PriorRecoveries,
		PriorFailedActions:  b.PriorFailedActions,
		ReminderCount:       b.ReminderCount,
	}
	return f
}

// DetectionInput builds the Detection Agent's contract for a scenario. It is the
// same contract the orchestrator builds from database rows, which is what lets
// the benchmark exercise the shipped agent rather than a copy of it.
func (sc *Scenario) DetectionInput(p domain.Policy) agents.DetectionInput {
	in := agents.DetectionInput{
		SourceType:    sc.Case.SourceType,
		Features:      sc.Features,
		CustomerName:  sc.Customer.Name,
		PolicySummary: agents.PolicySummary(p),
	}
	switch {
	case sc.Transaction != nil:
		in.FailureReason = sc.Transaction.FailureReason
		in.Method = sc.Transaction.Method
		in.PaymentStatus = sc.Transaction.Status
	case sc.Invoice != nil:
		in.InvoiceNumber = sc.Invoice.InvoiceNumber
		in.PaymentStatus = sc.Invoice.Status
	case sc.Subscription != nil:
		in.PlanID = sc.Subscription.PlanID
		in.PaymentStatus = sc.Subscription.Status
	case sc.Checkout != nil:
		in.PaymentStatus = sc.Checkout.Status
	}
	return in
}

// DiagnosisInput builds the Diagnosis Agent's contract.
//
// The failure reason reaches the agent unmodified, injection attempt and all.
// Sanitising it here would mean the benchmark tested a cleaned input the
// production path never sees, which is the opposite of what SRS 22.4 asks for.
func (sc *Scenario) DiagnosisInput(p domain.Policy, det agents.DetectionResult, st *caseState, now time.Time) agents.DiagnosisInput {
	return agents.DiagnosisInput{
		Case:                 sc.Case,
		Customer:             sc.Customer,
		Transaction:          sc.Transaction,
		Checkout:             sc.Checkout,
		Invoice:              sc.Invoice,
		Subscription:         sc.Subscription,
		DetectionReasonCodes: det.ReasonCodes,
		PriorActions:         st.priorActions,
		PriorRecoveries:      sc.Features.PriorRecoveries,
		MinConfidence:        p.MinActionConfidence,
		PolicySummary:        agents.PolicySummary(p),
		Now:                  now,
	}
}

// PlannerInput builds the Planner Agent's contract.
//
// Every count here comes from the harness's own record of what it has already
// done to this case, which is the simulated equivalent of the store queries the
// orchestrator runs. The planner cannot see how many attempts remain unless the
// harness tells it, and a strategy that respected limits it was never shown would
// be luck rather than design.
func (sc *Scenario) PlannerInput(p domain.Policy, diag agents.DiagnosisResult, st *caseState, now time.Time) agents.PlannerInput {
	return agents.PlannerInput{
		Case:                    sc.Case,
		Customer:                sc.Customer,
		Diagnosis:               diag,
		TrustedAmount:           sc.TrustedAmount,
		Policy:                  p,
		RetryCount:              st.retryCount,
		ReminderCount:           sc.Features.ReminderCount + st.reminderCount,
		CaseActionCount:         st.actionCount,
		ActionsForCustomerToday: st.actionCount,
		LastActionAt:            st.lastActionAt,
		PriorRecoveries:         sc.Features.PriorRecoveries,
		ConsecutiveAPIFailures:  0,
		Priors:                  nil,
		HasContact:              sc.HasContact,
		AlreadyPaid:             sc.AlreadyPaid,
		Mode:                    domain.ModeSimulation,
		Now:                     now,
	}
}

// orDefault lets the dataset override a record's status without every case
// having to state one.
func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// hashDigits derives stable digits for a synthetic phone number, so a case's
// contact detail is reproducible along with everything else about it.
func hashDigits(s string) int {
	n := 0
	for _, r := range s {
		if r >= '0' && r <= '9' {
			n = n*10 + int(r-'0')
		}
	}
	return n % 100000000
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
