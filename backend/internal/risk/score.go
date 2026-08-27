// Package risk implements the deterministic revenue-risk scoring from SRS 9.
//
// Every value here is a pure function of trusted database facts. No model
// output feeds the risk score, which is what makes case prioritisation
// reproducible across runs (SRS 9.2: "the final score must be deterministic
// once model probabilities and policy feasibility are supplied").
package risk

import (
	"math"
	"sort"
	"strings"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// Weights are the SRS 9.1 coefficients. They sum to 1.0, so a score is always
// in [0,1] given normalized components.
const (
	WeightFailureSeverity    = 0.30
	WeightCustomerIntent     = 0.20
	WeightPaymentReliability = 0.20
	WeightAmountScore        = 0.15
	WeightTimeSensitivity    = 0.10
	WeightRecoveryWindow     = 0.05
)

// AmountScoreCeiling is the amount at which amount_score saturates at 1.0.
// ₹50,000 (in paise) keeps mid-size B2B invoices inside the sensitive range.
const AmountScoreCeiling = domain.Money(5_000_000)

// Features are the normalized inputs to the risk score. Each field is a
// trusted fact drawn from the database or the synthetic dataset, never from a
// model.
type Features struct {
	SourceType domain.SourceType
	Amount     domain.Money

	// ErrorCode / FailureReason drive failure severity.
	ErrorCode     string
	FailureReason string
	AttemptCount  int

	// Customer context.
	Segment             domain.Segment
	CustomerSuccessRate float64
	LifetimeValue       domain.Money
	RecencyDays         int
	TotalPayments       int

	// Behaviour context.
	CheckoutViews       int
	MinutesSinceAbandon int
	DaysOverdue         int
	AgeMinutes          int

	// History context.
	PriorRecoveries    int
	PriorFailedActions int
	ReminderCount      int
}

// Components is the per-term breakdown, retained so the UI can explain the
// score instead of showing a bare number (SRS NFR-004).
type Components struct {
	FailureSeverity    float64 `json:"failure_severity"`
	CustomerIntent     float64 `json:"customer_intent"`
	PaymentReliability float64 `json:"payment_reliability"`
	AmountScore        float64 `json:"amount_score"`
	TimeSensitivity    float64 `json:"time_sensitivity"`
	RecoveryWindow     float64 `json:"recovery_window"`
}

// Assessment is the full deterministic detection result.
type Assessment struct {
	RiskScore     float64        `json:"risk_score"`
	Components    Components     `json:"components"`
	RevenueAtRisk domain.Money   `json:"revenue_at_risk"`
	Urgency       domain.Urgency `json:"urgency"`
	IsAtRisk      bool           `json:"is_at_risk"`
	ReasonCodes   []string       `json:"reason_codes"`
	EvidenceRefs  []string       `json:"evidence_refs"`
}

// errorCodeSeverity maps a gateway/issuer failure code to how strongly it
// suggests the revenue behind it is still recoverable. High means "worth acting
// on": a gateway timeout usually succeeds on a second attempt, an expired card
// never will.
//
// Every key is lower_snake_case and every lookup goes through normalizeCode, so
// the uppercase codes Razorpay actually sends (GATEWAY_ERROR) and the lowercase
// ones used in fixtures and manual entry resolve to the same severity. Mixing
// cases in this map is how the same failure ends up with two different risk
// scores depending on which component wrote the row.
var errorCodeSeverity = map[string]float64{
	// Transient: the instrument is fine, the attempt was unlucky.
	"gateway_error":   0.95,
	"issuer_down":     0.92,
	"payment_timeout": 0.90,
	"network_error":   0.88,
	"server_error":    0.88,
	"payment_failed":  0.70,

	// Recoverable, but not by re-presenting the same charge.
	"insufficient_funds": 0.60,
	// Razorpay reports a failed 3-D Secure step as AUTHENTICATION_ERROR; the
	// second spelling is the internal one. Both mean the instrument works and
	// the authentication needs redoing, so both carry the same severity.
	"authentication_error":                  0.55,
	"authentication_failed":                 0.55,
	"card_declined":                         0.45,
	"bad_request_error":                     0.35,
	"payment_cancelled":                     0.30,
	"international_transaction_not_allowed": 0.25,

	// Terminal: the instrument itself is dead. Listed with a severity anyway so
	// the score shown on a skipped case reflects why it was skipped.
	"card_expired":    0.20,
	"invalid_card":    0.15,
	"card_stolen":     0.05,
	"card_lost":       0.05,
	"fraud_suspected": 0.05,
}

// TerminalErrorCodes are the failure codes that mean no intervention can ever
// collect this payment, so no case is opened for them at all (FR-012).
//
// It is exported and named rather than inlined into the eligibility check
// because FR-012 requires the eligibility rules to be visible to an operator,
// and "visible" starts with the rule existing somewhere a person can read it.
//
// A stolen or lost card is on this list for the same reason an invalid one is:
// the issuer has permanently withdrawn the instrument. Re-presenting a charge
// against it cannot succeed, and repeatedly trying costs the merchant gateway
// reputation on top of the customer's patience. Suspected fraud is here because
// it is explicitly outside this system's mandate (SRS 5.2) — it is a human
// decision, and an automated collection attempt could make it worse.
var TerminalErrorCodes = map[string]bool{
	"fraud_suspected": true,
	"invalid_card":    true,
	"card_stolen":     true,
	"card_lost":       true,
}

// UnknownCodeSeverity is the severity assigned to a failure code that is not in
// errorCodeSeverity. It is mid-range and never zero: an unrecognised code means
// the system does not know whether the money is recoverable, and treating "I
// don't know" as "not worth chasing" would silently drop revenue.
const UnknownCodeSeverity = 0.55

// ErrorCodeSeverity reports the recoverable severity of a gateway failure code
// and whether the code was recognised at all.
//
// Both the lookup and the terminal check below are exported and route through
// normalizeCode, so no caller has to remember to canonicalise first. That is the
// point: the two were previously separate switch statements against literal
// lowercase keys, and the one that forgot to normalise its input silently
// stopped matching the day Razorpay's uppercase codes arrived.
func ErrorCodeSeverity(code string) (float64, bool) {
	sev, ok := errorCodeSeverity[normalizeCode(code)]
	return sev, ok
}

// IsTerminalErrorCode reports whether a failure code means this payment can
// never be collected, so no case should be opened for it (FR-012).
func IsTerminalErrorCode(code string) bool {
	return TerminalErrorCodes[normalizeCode(code)]
}

// Score computes the risk score and its components (SRS 9.1).
func Score(f Features) Assessment {
	c := Components{
		FailureSeverity:    failureSeverity(f),
		CustomerIntent:     customerIntent(f),
		PaymentReliability: paymentReliability(f),
		AmountScore:        amountScore(f.Amount),
		TimeSensitivity:    timeSensitivity(f),
		RecoveryWindow:     recoveryWindow(f),
	}

	score := WeightFailureSeverity*c.FailureSeverity +
		WeightCustomerIntent*c.CustomerIntent +
		WeightPaymentReliability*c.PaymentReliability +
		WeightAmountScore*c.AmountScore +
		WeightTimeSensitivity*c.TimeSensitivity +
		WeightRecoveryWindow*c.RecoveryWindow

	score = clamp01(score)

	a := Assessment{
		RiskScore:     round3(score),
		Components:    c,
		RevenueAtRisk: f.Amount,
		Urgency:       urgencyFor(score, f),
		ReasonCodes:   reasonCodes(f, c),
		EvidenceRefs:  evidenceRefs(f),
	}
	a.IsAtRisk = IsAtRisk(f, score)
	if !a.IsAtRisk {
		// A case that is not at risk carries no revenue at risk, so dashboard
		// totals cannot be inflated by unrecoverable events.
		a.RevenueAtRisk = 0
	}
	return a
}

// IsAtRisk applies the eligibility rules that gate case creation
// (SRS FR-012). These are deliberately explicit rather than a single
// threshold so an operator can read why a record was skipped.
func IsAtRisk(f Features, score float64) bool {
	// Terminal-unrecoverable signals: never create a case.
	if IsTerminalErrorCode(f.ErrorCode) {
		return false
	}
	// Zero or negative amounts carry no recoverable revenue.
	if f.Amount <= 0 {
		return false
	}
	// Exhausted history: repeated failed interventions with no recovery means
	// further automated contact is unlikely to help (SRS 10.3).
	if f.PriorFailedActions >= 3 && f.PriorRecoveries == 0 {
		return false
	}
	// Very stale checkout abandonment has effectively no recovery window.
	if f.SourceType == domain.SourceCheckoutAbandonment && f.MinutesSinceAbandon > 7*24*60 {
		return false
	}
	return score >= AtRiskThreshold
}

// AtRiskThreshold is the minimum score for a case to be considered actionable.
const AtRiskThreshold = 0.35

// failureSeverity scores how strongly the failure suggests recoverable revenue.
// High severity means "worth acting on", not "bad error".
func failureSeverity(f Features) float64 {
	switch f.SourceType {
	case domain.SourceCheckoutAbandonment:
		// Abandonment has no gateway error; severity comes from how deep the
		// customer got before dropping out.
		base := 0.55
		if f.CheckoutViews >= 3 {
			base += 0.20
		} else if f.CheckoutViews >= 2 {
			base += 0.10
		}
		return clamp01(base)
	case domain.SourceInvoiceOverdue:
		// Overdue receivables get more severe the longer they are unpaid, up
		// to the point where collectability starts falling.
		switch {
		case f.DaysOverdue <= 0:
			return 0.30
		case f.DaysOverdue <= 7:
			return 0.60
		case f.DaysOverdue <= 30:
			return 0.80
		case f.DaysOverdue <= 60:
			return 0.70
		default:
			return 0.50
		}
	case domain.SourceSubscriptionFailure:
		base := 0.70
		if f.AttemptCount > 1 {
			base -= 0.10 * float64(f.AttemptCount-1)
		}
		return clamp01(base)
	default: // payment failure
		sev, ok := ErrorCodeSeverity(f.ErrorCode)
		if !ok {
			sev = UnknownCodeSeverity
		}
		// Each additional attempt reduces the expected value of another try.
		if f.AttemptCount > 1 {
			sev -= 0.12 * float64(f.AttemptCount-1)
		}
		return clamp01(sev)
	}
}

// customerIntent estimates how much the customer wants to complete payment.
func customerIntent(f Features) float64 {
	intent := 0.40
	switch f.SourceType {
	case domain.SourceCheckoutAbandonment:
		intent = 0.30 + 0.12*float64(min(f.CheckoutViews, 4))
	case domain.SourceSubscriptionFailure:
		// An active subscription is itself a standing intent signal.
		intent = 0.70
	case domain.SourceInvoiceOverdue:
		intent = 0.55
	default:
		// A payment attempt is a strong intent signal.
		intent = 0.65
	}
	if f.PriorRecoveries > 0 {
		intent += 0.10
	}
	if f.TotalPayments >= 5 {
		intent += 0.08
	}
	if f.RecencyDays >= 0 && f.RecencyDays <= 7 {
		intent += 0.07
	} else if f.RecencyDays > 180 {
		intent -= 0.15
	}
	return clamp01(intent)
}

// paymentReliability is the customer's historical ability to pay successfully.
func paymentReliability(f Features) float64 {
	// With no history, fall back to a neutral prior rather than assuming the
	// customer is reliable.
	if f.TotalPayments == 0 {
		return 0.45
	}
	r := clamp01(f.CustomerSuccessRate)
	// Shrink toward the neutral prior when the sample is small, so one lucky
	// payment does not read as perfect reliability.
	weight := float64(f.TotalPayments) / (float64(f.TotalPayments) + 3.0)
	return clamp01(0.45*(1-weight) + r*weight)
}

// amountScore scales monetary size onto [0,1] with a log curve so that very
// large invoices do not dominate every other signal.
func amountScore(amount domain.Money) float64 {
	if amount <= 0 {
		return 0
	}
	if amount >= AmountScoreCeiling {
		return 1
	}
	return clamp01(math.Log1p(float64(amount)) / math.Log1p(float64(AmountScoreCeiling)))
}

// timeSensitivity is how urgently the case needs attention.
func timeSensitivity(f Features) float64 {
	switch f.SourceType {
	case domain.SourceCheckoutAbandonment:
		// Intent decays quickly: the first hour is the most valuable.
		switch {
		case f.MinutesSinceAbandon <= 30:
			return 1.00
		case f.MinutesSinceAbandon <= 60:
			return 0.85
		case f.MinutesSinceAbandon <= 240:
			return 0.60
		case f.MinutesSinceAbandon <= 1440:
			return 0.35
		default:
			return 0.15
		}
	case domain.SourceInvoiceOverdue:
		switch {
		case f.DaysOverdue >= 45:
			return 1.00
		case f.DaysOverdue >= 30:
			return 0.85
		case f.DaysOverdue >= 14:
			return 0.65
		case f.DaysOverdue >= 1:
			return 0.45
		default:
			return 0.20
		}
	default:
		switch {
		case f.AgeMinutes <= 15:
			return 1.00
		case f.AgeMinutes <= 60:
			return 0.85
		case f.AgeMinutes <= 360:
			return 0.60
		case f.AgeMinutes <= 1440:
			return 0.40
		default:
			return 0.20
		}
	}
}

// recoveryWindow estimates how much room is left to act before the
// opportunity closes.
func recoveryWindow(f Features) float64 {
	switch f.SourceType {
	case domain.SourceCheckoutAbandonment:
		if f.MinutesSinceAbandon >= 2880 {
			return 0.10
		}
		return clamp01(1.0 - float64(f.MinutesSinceAbandon)/2880.0)
	case domain.SourceInvoiceOverdue:
		if f.DaysOverdue >= 90 {
			return 0.10
		}
		return clamp01(1.0 - float64(f.DaysOverdue)/90.0)
	case domain.SourceSubscriptionFailure:
		// Card-network retry windows are typically a few days.
		return clamp01(1.0 - 0.25*float64(f.AttemptCount))
	default:
		if f.AttemptCount >= 3 {
			return 0.15
		}
		return clamp01(1.0 - 0.30*float64(f.AttemptCount-1))
	}
}

// urgencyFor maps the score plus hard signals onto the urgency enum.
func urgencyFor(score float64, f Features) domain.Urgency {
	// A large amount escalates urgency regardless of score, because the cost
	// of missing it is high.
	bigAmount := f.Amount >= 2_000_000 // ₹20,000
	switch {
	case score >= 0.80 || (score >= 0.65 && bigAmount):
		return domain.UrgencyCritical
	case score >= 0.62:
		return domain.UrgencyHigh
	case score >= 0.45:
		return domain.UrgencyMedium
	default:
		return domain.UrgencyLow
	}
}

// reasonCodes produces machine-readable reasons for the detection verdict
// (SRS 8.1). Codes are sorted for stable output across runs.
func reasonCodes(f Features, c Components) []string {
	codes := map[string]bool{}
	switch f.SourceType {
	case domain.SourcePaymentFailure:
		codes["payment_failure"] = true
	case domain.SourceCheckoutAbandonment:
		codes["checkout_abandonment"] = true
	case domain.SourceInvoiceOverdue:
		codes["overdue_receivable"] = true
	case domain.SourceSubscriptionFailure:
		codes["subscription_failure"] = true
	}
	if c.FailureSeverity >= 0.75 {
		codes["likely_transient"] = true
	}
	if c.CustomerIntent >= 0.70 {
		codes["high_intent"] = true
	}
	if c.PaymentReliability >= 0.75 {
		codes["reliable_payer"] = true
	}
	if c.PaymentReliability <= 0.35 {
		codes["unreliable_payer"] = true
	}
	if c.AmountScore >= 0.80 {
		codes["high_value"] = true
	}
	if f.AttemptCount <= 1 {
		codes["single_failure"] = true
	} else {
		codes["repeat_failure"] = true
	}
	if f.Segment == domain.SegmentRepeat || f.TotalPayments >= 5 {
		codes["repeat_customer"] = true
	}
	if f.Segment == domain.SegmentNew {
		codes["new_customer"] = true
	}
	if c.TimeSensitivity >= 0.85 {
		codes["time_critical"] = true
	}
	if c.RecoveryWindow <= 0.25 {
		codes["closing_window"] = true
	}
	if f.PriorFailedActions > 0 {
		codes["prior_intervention_failed"] = true
	}
	out := make([]string, 0, len(codes))
	for k := range codes {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// evidenceRefs names the input fields the assessment actually relied on, so
// the UI can show grounded evidence rather than prose (SRS FR-021, 8.1).
func evidenceRefs(f Features) []string {
	refs := []string{"transaction.amount"}
	switch f.SourceType {
	case domain.SourcePaymentFailure, domain.SourceSubscriptionFailure:
		refs = append(refs, "transaction.status", "transaction.error_code", "transaction.attempt_count")
	case domain.SourceCheckoutAbandonment:
		refs = append(refs, "checkout_session.last_activity_at", "checkout_session.page_views")
	case domain.SourceInvoiceOverdue:
		refs = append(refs, "invoice.due_date", "invoice.status", "invoice.reminder_count")
	}
	refs = append(refs, "customer.segment", "customer.success_rate", "customer.lifetime_value")
	if f.PriorFailedActions > 0 || f.PriorRecoveries > 0 {
		refs = append(refs, "recovery_actions.history")
	}
	sort.Strings(refs)
	return refs
}

// ExpectedRecoverableRevenue implements ERR from SRS 9.2:
//
//	ERR = revenue_at_risk × recovery_probability × intervention_feasibility
//
// The result is deterministic given its three inputs. Callers must supply
// feasibility from the policy engine, not from a model.
func ExpectedRecoverableRevenue(revenueAtRisk domain.Money, recoveryProbability, interventionFeasibility float64) domain.Money {
	if revenueAtRisk <= 0 {
		return 0
	}
	p := clamp01(recoveryProbability)
	feas := clamp01(interventionFeasibility)
	return domain.Money(math.Round(float64(revenueAtRisk) * p * feas))
}

// PriorityScore orders the intervention queue (SRS FR-013). Expected recovery
// dominates, with urgency and risk as tie-breakers, so the queue is sorted by
// money at stake rather than by model enthusiasm.
func PriorityScore(expectedRecovery domain.Money, urgency domain.Urgency, riskScore float64) float64 {
	return float64(expectedRecovery)/100.0 + float64(urgency.Rank())*250.0 + riskScore*100.0
}

// normalizeCode canonicalises a gateway error code for lookup.
//
// Razorpay sends codes uppercase (GATEWAY_ERROR); fixtures, seed data and
// manually entered rows use lowercase. Both must resolve to the same severity
// and hit the same terminal-code check, because a risk score that depends on the
// letter case of an input string is not a risk score.
func normalizeCode(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func clamp01(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return math.Max(0, math.Min(1, v))
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
