package domain

import "time"

// StrategyName identifies a policy under test in the Simulation Lab
// (SRS 17.3).
type StrategyName string

const (
	// StrategyRetryEverything retries every eligible payment failure.
	StrategyRetryEverything StrategyName = "retry_everything"
	// StrategyReminderEverything reminds every abandoned/overdue case.
	StrategyReminderEverything StrategyName = "reminder_everything"
	// StrategyStaticHeuristic picks an action from fixed if/else rules.
	StrategyStaticHeuristic StrategyName = "static_heuristic"
	// StrategyLedgerflow is the four-agent decision engine plus policy engine.
	StrategyLedgerflow StrategyName = "ledgerflow"
)

var AllStrategies = []StrategyName{
	StrategyRetryEverything,
	StrategyReminderEverything,
	StrategyStaticHeuristic,
	StrategyLedgerflow,
}

func (s StrategyName) Valid() bool {
	for _, v := range AllStrategies {
		if v == s {
			return true
		}
	}
	return false
}

// BenchmarkCase is one synthetic case with ground truth. Ground-truth fields
// are used only for evaluation, never as agent input — otherwise the benchmark
// would grade the agent on data it was handed (SRS 17.2).
type BenchmarkCase struct {
	ID         string     `json:"id"`
	DatasetID  string     `json:"dataset_id"`
	SourceType SourceType `json:"source_type"`

	// Transaction / receivable facts.
	Amount        Money  `json:"amount"`
	Method        string `json:"method,omitempty"`
	ErrorCode     string `json:"error_code,omitempty"`
	FailureReason string `json:"failure_reason,omitempty"`
	AttemptCount  int    `json:"attempt_count"`
	AgeMinutes    int    `json:"age_minutes"`
	// SourceStatus is the status on the underlying record — a transaction that is
	// failed or captured, an invoice that is issued or paid, a subscription that
	// is halted or cancelled. Empty means "the usual status for this source type".
	SourceStatus string `json:"source_status,omitempty"`
	// AmountPaid is money already received against this receivable. Only the
	// balance is collectable, so this is what stops a partially paid invoice from
	// being chased for its gross amount.
	AmountPaid Money `json:"amount_paid,omitempty"`
	// AlreadyPaid means external state shows the money has arrived. It is an
	// observable fact, not ground truth: it is exactly what the policy engine's
	// already-recovered stopping rule reads, and a strategy that acts anyway has
	// committed a real violation.
	AlreadyPaid bool `json:"already_paid,omitempty"`
	// NoContact means the customer has neither email nor phone on record, so no
	// intervention can be delivered to them at all.
	NoContact bool `json:"no_contact,omitempty"`

	// Customer facts.
	Segment             Segment `json:"segment"`
	CustomerSuccessRate float64 `json:"customer_success_rate"`
	LifetimeValue       Money   `json:"lifetime_value"`
	RecencyDays         int     `json:"recency_days"`
	// TotalPayments is how many payments this customer has ever attempted. The
	// risk scorer needs it to tell a 0.5 success rate over two payments from the
	// same rate over forty, which mean very different things.
	TotalPayments int `json:"total_payments"`

	// Behaviour facts.
	CheckoutViews       int `json:"checkout_views"`
	MinutesSinceAbandon int `json:"minutes_since_abandon,omitempty"`
	DaysOverdue         int `json:"days_overdue,omitempty"`

	// History facts.
	PriorRecoveries    int `json:"prior_recoveries"`
	PriorFailedActions int `json:"prior_failed_actions"`
	ReminderCount      int `json:"reminder_count"`

	// Ground truth (SRS 17.2). Held out from agent prompts.
	Recoverable         bool       `json:"recoverable"`
	BenchmarkBestAction ActionType `json:"benchmark_best_action"`
	// AcceptableActions is the approved equivalent set; a selection inside this
	// set counts as correct for intervention accuracy (SRS 3.2).
	AcceptableActions []ActionType `json:"acceptable_actions"`
	TrueRootCause     RootCause    `json:"true_root_cause"`
	// RecoveryProbabilityByAction is the simulated ground-truth response curve
	// used to resolve outcomes deterministically.
	RecoveryProbabilityByAction map[ActionType]float64 `json:"recovery_probability_by_action"`
	IsEdgeCase                  bool                   `json:"is_edge_case"`
	EdgeCaseKind                string                 `json:"edge_case_kind,omitempty"`
}

