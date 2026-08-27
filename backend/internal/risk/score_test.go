package risk

import (
	"math"
	"testing"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// TestWeightsSumToOne is the precondition for every other claim in this file.
// The SRS 9.1 formula only produces a score in [0,1] if its coefficients sum to
// 1.0, so a change to any single weight has to be paid for by another.
func TestWeightsSumToOne(t *testing.T) {
	sum := WeightFailureSeverity + WeightCustomerIntent + WeightPaymentReliability +
		WeightAmountScore + WeightTimeSensitivity + WeightRecoveryWindow
	if math.Abs(sum-1.0) > 1e-9 {
		t.Fatalf("weights sum to %v, want 1.0 (SRS 9.1)", sum)
	}
}

// TestScoreEqualsWeightedComponents checks that the breakdown shown to an
// operator actually reconstructs the score they are being shown (NFR-004).
//
// A UI that displays six component bars next to a total which is computed some
// other way is worse than showing no explanation at all, because it looks like
// an explanation. This asserts the two cannot drift apart.
func TestScoreEqualsWeightedComponents(t *testing.T) {
	for _, f := range representativeFeatures() {
		a := Score(f)
		c := a.Components
		want := WeightFailureSeverity*c.FailureSeverity +
			WeightCustomerIntent*c.CustomerIntent +
			WeightPaymentReliability*c.PaymentReliability +
			WeightAmountScore*c.AmountScore +
			WeightTimeSensitivity*c.TimeSensitivity +
			WeightRecoveryWindow*c.RecoveryWindow

		if math.Abs(a.RiskScore-round3(want)) > 1e-9 {
			t.Errorf("%s/%s: score %v but components weight to %v",
				f.SourceType, f.ErrorCode, a.RiskScore, round3(want))
		}
		if a.RiskScore < 0 || a.RiskScore > 1 {
			t.Errorf("%s/%s: score %v outside [0,1]", f.SourceType, f.ErrorCode, a.RiskScore)
		}
		for name, v := range map[string]float64{
			"failure_severity": c.FailureSeverity, "customer_intent": c.CustomerIntent,
			"payment_reliability": c.PaymentReliability, "amount_score": c.AmountScore,
			"time_sensitivity": c.TimeSensitivity, "recovery_window": c.RecoveryWindow,
		} {
			if v < 0 || v > 1 {
				t.Errorf("%s/%s: component %s = %v outside [0,1]", f.SourceType, f.ErrorCode, name, v)
			}
		}
	}
}

// TestScoreIsDeterministic is the SRS 9.2 requirement that the score be
// reproducible: same trusted facts, same number, every time. Map iteration order
// and time.Now() are the two usual ways this breaks.
func TestScoreIsDeterministic(t *testing.T) {
	for _, f := range representativeFeatures() {
		first := Score(f)
		for i := 0; i < 20; i++ {
			got := Score(f)
			if got.RiskScore != first.RiskScore {
				t.Fatalf("%s: score changed between calls: %v then %v", f.SourceType, first.RiskScore, got.RiskScore)
			}
			if len(got.ReasonCodes) != len(first.ReasonCodes) {
				t.Fatalf("%s: reason code count changed between calls", f.SourceType)
			}
			for j := range got.ReasonCodes {
				if got.ReasonCodes[j] != first.ReasonCodes[j] {
					t.Fatalf("%s: reason codes not in stable order: %v then %v",
						f.SourceType, first.ReasonCodes, got.ReasonCodes)
				}
			}
		}
	}
}

// TestErrorCodeCaseDoesNotChangeTheScore is a regression test.
//
// Razorpay sends failure codes uppercase (GATEWAY_ERROR); fixtures, seed rows and
// anything typed by hand use lowercase. Before this test existed the severity
// lookup compared the raw string against a lowercase key set, so the same failure
// scored 0.95 or fell through to the 0.55 default depending on which component
// had written the row — and the FR-012 terminal-code stop, which is also keyed on
// the code, could never match a real gateway payload at all.
func TestErrorCodeCaseDoesNotChangeTheScore(t *testing.T) {
	base := paymentFailure()
	for _, variants := range [][]string{
		{"gateway_error", "GATEWAY_ERROR", "Gateway_Error", "  gateway_error  "},
		{"card_declined", "CARD_DECLINED"},
		{"authentication_error", "AUTHENTICATION_ERROR"},
		{"bad_request_error", "BAD_REQUEST_ERROR"},
	} {
		f := base
		f.ErrorCode = variants[0]
		want := Score(f)
		for _, v := range variants[1:] {
			f.ErrorCode = v
			got := Score(f)
			if got.RiskScore != want.RiskScore {
				t.Errorf("%q scored %v but %q scored %v: the score depends on letter case",
					variants[0], want.RiskScore, v, got.RiskScore)
			}
			if got.IsAtRisk != want.IsAtRisk {
				t.Errorf("%q is_at_risk=%v but %q is_at_risk=%v",
					variants[0], want.IsAtRisk, v, got.IsAtRisk)
			}
		}
	}
}

// TestTerminalCodesNeverProduceACase is the FR-012 hard stop. These instruments
// are permanently withdrawn by the issuer or explicitly outside this system's
// mandate, so no case is opened however attractive the rest of the case looks.
func TestTerminalCodesNeverProduceACase(t *testing.T) {
	for code := range TerminalErrorCodes {
		for _, spelling := range []string{code, upper(code), " " + upper(code) + " "} {
			// Deliberately the most attractive case the scorer can be handed: a
			// large amount, a reliable repeat customer, fresh, first attempt. If
			// anything could talk the detector into acting, this would.
			f := Features{
				SourceType:          domain.SourcePaymentFailure,
				Amount:              domain.Money(4_000_000),
				ErrorCode:           spelling,
				AttemptCount:        1,
				Segment:             domain.SegmentHighValue,
				CustomerSuccessRate: 0.98,
				LifetimeValue:       domain.Money(50_000_000),
				RecencyDays:         1,
				TotalPayments:       40,
				AgeMinutes:          5,
			}
			a := Score(f)
			if a.IsAtRisk {
				t.Errorf("%q produced an at-risk case (score %v) — FR-012 terminal stop did not fire",
					spelling, a.RiskScore)
			}
			if a.RevenueAtRisk != 0 {
				t.Errorf("%q reported %d paise at risk on a case that will never be collected",
					spelling, a.RevenueAtRisk)
			}
		}
	}
}

// TestRazorpayErrorCodesAreRecognised pins the codes the system claims to
// understand. A code that falls through to UnknownCodeSeverity is not a bug on
// its own — the default exists for exactly that — but a code Razorpay sends
// routinely, and that the synthetic dataset therefore emits, should be scored
// deliberately rather than by accident.
func TestRazorpayErrorCodesAreRecognised(t *testing.T) {
	// Emitted by internal/simulation (dataset.go and edge.go) and by the
	// Razorpay test-mode integration.
	codes := []string{
		"GATEWAY_ERROR", "NETWORK_ERROR", "SERVER_ERROR", "BAD_REQUEST_ERROR",
		"AUTHENTICATION_ERROR", "CARD_DECLINED", "PAYMENT_TIMEOUT", "CARD_STOLEN",
	}
	for _, code := range codes {
		if _, ok := ErrorCodeSeverity(code); !ok {
			t.Errorf("%s is not in the severity table, so it silently scores the %v default",
				code, UnknownCodeSeverity)
		}
	}
}

// TestUnknownCodeIsNotTreatedAsSafe guards the direction of the default. An
// unrecognised failure means the system does not know whether the revenue is
// recoverable, and the safe reading of "don't know" is to look at it, not to
// discard it.
func TestUnknownCodeIsNotTreatedAsSafe(t *testing.T) {
	if UnknownCodeSeverity <= 0 {
		t.Fatalf("UnknownCodeSeverity = %v: an unrecognised code would score zero severity", UnknownCodeSeverity)
	}
	f := paymentFailure()
	f.ErrorCode = "SOME_CODE_RAZORPAY_ADDED_LAST_TUESDAY"
	if got := failureSeverity(f); got != UnknownCodeSeverity {
		t.Errorf("unknown code severity = %v, want %v", got, UnknownCodeSeverity)
	}
	if IsTerminalErrorCode(f.ErrorCode) {
		t.Error("an unrecognised code was treated as terminal-unrecoverable")
	}
}

// TestAtRiskThresholdIsTheOnlySoftGate checks that every non-threshold
// eligibility rule in FR-012 is a hard stop, independent of the score.
func TestAtRiskThresholdIsTheOnlySoftGate(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*Features)
	}{
		{"zero amount", func(f *Features) { f.Amount = 0 }},
		{"negative amount", func(f *Features) { f.Amount = -100 }},
		{"three failed interventions and no recovery", func(f *Features) {
			f.PriorFailedActions = 3
			f.PriorRecoveries = 0
		}},
	}
	for _, tc := range tests {
		f := paymentFailure()
		f.Amount = domain.Money(3_000_000)
		tc.mut(&f)
		if IsAtRisk(f, 0.99) {
			t.Errorf("%s: still at risk at score 0.99 — the rule is not a hard stop", tc.name)
		}
	}

	// Stale abandonment is source-specific: a week-old cart is dead, but a
	// week-old failed payment is not.
	stale := Features{
		SourceType:          domain.SourceCheckoutAbandonment,
		Amount:              domain.Money(500_000),
		MinutesSinceAbandon: 7*24*60 + 1,
	}
	if IsAtRisk(stale, 0.99) {
		t.Error("an abandonment older than seven days was still eligible")
	}
	stale.MinutesSinceAbandon = 7 * 24 * 60
	if !IsAtRisk(stale, 0.99) {
		t.Error("an abandonment exactly at the seven-day boundary was excluded; the rule is > not >=")
	}

	// A prior failed action is only disqualifying when nothing has ever worked.
	recovered := paymentFailure()
	recovered.PriorFailedActions = 5
	recovered.PriorRecoveries = 1
	if !IsAtRisk(recovered, 0.99) {
		t.Error("a customer who has recovered before was excluded for having failed attempts")
	}
}

