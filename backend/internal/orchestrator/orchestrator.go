// Package orchestrator runs LEDGERFLOW's recovery workflow: the loop from
// SRS 2.3 that takes a case from "something looks wrong" to "money arrived or a
// human owns it".
//
// The four agents are stateless functions over a fact snapshot. All state lives
// in the case row, and every step is a validated transition in the SRS 14.2 state
// machine, so the workflow can be interrupted at any point — a crash, a restart, a
// deploy — and resumed from the database alone. There is no in-memory queue and no
// per-case goroutine to lose.
//
// Two rules shape the whole file:
//
//   - Nothing an agent returns is trusted as an instruction. A model result is
//     persisted as a recommendation, then re-derived facts and the deterministic
//     policy engine decide what actually happens (SRS 19.2, 19.3).
//   - The system never fails open. A model timeout, invalid JSON or low confidence
//     routes the case to ESCALATE or NO_ACTION, never to an unattended execution
//     (SRS 20.4).
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ledgerflow/ledgerflow/internal/agents"
	"github.com/ledgerflow/ledgerflow/internal/domain"
	"github.com/ledgerflow/ledgerflow/internal/executor"
	"github.com/ledgerflow/ledgerflow/internal/idem"
	"github.com/ledgerflow/ledgerflow/internal/policy"
	"github.com/ledgerflow/ledgerflow/internal/risk"
	"github.com/ledgerflow/ledgerflow/internal/store"
)

// Store is the persistence surface the workflow needs.
//
// It is deliberately explicit rather than a wide interface: this list is the
// complete set of things the orchestrator can do to the database, which makes the
// blast radius of the pipeline auditable by reading one declaration (SRS NFR-007).
type Store interface {
	ClaimCasesForStage(ctx context.Context, status domain.CaseStatus, limit int) ([]domain.RiskCase, error)
	GetCase(ctx context.Context, id string) (*domain.RiskCase, error)
	GetCustomer(ctx context.Context, id string) (*domain.Customer, error)
	GetTransaction(ctx context.Context, id string) (*domain.Transaction, error)
	GetCheckoutSession(ctx context.Context, id string) (*domain.CheckoutSession, error)
	GetInvoice(ctx context.Context, id string) (*domain.Invoice, error)
	GetSubscription(ctx context.Context, id string) (*domain.Subscription, error)

	ListActionsForCase(ctx context.Context, caseID string) ([]domain.RecoveryAction, error)
	LoadCustomerHistory(ctx context.Context, customerID string) (store.CustomerHistory, error)
	LoadPolicyFacts(ctx context.Context, caseID, customerID, decisionID string) (store.PolicyFacts, error)
	StrategyPriors(ctx context.Context) (map[string]float64, error)
	ActivePolicyOrDefault(ctx context.Context) domain.Policy

	SaveDiagnosis(ctx context.Context, d *domain.Diagnosis) error
	LatestDiagnosis(ctx context.Context, caseID string) (*domain.Diagnosis, error)
	SaveDecision(ctx context.Context, d *domain.AgentDecision) error
	LatestDecision(ctx context.Context, caseID string) (*domain.AgentDecision, error)
	SavePolicyChecks(ctx context.Context, checks []domain.PolicyCheck) error
	RequestApproval(ctx context.Context, a *domain.Approval) (bool, error)

	UpdateCaseStatus(ctx context.Context, caseID string, to domain.CaseStatus, stopReason string) error
	UpdateCaseAssessment(ctx context.Context, caseID string, revenueAtRisk domain.Money,
		riskScore float64, urgency domain.Urgency, reasonCodes, evidenceRefs []string) error
	UpdateCaseExpectedRecovery(ctx context.Context, caseID string, expected domain.Money) error

	Audit(ctx context.Context, actor, entityType, entityID, caseID, eventType string, detail any) error
	IncrCounter(ctx context.Context, name string) error
}

// Executor is the action service, behind an interface so the benchmark harness can
// substitute a recording double and prove the pipeline issued no external call
// (SRS 22.4, 23.4).
type Executor interface {
	Execute(ctx context.Context, req executor.Request) (executor.Result, error)
}

// Counter names this package reports, kept local for the same reason as in the
// executor: a plain string is not worth a package dependency.
const (
	counterCasesAdvanced = "cases_advanced"
	counterPolicyBlocks  = "policy_blocks"
	counterEscalations   = "escalations"
	counterNoAction      = "cases_no_action"
	counterNotAtRisk     = "cases_not_at_risk"
	counterPipelineError = "pipeline_errors"
)

const actor = "orchestrator"

