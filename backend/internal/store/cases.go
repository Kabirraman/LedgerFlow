package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ledgerflow/ledgerflow/internal/domain"
	"github.com/ledgerflow/ledgerflow/internal/idem"
)

// --- risk cases ---

// caseCols lists the case columns in the order scanCase expects. qualifiedCaseCols
// produces the same list prefixed with a table alias for joined queries.
func caseCols(alias string) string {
	p := ""
	if alias != "" {
		p = alias + "."
	}
	cols := []string{p + "id", p + "reference", p + "source_type", p + "customer_id",
		"COALESCE(" + p + "transaction_id,'')", "COALESCE(" + p + "checkout_session_id,'')",
		"COALESCE(" + p + "invoice_id,'')", "COALESCE(" + p + "subscription_id,'')",
		p + "revenue_at_risk", p + "risk_score", p + "urgency", p + "expected_recovery",
		p + "recovered_amount", p + "status", p + "reason_codes", p + "evidence_refs",
		"COALESCE(" + p + "stop_reason,'')", p + "action_count", p + "mode",
		"COALESCE(" + p + "simulation_id,'')", p + "environment",
		p + "created_at", p + "updated_at", p + "closed_at"}
	return strings.Join(cols, ", ")
}

func scanCase(row rowScanner) (*domain.RiskCase, error) {
	var c domain.RiskCase
	var reasonCodes, evidenceRefs []byte
	err := row.Scan(&c.ID, &c.Reference, &c.SourceType, &c.CustomerID,
		&c.TransactionID, &c.CheckoutSessionID, &c.InvoiceID, &c.SubscriptionID,
		&c.RevenueAtRisk, &c.RiskScore, &c.Urgency, &c.ExpectedRecovery, &c.RecoveredAmount, &c.Status,
		&reasonCodes, &evidenceRefs, &c.StopReason, &c.ActionCount, &c.Mode, &c.SimulationID,
		&c.Environment, &c.CreatedAt, &c.UpdatedAt, &c.ClosedAt)
	if err != nil {
		return nil, err
	}
	c.ReasonCodes = scanStrings(reasonCodes)
	c.EvidenceRefs = scanStrings(evidenceRefs)
	return &c, nil
}

// CreateCase inserts a risk case and assigns its REV-#### reference.
//
// The partial unique indexes on the source pointers make this safe against a
// duplicate webhook: a second attempt to open a case for the same source record
// returns ErrDuplicateEvent rather than creating a rival case (SRS FR-003).
func (s *Store) CreateCase(ctx context.Context, c *domain.RiskCase) error {
	if c.ID == "" {
		c.ID = NewID("case")
	}
	now := s.now()
	c.CreatedAt, c.UpdatedAt = now, now
	if c.Status == "" {
		c.Status = domain.StatusNew
	}
	if c.Mode == "" {
		c.Mode = domain.ModeLiveTest
	}
	if c.Urgency == "" {
		c.Urgency = domain.UrgencyLow
	}

	if c.Reference == "" {
		var seq int64
		if err := s.pool.QueryRow(ctx, `SELECT nextval('risk_case_seq')`).Scan(&seq); err != nil {
			return fmt.Errorf("allocate case reference: %w", err)
		}
		c.Reference = idem.ReferenceForCase(seq)
	}

	const q = `
		INSERT INTO risk_cases (id, reference, source_type, customer_id, transaction_id, checkout_session_id,
		                        invoice_id, subscription_id, revenue_at_risk, risk_score, urgency,
		                        expected_recovery, recovered_amount, status, reason_codes, evidence_refs,
		                        stop_reason, action_count, mode, simulation_id, environment, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,'test',$21,$22)`
	_, err := s.pool.Exec(ctx, q, c.ID, c.Reference, c.SourceType, c.CustomerID,
		nullString(c.TransactionID), nullString(c.CheckoutSessionID), nullString(c.InvoiceID), nullString(c.SubscriptionID),
		c.RevenueAtRisk, c.RiskScore, c.Urgency, c.ExpectedRecovery, c.RecoveredAmount, c.Status,
		jsonStrings(c.ReasonCodes), jsonStrings(c.EvidenceRefs), c.StopReason, c.ActionCount,
		c.Mode, nullString(c.SimulationID), c.CreatedAt, c.UpdatedAt)
	if err != nil {
		if IsUniqueViolation(err) {
			return fmt.Errorf("%w: a case already exists for this source record (%s)",
				domain.ErrDuplicateEvent, ConstraintName(err))
		}
		return err
	}
	return nil
}

