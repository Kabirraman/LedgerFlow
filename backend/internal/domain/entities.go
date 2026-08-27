package domain

import (
	"encoding/json"
	"time"
)

// Money is a monetary amount in the smallest currency unit (paise), matching
// Razorpay's API convention. Integers avoid float rounding drift in recovery
// totals. ₹1,000.00 is Money(100000).
type Money int64

// Rupees renders the amount as a float for display only. Never use the result
// for arithmetic that feeds a stored total.
func (m Money) Rupees() float64 { return float64(m) / 100.0 }

// Customer holds the context and segmentation the agents reason over.
type Customer struct {
	ID                 string  `json:"id"`
	RazorpayCustomerID string  `json:"razorpay_customer_id,omitempty"`
	Name               string  `json:"name"`
	Email              string  `json:"email,omitempty"`
	Contact            string  `json:"contact,omitempty"`
	Segment            Segment `json:"segment"`
	LifetimeValue      Money   `json:"lifetime_value"`
	// SuccessRate is the historical share of successful payments in [0,1].
	SuccessRate   float64     `json:"success_rate"`
	TotalPayments int         `json:"total_payments"`
	Environment   Environment `json:"environment"`
	CreatedAt     time.Time   `json:"created_at"`
}

// Transaction is a payment-level fact. Amount and Status are trusted inputs:
// agents may read them but never overwrite them (SRS 19.2).
type Transaction struct {
	ID                string      `json:"id"`
	RazorpayPaymentID string      `json:"razorpay_payment_id,omitempty"`
	RazorpayOrderID   string      `json:"razorpay_order_id,omitempty"`
	CustomerID        string      `json:"customer_id"`
	Amount            Money       `json:"amount"`
	Currency          string      `json:"currency"`
	Status            string      `json:"status"`
	Method            string      `json:"method,omitempty"`
	FailureReason     string      `json:"failure_reason,omitempty"`
	ErrorCode         string      `json:"error_code,omitempty"`
	AttemptCount      int         `json:"attempt_count"`
	Environment       Environment `json:"environment"`
	CreatedAt         time.Time   `json:"created_at"`
}

// CheckoutSession tracks demo-checkout intent so abandonment is observed from
// first-party events rather than inferred from nonexistent Razorpay events
// (SRS 11.2).
type CheckoutSession struct {
	ID             string      `json:"id"`
	CustomerID     string      `json:"customer_id"`
	CartAmount     Money       `json:"cart_amount"`
	ItemCount      int         `json:"item_count"`
	PageViews      int         `json:"page_views"`
	StartedAt      time.Time   `json:"started_at"`
	LastActivityAt time.Time   `json:"last_activity_at"`
	Status         string      `json:"status"` // active | abandoned | converted
	Environment    Environment `json:"environment"`
}

// Invoice backs the B2B receivable workflow (SRS 11.3).
type Invoice struct {
	ID                string      `json:"id"`
	RazorpayInvoiceID string      `json:"razorpay_invoice_id,omitempty"`
	CustomerID        string      `json:"customer_id"`
	InvoiceNumber     string      `json:"invoice_number"`
	Amount            Money       `json:"amount"`
	AmountPaid        Money       `json:"amount_paid"`
	Status            string      `json:"status"`
	DueDate           time.Time   `json:"due_date"`
	ReminderCount     int         `json:"reminder_count"`
	Environment       Environment `json:"environment"`
	CreatedAt         time.Time   `json:"created_at"`
}

// Subscription backs the recurring-billing workflow (SRS 11.4).
type Subscription struct {
	ID                     string      `json:"id"`
	RazorpaySubscriptionID string      `json:"razorpay_subscription_id,omitempty"`
	CustomerID             string      `json:"customer_id"`
	PlanID                 string      `json:"plan_id,omitempty"`
	Amount                 Money       `json:"amount"`
	Status                 string      `json:"status"`
	FailedChargeCount      int         `json:"failed_charge_count"`
	CurrentEnd             time.Time   `json:"current_end"`
	Environment            Environment `json:"environment"`
	CreatedAt              time.Time   `json:"created_at"`
}