// Config tunes the workflow.
type Config struct {
	// BatchLimit bounds how many cases one stage claims per pass, so a backlog
	// cannot starve the later stages that are closer to money.
	BatchLimit int
	// MaxStages bounds how many transitions a single case may make in one pass.
	// A case normally walks NEW → ... → VERIFYING in one go; the bound exists so a
	// state-machine mistake becomes a stalled case rather than a spinning worker.
	MaxStages int
	// StageTimeout bounds one stage, including its model call.
	StageTimeout time.Duration
	// APIFailureBudget is how many consecutive execution failures a case may
	// accumulate before the policy engine stops it.
	APIFailureBudget int
	// EscalateFailuresAbove is the amount at risk above which an exhausted case is
	// handed to a human instead of being closed. Below it, a case that has used up
	// its retries is closed with a stop reason: filling the approval queue with
	// small write-offs would bury the cases where review is worth a person's time
	// (SRS 20.1 "4xx validation error → escalate / close", 12.1).
	EscalateFailuresAbove domain.Money
}

func (c Config) withDefaults() Config {
	if c.BatchLimit <= 0 {
		c.BatchLimit = 20
	}
	if c.MaxStages <= 0 {
		c.MaxStages = 8
	}
	if c.StageTimeout <= 0 {
		c.StageTimeout = 30 * time.Second
	}
	if c.APIFailureBudget <= 0 {
		c.APIFailureBudget = 2
	}
	if c.EscalateFailuresAbove <= 0 {
		c.EscalateFailuresAbove = 100_000 // ₹1,000
	}
	return c
}

// Orchestrator advances cases through the recovery workflow.
type Orchestrator struct {
	store  Store
	detect *agents.DetectionAgent
	diag   *agents.DiagnosisAgent
	plan   *agents.PlannerAgent
	policy *policy.Engine
	exec   Executor
	cfg    Config
	now    func() time.Time
}

// New builds an orchestrator. The policy engine is constructed here rather than
// injected: there is exactly one set of rules, and allowing a caller to supply its
// own would make the safety guarantees configurable.
func New(s Store, detect *agents.DetectionAgent, diag *agents.DiagnosisAgent,
	plan *agents.PlannerAgent, exec Executor, cfg Config) *Orchestrator {
	return &Orchestrator{
		store:  s,
		detect: detect,
		diag:   diag,
		plan:   plan,
		policy: policy.New(),
		exec:   exec,
		cfg:    cfg.withDefaults(),
		now:    func() time.Time { return time.Now().UTC() },
	}
}

// SetClock overrides the clock for deterministic tests and simulations.
func (o *Orchestrator) SetClock(fn func() time.Time) { o.now = fn }

// Progress is what one pass over a case did. It is returned to the API so the UI
// can show an operator the result of a manual "run pipeline" without polling
// (SRS 15.2).
type Progress struct {
	CaseID    string              `json:"case_id"`
	Reference string              `json:"reference"`
	From      domain.CaseStatus   `json:"from"`
	To        domain.CaseStatus   `json:"to"`
	Stages    []string            `json:"stages"`
	Action    domain.ActionType   `json:"action,omitempty"`
	Verdict   domain.PolicyResult `json:"policy_result,omitempty"`
	Reason    string              `json:"reason,omitempty"`
	Executed  bool                `json:"executed"`
	Escalated bool                `json:"escalated"`
	Blocked   bool                `json:"blocked"`
	Result    *executor.Result    `json:"result,omitempty"`
}

// Report summarises one worker pass.
type Report struct {
	Claimed   int      `json:"claimed"`
	Advanced  int      `json:"advanced"`
	Executed  int      `json:"executed"`
	Escalated int      `json:"escalated"`
	Blocked   int      `json:"blocked"`
	Errors    []string `json:"errors,omitempty"`
}

// stageOrder is the order stages are claimed in: closest to money first.
//
// FAILED leads because a stranded case is the one most likely to be silently
// losing recoverable revenue, and APPROVED precedes the analysis stages because a
// case that has already cleared policy should not wait behind fresh detection work
// during a backlog.
var stageOrder = []domain.CaseStatus{
	domain.StatusFailed,
	domain.StatusApproved,
	domain.StatusEscalated,
	domain.StatusPolicyReview,
	domain.StatusPlanned,
	domain.StatusDiagnosed,
	domain.StatusAnalyzing,
	domain.StatusRetrying,
	domain.StatusNew,
}

