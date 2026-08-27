package agents

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// diagnosisHardFloor is the confidence below which a model label carries no
// information worth acting on, so the cause is recorded as unknown.
//
// This is separate from Policy.MinActionConfidence: that gate decides whether an
// *action* may run autonomously, while this one decides whether we claim to know
// the cause at all (SRS 8.2, 20.4).
const diagnosisHardFloor = 0.40

// DiagnosisInput is the trusted fact set for the Diagnosis Agent (SRS 7 Agent 2).
type DiagnosisInput struct {
	Case     domain.RiskCase
	Customer *domain.Customer

	// Exactly one of these is set, matching Case.SourceType.
	Transaction  *domain.Transaction
	Checkout     *domain.CheckoutSession
	Invoice      *domain.Invoice
	Subscription *domain.Subscription

	// Detection context: what the deterministic scorer already concluded.
	DetectionReasonCodes []string

	// History for this case and customer.
	PriorActions    []domain.RecoveryAction
	PriorRecoveries int

	// MinConfidence is the merchant's autonomous-action threshold, used only to
	// flag a diagnosis as too weak to act on unattended.
	MinConfidence float64

	// PolicySummary is the human-readable control set.
	PolicySummary string

	Now time.Time
}

// DiagnosisResult is the validated diagnosis (SRS 8.2 schema plus provenance).
type DiagnosisResult struct {
	RootCause        domain.RootCause `json:"root_cause"`
	Confidence       float64          `json:"confidence"`
	Evidence         []string         `json:"evidence"`
	UncertaintyFlags []string         `json:"uncertainty_flags"`
	NextStep         string           `json:"next_step"`

	Source    string `json:"source"`
	ModelName string `json:"model_name,omitempty"`
	LatencyMS int64  `json:"latency_ms"`

	// LowConfidence is true when the diagnosis is too weak to justify an
	// unattended action. The orchestrator routes these to ESCALATE (SRS 20.4).
	LowConfidence bool `json:"low_confidence"`

	InjectionSuspected bool   `json:"injection_suspected,omitempty"`
	FallbackReason     string `json:"fallback_reason,omitempty"`
}

// Entity converts the result into the persisted record.
func (r DiagnosisResult) Entity(caseID string) domain.Diagnosis {
	return domain.Diagnosis{
		CaseID:           caseID,
		RootCause:        r.RootCause,
		Confidence:       r.Confidence,
		Evidence:         r.Evidence,
		UncertaintyFlags: r.UncertaintyFlags,
		NextStep:         r.NextStep,
		Source:           r.Source,
		ModelName:        r.ModelName,
		LatencyMS:        r.LatencyMS,
	}
}

// diagnosisModelOutput mirrors the SRS 8.2 schema exactly.
type diagnosisModelOutput struct {
	RootCause        string   `json:"root_cause"`
	Confidence       float64  `json:"confidence"`
	Evidence         []string `json:"evidence"`
	UncertaintyFlags []string `json:"uncertainty_flags"`
	NextStep         string   `json:"next_step"`
}

func diagnosisSchema() schema {
	causes := make([]string, 0, len(domain.AllRootCauses))
	for _, rc := range domain.AllRootCauses {
		causes = append(causes, string(rc))
	}
	return objectSchema(
		[]string{"root_cause", "confidence", "evidence", "uncertainty_flags", "next_step"},
		map[string]any{
			"root_cause":        enumSchema(causes),
			"confidence":        numberSchema(),
			"evidence":          stringArraySchema(),
			"uncertainty_flags": stringArraySchema(),
			"next_step":         stringSchema(),
		})
}

// DiagnosisAgent explains why revenue is at risk.
//
// It is the one agent where the model earns its place outright: the deterministic
// fallback can only map an error code to a label, while the model can reconcile
// conflicting signals. It is also the agent most exposed to injected text, since
// failure reasons and customer names are third-party data — so it may never
// invent a payment fact, and an unrecognised label collapses to unknown.
type DiagnosisAgent struct {
	client Client
}

// NewDiagnosisAgent constructs the agent. A nil or disabled client yields
// deterministic-only behaviour.
func NewDiagnosisAgent(c Client) *DiagnosisAgent { return &DiagnosisAgent{client: c} }

