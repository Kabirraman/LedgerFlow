package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// CaseDetail assembles everything the case view needs in one call: the facts,
// the reasoning, the control decisions, the actions taken and the verified
// outcome (SRS 16.2, FR-014).
//
// The evidence chain is the product here — an operator has to be able to answer
// "why did the system do this?" without reading logs — so a missing optional
// part degrades the view rather than failing the request.
func (s *Store) CaseDetail(ctx context.Context, caseID string) (*domain.CaseDetail, error) {
	c, err := s.GetCase(ctx, caseID)
	if err != nil {
		return nil, err
	}
	d := &domain.CaseDetail{
		Case:         *c,
		PolicyChecks: []domain.PolicyCheck{},
		Actions:      []domain.RecoveryAction{},
		Approvals:    []domain.Approval{},
		Outcomes:     []domain.Outcome{},
		Timeline:     []domain.TimelineItem{},
	}

	if cust, err := s.GetCustomer(ctx, c.CustomerID); err == nil {
		d.Customer = cust
	}
	switch {
	case c.TransactionID != "":
		if t, err := s.GetTransaction(ctx, c.TransactionID); err == nil {
			d.Transaction = t
		}
	case c.CheckoutSessionID != "":
		if cs, err := s.GetCheckoutSession(ctx, c.CheckoutSessionID); err == nil {
			d.Checkout = cs
		}
	case c.InvoiceID != "":
		if inv, err := s.GetInvoice(ctx, c.InvoiceID); err == nil {
			d.Invoice = inv
		}
	case c.SubscriptionID != "":
		if sub, err := s.GetSubscription(ctx, c.SubscriptionID); err == nil {
			d.Subscription = sub
		}
	}

	if diag, err := s.LatestDiagnosis(ctx, c.ID); err == nil {
		d.Diagnosis = diag
	} else if !isNotFound(err) {
		return nil, err
	}
	if dec, err := s.LatestDecision(ctx, c.ID); err == nil {
		d.Decision = dec
	} else if !isNotFound(err) {
		return nil, err
	}

	if d.PolicyChecks, err = s.ListPolicyChecks(ctx, c.ID); err != nil {
		return nil, err
	}
	if d.Actions, err = s.ListActionsForCase(ctx, c.ID); err != nil {
		return nil, err
	}
	if d.Approvals, err = s.ListApprovalsForCase(ctx, c.ID); err != nil {
		return nil, err
	}
	if d.Outcomes, err = s.ListOutcomesForCase(ctx, c.ID); err != nil {
		return nil, err
	}

	audit, err := s.ListAuditForCase(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	d.Timeline = buildTimeline(d, audit)
	return d, nil
}

// buildTimeline merges every recorded step into one chronological narrative.
// Audit entries that duplicate a richer typed entry are skipped, so the
// timeline reads once per real-world event.
func buildTimeline(d *domain.CaseDetail, audit []domain.AuditLog) []domain.TimelineItem {
	items := make([]domain.TimelineItem, 0, 8+len(audit))

	items = append(items, domain.TimelineItem{
		At:    d.Case.CreatedAt,
		Kind:  "detected",
		Title: fmt.Sprintf("Risk detected: %s", humanSource(d.Case.SourceType)),
		Detail: fmt.Sprintf("₹%.2f at risk · risk score %.2f · %s urgency",
			d.Case.RevenueAtRisk.Rupees(), d.Case.RiskScore, d.Case.Urgency),
		Result: strings.Join(d.Case.ReasonCodes, ", "),
		Actor:  "detection_agent",
	})

	if dg := d.Diagnosis; dg != nil {
		items = append(items, domain.TimelineItem{
			At:     dg.CreatedAt,
			Kind:   "diagnosed",
			Title:  fmt.Sprintf("Root cause: %s", humanRootCause(dg.RootCause)),
			Detail: strings.Join(dg.Evidence, "; "),
			Result: fmt.Sprintf("confidence %.0f%%", dg.Confidence*100),
			Actor:  agentActor("diagnosis_agent", dg.Source),
		})
	}

	if dec := d.Decision; dec != nil {
		detail := fmt.Sprintf("expected recovery ₹%.2f at %.0f%% probability",
			dec.ExpectedRecovery.Rupees(), dec.RecoveryProbability*100)
		if len(dec.Alternatives) > 0 {
			detail += " · alternatives considered: " + strings.Join(dec.Alternatives, ", ")
		}
		items = append(items, domain.TimelineItem{
			At:     dec.CreatedAt,
			Kind:   "planned",
			Title:  fmt.Sprintf("Planned intervention: %s", humanAction(dec.RecommendedAction)),
			Detail: detail,
			Result: strings.Join(dec.ReasonCodes, ", "),
			Actor:  agentActor("intervention_planner", dec.Source),
		})
	}

	// Policy checks collapse to one entry per decision: the operator wants the
	// verdict and the deciding rule, with the full rule list available
	// separately in the policy panel.
	for _, group := range groupChecksByDecision(d.PolicyChecks) {
		items = append(items, domain.TimelineItem{
			At:     group.At,
			Kind:   "policy",
			Title:  fmt.Sprintf("Policy evaluation: %s", group.Result),
			Detail: group.Detail,
			Result: string(group.Result),
			Actor:  "policy_engine",
		})
	}

	for _, a := range d.Approvals {
		items = append(items, domain.TimelineItem{
			At:     a.RequestedAt,
			Kind:   "escalated",
			Title:  "Escalated for human approval",
			Detail: a.Reason,
			Actor:  "policy_engine",
		})
		if a.DecidedAt != nil {
			items = append(items, domain.TimelineItem{
				At:     *a.DecidedAt,
				Kind:   "approval_decision",
				Title:  fmt.Sprintf("Human %s", a.Decision),
				Detail: a.DecisionNote,
				Result: string(a.Decision),
				Actor:  a.Reviewer,
			})
		}
	}

	for _, a := range d.Actions {
		at := a.RequestedAt
		if a.ExecutedAt != nil {
			at = *a.ExecutedAt
		}
		detail := fmt.Sprintf("₹%.2f", a.Amount.Rupees())
		if a.ExternalID != "" {
			detail += " · " + a.ExternalID
		}
		if a.ErrorMessage != "" {
			detail += " · " + a.ErrorMessage
		}
		items = append(items, domain.TimelineItem{
			At:     at,
			Kind:   "action",
			Title:  fmt.Sprintf("%s %s", humanAction(a.ActionType), a.Status),
			Detail: detail,
			Result: string(a.Status),
			Actor:  "action_executor",
		})
	}

	for _, o := range d.Outcomes {
		at := o.CreatedAt
		if o.RecoveredAt != nil {
			at = *o.RecoveredAt
		}
		title := "Outcome: " + string(o.Outcome)
		if o.Outcome == domain.OutcomeRecovered {
			title = fmt.Sprintf("Recovered ₹%.2f", o.RecoveredAmount.Rupees())
		}
		detail := o.Notes
		if o.TimeToRecoverySeconds > 0 {
			detail = fmt.Sprintf("%s · %.1f min to recovery", detail,
				float64(o.TimeToRecoverySeconds)/60.0)
		}
		items = append(items, domain.TimelineItem{
			At:     at,
			Kind:   "outcome",
			Title:  title,
			Detail: strings.TrimPrefix(detail, " · "),
			Result: string(o.Outcome),
			Actor:  o.VerificationSource,
		})
	}

	for _, e := range audit {
		if timelineCoveredByTypedEntry[e.EventType] {
			continue
		}
		items = append(items, domain.TimelineItem{
			At:    e.Timestamp,
			Kind:  "audit",
			Title: humanEventType(e.EventType),
			Actor: e.Actor,
		})
	}

	sort.SliceStable(items, func(i, j int) bool { return items[i].At.Before(items[j].At) })
	return items
}

// timelineCoveredByTypedEntry lists audit event types that already have a
// richer typed timeline entry, to avoid showing each step twice.
var timelineCoveredByTypedEntry = map[string]bool{
	"case_detected":      true,
	"case_diagnosed":     true,
	"decision_planned":   true,
	"policy_evaluated":   true,
	"action_executed":    true,
	"outcome_verified":   true,
	"approval_requested": true,
	"approval_decided":   true,
}

// checkGroup is one decision's governing policy verdict.
type checkGroup struct {
	Result domain.PolicyResult
	Detail string
	At     time.Time
}

// groupChecksByDecision reduces each decision's rule set to its governing
// verdict, using the same BLOCK > ESCALATE > PASS precedence as the engine.
func groupChecksByDecision(checks []domain.PolicyCheck) []checkGroup {
	rank := func(r domain.PolicyResult) int {
		switch r {
		case domain.PolicyBlock:
			return 3
		case domain.PolicyEscalate:
			return 2
		default:
			return 1
		}
	}

	type agg struct {
		group   checkGroup
		passing int
		total   int
	}
	order := []string{}
	byDecision := map[string]*agg{}

	for _, c := range checks {
		a, ok := byDecision[c.DecisionID]
		if !ok {
			a = &agg{group: checkGroup{Result: domain.PolicyPass, At: c.CreatedAt}}
			byDecision[c.DecisionID] = a
			order = append(order, c.DecisionID)
		}
		a.total++
		if c.Result == domain.PolicyPass {
			a.passing++
		}
		if rank(c.Result) > rank(a.group.Result) {
			a.group.Result = c.Result
			a.group.Detail = c.Rule + ": " + c.Details
		}
		if c.CreatedAt.After(a.group.At) {
			a.group.At = c.CreatedAt
		}
	}

	out := make([]checkGroup, 0, len(order))
	for _, id := range order {
		a := byDecision[id]
		if a.group.Detail == "" {
			a.group.Detail = fmt.Sprintf("%d of %d rules passed", a.passing, a.total)
		}
		out = append(out, a.group)
	}
	return out
}

func humanSource(st domain.SourceType) string {
	switch st {
	case domain.SourcePaymentFailure:
		return "payment failure"
	case domain.SourceCheckoutAbandonment:
		return "checkout abandonment"
	case domain.SourceInvoiceOverdue:
		return "overdue invoice"
	case domain.SourceSubscriptionFailure:
		return "subscription payment failure"
	}
	return string(st)
}

func humanRootCause(rc domain.RootCause) string {
	return strings.ReplaceAll(string(rc), "_", " ")
}

func humanAction(a domain.ActionType) string {
	switch a {
	case domain.ActionRetry:
		return "Payment retry"
	case domain.ActionPaymentLink:
		return "Payment link"
	case domain.ActionReminder:
		return "Reminder"
	case domain.ActionEscalate:
		return "Escalation"
	case domain.ActionNoAction:
		return "No action"
	}
	return string(a)
}

func humanEventType(t string) string {
	s := strings.ReplaceAll(t, "_", " ")
	if s == "" {
		return "event"
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// agentActor labels a step with whether the AI or the deterministic fallback
// produced it. Attributing the fallback honestly matters: a demo that presents
// rule-based output as model output misrepresents the system (SRS 25.2).
func agentActor(agent, source string) string {
	if source == "" || source == "ai" {
		return agent
	}
	return agent + " (" + source + ")"
}
