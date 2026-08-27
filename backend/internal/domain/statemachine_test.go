package domain

import (
	"errors"
	"strings"
	"testing"
)

// allStatuses is every state in the SRS 14.2 machine. Tests iterate it rather
// than the transitions map so that a state which was never added to the table
// shows up as a failure instead of being silently skipped.
var allStatuses = []CaseStatus{
	StatusNew, StatusAnalyzing, StatusDiagnosed, StatusPlanned, StatusPolicyReview,
	StatusApproved, StatusEscalated, StatusWaitingHuman, StatusRejected, StatusExecuting,
	StatusVerifying, StatusRecovered, StatusFailed, StatusRetrying, StatusBlocked, StatusClosed,
}

// TestTransitionTableMatchesSRS is the SRS 14.2 table transcribed independently
// and compared against the implementation.
//
// It is written out in full rather than derived from the same map the code uses,
// because a test that reads its expectations from the thing under test proves only
// that the map equals itself. Every allowed edge is asserted allowed and every
// unlisted edge asserted rejected, so both adding and removing a transition fails
// here.
func TestTransitionTableMatchesSRS(t *testing.T) {
	spec := map[CaseStatus][]CaseStatus{
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
		StatusRecovered:    {},
		StatusRejected:     {},
		StatusBlocked:      {},
		StatusClosed:       {},
	}

	if len(spec) != len(allStatuses) {
		t.Fatalf("spec covers %d states but there are %d", len(spec), len(allStatuses))
	}

	for _, from := range allStatuses {
		allowed := map[CaseStatus]bool{}
		for _, to := range spec[from] {
			allowed[to] = true
		}
		for _, to := range allStatuses {
			// The two universal rules are asserted separately below.
			if from == to || to == StatusClosed {
				continue
			}
			got := CanTransition(from, to)
			if got != allowed[to] {
				verb := "rejected"
				if got {
					verb = "allowed"
				}
				t.Errorf("%s -> %s was %s; SRS 14.2 says otherwise", from, to, verb)
			}
		}
	}
}

// TestAnyStateMayClose covers the first universal rule: a case can always be
// closed, because the payment can arrive or a stopping rule can fire at any point.
// CLOSED itself is final.
func TestAnyStateMayClose(t *testing.T) {
	for _, from := range allStatuses {
		if from == StatusClosed {
			continue
		}
		if !CanTransition(from, StatusClosed) {
			t.Errorf("%s -> CLOSED was rejected", from)
		}
	}
	for _, to := range allStatuses {
		if to == StatusClosed {
			continue
		}
		if CanTransition(StatusClosed, to) {
			t.Errorf("CLOSED -> %s was allowed; CLOSED is final", to)
		}
	}
}

// TestSameStateIsAlwaysAllowed covers the second universal rule. Workflow steps
// are retried — by the orchestrator, by a redelivered webhook, by an operator
// double-click — and a replay that lands the case in the state it is already in
// must be a no-op rather than an error, or every retry path needs its own
// special case.
func TestSameStateIsAlwaysAllowed(t *testing.T) {
	for _, s := range allStatuses {
		if !CanTransition(s, s) {
			t.Errorf("%s -> %s (no-op replay) was rejected", s, s)
		}
		if err := ValidateTransition(s, s); err != nil {
			t.Errorf("ValidateTransition(%s, %s) = %v", s, s, err)
		}
	}
}

// TestTerminalStatesAgree checks that the two ways of asking "is this case done"
// give the same answer. They are used in different places — the store filters
// open cases with one, the orchestrator skips finished ones with the other — and
// a disagreement would leave a case both closed and eligible for another
// intervention.
func TestTerminalStatesAgree(t *testing.T) {
	wantTerminal := map[CaseStatus]bool{
		StatusRecovered: true,
		StatusRejected:  true,
		StatusBlocked:   true,
		StatusClosed:    true,
	}
	for _, s := range allStatuses {
		if got := IsTerminal(s); got != wantTerminal[s] {
			t.Errorf("IsTerminal(%s) = %v, want %v", s, got, wantTerminal[s])
		}
		if got := s.IsTerminal(); got != wantTerminal[s] {
			t.Errorf("%s.IsTerminal() = %v, want %v", s, got, wantTerminal[s])
		}
	}
	if IsTerminal(CaseStatus("NOT_A_STATE")) {
		t.Error("an unknown status was reported terminal")
	}
}

