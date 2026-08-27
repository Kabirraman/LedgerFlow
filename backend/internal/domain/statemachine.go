package domain

import "fmt"

// transitions encodes the case state machine from SRS 14.2. A transition that
// is not listed here is rejected, which is what keeps the audit trail
// reconstructable: no case can arrive at RECOVERED without having passed
// through policy review and execution.
//
// The "any state → CLOSED" rule from the SRS is handled separately in
// CanTransition so it does not have to be duplicated on every row.
var transitions = map[CaseStatus][]CaseStatus{
	StatusNew:          {StatusAnalyzing, StatusBlocked},
	StatusAnalyzing:    {StatusDiagnosed, StatusEscalated, StatusBlocked},
	StatusDiagnosed:    {StatusPlanned, StatusEscalated, StatusBlocked},
	StatusPlanned:      {StatusPolicyReview},
	StatusPolicyReview: {StatusApproved, StatusEscalated, StatusBlocked},
	StatusEscalated:    {StatusWaitingHuman},
	StatusWaitingHuman: {StatusApproved, StatusRejected},
	StatusApproved:     {StatusExecuting},
	StatusExecuting:    {StatusVerifying, StatusFailed},
	StatusVerifying:    {StatusRecovered, StatusFailed},
	StatusFailed:       {StatusRetrying, StatusEscalated},
	StatusRetrying:     {StatusAnalyzing, StatusExecuting},
	// Terminal states have no outgoing transitions other than the universal
	// close rule.
	StatusRecovered: {},
	StatusRejected:  {},
	StatusBlocked:   {},
	StatusClosed:    {},
}

// CanTransition reports whether moving from -> to is permitted.
//
// Two universal rules apply on top of the table:
//   - Any state may move to CLOSED (payment completed, or a stopping rule
//     fired). CLOSED itself is final.
//   - A transition to the same state is a no-op and always allowed, so that
//     idempotent replays of a workflow step do not error.
func CanTransition(from, to CaseStatus) bool {
	if from == to {
		return true
	}
	if to == StatusClosed {
		return from != StatusClosed
	}
	for _, allowed := range transitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// ValidateTransition returns ErrInvalidTransition with both states named, so
// the failure is diagnosable from logs alone.
func ValidateTransition(from, to CaseStatus) error {
	if !CanTransition(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}
	return nil
}

// IsTerminal reports whether a case has finished moving.
//
// Derived from the transition table rather than from a second hand-written list,
// so a state that gains an outgoing transition stops being terminal automatically
// and the two cannot disagree.
func IsTerminal(s CaseStatus) bool {
	next, known := transitions[s]
	return known && len(next) == 0
}

// TerminalStatusForPolicy maps a policy verdict to the case state the workflow
// should move into (SRS 10.2, 11.1).
func TerminalStatusForPolicy(result PolicyResult) CaseStatus {
	switch result {
	case PolicyPass:
		return StatusApproved
	case PolicyEscalate:
		return StatusEscalated
	default:
		return StatusBlocked
	}
}