// BenchmarkDataset is a versioned, seeded synthetic dataset. Versioning the
// seed is what makes runs reproducible and blocks cherry-picking
// (SRS 25.2, NFR-008).
type BenchmarkDataset struct {
	ID        string          `json:"id"`
	Version   string          `json:"version"`
	Seed      int64           `json:"seed"`
	Size      int             `json:"size"`
	Mix       map[string]int  `json:"mix"`
	CreatedAt time.Time       `json:"created_at"`
	Cases     []BenchmarkCase `json:"cases,omitempty"`
}

// SimulationRun is the persisted result of one strategy over one dataset.
type SimulationRun struct {
	ID             string            `json:"id"`
	DatasetID      string            `json:"dataset_id"`
	DatasetVersion string            `json:"dataset_version"`
	Seed           int64             `json:"seed"`
	PolicyVersion  string            `json:"policy_version"`
	Strategy       StrategyName      `json:"strategy"`
	Baseline       StrategyName      `json:"baseline"`
	Status         string            `json:"status"`
	Result         SimulationResult  `json:"result"`
	BaselineResult *SimulationResult `json:"baseline_result,omitempty"`
	// UpliftPercent is (Y-B)/B * 100 as defined in SRS 17.4.
	UpliftPercent *float64         `json:"uplift_percent,omitempty"`
	Agreement     *AgentEvaluation `json:"agent_evaluation,omitempty"`
	StartedAt     time.Time        `json:"started_at"`
	FinishedAt    *time.Time       `json:"finished_at,omitempty"`
	DurationMS    int64            `json:"duration_ms"`
	CreatedBy     string           `json:"created_by,omitempty"`
}

// SimulationResult is the required output block from SRS 17.4.
type SimulationResult struct {
	Strategy              StrategyName   `json:"strategy"`
	CasesProcessed        int            `json:"cases_processed"`
	RevenueAtRisk         Money          `json:"revenue_at_risk"`
	EligibleOpportunities int            `json:"eligible_opportunities"`
	EligibleAmount        Money          `json:"eligible_amount"`
	ActionsExecuted       int            `json:"actions_executed"`
	RecoveredAmount       Money          `json:"recovered_amount"`
	RecoveredCount        int            `json:"recovered_count"`
	RecoveryRate          float64        `json:"recovery_rate"`
	Escalated             int            `json:"escalated"`
	StoppedSafely         int            `json:"stopped_safely"`
	Blocked               int            `json:"blocked"`
	PolicyViolations      int            `json:"policy_violations"`
	Errors                int            `json:"errors"`
	AvgTimeToRecoveryMin  float64        `json:"avg_time_to_recovery_min"`
	ActionBreakdown       map[string]int `json:"action_breakdown"`
}

