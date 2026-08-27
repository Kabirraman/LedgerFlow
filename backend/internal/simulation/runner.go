package simulation

import (
	"context"
	"fmt"
	"time"

	"github.com/ledgerflow/ledgerflow/internal/agents"
	"github.com/ledgerflow/ledgerflow/internal/domain"
	"github.com/ledgerflow/ledgerflow/internal/policy"
)

// Store is the persistence the runner needs, and nothing more.
//
// The interface is declared here, by the consumer, rather than in the store
// package: it names four methods out of the store's several dozen, so a reader of
// this file can see the runner's entire reach into the database without leaving it
// (SRS NFR-007). It is also what makes an in-memory or nil store a legitimate
// configuration for the CI benchmark, where no Postgres exists (SRS 23.3).
type Store interface {
	SaveDataset(ctx context.Context, d *domain.BenchmarkDataset) error
	StartRun(ctx context.Context, r *domain.SimulationRun) error
	FinishRun(ctx context.Context, r *domain.SimulationRun) error
	FailRun(ctx context.Context, id, reason string) error
}

// Runner executes benchmark runs.
//
// It has no gateway field and no executor field. The absence is the safety
// property: there is no configuration of this struct, and no bug in it, that could
// produce an HTTP request to Razorpay. Simulation results are computed by the
// world model in this package (SRS AC-009, FR-054, 22.4).
type Runner struct {
	engine *policy.Engine
	client agents.Client
	store  Store
}

// NewRunner builds a runner. store may be nil, in which case results are returned
// but not persisted. client may be nil or disabled, in which case the agents use
// their deterministic paths.
func NewRunner(client agents.Client, st Store) *Runner {
	return &Runner{engine: policy.New(), client: client, store: st}
}

// Request configures one benchmark run (SRS 17.2).
type Request struct {
	// Strategy is the system under test.
	Strategy domain.StrategyName
	// Baseline is what it is compared against. Empty means no comparison, so no
	// uplift figure is produced — an uplift number without a named baseline is
	// meaningless and the SRS 17.4 block requires the baseline to be reported
	// alongside it.
	Baseline domain.StrategyName

	Version string
	Seed    int64
	Size    int

	// Now anchors every relative age in the dataset. Injected so a run is
	// reproducible rather than dependent on when it was started.
	Now time.Time

	// Evaluate turns on the SRS 22.3 agent metrics. They need ground truth and a
	// per-case trace, so they cost memory proportional to the dataset.
	Evaluate bool

	CreatedBy string
}

func (req *Request) normalise() {
	if req.Version == "" {
		req.Version = DatasetVersion
	}
	if req.Seed == 0 {
		req.Seed = DefaultSeed
	}
	if req.Size <= 0 {
		req.Size = DefaultSize
	}
	if req.Now.IsZero() {
		req.Now = time.Now().UTC()
	}
	if req.Strategy == "" {
		req.Strategy = domain.StrategyLedgerflow
	}
}