// RiskCase is the unit of recovery work: the object that carries diagnosis,
// decision, policy checks, actions and outcome (SRS 14.1).
type RiskCase struct {
	ID         string     `json:"id"`
	Reference  string     `json:"reference"` // human-facing, e.g. REV-0182
	SourceType SourceType `json:"source_type"`
	CustomerID string     `json:"customer_id"`

	// Exactly one source pointer is set, matching SourceType.
	TransactionID     string `json:"transaction_id,omitempty"`
	CheckoutSessionID string `json:"checkout_session_id,omitempty"`
	InvoiceID         string `json:"invoice_id,omitempty"`
	SubscriptionID    string `json:"subscription_id,omitempty"`

	RevenueAtRisk Money   `json:"revenue_at_risk"`
	RiskScore     float64 `json:"risk_score"`
	Urgency       Urgency `json:"urgency"`
	// ExpectedRecovery is ERR from SRS 9.2; recomputed whenever the planner runs.
	ExpectedRecovery Money       `json:"expected_recovery"`
	RecoveredAmount  Money       `json:"recovered_amount"`
	Status           CaseStatus  `json:"status"`
	ReasonCodes      []string    `json:"reason_codes"`
	EvidenceRefs     []string    `json:"evidence_refs"`
	StopReason       string      `json:"stop_reason,omitempty"`
	ActionCount      int         `json:"action_count"`
	Mode             RunMode     `json:"mode"`
	SimulationID     string      `json:"simulation_id,omitempty"`
	Environment      Environment `json:"environment"`
	CreatedAt        time.Time   `json:"created_at"`
	UpdatedAt        time.Time   `json:"updated_at"`
	ClosedAt         *time.Time  `json:"closed_at,omitempty"`
}

// Diagnosis is the Diagnosis Agent's structured output (SRS 8.2).
type Diagnosis struct {
	ID               string    `json:"id"`
	CaseID           string    `json:"case_id"`
	RootCause        RootCause `json:"root_cause"`
	Confidence       float64   `json:"confidence"`
	Evidence         []string  `json:"evidence"`
	UncertaintyFlags []string  `json:"uncertainty_flags"`
	NextStep         string    `json:"next_step"`
	// Source records whether a model or the deterministic fallback produced
	// this diagnosis, so evaluation can separate the two (SRS 20.4).
	Source    string    `json:"source"`
	ModelName string    `json:"model_name,omitempty"`
	LatencyMS int64     `json:"latency_ms"`
	CreatedAt time.Time `json:"created_at"`
}

// AgentDecision is the Intervention Planner's structured output (SRS 8.3).
type AgentDecision struct {
	ID                  string     `json:"id"`
	CaseID              string     `json:"case_id"`
	RecommendedAction   ActionType `json:"recommended_action"`
	RecoveryProbability float64    `json:"recovery_probability"`
	ExpectedRecovery    Money      `json:"expected_recovery"`
	ReasonCodes         []string   `json:"reason_codes"`
	Alternatives        []string   `json:"alternatives"`
	StopCondition       string     `json:"stop_condition"`
	PolicyVersion       string     `json:"policy_version"`
	Source              string     `json:"source"`
	ModelName           string     `json:"model_name,omitempty"`
	LatencyMS           int64      `json:"latency_ms"`
	CreatedAt           time.Time  `json:"created_at"`
}

// PolicyCheck is one deterministic rule evaluation. Every rule that ran is
// persisted, not just the failing one, so reviewers see the whole control set
// (SRS 16.2).
type PolicyCheck struct {
	ID            string       `json:"id"`
	DecisionID    string       `json:"decision_id"`
	CaseID        string       `json:"case_id"`
	PolicyVersion string       `json:"policy_version"`
	Rule          string       `json:"rule"`
	Result        PolicyResult `json:"result"`
	Details       string       `json:"details"`
	CreatedAt     time.Time    `json:"created_at"`
}

// RecoveryAction records an external side effect. The idempotency key is
// unique in the database, which is what actually prevents duplicate side
// effects (SRS FR-043, 20.1).
type RecoveryAction struct {
	ID             string       `json:"id"`
	CaseID         string       `json:"case_id"`
	DecisionID     string       `json:"decision_id"`
	ActionType     ActionType   `json:"action_type"`
	IdempotencyKey string       `json:"idempotency_key"`
	ExternalID     string       `json:"external_id,omitempty"`
	ExternalURL    string       `json:"external_url,omitempty"`
	Amount         Money        `json:"amount"`
	Status         ActionStatus `json:"status"`
	ErrorCode      string       `json:"error_code,omitempty"`
	ErrorMessage   string       `json:"error_message,omitempty"`
	AttemptCount   int          `json:"attempt_count"`
	Mode           RunMode      `json:"mode"`
	Environment    Environment  `json:"environment"`
	RequestedAt    time.Time    `json:"requested_at"`
	ExecutedAt     *time.Time   `json:"executed_at,omitempty"`
	LatencyMS      int64        `json:"latency_ms"`
}

// Event is the raw + normalized event history. PayloadJSON keeps the original
// body so a case can be replayed without loss (SRS FR-004).
type Event struct {
	ID              string          `json:"id"`
	Source          string          `json:"source"`
	ExternalEventID string          `json:"external_event_id"`
	EventType       string          `json:"event_type"`
	PayloadJSON     json.RawMessage `json:"payload_json"`
	SignatureValid  bool            `json:"signature_valid"`
	RejectionReason string          `json:"rejection_reason,omitempty"`
	EntityID        string          `json:"entity_id,omitempty"`
	EntityTimestamp *time.Time      `json:"entity_timestamp,omitempty"`
	ReceivedAt      time.Time       `json:"received_at"`
	ProcessedAt     *time.Time      `json:"processed_at,omitempty"`
	Environment     Environment     `json:"environment"`
}

