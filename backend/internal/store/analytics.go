package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// liveCases restricts every dashboard aggregate to live_test cases. Simulation
// rows share the same tables (so the Simulation Lab can reuse the pipeline) and
// must never be mixed into reported business results (SRS 25.2).
const liveCases = `rc.mode = 'live_test'`

// DashboardSummary computes the SRS 16.1 KPI block. The whole panel is built
// from a handful of aggregate queries rather than one giant join, because the
// numbers come from different grains (cases, actions, outcomes, counters) and a
// single join would double-count.
func (s *Store) DashboardSummary(ctx context.Context) (*domain.DashboardSummary, error) {
	out := &domain.DashboardSummary{
		BySource: []domain.SourceBreakdown{},
		Activity: []domain.ActivityItem{},
	}

	// Case-grain aggregates.
	err := s.pool.QueryRow(ctx, `
		SELECT
			COALESCE(sum(rc.revenue_at_risk), 0),
			COALESCE(sum(rc.expected_recovery), 0),
			COALESCE(sum(rc.recovered_amount), 0),
			count(*) FILTER (WHERE NOT (rc.status IN ('RECOVERED','CLOSED','BLOCKED','REJECTED'))),
			COALESCE(sum(rc.revenue_at_risk) FILTER (WHERE rc.status <> 'RECOVERED'), 0),
			count(*) FILTER (WHERE rc.status IN ('ESCALATED','WAITING_HUMAN')),
			count(*) FILTER (WHERE rc.status = 'BLOCKED'),
			count(*),
			count(*) FILTER (WHERE rc.status = 'RECOVERED')
		FROM risk_cases rc WHERE `+liveCases).Scan(
		&out.RevenueAtRisk, &out.ExpectedRecovery, &out.RecoveredAmount,
		&out.OpenCases, &out.UnresolvedRevenue, &out.EscalatedCases, &out.BlockedActions,
		&out.Funnel.Identified, &out.Funnel.Recovered)
	if err != nil {
		return nil, fmt.Errorf("dashboard case aggregates: %w", err)
	}

	// Recovery rate is money-weighted, not case-weighted: recovering one large
	// case matters more than recovering several small ones (SRS 3.2).
	if out.RevenueAtRisk > 0 {
		out.RecoveryRate = float64(out.RecoveredAmount) / float64(out.RevenueAtRisk)
	}
	if out.Funnel.Identified > 0 {
		out.EscalationRate = float64(out.EscalatedCases) / float64(out.Funnel.Identified)
	}

	// Funnel middle stages.
	if err := s.pool.QueryRow(ctx, `
		SELECT
			count(DISTINCT rc.id) FILTER (WHERE EXISTS (SELECT 1 FROM diagnoses d WHERE d.case_id = rc.id)),
			count(DISTINCT rc.id) FILTER (WHERE EXISTS (
				SELECT 1 FROM recovery_actions ra
				WHERE ra.case_id = rc.id AND ra.status = 'executed'
				  AND ra.action_type IN ('retry','payment_link','reminder')))
		FROM risk_cases rc WHERE `+liveCases).Scan(&out.Funnel.Diagnosed, &out.Funnel.Actioned); err != nil {
		return nil, fmt.Errorf("dashboard funnel: %w", err)
	}

	// Action-grain aggregate: automated interventions actually performed.
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM recovery_actions ra
		WHERE ra.mode = 'live_test' AND ra.status = 'executed'
		  AND ra.action_type IN ('retry','payment_link','reminder')`).Scan(&out.AutomatedActions); err != nil {
		return nil, fmt.Errorf("dashboard actions: %w", err)
	}
	if out.AutomatedActions > 0 {
		out.RecoveredPerIntervention = out.RecoveredAmount / domain.Money(out.AutomatedActions)
	}

	// Outcome-grain aggregate: mean time to recovery.
	if err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(avg(o.time_to_recovery_seconds), 0) / 60.0
		FROM outcomes o JOIN risk_cases rc ON rc.id = o.case_id
		WHERE o.outcome = 'recovered' AND o.time_to_recovery_seconds > 0 AND `+liveCases).
		Scan(&out.AvgRecoveryMinutes); err != nil {
		return nil, fmt.Errorf("dashboard time to recovery: %w", err)
	}

	violations, err := s.CountPolicyViolations(ctx)
	if err != nil {
		return nil, fmt.Errorf("dashboard policy violations: %w", err)
	}
	out.PolicyViolations = violations

	if out.BySource, err = s.SourceBreakdown(ctx); err != nil {
		return nil, err
	}
	if out.Activity, err = s.RecentActivity(ctx, 20); err != nil {
		return nil, err
	}
	ops, err := s.OperationalMetrics(ctx)
	if err != nil {
		return nil, err
	}
	out.Operational = *ops
	return out, nil
}