// GetCase loads one case by id or by its REV-#### reference.
func (s *Store) GetCase(ctx context.Context, id string) (*domain.RiskCase, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+caseCols("")+` FROM risk_cases WHERE id = $1 OR reference = $1`, id)
	c, err := scanCase(row)
	if err != nil {
		return nil, notFound(err, "case "+id)
	}
	return c, nil
}

// FindOpenCaseBySource resolves an existing live-mode case for a source record,
// so ingestion can attach to it instead of creating a duplicate.
func (s *Store) FindOpenCaseBySource(ctx context.Context, sourceType domain.SourceType, sourceID string) (*domain.RiskCase, error) {
	col, err := sourceColumn(sourceType)
	if err != nil {
		return nil, err
	}
	q := fmt.Sprintf(`SELECT %s FROM risk_cases WHERE %s = $1 AND mode = 'live_test'
		ORDER BY created_at DESC LIMIT 1`, caseCols(""), col)
	row := s.pool.QueryRow(ctx, q, sourceID)
	c, scanErr := scanCase(row)
	if scanErr != nil {
		return nil, notFound(scanErr, "case for source record "+sourceID)
	}
	return c, nil
}

// FindOpenCaseByPaymentOrder resolves a still-open case from the Razorpay order
// a payment settled against.
//
// This is the weaker of the two attribution paths the verifier uses. It is exact
// correlation rather than a guess — the case was opened for a failed payment on
// this order, and money later arrived on the same order — but it does not name a
// recovery action, so a payment matched this way is recorded as organic rather
// than credited to a strategy (SRS 16.2, 25.2).
func (s *Store) FindOpenCaseByPaymentOrder(ctx context.Context, orderID string) (*domain.RiskCase, error) {
	if orderID == "" {
		return nil, fmt.Errorf("%w: order id is required", domain.ErrValidation)
	}
	q := `SELECT ` + caseCols("rc") + `
		FROM risk_cases rc
		JOIN transactions t ON t.id = rc.transaction_id
		WHERE t.razorpay_order_id = $1
		  AND rc.mode = 'live_test'
		  AND rc.status NOT IN ('RECOVERED','REJECTED','BLOCKED','CLOSED')
		ORDER BY rc.created_at DESC LIMIT 1`
	row := s.pool.QueryRow(ctx, q, orderID)
	c, err := scanCase(row)
	if err != nil {
		return nil, notFound(err, "open case for order "+orderID)
	}
	return c, nil
}

func sourceColumn(st domain.SourceType) (string, error) {
	switch st {
	case domain.SourcePaymentFailure:
		return "transaction_id", nil
	case domain.SourceCheckoutAbandonment:
		return "checkout_session_id", nil
	case domain.SourceInvoiceOverdue:
		return "invoice_id", nil
	case domain.SourceSubscriptionFailure:
		return "subscription_id", nil
	}
	return "", fmt.Errorf("%w: unknown source type %q", domain.ErrValidation, st)
}