// RunOnce claims work for every actionable stage and advances it.
//
// The pass is designed for one worker goroutine per process. Correctness under
// concurrency does not depend on that, though: ClaimCasesForStage takes row locks
// with FOR UPDATE SKIP LOCKED, and UpdateCaseStatus re-reads and validates the
// transition under a lock, so two workers racing on one case produce one advance
// and one rejected transition rather than two half-applied ones.
func (o *Orchestrator) RunOnce(ctx context.Context) (Report, error) {
	var rep Report
	for _, status := range stageOrder {
		cases, err := o.store.ClaimCasesForStage(ctx, status, o.cfg.BatchLimit)
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Sprintf("claim %s: %v", status, err))
			continue
		}
		for idx := range cases {
			rep.Claimed++
			// The claimed row is passed by id, not by value: AdvanceCase reloads it
			// so a case that moved between the claim and the advance is worked on as
			// it is now.
			prog, err := o.AdvanceCase(ctx, cases[idx].ID)
			if err != nil {
				_ = o.store.IncrCounter(ctx, counterPipelineError)
				rep.Errors = append(rep.Errors, fmt.Sprintf("case %s: %v", cases[idx].ID, err))
				continue
			}
			if len(prog.Stages) > 0 {
				rep.Advanced++
			}
			if prog.Executed {
				rep.Executed++
			}
			if prog.Escalated {
				rep.Escalated++
			}
			if prog.Blocked {
				rep.Blocked++
			}
		}
		if err := ctx.Err(); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// AdvanceCase walks one case as far as it can go in a single pass.
//
// The case is reloaded before every stage. That costs a query per step and buys
// the guarantee that each stage acts on committed state: if a reviewer rejects a
// case, or a payment webhook recovers it, mid-pass, the next stage sees the new
// status and stops instead of executing against a decision that is no longer live.
func (o *Orchestrator) AdvanceCase(ctx context.Context, caseID string) (Progress, error) {
	prog := Progress{CaseID: caseID}

	for i := 0; i < o.cfg.MaxStages; i++ {
		c, err := o.store.GetCase(ctx, caseID)
		if err != nil {
			return prog, err
		}
		if i == 0 {
			prog.From = c.Status
			prog.Reference = c.Reference
		}
		prog.To = c.Status

		if o.halted(c.Status) {
			return prog, nil
		}

		cc, err := o.loadForStage(ctx, *c)
		if err != nil {
			return prog, err
		}

		stage, err := o.step(ctx, cc, &prog)
		if err != nil {
			return prog, err
		}
		if stage == "" {
			// Nothing left this pass: either the case is waiting on something
			// external, or the stage deliberately stops here.
			return prog, nil
		}
		prog.Stages = append(prog.Stages, stage)
		_ = o.store.IncrCounter(ctx, counterCasesAdvanced)
	}

	// Hitting the bound is not an error for this case — it will be picked up on the
	// next tick — but it is worth recording, because a case that cannot finish in
	// eight transitions is a symptom.
	_ = o.store.Audit(ctx, actor, "case", caseID, caseID, "pipeline_stage_limit",
		map[string]any{"stages": prog.Stages, "status": prog.To})
	return prog, nil
}

// halted reports whether the workflow should leave the case alone.
//
// WAITING_HUMAN and VERIFYING are not terminal, but neither is waiting on the
// orchestrator: one needs a reviewer and the other needs the verifier. Advancing
// them from here would mean acting without the thing they are waiting for.
func (o *Orchestrator) halted(s domain.CaseStatus) bool {
	return domain.IsTerminal(s) || s == domain.StatusWaitingHuman ||
		s == domain.StatusVerifying || s == domain.StatusExecuting
}

// loadForStage builds the fact snapshot with a bounded timeout, so one slow query
// cannot hold a worker for the whole pass.
func (o *Orchestrator) loadForStage(ctx context.Context, c domain.RiskCase) (*caseContext, error) {
	sctx, cancel := context.WithTimeout(ctx, o.cfg.StageTimeout)
	defer cancel()
	return o.load(sctx, c)
}

// step dispatches on case status and returns the stage name it ran, or "" when the
// pass should stop.
func (o *Orchestrator) step(ctx context.Context, cc *caseContext, prog *Progress) (string, error) {
	sctx, cancel := context.WithTimeout(ctx, o.cfg.StageTimeout)
	defer cancel()

	switch cc.Case.Status {
	case domain.StatusNew, domain.StatusRetrying:
		return "detect", o.detectStage(sctx, cc, prog)
	case domain.StatusAnalyzing:
		return "diagnose", o.diagnoseStage(sctx, cc, prog)
	case domain.StatusDiagnosed:
		return "plan", o.planStage(sctx, cc, prog)
	case domain.StatusPlanned:
		return "policy", o.policyStage(sctx, cc, prog)
	case domain.StatusEscalated:
		return "approval", o.approvalStage(sctx, cc, prog)
	case domain.StatusApproved:
		return "execute", o.executeStage(sctx, cc, prog)
	case domain.StatusFailed:
		return "failure", o.failureStage(sctx, cc, prog)
	default:
		return "", nil
	}
}

// --- stage 1: detection (SRS 8.1, FR-020) ---

