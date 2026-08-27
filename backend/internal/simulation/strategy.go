package simulation

import (
	"context"
	"fmt"
	"time"

	"github.com/ledgerflow/ledgerflow/internal/agents"
	"github.com/ledgerflow/ledgerflow/internal/domain"
	"github.com/ledgerflow/ledgerflow/internal/risk"
)

// Benchmark parameters. Every one of these is a declared configuration of the
// experiment, fixed before any result existed, and versioned with the dataset.
// Tuning one to improve a number would invalidate the comparison, which is why
// they are constants in source rather than knobs on a request (SRS 25.2).
const (
	// MaxRounds is how many times the harness revisits a case. It matches the
	// benchmark policy's per-case action budget: a strategy that wanted a fourth
	// attempt would be blocked anyway, so more rounds would only add noise.
	MaxRounds = 3

	// RoundWaitMinutes is the simulated gap between rounds. It must exceed the
	// policy cooldown, or a strategy that correctly respects the cooldown could
	// never act twice and would be penalised for obeying a control.
	RoundWaitMinutes = 45

	// ApprovalDelayMinutes is how long a human review takes. Escalation is not
	// free — recovery gets slower — and the time-to-recovery figure should show
	// that rather than pretend approval is instant.
	ApprovalDelayMinutes = 90

	// ReviewApprovalRate is the share of escalated cases a reviewer approves.
	ReviewApprovalRate = 0.70
)

// Nominal probabilities the baselines report for their own actions.
//
// A baseline has no model and no calibration, so it cannot estimate per-case
// recovery odds. It states a flat prior instead. This is not a handicap invented
// for the benchmark: it is the actual epistemic position of a retry cron, and the
// calibration metric in SRS 18.2 exists precisely to distinguish it from a system
// that estimates per case.
const (
	nominalRetryOdds    = 0.35
	nominalReminderOdds = 0.30
	nominalLinkOdds     = 0.45
)

// BenchmarkPolicy is the pinned control set every strategy is evaluated under.
//
// It is deliberately not the merchant's live policy from the database. The live
// policy is editable from the admin screen, so a benchmark that read it would
// produce a different uplift next week from the same dataset and seed — the
// reproducibility requirement in NFR-008 would be unmeetable, and the "policy
// context" the SRS asks a run to record would be a moving target (SRS 17.2).
//
// Two thresholds are raised from the shipped defaults because those defaults are
// tuned for a cautious first production deployment, where ₹1,000 unattended is a
// sensible starting point. On a benchmark whose amounts span ₹199 to ₹2.5 lakh
// that setting would escalate nearly everything and the run would measure the
// approval queue rather than the recovery engine.
//
// MinActionConfidence is left at the production 0.70. Loosening a safety control
// to make results look better is exactly the manipulation SRS 25.2 prohibits, and
// unlike the amount ceilings it is not a deployment-caution parameter — it is the
// threshold that decides whether the system may act on a weak diagnosis at all.
func BenchmarkPolicy() domain.Policy {
	p := domain.DefaultPolicy()
	p.Version = "benchmark-v1"
	p.MaxAutomatedAmount = 5_000_000        // ₹50,000
	p.RequireHumanApprovalAbove = 2_500_000 // ₹25,000
	return p
}

// Decision is what a strategy chose for one case in one round.
//
// It embeds domain.AgentDecision because that is the exact type the policy engine
// consumes. A baseline's decision therefore goes through the same engine, the same
// rules and the same verdict as LEDGERFLOW's — the strategies differ in what they
// choose, never in what checks them.
type Decision struct {
	domain.AgentDecision

	// Detection and Diagnosis are set only by strategies that produce them.
	// The evaluator reads them for the SRS 22.3 agent metrics and skips
	// baselines, which have no detection or diagnosis stage to grade.
	Detection *agents.DetectionResult
	Diagnosis *agents.DiagnosisResult
}