// Diagnose returns a diagnosis, never an error: the deterministic mapping is
// always available, so the pipeline never stalls on model trouble.
func (a *DiagnosisAgent) Diagnose(ctx context.Context, in DiagnosisInput) DiagnosisResult {
	if in.Now.IsZero() {
		in.Now = time.Now().UTC()
	}

	base := deterministicDiagnosis(in)
	if a == nil || a.client == nil || !a.client.Enabled() {
		base.FallbackReason = "model_disabled"
		return finalizeDiagnosis(base, in.MinConfidence)
	}

	ev := buildDiagnosisEvidence(in)
	base.InjectionSuspected = ev.Suspicious()

	started := time.Now()
	raw, err := a.client.Generate(ctx, diagnosisSystemPrompt, diagnosisUserPrompt(ev), diagnosisSchema())
	base.LatencyMS = time.Since(started).Milliseconds()
	if err != nil {
		base.FallbackReason = fallbackReason(err)
		return finalizeDiagnosis(base, in.MinConfidence)
	}

	var model diagnosisModelOutput
	if err := decodeStrict(raw, &model); err != nil {
		base.FallbackReason = "invalid_json"
		return finalizeDiagnosis(base, in.MinConfidence)
	}

	// NormalizeRootCause is the allow-list: anything unrecognised becomes
	// unknown rather than propagating a label the rest of the system cannot
	// reason about (SRS FR-022).
	out := DiagnosisResult{
		RootCause:          domain.NormalizeRootCause(strings.ToLower(strings.TrimSpace(model.RootCause))),
		Confidence:         clamp01(model.Confidence),
		Evidence:           sanitizeEvidenceList(model.Evidence, 6),
		UncertaintyFlags:   sanitizeCodes(model.UncertaintyFlags, 6),
		NextStep:           sanitizeSentence(model.NextStep, 200),
		Source:             "ai",
		ModelName:          a.client.Name(),
		LatencyMS:          base.LatencyMS,
		InjectionSuspected: base.InjectionSuspected,
	}

	if out.RootCause == domain.RootCauseUnknown && domain.RootCause(model.RootCause) != domain.RootCauseUnknown {
		out.UncertaintyFlags = appendCode(out.UncertaintyFlags, "unrecognised_label_coerced")
	}
	// A confident-sounding answer with no evidence is not a diagnosis. Without
	// citations the label cannot be checked, so its confidence is capped.
	if len(out.Evidence) == 0 {
		out.Evidence = base.Evidence
		out.UncertaintyFlags = appendCode(out.UncertaintyFlags, "no_evidence_cited")
		if out.Confidence > 0.50 {
			out.Confidence = 0.50
		}
	}
	if out.Confidence < diagnosisHardFloor {
		out.RootCause = domain.RootCauseUnknown
		out.UncertaintyFlags = appendCode(out.UncertaintyFlags, "below_confidence_floor")
	}
	if out.InjectionSuspected {
		out.UncertaintyFlags = appendCode(out.UncertaintyFlags, "prompt_injection_detected")
	}
	if out.NextStep == "" {
		out.NextStep = base.NextStep
	}
	return finalizeDiagnosis(out, in.MinConfidence)
}

// finalizeDiagnosis applies the confidence gate that the orchestrator reads.
func finalizeDiagnosis(r DiagnosisResult, minConfidence float64) DiagnosisResult {
	if minConfidence <= 0 {
		minConfidence = domain.DefaultPolicy().MinActionConfidence
	}
	if r.Confidence < minConfidence || r.RootCause == domain.RootCauseUnknown {
		r.LowConfidence = true
		r.UncertaintyFlags = appendCode(r.UncertaintyFlags, "low_confidence")
	}
	if r.UncertaintyFlags == nil {
		r.UncertaintyFlags = []string{}
	}
	if r.Evidence == nil {
		r.Evidence = []string{}
	}
	return r
}

