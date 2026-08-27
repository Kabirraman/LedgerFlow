package agents

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ledgerflow/ledgerflow/internal/domain"
	"github.com/ledgerflow/ledgerflow/internal/risk"
)

// PlannerInput is the trusted fact set for the Intervention Planner
// (SRS 7 Agent 3).
type PlannerInput struct {
	Case      domain.RiskCase
	Customer  *domain.Customer
	Diagnosis DiagnosisResult

	// TrustedAmount is the amount from the underlying payment record. Every
	// monetary figure in the result is derived from this, never from model text
	// (SRS 19.2).
	TrustedAmount domain.Money

	// Policy is the active control set. It bounds what the planner may propose.
	Policy domain.Policy

	// History, all from the database.
	RetryCount              int
	ReminderCount           int
	CaseActionCount         int
	ActionsForCustomerToday int
	LastActionAt            *time.Time
	PriorRecoveries         int
	ConsecutiveAPIFailures  int

	// Priors maps "segment|source_type|action_type" to an observed success
	// rate, as returned by Store.StrategyPriors. Buckets with too little data
	// are already omitted upstream.
	Priors map[string]float64

	// HasContact reports whether the customer can actually be reached. Without
	// it, a reminder or payment link cannot be delivered.
	HasContact bool

	// AlreadyPaid short-circuits planning entirely.
	AlreadyPaid bool

	Mode domain.RunMode
	Now  time.Time
}

// PlannerResult is the validated intervention decision (SRS 8.3 plus provenance).
type PlannerResult struct {
	RecommendedAction   domain.ActionType `json:"recommended_action"`
	RecoveryProbability float64           `json:"recovery_probability"`
	ExpectedRecovery    domain.Money      `json:"expected_recovery"`
	ReasonCodes         []string          `json:"reason_codes"`
	Alternatives        []string          `json:"alternatives"`
	StopCondition       string            `json:"stop_condition"`

	// EligibleActions is the deterministic allow-list the model was restricted
	// to, retained so a reviewer can see the choice was bounded before the model
	// ever ran (SRS 19.3).
	EligibleActions []domain.ActionType `json:"eligible_actions"`

	Source    string `json:"source"`
	ModelName string `json:"model_name,omitempty"`
	LatencyMS int64  `json:"latency_ms"`

	InjectionSuspected bool   `json:"injection_suspected,omitempty"`
	FallbackReason     string `json:"fallback_reason,omitempty"`
}

// Entity converts the result into the persisted record.
func (r PlannerResult) Entity(caseID, policyVersion string) domain.AgentDecision {
	alts := r.Alternatives
	if alts == nil {
		alts = []string{}
	}
	return domain.AgentDecision{
		CaseID:              caseID,
		RecommendedAction:   r.RecommendedAction,
		RecoveryProbability: r.RecoveryProbability,
		ExpectedRecovery:    r.ExpectedRecovery,
		ReasonCodes:         r.ReasonCodes,
		Alternatives:        alts,
		StopCondition:       r.StopCondition,
		PolicyVersion:       policyVersion,
		Source:              r.Source,
		ModelName:           r.ModelName,
		LatencyMS:           r.LatencyMS,
	}
}

// plannerModelOutput mirrors the SRS 8.3 schema exactly.
type plannerModelOutput struct {
	RecommendedAction   string   `json:"recommended_action"`
	RecoveryProbability float64  `json:"recovery_probability"`
	ExpectedRecovery    int64    `json:"expected_recovery"`
	ReasonCodes         []string `json:"reason_codes"`
	Alternatives        []string `json:"alternatives"`
	StopCondition       string   `json:"stop_condition"`
}

func plannerSchema() schema {
	actions := make([]string, 0, len(domain.AllowedActions))
	for _, a := range domain.AllowedActions {
		actions = append(actions, string(a))
	}
	return objectSchema(
		[]string{"recommended_action", "recovery_probability", "expected_recovery",
			"reason_codes", "alternatives", "stop_condition"},
		map[string]any{
			"recommended_action":   enumSchema(actions),
			"recovery_probability": numberSchema(),
			"expected_recovery":    integerSchema(),
			"reason_codes":         stringArraySchema(),
			"alternatives":         arraySchema(enumSchema(actions)),
			"stop_condition":       stringSchema(),
		})
}