// TestAtRiskThresholdBoundary pins the documented threshold at its edges, since
// AtRiskThreshold is the single number that decides how much of the merchant's
// queue exists.
func TestAtRiskThresholdBoundary(t *testing.T) {
	f := paymentFailure()
	if !IsAtRisk(f, AtRiskThreshold) {
		t.Errorf("score exactly at AtRiskThreshold (%v) was not at risk; the comparison must be >=", AtRiskThreshold)
	}
	if IsAtRisk(f, AtRiskThreshold-0.001) {
		t.Errorf("score just below AtRiskThreshold (%v) was at risk", AtRiskThreshold)
	}
}

// TestAmountScoreSaturates checks the log curve's two ends. Without a ceiling a
// single ₹5 lakh invoice would dominate the amount term for every other case in
// the queue.
func TestAmountScoreSaturates(t *testing.T) {
	if got := amountScore(0); got != 0 {
		t.Errorf("amountScore(0) = %v, want 0", got)
	}
	if got := amountScore(-1); got != 0 {
		t.Errorf("amountScore(-1) = %v, want 0", got)
	}
	if got := amountScore(AmountScoreCeiling); got != 1 {
		t.Errorf("amountScore(ceiling) = %v, want 1", got)
	}
	if got := amountScore(AmountScoreCeiling * 100); got != 1 {
		t.Errorf("amountScore(100x ceiling) = %v, want 1 (saturated)", got)
	}

	// Monotonic below the ceiling, and compressed: doubling the amount must not
	// double the score.
	small, mid, large := amountScore(100_000), amountScore(1_000_000), amountScore(4_000_000)
	if !(small < mid && mid < large) {
		t.Errorf("amount score not monotonic: %v, %v, %v", small, mid, large)
	}
	if mid/small >= 10 {
		t.Errorf("amount score is not compressed: 10x the amount gave %vx the score", mid/small)
	}
}

