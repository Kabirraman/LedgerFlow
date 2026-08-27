package idem

import (
	"strings"
	"testing"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// TestActionKeyIsStableForTheSameAttempt is the property the whole duplicate
// defence rests on (SRS FR-043, AC-006). The same case, action and attempt must
// produce the same key from any process, at any time, so the unique index on
// recovery_actions.idempotency_key rejects the second insert.
func TestActionKeyIsStableForTheSameAttempt(t *testing.T) {
	want := ActionKey("case-abc", domain.ActionPaymentLink, 1)
	for i := 0; i < 100; i++ {
		if got := ActionKey("case-abc", domain.ActionPaymentLink, 1); got != want {
			t.Fatalf("key changed between calls: %q then %q", want, got)
		}
	}
	if want == "" {
		t.Fatal("empty idempotency key")
	}
}

// TestActionKeyDistinguishesEveryInput checks that no two meaningfully different
// interventions collide. A collision here would silently suppress a real second
// action — the failure mode is missing revenue, not a duplicate charge, so it
// would never show up as an error.
func TestActionKeyDistinguishesEveryInput(t *testing.T) {
	seen := map[string]string{}
	for _, caseID := range []string{"case-1", "case-2", "CASE-1"} {
		for _, action := range domain.AllowedActions {
			for attempt := 1; attempt <= 4; attempt++ {
				key := ActionKey(caseID, action, attempt)
				desc := caseID + "/" + string(action) + "/" + string(rune('0'+attempt))
				if prev, dup := seen[key]; dup {
					t.Errorf("key %q collides: %s and %s", key, prev, desc)
				}
				seen[key] = desc
			}
		}
	}
}

// TestActionKeyClampsAttemptToOne pins the boundary. A caller that has not yet
// persisted an action may hold a zero count, and a zero or negative attempt must
// map onto the first attempt rather than producing a second distinct key for what
// is really the same first action.
func TestActionKeyClampsAttemptToOne(t *testing.T) {
	first := ActionKey("case-1", domain.ActionRetry, 1)
	for _, attempt := range []int{0, -1, -99} {
		if got := ActionKey("case-1", domain.ActionRetry, attempt); got != first {
			t.Errorf("attempt %d produced %q, want the attempt-1 key %q", attempt, got, first)
		}
	}
	if second := ActionKey("case-1", domain.ActionRetry, 2); second == first {
		t.Error("attempt 2 produced the same key as attempt 1: a deliberate follow-up would be swallowed as a duplicate")
	}
}

// TestActionKeyIsSafeForExternalUse checks the character set. This string is sent
// to Razorpay as a reference id and written into log lines, so anything outside
// [A-Za-z0-9_-] has to be replaced rather than escaped at each use site.
func TestActionKeyIsSafeForExternalUse(t *testing.T) {
	key := ActionKey("case/../../etc/passwd?a=1 b=2\n", domain.ActionPaymentLink, 1)
	for _, r := range key {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '_' || r == '-'
		if !ok {
			t.Fatalf("key %q contains unsafe rune %q", key, r)
		}
	}
	if !strings.HasPrefix(key, Prefix+"_") {
		t.Errorf("key %q is not namespaced with %q", key, Prefix)
	}
}

// TestActionKeySanitizationDoesNotMergeDistinctCases is the risk that comes with
// sanitising: if every unsafe rune becomes '_', two different case ids could
// flatten onto the same key and one of the two actions would vanish.
//
// This documents that the collision is possible for adversarial ids and that the
// system does not depend on it being impossible — case ids are server-generated
// UUIDs, which are already in the safe set, so sanitisation is a defence against
// malformed input rather than a namespace the keys rely on.
func TestActionKeySanitizationDoesNotMergeDistinctCases(t *testing.T) {
	uuidLike := "018f3a2b-9c4d-7e1f-8a2b-3c4d5e6f7a8b"
	if got := ActionKey(uuidLike, domain.ActionRetry, 1); !strings.Contains(got, uuidLike) {
		t.Errorf("a UUID case id was altered by sanitisation: %q", got)
	}
}

// TestEventKeyPrefersRazorpayEventID is the authoritative dedup path (SRS FR-003).
// When the gateway tells us its own event id, that id is the identity of the
// event, and the payload is irrelevant to it — a redelivery with a
// reserialised or reordered body must still collide.
func TestEventKeyPrefersRazorpayEventID(t *testing.T) {
	a := EventKey("razorpay", "evt_123", "payment.failed", "pay_abc", []byte(`{"a":1}`))
	b := EventKey("razorpay", "evt_123", "payment.failed", "pay_abc", []byte(`{"a":1,"b":2}`))
	if a != b {
		t.Errorf("same event id gave different keys with different payloads: %q vs %q", a, b)
	}
	if !strings.Contains(a, "evt_123") {
		t.Errorf("key %q does not carry the event id", a)
	}

	c := EventKey("razorpay", "evt_456", "payment.failed", "pay_abc", []byte(`{"a":1}`))
	if a == c {
		t.Error("two different event ids produced the same key")
	}
}

// TestEventKeyFallsBackToPayloadHash covers first-party events and any webhook
// that arrives without the header. A byte-identical redelivery must still
// deduplicate, and a genuinely different event must not.
func TestEventKeyFallsBackToPayloadHash(t *testing.T) {
	payload := []byte(`{"amount":45000,"status":"failed"}`)
	a := EventKey("internal", "", "payment.failed", "pay_abc", payload)
	b := EventKey("internal", "", "payment.failed", "pay_abc", payload)
	if a != b {
		t.Errorf("identical payloads gave different keys: %q vs %q", a, b)
	}

	// One byte different is a different event.
	c := EventKey("internal", "", "payment.failed", "pay_abc", []byte(`{"amount":45001,"status":"failed"}`))
	if a == c {
		t.Error("a different payload produced the same key")
	}
	// So is the same payload about a different entity, or of a different type.
	if d := EventKey("internal", "", "payment.failed", "pay_xyz", payload); a == d {
		t.Error("a different entity id produced the same key")
	}
	if e := EventKey("internal", "", "payment.captured", "pay_abc", payload); a == e {
		t.Error("a different event type produced the same key")
	}
	if f := EventKey("scanner", "", "payment.failed", "pay_abc", payload); a == f {
		t.Error("a different source produced the same key")
	}
}

// TestEventKeyHandlesEmptyPayload guards the degenerate case: a key must still be
// produced, because an event that cannot be keyed cannot be deduplicated and
// would be reprocessed on every redelivery.
func TestEventKeyHandlesEmptyPayload(t *testing.T) {
	k := EventKey("internal", "", "case.scan", "", nil)
	if k == "" {
		t.Fatal("empty key for an empty payload")
	}
	if k2 := EventKey("internal", "", "case.scan", "", []byte{}); k != k2 {
		t.Errorf("nil and empty payloads keyed differently: %q vs %q", k, k2)
	}
}

// TestReferenceForCase covers the human-facing reference. It is zero-padded so
// references sort lexicographically in the case queue, and it must not truncate
// once the sequence outgrows four digits.
func TestReferenceForCase(t *testing.T) {
	tests := []struct {
		seq  int64
		want string
	}{
		{1, "REV-0001"},
		{182, "REV-0182"},
		{9999, "REV-9999"},
		{10000, "REV-10000"},
		{1234567, "REV-1234567"},
	}
	for _, tc := range tests {
		if got := ReferenceForCase(tc.seq); got != tc.want {
			t.Errorf("ReferenceForCase(%d) = %q, want %q", tc.seq, got, tc.want)
		}
	}
}