// PlannerAgent chooses one bounded recovery action per case.
//
// Its authority is narrow by construction (SRS 7 Agent 3: "May only choose
// allow-listed actions; cannot bypass policy or call Razorpay"). Three things are
// true of every decision it produces:
//
//   - The action set is computed deterministically before the model runs, so the
//     model selects from a list rather than proposing freely.
//   - Money is recomputed with the SRS 9.2 ERR formula from the trusted amount;
//     the model's own expected_recovery is never stored.
//   - The policy engine re-evaluates the result independently afterwards, so a
//     bad recommendation is caught even if every check here were bypassed.
type PlannerAgent struct {
	client Client
}

// NewPlannerAgent constructs the agent. A nil or disabled client yields
// deterministic-only behaviour.
func NewPlannerAgent(c Client) *PlannerAgent { return &PlannerAgent{client: c} }

// Plan returns an intervention decision, never an error.
func (a *PlannerAgent) Plan(ctx context.Context, in PlannerInput) PlannerResult {
	if in.Now.IsZero() {
		in.Now = time.Now().UTC()
	}
	if in.Policy.Version == "" {
		in.Policy = domain.DefaultPolicy()
	}

	eligible := EligibleActions(in)
	base := deterministicPlan(in, eligible)

	// With one eligible action there is nothing to choose, so the model call is
	// skipped: spending a request and a timeout budget on a foregone conclusion
	// is waste, not diligence.
	if len(eligible) <= 1 {
		base.FallbackReason = "no_choice_available"
		return base
	}
	if a == nil || a.client == nil || !a.client.Enabled() {
		base.FallbackReason = "model_disabled"
		return base
	}

	ev := buildPlannerEvidence(in, eligible)
	base.InjectionSuspected = ev.Suspicious()

	started := time.Now()
	raw, err := a.client.Generate(ctx, plannerSystemPrompt, plannerUserPrompt(ev), plannerSchema())
	base.LatencyMS = time.Since(started).Milliseconds()
	if err != nil {
		base.FallbackReason = fallbackReason(err)
		return base
	}

	var model plannerModelOutput
	if err := decodeStrict(raw, &model); err != nil {
		base.FallbackReason = "invalid_json"
		return base
	}

	// Allow-list enforcement. An off-list action is not clamped to something
	// nearby — it means the model ignored a hard constraint, so the whole
	// recommendation is discarded and the deterministic plan stands (SRS 22.4).
	action, err := domain.ParseActionType(strings.ToLower(strings.TrimSpace(model.RecommendedAction)))
	if err != nil || !containsAction(eligible, action) {
		base.FallbackReason = "action_not_permitted"
		base.ReasonCodes = appendCode(base.ReasonCodes, "model_action_rejected")
		return base
	}

	// The same escalation ceilings the deterministic path applies. A model
	// recommendation is not exempt from them, and a non-empty FallbackReason
	// records that its answer did not stand.
	overridden, overrides := enforceSafetyCeilings(in, eligible, action)
	safetyOverride := overridden != action
	action = overridden

	prob := clamp01(model.RecoveryProbability)
	// A model that reports a higher success rate than the observed history for
	// this exact strategy is optimistic beyond the evidence, so it is capped at
	// the observed rate plus a small allowance.
	if prior, ok := in.Priors[PriorKey(segmentOf(in), in.Case.SourceType, action)]; ok && prob > prior+0.15 {
		prob = prior + 0.15
		base.ReasonCodes = appendCode(base.ReasonCodes, "probability_capped_by_history")
	}
	if !action.IsExternal() {
		prob = 0
	}

	out := PlannerResult{
		RecommendedAction:   action,
		RecoveryProbability: prob,
		// Feasibility is 1.0 here: the policy engine has not run yet, and the
		// orchestrator recomputes ERR with the real feasibility once it has.
		ExpectedRecovery:   risk.ExpectedRecoverableRevenue(in.TrustedAmount, prob, 1.0),
		ReasonCodes:        mergeCodes(base.ReasonCodes, sanitizeCodes(model.ReasonCodes, 8)),
		Alternatives:       sanitizeAlternatives(model.Alternatives, action, eligible),
		StopCondition:      sanitizeSentence(model.StopCondition, 200),
		EligibleActions:    eligible,
		Source:             "ai",
		ModelName:          a.client.Name(),
		LatencyMS:          base.LatencyMS,
		InjectionSuspected: base.InjectionSuspected,
	}
	if safetyOverride {
		out.ReasonCodes = mergeCodes(out.ReasonCodes, overrides)
		out.FallbackReason = "safety_override"
		// The model's stop condition described the action it asked for, which is
		// no longer the one being recommended.
		out.StopCondition = stopConditionFor(in, action)
	}
	if out.ExpectedRecovery > in.TrustedAmount {
		out.ExpectedRecovery = in.TrustedAmount
	}
	if out.StopCondition == "" {
		out.StopCondition = base.StopCondition
	}
	if out.InjectionSuspected {
		out.ReasonCodes = appendCode(out.ReasonCodes, "prompt_injection_detected")
	}
	if len(out.Alternatives) == 0 {
		out.Alternatives = base.Alternatives
	}
	return out
}