// Run executes the strategy, then the baseline, over one dataset.
//
// Both strategies see the identical dataset and the identical world model, so any
// difference in recovered amount is attributable to the actions they chose. That
// is the only claim this benchmark makes, and it is the only one it can support.
func (r *Runner) Run(ctx context.Context, req Request) (*domain.SimulationRun, error) {
	req.normalise()
	if !req.Strategy.Valid() {
		return nil, fmt.Errorf("simulation: unknown strategy %q", req.Strategy)
	}
	if req.Baseline != "" && !req.Baseline.Valid() {
		return nil, fmt.Errorf("simulation: unknown baseline %q", req.Baseline)
	}
	if req.Baseline == req.Strategy {
		return nil, fmt.Errorf("simulation: baseline %q cannot be the strategy under test", req.Baseline)
	}

	pol := BenchmarkPolicy()
	dataset := GenerateDataset(req.Version, req.Seed, req.Size)
	if r.store != nil {
		// A stored dataset is what makes a published uplift checkable: the exact
		// cases can be re-read rather than regenerated on trust (SRS 25.2).
		if err := r.store.SaveDataset(ctx, &dataset); err != nil {
			return nil, fmt.Errorf("save dataset: %w", err)
		}
	}

	run := &domain.SimulationRun{
		DatasetID:      dataset.ID,
		DatasetVersion: dataset.Version,
		Seed:           dataset.Seed,
		PolicyVersion:  pol.Version,
		Strategy:       req.Strategy,
		Baseline:       req.Baseline,
		StartedAt:      req.Now,
		CreatedBy:      req.CreatedBy,
	}
	if r.store != nil {
		if err := r.store.StartRun(ctx, run); err != nil {
			return nil, fmt.Errorf("start run: %w", err)
		}
	}

	// From here on a failure has a persisted row to mark, so errors are recorded
	// rather than leaving a run stuck in 'running' forever.
	fail := func(err error) (*domain.SimulationRun, error) {
		if r.store != nil && run.ID != "" {
			_ = r.store.FailRun(ctx, run.ID, err.Error())
		}
		return nil, err
	}

	primary, err := NewStrategy(req.Strategy, r.client)
	if err != nil {
		return fail(err)
	}
	world := NewWorld(dataset.Seed)

	result, traces, err := r.runStrategy(ctx, primary, dataset.Cases, world, pol, req.Now, req.Evaluate)
	if err != nil {
		return fail(err)
	}
	run.Result = result

	if req.Baseline != "" {
		base, err := NewStrategy(req.Baseline, r.client)
		if err != nil {
			return fail(err)
		}
		baseResult, _, err := r.runStrategy(ctx, base, dataset.Cases, world, pol, req.Now, false)
		if err != nil {
			return fail(err)
		}
		run.BaselineResult = &baseResult
		if up, ok := uplift(result.RecoveredAmount, baseResult.RecoveredAmount); ok {
			run.UpliftPercent = &up
		}
	}

	if req.Evaluate {
		eval := Evaluate(traces)
		run.Agreement = &eval
	}

	finished := req.Now
	run.FinishedAt = &finished
	if r.store != nil {
		if err := r.store.FinishRun(ctx, run); err != nil {
			return nil, fmt.Errorf("finish run: %w", err)
		}
	}
	return run, nil
}

// uplift is the SRS 17.4 figure: (Y-B)/B as a percentage.
//
// It returns false when the baseline recovered nothing. A division by zero would
// yield an infinite uplift, which is not a stronger claim than a finite one — it
// is no claim at all, and reporting it as a number would be misleading.
func uplift(strategy, baseline domain.Money) (float64, bool) {
	if baseline <= 0 {
		return 0, false
	}
	return float64(strategy-baseline) / float64(baseline) * 100, true
}

// caseTrace is what the evaluator needs about one case: the ground truth, and what
// the strategy did on its first pass at it.
//
// Only the first round is traced. A case revisited three times would otherwise
// contribute three detection verdicts to a precision figure that is meant to be
// per case, and the second and third are conditioned on the first having acted.
type caseTrace struct {
	Case      domain.BenchmarkCase
	Detection *agents.DetectionResult
	Diagnosis *agents.DiagnosisResult
	Decision  Decision
	Verdict   domain.PolicyResult

	// Executed is the action actually carried out first, which may differ from
	// the recommendation when a reviewer authorised an alternative.
	Executed domain.ActionType

	// Recovered is the case-level outcome across all rounds. Calibration
	// compares it against the probability stated in the first round, which is
	// the estimate the system was willing to act on.
	Recovered bool
}

// respectsPolicy reports whether a strategy's actions are gated by the policy
// engine's verdict.
//
// Only LEDGERFLOW is. This is the most consequential modelling decision in the
// benchmark, so it is stated plainly rather than buried: the baselines represent
// what a merchant gets *without* this system, and a retry cron has no policy
// engine to stop it. Gating them would credit LEDGERFLOW's safety layer to its own
// competitors, suppress their recovered amount, and inflate the uplift figure.
//
// The comparison is therefore harder than it needs to be — baselines are allowed
// to do things LEDGERFLOW is forbidden from doing — and their policy-violation
// count is what that latitude costs them. It is also why violations never pay: a
// case the engine blocks as already-paid or unrecoverable has a zero response
// curve, so barging through it returns nothing but a violation.
func respectsPolicy(name domain.StrategyName) bool {
	return name == domain.StrategyLedgerflow
}