// detectStage rescores the case and decides whether it is worth working at all.
//
// A RETRYING case comes back through here rather than jumping straight to
// execution, because the facts have changed: an action failed, time has passed, and
// the amount may already have been paid. Re-detecting is how a case that stopped
// being worth chasing exits the loop instead of retrying forever.
func (o *Orchestrator) detectStage(ctx context.Context, cc *caseContext, prog *Progress) error {
	res := o.detect.Detect(ctx, cc.detectionInput())

	if err := o.store.UpdateCaseAssessment(ctx, cc.Case.ID, res.RevenueAtRisk,
		res.RiskScore, res.Urgency, res.ReasonCodes, res.EvidenceRefs); err != nil {
		return fmt.Errorf("save assessment for case %s: %w", cc.Case.ID, err)
	}
	_ = o.store.Audit(ctx, actor, "case", cc.Case.ID, cc.Case.ID, "detection_completed",
		map[string]any{
			"is_at_risk":          res.IsAtRisk,
			"risk_score":          res.RiskScore,
			"urgency":             res.Urgency,
			"revenue_at_risk":     res.RevenueAtRisk,
			"reason_codes":        res.ReasonCodes,
			"source":              res.Source,
			"latency_ms":          res.LatencyMS,
			"injection_suspected": res.InjectionSuspected,
			"fallback_reason":     res.FallbackReason,
		})

	// The money may have arrived while the case sat in the queue. Closing here
	// saves a customer from being chased for a paid invoice, which is the single
	// most damaging thing this system could get wrong (SRS 20.3).
	if cc.Facts.AlreadyPaid {
		prog.Reason = "payment already received"
		return o.transition(ctx, cc, domain.StatusClosed, prog.Reason, prog)
	}

	if !res.IsAtRisk {
		_ = o.store.IncrCounter(ctx, counterNotAtRisk)
		prog.Reason = "detection: not at risk"
		return o.transition(ctx, cc, domain.StatusClosed, prog.Reason, prog)
	}
	return o.transition(ctx, cc, domain.StatusAnalyzing, "", prog)
}

// --- stage 2: diagnosis (SRS 8.2, FR-025) ---

// diagnoseStage establishes why the money did not arrive.
func (o *Orchestrator) diagnoseStage(ctx context.Context, cc *caseContext, prog *Progress) error {
	res := o.diag.Diagnose(ctx, cc.diagnosisInput(o.now()))

	entity := res.Entity(cc.Case.ID)
	if err := o.store.SaveDiagnosis(ctx, &entity); err != nil {
		return fmt.Errorf("save diagnosis for case %s: %w", cc.Case.ID, err)
	}
	_ = o.store.Audit(ctx, actor, "diagnosis", entity.ID, cc.Case.ID, "diagnosis_completed",
		map[string]any{
			"root_cause":          res.RootCause,
			"confidence":          res.Confidence,
			"low_confidence":      res.LowConfidence,
			"uncertainty_flags":   res.UncertaintyFlags,
			"source":              res.Source,
			"latency_ms":          res.LatencyMS,
			"injection_suspected": res.InjectionSuspected,
			"fallback_reason":     res.FallbackReason,
		})

	// SRS 20.4: a diagnosis too weak to justify an unattended action goes to a
	// human. It does not proceed with a guess, and it does not stall.
	if res.LowConfidence {
		prog.Reason = fmt.Sprintf("diagnosis confidence %.2f below %.2f",
			res.Confidence, cc.Policy.MinActionConfidence)
		return o.escalate(ctx, cc, prog.Reason, prog)
	}
	return o.transition(ctx, cc, domain.StatusDiagnosed, "", prog)
}

// --- stage 3: planning (SRS 8.3, FR-030) ---