// EligibleActions computes which actions are permissible for a case before any
// model runs (SRS 10.1, FR-030).
//
// This is the structural half of the guardrail. The model picks from this list;
// it does not get to widen it. The policy engine then re-checks the pick, so the
// two controls are independent rather than the same check twice.
func EligibleActions(in PlannerInput) []domain.ActionType {
	// escalate and no_action are always available: there must always be a safe
	// answer, or the pipeline would have to act when acting is wrong.
	safe := []domain.ActionType{domain.ActionEscalate, domain.ActionNoAction}

	if in.AlreadyPaid {
		return []domain.ActionType{domain.ActionNoAction}
	}
	if in.Case.Status.IsTerminal() {
		return []domain.ActionType{domain.ActionNoAction}
	}
	if in.TrustedAmount <= 0 {
		return safe
	}
	// Budgets exhausted: no further contact, but a human may still look.
	if in.Policy.MaxActionsPerCase > 0 && in.CaseActionCount >= in.Policy.MaxActionsPerCase {
		return safe
	}
	if in.Policy.MaxActionsPerCustomerPerDay > 0 &&
		in.ActionsForCustomerToday >= in.Policy.MaxActionsPerCustomerPerDay {
		return safe
	}
	if in.ConsecutiveAPIFailures >= 2 {
		return safe
	}
	// Cooldown: contacting the same customer again too soon is the failure mode
	// this system must not demonstrate.
	if in.LastActionAt != nil && in.Policy.CooldownMinutes > 0 &&
		in.Now.Sub(*in.LastActionAt) < time.Duration(in.Policy.CooldownMinutes)*time.Minute {
		return safe
	}

	out := make([]domain.ActionType, 0, 5)

	// retry: only where there is a charge to re-attempt, and only for causes a
	// re-attempt can actually fix.
	if in.Case.SourceType == domain.SourcePaymentFailure ||
		in.Case.SourceType == domain.SourceSubscriptionFailure {
		if in.RetryCount < in.Policy.MaxRetryCount && retryableCause(in.Diagnosis.RootCause) {
			out = append(out, domain.ActionRetry)
		}
	}

	// payment_link works for every workflow, but only if the customer is
	// reachable.
	if in.HasContact {
		out = append(out, domain.ActionPaymentLink)
	}

	// reminder suits abandonment and receivables, subject to the per-case cap.
	if in.HasContact && in.ReminderCount < in.Policy.MaxRemindersPerCase {
		switch in.Case.SourceType {
		case domain.SourceCheckoutAbandonment, domain.SourceInvoiceOverdue,
			domain.SourceSubscriptionFailure:
			out = append(out, domain.ActionReminder)
		}
	}

	return append(out, safe...)
}

// retryableCause reports whether re-attempting the same charge has a plausible
// chance of succeeding. Retrying an authentication failure or an unknown decline
// just annoys the customer, so those route to a payment link instead.
func retryableCause(rc domain.RootCause) bool {
	switch rc {
	case domain.RootCauseTransientFailure, domain.RootCauseInsufficientFunds,
		domain.RootCauseSubscriptionFailure:
		return true
	}
	return false
}

