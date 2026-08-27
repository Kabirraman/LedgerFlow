// Package domain holds LEDGERFLOW's core types, enumerations and the case
// state machine. It has no dependencies on storage, HTTP or AI providers so
// that every other package can safely import it (SRS NFR-007).
package domain

import "fmt"

// Environment tags every record so test-mode data can never be confused with
// live data (SRS FR-006).
type Environment string

const (
	EnvTest Environment = "test"
	EnvLive Environment = "live"
)

// RunMode selects the operating mode described in SRS 2.5.
type RunMode string

const (
	// ModeLiveTest uses real Razorpay test-mode resources and webhooks.
	ModeLiveTest RunMode = "live_test"
	// ModeSimulation uses the synthetic benchmark and must never reach an
	// external Razorpay endpoint (SRS AC-009).
	ModeSimulation RunMode = "simulation"
	// ModeReview is the human-in-the-loop path for high-value cases.
	ModeReview RunMode = "review"
)

// AllRunModes is the mode vocabulary.
var AllRunModes = []RunMode{ModeLiveTest, ModeSimulation, ModeReview}

// Valid reports whether the mode is one this system recognises.
func (m RunMode) Valid() bool {
	for _, v := range AllRunModes {
		if v == m {
			return true
		}
	}
	return false
}

// SourceType identifies which revenue-leak workflow produced a case
// (SRS 11.1 - 11.4).
type SourceType string

const (
	SourcePaymentFailure      SourceType = "payment_failure"
	SourceCheckoutAbandonment SourceType = "checkout_abandonment"
	SourceInvoiceOverdue      SourceType = "invoice_overdue"
	SourceSubscriptionFailure SourceType = "subscription_failure"
)

// AllSourceTypes is the canonical ordering used by dashboards and filters.
var AllSourceTypes = []SourceType{
	SourcePaymentFailure,
	SourceCheckoutAbandonment,
	SourceInvoiceOverdue,
	SourceSubscriptionFailure,
}

func (s SourceType) Valid() bool {
	for _, v := range AllSourceTypes {
		if v == s {
			return true
		}
	}
	return false
}

// Urgency is the Detection Agent's time-pressure signal (SRS 8.1).
type Urgency string

const (
	UrgencyLow      Urgency = "low"
	UrgencyMedium   Urgency = "medium"
	UrgencyHigh     Urgency = "high"
	UrgencyCritical Urgency = "critical"
)

func (u Urgency) Valid() bool {
	switch u {
	case UrgencyLow, UrgencyMedium, UrgencyHigh, UrgencyCritical:
		return true
	}
	return false
}

// Rank gives urgency a sortable weight for the prioritized queue (SRS FR-013).
func (u Urgency) Rank() int {
	switch u {
	case UrgencyCritical:
		return 4
	case UrgencyHigh:
		return 3
	case UrgencyMedium:
		return 2
	case UrgencyLow:
		return 1
	}
	return 0
}

// RootCause is the closed set of diagnosis labels from SRS 8.2. Free-text
// diagnoses are rejected: an unrecognised label is coerced to
// RootCauseUnknown, which routes the case to review or a safe action.
type RootCause string

const (
	RootCauseTransientFailure     RootCause = "transient_failure"
	RootCauseInsufficientFunds    RootCause = "insufficient_funds"
	RootCauseCheckoutAbandonment  RootCause = "checkout_abandonment"
	RootCauseOverdueReceivable    RootCause = "overdue_receivable"
	RootCauseSubscriptionFailure  RootCause = "subscription_failure"
	RootCauseAuthenticationFailed RootCause = "authentication_failed"
	RootCauseUnknown              RootCause = "unknown"
)

// AllRootCauses is the canonical ordering, and the vocabulary handed to the
// model as an enum constraint.
var AllRootCauses = []RootCause{
	RootCauseTransientFailure,
	RootCauseInsufficientFunds,
	RootCauseCheckoutAbandonment,
	RootCauseOverdueReceivable,
	RootCauseSubscriptionFailure,
	RootCauseAuthenticationFailed,
	RootCauseUnknown,
}

func (r RootCause) Valid() bool {
	for _, v := range AllRootCauses {
		if v == r {
			return true
		}
	}
	return false
}

// NormalizeRootCause maps model output onto the allow-list, defaulting to
// unknown rather than trusting an unrecognised label (SRS FR-022).
func NormalizeRootCause(s string) RootCause {
	rc := RootCause(s)
	if rc.Valid() {
		return rc
	}
	return RootCauseUnknown
}

// ActionType is the allow-listed intervention set. The Intervention Planner
// may only choose from this set (SRS FR-030); anything else is rejected before
// it can reach the executor.
type ActionType string

const (
	ActionRetry       ActionType = "retry"
	ActionPaymentLink ActionType = "payment_link"
	ActionReminder    ActionType = "reminder"
	ActionEscalate    ActionType = "escalate"
	ActionNoAction    ActionType = "no_action"
)

// AllowedActions is the executor allow-list. Order is significant only for
// deterministic presentation.
var AllowedActions = []ActionType{
	ActionRetry,
	ActionPaymentLink,
	ActionReminder,
	ActionEscalate,
	ActionNoAction,
}

// Valid reports whether the action is on the allow-list.
func (a ActionType) Valid() bool {
	for _, v := range AllowedActions {
		if v == a {
			return true
		}
	}
	return false
}

// IsExternal reports whether executing the action performs a side effect
// against an external payment API. escalate and no_action never do.
func (a ActionType) IsExternal() bool {
	switch a {
	case ActionRetry, ActionPaymentLink, ActionReminder:
		return true
	}
	return false
}