// TestPaymentReliabilityShrinksSmallSamples is the difference between a customer
// who has paid once and one who has paid forty times, both at a 100% rate. The
// first is not yet evidence.
func TestPaymentReliabilityShrinksSmallSamples(t *testing.T) {
	none := paymentReliability(Features{TotalPayments: 0, CustomerSuccessRate: 1})
	if none != 0.45 {
		t.Errorf("no history = %v, want the 0.45 neutral prior", none)
	}

	one := paymentReliability(Features{TotalPayments: 1, CustomerSuccessRate: 1})
	many := paymentReliability(Features{TotalPayments: 40, CustomerSuccessRate: 1})
	if !(one < many) {
		t.Errorf("one perfect payment (%v) scored no lower than forty (%v)", one, many)
	}
	if one <= none {
		t.Errorf("one perfect payment (%v) did not move above the prior (%v)", one, none)
	}
	if many <= 0.9 {
		t.Errorf("forty perfect payments only reached %v", many)
	}
}

// TestExpectedRecoverableRevenue covers the SRS 9.2 ERR formula, which is the
// number the queue is ordered by and the number a merchant is quoted.
func TestExpectedRecoverableRevenue(t *testing.T) {
	tests := []struct {
		name           string
		atRisk         domain.Money
		prob, feasible float64
		want           domain.Money
	}{
		{"straightforward", 100_000, 0.5, 1.0, 50_000},
		{"both factors apply", 100_000, 0.5, 0.5, 25_000},
		{"certain and feasible returns the full amount", 100_000, 1, 1, 100_000},
		{"zero probability recovers nothing", 100_000, 0, 1, 0},
		{"infeasible recovers nothing", 100_000, 1, 0, 0},
		{"no revenue at risk", 0, 1, 1, 0},
		{"negative revenue at risk", -5_000, 1, 1, 0},
		{"probability above one is clamped", 100_000, 1.7, 1, 100_000},
		{"negative probability is clamped", 100_000, -0.4, 1, 0},
		{"feasibility above one is clamped", 100_000, 1, 2.5, 100_000},
		{"rounds to whole paise", 3, 0.5, 1, 2},
	}
	for _, tc := range tests {
		if got := ExpectedRecoverableRevenue(tc.atRisk, tc.prob, tc.feasible); got != tc.want {
			t.Errorf("%s: ERR(%d, %v, %v) = %d, want %d", tc.name, tc.atRisk, tc.prob, tc.feasible, got, tc.want)
		}
	}
}

