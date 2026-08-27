package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// --- recovery actions ---

const actionCols = `id, case_id, COALESCE(decision_id,''), action_type, idempotency_key,
	external_id, external_url, amount, status, error_code, error_message, attempt_count,
	mode, environment, requested_at, executed_at, latency_ms`

func scanAction(row rowScanner) (*domain.RecoveryAction, error) {
	var a domain.RecoveryAction
	err := row.Scan(&a.ID, &a.CaseID, &a.DecisionID, &a.ActionType, &a.IdempotencyKey,
		&a.ExternalID, &a.ExternalURL, &a.Amount, &a.Status, &a.ErrorCode, &a.ErrorMessage,
		&a.AttemptCount, &a.Mode, &a.Environment, &a.RequestedAt, &a.ExecutedAt, &a.LatencyMS)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ReserveAction records the intent to act *before* any external call is made.
//
// This ordering is the whole point: the row (and therefore the idempotency key)
// exists in the database before a side effect can occur, so a crash between
// insert and call leaves a recoverable 'pending' row rather than an untracked
// payment link. If the key already exists, the existing row is returned with
// created=false and the caller must not perform the side effect again
// (SRS FR-043, 20.1, AC-006).
func (s *Store) ReserveAction(ctx context.Context, a *domain.RecoveryAction) (created bool, err error) {
	if a.IdempotencyKey == "" {
		return false, fmt.Errorf("%w: idempotency key is required", domain.ErrValidation)
	}
	if a.ID == "" {
		a.ID = NewID("act")
	}
	if a.RequestedAt.IsZero() {
		a.RequestedAt = s.now()
	}
	if a.Status == "" {
		a.Status = domain.ActionStatusPending
	}
	if a.Mode == "" {
		a.Mode = domain.ModeLiveTest
	}

	const q = `
		INSERT INTO recovery_actions (id, case_id, decision_id, action_type, idempotency_key,
		                              external_id, external_url, amount, status, error_code,
		                              error_message, attempt_count, mode, environment, requested_at,
		                              executed_at, latency_ms)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'test',$14,$15,$16)`
	_, execErr := s.pool.Exec(ctx, q, a.ID, a.CaseID, nullString(a.DecisionID), a.ActionType, a.IdempotencyKey,
		a.ExternalID, a.ExternalURL, a.Amount, a.Status, a.ErrorCode, a.ErrorMessage,
		a.AttemptCount, a.Mode, a.RequestedAt, a.ExecutedAt, a.LatencyMS)
	if execErr == nil {
		return true, nil
	}
	if !IsUniqueViolation(execErr) {
		return false, execErr
	}

	existing, getErr := s.GetActionByIdempotencyKey(ctx, a.IdempotencyKey)
	if getErr != nil {
		return false, fmt.Errorf("duplicate idempotency key %s but existing row unreadable: %w", a.IdempotencyKey, getErr)
	}
	*a = *existing
	return false, nil
}

// GetAction loads an action by id.
func (s *Store) GetAction(ctx context.Context, id string) (*domain.RecoveryAction, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+actionCols+` FROM recovery_actions WHERE id = $1`, id)
	a, err := scanAction(row)
	if err != nil {
		return nil, notFound(err, "action "+id)
	}
	return a, nil
}

// GetActionByIdempotencyKey loads an action by its idempotency key.
func (s *Store) GetActionByIdempotencyKey(ctx context.Context, key string) (*domain.RecoveryAction, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+actionCols+` FROM recovery_actions WHERE idempotency_key = $1`, key)
	a, err := scanAction(row)
	if err != nil {
		return nil, notFound(err, "action with key "+key)
	}
	return a, nil
}

// FindActionByExternalID resolves a Razorpay resource id back to the action that
// created it. This is how a payment webhook is attributed to a recovery attempt.
func (s *Store) FindActionByExternalID(ctx context.Context, externalID string) (*domain.RecoveryAction, error) {
	if externalID == "" {
		return nil, fmt.Errorf("%w: external id is required", domain.ErrValidation)
	}
	row := s.pool.QueryRow(ctx, `SELECT `+actionCols+
		` FROM recovery_actions WHERE external_id = $1 ORDER BY requested_at DESC LIMIT 1`, externalID)
	a, err := scanAction(row)
	if err != nil {
		return nil, notFound(err, "action for external id "+externalID)
	}
	return a, nil
}