// UpdateCaseStatus transitions a case, validating the move against the state
// machine while holding a row lock, so two concurrent workers cannot both
// advance the same case (SRS 14.2).
func (s *Store) UpdateCaseStatus(ctx context.Context, caseID string, to domain.CaseStatus, stopReason string) error {
	return s.InTx(ctx, func(tx pgx.Tx) error {
		var from domain.CaseStatus
		if err := tx.QueryRow(ctx, `SELECT status FROM risk_cases WHERE id = $1 FOR UPDATE`, caseID).Scan(&from); err != nil {
			return notFound(err, "case "+caseID)
		}
		if err := domain.ValidateTransition(from, to); err != nil {
			return err
		}
		closedAt := "closed_at"
		if to.IsTerminal() {
			closedAt = "COALESCE(closed_at, $4)"
		}
		q := fmt.Sprintf(`UPDATE risk_cases SET status = $2,
			stop_reason = COALESCE(NULLIF($3,''), stop_reason),
			updated_at = $4, closed_at = %s WHERE id = $1`, closedAt)
		_, err := tx.Exec(ctx, q, caseID, to, stopReason, s.now())
		return err
	})
}

// UpdateCaseAssessment stores refreshed detection output.
func (s *Store) UpdateCaseAssessment(ctx context.Context, caseID string, revenueAtRisk domain.Money,
	riskScore float64, urgency domain.Urgency, reasonCodes, evidenceRefs []string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE risk_cases SET revenue_at_risk = $2, risk_score = $3, urgency = $4,
			reason_codes = $5, evidence_refs = $6, updated_at = $7 WHERE id = $1`,
		caseID, revenueAtRisk, riskScore, urgency, jsonStrings(reasonCodes), jsonStrings(evidenceRefs), s.now())
	return err
}

// UpdateCaseExpectedRecovery stores the ERR value from SRS 9.2.
func (s *Store) UpdateCaseExpectedRecovery(ctx context.Context, caseID string, expected domain.Money) error {
	_, err := s.pool.Exec(ctx, `UPDATE risk_cases SET expected_recovery = $2, updated_at = $3 WHERE id = $1`,
		caseID, expected, s.now())
	return err
}

// RecordCaseRecovery marks money recovered. The status guard makes the write
// idempotent: a redelivered payment webhook cannot bank the amount twice
// (SRS AC-006).
func (s *Store) RecordCaseRecovery(ctx context.Context, caseID string, amount domain.Money) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE risk_cases
		SET recovered_amount = $2, status = 'RECOVERED', updated_at = $3, closed_at = COALESCE(closed_at, $3)
		WHERE id = $1 AND status <> 'RECOVERED'`, caseID, amount, s.now())
	return err
}

