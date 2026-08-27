package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// --- users (SRS 19.1) ---

// CreateUser inserts a user. The caller supplies an already-hashed password;
// this layer never sees a plaintext credential.
func (s *Store) CreateUser(ctx context.Context, u *domain.User) error {
	if u.ID == "" {
		u.ID = NewID("usr")
	}
	if u.CreatedAt.IsZero() {
		u.CreatedAt = s.now()
	}
	u.Email = strings.ToLower(strings.TrimSpace(u.Email))
	if u.Email == "" || u.PasswordHash == "" {
		return fmt.Errorf("%w: email and password hash are required", domain.ErrValidation)
	}
	if !u.Role.Valid() {
		return fmt.Errorf("%w: unknown role %q", domain.ErrValidation, u.Role)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO users (id, email, name, role, password_hash, created_at)
		VALUES ($1,$2,$3,$4,$5,$6)`, u.ID, u.Email, u.Name, u.Role, u.PasswordHash, u.CreatedAt)
	if IsUniqueViolation(err) {
		return fmt.Errorf("%w: a user with email %s already exists", domain.ErrValidation, u.Email)
	}
	return err
}

// EnsureUser creates a user if the email is not already present, and reports
// whether it inserted. Used by the first-boot seeder so restarting the stack
// never overwrites a password an operator has changed (SRS 23.3).
func (s *Store) EnsureUser(ctx context.Context, u *domain.User) (created bool, err error) {
	existing, err := s.FindUserByEmail(ctx, u.Email)
	if err == nil {
		*u = *existing
		return false, nil
	}
	if !isNotFound(err) {
		return false, err
	}
	if err := s.CreateUser(ctx, u); err != nil {
		return false, err
	}
	return true, nil
}

const userCols = `id, email, name, role, password_hash, created_at`

func scanUser(row rowScanner) (*domain.User, error) {
	var u domain.User
	if err := row.Scan(&u.ID, &u.Email, &u.Name, &u.Role, &u.PasswordHash, &u.CreatedAt); err != nil {
		return nil, err
	}
	return &u, nil
}

// FindUserByEmail resolves a login identity.
func (s *Store) FindUserByEmail(ctx context.Context, email string) (*domain.User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	row := s.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE email = $1`, email)
	u, err := scanUser(row)
	if err != nil {
		return nil, notFound(err, "user "+email)
	}
	return u, nil
}

// GetUser loads a user by id.
func (s *Store) GetUser(ctx context.Context, id string) (*domain.User, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id = $1`, id)
	u, err := scanUser(row)
	if err != nil {
		return nil, notFound(err, "user "+id)
	}
	return u, nil
}

// ListUsers returns every user for the admin screen.
func (s *Store) ListUsers(ctx context.Context) ([]domain.User, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+userCols+` FROM users ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// --- policies (SRS 10.1, FR-041) ---

const policyCols = `version, max_retry_count, max_automated_amount, min_action_confidence,
	cooldown_minutes, max_actions_per_customer_per_day, require_human_approval_above,
	max_reminders_per_case, max_actions_per_case, updated_at, updated_by`

func scanPolicy(row rowScanner) (*domain.Policy, error) {
	var p domain.Policy
	err := row.Scan(&p.Version, &p.MaxRetryCount, &p.MaxAutomatedAmount, &p.MinActionConfidence,
		&p.CooldownMinutes, &p.MaxActionsPerCustomerPerDay, &p.RequireHumanApprovalAbove,
		&p.MaxRemindersPerCase, &p.MaxActionsPerCase, &p.UpdatedAt, &p.UpdatedBy)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ActivePolicy returns the single active policy version.
//
// A missing active policy is not treated as "no limits": ErrNotFound is
// returned and the caller falls back to domain.DefaultPolicy, so the system
// fails closed onto the conservative baseline (SRS 20.4).
func (s *Store) ActivePolicy(ctx context.Context) (*domain.Policy, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+policyCols+` FROM policies WHERE is_active LIMIT 1`)
	p, err := scanPolicy(row)
	if err != nil {
		return nil, notFound(err, "active policy")
	}
	return p, nil
}

// ActivePolicyOrDefault never fails on a missing policy row.
func (s *Store) ActivePolicyOrDefault(ctx context.Context) domain.Policy {
	if p, err := s.ActivePolicy(ctx); err == nil {
		return *p
	}
	return domain.DefaultPolicy()
}

// GetPolicy loads a specific policy version.
func (s *Store) GetPolicy(ctx context.Context, version string) (*domain.Policy, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+policyCols+` FROM policies WHERE version = $1`, version)
	p, err := scanPolicy(row)
	if err != nil {
		return nil, notFound(err, "policy "+version)
	}
	return p, nil
}

