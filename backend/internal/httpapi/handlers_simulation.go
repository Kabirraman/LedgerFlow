package httpapi

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/ledgerflow/ledgerflow/internal/domain"
	"github.com/ledgerflow/ledgerflow/internal/simulation"
)

// simulationRequest configures one benchmark run (SRS 16.4).
//
// Every field that affects the result is client-supplied and echoed back on the
// run record: dataset version, seed, size, strategy and baseline. That is the
// anti-cherry-picking requirement — a number whose generating parameters are not
// recorded alongside it cannot be reproduced or challenged (SRS 25.2).
type simulationRequest struct {
	Strategy domain.StrategyName `json:"strategy"`
	Baseline domain.StrategyName `json:"baseline"`
	Version  string              `json:"version"`
	Seed     int64               `json:"seed"`
	Size     int                 `json:"size"`
	Evaluate bool                `json:"evaluate"`
}

// maxSimulationSize caps a single run. The benchmark is 200 cases (SRS 17.1); the
// ceiling is well above that but finite, because an unbounded size is a way to tie
// up the process from a single request.
const maxSimulationSize = 2000

// runSimulation executes the synthetic benchmark (SRS 17, AC-008).
//
// This route cannot reach Razorpay. The runner is constructed without a gateway and
// without an executor, so there is no object in the call graph that holds a
// credential or knows a URL — the boundary is structural rather than a mode check
// that could be inverted by a wrong flag (SRS AC-009, 23.4).
func (s *Server) runSimulation(c *gin.Context) {
	if s.deps.Simulator == nil {
		notConfigured(c, "the simulation lab")
		return
	}

	var req simulationRequest
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			failWith(c, http.StatusBadRequest, "invalid_body", "expected a JSON object of simulation parameters")
			return
		}
	}

	details := map[string]string{}
	if req.Strategy != "" && !req.Strategy.Valid() {
		details["strategy"] = "unknown strategy " + quote(string(req.Strategy)) + "; expected one of " + joinStrategies()
	}
	if req.Baseline != "" && !req.Baseline.Valid() {
		details["baseline"] = "unknown baseline " + quote(string(req.Baseline)) + "; expected one of " + joinStrategies()
	}
	if req.Size < 0 || req.Size > maxSimulationSize {
		details["size"] = "size must be between 1 and 2000"
	}
	if req.Strategy != "" && req.Strategy == req.Baseline {
		// Comparing a strategy with itself would produce a 0% uplift that looks like
		// a measurement rather than a tautology.
		details["baseline"] = "the baseline must differ from the strategy under test"
	}
	if len(details) > 0 {
		failValidation(c, details)
		return
	}

	ident, _ := identityOf(c)
	run, err := s.deps.Simulator.Run(c.Request.Context(), simulation.Request{
		Strategy:  req.Strategy,
		Baseline:  req.Baseline,
		Version:   strings.TrimSpace(req.Version),
		Seed:      req.Seed,
		Size:      req.Size,
		Now:       s.now(),
		Evaluate:  req.Evaluate,
		CreatedBy: ident.Actor(),
	})
	if err != nil {
		fail(c, err)
		return
	}

	_ = s.deps.Store.Audit(c.Request.Context(), ident.Actor(), "simulation", run.ID, "", "simulation_run",
		map[string]any{
			"strategy": run.Strategy, "baseline": run.Baseline,
			"dataset_version": run.DatasetVersion, "seed": run.Seed, "size": run.Result.CasesProcessed,
		})

	ok(c, simulationResponse(run))
}

// listSimulations returns recent runs.
func (s *Server) listSimulations(c *gin.Context) {
	limit := intQuery(c, "limit", 20, 1, 100)
	runs, err := s.deps.Store.ListRuns(c.Request.Context(), limit)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"runs": runs, "limit": limit, "data_label": "synthetic benchmark"})
}

// getSimulation returns one run with its formatted report.
func (s *Server) getSimulation(c *gin.Context) {
	id, valid := pathID(c, "id")
	if !valid {
		return
	}
	run, err := s.deps.Store.GetRun(c.Request.Context(), id)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, simulationResponse(run))
}