// IncrementCaseActionCount bumps the per-case action budget counter.
func (s *Store) IncrementCaseActionCount(ctx context.Context, caseID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE risk_cases SET action_count = action_count + 1, updated_at = $2 WHERE id = $1`,
		caseID, s.now())
	return err
}

// ClaimCasesForStage locks and returns cases sitting in a given status, so a
// background worker can advance them without racing another replica.
func (s *Store) ClaimCasesForStage(ctx context.Context, status domain.CaseStatus, limit int) ([]domain.RiskCase, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	q := `SELECT ` + caseCols("") + ` FROM risk_cases
		WHERE status = $1 AND mode = 'live_test'
		ORDER BY expected_recovery DESC, risk_score DESC, created_at
		LIMIT $2 FOR UPDATE SKIP LOCKED`
	var out []domain.RiskCase
	err := s.InTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, status, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			c, err := scanCase(rows)
			if err != nil {
				return err
			}
			out = append(out, *c)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListCases returns a filtered, prioritised page of cases (SRS FR-013, 16.1).
func (s *Store) ListCases(ctx context.Context, f domain.CaseFilter) (*domain.CasePage, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 25
	}
	if f.Offset < 0 {
		f.Offset = 0
	}

	where := []string{"TRUE"}
	args := []any{}
	add := func(clause string, val any) {
		args = append(args, val)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}
	if f.SourceType != "" {
		add("rc.source_type = $%d", f.SourceType)
	}
	if f.Status != "" {
		add("rc.status = $%d", f.Status)
	}
	if f.Segment != "" {
		add("cu.segment = $%d", f.Segment)
	}
	if f.MinRisk > 0 {
		add("rc.risk_score >= $%d", f.MinRisk)
	}
	if f.Mode != "" {
		add("rc.mode = $%d", f.Mode)
	} else {
		// Default view hides simulation cases so benchmark rows never inflate the
		// operator's live queue (SRS 25.2).
		where = append(where, "rc.mode <> 'simulation'")
	}
	if f.ActionType != "" {
		add("d.recommended_action = $%d", f.ActionType)
	}
	if f.Search != "" {
		args = append(args, "%"+strings.ToLower(f.Search)+"%")
		n := len(args)
		where = append(where, fmt.Sprintf(
			"(lower(rc.reference) LIKE $%d OR lower(cu.name) LIKE $%d OR lower(COALESCE(cu.email,'')) LIKE $%d)", n, n, n))
	}

	orderBy := "rc.expected_recovery DESC, rc.risk_score DESC, rc.created_at DESC"
	switch f.SortBy {
	case "risk_score":
		orderBy = "rc.risk_score DESC, rc.expected_recovery DESC"
	case "created_at":
		orderBy = "rc.created_at DESC"
	case "revenue_at_risk":
		orderBy = "rc.revenue_at_risk DESC"
	case "urgency":
		orderBy = `CASE rc.urgency WHEN 'critical' THEN 4 WHEN 'high' THEN 3 WHEN 'medium' THEN 2 ELSE 1 END DESC,
			rc.expected_recovery DESC`
	}

	// Latest diagnosis, decision and worst policy verdict per case, joined
	// laterally so a queue page costs one query rather than 3N.
	base := `
		FROM risk_cases rc
		JOIN customers cu ON cu.id = rc.customer_id
		LEFT JOIN LATERAL (
			SELECT root_cause, confidence FROM diagnoses
			WHERE case_id = rc.id ORDER BY created_at DESC LIMIT 1
		) dg ON TRUE
		LEFT JOIN LATERAL (
			SELECT id, recommended_action FROM agent_decisions
			WHERE case_id = rc.id ORDER BY created_at DESC LIMIT 1
		) d ON TRUE
		LEFT JOIN LATERAL (
			SELECT result FROM policy_checks WHERE decision_id = d.id
			ORDER BY CASE result WHEN 'BLOCK' THEN 3 WHEN 'ESCALATE' THEN 2 ELSE 1 END DESC LIMIT 1
		) pc ON TRUE
		WHERE ` + strings.Join(where, " AND ")

	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) `+base, args...).Scan(&total); err != nil {
		return nil, fmt.Errorf("count cases: %w", err)
	}

	args = append(args, f.Limit, f.Offset)
	q := `SELECT ` + caseCols("rc") + `,
			cu.name, cu.segment, COALESCE(dg.root_cause,''), COALESCE(dg.confidence,0),
			COALESCE(d.recommended_action,''), COALESCE(pc.result,'')
		` + base + ` ORDER BY ` + orderBy +
		fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)-1, len(args))

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list cases: %w", err)
	}
	defer rows.Close()

	page := &domain.CasePage{Items: []domain.CaseListItem{}, Total: total, Limit: f.Limit, Offset: f.Offset}
	for rows.Next() {
		var item domain.CaseListItem
		var reasonCodes, evidenceRefs []byte
		err := rows.Scan(&item.ID, &item.Reference, &item.SourceType, &item.CustomerID,
			&item.TransactionID, &item.CheckoutSessionID, &item.InvoiceID, &item.SubscriptionID,
			&item.RevenueAtRisk, &item.RiskScore, &item.Urgency, &item.ExpectedRecovery, &item.RecoveredAmount,
			&item.Status, &reasonCodes, &evidenceRefs, &item.StopReason, &item.ActionCount, &item.Mode,
			&item.SimulationID, &item.Environment, &item.CreatedAt, &item.UpdatedAt, &item.ClosedAt,
			&item.CustomerName, &item.CustomerSegment, &item.RootCause, &item.Confidence,
			&item.RecommendedAction, &item.PolicyResult)
		if err != nil {
			return nil, fmt.Errorf("scan case row: %w", err)
		}
		item.ReasonCodes = scanStrings(reasonCodes)
		item.EvidenceRefs = scanStrings(evidenceRefs)
		page.Items = append(page.Items, item)
	}
	return page, rows.Err()
}