// runStrategy runs one strategy over every case.
func (r *Runner) runStrategy(
	ctx context.Context,
	s Strategy,
	cases []domain.BenchmarkCase,
	world World,
	pol domain.Policy,
	now time.Time,
	trace bool,
) (domain.SimulationResult, []caseTrace, error) {
	res := domain.SimulationResult{
		Strategy:        s.Name(),
		ActionBreakdown: map[string]int{},
	}
	var traces []caseTrace
	if trace {
		traces = make([]caseTrace, 0, len(cases))
	}
	var recoveryMinutes float64

	for i := range cases {
		if err := ctx.Err(); err != nil {
			return res, nil, err
		}
		t := r.runCase(ctx, s, cases[i], world, pol, now, &res)
		if t.Recovered {
			recoveryMinutes += t.recoveryMinutes
		}
		if trace {
			traces = append(traces, t.caseTrace)
		}
	}

	if res.RevenueAtRisk > 0 {
		// Identical to the definition in store/analytics.go. The dashboard and the
		// benchmark must not be able to disagree about what a recovery rate is.
		res.RecoveryRate = float64(res.RecoveredAmount) / float64(res.RevenueAtRisk)
	}
	if res.RecoveredCount > 0 {
		res.AvgTimeToRecoveryMin = recoveryMinutes / float64(res.RecoveredCount)
	}
	return res, traces, nil
}

// caseOutcome is one case's trace plus the timing the aggregate needs.
type caseOutcome struct {
	caseTrace
	recoveryMinutes float64
}

// runCase plays one case through up to MaxRounds interventions.
//
// The loop is the SRS 2.3 product loop with the external calls replaced by the
// world model: decide, check policy, execute, verify, and either recover or come
// back later. Every counter the SRS 17.4 output block requires is accumulated
// here, at the point the thing being counted actually happens, rather than
// reconstructed afterwards from a log.
func (r *Runner) runCase(
	ctx context.Context,
	s Strategy,
	b domain.BenchmarkCase,
	world World,
	pol domain.Policy,
	now time.Time,
	res *domain.SimulationResult,
) caseOutcome {
	sc := NewScenario(b, now)
	st := newCaseState(b.ID)
	out := caseOutcome{caseTrace: caseTrace{Case: b}}
	gated := respectsPolicy(s.Name())

	res.CasesProcessed++
	res.RevenueAtRisk += sc.TrustedAmount

	var (
		countedEscalation bool
		countedBlock      bool
		countedEligible   bool
	)
	clock := now

	for round := 0; round < MaxRounds; round++ {
		d := s.Decide(ctx, sc, st, pol, clock)

		if round == 0 {
			out.Detection = d.Detection
			out.Diagnosis = d.Diagnosis
			out.Decision = d
		}

		chosen := d.RecommendedAction
		if !chosen.Valid() {
			// A strategy that proposes an action outside the allow-list is a
			// harness-level anomaly, not a recovery outcome. It is counted as an
			// error and the case stops; silently coercing it to no_action would
			// hide a real defect (SRS 22.4, 20.4).
			res.Errors++
			break
		}
		res.ActionBreakdown[string(chosen)]++

		if chosen == domain.ActionNoAction {
			st.record(chosen, domain.ActionStatusSkipped, 0, clock)
			break
		}

		verdict := r.evaluate(sc, st, d, chosen, pol, clock)
		if round == 0 {
			out.Verdict = verdict.Result
		}

		switch verdict.Result {
		case domain.PolicyBlock:
			if !countedBlock {
				res.Blocked++
				countedBlock = true
			}
			if gated {
				// A BLOCK is final. This is the hard stop from SRS 10.3, and the
				// reason LEDGERFLOW's violation count is zero by construction.
				st.record(chosen, domain.ActionStatusSkipped, 0, clock)
				return r.finish(out, st, res)
			}
			if chosen.IsExternal() {
				res.PolicyViolations++
			}

		case domain.PolicyEscalate:
			if !countedEscalation {
				res.Escalated++
				countedEscalation = true
			}
			if !gated {
				// An ungated strategy never waits for a human; it just acts.
				break
			}
			if !world.reviewApproves(b.ID, ReviewApprovalRate) {
				// A reviewer declining is a real terminal outcome for the case
				// and a working safety control, not a system failure.
				st.record(chosen, domain.ActionStatusSkipped, 0, clock)
				return r.finish(out, st, res)
			}
			st.approved = true
			clock = clock.Add(ApprovalDelayMinutes * time.Minute)

			// A reviewer approves a decision, so what they authorise is what the
			// strategy asked for. When the strategy asked for a hand-off, the
			// reviewer carries out the strategy's own stated second choice — the
			// harness reads the alternative rather than choosing one, so no
			// decision logic leaks into the benchmark.
			chosen = authorisedAction(d)
			if !chosen.IsExternal() {
				st.record(domain.ActionEscalate, domain.ActionStatusSkipped, 0, clock)
				return r.finish(out, st, res)
			}
			// The substituted action is re-checked from scratch. Approval lifts an
			// escalation; it must never let an unchecked action through.
			verdict = r.evaluate(sc, st, d, chosen, pol, clock)
			if verdict.Result != domain.PolicyPass {
				if verdict.Result == domain.PolicyBlock && !countedBlock {
					res.Blocked++
					countedBlock = true
				}
				st.record(chosen, domain.ActionStatusSkipped, 0, clock)
				return r.finish(out, st, res)
			}
		}

		if !chosen.IsExternal() {
			// escalate reached here only for an ungated strategy, which has no
			// review queue to hand off to. Nothing executes and the case ends.
			st.record(chosen, domain.ActionStatusSkipped, 0, clock)
			break
		}

		if !countedEligible {
			res.EligibleOpportunities++
			res.EligibleAmount += sc.TrustedAmount
			countedEligible = true
		}
		res.ActionsExecuted++

		outcome := world.Resolve(b, chosen, st.contacts+1, st.byType[chosen])
		st.record(chosen, domain.ActionStatusExecuted, sc.TrustedAmount, clock)
		if out.Executed == "" {
			out.Executed = chosen
		}

		if outcome.Recovered {
			st.recovered = true
			st.recoveredAt = clock
			res.RecoveredCount++
			res.RecoveredAmount += sc.TrustedAmount
			out.recoveryMinutes = clock.Sub(now).Minutes()
			break
		}

		// Verification found no payment. Wait out the cooldown and try again if
		// the strategy still has budget.
		clock = clock.Add(RoundWaitMinutes * time.Minute)
	}

	return r.finish(out, st, res)
}

