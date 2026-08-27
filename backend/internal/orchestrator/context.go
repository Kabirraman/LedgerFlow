package orchestrator

import (
	"context"
	"fmt"
	"time"

	"github.com/ledgerflow/ledgerflow/internal/agents"
	"github.com/ledgerflow/ledgerflow/internal/domain"
	"github.com/ledgerflow/ledgerflow/internal/risk"
	"github.com/ledgerflow/ledgerflow/internal/store"
)

// caseContext is every trusted fact the pipeline needs about one case, loaded in
// one place so the four agents, the policy engine and the executor all reason
// over the same snapshot.
//
// Nothing in here is derived from model output. That is the point: the agents
// receive this, and whatever they return is validated back against it (SRS 19.2).
type caseContext struct {
	Case     domain.RiskCase
	Customer *domain.Customer

	// Exactly one source record is set, matching Case.SourceType.
	Transaction  *domain.Transaction
	Checkout     *domain.CheckoutSession
	Invoice      *domain.Invoice
	Subscription *domain.Subscription

	// TrustedAmount is the money actually at stake according to the source
	// record. Every downstream amount check compares against this value, and the
	// executor rejects any action whose amount differs from it (SRS 22.4).
	TrustedAmount domain.Money

	PriorActions []domain.RecoveryAction
	History      store.CustomerHistory
	Policy       domain.Policy
	Priors       map[string]float64
	Facts        store.PolicyFacts
	Features     risk.Features

	// HasContact reports whether the customer can be reached at all. A recovery
	// action with nowhere to send it is not an intervention.
	HasContact bool
}

// load assembles the snapshot for one case.
//
// A missing source record is a hard error rather than a defaulted zero: acting on
// a case whose underlying payment record cannot be read would mean acting on an
// amount nobody can vouch for.
func (o *Orchestrator) load(ctx context.Context, c domain.RiskCase) (*caseContext, error) {
	cc := &caseContext{Case: c}

	cust, err := o.store.GetCustomer(ctx, c.CustomerID)
	if err != nil {
		return nil, fmt.Errorf("load customer for case %s: %w", c.ID, err)
	}
	cc.Customer = cust
	cc.HasContact = cust.Email != "" || cust.Contact != ""

	if err := o.loadSource(ctx, cc); err != nil {
		return nil, err
	}

	if cc.PriorActions, err = o.store.ListActionsForCase(ctx, c.ID); err != nil {
		return nil, fmt.Errorf("load actions for case %s: %w", c.ID, err)
	}
	if cc.History, err = o.store.LoadCustomerHistory(ctx, c.CustomerID); err != nil {
		return nil, err
	}
	if cc.Priors, err = o.store.StrategyPriors(ctx); err != nil {
		return nil, fmt.Errorf("load strategy priors: %w", err)
	}
	cc.Policy = o.store.ActivePolicyOrDefault(ctx)

	// The decision id is only known after planning, so the approval fact is
	// loaded again at policy time with the real id. Everything else is stable.
	decisionID := ""
	if d, err := o.store.LatestDecision(ctx, c.ID); err == nil {
		decisionID = d.ID
	}
	if cc.Facts, err = o.store.LoadPolicyFacts(ctx, c.ID, c.CustomerID, decisionID); err != nil {
		return nil, err
	}

	cc.Features = cc.features(o.now())
	return cc, nil
}