// enforceSafetyCeilings downgrades an external action to ESCALATE when the case
// is one a human must see, and reports the codes explaining why.
//
// It runs on whatever action was selected, model or deterministic. Keeping these
// two rules on the deterministic path alone would mean a model recommendation
// bypassed them — the policy engine would still catch it downstream, but a layer
// whose contract is "cannot bypass policy" must not be the one relying on that
// (SRS 7 Agent 3, 20.4).
func enforceSafetyCeilings(in PlannerInput, eligible []domain.ActionType, chosen domain.ActionType) (domain.ActionType, []string) {
	codes := []string{}
	if !chosen.IsExternal() || !containsAction(eligible, domain.ActionEscalate) {
		return chosen, codes
	}

	// A weak diagnosis must not drive an autonomous action. Escalating is the
	// safe state the SRS requires when confidence is insufficient (SRS 20.4).
	if in.Diagnosis.LowConfidence {
		chosen = domain.ActionEscalate
		codes = append(codes, "low_confidence_diagnosis")
	}
	// High-value cases go to a human even when everything else looks clean.
	if in.TrustedAmount > in.Policy.MaxAutomatedAmount {
		chosen = domain.ActionEscalate
		codes = append(codes, "above_autonomous_ceiling")
	}
	return chosen, codes
}

// deterministicPlan is the fallback and the baseline (SRS 20.4).
//
// It is a real strategy, not a stub: it picks the action that best fits the
// diagnosed cause using the historical priors when they exist. That matters
// twice over — it is what runs when the model is unavailable, and it is the
// "static heuristic" arm of the SRS 17.3 benchmark.
func deterministicPlan(in PlannerInput, eligible []domain.ActionType) PlannerResult {
	r := PlannerResult{
		Source:          "deterministic",
		EligibleActions: eligible,
		ReasonCodes:     []string{},
		Alternatives:    []string{},
	}

	// Rank the eligible actions by fitness for this case, then take the best.
	type scored struct {
		action domain.ActionType
		score  float64
	}
	ranked := make([]scored, 0, len(eligible))
	for _, a := range eligible {
		ranked = append(ranked, scored{a, deterministicFitness(in, a)})
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })

	chosen := domain.ActionNoAction
	if len(ranked) > 0 {
		chosen = ranked[0].action
	}

	chosen, overrides := enforceSafetyCeilings(in, eligible, chosen)
	r.ReasonCodes = mergeCodes(r.ReasonCodes, overrides)

	r.RecommendedAction = chosen
	r.RecoveryProbability = estimateRecoveryProbability(in, chosen)
	r.ExpectedRecovery = risk.ExpectedRecoverableRevenue(in.TrustedAmount, r.RecoveryProbability, 1.0)
	if r.ExpectedRecovery > in.TrustedAmount {
		r.ExpectedRecovery = in.TrustedAmount
	}
	r.ReasonCodes = mergeCodes(r.ReasonCodes, deterministicReasonCodes(in, chosen))
	r.StopCondition = stopConditionFor(in, chosen)

	for _, s := range ranked {
		if s.action != chosen && len(r.Alternatives) < 3 {
			r.Alternatives = append(r.Alternatives, string(s.action))
		}
	}
	return r
}