// SourceBreakdown groups KPIs by the four revenue-loss workflows (SRS 16.1).
// Every source type appears even at zero, so the demo panel does not change
// shape as cases arrive.
func (s *Store) SourceBreakdown(ctx context.Context) ([]domain.SourceBreakdown, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT rc.source_type, count(*), COALESCE(sum(rc.revenue_at_risk),0), COALESCE(sum(rc.recovered_amount),0)
		FROM risk_cases rc WHERE `+liveCases+` GROUP BY rc.source_type`)
	if err != nil {
		return nil, fmt.Errorf("source breakdown: %w", err)
	}
	defer rows.Close()

	seen := map[domain.SourceType]domain.SourceBreakdown{}
	for rows.Next() {
		var b domain.SourceBreakdown
		if err := rows.Scan(&b.SourceType, &b.Cases, &b.RevenueAtRisk, &b.Recovered); err != nil {
			return nil, err
		}
		if b.RevenueAtRisk > 0 {
			b.RecoveryRate = float64(b.Recovered) / float64(b.RevenueAtRisk)
		}
		seen[b.SourceType] = b
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]domain.SourceBreakdown, 0, len(domain.AllSourceTypes))
	for _, st := range domain.AllSourceTypes {
		if b, ok := seen[st]; ok {
			out = append(out, b)
			continue
		}
		out = append(out, domain.SourceBreakdown{SourceType: st})
	}
	return out, nil
}

// RecentActivity is the live feed on the dashboard (SRS 16.1). It unions the
// four events an operator cares about — executed, blocked, escalated,
// recovered — so the feed reads as a narrative rather than a table dump.
func (s *Store) RecentActivity(ctx context.Context, limit int) ([]domain.ActivityItem, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	const q = `
		SELECT case_id, reference, kind, action_type, amount, detail, at FROM (
			SELECT ra.case_id, rc.reference, 'executed' AS kind, ra.action_type::text AS action_type,
			       ra.amount, COALESCE(NULLIF(ra.external_id,''), ra.idempotency_key) AS detail,
			       COALESCE(ra.executed_at, ra.requested_at) AS at
			FROM recovery_actions ra JOIN risk_cases rc ON rc.id = ra.case_id
			WHERE ra.status = 'executed' AND ra.mode = 'live_test'

			UNION ALL

			SELECT o.case_id, rc.reference, 'recovered', '', o.recovered_amount,
			       o.verification_source, COALESCE(o.recovered_at, o.created_at)
			FROM outcomes o JOIN risk_cases rc ON rc.id = o.case_id
			WHERE o.outcome = 'recovered' AND rc.mode = 'live_test'

			UNION ALL

			SELECT pc.case_id, rc.reference, 'blocked', COALESCE(d.recommended_action::text,''),
			       rc.revenue_at_risk, pc.rule || ': ' || pc.details, pc.created_at
			FROM policy_checks pc
			JOIN risk_cases rc ON rc.id = pc.case_id
			LEFT JOIN agent_decisions d ON d.id = pc.decision_id
			WHERE pc.result = 'BLOCK' AND rc.mode = 'live_test'

			UNION ALL

			SELECT a.case_id, rc.reference, 'escalated', COALESCE(d.recommended_action::text,''),
			       rc.revenue_at_risk, a.reason, a.requested_at
			FROM approvals a
			JOIN risk_cases rc ON rc.id = a.case_id
			LEFT JOIN agent_decisions d ON d.id = a.decision_id
			WHERE rc.mode = 'live_test'
		) feed
		ORDER BY at DESC LIMIT $1`

	rows, err := s.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("recent activity: %w", err)
	}
	defer rows.Close()
	out := []domain.ActivityItem{}
	for rows.Next() {
		var it domain.ActivityItem
		var actionType string
		if err := rows.Scan(&it.CaseID, &it.Reference, &it.Kind, &actionType,
			&it.Amount, &it.Detail, &it.At); err != nil {
			return nil, err
		}
		it.ActionType = domain.ActionType(actionType)
		it.Detail = truncate(it.Detail, 160)
		out = append(out, it)
	}
	return out, rows.Err()
}

// OperationalMetrics reads the SRS 18.3 counters.
func (s *Store) OperationalMetrics(ctx context.Context) (*domain.OperationalMetrics, error) {
	counters, err := s.Counters(ctx)
	if err != nil {
		return nil, fmt.Errorf("operational metrics: %w", err)
	}
	m := &domain.OperationalMetrics{
		WebhooksReceived:         int(counters[CounterWebhooksReceived].Count),
		WebhookSignatureFailures: int(counters[CounterWebhookSignatureFailures].Count),
		DuplicateEvents:          int(counters[CounterDuplicateEvents].Count),
		ActionAPIFailures:        int(counters[CounterActionAPIFailures].Count),
		AvgActionLatencyMS:       counters[CounterActionLatency].Mean(),
		AvgAgentLatencyMS:        counters[CounterAgentLatency].Mean(),
		AgentFallbackCount:       int(counters[CounterAgentFallbacks].Count),
	}
	if m.WebhooksReceived > 0 {
		m.DuplicateEventRate = float64(m.DuplicateEvents) / float64(m.WebhooksReceived)
	}
	return m, nil
}

// CustomerHistory is the trailing behaviour of one customer that risk scoring and
// diagnosis read (SRS 9.1, 8.2). Both fields come from persisted facts, never
// from a webhook payload or model output.
type CustomerHistory struct {
	// Recoveries counts this customer's prior cases that ended in verified
	// recovery. A customer who has responded to a recovery attempt before is
	// materially more likely to respond again.
	Recoveries int
	// LastPaymentAt is the most recent successful payment. Nil means none on
	// record, which the scorer treats as unknown rather than as "long ago".
	LastPaymentAt *time.Time
}

// LoadCustomerHistory gathers both facts in one round trip.
func (s *Store) LoadCustomerHistory(ctx context.Context, customerID string) (CustomerHistory, error) {
	var h CustomerHistory
	if customerID == "" {
		return h, nil
	}
	const q = `
		SELECT
			(SELECT count(*) FROM outcomes o
			   JOIN risk_cases rc ON rc.id = o.case_id
			  WHERE rc.customer_id = $1 AND o.outcome = 'recovered'),
			(SELECT max(created_at) FROM transactions
			  WHERE customer_id = $1 AND status IN ('captured','authorized'))`
	if err := s.pool.QueryRow(ctx, q, customerID).Scan(&h.Recoveries, &h.LastPaymentAt); err != nil {
		return h, fmt.Errorf("load history for customer %s: %w", customerID, err)
	}
	return h, nil
}

// --- strategy metrics (SRS FR-053) ---

// RecordStrategyAttempt increments the attempts side of the learning loop. It is
// the counterpart to SettleRecovery, which increments successes: attempts are
// counted when an action is executed, successes only when recovery is verified,
// so a success rate can never exceed 1.
func (s *Store) RecordStrategyAttempt(ctx context.Context, seg domain.Segment,
	st domain.SourceType, at domain.ActionType) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO strategy_metrics (id, segment, source_type, action_type, attempts, successes, recovered_amount, updated_at)
		VALUES ($1,$2,$3,$4,1,0,0,$5)
		ON CONFLICT ON CONSTRAINT strategy_metrics_unique DO UPDATE SET
			attempts = strategy_metrics.attempts + 1,
			updated_at = EXCLUDED.updated_at`,
		NewID("smet"), seg, st, at, s.now())
	return err
}

