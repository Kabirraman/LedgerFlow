package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// --- events ---

const eventCols = `id, source, external_event_id, event_type, payload_json, signature_valid,
	rejection_reason, entity_id, entity_timestamp, received_at, processed_at, environment`

func scanEvent(row rowScanner) (*domain.Event, error) {
	var e domain.Event
	var payload []byte
	err := row.Scan(&e.ID, &e.Source, &e.ExternalEventID, &e.EventType, &payload, &e.SignatureValid,
		&e.RejectionReason, &e.EntityID, &e.EntityTimestamp, &e.ReceivedAt, &e.ProcessedAt, &e.Environment)
	if err != nil {
		return nil, err
	}
	e.PayloadJSON = json.RawMessage(payload)
	return &e, nil
}

// RecordEvent persists a received event. The unique index on external_event_id
// is the deduplication mechanism: a redelivered webhook returns
// created=false and the caller must not reprocess it (SRS FR-003, AC-006).
//
// Rejected events (bad signature) are recorded too, with signature_valid=false,
// so an attack or misconfiguration is visible in the audit trail rather than
// silently dropped (SRS FR-002).
func (s *Store) RecordEvent(ctx context.Context, e *domain.Event) (created bool, err error) {
	if e.ExternalEventID == "" {
		return false, fmt.Errorf("%w: external event id is required", domain.ErrValidation)
	}
	if e.ID == "" {
		e.ID = NewID("evt")
	}
	if e.ReceivedAt.IsZero() {
		e.ReceivedAt = s.now()
	}
	if len(e.PayloadJSON) == 0 {
		e.PayloadJSON = json.RawMessage(`{}`)
	}

	const q = `
		INSERT INTO events (id, source, external_event_id, event_type, payload_json, signature_valid,
		                    rejection_reason, entity_id, entity_timestamp, received_at, processed_at, environment)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'test')`
	_, execErr := s.pool.Exec(ctx, q, e.ID, e.Source, e.ExternalEventID, e.EventType,
		[]byte(e.PayloadJSON), e.SignatureValid, truncate(e.RejectionReason, 500), e.EntityID,
		e.EntityTimestamp, e.ReceivedAt, e.ProcessedAt)
	if execErr == nil {
		return true, nil
	}
	if !IsUniqueViolation(execErr) {
		return false, execErr
	}
	row := s.pool.QueryRow(ctx, `SELECT `+eventCols+` FROM events WHERE external_event_id = $1`, e.ExternalEventID)
	existing, getErr := scanEvent(row)
	if getErr != nil {
		return false, notFound(getErr, "event "+e.ExternalEventID)
	}
	*e = *existing
	return false, nil
}

// MarkEventProcessed stamps an event as handled.
func (s *Store) MarkEventProcessed(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE events SET processed_at = $2 WHERE id = $1 AND processed_at IS NULL`,
		id, s.now())
	return err
}

// GetEvent loads one event.
func (s *Store) GetEvent(ctx context.Context, id string) (*domain.Event, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+eventCols+` FROM events WHERE id = $1`, id)
	e, err := scanEvent(row)
	if err != nil {
		return nil, notFound(err, "event "+id)
	}
	return e, nil
}

// LatestEntityTimestamp returns the newest entity timestamp already processed
// for an entity. Out-of-order delivery is handled by comparing against this:
// an event whose entity state is older than what we already applied is
// discarded rather than allowed to regress the case (SRS FR-004).
func (s *Store) LatestEntityTimestamp(ctx context.Context, entityID string) (*time.Time, error) {
	if entityID == "" {
		return nil, nil
	}
	var ts *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT max(entity_timestamp) FROM events
		WHERE entity_id = $1 AND signature_valid AND processed_at IS NOT NULL`, entityID).Scan(&ts)
	if err != nil {
		return nil, err
	}
	return ts, nil
}

// ListEvents returns the most recent events for the ops view.
func (s *Store) ListEvents(ctx context.Context, limit int) ([]domain.Event, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `SELECT `+eventCols+
		` FROM events ORDER BY received_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Event{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// --- operational counters (SRS 18.3) ---

// Counter names. Kept as constants so the dashboard query and the incrementing
// call sites cannot drift apart.
const (
	CounterWebhooksReceived         = "webhooks_received"
	CounterWebhookSignatureFailures = "webhook_signature_failures"
	CounterDuplicateEvents          = "duplicate_events"
	CounterActionAPIFailures        = "action_api_failures"
	CounterActionLatency            = "action_latency_ms"
	CounterAgentLatency             = "agent_latency_ms"
	CounterAgentFallbacks           = "agent_fallbacks"
	CounterAgentInvalidJSON         = "agent_invalid_json"
	CounterAgentCalls               = "agent_calls"
	CounterPromptInjectionBlocked   = "prompt_injection_blocked"
	CounterActionsExecuted          = "actions_executed"
	CounterActionsRejected          = "actions_rejected"
	CounterActionsAmbiguous         = "actions_ambiguous"
	CounterDuplicateActionRequests  = "duplicate_action_requests"
	CounterPolicyBlocks             = "policy_blocks"
	CounterEscalations              = "escalations"
)

// IncrCounter bumps a named counter by one.
func (s *Store) IncrCounter(ctx context.Context, name string) error {
	return s.AddCounter(ctx, name, 1, 0)
}

// AddCounter bumps a counter by n and adds sum to its accumulated total, which
// is how latency averages are kept without a metrics backend.
func (s *Store) AddCounter(ctx context.Context, name string, n, sum int64) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO operational_counters (name, value, sum_value, updated_at)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (name) DO UPDATE SET
			value = operational_counters.value + EXCLUDED.value,
			sum_value = operational_counters.sum_value + EXCLUDED.sum_value,
			updated_at = EXCLUDED.updated_at`, name, n, sum, s.now())
	return err
}

// CounterValue is one counter's state.
type CounterValue struct {
	Count int64
	Sum   int64
}

// Mean returns sum/count, or 0 when there is no sample.
func (c CounterValue) Mean() float64 {
	if c.Count == 0 {
		return 0
	}
	return float64(c.Sum) / float64(c.Count)
}

// Counters returns every operational counter.
func (s *Store) Counters(ctx context.Context) (map[string]CounterValue, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, value, sum_value FROM operational_counters`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]CounterValue{}
	for rows.Next() {
		var name string
		var v CounterValue
		if err := rows.Scan(&name, &v.Count, &v.Sum); err != nil {
			return nil, err
		}
		out[name] = v
	}
	return out, rows.Err()
}