// deterministicFitness scores how well an action suits the diagnosed cause.
// Scores are ordinal only — they rank candidates and are not probabilities.
func deterministicFitness(in PlannerInput, a domain.ActionType) float64 {
	rc := in.Diagnosis.RootCause
	score := 0.0

	switch a {
	case domain.ActionRetry:
		switch rc {
		case domain.RootCauseTransientFailure:
			score = 0.90
		case domain.RootCauseSubscriptionFailure:
			score = 0.70
		case domain.RootCauseInsufficientFunds:
			// Worth one attempt later, but the balance may still be short.
			score = 0.45
		default:
			score = 0.20
		}
		// Each prior retry lowers the value of another one.
		score -= 0.20 * float64(in.RetryCount)

	case domain.ActionPaymentLink:
		switch rc {
		case domain.RootCauseInsufficientFunds, domain.RootCauseAuthenticationFailed:
			// Let the customer choose a different instrument.
			score = 0.80
		case domain.RootCauseCheckoutAbandonment:
			score = 0.65
		case domain.RootCauseOverdueReceivable:
			score = 0.72
		case domain.RootCauseSubscriptionFailure:
			score = 0.60
		case domain.RootCauseTransientFailure:
			score = 0.50
		default:
			score = 0.45
		}

	case domain.ActionReminder:
		switch rc {
		case domain.RootCauseCheckoutAbandonment:
			score = 0.70
		case domain.RootCauseOverdueReceivable:
			score = 0.68
		default:
			score = 0.35
		}
		score -= 0.15 * float64(in.ReminderCount)

	case domain.ActionEscalate:
		// Escalation ranks above a doubtful automated action but below a clearly
		// suitable one, so the human queue stays useful rather than flooded.
		score = 0.40
		if in.Diagnosis.LowConfidence {
			score = 0.62
		}
		if in.TrustedAmount > in.Policy.MaxAutomatedAmount {
			score = 0.85
		}

	case domain.ActionNoAction:
		score = 0.30
		if in.Diagnosis.RootCause == domain.RootCauseUnknown && in.TrustedAmount < 10_000 {
			// Small amount, no idea why: not worth contacting anyone.
			score = 0.55
		}
	}

	// Historical performance for this exact segment/source/action nudges the
	// ranking. It is a nudge, not a takeover: early in a deployment there is not
	// enough data to justify more.
	if prior, ok := in.Priors[PriorKey(segmentOf(in), in.Case.SourceType, a)]; ok {
		score = 0.75*score + 0.25*prior
	}

	// Customer intent and reliability shift the whole external-contact family.
	if a.IsExternal() && in.Customer != nil {
		if in.Customer.SuccessRate > 0.80 && in.Customer.TotalPayments >= 3 {
			score += 0.05
		}
		if in.Customer.SuccessRate < 0.30 && in.Customer.TotalPayments >= 3 {
			score -= 0.05
		}
	}
	return score
}

// estimateRecoveryProbability produces the probability term of ERR (SRS 9.2)
// without a model, preferring observed history over the built-in prior.
func estimateRecoveryProbability(in PlannerInput, a domain.ActionType) float64 {
	if !a.IsExternal() {
		return 0
	}
	if prior, ok := in.Priors[PriorKey(segmentOf(in), in.Case.SourceType, a)]; ok {
		return clamp01(prior)
	}

	// Cold-start priors. Deliberately conservative: overstating recovery
	// probability would inflate every expected-recovery figure on the dashboard.
	base := 0.30
	switch a {
	case domain.ActionRetry:
		base = 0.35
		if in.Diagnosis.RootCause == domain.RootCauseTransientFailure {
			base = 0.55
		}
	case domain.ActionPaymentLink:
		base = 0.32
		if in.Case.SourceType == domain.SourceInvoiceOverdue {
			base = 0.38
		}
	case domain.ActionReminder:
		base = 0.22
		if in.Case.SourceType == domain.SourceCheckoutAbandonment {
			base = 0.28
		}
	}

	// Confidence in the cause scales the estimate: acting on a guess should not
	// promise the same return as acting on a diagnosis.
	base *= 0.6 + 0.4*clamp01(in.Diagnosis.Confidence)

	if in.PriorRecoveries > 0 {
		base += 0.05
	}
	if in.RetryCount > 0 || in.ReminderCount > 0 {
		base -= 0.05
	}
	if in.Customer != nil && in.Customer.TotalPayments >= 3 {
		base += 0.10 * (in.Customer.SuccessRate - 0.5)
	}
	return clamp01(base)
}

