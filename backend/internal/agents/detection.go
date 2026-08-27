package agents

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ledgerflow/ledgerflow/internal/domain"
	"github.com/ledgerflow/ledgerflow/internal/risk"
)

// DetectionInput is the trusted fact set handed to the Detection Agent
// (SRS 7 Agent 1). Every field is read from the database or the synthetic
// dataset.
type DetectionInput struct {
	SourceType domain.SourceType
	Features   risk.Features

	// Free-text context. Untrusted: sanitised before it enters the prompt.
	FailureReason string
	CustomerName  string
	Method        string
	PaymentStatus string
	InvoiceNumber string
	PlanID        string

	// PolicySummary is the short, human-readable control set given to the model
	// so its reasoning is aware of the limits it operates under.
	PolicySummary string
}

// DetectionResult is the reconciled detection verdict.
type DetectionResult struct {
	IsAtRisk      bool           `json:"is_at_risk"`
	RiskScore     float64        `json:"risk_score"`
	RevenueAtRisk domain.Money   `json:"revenue_at_risk"`
	Urgency       domain.Urgency `json:"urgency"`
	ReasonCodes   []string       `json:"reason_codes"`
	EvidenceRefs  []string       `json:"evidence_refs"`

	// Components is the deterministic per-term breakdown, kept for the UI.
	Components risk.Components `json:"components"`

	// ModelRiskScore is what the model proposed, retained for calibration
	// metrics only. It never drives a decision.
	ModelRiskScore float64 `json:"model_risk_score,omitempty"`

	// Source is "ai" or "deterministic". Presenting a fallback as model output
	// would misrepresent the system, so this is recorded, surfaced and audited
	// (SRS 25.2).
	Source    string `json:"source"`
	ModelName string `json:"model_name,omitempty"`
	LatencyMS int64  `json:"latency_ms"`

	// InjectionSuspected reports that untrusted evidence contained something
	// shaped like an instruction.
	InjectionSuspected bool `json:"injection_suspected,omitempty"`
	// FallbackReason explains why the deterministic path was used.
	FallbackReason string `json:"fallback_reason,omitempty"`
}

// detectionModelOutput mirrors the SRS 8.1 schema exactly.
type detectionModelOutput struct {
	IsAtRisk      bool     `json:"is_at_risk"`
	RiskScore     float64  `json:"risk_score"`
	RevenueAtRisk int64    `json:"revenue_at_risk"`
	Urgency       string   `json:"urgency"`
	ReasonCodes   []string `json:"reason_codes"`
	EvidenceRefs  []string `json:"evidence_refs"`
}

func detectionSchema() schema {
	return objectSchema(
		[]string{"is_at_risk", "risk_score", "revenue_at_risk", "urgency", "reason_codes", "evidence_refs"},
		map[string]any{
			"is_at_risk":      boolSchema(),
			"risk_score":      numberSchema(),
			"revenue_at_risk": integerSchema(),
			"urgency":         enumSchema([]string{"low", "medium", "high", "critical"}),
			"reason_codes":    stringArraySchema(),
			"evidence_refs":   stringArraySchema(),
		})
}

// DetectionAgent finds revenue-at-risk opportunities.
//
// The deterministic scorer runs first and is authoritative for money and score;
// the model contributes explanation and may only *raise* risk. That split is
// what keeps the SRS 9.1 formula honest — a model-generated risk score would
// make the documented weights fiction — while still using the model for the
// judgement it is actually good at.
type DetectionAgent struct {
	client Client
}

// NewDetectionAgent constructs the agent. A nil or disabled client is valid and
// yields deterministic-only behaviour.
func NewDetectionAgent(c Client) *DetectionAgent { return &DetectionAgent{client: c} }