// MarkActionExecuted records a successful external call.
func (s *Store) MarkActionExecuted(ctx context.Context, id, externalID, externalURL string, latencyMS int64) error {
	now := s.now()
	_, err := s.pool.Exec(ctx, `
		UPDATE recovery_actions
		SET status = 'executed', external_id = $2, external_url = $3, executed_at = $4,
			latency_ms = $5, attempt_count = attempt_count + 1, error_code = '', error_message = ''
		WHERE id = $1`, id, externalID, externalURL, now, latencyMS)
	return err
}

// MarkActionFailed records a definitive failure (a 4xx, or a retry budget that
// ran out). Definitive means: no external resource was created.
func (s *Store) MarkActionFailed(ctx context.Context, id, code, message string, latencyMS int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE recovery_actions
		SET status = 'failed', error_code = $2, error_message = $3, latency_ms = $4,
			attempt_count = attempt_count + 1
		WHERE id = $1`, id, code, truncate(message, 1000), latencyMS)
	return err
}

// MarkActionAmbiguous records a call whose outcome is unknown — a timeout or
// transport error where the request may have succeeded. Ambiguous actions are
// reconciled against the gateway before any further attempt (SRS 20.2).
func (s *Store) MarkActionAmbiguous(ctx context.Context, id, code, message string, latencyMS int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE recovery_actions
		SET status = 'ambiguous', error_code = $2, error_message = $3, latency_ms = $4,
			attempt_count = attempt_count + 1
		WHERE id = $1`, id, code, truncate(message, 1000), latencyMS)
	return err
}