func deterministicReasonCodes(in PlannerInput, a domain.ActionType) []string {
	codes := []string{}
	switch in.Diagnosis.RootCause {
	case domain.RootCauseTransientFailure:
		codes = append(codes, "transient_failure_retryable")
	case domain.RootCauseInsufficientFunds:
		codes = append(codes, "insufficient_funds_needs_customer_action")
	case domain.RootCauseCheckoutAbandonment:
		codes = append(codes, "abandoned_cart_recoverable")
	case domain.RootCauseOverdueReceivable:
		codes = append(codes, "receivable_overdue")
	case domain.RootCauseSubscriptionFailure:
		codes = append(codes, "subscription_charge_failed")
	case domain.RootCauseAuthenticationFailed:
		codes = append(codes, "authentication_failed_prefer_link")
	case domain.RootCauseUnknown:
		codes = append(codes, "root_cause_unknown")
	}
	if a == domain.ActionEscalate {
		codes = append(codes, "requires_human_review")
	}
	if a == domain.ActionNoAction {
		codes = append(codes, "no_positive_expected_value")
	}
	if in.RetryCount > 0 {
		codes = append(codes, "prior_retry_attempted")
	}
	if in.ReminderCount > 0 {
		codes = append(codes, "prior_reminder_sent")
	}
	if in.Customer != nil && in.Customer.Segment == domain.SegmentHighValue {
		codes = append(codes, "high_value_customer")
	}
	if _, ok := in.Priors[PriorKey(segmentOf(in), in.Case.SourceType, a)]; ok {
		codes = append(codes, "history_backed_strategy")
	}
	return codes
}

// stopConditionFor states when to stop pursuing the case, in the operator's
// terms. It is derived from the policy so it can never promise more attempts
// than the policy permits (SRS 10.3).
func stopConditionFor(in PlannerInput, a domain.ActionType) string {
	switch a {
	case domain.ActionRetry:
		return fmt.Sprintf("Stop after %d retries, once the payment succeeds, or if the failure becomes non-transient.",
			in.Policy.MaxRetryCount)
	case domain.ActionPaymentLink:
		return fmt.Sprintf("Stop once the link is paid, after %d total actions on this case, or if the customer opts out.",
			in.Policy.MaxActionsPerCase)
	case domain.ActionReminder:
		return fmt.Sprintf("Stop after %d reminders or once payment is received.",
			in.Policy.MaxRemindersPerCase)
	case domain.ActionEscalate:
		return "Stop automated activity until a reviewer decides."
	default:
		return "No further automated action on this case."
	}
}

func buildPlannerEvidence(in PlannerInput, eligible []domain.ActionType) *evidenceBuilder {
	ev := newEvidence()
	c := in.Case

	ev.Add("case.reference", c.Reference)
	ev.Add("case.source_type", string(c.SourceType))
	ev.Add("case.status", string(c.Status))
	ev.Add("case.urgency", string(c.Urgency))
	ev.Add("case.risk_score", fmt.Sprintf("%.3f", c.RiskScore))
	ev.AddMoney("case.revenue_at_risk", int64(in.TrustedAmount))
	ev.Add("case.age_minutes", int(in.Now.Sub(c.CreatedAt).Minutes()))

	ev.Section("Diagnosis")
	ev.Add("diagnosis.root_cause", string(in.Diagnosis.RootCause))
	ev.Add("diagnosis.confidence", fmt.Sprintf("%.2f", in.Diagnosis.Confidence))
	ev.AddList("diagnosis.uncertainty_flags", in.Diagnosis.UncertaintyFlags)
	for i, e := range in.Diagnosis.Evidence {
		if i >= 4 {
			break
		}
		ev.AddText(fmt.Sprintf("diagnosis.evidence.%d", i), e)
	}

	if cu := in.Customer; cu != nil {
		ev.Section("Customer")
		ev.Add("customer.segment", string(cu.Segment))
		ev.Add("customer.success_rate", fmt.Sprintf("%.2f", cu.SuccessRate))
		ev.Add("customer.total_payments", cu.TotalPayments)
		ev.AddMoney("customer.lifetime_value", int64(cu.LifetimeValue))
		ev.Add("customer.reachable", in.HasContact)
	}

	ev.Section("Intervention history")
	ev.Add("history.retry_count", in.RetryCount)
	ev.Add("history.reminder_count", in.ReminderCount)
	ev.Add("history.actions_on_case", in.CaseActionCount)
	ev.Add("history.actions_for_customer_today", in.ActionsForCustomerToday)
	ev.Add("history.prior_recoveries", in.PriorRecoveries)
	if in.LastActionAt != nil {
		ev.Add("history.minutes_since_last_action", int(in.Now.Sub(*in.LastActionAt).Minutes()))
	}

	// Historical strategy performance, so the probability the model reports is
	// anchored to observed outcomes rather than invented.
	seg := segmentOf(in)
	priorLines := make([]string, 0, len(eligible))
	for _, a := range eligible {
		if p, ok := in.Priors[PriorKey(seg, c.SourceType, a)]; ok {
			priorLines = append(priorLines, fmt.Sprintf("%s=%.2f", a, p))
		}
	}
	if len(priorLines) > 0 {
		ev.Section("Observed strategy success rates for this segment and workflow")
		ev.AddList("history.strategy_success_rates", priorLines)
	}

	ev.Section("ALLOWED ACTIONS")
	names := make([]string, 0, len(eligible))
	for _, a := range eligible {
		names = append(names, string(a))
	}
	ev.AddList("policy.allowed_actions", names)
	ev.Add("policy.note", "You may only choose from policy.allowed_actions. Any other value is rejected.")

	ev.Section("POLICY SUMMARY")
	ev.Add("policy.version", in.Policy.Version)
	ev.AddMoney("policy.max_automated_amount", int64(in.Policy.MaxAutomatedAmount))
	ev.AddMoney("policy.require_human_approval_above", int64(in.Policy.RequireHumanApprovalAbove))
	ev.Add("policy.min_action_confidence", fmt.Sprintf("%.2f", in.Policy.MinActionConfidence))
	ev.Add("policy.max_retry_count", in.Policy.MaxRetryCount)
	ev.Add("policy.max_reminders_per_case", in.Policy.MaxRemindersPerCase)
	ev.Add("policy.max_actions_per_case", in.Policy.MaxActionsPerCase)
	ev.Add("policy.cooldown_minutes", in.Policy.CooldownMinutes)
	ev.Add("run.mode", string(in.Mode))
	return ev
}