// listDatasets returns the versioned benchmark datasets (SRS 25.2).
func (s *Server) listDatasets(c *gin.Context) {
	limit := intQuery(c, "limit", 20, 1, 100)
	datasets, err := s.deps.Store.ListDatasets(c.Request.Context(), limit)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{
		"datasets": datasets,
		"defaults": gin.H{
			"version":    simulation.DatasetVersion,
			"seed":       simulation.DefaultSeed,
			"size":       simulation.DefaultSize,
			"strategies": domain.AllStrategies,
		},
		// The mix is published so the reader can see the dataset is not weighted
		// toward the cases LEDGERFLOW happens to handle well (SRS 17.1, 25.2).
		"declared_mix": simulation.DefaultMix.Map(),
	})
}

// reportLine is one labelled figure of the SRS 17.4 output block.
type reportLine struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// simulationResponse wraps a run with the report block and its provenance.
func simulationResponse(run *domain.SimulationRun) gin.H {
	body := gin.H{
		"run":    run,
		"report": benchmarkReport(run),
		// Required labelling: these are synthetic benchmark results, and presenting
		// them as live merchant revenue is prohibited (SRS 25.2).
		"data_label": "synthetic benchmark results — not live merchant revenue",
		"reproduce": gin.H{
			"dataset_version": run.DatasetVersion,
			"seed":            run.Seed,
			"policy_version":  run.PolicyVersion,
			"strategy":        run.Strategy,
			"baseline":        run.Baseline,
		},
	}
	if run.Agreement != nil {
		body["agent_evaluation"] = run.Agreement
	}
	return body
}

// benchmarkReport renders the exact output block SRS 17.4 requires, in the order
// it specifies, with the labels it specifies.
//
// It is generated from the persisted result rather than composed by hand at demo
// time, so what a screenshot shows is what the run recorded. When there is no
// baseline the two comparison lines say so instead of printing a zero: an uplift of
// 0% and an uplift that was never measured are different claims (SRS AC-008).
func benchmarkReport(run *domain.SimulationRun) []reportLine {
	r := run.Result
	lines := []reportLine{
		{"Cases processed", itoa(r.CasesProcessed)},
		{"Revenue at risk", "₹" + formatRupees(r.RevenueAtRisk)},
		{"Eligible opportunities", itoa(r.EligibleOpportunities)},
		{"Actions executed", itoa(r.ActionsExecuted)},
		{"Recovered amount", "₹" + formatRupees(r.RecoveredAmount)},
		{"Recovery rate", percent(r.RecoveryRate) + " (" + itoa(r.RecoveredCount) + " of " + itoa(r.ActionsExecuted) + " actions)"},
		{"Escalated", itoa(r.Escalated)},
		{"Stopped safely", itoa(r.StoppedSafely)},
		{"Policy violations", itoa(r.PolicyViolations)},
	}

	if run.BaselineResult == nil {
		lines = append(lines,
			reportLine{"Baseline recovered amount", "not measured — no baseline was selected"},
			reportLine{"LEDGERFLOW uplift", "not measured — no baseline was selected"},
		)
		return lines
	}

	base := run.BaselineResult
	lines = append(lines, reportLine{
		Label: "Baseline recovered amount",
		Value: "₹" + formatRupees(base.RecoveredAmount) + " (" + string(run.Baseline) + ")",
	})

	switch {
	case run.UpliftPercent != nil:
		// Signed, deliberately. AC-008 asks for an honest benchmark, which means a
		// negative number has to be able to appear here.
		lines = append(lines, reportLine{"LEDGERFLOW uplift", signedPercent(*run.UpliftPercent)})
	case base.RecoveredAmount == 0 && r.RecoveredAmount > 0:
		lines = append(lines, reportLine{"LEDGERFLOW uplift",
			"baseline recovered nothing, so a percentage uplift is undefined; absolute gain ₹" +
				formatRupees(r.RecoveredAmount)})
	default:
		lines = append(lines, reportLine{"LEDGERFLOW uplift", "not measurable from this run"})
	}
	return lines
}