// TestExpectedRecoveryNeverExceedsRevenueAtRisk is the invariant behind the
// policy engine's amount-integrity rule: the system must never claim it can
// recover more than is owed. Both multipliers are probabilities, so this holds
// for every input, including the NaN a divide-by-zero upstream could produce.
func TestExpectedRecoveryNeverExceedsRevenueAtRisk(t *testing.T) {
	amounts := []domain.Money{1, 999, 100_000, 25_000_000}
	factors := []float64{0, 0.01, 0.5, 0.999, 1, 1.5, math.NaN(), math.Inf(1), math.Inf(-1)}
	for _, amt := range amounts {
		for _, p := range factors {
			for _, feas := range factors {
				got := ExpectedRecoverableRevenue(amt, p, feas)
				if got < 0 {
					t.Errorf("ERR(%d, %v, %v) = %d: negative expected recovery", amt, p, feas, got)
				}
				if got > amt {
					t.Errorf("ERR(%d, %v, %v) = %d: more than the amount at risk", amt, p, feas, got)
				}
			}
		}
	}
}

// TestPriorityScoreOrdersByMoneyThenUrgency checks the FR-013 queue ordering:
// expected recovery dominates, with urgency and risk as tie-breakers. The point
// is that the queue is sorted by money at stake rather than by how confident a
// model sounded.
func TestPriorityScoreOrdersByMoneyThenUrgency(t *testing.T) {
	// Same urgency and risk: more money first.
	low := PriorityScore(10_000, domain.UrgencyMedium, 0.5)
	high := PriorityScore(90_000, domain.UrgencyMedium, 0.5)
	if high <= low {
		t.Errorf("₹900 case (%v) did not outrank ₹100 case (%v)", high, low)
	}

	// Same money: higher urgency first, and urgency outweighs the risk term so a
	// critical case cannot be buried by a high-scoring low-urgency one.
	critical := PriorityScore(10_000, domain.UrgencyCritical, 0.10)
	relaxed := PriorityScore(10_000, domain.UrgencyLow, 0.99)
	if critical <= relaxed {
		t.Errorf("critical case (%v) ranked below a low-urgency one (%v)", critical, relaxed)
	}

	// Everything else equal: higher risk first.
	riskier := PriorityScore(10_000, domain.UrgencyMedium, 0.9)
	safer := PriorityScore(10_000, domain.UrgencyMedium, 0.2)
	if riskier <= safer {
		t.Errorf("higher-risk case (%v) did not outrank lower (%v)", riskier, safer)
	}
}

