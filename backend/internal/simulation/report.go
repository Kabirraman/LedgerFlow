package simulation

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// Report renders the required output block from SRS 17.4.
//
// The label set and their order are taken verbatim from the specification, so the
// figure a reviewer reads on the terminal, in the CI log and on the Simulation Lab
// screen is the same figure computed by the same code. A report assembled
// separately for presentation is a report that can drift from the run it describes.
//
// Everything reported here is a counter the runner incremented while the case was
// being played, never a number derived afterwards to make the block complete.
func Report(run *domain.SimulationRun) string {
	if run == nil {
		return ""
	}
	res := run.Result
	var b strings.Builder

	fmt.Fprintf(&b, "LEDGERFLOW benchmark — strategy %s\n", res.Strategy)
	fmt.Fprintf(&b, "dataset %s seed %d size %d | policy %s\n",
		run.DatasetVersion, run.Seed, res.CasesProcessed, run.PolicyVersion)
	b.WriteString(strings.Repeat("-", 56) + "\n")

	fmt.Fprintf(&b, "Cases processed:          %d\n", res.CasesProcessed)
	fmt.Fprintf(&b, "Revenue at risk:          %s\n", rupees(res.RevenueAtRisk))
	fmt.Fprintf(&b, "Eligible opportunities:   %d\n", res.EligibleOpportunities)
	fmt.Fprintf(&b, "Actions executed:         %d\n", res.ActionsExecuted)
	fmt.Fprintf(&b, "Recovered amount:         %s\n", rupees(res.RecoveredAmount))
	fmt.Fprintf(&b, "Recovery rate:            %.1f%% (%d cases)\n", res.RecoveryRate*100, res.RecoveredCount)
	fmt.Fprintf(&b, "Escalated:                %d\n", res.Escalated)
	fmt.Fprintf(&b, "Stopped safely:           %d\n", res.StoppedSafely)
	fmt.Fprintf(&b, "Policy violations:        %d\n", res.PolicyViolations)

	if run.BaselineResult != nil {
		base := *run.BaselineResult
		fmt.Fprintf(&b, "Baseline (%s) recovered amount: %s\n", base.Strategy, rupees(base.RecoveredAmount))
		if run.UpliftPercent != nil {
			fmt.Fprintf(&b, "LEDGERFLOW uplift:        %+.1f%%\n", *run.UpliftPercent)
		} else {
			// A baseline that recovered nothing yields no ratio. Saying so is more
			// honest than printing an infinity and more useful than printing nothing.
			b.WriteString("LEDGERFLOW uplift:        n/a (baseline recovered nothing)\n")
		}
		// The baseline's own violation count belongs next to its recovered amount.
		// Reporting only the gross figure would let a baseline that recovers more by
		// ignoring every control look strictly better than one that respects them,
		// which is the single most misleading thing this block could do.
		fmt.Fprintf(&b, "  baseline: %d actions, %d violations, %d stopped safely\n",
			base.ActionsExecuted, base.PolicyViolations, base.StoppedSafely)
	}

	// Supporting detail. It is below the required block rather than mixed into it,
	// so the specified lines stay verifiable against SRS 17.4 at a glance.
	b.WriteString(strings.Repeat("-", 56) + "\n")
	fmt.Fprintf(&b, "Blocked:                  %d\n", res.Blocked)
	fmt.Fprintf(&b, "Harness errors:           %d\n", res.Errors)
	fmt.Fprintf(&b, "Avg time to recovery:     %.0f min\n", res.AvgTimeToRecoveryMin)
	fmt.Fprintf(&b, "Actions:                  %s\n", breakdown(res.ActionBreakdown))

	if e := run.Agreement; e != nil {
		b.WriteString(strings.Repeat("-", 56) + "\n")
		b.WriteString("Agent evaluation (SRS 22.3)\n")
		fmt.Fprintf(&b, "  detection    precision %.2f  recall %.2f  f1 %.2f\n",
			e.DetectionPrecision, e.DetectionRecall, e.DetectionF1)
		fmt.Fprintf(&b, "  diagnosis    accuracy  %.2f  evidence coverage %.2f\n",
			e.DiagnosisAccuracy, e.EvidenceCoverage)
		fmt.Fprintf(&b, "  planner      accuracy  %.2f over %d committed (%d deferred to review)\n",
			e.InterventionAccuracy, e.InterventionsGraded, e.InterventionsDeferred)
		fmt.Fprintf(&b, "  planner      calibration error %.2f over %d samples\n",
			e.CalibrationError, e.CalibrationSamples)
		fmt.Fprintf(&b, "  executor     unauthorized actions %d\n", e.UnauthorizedActions)
		if e.ModelCalls == 0 {
			// Without this line a schema-valid rate of 0.00 over no calls reads as a
			// failing model rather than as a run with no AI client configured.
			b.WriteString("  model        no model calls (deterministic fallback path)\n")
		} else {
			fmt.Fprintf(&b, "  model        %d calls, schema valid %.2f\n", e.ModelCalls, e.SchemaValidRate)
		}
		if ok, missed := MeetsTargets(*e); !ok {
			fmt.Fprintf(&b, "  MISSED TARGETS: %s\n", strings.Join(missed, ", "))
		}
	}
	return b.String()
}

// rupees formats paise for human reading. Money stays an int64 everywhere else;
// this is the one place a float is acceptable, because the output is being read
// rather than added up.
func rupees(m domain.Money) string {
	return fmt.Sprintf("₹%.2f", m.Rupees())
}

// breakdown renders the action histogram in a fixed order so two reports can be
// diffed against each other.
func breakdown(counts map[string]int) string {
	if len(counts) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, counts[k]))
	}
	return strings.Join(parts, " ")
}