func (o *Orchestrator) loadSource(ctx context.Context, cc *caseContext) error {
	c := cc.Case
	var err error
	switch c.SourceType {
	case domain.SourcePaymentFailure:
		if c.TransactionID == "" {
			return fmt.Errorf("%w: case %s has no transaction", domain.ErrValidation, c.ID)
		}
		if cc.Transaction, err = o.store.GetTransaction(ctx, c.TransactionID); err != nil {
			return fmt.Errorf("load transaction for case %s: %w", c.ID, err)
		}
		cc.TrustedAmount = cc.Transaction.Amount

	case domain.SourceCheckoutAbandonment:
		if c.CheckoutSessionID == "" {
			return fmt.Errorf("%w: case %s has no checkout session", domain.ErrValidation, c.ID)
		}
		if cc.Checkout, err = o.store.GetCheckoutSession(ctx, c.CheckoutSessionID); err != nil {
			return fmt.Errorf("load checkout session for case %s: %w", c.ID, err)
		}
		cc.TrustedAmount = cc.Checkout.CartAmount

	case domain.SourceInvoiceOverdue:
		if c.InvoiceID == "" {
			return fmt.Errorf("%w: case %s has no invoice", domain.ErrValidation, c.ID)
		}
		if cc.Invoice, err = o.store.GetInvoice(ctx, c.InvoiceID); err != nil {
			return fmt.Errorf("load invoice for case %s: %w", c.ID, err)
		}
		// Only the unpaid balance is collectable. Chasing the gross amount of a
		// partially paid invoice would demand money that already arrived.
		cc.TrustedAmount = cc.Invoice.Amount - cc.Invoice.AmountPaid

	case domain.SourceSubscriptionFailure:
		if c.SubscriptionID == "" {
			return fmt.Errorf("%w: case %s has no subscription", domain.ErrValidation, c.ID)
		}
		if cc.Subscription, err = o.store.GetSubscription(ctx, c.SubscriptionID); err != nil {
			return fmt.Errorf("load subscription for case %s: %w", c.ID, err)
		}
		cc.TrustedAmount = cc.Subscription.Amount

	default:
		return fmt.Errorf("%w: case %s has unknown source type %q", domain.ErrValidation, c.ID, c.SourceType)
	}

	if cc.TrustedAmount < 0 {
		cc.TrustedAmount = 0
	}
	return nil
}

// features rebuilds the SRS 9.1 feature vector from the loaded snapshot.
//
// Ingestion scored the case once with the facts available at the time. Re-deriving
// the features here means a case that has since accumulated failed actions, or
// whose customer has since recovered elsewhere, is scored on what is true now.
func (cc *caseContext) features(now time.Time) risk.Features {
	f := risk.Features{
		SourceType:          cc.Case.SourceType,
		Amount:              cc.TrustedAmount,
		Segment:             cc.Customer.Segment,
		CustomerSuccessRate: cc.Customer.SuccessRate,
		LifetimeValue:       cc.Customer.LifetimeValue,
		TotalPayments:       cc.Customer.TotalPayments,
		RecencyDays:         recencyDays(cc.History.LastPaymentAt, now),
		PriorRecoveries:     cc.History.Recoveries,
		PriorFailedActions:  countFailedActions(cc.PriorActions),
		AgeMinutes:          int(now.Sub(cc.Case.CreatedAt).Minutes()),
	}

	switch {
	case cc.Transaction != nil:
		f.ErrorCode = cc.Transaction.ErrorCode
		f.FailureReason = cc.Transaction.FailureReason
		f.AttemptCount = cc.Transaction.AttemptCount
	case cc.Checkout != nil:
		f.CheckoutViews = cc.Checkout.PageViews
		f.MinutesSinceAbandon = int(now.Sub(cc.Checkout.LastActivityAt).Minutes())
	case cc.Invoice != nil:
		f.DaysOverdue = int(now.Sub(cc.Invoice.DueDate).Hours() / 24)
		f.ReminderCount = cc.Invoice.ReminderCount
	case cc.Subscription != nil:
		f.AttemptCount = cc.Subscription.FailedChargeCount
	}
	return f
}

// recencyDays returns -1 when there is no payment on record.
//
// Negative means "unknown", which the scorer treats as neither recent nor stale.
// Returning a large number instead would silently punish every new customer for
// having no history, and returning zero would flatter them.
func recencyDays(last *time.Time, now time.Time) int {
	if last == nil || last.IsZero() {
		return -1
	}
	d := int(now.Sub(*last).Hours() / 24)
	if d < 0 {
		return 0
	}
	return d
}