// deterministicDiagnosis maps trusted facts onto a root cause without a model.
//
// Confidence values here are deliberately modest. The mapping is real but
// shallow — it reads an error code, it does not weigh conflicting signals — and
// reporting it as high confidence would overstate what the rule knows.
func deterministicDiagnosis(in DiagnosisInput) DiagnosisResult {
	r := DiagnosisResult{
		Source:           "deterministic",
		UncertaintyFlags: []string{"deterministic_fallback"},
		Evidence:         []string{},
	}

	switch in.Case.SourceType {
	case domain.SourceCheckoutAbandonment:
		r.RootCause = domain.RootCauseCheckoutAbandonment
		r.Confidence = 0.72 // the source workflow *is* the cause here
		r.NextStep = "Confirm the cart is still valid before contacting the customer."
		if in.Checkout != nil {
			r.Evidence = append(r.Evidence,
				fmt.Sprintf("checkout_session.page_views=%d", in.Checkout.PageViews),
				fmt.Sprintf("checkout_session.status=%s", in.Checkout.Status))
		}

	case domain.SourceInvoiceOverdue:
		r.RootCause = domain.RootCauseOverdueReceivable
		r.Confidence = 0.70
		r.NextStep = "Verify no offline payment was received against this invoice."
		if in.Invoice != nil {
			r.Evidence = append(r.Evidence,
				fmt.Sprintf("invoice.status=%s", in.Invoice.Status),
				fmt.Sprintf("invoice.due_date=%s", in.Invoice.DueDate.Format("2006-01-02")),
				fmt.Sprintf("invoice.reminder_count=%d", in.Invoice.ReminderCount))
		}

	case domain.SourceSubscriptionFailure:
		r.RootCause = domain.RootCauseSubscriptionFailure
		r.Confidence = 0.65
		r.NextStep = "Check whether the saved payment instrument is still valid."
		if in.Subscription != nil {
			r.Evidence = append(r.Evidence,
				fmt.Sprintf("subscription.status=%s", in.Subscription.Status),
				fmt.Sprintf("subscription.failed_charge_count=%d", in.Subscription.FailedChargeCount))
		}

	default: // payment failure
		code := ""
		if in.Transaction != nil {
			code = strings.ToLower(strings.TrimSpace(in.Transaction.ErrorCode))
			r.Evidence = append(r.Evidence,
				fmt.Sprintf("transaction.status=%s", in.Transaction.Status),
				fmt.Sprintf("transaction.attempt_count=%d", in.Transaction.AttemptCount))
			if in.Transaction.ErrorCode != "" {
				r.Evidence = append(r.Evidence, "transaction.error_code="+in.Transaction.ErrorCode)
			}
		}
		cause, conf := causeFromErrorCode(code)
		r.RootCause = cause
		r.Confidence = conf
		r.NextStep = "Confirm the failure code with the gateway before retrying."
		if code == "" {
			r.UncertaintyFlags = appendCode(r.UncertaintyFlags, "no_error_code")
			r.NextStep = "Fetch the gateway error code for this payment."
		}
	}

	if in.Transaction == nil && in.Checkout == nil && in.Invoice == nil && in.Subscription == nil {
		r.RootCause = domain.RootCauseUnknown
		r.Confidence = 0.25
		r.UncertaintyFlags = appendCode(r.UncertaintyFlags, "source_record_missing")
		r.NextStep = "Reload the underlying payment record for this case."
	}
	if len(in.PriorActions) > 0 && in.PriorRecoveries == 0 {
		r.UncertaintyFlags = appendCode(r.UncertaintyFlags, "prior_intervention_failed")
	}
	return r
}

// causeFromErrorCode maps a gateway error code onto a root cause. Codes not on
// this list return unknown: guessing a cause from an unfamiliar code would put
// an unfounded label in front of an operator.
func causeFromErrorCode(code string) (domain.RootCause, float64) {
	switch code {
	case "gateway_error", "payment_timeout", "issuer_down", "network_error", "server_error":
		return domain.RootCauseTransientFailure, 0.75
	case "insufficient_funds", "payment_failed_insufficient_balance":
		return domain.RootCauseInsufficientFunds, 0.78
	case "authentication_failed", "otp_failed", "3ds_failed", "invalid_otp":
		return domain.RootCauseAuthenticationFailed, 0.76
	case "card_declined", "invalid_card", "card_expired",
		"international_transaction_not_allowed", "payment_cancelled":
		// Declines are real but the *reason* for the decline is issuer-side, so
		// the honest label is unknown rather than a guess at the customer's
		// bank balance.
		return domain.RootCauseUnknown, 0.45
	case "payment_failed", "bad_request_error":
		return domain.RootCauseTransientFailure, 0.55
	case "":
		return domain.RootCauseUnknown, 0.30
	default:
		return domain.RootCauseUnknown, 0.35
	}
}