// TestTerminalStatesHaveNoWayOut is the audit property. Once a case is recovered,
// rejected or blocked, the only move left is CLOSED — nothing may reopen it into
// the execution path, because a second intervention after a recorded recovery is
// a duplicate demand on the customer (SRS 20.1).
func TestTerminalStatesHaveNoWayOut(t *testing.T) {
	for _, from := range []CaseStatus{StatusRecovered, StatusRejected, StatusBlocked, StatusClosed} {
		for _, to := range allStatuses {
			if to == from || to == StatusClosed {
				continue
			}
			if CanTransition(from, to) {
				t.Errorf("terminal %s -> %s was allowed", from, to)
			}
		}
	}
}

// TestRecoveredIsOnlyReachableThroughVerification is the SRS 14.2 guarantee that
// makes the recovered amount trustworthy: a case cannot be marked RECOVERED
// because an agent said so, only because VERIFYING confirmed it against the
// gateway.
func TestRecoveredIsOnlyReachableThroughVerification(t *testing.T) {
	for _, from := range allStatuses {
		if from == StatusVerifying || from == StatusRecovered {
			continue
		}
		if CanTransition(from, StatusRecovered) {
			t.Errorf("%s -> RECOVERED was allowed, bypassing verification", from)
		}
	}
	if !CanTransition(StatusVerifying, StatusRecovered) {
		t.Error("VERIFYING -> RECOVERED was rejected")
	}
}

// TestExecutingIsOnlyReachableThroughApproval is the other half of the same
// guarantee, and the state-machine expression of SRS AC-003: nothing executes
// until a policy verdict or a human has approved it.
func TestExecutingIsOnlyReachableThroughApproval(t *testing.T) {
	for _, from := range allStatuses {
		switch from {
		case StatusApproved, StatusRetrying, StatusExecuting:
			continue
		}
		if CanTransition(from, StatusExecuting) {
			t.Errorf("%s -> EXECUTING was allowed without passing through APPROVED", from)
		}
	}
}

// TestHappyPathIsWalkable walks the SRS 2.3 product loop end to end. Each
// individual edge is checked above; this asserts they compose into the path the
// system actually takes, so an edge removed from the middle fails as a broken
// workflow rather than as one missing table entry.
func TestHappyPathIsWalkable(t *testing.T) {
	paths := map[string][]CaseStatus{
		"autonomous recovery": {
			StatusNew, StatusAnalyzing, StatusDiagnosed, StatusPlanned, StatusPolicyReview,
			StatusApproved, StatusExecuting, StatusVerifying, StatusRecovered, StatusClosed,
		},
		"escalated then approved": {
			StatusNew, StatusAnalyzing, StatusDiagnosed, StatusPlanned, StatusPolicyReview,
			StatusEscalated, StatusWaitingHuman, StatusApproved, StatusExecuting,
			StatusVerifying, StatusRecovered,
		},
		"escalated then rejected": {
			StatusNew, StatusAnalyzing, StatusDiagnosed, StatusEscalated,
			StatusWaitingHuman, StatusRejected, StatusClosed,
		},
		"failed then retried": {
			StatusNew, StatusAnalyzing, StatusDiagnosed, StatusPlanned, StatusPolicyReview,
			StatusApproved, StatusExecuting, StatusFailed, StatusRetrying, StatusExecuting,
			StatusVerifying, StatusRecovered,
		},
		"failed then escalated": {
			StatusNew, StatusAnalyzing, StatusDiagnosed, StatusPlanned, StatusPolicyReview,
			StatusApproved, StatusExecuting, StatusVerifying, StatusFailed,
			StatusEscalated, StatusWaitingHuman, StatusApproved, StatusExecuting,
		},
		"retry re-analyses": {
			StatusNew, StatusAnalyzing, StatusDiagnosed, StatusPlanned, StatusPolicyReview,
			StatusApproved, StatusExecuting, StatusFailed, StatusRetrying, StatusAnalyzing,
		},
		"blocked at detection": {StatusNew, StatusBlocked},
		"blocked at policy": {
			StatusNew, StatusAnalyzing, StatusDiagnosed, StatusPlanned,
			StatusPolicyReview, StatusBlocked,
		},
	}
	for name, path := range paths {
		for i := 0; i < len(path)-1; i++ {
			if err := ValidateTransition(path[i], path[i+1]); err != nil {
				t.Errorf("%s: step %d: %v", name, i, err)
			}
		}
	}
}