// planStage chooses one action from the deterministic allow-list.
//
// The diagnosis is re-read from the database rather than carried in memory from the
// previous stage. That is what makes the pipeline resumable: a process that dies
// after diagnosis restarts at planning with the same persisted evidence, and the
// decision the planner records is provably the one derived from the diagnosis a
// reviewer can see.
func (o *Orchestrator) planStage(ctx context.Context, cc *caseContext, prog *Progress) error {
	stored, err := o.store.LatestDiagnosis(ctx, cc.Case.ID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			// Diagnosed with no diagnosis on record should be impossible. Sending
			// the case back to ANALYZING repairs it; guessing a root cause here
			// would not.
			return o.transition(ctx, cc, domain.StatusAnalyzing, "diagnosis missing", prog)
		}
		return fmt.Errorf("load diagnosis for case %s: %w", cc.Case.ID, err)
	}

	res := o.plan.Plan(ctx, cc.plannerInput(diagnosisFrom(*stored, cc.Policy), o.now()))
	decision := res.Entity(cc.Case.ID, cc.Policy.Version)
	if err := o.store.SaveDecision(ctx, &decision); err != nil {
		return fmt.Errorf("save decision for case %s: %w", cc.Case.ID, err)
	}
	prog.Action = res.RecommendedAction
	_ = o.store.Audit(ctx, actor, "decision", decision.ID, cc.Case.ID, "plan_completed",
		map[string]any{
			"recommended_action":   res.RecommendedAction,
			"recovery_probability": res.RecoveryProbability,
			"expected_recovery":    res.ExpectedRecovery,
			"eligible_actions":     res.EligibleActions,
			"alternatives":         res.Alternatives,
			"stop_condition":       res.StopCondition,
			"reason_codes":         res.ReasonCodes,
			"source":               res.Source,
			"latency_ms":           res.LatencyMS,
			"injection_suspected":  res.InjectionSuspected,
			"fallback_reason":      res.FallbackReason,
		})

	// Every decision goes through policy review, including no_action and escalate.
	// Short-circuiting the harmless ones would leave the case detail page with an
	// empty control set exactly where a reviewer most wants to see that the rules
	// ran (SRS 16.2).
	return o.transition(ctx, cc, domain.StatusPolicyReview, "", prog)
}

// diagnosisFrom rebuilds the planner's diagnosis input from the persisted record.
//
// LowConfidence is recomputed against the policy in force now rather than restored
// from the row, so tightening MinActionConfidence takes effect on cases already
// mid-flight instead of only on new ones.
func diagnosisFrom(d domain.Diagnosis, p domain.Policy) agents.DiagnosisResult {
	return agents.DiagnosisResult{
		RootCause:        d.RootCause,
		Confidence:       d.Confidence,
		Evidence:         d.Evidence,
		UncertaintyFlags: d.UncertaintyFlags,
		NextStep:         d.NextStep,
		Source:           d.Source,
		ModelName:        d.ModelName,
		LatencyMS:        d.LatencyMS,
		LowConfidence:    d.Confidence < p.MinActionConfidence,
	}
}

// --- stage 4: policy review (SRS 10, FR-035) ---

// policyStage runs the deterministic rules and decides the case's fate.
//
// This is the gate the rest of the system is built around: whatever the planner
// recommended, nothing executes unless these rules return PASS.
func (o *Orchestrator) policyStage(ctx context.Context, cc *caseContext, prog *Progress) error {
	decision, err := o.store.LatestDecision(ctx, cc.Case.ID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return o.transition(ctx, cc, domain.StatusPolicyReview, "decision missing", prog)
		}
		return fmt.Errorf("load decision for case %s: %w", cc.Case.ID, err)
	}

	// The facts are reloaded with the real decision id, because HasHumanApproval is
	// a fact about *this* decision. Using the snapshot loaded before planning would
	// miss an approval a reviewer granted moments ago, and re-escalate a case the
	// human already cleared.
	facts, err := o.store.LoadPolicyFacts(ctx, cc.Case.ID, cc.Case.CustomerID, decision.ID)
	if err != nil {
		return err
	}
	cc.Facts = facts

	verdict := o.policy.Evaluate(policy.Request{
		Case:                    cc.Case,
		Decision:                *decision,
		Policy:                  cc.Policy,
		TrustedAmount:           cc.TrustedAmount,
		RetryCount:              facts.RetryCount,
		ReminderCount:           facts.ReminderCount,
		CaseActionCount:         facts.CaseActionCount,
		ActionsForCustomerToday: facts.ActionsForCustomerToday,
		LastActionAt:            facts.LastActionAt,
		AlreadyPaid:             facts.AlreadyPaid,
		ConsecutiveAPIFailures:  facts.ConsecutiveAPIFailures,
		APIFailureBudget:        o.cfg.APIFailureBudget,
		HasHumanApproval:        facts.HasHumanApproval,
		Mode:                    cc.Case.Mode,
		Now:                     o.now(),
	})
	prog.Action = decision.RecommendedAction
	prog.Verdict = verdict.Result

	if len(verdict.Checks) > 0 {
		if err := o.store.SavePolicyChecks(ctx, verdict.Checks); err != nil {
			return fmt.Errorf("save policy checks for case %s: %w", cc.Case.ID, err)
		}
	}

	// ERR is recomputed here rather than at planning time because feasibility is a
	// policy output, not a model output: an action the rules will gate is worth less
	// than one that can execute now (SRS 9.2).
	err2 := o.store.UpdateCaseExpectedRecovery(ctx, cc.Case.ID,
		risk.ExpectedRecoverableRevenue(cc.Case.RevenueAtRisk,
			decision.RecoveryProbability, verdict.Feasibility))
	if err2 != nil {
		return fmt.Errorf("update expected recovery for case %s: %w", cc.Case.ID, err2)
	}

	_ = o.store.Audit(ctx, actor, "decision", decision.ID, cc.Case.ID, "policy_evaluated",
		map[string]any{
			"result":        verdict.Result,
			"deciding_rule": verdict.DecidingRule,
			"reason":        verdict.Reason,
			"feasibility":   verdict.Feasibility,
			"stop_reason":   verdict.StopReason,
			"checks":        len(verdict.Checks),
			"action":        decision.RecommendedAction,
		})

	// PLANNED → POLICY_REVIEW is a real transition in the state machine, so the
	// review is visible on the timeline even when the verdict is decided in the
	// same pass.
	if err := o.transition(ctx, cc, domain.StatusPolicyReview, "", prog); err != nil {
		return err
	}
	prog.Reason = verdict.Reason

	switch verdict.Result {
	case domain.PolicyBlock:
		_ = o.store.IncrCounter(ctx, counterPolicyBlocks)
		prog.Blocked = true
		reason := verdict.StopReason
		if reason == "" {
			reason = verdict.Reason
		}
		return o.transition(ctx, cc, domain.StatusBlocked, reason, prog)

	case domain.PolicyEscalate:
		return o.escalate(ctx, cc, verdict.Reason, prog)
	}

	// PASS. The planner may still have chosen not to act, and a deliberate
	// no_action is a legitimate outcome rather than a failure (SRS FR-030).
	switch decision.RecommendedAction {
	case domain.ActionNoAction:
		_ = o.store.IncrCounter(ctx, counterNoAction)
		prog.Reason = "planner selected no_action: " + decision.StopCondition
		return o.transition(ctx, cc, domain.StatusClosed, prog.Reason, prog)
	case domain.ActionEscalate:
		return o.escalate(ctx, cc, "planner selected escalate", prog)
	}
	return o.transition(ctx, cc, domain.StatusApproved, "", prog)
}