// --- diagnoses ---

// SaveDiagnosis appends a diagnosis. Diagnoses are never updated in place, so a
// re-analysis leaves the earlier reasoning intact for audit (SRS FR-052).
func (s *Store) SaveDiagnosis(ctx context.Context, d *domain.Diagnosis) error {
	if d.ID == "" {
		d.ID = NewID("diag")
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = s.now()
	}
	if !d.RootCause.Valid() {
		d.RootCause = domain.RootCauseUnknown
	}
	const q = `
		INSERT INTO diagnoses (id, case_id, root_cause, confidence, evidence_json, uncertainty_flags,
		                       next_step, source, model_name, latency_ms, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`
	_, err := s.pool.Exec(ctx, q, d.ID, d.CaseID, d.RootCause, d.Confidence, jsonStrings(d.Evidence),
		jsonStrings(d.UncertaintyFlags), d.NextStep, d.Source, d.ModelName, d.LatencyMS, d.CreatedAt)
	return err
}

const diagnosisCols = `id, case_id, root_cause, confidence, evidence_json, uncertainty_flags,
	COALESCE(next_step,''), source, COALESCE(model_name,''), latency_ms, created_at`

func scanDiagnosis(row rowScanner) (*domain.Diagnosis, error) {
	var d domain.Diagnosis
	var evidence, flags []byte
	err := row.Scan(&d.ID, &d.CaseID, &d.RootCause, &d.Confidence, &evidence, &flags,
		&d.NextStep, &d.Source, &d.ModelName, &d.LatencyMS, &d.CreatedAt)
	if err != nil {
		return nil, err
	}
	d.Evidence = scanStrings(evidence)
	d.UncertaintyFlags = scanStrings(flags)
	return &d, nil
}

// LatestDiagnosis returns the most recent diagnosis for a case.
func (s *Store) LatestDiagnosis(ctx context.Context, caseID string) (*domain.Diagnosis, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+diagnosisCols+
		` FROM diagnoses WHERE case_id = $1 ORDER BY created_at DESC LIMIT 1`, caseID)
	d, err := scanDiagnosis(row)
	if err != nil {
		return nil, notFound(err, "diagnosis for case "+caseID)
	}
	return d, nil
}

// --- agent decisions ---

// SaveDecision appends a planner decision. An action outside the allow-list is
// rejected here as well as in the policy engine: defence in depth around the
// one field that can move money (SRS 19.2).
func (s *Store) SaveDecision(ctx context.Context, d *domain.AgentDecision) error {
	if d.ID == "" {
		d.ID = NewID("dec")
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = s.now()
	}
	if !d.RecommendedAction.Valid() {
		return fmt.Errorf("%w: %q", domain.ErrActionNotAllowed, d.RecommendedAction)
	}
	const q = `
		INSERT INTO agent_decisions (id, case_id, recommended_action, recovery_probability, expected_recovery,
		                             reason_codes, alternatives, stop_condition, policy_version, source,
		                             model_name, latency_ms, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`
	_, err := s.pool.Exec(ctx, q, d.ID, d.CaseID, d.RecommendedAction, d.RecoveryProbability, d.ExpectedRecovery,
		jsonStrings(d.ReasonCodes), jsonStrings(d.Alternatives), d.StopCondition, d.PolicyVersion,
		d.Source, d.ModelName, d.LatencyMS, d.CreatedAt)
	return err
}

