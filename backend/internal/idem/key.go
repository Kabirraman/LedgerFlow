// Package idem generates the deterministic idempotency keys that prevent
// duplicate financial side effects (SRS FR-003, FR-043, 20.1).
//
// The key is a pure function of (case, action, attempt) so that a retried
// request — whether from a webhook redelivery, an operator double-click or a
// process restart mid-flight — produces the same key and therefore collides
// with the existing action row instead of creating a second one.
package idem

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// Prefix namespaces every key LEDGERFLOW sends to an external API.
const Prefix = "ledgerflow"

// ActionKey builds the idempotency key for a recovery action.
//
// The attempt number is part of the key so that a *deliberate* second
// intervention on the same case (a follow-up reminder, say) is distinguishable
// from an accidental replay of the first one. Callers must pass the attempt
// number from the persisted action count, never from a counter held in memory.
func ActionKey(caseID string, action domain.ActionType, attempt int) string {
	if attempt < 1 {
		attempt = 1
	}
	return fmt.Sprintf("%s_%s_%s_%d", Prefix, sanitize(caseID), sanitize(string(action)), attempt)
}

// EventKey builds the deduplication key for an inbound event.
//
// Razorpay supplies an x-razorpay-event-id header for webhooks; when present it
// is authoritative. When absent (or for first-party events) the key is derived
// from a hash of the event type, entity id and payload so that a byte-identical
// redelivery still deduplicates (SRS FR-003).
func EventKey(source, externalEventID, eventType, entityID string, payload []byte) string {
	if externalEventID != "" {
		return fmt.Sprintf("%s:%s", sanitize(source), sanitize(externalEventID))
	}
	sum := sha256.Sum256(payload)
	return fmt.Sprintf("%s:%s:%s:%s", sanitize(source), sanitize(eventType), sanitize(entityID), hex.EncodeToString(sum[:12]))
}

// ReferenceForCase builds the human-facing case reference (e.g. REV-0182).
func ReferenceForCase(seq int64) string {
	return fmt.Sprintf("REV-%04d", seq)
}

// sanitize keeps keys safe for use in URLs, headers and log lines. Razorpay's
// reference_id fields accept alphanumerics, hyphens and underscores.
func sanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