// --- stage 5: approval queue (SRS 12, FR-045) ---

// approvalStage puts the case in front of a human and stops the pass.
//
// The approval row is keyed on the decision, and RequestApproval is idempotent, so
// a case that re-enters ESCALATED does not produce a second queue entry for the
// same decision.
func (o *Orchestrator) approvalStage(ctx context.Context, cc *caseContext, prog *Progress) error {
	reason := prog.Reason
	if reason == "" {
		reason = cc.Case.StopReason
	}
	if reason == "" {
		reason = "escalated for human review"
	}

	decision, err := o.store.LatestDecision(ctx, cc.Case.ID)
	if err != nil {
		if !errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("load decision for case %s: %w", cc.Case.ID, err)
		}
		// A case can escalate before a decision exists — a low-confidence diagnosis
		// does exactly that. The approval row requires one, so the case waits in
		// ESCALATED and planning is retried on the next pass rather than a
		// placeholder decision being invented to satisfy the schema.
		_ = o.store.Audit(ctx, actor, "case", cc.Case.ID, cc.Case.ID, "escalation_deferred",
			map[string]any{"reason": "no decision to review yet"})
		return o.transition(ctx, cc, domain.StatusAnalyzing, reason, prog)
	}

	created, err := o.store.RequestApproval(ctx, &domain.Approval{
		CaseID:     cc.Case.ID,
		DecisionID: decision.ID,
		Reason:     reason,
	})
	if err != nil {
		return fmt.Errorf("request approval for case %s: %w", cc.Case.ID, err)
	}
	if created {
		_ = o.store.IncrCounter(ctx, counterEscalations)
		_ = o.store.Audit(ctx, actor, "decision", decision.ID, cc.Case.ID, "approval_requested",
			map[string]any{"reason": reason, "action": decision.RecommendedAction,
				"revenue_at_risk": cc.Case.RevenueAtRisk})
	}
	prog.Escalated = true
	return o.transition(ctx, cc, domain.StatusWaitingHuman, reason, prog)
}

// --- stage 6: execution (SRS 8.4, FR-040) ---

