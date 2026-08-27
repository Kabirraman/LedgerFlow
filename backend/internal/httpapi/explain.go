package httpapi

import (
	"sort"
	"strconv"
	"strings"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// explanation is the "why this action" block required by SRS AC-010 and FR-063.
//
// It is assembled entirely from persisted, structured facts: the reason codes the
// agents emitted, the policy checks that ran, the alternatives that were
// considered and the amounts held in the database. There is no model prose here
// and no private chain-of-thought — the SRS is explicit that the reasoning shown
// must be the recorded decision trail rather than the model's internal narration
// (SRS 8.5, 19.4, AC-010).
type explanation struct {
	Headline string `json:"headline"`
	// Because is the ordered list of grounded reasons, each a code plus its plain
	// reading, so the UI can show either and a reviewer can audit the mapping.
	Because []explainItem `json:"because"`
	// Evidence names the fields the assessment relied on. Field names, not values:
	// the values are already on the case detail, and duplicating them here would
	// create a second place for them to disagree.
	Evidence []string `json:"evidence"`
	// Considered lists the actions that were eligible but not chosen (SRS FR-035).
	Considered []string `json:"considered,omitempty"`
	// Controls is every policy rule that ran, passing ones included, because a
	// reviewer needs to see the control set and not just the objection (SRS 16.2).
	Controls []explainControl `json:"controls"`
	// StopCondition is when the system will stop pursuing this case.
	StopCondition string `json:"stop_condition,omitempty"`
	// DecidedBy is "model" or "deterministic". A fallback decision must be legible
	// as one (SRS 20.4, 22.3).
	DecidedBy string `json:"decided_by,omitempty"`
	ModelName string `json:"model_name,omitempty"`
	// Uncertainty carries the diagnosis's own admitted gaps, including UNKNOWN.
	Uncertainty []string `json:"uncertainty,omitempty"`
	// Confidence is the planner's recovery probability, and ExpectedRecovery the
	// SRS 9.2 figure. Both are shown so a reviewer can see the arithmetic behind
	// the prioritisation rather than a ranking with no stated basis.
	Confidence       float64      `json:"confidence"`
	ExpectedRecovery domain.Money `json:"expected_recovery"`
}

type explainItem struct {
	Code    string `json:"code"`
	Reading string `json:"reading"`
}

type explainControl struct {
	Rule    string              `json:"rule"`
	Reading string              `json:"reading"`
	Result  domain.PolicyResult `json:"result"`
	Details string              `json:"details,omitempty"`
}

// buildExplanation renders the decision trail for one case.
//
// A case with no decision yet gets an honest "not decided" headline rather than an
// invented rationale. Explaining a decision that has not happened would be the
// worst kind of interface: convincing and untrue.
func buildExplanation(d *domain.CaseDetail) explanation {
	ex := explanation{Because: []explainItem{}, Evidence: []string{}, Controls: []explainControl{}}
	if d == nil {
		return ex
	}

	ex.Evidence = append(ex.Evidence, d.Case.EvidenceRefs...)

	codes := append([]string{}, d.Case.ReasonCodes...)
	if d.Diagnosis != nil {
		ex.Uncertainty = d.Diagnosis.UncertaintyFlags
		ex.Evidence = mergeUnique(ex.Evidence, d.Diagnosis.Evidence)
		if d.Diagnosis.RootCause == domain.RootCauseUnknown {
			// UNKNOWN is a permitted, meaningful answer (SRS 8.2). Saying so beats
			// leaving the reader to infer it from an absent cause.
			ex.Uncertainty = mergeUnique(ex.Uncertainty, []string{"root cause could not be determined from the available evidence"})
		}
	}

	switch {
	case d.Decision == nil:
		ex.Headline = "No action has been selected yet — this case is still " + humanStatus(d.Case.Status) + "."
	default:
		codes = mergeUnique(codes, d.Decision.ReasonCodes)
		ex.Considered = d.Decision.Alternatives
		ex.StopCondition = d.Decision.StopCondition
		ex.DecidedBy = d.Decision.Source
		ex.ModelName = d.Decision.ModelName
		ex.Confidence = d.Decision.RecoveryProbability
		ex.ExpectedRecovery = d.Decision.ExpectedRecovery
		ex.Headline = headline(d)
	}

	for _, code := range codes {
		ex.Because = append(ex.Because, explainItem{Code: code, Reading: readReason(code)})
	}
	for _, pc := range d.PolicyChecks {
		ex.Controls = append(ex.Controls, explainControl{
			Rule:    pc.Rule,
			Reading: readRule(pc.Rule),
			Result:  pc.Result,
			Details: pc.Details,
		})
	}
	sort.SliceStable(ex.Controls, func(i, j int) bool {
		// Objections first: a reviewer opening this block wants the blocking rule at
		// the top, not alphabetically buried among the passes.
		return controlWeight(ex.Controls[i].Result) < controlWeight(ex.Controls[j].Result)
	})
	return ex
}

func controlWeight(r domain.PolicyResult) int {
	switch r {
	case domain.PolicyBlock:
		return 0
	case domain.PolicyEscalate:
		return 1
	default:
		return 2
	}
}

// headline states the decision in one sentence, using the trusted amount from the
// case record rather than anything the model produced (SRS 19.4).
func headline(d *domain.CaseDetail) string {
	dec := d.Decision
	amount := formatRupees(d.Case.RevenueAtRisk)
	who := "the customer"
	if d.Customer != nil && d.Customer.Name != "" {
		who = d.Customer.Name
	}
	odds := strconv.Itoa(int(dec.RecoveryProbability*100+0.5)) + "%"

	switch dec.RecommendedAction {
	case domain.ActionRetry:
		return "Retry the ₹" + amount + " payment for " + who +
			", because the failure looks transient and a retry has an estimated " + odds + " chance of succeeding."
	case domain.ActionPaymentLink:
		return "Send " + who + " a payment link for ₹" + amount +
			", because the customer needs to act on the payment themselves; estimated " + odds + " chance of recovery."
	case domain.ActionReminder:
		return "Send " + who + " a reminder about ₹" + amount +
			", because the intent is there but the payment has not been completed; estimated " + odds + " chance of recovery."
	case domain.ActionEscalate:
		return "Escalate ₹" + amount + " for " + who +
			" to a human reviewer rather than acting automatically."
	case domain.ActionNoAction:
		return "Take no action on ₹" + amount + " for " + who +
			", because no available intervention has positive expected value here."
	default:
		return "No recognised action was selected for this case."
	}
}

func humanStatus(s domain.CaseStatus) string {
	switch s {
	case domain.StatusNew:
		return "newly detected"
	case domain.StatusAnalyzing:
		return "being analysed"
	case domain.StatusDiagnosed:
		return "diagnosed and awaiting a plan"
	case domain.StatusPolicyReview:
		return "in policy review"
	case domain.StatusWaitingHuman:
		return "waiting for a human decision"
	case domain.StatusBlocked:
		return "blocked by policy"
	default:
		return string(s)
	}
}

// reasonReadings maps every reason code the agents can emit to plain English.
//
// The map is exhaustive over the vocabulary produced by internal/risk and
// internal/agents. An unmapped code still renders — readReason falls back to the
// code itself rather than dropping it, because a reason silently omitted from an
// explanation is worse than an ugly one.
var reasonReadings = map[string]string{
	// Detection, from the risk scorer.
	"payment_failure":           "a payment attempt failed",
	"checkout_abandonment":      "a checkout was started and never completed",
	"overdue_receivable":        "an invoice is past its due date",
	"subscription_failure":      "a subscription charge failed",
	"likely_transient":          "the failure type usually clears on its own",
	"high_intent":               "the customer showed strong purchase intent",
	"reliable_payer":            "this customer's payments usually succeed",
	"unreliable_payer":          "this customer's payments often fail",
	"high_value":                "the amount is large relative to typical cases",
	"single_failure":            "this is the first failed attempt",
	"repeat_failure":            "the payment has failed more than once",
	"repeat_customer":           "the customer has paid successfully before",
	"new_customer":              "this is a first-time customer",
	"time_critical":             "the failure is recent, so acting now matters",
	"closing_window":            "the window for recovering this is nearly closed",
	"prior_intervention_failed": "an earlier recovery attempt did not work",

	// Diagnosis and planning.
	"transient_failure_retryable":              "the root cause is a temporary failure that can be retried",
	"insufficient_funds_needs_customer_action": "the customer's funds were insufficient, so they must act",
	"abandoned_cart_recoverable":               "the abandoned cart can still be completed",
	"receivable_overdue":                       "the receivable is overdue and needs collection",
	"subscription_charge_failed":               "the recurring charge failed",
	"authentication_failed_prefer_link":        "authentication failed, so a fresh payment link is safer than a retry",
	"root_cause_unknown":                       "the root cause could not be established from the evidence",
	"requires_human_review":                    "policy or confidence requires a person to decide",
	"no_positive_expected_value":               "no available action is worth its cost here",
	"prior_retry_attempted":                    "a retry has already been attempted",
	"prior_reminder_sent":                      "a reminder has already been sent",
	"high_value_customer":                      "the customer is in the high-value segment",
	"history_backed_strategy":                  "this action has a measured track record for similar cases",

	// Safety overrides. These are the ones a reviewer most needs in plain words.
	"low_confidence_diagnosis":      "the diagnosis was not confident enough to act on automatically",
	"above_autonomous_ceiling":      "the amount is above the limit this system may act on without a human",
	"below_confidence_floor":        "confidence fell below the policy threshold",
	"low_confidence":                "the model's confidence was too low to rely on",
	"safety_override":               "a safety rule overrode the suggested action",
	"model_action_rejected":         "the model suggested an action that is not permitted, so it was discarded",
	"model_flagged_risk":            "the model flagged this case as at risk",
	"prompt_injection_detected":     "text in the customer's data tried to influence the decision and was ignored",
	"probability_capped_by_history": "the estimated success rate was capped by measured history",
	"unrecognised_label_coerced":    "an unrecognised model label was coerced to a safe value",
	"no_evidence_cited":             "the model cited no evidence, so its output was not relied on",
	"no_error_code":                 "the gateway supplied no error code to diagnose from",
	"source_record_missing":         "the underlying source record could not be loaded",
}

func readReason(code string) string {
	if r, ok := reasonReadings[code]; ok {
		return r
	}
	return code
}

// ruleReadings maps policy rule identifiers to plain English. The rule names come
// from internal/policy, and the readings describe the control rather than the
// outcome, so a PASS row reads correctly too.
var ruleReadings = map[string]string{
	"action_allow_list":                "the action must be one of the allow-listed types",
	"amount_integrity":                 "the amount must match the trusted transaction record",
	"simulation_boundary":              "a simulated case may never reach a live gateway",
	"terminal_case_state":              "a closed or recovered case must not be acted on again",
	"already_recovered":                "the money has already been recovered",
	"customer_already_paid":            "the customer has already paid by another route",
	"conflicting_external_state":       "the gateway's state conflicts with acting now",
	"max_retry_count":                  "retries are capped per case",
	"max_reminder_count":               "reminders are capped per case",
	"max_actions_per_case":             "total actions are capped per case",
	"max_actions_per_customer_per_day": "actions are capped per customer per day",
	"cooldown_minutes":                 "a cooldown must elapse between actions",
	"min_action_confidence":            "the planner's confidence must clear the policy floor",
	"max_automated_amount":             "amounts above the ceiling may not be acted on automatically",
	"require_human_approval_above":     "amounts above this threshold need human approval",
	"api_failure_budget":               "repeated gateway failures halt further attempts",
	"feasibility":                      "the action must be feasible for this case",
}

func readRule(rule string) string {
	if r, ok := ruleReadings[rule]; ok {
		return r
	}
	return rule
}

// mergeUnique appends the items of b that a does not already contain, preserving
// order. Used so evidence and reason codes from two agents combine without
// repeating a line in the explanation.
func mergeUnique(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	for _, v := range a {
		seen[v] = true
	}
	out := a
	for _, v := range b {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// formatRupees renders paise as rupees with Indian digit grouping — 12,34,567.00
// rather than 1,234,567.00 — because this is a rupee product and a merchant reads
// the wrong grouping as a wrong number.
//
// The conversion divides by 100 at the last moment. All arithmetic upstream is
// integer paise, so nothing here can introduce a rounding drift into a total
// (SRS 9.3, NFR-014).
func formatRupees(m domain.Money) string {
	paise := int64(m)
	sign := ""
	if paise < 0 {
		sign, paise = "-", -paise
	}
	whole := strconv.FormatInt(paise/100, 10)
	frac := paise % 100

	grouped := groupIndian(whole)
	return sign + grouped + "." + pad2(frac)
}

// groupIndian inserts separators after the last three digits and then every two.
func groupIndian(s string) string {
	if len(s) <= 3 {
		return s
	}
	head, tail := s[:len(s)-3], s[len(s)-3:]
	parts := []string{}
	for len(head) > 2 {
		parts = append([]string{head[len(head)-2:]}, parts...)
		head = head[:len(head)-2]
	}
	if head != "" {
		parts = append([]string{head}, parts...)
	}
	return strings.Join(parts, ",") + "," + tail
}

func pad2(n int64) string {
	if n < 10 {
		return "0" + strconv.FormatInt(n, 10)
	}
	return strconv.FormatInt(n, 10)
}