// Strategy is one recovery policy under test (SRS 17.3).
//
// The signature is the enforcement point for the benchmark's central rule: a
// strategy receives a *Scenario, which carries no ground truth, and a *caseState,
// which carries only what the harness has already done. There is no parameter
// through which the answer key could arrive.
type Strategy interface {
	Name() domain.StrategyName
	Decide(ctx context.Context, sc *Scenario, st *caseState, p domain.Policy, now time.Time) Decision
}

// NewStrategy builds a strategy by name.
//
// The AI client is used only by the LEDGERFLOW strategy. A nil or disabled client
// is a supported configuration, not an error: the agents fall back to their
// deterministic paths, which is how the benchmark regression runs in CI with no
// Gemini key present (SRS 20.4, 23.3).
func NewStrategy(name domain.StrategyName, client agents.Client) (Strategy, error) {
	switch name {
	case domain.StrategyRetryEverything:
		return retryEverything{}, nil
	case domain.StrategyReminderEverything:
		return reminderEverything{}, nil
	case domain.StrategyStaticHeuristic:
		return staticHeuristic{}, nil
	case domain.StrategyLedgerflow:
		return NewLedgerflow(client), nil
	default:
		return nil, fmt.Errorf("simulation: unknown strategy %q", name)
	}
}

// caseState is the harness's record of what has happened to one case.
//
// It is the simulated stand-in for the store queries the orchestrator runs before
// planning — retry counts, reminder counts, last action time. A strategy is shown
// this state, so a strategy that respects a limit does so because it was told the
// count, not because the harness quietly refused on its behalf.
type caseState struct {
	caseID string

	// contacts counts external actions delivered to the customer, across all
	// action types. It indexes the world model's propensity sequence.
	contacts int

	// actionCount, retryCount and reminderCount are what the policy engine's
	// budget rules read. reminderCount counts only reminders this run sent; the
	// dataset's own prior reminder count is added when the input is built.
	actionCount   int
	retryCount    int
	reminderCount int

	// byType drives attempt decay: the third identical reminder is worth less
	// than the first.
	byType map[domain.ActionType]int

	lastActionAt  *time.Time
	priorActions  []domain.RecoveryAction
	firstActionAt time.Time

	// approved records that the simulated reviewer cleared this case, which
	// downgrades a later ESCALATE to PASS exactly as a real approval does.
	approved bool

	recovered   bool
	recoveredAt time.Time
}

func newCaseState(caseID string) *caseState {
	return &caseState{caseID: caseID, byType: map[domain.ActionType]int{}}
}

// record updates the state after an action was carried out.
func (st *caseState) record(action domain.ActionType, status domain.ActionStatus, amount domain.Money, at time.Time) {
	st.actionCount++
	st.byType[action]++
	switch action {
	case domain.ActionRetry:
		st.retryCount++
	case domain.ActionReminder:
		st.reminderCount++
	}
	if action.IsExternal() {
		st.contacts++
	}
	when := at
	st.lastActionAt = &when
	if st.firstActionAt.IsZero() {
		st.firstActionAt = at
	}
	st.priorActions = append(st.priorActions, domain.RecoveryAction{
		ID:          fmt.Sprintf("%s-a%d", st.caseID, st.actionCount),
		CaseID:      st.caseID,
		ActionType:  action,
		Amount:      amount,
		Status:      status,
		Mode:        domain.ModeSimulation,
		Environment: domain.EnvTest,
		RequestedAt: at,
		ExecutedAt:  &when,
	})
}

// decisionFor assembles a baseline decision.
//
// ExpectedRecovery is computed with the SRS 9.2 ERR formula from the trusted
// amount, the same way the planner does it. A baseline is allowed to be wrong
// about its odds; it is not allowed to be wrong about the money, because then the
// comparison would be measuring two different definitions of expected recovery.
func decisionFor(action domain.ActionType, odds float64, trusted domain.Money, reasons ...string) Decision {
	return Decision{AgentDecision: domain.AgentDecision{
		RecommendedAction:   action,
		RecoveryProbability: odds,
		ExpectedRecovery:    risk.ExpectedRecoverableRevenue(trusted, odds, 1.0),
		ReasonCodes:         reasons,
		Alternatives:        []string{},
		StopCondition:       "baseline: fixed rule, no adaptive stop",
		Source:              "baseline",
	}}
}