// executeStage performs the approved action.
//
// The orchestrator does not touch Razorpay. It hands the executor a request built
// entirely from the trusted snapshot — amount from the source record, contact from
// the customer row, action from the persisted decision — and the executor validates
// all of it again independently before any call is made (SRS 19.2, 22.4).
func (o *Orchestrator) executeStage(ctx context.Context, cc *caseContext, prog *Progress) error {
	decision, err := o.store.LatestDecision(ctx, cc.Case.ID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return o.transition(ctx, cc, domain.StatusFailed, "no decision to execute", prog)
		}
		return fmt.Errorf("load decision for case %s: %w", cc.Case.ID, err)
	}

	// EXECUTING is recorded before the call, so a crash mid-call leaves a case
	// visibly stuck in EXECUTING rather than looking approved-but-untouched. The
	// reserved action row is what actually prevents a duplicate side effect.
	if err := o.transition(ctx, cc, domain.StatusExecuting, "", prog); err != nil {
		return err
	}

	req := o.buildRequest(cc, *decision)
	res, err := o.exec.Execute(ctx, req)
	prog.Action = req.Action
	prog.Result = &res

	if err != nil {
		// An error from the executor means the attempt did not complete cleanly.
		// The action row already records precisely what happened — executed, failed
		// or ambiguous — so the case moves to FAILED and the failure stage decides
		// what to do next using that record.
		_ = o.store.Audit(ctx, actor, "case", cc.Case.ID, cc.Case.ID, "execution_error",
			map[string]any{"action": req.Action, "error": err.Error()})
		return o.transition(ctx, cc, domain.StatusFailed, "execution error: "+err.Error(), prog)
	}

	switch {
	case res.Executed():
		prog.Executed = true
		// VERIFYING, not RECOVERED: an executed action is a demand for money, not
		// money. Only the verifier banks revenue (SRS 6.1 stage 8).
		return o.transition(ctx, cc, domain.StatusVerifying, "", prog)

	case res.Duplicate:
		// An identical action already exists. If it is still in flight the case must
		// wait for it rather than start a second one; the reconciler and verifier
		// resolve it from the gateway.
		prog.Reason = "duplicate action request; awaiting existing action"
		return o.transition(ctx, cc, domain.StatusVerifying, prog.Reason, prog)

	case res.Rejected:
		prog.Reason = "executor rejected: " + res.RejectReason
		prog.Blocked = true
		// A rejection is the executor's independent validation refusing the action.
		// That is a control working, not a transport failure, so it is terminal for
		// this attempt rather than retryable.
		return o.transition(ctx, cc, domain.StatusBlocked, prog.Reason, prog)

	default:
		prog.Reason = "action not executed: " + string(res.Status)
		return o.transition(ctx, cc, domain.StatusFailed, prog.Reason, prog)
	}
}

// buildRequest assembles the executor contract from trusted state only.
func (o *Orchestrator) buildRequest(cc *caseContext, d domain.AgentDecision) executor.Request {
	attempt := attemptOrdinal(cc.PriorActions, d.RecommendedAction)
	return executor.Request{
		CaseID:             cc.Case.ID,
		Action:             d.RecommendedAction,
		Approved:           true,
		PolicyVersion:      cc.Policy.Version,
		IdempotencyKey:     idem.ActionKey(cc.Case.ID, d.RecommendedAction, attempt),
		TargetAmount:       cc.TrustedAmount,
		RazorpayResourceID: cc.resourceID(),
		DecisionID:         d.ID,
		Mode:               cc.Case.Mode,
		TrustedAmount:      cc.TrustedAmount,
		CustomerID:         cc.Customer.ID,
		CustomerName:       cc.Customer.Name,
		CustomerEmail:      cc.Customer.Email,
		CustomerContact:    cc.Customer.Contact,
		Segment:            cc.Customer.Segment,
		SourceType:         cc.Case.SourceType,
		Description:        cc.description(),
		InvoiceID:          cc.Case.InvoiceID,
		Attempt:            attempt,
	}
}

// attemptOrdinal is the 1-based sequence number of the next action of this type,
// which makes the idempotency key deterministic across restarts.
//
// Only settled attempts are counted. A pending or ambiguous action deliberately
// leaves the ordinal unchanged, so a retry of this stage regenerates the *same*
// key, collides with the existing row and is returned as a duplicate instead of
// producing a second demand for money while the first one's fate is unknown
// (SRS 20.1, 20.2).
func attemptOrdinal(actions []domain.RecoveryAction, t domain.ActionType) int {
	n := 1
	for _, a := range actions {
		if a.ActionType != t {
			continue
		}
		switch a.Status {
		case domain.ActionStatusExecuted, domain.ActionStatusFailed, domain.ActionStatusSkipped:
			n++
		}
	}
	return n
}

// resourceID is the existing Razorpay resource the action operates on. An empty
// value means the action creates a new resource rather than acting on one.
func (cc *caseContext) resourceID() string {
	switch {
	case cc.Transaction != nil:
		return cc.Transaction.RazorpayPaymentID
	case cc.Invoice != nil:
		return cc.Invoice.RazorpayInvoiceID
	case cc.Subscription != nil:
		return cc.Subscription.RazorpaySubscriptionID
	default:
		return ""
	}
}

// description is what the customer sees on a payment link. It names the case
// reference or invoice number and nothing else: this string leaves our systems, so
// it carries no internal reasoning, risk score or diagnosis (SRS 19.1).
func (cc *caseContext) description() string {
	if cc.Invoice != nil && cc.Invoice.InvoiceNumber != "" {
		return "Invoice " + cc.Invoice.InvoiceNumber
	}
	return "Payment " + cc.Case.Reference
}