// ListPolicies returns every stored policy version, newest first. Old versions
// are retained because decisions reference the version that authorised them
// (SRS FR-042).
func (s *Store) ListPolicies(ctx context.Context) ([]domain.Policy, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+policyCols+` FROM policies ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Policy{}
	for rows.Next() {
		p, err := scanPolicy(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// SavePolicy upserts a policy version and optionally activates it.
//
// Activation clears the previous active flag inside the same transaction: the
// partial unique index would otherwise reject the write, and more importantly
// two simultaneously-active control sets would make "which limits applied?"
// unanswerable.
func (s *Store) SavePolicy(ctx context.Context, p *domain.Policy, activate bool) error {
	if err := validatePolicy(p); err != nil {
		return err
	}
	p.UpdatedAt = s.now()

	return s.InTx(ctx, func(tx pgx.Tx) error {
		if activate {
			if _, err := tx.Exec(ctx, `UPDATE policies SET is_active = FALSE WHERE is_active`); err != nil {
				return err
			}
		}
		const q = `
			INSERT INTO policies (version, max_retry_count, max_automated_amount, min_action_confidence,
			                      cooldown_minutes, max_actions_per_customer_per_day, require_human_approval_above,
			                      max_reminders_per_case, max_actions_per_case, is_active, updated_at, updated_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
			ON CONFLICT (version) DO UPDATE SET
				max_retry_count = EXCLUDED.max_retry_count,
				max_automated_amount = EXCLUDED.max_automated_amount,
				min_action_confidence = EXCLUDED.min_action_confidence,
				cooldown_minutes = EXCLUDED.cooldown_minutes,
				max_actions_per_customer_per_day = EXCLUDED.max_actions_per_customer_per_day,
				require_human_approval_above = EXCLUDED.require_human_approval_above,
				max_reminders_per_case = EXCLUDED.max_reminders_per_case,
				max_actions_per_case = EXCLUDED.max_actions_per_case,
				is_active = policies.is_active OR EXCLUDED.is_active,
				updated_at = EXCLUDED.updated_at,
				updated_by = EXCLUDED.updated_by`
		_, err := tx.Exec(ctx, q, p.Version, p.MaxRetryCount, p.MaxAutomatedAmount, p.MinActionConfidence,
			p.CooldownMinutes, p.MaxActionsPerCustomerPerDay, p.RequireHumanApprovalAbove,
			p.MaxRemindersPerCase, p.MaxActionsPerCase, activate, p.UpdatedAt, p.UpdatedBy)
		return err
	})
}

// EnsureDefaultPolicy seeds the SRS 10.1 baseline on first boot without
// disturbing an existing active policy.
func (s *Store) EnsureDefaultPolicy(ctx context.Context) (domain.Policy, error) {
	if p, err := s.ActivePolicy(ctx); err == nil {
		return *p, nil
	}
	p := domain.DefaultPolicy()
	p.UpdatedBy = "system"
	if err := s.SavePolicy(ctx, &p, true); err != nil {
		return domain.Policy{}, err
	}
	return p, nil
}

// PolicyLimits caps admin-editable values. A prototype that lets an operator
// set MaxAutomatedAmount to ₹10,00,000 or MinActionConfidence to 0 would defeat
// the safety story it is meant to demonstrate (SRS 19.3).
const (
	policyMaxAutomatedCeiling = domain.Money(2_000_000) // ₹20,000
	policyMinConfidenceFloor  = 0.50
	policyMaxRetryCeiling     = 5
	policyMaxActionsCeiling   = 10
)

// PolicyBound is one admin-editable field's permitted range.
//
// Max is a pointer because one field genuinely has no ceiling: requiring human
// approval above an arbitrarily large amount is always safe, so validatePolicy
// only checks it is not negative. A null max says that; a made-up large number
// would read as a real limit.
type PolicyBound struct {
	Min float64  `json:"min"`
	Max *float64 `json:"max"`
}

// PolicyLimits publishes the bounds validatePolicy enforces, so the admin screen
// can show them instead of discovering them by being rejected.
//
// It is derived from the same constants the validator reads. A hand-written copy
// of these numbers in the API layer is the kind of duplication that drifts
// silently — the form offers a range the store refuses, and the operator is told
// their correct value is invalid.
func PolicyLimits() map[string]PolicyBound {
	upTo := func(min, max float64) PolicyBound { return PolicyBound{Min: min, Max: &max} }
	return map[string]PolicyBound{
		"max_retry_count":                  upTo(0, policyMaxRetryCeiling),
		"max_automated_amount":             upTo(0, float64(policyMaxAutomatedCeiling)),
		"min_action_confidence":            upTo(policyMinConfidenceFloor, 1),
		"cooldown_minutes":                 upTo(0, 24*60),
		"max_actions_per_customer_per_day": upTo(0, policyMaxActionsCeiling),
		"max_reminders_per_case":           upTo(0, policyMaxActionsCeiling),
		"max_actions_per_case":             upTo(0, policyMaxActionsCeiling),
		"require_human_approval_above":     {Min: 0},
	}
}

func validatePolicy(p *domain.Policy) error {
	if p == nil {
		return fmt.Errorf("%w: policy is required", domain.ErrValidation)
	}
	p.Version = strings.TrimSpace(p.Version)
	if p.Version == "" {
		return fmt.Errorf("%w: policy version is required", domain.ErrValidation)
	}
	switch {
	case p.MaxRetryCount < 0 || p.MaxRetryCount > policyMaxRetryCeiling:
		return fmt.Errorf("%w: max_retry_count must be between 0 and %d",
			domain.ErrValidation, policyMaxRetryCeiling)
	case p.MaxAutomatedAmount < 0 || p.MaxAutomatedAmount > policyMaxAutomatedCeiling:
		return fmt.Errorf("%w: max_automated_amount must be between 0 and %d paise",
			domain.ErrValidation, policyMaxAutomatedCeiling)
	case p.MinActionConfidence < policyMinConfidenceFloor || p.MinActionConfidence > 1:
		return fmt.Errorf("%w: min_action_confidence must be between %.2f and 1.0",
			domain.ErrValidation, policyMinConfidenceFloor)
	case p.CooldownMinutes < 0 || p.CooldownMinutes > 24*60:
		return fmt.Errorf("%w: cooldown_minutes must be between 0 and 1440", domain.ErrValidation)
	case p.MaxActionsPerCustomerPerDay < 0 || p.MaxActionsPerCustomerPerDay > policyMaxActionsCeiling:
		return fmt.Errorf("%w: max_actions_per_customer_per_day must be between 0 and %d",
			domain.ErrValidation, policyMaxActionsCeiling)
	case p.RequireHumanApprovalAbove < 0:
		return fmt.Errorf("%w: require_human_approval_above must not be negative", domain.ErrValidation)
	case p.MaxRemindersPerCase < 0 || p.MaxRemindersPerCase > policyMaxActionsCeiling:
		return fmt.Errorf("%w: max_reminders_per_case must be between 0 and %d",
			domain.ErrValidation, policyMaxActionsCeiling)
	case p.MaxActionsPerCase < 0 || p.MaxActionsPerCase > policyMaxActionsCeiling:
		return fmt.Errorf("%w: max_actions_per_case must be between 0 and %d",
			domain.ErrValidation, policyMaxActionsCeiling)
	}
	return nil
}
