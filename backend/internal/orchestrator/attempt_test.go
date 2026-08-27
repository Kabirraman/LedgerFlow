package orchestrator

import (
	"testing"

	"github.com/ledgerflow/ledgerflow/internal/domain"
	"github.com/ledgerflow/ledgerflow/internal/idem"
)

func action(t domain.ActionType, status domain.ActionStatus) domain.RecoveryAction {
	return domain.RecoveryAction{ActionType: t, Status: status}
}

// TestAttemptOrdinalCountsOnlySettledAttempts is the rule that turns the
// idempotency key into a duplicate-suppression mechanism rather than just a
// unique string.
//
// The ordinal advances only once an attempt's fate is known. While an action is
// pending or ambiguous the ordinal stays put, so retrying that stage regenerates
// the same key, collides with the existing row and is reported as a duplicate —
// instead of issuing a second demand for money for an attempt that may already
// have reached the customer (SRS 20.1, 20.2).
func TestAttemptOrdinalCountsOnlySettledAttempts(t *testing.T) {
	tests := []struct {
		name  string
		prior []domain.RecoveryAction
		of    domain.ActionType
		want  int
	}{
		{
			name: "no history starts at one",
			of:   domain.ActionRetry,
			want: 1,
		},
		{
			name:  "a settled attempt advances the ordinal",
			prior: []domain.RecoveryAction{action(domain.ActionRetry, domain.ActionStatusExecuted)},
			of:    domain.ActionRetry,
			want:  2,
		},
		{
			name:  "a failed attempt is settled",
			prior: []domain.RecoveryAction{action(domain.ActionRetry, domain.ActionStatusFailed)},
			of:    domain.ActionRetry,
			want:  2,
		},
		{
			name:  "a skipped attempt is settled",
			prior: []domain.RecoveryAction{action(domain.ActionRetry, domain.ActionStatusSkipped)},
			of:    domain.ActionRetry,
			want:  2,
		},
		{
			name:  "a pending attempt does not advance the ordinal",
			prior: []domain.RecoveryAction{action(domain.ActionRetry, domain.ActionStatusPending)},
			of:    domain.ActionRetry,
			want:  1,
		},
		{
			name:  "an ambiguous attempt does not advance the ordinal",
			prior: []domain.RecoveryAction{action(domain.ActionRetry, domain.ActionStatusAmbiguous)},
			of:    domain.ActionRetry,
			want:  1,
		},
		{
			name: "a pending attempt after settled ones holds the ordinal at its own slot",
			prior: []domain.RecoveryAction{
				action(domain.ActionRetry, domain.ActionStatusExecuted),
				action(domain.ActionRetry, domain.ActionStatusFailed),
				action(domain.ActionRetry, domain.ActionStatusPending),
			},
			of:   domain.ActionRetry,
			want: 3,
		},
		{
			name: "ordinals are per action type",
			prior: []domain.RecoveryAction{
				action(domain.ActionPaymentLink, domain.ActionStatusExecuted),
				action(domain.ActionPaymentLink, domain.ActionStatusExecuted),
				action(domain.ActionRetry, domain.ActionStatusExecuted),
				action(domain.ActionReminder, domain.ActionStatusFailed),
			},
			of:   domain.ActionRetry,
			want: 2,
		},
		{
			name: "an unrelated history leaves the ordinal at one",
			prior: []domain.RecoveryAction{
				action(domain.ActionPaymentLink, domain.ActionStatusExecuted),
				action(domain.ActionReminder, domain.ActionStatusExecuted),
			},
			of:   domain.ActionRetry,
			want: 1,
		},
		{
			name: "an unknown status is not treated as settled",
			prior: []domain.RecoveryAction{
				action(domain.ActionRetry, domain.ActionStatus("mystery")),
			},
			of:   domain.ActionRetry,
			want: 1,
		},
	}

	for _, tc := range tests {
		if got := attemptOrdinal(tc.prior, tc.of); got != tc.want {
			t.Errorf("%s: attemptOrdinal = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestAttemptOrdinalProducesAStableKeyForAnUnsettledRetry states the consequence
// the ordinal exists for. It is the property, not the counter, that has to hold:
// re-running a stage whose action never settled must reproduce the same key.
func TestAttemptOrdinalProducesAStableKeyForAnUnsettledRetry(t *testing.T) {
	caseID := "case-42"

	// First pass: nothing has happened yet.
	first := idem.ActionKey(caseID, domain.ActionPaymentLink,
		attemptOrdinal(nil, domain.ActionPaymentLink))

	// The action row was reserved but the external call's fate is unknown, so the
	// stage runs again after a restart.
	pending := []domain.RecoveryAction{action(domain.ActionPaymentLink, domain.ActionStatusPending)}
	second := idem.ActionKey(caseID, domain.ActionPaymentLink,
		attemptOrdinal(pending, domain.ActionPaymentLink))

	if first != second {
		t.Errorf("an unsettled action produced a new key (%q then %q), so the restart would send a second payment link", first, second)
	}

	// Once it settles, a genuinely new attempt gets a distinct key — otherwise a
	// legitimate second attempt would be swallowed as a duplicate forever.
	settled := []domain.RecoveryAction{action(domain.ActionPaymentLink, domain.ActionStatusExecuted)}
	third := idem.ActionKey(caseID, domain.ActionPaymentLink,
		attemptOrdinal(settled, domain.ActionPaymentLink))
	if third == first {
		t.Errorf("a settled attempt reused key %q, so no second attempt could ever run", third)
	}
}