func plannerUserPrompt(ev *evidenceBuilder) string {
	return ev.String() + `
TASK
Choose the single best recovery action for this case from policy.allowed_actions, with a calibrated recovery probability and an explicit stop condition.
Set expected_recovery to at most case.revenue_at_risk, in paise.
Return the JSON object only.`
}

// PriorKey builds the strategy-priors map key used by Store.StrategyPriors.
func PriorKey(seg domain.Segment, st domain.SourceType, at domain.ActionType) string {
	return string(seg) + "|" + string(st) + "|" + string(at)
}

// PolicySummary renders the active controls as one line for a prompt, so the
// model reasons inside the same limits the engine will enforce.
func PolicySummary(p domain.Policy) string {
	return fmt.Sprintf(
		"version %s: max %d retries, max %d reminders and %d actions per case, %d actions per customer per day, %d-minute cooldown, autonomous up to ₹%.2f, human approval above ₹%.2f, minimum action confidence %.2f",
		p.Version, p.MaxRetryCount, p.MaxRemindersPerCase, p.MaxActionsPerCase,
		p.MaxActionsPerCustomerPerDay, p.CooldownMinutes,
		p.MaxAutomatedAmount.Rupees(), p.RequireHumanApprovalAbove.Rupees(),
		p.MinActionConfidence)
}

func segmentOf(in PlannerInput) domain.Segment {
	if in.Customer != nil && in.Customer.Segment.Valid() {
		return in.Customer.Segment
	}
	return domain.SegmentNew
}

func containsAction(list []domain.ActionType, a domain.ActionType) bool {
	for _, v := range list {
		if v == a {
			return true
		}
	}
	return false
}

// sanitizeAlternatives keeps only eligible actions other than the chosen one,
// so the "alternatives considered" shown to a reviewer are all actions that
// could genuinely have run.
func sanitizeAlternatives(in []string, chosen domain.ActionType, eligible []domain.ActionType) []string {
	out := make([]string, 0, 3)
	seen := map[string]bool{string(chosen): true}
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		a := domain.ActionType(s)
		if seen[s] || !a.Valid() || !containsAction(eligible, a) {
			continue
		}
		seen[s] = true
		out = append(out, s)
		if len(out) >= 3 {
			break
		}
	}
	return out
}