// Detect returns a detection verdict, never an error: detection must always
// produce an answer, and the deterministic scorer is always available.
func (a *DetectionAgent) Detect(ctx context.Context, in DetectionInput) DetectionResult {
	base := risk.Score(in.Features)
	out := DetectionResult{
		IsAtRisk:      base.IsAtRisk,
		RiskScore:     base.RiskScore,
		RevenueAtRisk: base.RevenueAtRisk,
		Urgency:       base.Urgency,
		ReasonCodes:   base.ReasonCodes,
		EvidenceRefs:  base.EvidenceRefs,
		Components:    base.Components,
		Source:        "deterministic",
	}

	if a == nil || a.client == nil || !a.client.Enabled() {
		out.FallbackReason = "model_disabled"
		return out
	}

	ev := buildDetectionEvidence(in, base)
	out.InjectionSuspected = ev.Suspicious()

	started := time.Now()
	raw, err := a.client.Generate(ctx, detectionSystemPrompt, detectionUserPrompt(ev), detectionSchema())
	out.LatencyMS = time.Since(started).Milliseconds()
	if err != nil {
		out.FallbackReason = fallbackReason(err)
		return out
	}

	var model detectionModelOutput
	if err := decodeStrict(raw, &model); err != nil {
		out.FallbackReason = "invalid_json"
		return out
	}

	out.ModelName = a.client.Name()
	out.ModelRiskScore = clamp01(model.RiskScore)

	// Reconciliation. Note what is NOT taken from the model:
	//   - revenue_at_risk stays the trusted database amount (SRS 19.2).
	//   - risk_score stays the SRS 9.1 formula result.
	// The model may only escalate risk, never suppress it, so a confused or
	// manipulated model cannot make a real case disappear (SRS 20.4).
	if model.IsAtRisk && !out.IsAtRisk && in.Features.Amount > 0 {
		out.IsAtRisk = true
		out.RevenueAtRisk = in.Features.Amount
		out.ReasonCodes = appendCode(out.ReasonCodes, "model_flagged_risk")
	}
	if u := domain.Urgency(strings.ToLower(strings.TrimSpace(model.Urgency))); u.Valid() && u.Rank() > out.Urgency.Rank() {
		out.Urgency = u
	}
	out.ReasonCodes = mergeCodes(out.ReasonCodes, sanitizeCodes(model.ReasonCodes, 8))
	out.EvidenceRefs = mergeCodes(out.EvidenceRefs, sanitizeCodes(model.EvidenceRefs, 8))
	if ev.Suspicious() {
		out.ReasonCodes = appendCode(out.ReasonCodes, "prompt_injection_detected")
	}
	out.Source = "ai"
	return out
}