// AgentEvaluation holds the AI evaluation metrics from SRS 22.3.
type AgentEvaluation struct {
	DetectionPrecision   float64 `json:"detection_precision"`
	DetectionRecall      float64 `json:"detection_recall"`
	DetectionF1          float64 `json:"detection_f1"`
	DiagnosisAccuracy    float64 `json:"diagnosis_accuracy"`
	InterventionAccuracy float64 `json:"intervention_accuracy"`
	// InterventionsGraded and InterventionsDeferred split the planner's decisions
	// into the ones accuracy was measured on and the ones handed to a human.
	//
	// Both numbers are reported because the accuracy figure alone is gameable in
	// the most obvious way available to a cautious planner: escalate everything and
	// be graded on the handful of cases left. Publishing the denominator next to
	// the ratio makes that immediately visible instead of hidden.
	InterventionsGraded   int `json:"interventions_graded"`
	InterventionsDeferred int `json:"interventions_deferred"`
	// CalibrationError is the mean absolute gap between predicted recovery
	// probability and observed outcome, bucketed (SRS 18.2).
	CalibrationError float64 `json:"calibration_error"`
	// CalibrationSamples is how many executed actions had a stated probability
	// describing that same action, which is the only pairing calibration can be
	// measured on.
	CalibrationSamples int     `json:"calibration_samples"`
	EvidenceCoverage   float64 `json:"evidence_coverage"`
	// ModelCalls is how many agent calls actually reached the model and returned
	// a response. SchemaValidRate is a share of this number, so without it a rate
	// of 1.0 over zero calls would read as a perfect score instead of as an
	// evaluation that never tested the model — which is exactly what happens in
	// CI, where the agents run their deterministic paths with no API key.
	ModelCalls          int     `json:"model_calls"`
	SchemaValidRate     float64 `json:"schema_valid_rate"`
	UnauthorizedActions int     `json:"unauthorized_actions"`
}

// DashboardSummary backs GET /api/dashboard/summary (SRS 16.1).
type DashboardSummary struct {
	RevenueAtRisk            Money              `json:"revenue_at_risk"`
	ExpectedRecovery         Money              `json:"expected_recovery"`
	RecoveredAmount          Money              `json:"recovered_amount"`
	RecoveryRate             float64            `json:"recovery_rate"`
	AutomatedActions         int                `json:"automated_actions"`
	EscalatedCases           int                `json:"escalated_cases"`
	BlockedActions           int                `json:"blocked_actions"`
	OpenCases                int                `json:"open_cases"`
	UnresolvedRevenue        Money              `json:"unresolved_revenue"`
	AvgRecoveryMinutes       float64            `json:"avg_recovery_minutes"`
	RecoveredPerIntervention Money              `json:"recovered_per_intervention"`
	EscalationRate           float64            `json:"escalation_rate"`
	PolicyViolations         int                `json:"policy_violations"`
	Funnel                   RecoveryFunnel     `json:"funnel"`
	BySource                 []SourceBreakdown  `json:"by_source"`
	Activity                 []ActivityItem     `json:"activity"`
	Operational              OperationalMetrics `json:"operational"`
}

// RecoveryFunnel is the identified → diagnosed → actioned → recovered funnel.
type RecoveryFunnel struct {
	Identified int `json:"identified"`
	Diagnosed  int `json:"diagnosed"`
	Actioned   int `json:"actioned"`
	Recovered  int `json:"recovered"`
}

// SourceBreakdown groups KPIs by workflow.
type SourceBreakdown struct {
	SourceType    SourceType `json:"source_type"`
	Cases         int        `json:"cases"`
	RevenueAtRisk Money      `json:"revenue_at_risk"`
	Recovered     Money      `json:"recovered"`
	RecoveryRate  float64    `json:"recovery_rate"`
}

// ActivityItem is one line in the live activity feed (SRS 16.1).
type ActivityItem struct {
	CaseID     string     `json:"case_id"`
	Reference  string     `json:"reference"`
	Kind       string     `json:"kind"` // executed | blocked | escalated | recovered
	ActionType ActionType `json:"action_type,omitempty"`
	Amount     Money      `json:"amount"`
	Detail     string     `json:"detail"`
	At         time.Time  `json:"at"`
}

// OperationalMetrics backs SRS 18.3.
type OperationalMetrics struct {
	WebhooksReceived         int     `json:"webhooks_received"`
	WebhookSignatureFailures int     `json:"webhook_signature_failures"`
	DuplicateEvents          int     `json:"duplicate_events"`
	DuplicateEventRate       float64 `json:"duplicate_event_rate"`
	ActionAPIFailures        int     `json:"action_api_failures"`
	AvgActionLatencyMS       float64 `json:"avg_action_latency_ms"`
	AvgAgentLatencyMS        float64 `json:"avg_agent_latency_ms"`
	AgentFallbackCount       int     `json:"agent_fallback_count"`
}