// finish records the case-level outcome.
func (r *Runner) finish(out caseOutcome, st *caseState, res *domain.SimulationResult) caseOutcome {
	out.Recovered = st.recovered
	// StoppedSafely counts cases where the customer was never contacted at all.
	// Each one is either a correct decline or a forfeited opportunity, and the
	// pair (stopped safely, recovery rate) is what tells them apart — a strategy
	// cannot raise this number without either declining correctly or giving up
	// revenue, and both show elsewhere in the block.
	if st.contacts == 0 && !st.recovered {
		res.StoppedSafely++
	}
	return out
}

// evaluate runs the real policy engine for one proposed action.
//
// The decision handed to the engine carries the substituted action rather than the
// strategy's original recommendation, because the engine must check what will
// actually execute. Everything else — trusted amount, counts, timestamps — comes
// from the harness's own record, which is the simulated equivalent of the store
// reads the orchestrator performs.
func (r *Runner) evaluate(
	sc *Scenario,
	st *caseState,
	d Decision,
	chosen domain.ActionType,
	pol domain.Policy,
	at time.Time,
) policy.Verdict {
	dec := d.AgentDecision
	dec.RecommendedAction = chosen
	// ExpectedRecovery must never exceed the trusted amount, and the engine
	// blocks on that (rule: amount integrity). Leaving a stale figure from a
	// substituted action would trip it spuriously, so it is recomputed.
	if dec.ExpectedRecovery > sc.TrustedAmount {
		dec.ExpectedRecovery = sc.TrustedAmount
	}
	return r.engine.Evaluate(policy.Request{
		Case:                    sc.Case,
		Decision:                dec,
		Policy:                  pol,
		TrustedAmount:           sc.TrustedAmount,
		RetryCount:              st.retryCount,
		ReminderCount:           sc.Features.ReminderCount + st.reminderCount,
		CaseActionCount:         st.actionCount,
		ActionsForCustomerToday: st.actionCount,
		LastActionAt:            st.lastActionAt,
		AlreadyPaid:             sc.AlreadyPaid,
		HasHumanApproval:        st.approved,
		Mode:                    domain.ModeSimulation,
		Now:                     at,
	})
}

// authorisedAction resolves what a reviewer carries out for an escalated case.
//
// For any concrete recommendation it is that recommendation. For a hand-off it is
// the first external action on the strategy's own alternatives list, and no_action
// when the strategy offered no external alternative — a case nobody could act on
// safely stays unactioned.
func authorisedAction(d Decision) domain.ActionType {
	if d.RecommendedAction != domain.ActionEscalate {
		return d.RecommendedAction
	}
	for _, alt := range d.Alternatives {
		a, err := domain.ParseActionType(alt)
		if err == nil && a.IsExternal() {
			return a
		}
	}
	return domain.ActionNoAction
}