// Approval is the human-in-the-loop record (SRS FR-045, 10.4).
type Approval struct {
	ID           string           `json:"id"`
	CaseID       string           `json:"case_id"`
	DecisionID   string           `json:"decision_id"`
	Reason       string           `json:"reason"` // why approval was required
	RequestedAt  time.Time        `json:"requested_at"`
	Reviewer     string           `json:"reviewer,omitempty"`
	Decision     ApprovalDecision `json:"decision"`
	DecisionNote string           `json:"decision_note,omitempty"`
	DecidedAt    *time.Time       `json:"decided_at,omitempty"`
}

// Outcome is the verified result of an action (SRS FR-051).
type Outcome struct {
	ID              string      `json:"id"`
	CaseID          string      `json:"case_id"`
	ActionID        string      `json:"action_id,omitempty"`
	Outcome         OutcomeType `json:"outcome"`
	RecoveredAmount Money       `json:"recovered_amount"`
	RecoveredAt     *time.Time  `json:"recovered_at,omitempty"`
	// TimeToRecoverySeconds is measured from case creation to verification.
	TimeToRecoverySeconds int64     `json:"time_to_recovery_seconds"`
	VerificationSource    string    `json:"verification_source"`
	Notes                 string    `json:"notes,omitempty"`
	CreatedAt             time.Time `json:"created_at"`
}

// AuditLog is the append-only decision trail (SRS FR-052).
type AuditLog struct {
	ID          string          `json:"id"`
	Actor       string          `json:"actor"`
	EntityType  string          `json:"entity_type"`
	EntityID    string          `json:"entity_id"`
	CaseID      string          `json:"case_id,omitempty"`
	ActionID    string          `json:"action_id,omitempty"`
	EventType   string          `json:"event_type"`
	PayloadJSON json.RawMessage `json:"payload_json,omitempty"`
	Timestamp   time.Time       `json:"timestamp"`
}

// StrategyMetric aggregates intervention performance per segment and action
// so the planner can prefer evidence-backed strategies (SRS FR-053).
type StrategyMetric struct {
	ID              string     `json:"id"`
	Segment         Segment    `json:"segment"`
	SourceType      SourceType `json:"source_type"`
	ActionType      ActionType `json:"action_type"`
	Attempts        int        `json:"attempts"`
	Successes       int        `json:"successes"`
	RecoveredAmount Money      `json:"recovered_amount"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

// SuccessRate is the observed recovery rate, or -1 when there is not enough
// data to report one. Callers must not treat a small sample as a strong prior.
func (s StrategyMetric) SuccessRate() float64 {
	if s.Attempts == 0 {
		return -1
	}
	return float64(s.Successes) / float64(s.Attempts)
}

// Policy is the merchant-configured control set (SRS 10.1). Amounts are paise.
type Policy struct {
	Version                     string    `json:"version"`
	MaxRetryCount               int       `json:"max_retry_count"`
	MaxAutomatedAmount          Money     `json:"max_automated_amount"`
	MinActionConfidence         float64   `json:"min_action_confidence"`
	CooldownMinutes             int       `json:"cooldown_minutes"`
	MaxActionsPerCustomerPerDay int       `json:"max_actions_per_customer_per_day"`
	RequireHumanApprovalAbove   Money     `json:"require_human_approval_above"`
	MaxRemindersPerCase         int       `json:"max_reminders_per_case"`
	MaxActionsPerCase           int       `json:"max_actions_per_case"`
	UpdatedAt                   time.Time `json:"updated_at"`
	UpdatedBy                   string    `json:"updated_by,omitempty"`
}

// DefaultPolicy returns the SRS 10.1 baseline. Values are intentionally
// conservative: the prototype prefers escalation over autonomous action.
func DefaultPolicy() Policy {
	return Policy{
		Version:                     "v1",
		MaxRetryCount:               2,
		MaxAutomatedAmount:          500000, // ₹5,000
		MinActionConfidence:         0.70,
		CooldownMinutes:             30,
		MaxActionsPerCustomerPerDay: 3,
		RequireHumanApprovalAbove:   100000, // ₹1,000
		MaxRemindersPerCase:         2,
		MaxActionsPerCase:           3,
	}
}

// User is an operator/reviewer/admin of the dashboard (SRS 15.1).
type User struct {
	ID           string    `json:"id"`
	Email        string    `json:"email"`
	Name         string    `json:"name"`
	Role         Role      `json:"role"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
}