// TestUrgencyEscalatesOnAmount records the deliberate exception in urgencyFor: a
// large amount raises urgency at a lower score than a small one, because the cost
// of missing it is higher.
func TestUrgencyEscalatesOnAmount(t *testing.T) {
	small := Features{SourceType: domain.SourcePaymentFailure, Amount: 50_000}
	big := Features{SourceType: domain.SourcePaymentFailure, Amount: domain.Money(2_000_000)}

	if got := urgencyFor(0.70, small); got != domain.UrgencyHigh {
		t.Errorf("small amount at 0.70 = %s, want high", got)
	}
	if got := urgencyFor(0.70, big); got != domain.UrgencyCritical {
		t.Errorf("₹20,000 at 0.70 = %s, want critical", got)
	}
	if got := urgencyFor(0.30, big); got != domain.UrgencyLow {
		t.Errorf("a large amount at 0.30 = %s, want low: amount must not override a weak score entirely", got)
	}
}

// TestReasonCodesAreGrounded checks that the detection verdict comes with
// machine-readable reasons naming the source type and the signals that fired
// (SRS 8.1, FR-021). Prose is not evidence; these codes are what the UI shows.
func TestReasonCodesAreGrounded(t *testing.T) {
	for _, f := range representativeFeatures() {
		a := Score(f)
		if len(a.ReasonCodes) == 0 {
			t.Errorf("%s: no reason codes", f.SourceType)
		}
		if len(a.EvidenceRefs) == 0 {
			t.Errorf("%s: no evidence refs", f.SourceType)
		}
		want := map[domain.SourceType]string{
			domain.SourcePaymentFailure:      "payment_failure",
			domain.SourceCheckoutAbandonment: "checkout_abandonment",
			domain.SourceInvoiceOverdue:      "overdue_receivable",
			domain.SourceSubscriptionFailure: "subscription_failure",
		}[f.SourceType]
		if !contains(a.ReasonCodes, want) {
			t.Errorf("%s: reason codes %v do not name the source type", f.SourceType, a.ReasonCodes)
		}
		for _, ref := range a.EvidenceRefs {
			if ref == "" {
				t.Errorf("%s: empty evidence ref", f.SourceType)
			}
		}
	}
}

