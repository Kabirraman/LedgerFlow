// Package policy implements LEDGERFLOW's deterministic policy engine.
//
// The engine is the only component that decides whether a proposed action may
// execute. It contains no model calls, no I/O and no randomness: given the same
// request it always returns the same verdict, which is what makes the
// "0% policy violation" target in SRS 3.2 testable.
//
// Every rule that runs is recorded, not just the failing one, so a reviewer can
// see the full control set that cleared an action (SRS 16.2).
package policy

import (
	"fmt"
	"time"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// Rule names are stable identifiers used in the audit trail and in tests.
const (
	RuleActionAllowList    = "action_allow_list"
	RuleAmountIntegrity    = "amount_integrity"
	RuleAlreadyRecovered   = "already_recovered"
	RuleConflictingState   = "conflicting_external_state"
	RuleMaxRetryCount      = "max_retry_count"
	RuleMaxReminderCount   = "max_reminder_count"
	RuleMaxActionsPerCase  = "max_actions_per_case"
	RuleDailyActionLimit   = "max_actions_per_customer_per_day"
	RuleCooldown           = "cooldown_minutes"
	RuleMinConfidence      = "min_action_confidence"
	RuleAPIFailureBudget   = "api_failure_budget"
	RuleMaxAutomatedAmount = "max_automated_amount"
	RuleHumanApproval      = "require_human_approval_above"
	RuleSimulationBoundary = "simulation_boundary"
	RuleTerminalCaseState  = "terminal_case_state"
)

// Request is the complete, trusted input to a policy evaluation. Every field is
// supplied by the caller from database facts; nothing here comes from a model
// except Decision, which is exactly what the engine is auditing.
type Request struct {
	Case     domain.RiskCase
	Decision domain.AgentDecision
	Policy   domain.Policy

	// TrustedAmount is the amount from the underlying transaction, invoice,
	// subscription or checkout session. The decision's own amount is compared
	// against this and never trusted on its own (SRS 19.2, 22.4).
	TrustedAmount domain.Money

	// Action history for this case.
	RetryCount      int
	ReminderCount   int
	CaseActionCount int

	// ActionsForCustomerToday counts executed actions for this customer within
	// the trailing 24 hours.
	ActionsForCustomerToday int

	// LastActionAt is the most recent executed action for this customer, used
	// for the cooldown rule. Nil means no prior action.
	LastActionAt *time.Time

	// AlreadyPaid is true when external state shows the money has arrived.
	AlreadyPaid bool

	// ConflictingExternalState is true when the external status is unknown or
	// contradicts internal state (SRS 20.3 "conflicting state").
	ConflictingExternalState bool

	// ConsecutiveAPIFailures is the count of failed execution attempts for
	// this case.
	ConsecutiveAPIFailures int

	// APIFailureBudget bounds retries after transport errors.
	APIFailureBudget int

	// HasHumanApproval is true when a reviewer explicitly approved this
	// decision. It downgrades ESCALATE to PASS but never overrides a BLOCK.
	HasHumanApproval bool

	// Mode is the run mode; simulation mode may not produce external calls.
	Mode domain.RunMode

	// Now is injected so evaluations are reproducible in tests.
	Now time.Time
}

// Verdict is the engine's output.
type Verdict struct {
	Result domain.PolicyResult `json:"result"`
	// Reason is the human-readable summary of the deciding rule.
	Reason string `json:"reason"`
	// DecidingRule names the rule that produced a non-PASS result.
	DecidingRule string `json:"deciding_rule,omitempty"`
	// Checks is every rule evaluated, in evaluation order.
	Checks []domain.PolicyCheck `json:"checks"`
	// Feasibility is the intervention feasibility term of ERR (SRS 9.2):
	// 1.0 when the action can execute now, lower when it is gated, 0 when it
	// cannot execute at all.
	Feasibility float64 `json:"feasibility"`
	// StopReason is set when a mandatory stopping rule fired (SRS 10.3).
	StopReason string `json:"stop_reason,omitempty"`
	// RequiresApproval is true when the case must wait for a human.
	RequiresApproval bool `json:"requires_approval"`
}

// Engine evaluates actions against a policy. It is stateless and safe for
// concurrent use.
type Engine struct{}

// New returns a policy engine.
func New() *Engine { return &Engine{} }

// Evaluate runs every rule and returns the aggregate verdict.
//
// Precedence is deliberate and fixed:
//  1. BLOCK wins over everything. A blocked action can never execute, and a
//     human approval cannot override it.
//  2. ESCALATE wins over PASS, unless an explicit approval is already on
//     record for this decision.
//  3. PASS only when every rule passed.
func (e *Engine) Evaluate(req Request) Verdict {
	if req.Now.IsZero() {
		req.Now = time.Now().UTC()
	}
	if req.APIFailureBudget == 0 {
		req.APIFailureBudget = 2
	}

	v := Verdict{Result: domain.PolicyPass, Feasibility: 1.0}
	rec := &recorder{caseID: req.Case.ID, decisionID: req.Decision.ID, version: req.Policy.Version, now: req.Now}

	action := req.Decision.RecommendedAction

	// --- Rule 1: allow-list. An unrecognised action never reaches an API.
	if !action.Valid() {
		rec.block(RuleActionAllowList, fmt.Sprintf("action %q is not on the executor allow-list", string(action)))
	} else {
		rec.pass(RuleActionAllowList, fmt.Sprintf("action %q is allow-listed", action))
	}

	// Non-external actions still get audited, but most money rules do not
	// apply to them. escalate/no_action move the case without side effects.
	external := action.IsExternal()

	// --- Rule 2: simulation boundary (SRS AC-009).
	if req.Mode == domain.ModeSimulation && external {
		// This is not a violation by itself: the simulator resolves outcomes
		// internally. The rule exists so the executor can assert that no
		// external transport is reachable from this path.
		rec.pass(RuleSimulationBoundary, "simulation mode: action resolved internally, no external API call permitted")
	} else if external {
		rec.pass(RuleSimulationBoundary, "live test mode: external call permitted through action service only")
	}

	// --- Rule 3: terminal case state.
	//
	// The four terminal states are RECOVERED, REJECTED, BLOCKED and CLOSED. None
	// of them may produce another action: a case that has already been recovered
	// or closed getting one more payment link is a duplicate demand on the
	// customer (SRS 20.1).
	if req.Case.Status.IsTerminal() {
		rec.block(RuleTerminalCaseState, fmt.Sprintf("case is already in terminal state %s", req.Case.Status))
	} else {
		rec.pass(RuleTerminalCaseState, fmt.Sprintf("case state %s permits action", req.Case.Status))
	}

	// --- Rule 4: amount integrity (SRS 22.4).
	// The planner's expected recovery may be lower than the trusted amount
	// (partial expectation is legitimate) but it may never exceed it, and the
	// action can never target more than the trusted amount.
	if external {
		switch {
		case req.TrustedAmount <= 0:
			rec.block(RuleAmountIntegrity, "no trusted amount available for this case")
		case req.Decision.ExpectedRecovery > req.TrustedAmount:
			rec.block(RuleAmountIntegrity, fmt.Sprintf(
				"proposed expected recovery %d exceeds trusted amount %d", req.Decision.ExpectedRecovery, req.TrustedAmount))
		default:
			rec.pass(RuleAmountIntegrity, fmt.Sprintf("target amount %d matches trusted record", req.TrustedAmount))
		}
	}

	// --- Rule 5: already recovered (stopping rule, SRS 10.3).
	if req.AlreadyPaid {
		rec.block(RuleAlreadyRecovered, "customer has already completed payment; stop acting")
		v.StopReason = "customer_already_paid"
	} else {
		rec.pass(RuleAlreadyRecovered, "no completed payment on record")
	}

	// --- Rule 6: conflicting or unknown external state (SRS 20.3).
	if req.ConflictingExternalState {
		rec.escalate(RuleConflictingState, "external state is unknown or conflicts with internal state; requires review")
		v.StopReason = "conflicting_external_state"
	} else {
		rec.pass(RuleConflictingState, "external state is consistent")
	}

	// --- Rule 7: retry limit (stopping rule).
	if action == domain.ActionRetry {
		if req.RetryCount >= req.Policy.MaxRetryCount {
			rec.block(RuleMaxRetryCount, fmt.Sprintf("retry count %d has reached limit %d", req.RetryCount, req.Policy.MaxRetryCount))
			v.StopReason = "max_retries_reached"
		} else {
			rec.pass(RuleMaxRetryCount, fmt.Sprintf("retry count %d is below limit %d", req.RetryCount, req.Policy.MaxRetryCount))
		}
	}

	// --- Rule 8: reminder limit (stopping rule).
	if action == domain.ActionReminder {
		if req.ReminderCount >= req.Policy.MaxRemindersPerCase {
			rec.block(RuleMaxReminderCount, fmt.Sprintf("reminder count %d has reached limit %d", req.ReminderCount, req.Policy.MaxRemindersPerCase))
			v.StopReason = "max_reminders_reached"
		} else {
			rec.pass(RuleMaxReminderCount, fmt.Sprintf("reminder count %d is below limit %d", req.ReminderCount, req.Policy.MaxRemindersPerCase))
		}
	}

	// --- Rule 9: per-case action budget (stopping rule).
	if external {
		if req.Policy.MaxActionsPerCase > 0 && req.CaseActionCount >= req.Policy.MaxActionsPerCase {
			rec.block(RuleMaxActionsPerCase, fmt.Sprintf("case already has %d actions, limit is %d", req.CaseActionCount, req.Policy.MaxActionsPerCase))
			v.StopReason = "case_action_budget_exhausted"
		} else {
			rec.pass(RuleMaxActionsPerCase, fmt.Sprintf("case action count %d is within limit %d", req.CaseActionCount, req.Policy.MaxActionsPerCase))
		}
	}

	// --- Rule 10: daily per-customer action limit (stopping rule).
	if external {
		if req.Policy.MaxActionsPerCustomerPerDay > 0 && req.ActionsForCustomerToday >= req.Policy.MaxActionsPerCustomerPerDay {
			rec.block(RuleDailyActionLimit, fmt.Sprintf("customer already received %d actions today, limit is %d",
				req.ActionsForCustomerToday, req.Policy.MaxActionsPerCustomerPerDay))
			v.StopReason = "daily_action_limit_reached"
		} else {
			rec.pass(RuleDailyActionLimit, fmt.Sprintf("customer has %d of %d daily actions used",
				req.ActionsForCustomerToday, req.Policy.MaxActionsPerCustomerPerDay))
		}
	}

	// --- Rule 11: cooldown between contacts.
	if external && req.LastActionAt != nil && req.Policy.CooldownMinutes > 0 {
		elapsed := req.Now.Sub(*req.LastActionAt)
		cooldown := time.Duration(req.Policy.CooldownMinutes) * time.Minute
		if elapsed < cooldown {
			rec.block(RuleCooldown, fmt.Sprintf("last action was %s ago; cooldown is %s",
				elapsed.Round(time.Second), cooldown))
			v.StopReason = "cooldown_active"
		} else {
			rec.pass(RuleCooldown, fmt.Sprintf("last action was %s ago; cooldown %s satisfied",
				elapsed.Round(time.Second), cooldown))
		}
	} else if external {
		rec.pass(RuleCooldown, "no prior action for this customer")
	}

	// --- Rule 12: minimum confidence for autonomous action (stopping rule).
	if external {
		if req.Decision.RecoveryProbability < req.Policy.MinActionConfidence {
			rec.escalate(RuleMinConfidence, fmt.Sprintf("confidence %.2f is below autonomous threshold %.2f",
				req.Decision.RecoveryProbability, req.Policy.MinActionConfidence))
			if v.StopReason == "" {
				v.StopReason = "below_confidence_threshold"
			}
		} else {
			rec.pass(RuleMinConfidence, fmt.Sprintf("confidence %.2f meets threshold %.2f",
				req.Decision.RecoveryProbability, req.Policy.MinActionConfidence))
		}
	}

	// --- Rule 13: API failure budget (stopping rule).
	if external {
		if req.ConsecutiveAPIFailures >= req.APIFailureBudget {
			rec.block(RuleAPIFailureBudget, fmt.Sprintf("%d consecutive API failures exceeds budget %d",
				req.ConsecutiveAPIFailures, req.APIFailureBudget))
			v.StopReason = "api_failure_budget_exhausted"
		} else {
			rec.pass(RuleAPIFailureBudget, fmt.Sprintf("%d of %d API failure budget used",
				req.ConsecutiveAPIFailures, req.APIFailureBudget))
		}
	}

	// --- Rule 14: autonomous monetary ceiling (stopping rule).
	if external {
		if req.TrustedAmount > req.Policy.MaxAutomatedAmount {
			rec.escalate(RuleMaxAutomatedAmount, fmt.Sprintf("amount %d exceeds autonomous ceiling %d",
				req.TrustedAmount, req.Policy.MaxAutomatedAmount))
			if v.StopReason == "" {
				v.StopReason = "exceeds_autonomous_amount"
			}
		} else {
			rec.pass(RuleMaxAutomatedAmount, fmt.Sprintf("amount %d is within autonomous ceiling %d",
				req.TrustedAmount, req.Policy.MaxAutomatedAmount))
		}
	}

	// --- Rule 15: human approval threshold (SRS FR-045).
	if external {
		if req.TrustedAmount > req.Policy.RequireHumanApprovalAbove {
			rec.escalate(RuleHumanApproval, fmt.Sprintf("amount %d is above human-approval threshold %d",
				req.TrustedAmount, req.Policy.RequireHumanApprovalAbove))
		} else {
			rec.pass(RuleHumanApproval, fmt.Sprintf("amount %d is below human-approval threshold %d",
				req.TrustedAmount, req.Policy.RequireHumanApprovalAbove))
		}
	}

	// The planner may also ask for escalation directly.
	if action == domain.ActionEscalate {
		rec.escalate(RuleHumanApproval, "planner explicitly requested escalation")
	}

	v.Checks = rec.checks
	v.Result, v.DecidingRule, v.Reason = rec.aggregate()

	// An explicit approval on record satisfies every escalation, but never a
	// block: BLOCK is a hard rule, not a request for permission.
	if v.Result == domain.PolicyEscalate && req.HasHumanApproval {
		v.Checks = append(v.Checks, rec.build(RuleHumanApproval, domain.PolicyPass,
			"reviewer approval on record satisfies escalation"))
		v.Result = domain.PolicyPass
		v.Reason = "approved by reviewer"
		v.DecidingRule = ""
	}

	switch v.Result {
	case domain.PolicyPass:
		v.Feasibility = 1.0
		v.StopReason = ""
	case domain.PolicyEscalate:
		// A gated action is still recoverable, just not autonomously. The
		// discount reflects approval latency and reviewer rejection risk.
		v.Feasibility = 0.75
		v.RequiresApproval = true
	case domain.PolicyBlock:
		v.Feasibility = 0.0
	}

	// no_action and escalate never move money, so ERR feasibility is zero for
	// the purpose of "what will this action recover".
	if !external {
		v.Feasibility = 0.0
	}

	return v
}

// recorder accumulates policy checks in evaluation order.
type recorder struct {
	caseID     string
	decisionID string
	version    string
	now        time.Time
	checks     []domain.PolicyCheck
	worst      domain.PolicyResult
	decider    string
	reason     string
}

func (r *recorder) build(rule string, result domain.PolicyResult, details string) domain.PolicyCheck {
	return domain.PolicyCheck{
		CaseID:        r.caseID,
		DecisionID:    r.decisionID,
		PolicyVersion: r.version,
		Rule:          rule,
		Result:        result,
		Details:       details,
		CreatedAt:     r.now,
	}
}

func (r *recorder) add(rule string, result domain.PolicyResult, details string) {
	r.checks = append(r.checks, r.build(rule, result, details))
	// BLOCK outranks ESCALATE outranks PASS; the first rule at the worst
	// severity is reported as the decider.
	if rank(result) > rank(r.worst) {
		r.worst = result
		r.decider = rule
		r.reason = details
	}
}

func (r *recorder) pass(rule, details string)     { r.add(rule, domain.PolicyPass, details) }
func (r *recorder) block(rule, details string)    { r.add(rule, domain.PolicyBlock, details) }
func (r *recorder) escalate(rule, details string) { r.add(rule, domain.PolicyEscalate, details) }

func (r *recorder) aggregate() (domain.PolicyResult, string, string) {
	if r.worst == "" {
		return domain.PolicyPass, "", "all policy checks passed"
	}
	if r.worst == domain.PolicyPass {
		return domain.PolicyPass, "", "all policy checks passed"
	}
	return r.worst, r.decider, r.reason
}

func rank(p domain.PolicyResult) int {
	switch p {
	case domain.PolicyBlock:
		return 3
	case domain.PolicyEscalate:
		return 2
	case domain.PolicyPass:
		return 1
	}
	return 0
}