func buildDetectionEvidence(in DetectionInput, base risk.Assessment) *evidenceBuilder {
	f := in.Features
	ev := newEvidence()

	ev.Add("case.source_type", string(in.SourceType))
	ev.AddMoney("transaction.amount", int64(f.Amount))
	ev.Add("transaction.currency", "INR")
	ev.Add("transaction.status", in.PaymentStatus)
	ev.Add("transaction.error_code", f.ErrorCode)
	ev.AddText("transaction.failure_reason", in.FailureReason)
	ev.Add("transaction.method", in.Method)
	ev.Add("transaction.attempt_count", f.AttemptCount)
	ev.Add("transaction.age_minutes", f.AgeMinutes)

	ev.Section("Customer")
	ev.AddText("customer.name", in.CustomerName)
	ev.Add("customer.segment", string(f.Segment))
	ev.Add("customer.success_rate", fmt.Sprintf("%.2f", f.CustomerSuccessRate))
	ev.AddMoney("customer.lifetime_value", int64(f.LifetimeValue))
	ev.Add("customer.total_payments", f.TotalPayments)
	ev.Add("customer.days_since_last_activity", f.RecencyDays)

	ev.Section("Workflow context")
	switch in.SourceType {
	case domain.SourceCheckoutAbandonment:
		ev.Add("checkout_session.page_views", f.CheckoutViews)
		ev.Add("checkout_session.minutes_since_last_activity", f.MinutesSinceAbandon)
	case domain.SourceInvoiceOverdue:
		ev.Add("invoice.number", in.InvoiceNumber)
		ev.Add("invoice.days_overdue", f.DaysOverdue)
		ev.Add("invoice.reminder_count", f.ReminderCount)
	case domain.SourceSubscriptionFailure:
		ev.Add("subscription.plan_id", in.PlanID)
		ev.Add("subscription.failed_charge_count", f.AttemptCount)
	}

	ev.Section("Intervention history")
	ev.Add("recovery_actions.prior_recoveries", f.PriorRecoveries)
	ev.Add("recovery_actions.prior_failed_actions", f.PriorFailedActions)
	ev.Add("recovery_actions.reminder_count", f.ReminderCount)

	// The deterministic assessment is supplied as evidence so the model
	// critiques a computed baseline rather than inventing a score from scratch.
	ev.Section("Deterministic risk assessment (computed, authoritative)")
	ev.Add("computed.risk_score", fmt.Sprintf("%.3f", base.RiskScore))
	ev.Add("computed.failure_severity", fmt.Sprintf("%.2f", base.Components.FailureSeverity))
	ev.Add("computed.customer_intent", fmt.Sprintf("%.2f", base.Components.CustomerIntent))
	ev.Add("computed.payment_reliability", fmt.Sprintf("%.2f", base.Components.PaymentReliability))
	ev.Add("computed.amount_score", fmt.Sprintf("%.2f", base.Components.AmountScore))
	ev.Add("computed.time_sensitivity", fmt.Sprintf("%.2f", base.Components.TimeSensitivity))
	ev.Add("computed.recovery_window", fmt.Sprintf("%.2f", base.Components.RecoveryWindow))
	ev.Add("computed.is_at_risk", base.IsAtRisk)
	ev.AddList("computed.reason_codes", base.ReasonCodes)

	if in.PolicySummary != "" {
		ev.Section("Merchant policy summary")
		ev.Add("policy.summary", in.PolicySummary)
	}
	return ev
}

func detectionUserPrompt(ev *evidenceBuilder) string {
	return ev.String() + `
TASK
Assess whether this record represents genuine revenue at risk that merits an automated recovery intervention.
Set revenue_at_risk to exactly the paise integer given as transaction.amount.
Return the JSON object only.`
}

// --- shared helpers ---

// fallbackReason maps a model failure onto a short, stable label for counters
// and audit records.
func fallbackReason(err error) string {
	var me *ModelError
	if errors.As(err, &me) {
		return string(me.Kind)
	}
	if errors.Is(err, ErrModelDisabled) {
		return "model_disabled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if errors.Is(err, domain.ErrAgentUnavailable) {
		return "agent_unavailable"
	}
	return "unknown_error"
}

// sanitizeCodes keeps only short snake_case labels. Model-supplied reason codes
// and evidence refs are rendered in the UI, so they are constrained rather than
// trusted: a label is a token, not free text.
func sanitizeCodes(in []string, limit int) []string {
	out := make([]string, 0, limit)
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		s = strings.ReplaceAll(s, " ", "_")
		if !validCode(s) {
			continue
		}
		out = append(out, s)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// validCode allows lowercase letters, digits, underscore and dot (dots appear
// in evidence field paths like customer.success_rate).
func validCode(s string) bool {
	if len(s) < 2 || len(s) > 48 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

func mergeCodes(base, extra []string) []string {
	seen := make(map[string]bool, len(base)+len(extra))
	out := make([]string, 0, len(base)+len(extra))
	for _, s := range append(append([]string{}, base...), extra...) {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func appendCode(base []string, code string) []string {
	for _, s := range base {
		if s == code {
			return base
		}
	}
	return append(base, code)
}

func clamp01(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	default:
		return v
	}
}
