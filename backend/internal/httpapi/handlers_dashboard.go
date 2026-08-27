package httpapi

import (
	"net/http"
	"sort"

	"github.com/gin-gonic/gin"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// health reports whether the process can serve traffic.
//
// The database check is the whole point: a process that has lost its pool answers
// HTTP fine but cannot recover a rupee, so reporting it healthy would keep a
// broken instance in a load balancer.
func (s *Server) health(c *gin.Context) {
	body := gin.H{
		"status":  "ok",
		"time":    s.now(),
		"env":     s.deps.Config.AppEnv,
		"mode":    s.deps.Config.Razorpay.Mode,
		"version": buildVersion,
	}
	if err := s.deps.Store.Ping(c.Request.Context()); err != nil {
		body["status"] = "degraded"
		body["database"] = "unreachable"
		c.JSON(http.StatusServiceUnavailable, body)
		return
	}
	body["database"] = "ok"
	ok(c, body)
}

// buildVersion identifies the build. Overridden at link time with
// -ldflags "-X github.com/ledgerflow/ledgerflow/internal/httpapi.buildVersion=..."
var buildVersion = "dev"

// version reports what this deployment is wired to.
//
// Every field is a name or a boolean. No key, secret or URL containing
// credentials is exposed, and the Razorpay mode is reported explicitly so a demo
// audience can see the system is in test mode rather than being told (SRS 23.4,
// 25.2).
func (s *Server) version(c *gin.Context) {
	gatewayName, external := "none", false
	if s.deps.Gateway != nil {
		gatewayName, external = s.deps.Gateway.Name(), s.deps.Gateway.External()
	}
	ok(c, gin.H{
		"version":             buildVersion,
		"environment":         s.deps.Config.AppEnv,
		"razorpay_mode":       s.deps.Config.Razorpay.Mode,
		"razorpay_configured": s.deps.Config.Razorpay.Configured(),
		"gateway":             gatewayName,
		"gateway_external":    external,
		"model_configured":    s.deps.Config.Gemini.Configured(),
		"model":               s.deps.Config.Gemini.Model,
		"auto_execute":        s.deps.Config.AutoExecuteApproved,
		"live_mode_supported": false,
	})
}

// dashboardSummary is the KPI block from SRS 16.1 and 18.1.
//
// Every figure is computed by the store from persisted outcomes. The handler adds
// nothing, because a KPI assembled in the API layer would be a second definition
// of recovered revenue (SRS 25.2).
func (s *Server) dashboardSummary(c *gin.Context) {
	summary, err := s.deps.Store.DashboardSummary(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{
		"summary": summary,
		// The provenance label travels with the numbers so a screenshot cannot be
		// mistaken for live merchant revenue (SRS 25.2).
		"data_label": "razorpay test mode",
		"as_of":      s.now(),
	})
}

// strategyPerformanceRow is one learning-loop row, flattened for display.
type strategyPerformanceRow struct {
	Segment         domain.Segment    `json:"segment"`
	SourceType      domain.SourceType `json:"source_type"`
	ActionType      domain.ActionType `json:"action_type"`
	Attempts        int               `json:"attempts"`
	Successes       int               `json:"successes"`
	RecoveredAmount domain.Money      `json:"recovered_amount"`
	// SuccessRate is nil when there is no sample, rather than 0. A strategy that
	// has never been tried and a strategy that always fails must not look the same
	// on a dashboard (SRS 18.2).
	SuccessRate *float64 `json:"success_rate"`
	// Sufficient marks rows with enough attempts to read as a signal. The
	// threshold is reported so the reader can judge it rather than trust it.
	Sufficient bool `json:"sufficient"`
}

// minAttemptsForSignal is how many attempts a strategy row needs before its
// success rate is worth acting on. Deliberately low for a prototype, and
// deliberately explicit.
const minAttemptsForSignal = 5

// strategyPerformance reports the learning loop from SRS 13.6 and 18.2.
func (s *Server) strategyPerformance(c *gin.Context) {
	metrics, err := s.deps.Store.ListStrategyMetrics(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}

	rows := make([]strategyPerformanceRow, 0, len(metrics))
	var totalAttempts, totalSuccesses int
	var totalRecovered domain.Money
	for _, m := range metrics {
		row := strategyPerformanceRow{
			Segment:         m.Segment,
			SourceType:      m.SourceType,
			ActionType:      m.ActionType,
			Attempts:        m.Attempts,
			Successes:       m.Successes,
			RecoveredAmount: m.RecoveredAmount,
			Sufficient:      m.Attempts >= minAttemptsForSignal,
		}
		if rate := m.SuccessRate(); rate >= 0 {
			row.SuccessRate = &rate
		}
		rows = append(rows, row)
		totalAttempts += m.Attempts
		totalSuccesses += m.Successes
		totalRecovered += m.RecoveredAmount
	}

	// Best-performing first, but only among rows with a usable sample: a 1/1 row
	// would otherwise top the table at 100%.
	sort.SliceStable(rows, func(i, j int) bool {
		li, lj := rows[i].Sufficient, rows[j].Sufficient
		if li != lj {
			return li
		}
		ri, rj := 0.0, 0.0
		if rows[i].SuccessRate != nil {
			ri = *rows[i].SuccessRate
		}
		if rows[j].SuccessRate != nil {
			rj = *rows[j].SuccessRate
		}
		if ri != rj {
			return ri > rj
		}
		return rows[i].RecoveredAmount > rows[j].RecoveredAmount
	})

	overall := 0.0
	if totalAttempts > 0 {
		overall = float64(totalSuccesses) / float64(totalAttempts)
	}
	ok(c, gin.H{
		"strategies": rows,
		"totals": gin.H{
			"attempts":            totalAttempts,
			"successes":           totalSuccesses,
			"recovered_amount":    totalRecovered,
			"success_rate":        overall,
			"min_attempts_signal": minAttemptsForSignal,
		},
		"data_label": "razorpay test mode",
	})
}

// opsMetrics exposes the operational counters from SRS 18.3.
func (s *Server) opsMetrics(c *gin.Context) {
	counters, err := s.deps.Store.Counters(c.Request.Context())
	if err != nil {
		fail(c, err)
		return
	}
	flat := make(map[string]gin.H, len(counters))
	for name, v := range counters {
		flat[name] = gin.H{"count": v.Count, "sum": v.Sum, "mean": v.Mean()}
	}
	ok(c, gin.H{"counters": flat, "as_of": s.now()})
}

// listEvents returns recent ingested events, including the ones rejected for a bad
// signature. Those are the interesting ones: a rejected event that never appeared
// anywhere would make a misconfigured webhook secret invisible (SRS FR-002).
func (s *Server) listEvents(c *gin.Context) {
	limit := intQuery(c, "limit", 50, 1, 200)
	events, err := s.deps.Store.ListEvents(c.Request.Context(), limit)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"events": events, "limit": limit})
}

// notConfigured reports a route whose optional collaborator was not wired, which
// is a deployment state rather than a client error.
func notConfigured(c *gin.Context, what string) {
	failWith(c, http.StatusServiceUnavailable, "not_configured", what+" is not enabled in this deployment")
}
