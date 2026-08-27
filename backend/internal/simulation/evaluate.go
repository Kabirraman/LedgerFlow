package simulation

import (
	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// AI evaluation targets from SRS 22.3. They are constants here so the benchmark
// regression test can assert against the spec rather than against whatever the
// current implementation happens to score.
const (
	TargetDetectionPrecision   = 0.80
	TargetDetectionRecall      = 0.75
	TargetDiagnosisAccuracy    = 0.80
	TargetInterventionAccuracy = 0.75
)

// schemaFailureReasons are the fallback reasons that mean the model returned a
// response and the response was unusable.
//
// Everything else — model_disabled, timeout, agent_unavailable, transport errors —
// is a call that produced no response at all. Counting those as schema failures
// would conflate "the model is wrong" with "the model is unreachable", which are
// different problems with different fixes, and would make a network outage look
// like a prompt defect.
var schemaFailureReasons = map[string]bool{
	"invalid_json":         true,
	"action_not_permitted": true,
}

// Evaluate scores a strategy's traces against ground truth (SRS 22.3).
//
// Every metric here is computed over an explicit denominator, and every denominator
// excludes cases the metric cannot speak to: a diagnosis that never ran is not a
// wrong diagnosis, and a probability with no executed action has no observed
// outcome to be calibrated against. Padding a denominator with cases the system
// was never asked about is the easiest way to manufacture a good score, so the
// exclusions are stated rather than implied.
func Evaluate(traces []caseTrace) domain.AgentEvaluation {
	var eval domain.AgentEvaluation

	var truePos, falsePos, falseNeg int
	var diagTotal, diagCorrect int
	var planTotal, planCorrect int
	var evidenceTotal, evidenceCited int
	var schemaValid, schemaInvalid int

	// Calibration samples: the probability the system stated, paired with what
	// actually happened. Only cases where an action executed contribute, because
	// only those have an observed outcome.
	type sample struct {
		predicted float64
		recovered bool
	}
	var samples []sample

	for _, t := range traces {
		// Detection. The ground-truth label is "this case is a real recovery
		// opportunity", which is the question a merchant is actually asking: of the
		// cases you told me to work, how many were worth working, and of the ones
		// worth working, how many did you find.
		if t.Detection != nil {
			switch {
			case t.Detection.IsAtRisk && t.Case.Recoverable:
				truePos++
			case t.Detection.IsAtRisk && !t.Case.Recoverable:
				falsePos++
			case !t.Detection.IsAtRisk && t.Case.Recoverable:
				falseNeg++
			}
			countSchema(t.Detection.Source, t.Detection.FallbackReason, &schemaValid, &schemaInvalid)
		}

		// Diagnosis. Cases whose true root cause is UNKNOWN are excluded: SRS 22.3
		// sets the target on supported labels, and UNKNOWN is the agent's honest
		// answer rather than a label to be graded against.
		if t.Diagnosis != nil {
			countSchema(t.Diagnosis.Source, t.Diagnosis.FallbackReason, &schemaValid, &schemaInvalid)
			evidenceTotal++
			if len(t.Diagnosis.Evidence) > 0 {
				evidenceCited++
			}
			if t.Case.TrueRootCause != domain.RootCauseUnknown {
				diagTotal++
				if t.Diagnosis.RootCause == t.Case.TrueRootCause {
					diagCorrect++
				}
			}
		}

		// Intervention. Graded only where a plan was actually produced. A case
		// detection declined never reached the planner, so it cannot be scored
		// here — the cost of declining it is already charged to detection recall.
		if t.Decision.RecommendedAction != "" && t.Detection != nil && t.Detection.IsAtRisk {
			countSchema(t.Decision.Source, "", &schemaValid, &schemaInvalid)
			// An escalation is a deferral, not a choice. Grading it against the
			// benchmark-best action would score the planner as wrong for declining
			// to act on a diagnosis too weak to act on — which is the behaviour SRS
			// 20.4 requires of it. It is counted separately instead, so a planner
			// that defers its way to a high accuracy is visible rather than
			// flattered.
			if t.Decision.RecommendedAction == domain.ActionEscalate {
				eval.InterventionsDeferred++
			} else {
				planTotal++
				if acceptableChoice(t.Decision.RecommendedAction, t.Case) {
					planCorrect++
				}
			}
		}

		// Calibration pairs a stated probability with an observed outcome, so it
		// requires that the probability described the action that ran. When a
		// reviewer authorised something other than the recommendation, the planner
		// stated no probability for what actually executed and there is nothing of
		// the planner's to calibrate.
		if t.Executed != "" {
			if t.Executed == t.Decision.RecommendedAction {
				samples = append(samples, sample{predicted: t.Decision.RecoveryProbability, recovered: t.Recovered})
			}
			// An action executed against a BLOCK verdict is an unauthorized action,
			// and so is one outside the allow-list. SRS 22.3 requires this to be
			// zero; it is reported rather than asserted so a regression shows up as
			// a number instead of a panic.
			if t.Verdict == domain.PolicyBlock || !t.Executed.Valid() {
				eval.UnauthorizedActions++
			}
		}
	}

	eval.DetectionPrecision = ratio(truePos, truePos+falsePos)
	eval.DetectionRecall = ratio(truePos, truePos+falseNeg)
	eval.DetectionF1 = f1(eval.DetectionPrecision, eval.DetectionRecall)
	eval.DiagnosisAccuracy = ratio(diagCorrect, diagTotal)
	eval.InterventionAccuracy = ratio(planCorrect, planTotal)
	eval.InterventionsGraded = planTotal
	eval.EvidenceCoverage = ratio(evidenceCited, evidenceTotal)
	eval.ModelCalls = schemaValid + schemaInvalid
	if eval.ModelCalls > 0 {
		eval.SchemaValidRate = ratio(schemaValid, eval.ModelCalls)
	}

	predicted := make([]float64, 0, len(samples))
	outcomes := make([]bool, 0, len(samples))
	for _, s := range samples {
		predicted = append(predicted, s.predicted)
		outcomes = append(outcomes, s.recovered)
	}
	eval.CalibrationSamples = len(samples)
	eval.CalibrationError = calibrationError(predicted, outcomes)
	return eval
}

// acceptableChoice implements the SRS 3.2 rule: the benchmark-best action, or any
// action a reviewer accepted as equally reasonable, counts as correct.
//
// Grading against a single "right answer" would punish a defensible alternative —
// a payment link instead of a retry on an authentication failure recovers a similar
// share and is not a mistake — and would make the accuracy figure a measure of
// agreement with one opinion rather than of competence.
func acceptableChoice(chosen domain.ActionType, b domain.BenchmarkCase) bool {
	if chosen == b.BenchmarkBestAction {
		return true
	}
	for _, a := range b.AcceptableActions {
		if chosen == a {
			return true
		}
	}
	return false
}

// countSchema classifies one agent call.
func countSchema(source, fallbackReason string, valid, invalid *int) {
	if source == "ai" {
		*valid++
		return
	}
	if schemaFailureReasons[fallbackReason] {
		*invalid++
	}
}

// calibrationError is the bucketed gap between stated and observed recovery
// probability (SRS 18.2).
//
// Predictions are grouped into deciles and each bucket compares its mean
// prediction against the share of its cases that actually recovered; the buckets
// are then averaged weighted by size. Bucketing is what makes the number mean
// something: a per-case absolute error would be dominated by the irreducible
// randomness of a single Bernoulli outcome, and a system that predicted 0.7 for a
// case that failed would be scored as badly wrong when it was in fact well
// calibrated.
func calibrationError(predicted []float64, recovered []bool) float64 {
	if len(predicted) == 0 || len(predicted) != len(recovered) {
		return 0
	}
	const buckets = 10
	var sum [buckets]float64
	var hits [buckets]int
	var count [buckets]int

	for i, p := range predicted {
		if p < 0 {
			p = 0
		}
		if p > 1 {
			p = 1
		}
		b := int(p * buckets)
		if b >= buckets {
			b = buckets - 1
		}
		sum[b] += p
		count[b]++
		if recovered[i] {
			hits[b]++
		}
	}

	var weighted float64
	var total int
	for b := 0; b < buckets; b++ {
		if count[b] == 0 {
			continue
		}
		meanPredicted := sum[b] / float64(count[b])
		observed := float64(hits[b]) / float64(count[b])
		gap := meanPredicted - observed
		if gap < 0 {
			gap = -gap
		}
		weighted += gap * float64(count[b])
		total += count[b]
	}
	if total == 0 {
		return 0
	}
	return weighted / float64(total)
}

// MinGradedInterventions is the smallest number of committed actions an accuracy
// figure is allowed to rest on.
//
// Without a floor, the intervention target has a trivial pass: escalate every
// case but one, get that one right, report 1.00. The floor is set to a tenth of
// the SRS 17.1 benchmark size, which is small enough not to be a second hidden
// target and large enough that clearing it requires the planner to have actually
// decided something.
const MinGradedInterventions = DefaultSize / 10

// MeetsTargets reports whether an evaluation clears every SRS 22.3 threshold.
//
// It returns the failing metric names rather than a bare boolean so a CI failure
// says which target was missed, which is the only form of this check that is
// actually useful when it fires (SRS 23.3).
func MeetsTargets(e domain.AgentEvaluation) (bool, []string) {
	var missed []string
	if e.DetectionPrecision < TargetDetectionPrecision {
		missed = append(missed, "detection_precision")
	}
	if e.DetectionRecall < TargetDetectionRecall {
		missed = append(missed, "detection_recall")
	}
	if e.DiagnosisAccuracy < TargetDiagnosisAccuracy {
		missed = append(missed, "diagnosis_accuracy")
	}
	if e.InterventionsGraded < MinGradedInterventions {
		missed = append(missed, "intervention_accuracy_not_measured")
	} else if e.InterventionAccuracy < TargetInterventionAccuracy {
		missed = append(missed, "intervention_accuracy")
	}
	if e.UnauthorizedActions > 0 {
		missed = append(missed, "unauthorized_actions")
	}
	return len(missed) == 0, missed
}

func ratio(n, d int) float64 {
	if d <= 0 {
		return 0
	}
	return float64(n) / float64(d)
}

func f1(precision, recall float64) float64 {
	if precision+recall == 0 {
		return 0
	}
	return 2 * precision * recall / (precision + recall)
}