// noActionDecision is the decision that spends nothing.
func noActionDecision(reason string) Decision {
	return Decision{AgentDecision: domain.AgentDecision{
		RecommendedAction:   domain.ActionNoAction,
		RecoveryProbability: 0,
		ExpectedRecovery:    0,
		ReasonCodes:         []string{reason},
		Alternatives:        []string{},
		StopCondition:       "no action taken",
		Source:              "baseline",
	}}
}

// retryEverything is the SRS 17.3 retry-everything baseline: re-present every
// failed payment, no diagnosis, no limits.
//
// It is the most common thing merchants actually do, and it is a genuinely strong
// baseline on transient failures — which is why beating it requires the system to
// be right about the failures retrying cannot fix. It ignores its own retry count
// deliberately: the resulting BLOCK verdicts are not a bug in the baseline, they
// are the measurement.
type retryEverything struct{}

func (retryEverything) Name() domain.StrategyName { return domain.StrategyRetryEverything }

func (retryEverything) Decide(_ context.Context, sc *Scenario, _ *caseState, _ domain.Policy, _ time.Time) Decision {
	switch sc.Case.SourceType {
	case domain.SourcePaymentFailure, domain.SourceSubscriptionFailure:
		return decisionFor(domain.ActionRetry, nominalRetryOdds, sc.TrustedAmount, "baseline_retry_every_failure")
	default:
		// An abandoned cart and an unpaid invoice have no authorised payment to
		// re-present. This baseline has no other move, so it makes none.
		return noActionDecision("baseline_nothing_to_retry")
	}
}

// reminderEverything is the SRS 17.3 reminder-everything baseline: email every
// case, every round.
//
// Its weakness is the one the benchmark is designed to expose. Reminders decay
// hard on repetition and are useless on a transient gateway error, so a strategy
// that sends them indiscriminately spends its whole contact budget on cases a
// retry would have recovered for free.
type reminderEverything struct{}

func (reminderEverything) Name() domain.StrategyName { return domain.StrategyReminderEverything }

func (reminderEverything) Decide(_ context.Context, sc *Scenario, _ *caseState, _ domain.Policy, _ time.Time) Decision {
	return decisionFor(domain.ActionReminder, nominalReminderOdds, sc.TrustedAmount, "baseline_remind_every_case")
}

// staticHeuristic is the SRS 17.3 static-heuristic baseline: fixed if/else rules
// with no model.
//
// This is deliberately the strongest of the three. It maps error codes to actions
// the way a competent engineer would after a week with the payment logs, and it
// respects the retry, reminder and amount limits. Making it weak would have been
// easy and would have made the uplift figure worthless — the interesting question
// is whether four agents beat good hand-written rules, not whether they beat bad
// ones.
//
// What it cannot do is reason about the case in front of it: it has one rule per
// error code and no view of customer intent, payment history, or whether this
// specific customer has already ignored two emails.
type staticHeuristic struct{}

func (staticHeuristic) Name() domain.StrategyName { return domain.StrategyStaticHeuristic }

// transientCodes are the error codes this heuristic treats as worth re-presenting.
var transientCodes = map[string]bool{
	"GATEWAY_ERROR":   true,
	"NETWORK_ERROR":   true,
	"PAYMENT_TIMEOUT": true,
}