// TestClassifySegments covers the SRS 9.3 precedence. The order of the checks is
// the definition — a subscription customer with a large invoice is handled by the
// recurring workflow, not the B2B one — so each case here pins one boundary.
func TestClassifySegments(t *testing.T) {
	tests := []struct {
		name string
		in   SegmentInput
		want domain.Segment
	}{
		{"subscription source", SegmentInput{SourceType: domain.SourceSubscriptionFailure}, domain.SegmentSubscription},
		{"has a subscription record", SegmentInput{HasSubscription: true}, domain.SegmentSubscription},
		{
			"subscription beats high value",
			SegmentInput{HasSubscription: true, LifetimeValue: HighValueLifetimeThreshold * 10},
			domain.SegmentSubscription,
		},
		{"invoice source", SegmentInput{SourceType: domain.SourceInvoiceOverdue}, domain.SegmentB2B},
		{"custom email domain", SegmentInput{Email: "ap@acme-industries.in"}, domain.SegmentB2B},
		{
			"b2b beats high value",
			SegmentInput{Email: "ap@acme.in", LifetimeValue: HighValueLifetimeThreshold * 10},
			domain.SegmentB2B,
		},
		{"free email is not b2b", SegmentInput{Email: "someone@gmail.com", TotalPayments: 5}, domain.SegmentRepeat},
		{"free email uppercase is not b2b", SegmentInput{Email: "Someone@GMAIL.COM"}, domain.SegmentNew},
		{"lifetime value at the threshold", SegmentInput{LifetimeValue: HighValueLifetimeThreshold}, domain.SegmentHighValue},
		{"lifetime value below the threshold", SegmentInput{LifetimeValue: HighValueLifetimeThreshold - 1}, domain.SegmentNew},
		{"repeat at the threshold", SegmentInput{TotalPayments: RepeatPaymentThreshold}, domain.SegmentRepeat},
		{"one payment is still new", SegmentInput{TotalPayments: 1}, domain.SegmentNew},
		{"nothing known", SegmentInput{}, domain.SegmentNew},
		{"malformed email", SegmentInput{Email: "not-an-email"}, domain.SegmentNew},
		{"email with no domain", SegmentInput{Email: "trailing@"}, domain.SegmentNew},
		{"email with no local part", SegmentInput{Email: "@example.com"}, domain.SegmentNew},
		{"domain without a dot", SegmentInput{Email: "root@localhost"}, domain.SegmentNew},
	}
	for _, tc := range tests {
		if got := Classify(tc.in); got != tc.want {
			t.Errorf("%s: Classify = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// paymentFailure is a plain, unremarkable recoverable payment failure: the
// starting point for tests that vary one field.
func paymentFailure() Features {
	return Features{
		SourceType:          domain.SourcePaymentFailure,
		Amount:              domain.Money(250_000),
		ErrorCode:           "GATEWAY_ERROR",
		FailureReason:       "Payment processing failed at the gateway",
		AttemptCount:        1,
		Segment:             domain.SegmentRepeat,
		CustomerSuccessRate: 0.8,
		LifetimeValue:       domain.Money(1_500_000),
		RecencyDays:         14,
		TotalPayments:       6,
		AgeMinutes:          30,
	}
}

// representativeFeatures spans all four workflows plus the boundaries that have
// historically produced out-of-range components: zero amounts, no history,
// enormous amounts, stale records.
func representativeFeatures() []Features {
	return []Features{
		paymentFailure(),
		{SourceType: domain.SourcePaymentFailure, Amount: 1, ErrorCode: "unrecognised", AttemptCount: 9, RecencyDays: 400},
		{
			SourceType: domain.SourcePaymentFailure, Amount: domain.Money(90_000_000),
			ErrorCode: "PAYMENT_TIMEOUT", AttemptCount: 1, CustomerSuccessRate: 1, TotalPayments: 50,
			LifetimeValue: domain.Money(500_000_000), RecencyDays: 0, AgeMinutes: 1,
		},
		{
			SourceType: domain.SourceCheckoutAbandonment, Amount: domain.Money(400_000),
			CheckoutViews: 9, MinutesSinceAbandon: 12, AgeMinutes: 12, TotalPayments: 3, CustomerSuccessRate: 0.9,
		},
		{
			SourceType: domain.SourceCheckoutAbandonment, Amount: domain.Money(400_000),
			CheckoutViews: 0, MinutesSinceAbandon: 100_000, AgeMinutes: 100_000,
		},
		{
			SourceType: domain.SourceInvoiceOverdue, Amount: domain.Money(25_000_000),
			DaysOverdue: 50, ReminderCount: 1, AgeMinutes: 50 * 24 * 60, TotalPayments: 12, CustomerSuccessRate: 0.75,
		},
		{SourceType: domain.SourceInvoiceOverdue, Amount: domain.Money(6_000), DaysOverdue: 200, AgeMinutes: 200 * 24 * 60},
		{
			SourceType: domain.SourceSubscriptionFailure, Amount: domain.Money(99_900),
			ErrorCode: "BAD_REQUEST_ERROR", AttemptCount: 1, TotalPayments: 8, CustomerSuccessRate: 0.85, AgeMinutes: 60,
		},
		{
			SourceType: domain.SourceSubscriptionFailure, Amount: domain.Money(99_900),
			ErrorCode: "BAD_REQUEST_ERROR", AttemptCount: 6, TotalPayments: 8, CustomerSuccessRate: 0.2, AgeMinutes: 6000,
		},
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// upper is a local ASCII upcaser, used to build the miscased spellings the
// normalisation regression tests depend on.
func upper(s string) string {
	out := []byte(s)
	for i := range out {
		if out[i] >= 'a' && out[i] <= 'z' {
			out[i] -= 32
		}
	}
	return string(out)
}