func countFailedActions(actions []domain.RecoveryAction) int {
	n := 0
	for _, a := range actions {
		if a.Status == domain.ActionStatusFailed || a.Status == domain.ActionStatusAmbiguous {
			n++
		}
	}
	return n
}

func countActionsOfType(actions []domain.RecoveryAction, t domain.ActionType) int {
	n := 0
	for _, a := range actions {
		if a.ActionType != t {
			continue
		}
		if a.Status == domain.ActionStatusExecuted || a.Status == domain.ActionStatusAmbiguous {
			n++
		}
	}
	return n
}

// detectionInput builds the Detection Agent's contract (SRS 8.1).
func (cc *caseContext) detectionInput() agents.DetectionInput {
	in := agents.DetectionInput{
		SourceType:    cc.Case.SourceType,
		Features:      cc.Features,
		CustomerName:  cc.Customer.Name,
		PolicySummary: agents.PolicySummary(cc.Policy),
	}
	switch {
	case cc.Transaction != nil:
		in.FailureReason = cc.Transaction.FailureReason
		in.Method = cc.Transaction.Method
		in.PaymentStatus = cc.Transaction.Status
	case cc.Invoice != nil:
		in.InvoiceNumber = cc.Invoice.InvoiceNumber
		in.PaymentStatus = cc.Invoice.Status
	case cc.Subscription != nil:
		in.PlanID = cc.Subscription.PlanID
		in.PaymentStatus = cc.Subscription.Status
	case cc.Checkout != nil:
		in.PaymentStatus = cc.Checkout.Status
	}
	return in
}

// diagnosisInput builds the Diagnosis Agent's contract (SRS 8.2).
func (cc *caseContext) diagnosisInput(now time.Time) agents.DiagnosisInput {
	return agents.DiagnosisInput{
		Case:                 cc.Case,
		Customer:             cc.Customer,
		Transaction:          cc.Transaction,
		Checkout:             cc.Checkout,
		Invoice:              cc.Invoice,
		Subscription:         cc.Subscription,
		DetectionReasonCodes: cc.Case.ReasonCodes,
		PriorActions:         cc.PriorActions,
		PriorRecoveries:      cc.History.Recoveries,
		MinConfidence:        cc.Policy.MinActionConfidence,
		PolicySummary:        agents.PolicySummary(cc.Policy),
		Now:                  now,
	}
}

// plannerInput builds the Intervention Planner's contract (SRS 8.3).
//
// Every count comes from LoadPolicyFacts rather than from the loaded action list,
// so the planner and the policy engine are looking at the same numbers. If they
// disagreed, the planner would propose actions the engine then blocks, and the
// case would stall while looking healthy.
func (cc *caseContext) plannerInput(diag agents.DiagnosisResult, now time.Time) agents.PlannerInput {
	return agents.PlannerInput{
		Case:                    cc.Case,
		Customer:                cc.Customer,
		Diagnosis:               diag,
		TrustedAmount:           cc.TrustedAmount,
		Policy:                  cc.Policy,
		RetryCount:              cc.Facts.RetryCount,
		ReminderCount:           cc.Facts.ReminderCount,
		CaseActionCount:         cc.Facts.CaseActionCount,
		ActionsForCustomerToday: cc.Facts.ActionsForCustomerToday,
		LastActionAt:            cc.Facts.LastActionAt,
		PriorRecoveries:         cc.History.Recoveries,
		ConsecutiveAPIFailures:  cc.Facts.ConsecutiveAPIFailures,
		Priors:                  cc.Priors,
		HasContact:              cc.HasContact,
		AlreadyPaid:             cc.Facts.AlreadyPaid,
		Mode:                    cc.Case.Mode,
		Now:                     now,
	}
}