// ListStrategyMetrics returns the learned performance table.
func (s *Store) ListStrategyMetrics(ctx context.Context) ([]domain.StrategyMetric, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, segment, source_type, action_type, attempts, successes, recovered_amount, updated_at
		FROM strategy_metrics ORDER BY recovered_amount DESC, attempts DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.StrategyMetric{}
	for rows.Next() {
		var m domain.StrategyMetric
		if err := rows.Scan(&m.ID, &m.Segment, &m.SourceType, &m.ActionType,
			&m.Attempts, &m.Successes, &m.RecoveredAmount, &m.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// StrategyPriors returns observed success rates keyed by
// "<segment>|<source_type>|<action_type>", for the planner's evidence-backed
// preference (SRS FR-032). Buckets with too few attempts are omitted rather
// than reported as extreme rates: one success out of one attempt is not a
// 100% strategy.
const minAttemptsForPrior = 5

func (s *Store) StrategyPriors(ctx context.Context) (map[string]float64, error) {
	metrics, err := s.ListStrategyMetrics(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, m := range metrics {
		if m.Attempts < minAttemptsForPrior {
			continue
		}
		out[string(m.Segment)+"|"+string(m.SourceType)+"|"+string(m.ActionType)] = m.SuccessRate()
	}
	return out, nil
}

// --- audit log (SRS FR-052) ---

// AppendAudit writes one append-only audit entry. Audit writes never fail a
// business operation: callers log the error and continue, because losing an
// audit line is less harmful than rolling back a completed recovery.
func (s *Store) AppendAudit(ctx context.Context, e domain.AuditLog) error {
	if e.Timestamp.IsZero() {
		e.Timestamp = s.now()
	}
	var payload []byte
	if len(e.PayloadJSON) > 0 {
		payload = []byte(e.PayloadJSON)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO audit_logs (actor, entity_type, entity_id, case_id, action_id, event_type, payload_json, timestamp)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		e.Actor, e.EntityType, e.EntityID, nullString(e.CaseID), nullString(e.ActionID),
		e.EventType, payload, e.Timestamp)
	return err
}

// Audit is a convenience wrapper that marshals detail to the payload column.
func (s *Store) Audit(ctx context.Context, actor, entityType, entityID, caseID, eventType string, detail any) error {
	var payload json.RawMessage
	if detail != nil {
		if b, err := json.Marshal(detail); err == nil {
			payload = b
		}
	}
	return s.AppendAudit(ctx, domain.AuditLog{
		Actor:       actor,
		EntityType:  entityType,
		EntityID:    entityID,
		CaseID:      caseID,
		EventType:   eventType,
		PayloadJSON: payload,
	})
}

const auditCols = `id, actor, entity_type, entity_id, COALESCE(case_id,''), COALESCE(action_id,''),
	event_type, payload_json, timestamp`

func scanAudit(row rowScanner) (*domain.AuditLog, error) {
	var e domain.AuditLog
	var id int64
	var payload []byte
	if err := row.Scan(&id, &e.Actor, &e.EntityType, &e.EntityID, &e.CaseID, &e.ActionID,
		&e.EventType, &payload, &e.Timestamp); err != nil {
		return nil, err
	}
	e.ID = fmt.Sprintf("%d", id)
	if len(payload) > 0 {
		e.PayloadJSON = json.RawMessage(payload)
	}
	return &e, nil
}

// ListAuditForCase returns a case's audit trail, oldest first, which is the
// order the case timeline renders in.
func (s *Store) ListAuditForCase(ctx context.Context, caseID string) ([]domain.AuditLog, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+auditCols+
		` FROM audit_logs WHERE case_id = $1 ORDER BY timestamp, id`, caseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.AuditLog{}
	for rows.Next() {
		e, err := scanAudit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// ListAudit returns the global audit trail, newest first.
func (s *Store) ListAudit(ctx context.Context, limit, offset int) ([]domain.AuditLog, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.pool.Query(ctx, `SELECT `+auditCols+
		` FROM audit_logs ORDER BY timestamp DESC, id DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.AuditLog{}
	for rows.Next() {
		e, err := scanAudit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}