const decisionCols = `id, case_id, recommended_action, recovery_probability, expected_recovery,
	reason_codes, alternatives, COALESCE(stop_condition,''), policy_version, source,
	COALESCE(model_name,''), latency_ms, created_at`

func scanDecision(row rowScanner) (*domain.AgentDecision, error) {
	var d domain.AgentDecision
	var reasons, alts []byte
	err := row.Scan(&d.ID, &d.CaseID, &d.RecommendedAction, &d.RecoveryProbability, &d.ExpectedRecovery,
		&reasons, &alts, &d.StopCondition, &d.PolicyVersion, &d.Source, &d.ModelName, &d.LatencyMS, &d.CreatedAt)
	if err != nil {
		return nil, err
	}
	d.ReasonCodes = scanStrings(reasons)
	d.Alternatives = scanStrings(alts)
	return &d, nil
}

// LatestDecision returns the most recent planner decision for a case.
func (s *Store) LatestDecision(ctx context.Context, caseID string) (*domain.AgentDecision, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+decisionCols+
		` FROM agent_decisions WHERE case_id = $1 ORDER BY created_at DESC LIMIT 1`, caseID)
	d, err := scanDecision(row)
	if err != nil {
		return nil, notFound(err, "decision for case "+caseID)
	}
	return d, nil
}

// GetDecision loads a decision by id.
func (s *Store) GetDecision(ctx context.Context, id string) (*domain.AgentDecision, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+decisionCols+` FROM agent_decisions WHERE id = $1`, id)
	d, err := scanDecision(row)
	if err != nil {
		return nil, notFound(err, "decision "+id)
	}
	return d, nil
}

// --- policy checks ---

// SavePolicyChecks appends every evaluated rule in one transaction, so the
// recorded control set is either complete or absent — never partial.
func (s *Store) SavePolicyChecks(ctx context.Context, checks []domain.PolicyCheck) error {
	if len(checks) == 0 {
		return nil
	}
	return s.InTx(ctx, func(tx pgx.Tx) error {
		const q = `
			INSERT INTO policy_checks (id, decision_id, case_id, policy_version, rule, result, details, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`
		for i := range checks {
			c := &checks[i]
			if c.ID == "" {
				c.ID = NewID("pchk")
			}
			if c.CreatedAt.IsZero() {
				c.CreatedAt = s.now()
			}
			if _, err := tx.Exec(ctx, q, c.ID, c.DecisionID, c.CaseID, c.PolicyVersion,
				c.Rule, c.Result, c.Details, c.CreatedAt); err != nil {
				return err
			}
		}
		return nil
	})
}

// ListPolicyChecks returns every check recorded for a case, oldest first.
func (s *Store) ListPolicyChecks(ctx context.Context, caseID string) ([]domain.PolicyCheck, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, decision_id, case_id, policy_version, rule, result, COALESCE(details,''), created_at
		FROM policy_checks WHERE case_id = $1 ORDER BY created_at, id`, caseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.PolicyCheck{}
	for rows.Next() {
		var c domain.PolicyCheck
		if err := rows.Scan(&c.ID, &c.DecisionID, &c.CaseID, &c.PolicyVersion, &c.Rule,
			&c.Result, &c.Details, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CountPolicyViolations counts executed external actions that lack a PASS
// verdict for their decision. This must always be zero (SRS 3.2, AC-003); it is
// surfaced on the dashboard so a regression is visible rather than silent.
func (s *Store) CountPolicyViolations(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM recovery_actions ra
		WHERE ra.status = 'executed'
		  AND ra.action_type IN ('retry','payment_link','reminder')
		  AND NOT EXISTS (
			SELECT 1 FROM policy_checks pc
			WHERE pc.decision_id = ra.decision_id AND pc.result = 'PASS'
		  )`).Scan(&n)
	return n, err
}