// TestValidateTransitionNamesBothStates checks the error is diagnosable from a log
// line alone. An "invalid transition" with no states in it sends whoever is on
// call back to the database to work out what the case was doing.
func TestValidateTransitionNamesBothStates(t *testing.T) {
	err := ValidateTransition(StatusRecovered, StatusExecuting)
	if err == nil {
		t.Fatal("RECOVERED -> EXECUTING did not error")
	}
	if !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("error %v does not wrap ErrInvalidTransition, so callers cannot classify it", err)
	}
	msg := err.Error()
	for _, want := range []string{"RECOVERED", "EXECUTING"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not name %s", msg, want)
		}
	}
}

// TestTerminalStatusForPolicy pins the verdict-to-state mapping (SRS 10.2, 11.1).
//
// The default branch matters most: an unrecognised verdict must land on BLOCKED,
// not on APPROVED. A policy engine returning something the workflow does not
// understand is a reason to stop, never a reason to proceed (SRS 20.4).
func TestTerminalStatusForPolicy(t *testing.T) {
	cases := map[PolicyResult]CaseStatus{
		PolicyPass:                  StatusApproved,
		PolicyEscalate:              StatusEscalated,
		PolicyBlock:                 StatusBlocked,
		PolicyResult(""):            StatusBlocked,
		PolicyResult("MAYBE_LATER"): StatusBlocked,
	}
	for verdict, want := range cases {
		if got := TerminalStatusForPolicy(verdict); got != want {
			t.Errorf("TerminalStatusForPolicy(%q) = %s, want %s", verdict, got, want)
		}
	}
}

// TestAllCaseStatusesCoversTransitionTable is a drift guard.
//
// AllCaseStatuses is an explicit ordered slice rather than the keys of the
// transition table, because Go randomises map iteration order and a UI whose state
// filter reshuffled on every request would look broken. The cost of that choice is
// a second list that can fall out of step with the first: adding a state to the
// machine without adding it here would make the new state invisible to the filter
// and, worse, make CaseStatus.Valid reject it — an API rejecting a status its own
// workflow produces. This test is what makes that mistake a failing build.
func TestAllCaseStatusesCoversTransitionTable(t *testing.T) {
	listed := map[CaseStatus]bool{}
	for _, s := range AllCaseStatuses {
		if listed[s] {
			t.Errorf("AllCaseStatuses lists %s twice", s)
		}
		listed[s] = true
	}

	for from, targets := range transitions {
		if !listed[from] {
			t.Errorf("state %s has transitions but is missing from AllCaseStatuses", from)
		}
		for _, to := range targets {
			if !listed[to] {
				t.Errorf("state %s is a transition target but is missing from AllCaseStatuses", to)
			}
		}
	}

	// And the converse: a state in the vocabulary that the machine has never heard
	// of would be offered as a filter that can match nothing.
	for _, s := range AllCaseStatuses {
		if _, known := transitions[s]; !known {
			t.Errorf("AllCaseStatuses lists %s, which the transition table does not define", s)
		}
		if !s.Valid() {
			t.Errorf("%s is in AllCaseStatuses but Valid() rejects it", s)
		}
	}
}

// TestRunModeValid pins the run-mode vocabulary the API validates filters against.
func TestRunModeValid(t *testing.T) {
	for _, m := range AllRunModes {
		if !m.Valid() {
			t.Errorf("%s is in AllRunModes but Valid() rejects it", m)
		}
	}
	for _, bad := range []RunMode{"", "production", "LIVE_TEST", "dry_run"} {
		if bad.Valid() {
			t.Errorf("RunMode(%q).Valid() = true, want false", bad)
		}
	}
}