func buildDiagnosisEvidence(in DiagnosisInput) *evidenceBuilder {
	ev := newEvidence()
	c := in.Case

	ev.Add("case.reference", c.Reference)
	ev.Add("case.source_type", string(c.SourceType))
	ev.Add("case.status", string(c.Status))
	ev.AddMoney("case.revenue_at_risk", int64(c.RevenueAtRisk))
	ev.Add("case.risk_score", fmt.Sprintf("%.3f", c.RiskScore))
	ev.Add("case.urgency", string(c.Urgency))
	ev.AddList("case.detection_reason_codes", in.DetectionReasonCodes)
	ev.Add("case.age_minutes", int(in.Now.Sub(c.CreatedAt).Minutes()))

	if cu := in.Customer; cu != nil {
		ev.Section("Customer")
		ev.AddText("customer.name", cu.Name)
		ev.Add("customer.segment", string(cu.Segment))
		ev.Add("customer.success_rate", fmt.Sprintf("%.2f", cu.SuccessRate))
		ev.Add("customer.total_payments", cu.TotalPayments)
		ev.AddMoney("customer.lifetime_value", int64(cu.LifetimeValue))
	}

	switch {
	case in.Transaction != nil:
		t := in.Transaction
		ev.Section("Payment")
		ev.AddMoney("transaction.amount", int64(t.Amount))
		ev.Add("transaction.currency", t.Currency)
		ev.Add("transaction.status", t.Status)
		ev.Add("transaction.method", t.Method)
		ev.Add("transaction.error_code", t.ErrorCode)
		ev.AddText("transaction.failure_reason", t.FailureReason)
		ev.Add("transaction.attempt_count", t.AttemptCount)
		ev.Add("transaction.created_at", t.CreatedAt.UTC().Format(time.RFC3339))
	case in.Checkout != nil:
		cs := in.Checkout
		ev.Section("Checkout session")
		ev.AddMoney("checkout_session.cart_amount", int64(cs.CartAmount))
		ev.Add("checkout_session.item_count", cs.ItemCount)
		ev.Add("checkout_session.page_views", cs.PageViews)
		ev.Add("checkout_session.status", cs.Status)
		ev.Add("checkout_session.minutes_since_last_activity", int(in.Now.Sub(cs.LastActivityAt).Minutes()))
	case in.Invoice != nil:
		iv := in.Invoice
		ev.Section("Invoice")
		ev.Add("invoice.number", iv.InvoiceNumber)
		ev.AddMoney("invoice.amount", int64(iv.Amount))
		ev.AddMoney("invoice.amount_paid", int64(iv.AmountPaid))
		ev.Add("invoice.status", iv.Status)
		ev.Add("invoice.due_date", iv.DueDate.UTC().Format("2006-01-02"))
		ev.Add("invoice.days_overdue", int(in.Now.Sub(iv.DueDate).Hours()/24))
		ev.Add("invoice.reminder_count", iv.ReminderCount)
	case in.Subscription != nil:
		sb := in.Subscription
		ev.Section("Subscription")
		ev.Add("subscription.plan_id", sb.PlanID)
		ev.AddMoney("subscription.amount", int64(sb.Amount))
		ev.Add("subscription.status", sb.Status)
		ev.Add("subscription.failed_charge_count", sb.FailedChargeCount)
		ev.Add("subscription.current_end", sb.CurrentEnd.UTC().Format(time.RFC3339))
	}

	ev.Section("Intervention history")
	if len(in.PriorActions) == 0 {
		ev.Add("recovery_actions.count", 0)
	} else {
		for i, a := range in.PriorActions {
			if i >= 5 {
				break
			}
			line := fmt.Sprintf("%s status=%s", a.ActionType, a.Status)
			if a.ErrorCode != "" {
				line += " error=" + a.ErrorCode
			}
			ev.Add(fmt.Sprintf("recovery_actions.%d", i), line)
		}
		ev.Add("recovery_actions.count", len(in.PriorActions))
	}
	ev.Add("recovery_actions.prior_recoveries", in.PriorRecoveries)

	if in.PolicySummary != "" {
		ev.Section("Merchant policy summary")
		ev.Add("policy.summary", in.PolicySummary)
	}
	return ev
}

func diagnosisUserPrompt(ev *evidenceBuilder) string {
	return ev.String() + `
TASK
Determine the single most likely root cause of this revenue-at-risk case, with an honest confidence and the evidence you relied on.
If the evidence does not clearly support one cause, answer "unknown".
Return the JSON object only.`
}

// sanitizeEvidenceList cleans model-supplied evidence strings. These are quoted
// back to the operator, so they are treated as untrusted display text: the
// model could echo injected content from the evidence block.
func sanitizeEvidenceList(in []string, limit int) []string {
	out := make([]string, 0, limit)
	seen := map[string]bool{}
	for _, s := range in {
		clean, _ := sanitizeFreeText(s)
		clean = truncateText(clean, 160)
		if clean == "" || seen[clean] {
			continue
		}
		seen[clean] = true
		out = append(out, clean)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// sanitizeSentence cleans a single free-text field from the model.
func sanitizeSentence(s string, max int) string {
	clean, _ := sanitizeFreeText(s)
	return truncateText(clean, max)
}