// MarkActionSkipped records that the action was deliberately not performed —
// blocked by policy, or already satisfied.
func (s *Store) MarkActionSkipped(ctx context.Context, id, reason string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE recovery_actions SET status = 'skipped', error_message = $2 WHERE id = $1`,
		id, truncate(reason, 1000))
	return err
}

// ListActionsForCase returns the action history of a case, oldest first.
func (s *Store) ListActionsForCase(ctx context.Context, caseID string) ([]domain.RecoveryAction, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+actionCols+
		` FROM recovery_actions WHERE case_id = $1 ORDER BY requested_at, id`, caseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.RecoveryAction{}
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// ListActionsAwaitingVerification returns executed external actions that have no
// terminal outcome yet, oldest first. This drives the verification poller
// (SRS FR-049).
func (s *Store) ListActionsAwaitingVerification(ctx context.Context, olderThan time.Duration, limit int) ([]domain.RecoveryAction, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	cutoff := s.now().Add(-olderThan)
	rows, err := s.pool.Query(ctx, `
		SELECT `+actionCols+` FROM recovery_actions ra
		WHERE ra.status = 'executed'
		  AND ra.mode = 'live_test'
		  AND ra.action_type IN ('retry','payment_link','reminder')
		  AND ra.executed_at <= $1
		  AND NOT EXISTS (
			SELECT 1 FROM outcomes o
			WHERE o.action_id = ra.id AND o.outcome IN ('recovered','not_recovered','stopped')
		  )
		ORDER BY ra.executed_at
		LIMIT $2`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.RecoveryAction{}
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// ListUnresolvedActions returns actions whose external outcome is unknown, so the
// reconciler can query the gateway before anything else is attempted.
//
// Two statuses qualify, and the second is easy to overlook. An 'ambiguous' action
// is one whose call returned a timeout or 5xx. A stale 'pending' action is one that
// was reserved and never marked at all — the process died between the reserve and
// the response — which is exactly as unresolved, and rather more dangerous, because
// nothing else in the system will ever look at it again. staleAfter is how long a
// pending row is given to resolve itself before it is treated that way; a call in
// flight must not be reconciled underneath itself (SRS 20.2).
func (s *Store) ListUnresolvedActions(ctx context.Context, staleAfter time.Duration,
	limit int) ([]domain.RecoveryAction, error) {

	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if staleAfter <= 0 {
		staleAfter = 5 * time.Minute
	}
	cutoff := s.now().Add(-staleAfter)
	rows, err := s.pool.Query(ctx, `SELECT `+actionCols+
		` FROM recovery_actions
		  WHERE mode = 'live_test'
		    AND (status = 'ambiguous' OR (status = 'pending' AND requested_at <= $2))
		  ORDER BY requested_at LIMIT $1`, limit, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.RecoveryAction{}
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// --- policy facts ---

// PolicyFacts is the set of trusted counters the policy engine needs. Loading
// them in one round trip keeps the evaluation atomic enough that two workers
// cannot both see a stale count (SRS 10.2).
type PolicyFacts struct {
	RetryCount              int
	ReminderCount           int
	CaseActionCount         int
	ActionsForCustomerToday int
	LastActionAt            *time.Time
	ConsecutiveAPIFailures  int
	AlreadyPaid             bool
	HasHumanApproval        bool
}

// LoadPolicyFacts gathers every counter the policy engine reads for one case.
func (s *Store) LoadPolicyFacts(ctx context.Context, caseID, customerID, decisionID string) (PolicyFacts, error) {
	var f PolicyFacts
	now := s.now()
	dayAgo := now.Add(-24 * time.Hour)

	const q = `
		SELECT
			(SELECT count(*) FROM recovery_actions
			  WHERE case_id = $1 AND action_type = 'retry' AND status IN ('executed','ambiguous')),
			(SELECT count(*) FROM recovery_actions
			  WHERE case_id = $1 AND action_type = 'reminder' AND status IN ('executed','ambiguous')),
			(SELECT count(*) FROM recovery_actions
			  WHERE case_id = $1 AND action_type IN ('retry','payment_link','reminder')
			    AND status IN ('executed','ambiguous')),
			(SELECT count(*) FROM recovery_actions ra
			   JOIN risk_cases rc ON rc.id = ra.case_id
			  WHERE rc.customer_id = $2 AND ra.status IN ('executed','ambiguous')
			    AND ra.action_type IN ('retry','payment_link','reminder')
			    AND ra.requested_at >= $3),
			(SELECT max(COALESCE(ra.executed_at, ra.requested_at)) FROM recovery_actions ra
			   JOIN risk_cases rc ON rc.id = ra.case_id
			  WHERE rc.customer_id = $2 AND ra.status IN ('executed','ambiguous')
			    AND ra.action_type IN ('retry','payment_link','reminder')),
			(SELECT count(*) FROM recovery_actions
			  WHERE case_id = $1 AND status IN ('failed','ambiguous')),
			(SELECT EXISTS (SELECT 1 FROM outcomes
			  WHERE case_id = $1 AND outcome = 'recovered')),
			(SELECT EXISTS (SELECT 1 FROM approvals
			  WHERE decision_id = $4 AND decision = 'approved'))`
	err := s.pool.QueryRow(ctx, q, caseID, customerID, dayAgo, decisionID).Scan(
		&f.RetryCount, &f.ReminderCount, &f.CaseActionCount, &f.ActionsForCustomerToday,
		&f.LastActionAt, &f.ConsecutiveAPIFailures, &f.AlreadyPaid, &f.HasHumanApproval)
	if err != nil {
		return f, fmt.Errorf("load policy facts for case %s: %w", caseID, err)
	}
	return f, nil
}

// --- approvals ---

const approvalCols = `id, case_id, decision_id, reason, requested_at, reviewer, decision,
	decision_note, decided_at`

func scanApproval(row rowScanner) (*domain.Approval, error) {
	var a domain.Approval
	err := row.Scan(&a.ID, &a.CaseID, &a.DecisionID, &a.Reason, &a.RequestedAt,
		&a.Reviewer, &a.Decision, &a.DecisionNote, &a.DecidedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// RequestApproval opens a human-review request. It is idempotent: a second
// request for the same decision returns the pending row rather than queueing a
// duplicate (enforced by approvals_pending_decision_uidx).
func (s *Store) RequestApproval(ctx context.Context, a *domain.Approval) (created bool, err error) {
	if a.ID == "" {
		a.ID = NewID("appr")
	}
	if a.RequestedAt.IsZero() {
		a.RequestedAt = s.now()
	}
	a.Decision = domain.ApprovalPending

	const q = `
		INSERT INTO approvals (id, case_id, decision_id, reason, requested_at, reviewer, decision,
		                       decision_note, decided_at)
		VALUES ($1,$2,$3,$4,$5,'','pending','',NULL)`
	_, execErr := s.pool.Exec(ctx, q, a.ID, a.CaseID, a.DecisionID, truncate(a.Reason, 500), a.RequestedAt)
	if execErr == nil {
		return true, nil
	}
	if !IsUniqueViolation(execErr) {
		return false, execErr
	}
	row := s.pool.QueryRow(ctx, `SELECT `+approvalCols+
		` FROM approvals WHERE decision_id = $1 AND decision = 'pending'`, a.DecisionID)
	existing, getErr := scanApproval(row)
	if getErr != nil {
		return false, notFound(getErr, "pending approval for decision "+a.DecisionID)
	}
	*a = *existing
	return false, nil
}

// GetApproval loads one approval.
func (s *Store) GetApproval(ctx context.Context, id string) (*domain.Approval, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+approvalCols+` FROM approvals WHERE id = $1`, id)
	a, err := scanApproval(row)
	if err != nil {
		return nil, notFound(err, "approval "+id)
	}
	return a, nil
}

// ListApprovals returns the approval queue. An empty decision filter returns all.
func (s *Store) ListApprovals(ctx context.Context, decision domain.ApprovalDecision, limit int) ([]domain.Approval, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT ` + approvalCols + ` FROM approvals`
	args := []any{}
	if decision != "" {
		q += ` WHERE decision = $1`
		args = append(args, decision)
	}
	q += fmt.Sprintf(` ORDER BY requested_at DESC LIMIT $%d`, len(args)+1)
	args = append(args, limit)

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Approval{}
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// ListApprovalsForCase returns every approval record on a case.
func (s *Store) ListApprovalsForCase(ctx context.Context, caseID string) ([]domain.Approval, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+approvalCols+
		` FROM approvals WHERE case_id = $1 ORDER BY requested_at`, caseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Approval{}
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// ApprovalQueue returns pending review requests with their case and decision
// context, ordered the way SRS 16.3 requires: highest value and lowest confidence
// first, because those are the two reasons a human is in the loop at all.
//
// The context arrives in one join rather than a lookup per row. A reviewer screen
// that issued two extra queries per pending approval would be slowest exactly when
// the backlog is largest.
//
// already_executed is computed from recovery_actions rather than trusted from the
// case status: a decision whose action has already run must not be presented as
// something to authorise. Those rows are dropped when the caller asks to hide
// them, which the review queue does.
func (s *Store) ApprovalQueue(ctx context.Context, limit int, hideExecuted bool) ([]domain.ApprovalQueueItem, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	q := `
		SELECT a.id, a.case_id, a.decision_id, a.reason, a.requested_at, a.reviewer, a.decision,
		       a.decision_note, a.decided_at,
		       rc.reference, rc.source_type, rc.status, rc.revenue_at_risk, rc.expected_recovery,
		       rc.risk_score, rc.urgency,
		       cu.name, cu.segment,
		       d.recommended_action, d.recovery_probability, d.reason_codes,
		       EXISTS (
		           SELECT 1 FROM recovery_actions ra
		           WHERE ra.decision_id = a.decision_id AND ra.status IN ('executed','ambiguous')
		       ) AS already_executed
		FROM approvals a
		JOIN risk_cases     rc ON rc.id = a.case_id
		JOIN customers      cu ON cu.id = rc.customer_id
		JOIN agent_decisions d ON d.id  = a.decision_id
		WHERE a.decision = 'pending'`
	if hideExecuted {
		q += `
		  AND NOT EXISTS (
		      SELECT 1 FROM recovery_actions ra
		      WHERE ra.decision_id = a.decision_id AND ra.status IN ('executed','ambiguous')
		  )`
	}
	// Value first, then least-confident first at equal value: a large amount the
	// planner is unsure about is the row most in need of a person.
	q += `
		ORDER BY rc.revenue_at_risk DESC, d.recovery_probability ASC, a.requested_at ASC
		LIMIT $1`

	rows, err := s.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	now := s.now()
	out := []domain.ApprovalQueueItem{}
	for rows.Next() {
		var it domain.ApprovalQueueItem
		var reasonCodes []byte
		err := rows.Scan(&it.ID, &it.CaseID, &it.DecisionID, &it.Reason, &it.RequestedAt,
			&it.Reviewer, &it.Decision, &it.DecisionNote, &it.DecidedAt,
			&it.Reference, &it.SourceType, &it.CaseStatus, &it.RevenueAtRisk, &it.ExpectedRecovery,
			&it.RiskScore, &it.Urgency,
			&it.CustomerName, &it.CustomerSegment,
			&it.RecommendedAction, &it.Confidence, &reasonCodes,
			&it.AlreadyExecuted)
		if err != nil {
			return nil, err
		}
		it.ReasonCodes = scanStrings(reasonCodes)
		if mins := int(now.Sub(it.RequestedAt).Minutes()); mins > 0 {
			it.WaitingMinutes = mins
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// PendingApprovalCount counts every approval still awaiting a decision, ignoring the
// page limit.
//
// The review queue needs this separately from len(items): a screen that reported the
// returned row count as the backlog would say "50 pending" whether the real number
// were 50 or 500, which is the one number a reviewer deciding whether to keep working
// actually needs to be true. hideExecuted must match the value passed to
// ApprovalQueue, or the count will not describe the same set of rows.
func (s *Store) PendingApprovalCount(ctx context.Context, hideExecuted bool) (int, error) {
	q := `SELECT count(*) FROM approvals a WHERE a.decision = 'pending'`
	if hideExecuted {
		q += `
		  AND NOT EXISTS (
		      SELECT 1 FROM recovery_actions ra
		      WHERE ra.decision_id = a.decision_id AND ra.status IN ('executed','ambiguous')
		  )`
	}
	var n int
	if err := s.pool.QueryRow(ctx, q).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// DecideApproval records a reviewer's verdict. Only a pending approval can be
// decided, so a double submit from two reviewers cannot overwrite the first
// decision. A rejection requires a note (SRS FR-046).
func (s *Store) DecideApproval(ctx context.Context, id string, decision domain.ApprovalDecision,
	reviewer, note string) (*domain.Approval, error) {
	if decision != domain.ApprovalApproved && decision != domain.ApprovalRejected {
		return nil, fmt.Errorf("%w: decision must be approved or rejected", domain.ErrValidation)
	}
	if decision == domain.ApprovalRejected && note == "" {
		return nil, fmt.Errorf("%w: a rejection reason is required", domain.ErrValidation)
	}

	var out *domain.Approval
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT `+approvalCols+` FROM approvals WHERE id = $1 FOR UPDATE`, id)
		existing, err := scanApproval(row)
		if err != nil {
			return notFound(err, "approval "+id)
		}
		if existing.Decision != domain.ApprovalPending {
			return fmt.Errorf("%w: approval %s was already %s", domain.ErrValidation, id, existing.Decision)
		}
		now := s.now()
		if _, err := tx.Exec(ctx, `
			UPDATE approvals SET decision = $2, reviewer = $3, decision_note = $4, decided_at = $5
			WHERE id = $1 AND decision = 'pending'`,
			id, decision, reviewer, truncate(note, 1000), now); err != nil {
			return err
		}
		existing.Decision = decision
		existing.Reviewer = reviewer
		existing.DecisionNote = note
		existing.DecidedAt = &now
		out = existing
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// --- outcomes ---

const outcomeCols = `id, case_id, COALESCE(action_id,''), outcome, recovered_amount, recovered_at,
	time_to_recovery_seconds, verification_source, notes, created_at`

func scanOutcome(row rowScanner) (*domain.Outcome, error) {
	var o domain.Outcome
	err := row.Scan(&o.ID, &o.CaseID, &o.ActionID, &o.Outcome, &o.RecoveredAmount, &o.RecoveredAt,
		&o.TimeToRecoverySeconds, &o.VerificationSource, &o.Notes, &o.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// RecordOutcome persists a verified result. A second 'recovered' outcome for the
// same action collides with outcomes_recovered_action_uidx and is reported as a
// duplicate rather than banked twice — the second line of defence against
// double-counting recovered revenue (SRS AC-006).
func (s *Store) RecordOutcome(ctx context.Context, o *domain.Outcome) (created bool, err error) {
	if o.ID == "" {
		o.ID = NewID("out")
	}
	if o.CreatedAt.IsZero() {
		o.CreatedAt = s.now()
	}
	const q = `
		INSERT INTO outcomes (id, case_id, action_id, outcome, recovered_amount, recovered_at,
		                      time_to_recovery_seconds, verification_source, notes, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`
	_, execErr := s.pool.Exec(ctx, q, o.ID, o.CaseID, nullString(o.ActionID), o.Outcome,
		o.RecoveredAmount, o.RecoveredAt, o.TimeToRecoverySeconds, o.VerificationSource,
		truncate(o.Notes, 1000), o.CreatedAt)
	if execErr == nil {
		return true, nil
	}
	if IsUniqueViolation(execErr) {
		return false, nil
	}
	return false, execErr
}

// ListOutcomesForCase returns every recorded outcome for a case.
func (s *Store) ListOutcomesForCase(ctx context.Context, caseID string) ([]domain.Outcome, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+outcomeCols+
		` FROM outcomes WHERE case_id = $1 ORDER BY created_at, id`, caseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Outcome{}
	for rows.Next() {
		o, err := scanOutcome(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *o)
	}
	return out, rows.Err()
}

// SettleRecovery is the single atomic write that banks a recovery: it records
// the outcome, updates the case, and updates the strategy metric together. If
// the outcome is a duplicate, nothing else is applied — that is what keeps the
// recovered total exact under webhook redelivery (SRS AC-006).
func (s *Store) SettleRecovery(ctx context.Context, o *domain.Outcome, segment domain.Segment,
	sourceType domain.SourceType, actionType domain.ActionType) (banked bool, err error) {
	if o.ID == "" {
		o.ID = NewID("out")
	}
	if o.CreatedAt.IsZero() {
		o.CreatedAt = s.now()
	}
	o.Outcome = domain.OutcomeRecovered

	err = s.InTx(ctx, func(tx pgx.Tx) error {
		_, insErr := tx.Exec(ctx, `
			INSERT INTO outcomes (id, case_id, action_id, outcome, recovered_amount, recovered_at,
			                      time_to_recovery_seconds, verification_source, notes, created_at)
			VALUES ($1,$2,$3,'recovered',$4,$5,$6,$7,$8,$9)`,
			o.ID, o.CaseID, nullString(o.ActionID), o.RecoveredAmount, o.RecoveredAt,
			o.TimeToRecoverySeconds, o.VerificationSource, truncate(o.Notes, 1000), o.CreatedAt)
		if insErr != nil {
			if IsUniqueViolation(insErr) {
				// Already banked. Leave every total untouched.
				return nil
			}
			return insErr
		}
		banked = true

		if _, err := tx.Exec(ctx, `
			UPDATE risk_cases
			SET recovered_amount = $2, status = 'RECOVERED', updated_at = $3,
				closed_at = COALESCE(closed_at, $3)
			WHERE id = $1 AND status <> 'RECOVERED'`, o.CaseID, o.RecoveredAmount, o.CreatedAt); err != nil {
			return err
		}

		if actionType == "" {
			return nil
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO strategy_metrics (id, segment, source_type, action_type, attempts, successes,
			                              recovered_amount, updated_at)
			VALUES ($1,$2,$3,$4,1,1,$5,$6)
			ON CONFLICT (segment, source_type, action_type) DO UPDATE SET
				successes = strategy_metrics.successes + 1,
				recovered_amount = strategy_metrics.recovered_amount + EXCLUDED.recovered_amount,
				updated_at = EXCLUDED.updated_at`,
			NewID("sm"), segment, sourceType, actionType, o.RecoveredAmount, o.CreatedAt)
		return err
	})
	if err != nil {
		return false, err
	}
	return banked, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