// CaseDetail is the aggregate returned by GET /api/cases/:id (SRS 16.2).
type CaseDetail struct {
	Case         RiskCase         `json:"case"`
	Customer     *Customer        `json:"customer,omitempty"`
	Transaction  *Transaction     `json:"transaction,omitempty"`
	Checkout     *CheckoutSession `json:"checkout_session,omitempty"`
	Invoice      *Invoice         `json:"invoice,omitempty"`
	Subscription *Subscription    `json:"subscription,omitempty"`
	Diagnosis    *Diagnosis       `json:"diagnosis,omitempty"`
	Decision     *AgentDecision   `json:"decision,omitempty"`
	PolicyChecks []PolicyCheck    `json:"policy_checks"`
	Actions      []RecoveryAction `json:"actions"`
	Approvals    []Approval       `json:"approvals"`
	Outcomes     []Outcome        `json:"outcomes"`
	Timeline     []TimelineItem   `json:"timeline"`
}

// TimelineItem is one ordered entry in the case timeline (SRS 16.2).
type TimelineItem struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Title  string    `json:"title"`
	Detail string    `json:"detail,omitempty"`
	Result string    `json:"result,omitempty"`
	Actor  string    `json:"actor,omitempty"`
}

// CaseFilter drives the queue listing (SRS 16.1 filters).
type CaseFilter struct {
	SourceType SourceType
	Segment    Segment
	Status     CaseStatus
	ActionType ActionType
	MinRisk    float64
	Mode       RunMode
	Search     string
	Limit      int
	Offset     int
	SortBy     string // expected_recovery | risk_score | created_at | revenue_at_risk
}

// CasePage is a paginated case listing.
type CasePage struct {
	Items  []CaseListItem `json:"items"`
	Total  int            `json:"total"`
	Limit  int            `json:"limit"`
	Offset int            `json:"offset"`
}

// CaseListItem is the flattened row shown in the queue.
type CaseListItem struct {
	RiskCase
	CustomerName      string       `json:"customer_name"`
	CustomerSegment   Segment      `json:"customer_segment"`
	RootCause         RootCause    `json:"root_cause,omitempty"`
	Confidence        float64      `json:"confidence,omitempty"`
	RecommendedAction ActionType   `json:"recommended_action,omitempty"`
	PolicyResult      PolicyResult `json:"policy_result,omitempty"`
}

// ApprovalQueueItem is one row of the review queue (SRS 16.3).
//
// It carries the case and decision context a reviewer needs to judge the request
// without opening the case: what is being proposed, for how much money, with what
// confidence, and why it needed a human at all.
type ApprovalQueueItem struct {
	Approval
	Reference         string     `json:"reference"`
	SourceType        SourceType `json:"source_type"`
	CustomerName      string     `json:"customer_name"`
	CustomerSegment   Segment    `json:"customer_segment"`
	CaseStatus        CaseStatus `json:"case_status"`
	RevenueAtRisk     Money      `json:"revenue_at_risk"`
	ExpectedRecovery  Money      `json:"expected_recovery"`
	RiskScore         float64    `json:"risk_score"`
	Urgency           Urgency    `json:"urgency"`
	RecommendedAction ActionType `json:"recommended_action"`
	Confidence        float64    `json:"confidence"`
	ReasonCodes       []string   `json:"reason_codes"`
	// AlreadyExecuted is true when an action for this decision has already been
	// performed. Such a row is excluded from the queue: approving it would invite a
	// reviewer to authorise something that already happened (SRS 16.3).
	AlreadyExecuted bool `json:"already_executed"`
	// WaitingMinutes is how long the request has been queued, which is the number a
	// reviewer needs to spot a case going stale.
	WaitingMinutes int `json:"waiting_minutes"`
}