func (staticHeuristic) Decide(_ context.Context, sc *Scenario, st *caseState, p domain.Policy, _ time.Time) Decision {
	if sc.TrustedAmount <= 0 {
		return noActionDecision("heuristic_nothing_at_stake")
	}
	// A reviewer who has already approved this case has answered the ceiling
	// question, so the heuristic stops asking. Without this the case would
	// escalate forever and the baseline would forfeit every large recovery for a
	// reason no engineer would ship.
	if sc.TrustedAmount > p.MaxAutomatedAmount && !st.approved {
		return decisionFor(domain.ActionEscalate, 0, sc.TrustedAmount, "heuristic_above_autonomous_ceiling")
	}
	if st.actionCount >= p.MaxActionsPerCase {
		return noActionDecision("heuristic_case_budget_spent")
	}

	remindersUsed := sc.Features.ReminderCount + st.reminderCount
	canRemind := remindersUsed < p.MaxRemindersPerCase && sc.HasContact
	canRetry := st.retryCount < p.MaxRetryCount

	switch sc.Case.SourceType {
	case domain.SourcePaymentFailure, domain.SourceSubscriptionFailure:
		if transientCodes[sc.Features.ErrorCode] && canRetry {
			return decisionFor(domain.ActionRetry, nominalRetryOdds, sc.TrustedAmount, "heuristic_transient_error_retry")
		}
		if sc.HasContact {
			return decisionFor(domain.ActionPaymentLink, nominalLinkOdds, sc.TrustedAmount, "heuristic_non_transient_send_link")
		}
		return decisionFor(domain.ActionEscalate, 0, sc.TrustedAmount, "heuristic_no_contact_channel")

	case domain.SourceCheckoutAbandonment, domain.SourceInvoiceOverdue:
		if canRemind {
			return decisionFor(domain.ActionReminder, nominalReminderOdds, sc.TrustedAmount, "heuristic_remind_open_receivable")
		}
		if sc.HasContact {
			return decisionFor(domain.ActionPaymentLink, nominalLinkOdds, sc.TrustedAmount, "heuristic_reminder_budget_spent")
		}
		return decisionFor(domain.ActionEscalate, 0, sc.TrustedAmount, "heuristic_no_contact_channel")

	default:
		return noActionDecision("heuristic_unhandled_source_type")
	}
}

// Ledgerflow is the system under test: the real Detection, Diagnosis and Planner
// agents, in sequence, on the same cases as the baselines.
//
// Nothing here decides anything. It is a three-call adapter, and that is the
// point — if the benchmark contained decision logic of its own, the result would
// describe the benchmark rather than the product (SRS 22.3).
type Ledgerflow struct {
	detection *agents.DetectionAgent
	diagnosis *agents.DiagnosisAgent
	planner   *agents.PlannerAgent
}

// NewLedgerflow wires the three agents to one AI client.
func NewLedgerflow(client agents.Client) *Ledgerflow {
	return &Ledgerflow{
		detection: agents.NewDetectionAgent(client),
		diagnosis: agents.NewDiagnosisAgent(client),
		planner:   agents.NewPlannerAgent(client),
	}
}

func (*Ledgerflow) Name() domain.StrategyName { return domain.StrategyLedgerflow }

func (s *Ledgerflow) Decide(ctx context.Context, sc *Scenario, st *caseState, p domain.Policy, now time.Time) Decision {
	det := s.detection.Detect(ctx, sc.DetectionInput(p))

	// Detection is graded on every case, including the ones it declines, because
	// recall is measured on what it let through as much as precision on what it
	// flagged. So the result is attached even when the pipeline stops here.
	if !det.IsAtRisk {
		d := noActionDecision("detection_not_at_risk")
		d.Source = det.Source
		d.Detection = &det
		return d
	}

	diag := s.diagnosis.Diagnose(ctx, sc.DiagnosisInput(p, det, st, now))
	plan := s.planner.Plan(ctx, sc.PlannerInput(p, diag, st, now))

	d := Decision{AgentDecision: plan.Entity(sc.Case.ID, p.Version)}
	d.Detection = &det
	d.Diagnosis = &diag
	return d
}