// --- stage 7: failure handling (SRS 10.1, 20.1) ---

// failureStage decides what happens after an attempt did not produce money.
//
// It deliberately ends the pass. Retrying immediately would ignore the cooldown
// rule, which is what keeps the system from turning one failure into a burst of
// messages to a customer. The case is re-detected on a later tick, by which time
// the cooldown has either elapsed or the case has been recovered elsewhere.
func (o *Orchestrator) failureStage(ctx context.Context, cc *caseContext, prog *Progress) error {
	p := cc.Policy
	exhausted, why := "", ""
	switch {
	case cc.Facts.ConsecutiveAPIFailures >= o.cfg.APIFailureBudget:
		exhausted, why = "api_failure_budget", fmt.Sprintf("%d consecutive execution failures",
			cc.Facts.ConsecutiveAPIFailures)
	case cc.Facts.CaseActionCount >= p.MaxActionsPerCase:
		exhausted, why = "max_actions_per_case", fmt.Sprintf("%d of %d actions used",
			cc.Facts.CaseActionCount, p.MaxActionsPerCase)
	case cc.Facts.RetryCount >= p.MaxRetryCount && cc.Facts.ReminderCount >= p.MaxRemindersPerCase:
		exhausted, why = "retry_and_reminder_limits", fmt.Sprintf("%d retries and %d reminders used",
			cc.Facts.RetryCount, cc.Facts.ReminderCount)
	}

	if exhausted == "" {
		_ = o.store.Audit(ctx, actor, "case", cc.Case.ID, cc.Case.ID, "retry_scheduled",
			map[string]any{
				"case_action_count":        cc.Facts.CaseActionCount,
				"retry_count":              cc.Facts.RetryCount,
				"consecutive_api_failures": cc.Facts.ConsecutiveAPIFailures,
			})
		prog.Reason = "retrying after failed attempt"
		return o.transition(ctx, cc, domain.StatusRetrying, prog.Reason, prog)
	}

	_ = o.store.Audit(ctx, actor, "case", cc.Case.ID, cc.Case.ID, "automation_exhausted",
		map[string]any{"rule": exhausted, "detail": why,
			"revenue_at_risk": cc.Case.RevenueAtRisk})

	// Automation is out of moves. Whether that ends in a person's queue or in a
	// documented write-off depends on whether the amount justifies the attention.
	prog.Reason = "automation exhausted (" + exhausted + "): " + why
	if cc.Case.RevenueAtRisk >= o.cfg.EscalateFailuresAbove {
		return o.escalate(ctx, cc, prog.Reason, prog)
	}
	return o.transition(ctx, cc, domain.StatusClosed, prog.Reason, prog)
}

// --- transitions ---

// escalate moves a case to ESCALATED, which the approval stage then turns into a
// queue entry on the next step of the same pass.
func (o *Orchestrator) escalate(ctx context.Context, cc *caseContext, reason string, prog *Progress) error {
	prog.Escalated = true
	prog.Reason = reason
	return o.transition(ctx, cc, domain.StatusEscalated, reason, prog)
}

// transition applies a state change and keeps Progress in step with it.
//
// An invalid transition is not an error to the caller. It means the case moved
// underneath this pass — a reviewer decided it, a webhook recovered it — and the
// right response is to record that and stop, not to force the state we expected.
func (o *Orchestrator) transition(ctx context.Context, cc *caseContext,
	to domain.CaseStatus, reason string, prog *Progress) error {
	if err := o.store.UpdateCaseStatus(ctx, cc.Case.ID, to, reason); err != nil {
		if errors.Is(err, domain.ErrInvalidTransition) {
			_ = o.store.Audit(ctx, actor, "case", cc.Case.ID, cc.Case.ID, "transition_skipped",
				map[string]any{"from": cc.Case.Status, "to": to, "reason": reason,
					"error": err.Error()})
			return nil
		}
		return fmt.Errorf("case %s %s -> %s: %w", cc.Case.ID, cc.Case.Status, to, err)
	}
	cc.Case.Status = to
	prog.To = to
	return nil
}

// Start runs RunOnce on a ticker until ctx is cancelled.
//
// Errors are reported and the loop continues: a single unworkable case, or a
// transient database problem, must not stop the workflow for every other case in
// the system.
func (o *Orchestrator) Start(ctx context.Context, every time.Duration, onError func(error)) {
	if every <= 0 {
		every = 15 * time.Second
	}
	ticker := time.NewTicker(every)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				rep, err := o.RunOnce(ctx)
				if err != nil && onError != nil {
					onError(err)
				}
				for _, e := range rep.Errors {
					if onError != nil {
						onError(errors.New(e))
					}
				}
			}
		}
	}()
}
