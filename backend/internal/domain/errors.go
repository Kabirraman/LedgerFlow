package domain

import "errors"

// Sentinel errors. Callers match on these with errors.Is rather than string
// comparison so that HTTP handlers can map them to status codes centrally.
var (
	// ErrNotFound is returned when a requested entity does not exist.
	ErrNotFound = errors.New("not found")

	// ErrActionNotAllowed means a proposed action is not on the executor
	// allow-list (SRS FR-030, 19.2).
	ErrActionNotAllowed = errors.New("action not allow-listed")

	// ErrPolicyBlocked means the deterministic policy engine returned BLOCK.
	ErrPolicyBlocked = errors.New("action blocked by policy")

	// ErrApprovalRequired means the policy engine returned ESCALATE and no
	// human approval is on record yet (SRS 10.2).
	ErrApprovalRequired = errors.New("human approval required")

	// ErrInvalidTransition means a case state change is not permitted by the
	// state machine (SRS 14.2).
	ErrInvalidTransition = errors.New("invalid case state transition")

	// ErrSimulationBoundary means an external call was attempted from
	// simulation mode. This must never happen (SRS AC-009).
	ErrSimulationBoundary = errors.New("simulation mode must not call external APIs")

	// ErrInvalidSignature means webhook signature verification failed
	// (SRS FR-002, 19.3).
	ErrInvalidSignature = errors.New("invalid webhook signature")

	// ErrDuplicateEvent means the event was already processed (SRS FR-003).
	ErrDuplicateEvent = errors.New("duplicate event")

	// ErrAmountMismatch means a proposed amount disagrees with the trusted
	// amount held in the database (SRS 19.2, 22.4).
	ErrAmountMismatch = errors.New("amount does not match trusted record")

	// ErrUnauthorized / ErrForbidden back the auth middleware.
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")

	// ErrValidation is a generic bad-input error.
	ErrValidation = errors.New("validation error")

	// ErrAgentUnavailable means the AI provider failed or returned
	// unusable output; callers must fall back to a safe state (SRS 20.4).
	ErrAgentUnavailable = errors.New("agent unavailable")

	// ErrStopRuleTriggered means a mandatory stopping rule halted the case
	// (SRS 10.3).
	ErrStopRuleTriggered = errors.New("stopping rule triggered")
)