// ParseActionType validates untrusted input (model output or API payload)
// against the allow-list.
func ParseActionType(s string) (ActionType, error) {
	a := ActionType(s)
	if !a.Valid() {
		return "", fmt.Errorf("%w: %q is not an allow-listed action", ErrActionNotAllowed, s)
	}
	return a, nil
}

// PolicyResult is the deterministic policy engine verdict (SRS 10.2).
type PolicyResult string

const (
	PolicyPass     PolicyResult = "PASS"
	PolicyBlock    PolicyResult = "BLOCK"
	PolicyEscalate PolicyResult = "ESCALATE"
)

// CaseStatus enumerates the case state machine from SRS 14.2.
type CaseStatus string

const (
	StatusNew          CaseStatus = "NEW"
	StatusAnalyzing    CaseStatus = "ANALYZING"
	StatusDiagnosed    CaseStatus = "DIAGNOSED"
	StatusPlanned      CaseStatus = "PLANNED"
	StatusPolicyReview CaseStatus = "POLICY_REVIEW"
	StatusApproved     CaseStatus = "APPROVED"
	StatusEscalated    CaseStatus = "ESCALATED"
	StatusWaitingHuman CaseStatus = "WAITING_HUMAN"
	StatusRejected     CaseStatus = "REJECTED"
	StatusExecuting    CaseStatus = "EXECUTING"
	StatusVerifying    CaseStatus = "VERIFYING"
	StatusRecovered    CaseStatus = "RECOVERED"
	StatusFailed       CaseStatus = "FAILED"
	StatusRetrying     CaseStatus = "RETRYING"
	StatusBlocked      CaseStatus = "BLOCKED"
	StatusClosed       CaseStatus = "CLOSED"
)

// AllCaseStatuses is the vocabulary in workflow order, used for filter validation
// and for listing the legal next states of a case.
//
// It is an explicit ordered slice rather than the keys of the transition table:
// map iteration order in Go is randomised, and a UI whose state filter reshuffled
// on every request would look broken.
var AllCaseStatuses = []CaseStatus{
	StatusNew, StatusAnalyzing, StatusDiagnosed, StatusPlanned, StatusPolicyReview,
	StatusEscalated, StatusWaitingHuman, StatusApproved, StatusRejected,
	StatusExecuting, StatusVerifying, StatusRecovered, StatusFailed, StatusRetrying,
	StatusBlocked, StatusClosed,
}

// Valid reports whether the status is one this system issues. An unrecognised
// value from a query string is refused rather than treated as "no filter".
func (c CaseStatus) Valid() bool {
	for _, v := range AllCaseStatuses {
		if v == c {
			return true
		}
	}
	return false
}

// IsTerminal reports whether no further automated transition is expected.
//
// It delegates to the package-level IsTerminal, which derives the answer from the
// state machine table. Keeping a second hand-written list here would be a second
// thing to update, and the two disagreeing is how a case ends up simultaneously
// closed and eligible for another intervention.
func (c CaseStatus) IsTerminal() bool {
	return IsTerminal(c)
}

// ActionStatus tracks the lifecycle of a recovery action record. The record is
// persisted as Pending *before* execution so a crash mid-flight leaves a
// reconcilable trace (SRS FR-043).
type ActionStatus string

const (
	ActionStatusPending   ActionStatus = "pending"
	ActionStatusExecuted  ActionStatus = "executed"
	ActionStatusFailed    ActionStatus = "failed"
	ActionStatusSkipped   ActionStatus = "skipped"
	ActionStatusAmbiguous ActionStatus = "ambiguous"
)

// OutcomeType is the verified result of an intervention (SRS FR-050).
type OutcomeType string

const (
	OutcomeRecovered    OutcomeType = "recovered"
	OutcomeNotRecovered OutcomeType = "not_recovered"
	OutcomePending      OutcomeType = "pending"
	OutcomeStopped      OutcomeType = "stopped"
	OutcomeEscalated    OutcomeType = "escalated"
)

// Segment is the customer segmentation from SRS 9.3.
type Segment string

const (
	SegmentNew          Segment = "new"
	SegmentRepeat       Segment = "repeat"
	SegmentHighValue    Segment = "high_value"
	SegmentB2B          Segment = "b2b"
	SegmentSubscription Segment = "subscription"
)

var AllSegments = []Segment{
	SegmentNew, SegmentRepeat, SegmentHighValue, SegmentB2B, SegmentSubscription,
}

func (s Segment) Valid() bool {
	for _, v := range AllSegments {
		if v == s {
			return true
		}
	}
	return false
}

// Role gates the internal API surface (SRS 15.1).
type Role string

const (
	RoleOperator Role = "operator"
	RoleReviewer Role = "reviewer"
	RoleAdmin    Role = "admin"
)

// AllRoles lists every assignable role.
var AllRoles = []Role{RoleOperator, RoleReviewer, RoleAdmin}

// Permits reports whether the role satisfies a required role. Admin implies
// reviewer, and reviewer implies operator.
func (r Role) Permits(required Role) bool {
	rank := map[Role]int{RoleOperator: 1, RoleReviewer: 2, RoleAdmin: 3}
	return rank[r] >= rank[required]
}

// Valid reports whether r is a known role. An unknown role must never be
// treated as permissive.
func (r Role) Valid() bool {
	for _, v := range AllRoles {
		if v == r {
			return true
		}
	}
	return false
}

// ApprovalDecision records the explicit human verdict (SRS 10.4).
type ApprovalDecision string

const (
	ApprovalPending  ApprovalDecision = "pending"
	ApprovalApproved ApprovalDecision = "approved"
	ApprovalRejected ApprovalDecision = "rejected"
)
