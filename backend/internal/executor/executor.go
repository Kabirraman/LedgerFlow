// Package executor implements the Action Execution Service — LEDGERFLOW's
// fourth agent, and the only component permitted to call a payment API
// (SRS 7 Agent 4, FR-042, 19.2).
//
// It makes no generative decisions. Its entire job is to take an already-approved
// decision and either perform exactly one bounded side effect or refuse, with
// three properties that must hold under every failure mode:
//
//   - Nothing executes that was not validated here, independently of whatever the
//     planner or the policy engine concluded (SRS 22.4).
//   - No side effect happens twice, enforced by a unique database constraint
//     rather than by in-process bookkeeping (SRS 20.1, AC-006).
//   - A call whose outcome is unknown is recorded as ambiguous and reconciled
//     against the gateway before anything else is attempted (SRS 20.2).
package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ledgerflow/ledgerflow/internal/domain"
	"github.com/ledgerflow/ledgerflow/internal/idem"
	"github.com/ledgerflow/ledgerflow/internal/razorpay"
)

// Request is the executor's input contract (SRS 8.4).
//
// Every field is supplied from trusted state: the case and decision come from the
// database, the amount from the underlying payment record, and Approved from the
// policy verdict. Nothing here originates in model output.
type Request struct {
	CaseID         string
	Action         domain.ActionType
	Approved       bool
	PolicyVersion  string
	IdempotencyKey string
	TargetAmount   domain.Money

	// RazorpayResourceID is the existing resource to act on: the payment id for a
	// retry, the invoice id for an invoice reminder, the subscription id for a
	// subscription charge.
	RazorpayResourceID string

	DecisionID string
	Mode       domain.RunMode

	// TrustedAmount is read straight from the source record and compared against
	// TargetAmount. Two independently sourced amounts that disagree mean
	// something upstream is wrong, and the action is refused (SRS 22.4).
	TrustedAmount domain.Money

	// Customer contact details, for delivering links and reminders.
	CustomerID      string
	CustomerName    string
	CustomerEmail   string
	CustomerContact string

	// Segment and SourceType are recorded as a strategy attempt so the learning
	// loop counts what was tried, not only what worked (SRS FR-053).
	Segment    domain.Segment
	SourceType domain.SourceType

	// Description appears on the payment link the customer sees.
	Description string

	// InvoiceID is the local invoice row, so its reminder counter can be bumped.
	InvoiceID string

	// Attempt is the retry ordinal, used to derive the idempotency key when one
	// is not supplied.
	Attempt int
}

// Result is the executor's outcome. Rejected and Executed are mutually
// exclusive; Duplicate means an earlier identical request already ran.
type Result struct {
	ActionID     string              `json:"action_id"`
	Action       domain.ActionType   `json:"action"`
	Status       domain.ActionStatus `json:"status"`
	ExternalID   string              `json:"external_id,omitempty"`
	ExternalURL  string              `json:"external_url,omitempty"`
	Amount       domain.Money        `json:"amount"`
	LatencyMS    int64               `json:"latency_ms"`
	Duplicate    bool                `json:"duplicate"`
	Rejected     bool                `json:"rejected"`
	RejectReason string              `json:"reject_reason,omitempty"`
	Gateway      string              `json:"gateway"`
}

// Executed reports whether a side effect actually took place.
func (r Result) Executed() bool { return r.Status == domain.ActionStatusExecuted && !r.Rejected }

// Store is the narrow persistence surface the executor needs. Declaring it here
// rather than importing a wide Store interface keeps the dependency honest about
// what this package can touch (SRS NFR-007).
type Store interface {
	ReserveAction(ctx context.Context, a *domain.RecoveryAction) (bool, error)
	MarkActionExecuted(ctx context.Context, id, externalID, externalURL string, latencyMS int64) error
	MarkActionFailed(ctx context.Context, id, code, message string, latencyMS int64) error
	MarkActionAmbiguous(ctx context.Context, id, code, message string, latencyMS int64) error
	MarkActionSkipped(ctx context.Context, id, reason string) error
	IncrementCaseActionCount(ctx context.Context, caseID string) error
	IncrementInvoiceReminder(ctx context.Context, id string) error
	RecordStrategyAttempt(ctx context.Context, seg domain.Segment,
		st domain.SourceType, at domain.ActionType) error
	Audit(ctx context.Context, actor, entityType, entityID, caseID, eventType string, detail any) error
	IncrCounter(ctx context.Context, name string) error
	AddCounter(ctx context.Context, name string, n, sum int64) error
}

// Counter names the executor reports. Duplicated as a small local list rather
// than imported, so this package does not depend on the store package's
// constants for a plain string.
const (
	counterActionsExecuted         = "actions_executed"
	counterActionsRejected         = "actions_rejected"
	counterActionsAmbiguous        = "actions_ambiguous"
	counterDuplicateActionRequests = "duplicate_action_requests"
	counterActionAPIFailures       = "action_api_failures"
	counterActionLatency           = "action_latency_ms"
)

// Config tunes execution.
type Config struct {
	// LinkExpiryHours bounds how long a generated payment link stays valid.
	LinkExpiryHours int
	// CallbackURL is where a paid link returns the customer.
	CallbackURL string
	// Timeout bounds a single gateway call.
	Timeout time.Duration
	// NotifyEmail and NotifySMS control delivery channels.
	NotifyEmail bool
	NotifySMS   bool
}

func (c Config) withDefaults() Config {
	if c.LinkExpiryHours <= 0 {
		c.LinkExpiryHours = 48
	}
	if c.Timeout <= 0 {
		c.Timeout = 15 * time.Second
	}
	return c
}

// Executor performs approved recovery actions.
type Executor struct {
	store   Store
	gateway razorpay.Gateway
	cfg     Config
	now     func() time.Time
}

// New builds an executor. The gateway is the only external transport it holds.
func New(s Store, g razorpay.Gateway, cfg Config) *Executor {
	return &Executor{
		store:   s,
		gateway: g,
		cfg:     cfg.withDefaults(),
		now:     func() time.Time { return time.Now().UTC() },
	}
}

// SetClock overrides the clock for deterministic tests.
func (e *Executor) SetClock(fn func() time.Time) { e.now = fn }

// ErrNotApproved means the caller tried to execute without a policy PASS.
var ErrNotApproved = errors.New("executor: action was not approved by the policy engine")

// Execute performs one action, or refuses.
//
// The order of operations is the safety property. Validation runs before the row
// is reserved; the row is reserved before the external call; the external call
// happens at most once per idempotency key. A refusal never touches the gateway.
func (e *Executor) Execute(ctx context.Context, req Request) (Result, error) {
	res := Result{Action: req.Action, Amount: req.TargetAmount, Gateway: e.gatewayName()}

	// --- Stage 1: deterministic validation. No side effects are possible here.
	if reason := e.validate(req); reason != "" {
		res.Rejected = true
		res.RejectReason = reason
		res.Status = domain.ActionStatusSkipped
		_ = e.store.IncrCounter(ctx, counterActionsRejected)
		_ = e.store.Audit(ctx, "action_executor", "case", req.CaseID, req.CaseID,
			"action_rejected", map[string]any{
				"action":         string(req.Action),
				"reason":         reason,
				"policy_version": req.PolicyVersion,
			})
		return res, fmt.Errorf("%w: %s", domain.ErrActionNotAllowed, reason)
	}

	// escalate and no_action are recorded but never call anything. Persisting
	// them keeps the audit trail complete: "we decided not to act" is a decision
	// an operator needs to see.
	if !req.Action.IsExternal() {
		return e.recordNonExternal(ctx, req)
	}

	key := req.IdempotencyKey
	if key == "" {
		key = idem.ActionKey(req.CaseID, req.Action, req.Attempt)
	}

	// --- Stage 2: reserve. The row and its unique key exist before any call, so
	// a crash mid-flight leaves a reconcilable 'pending' row (SRS FR-043).
	action := &domain.RecoveryAction{
		CaseID:         req.CaseID,
		DecisionID:     req.DecisionID,
		ActionType:     req.Action,
		IdempotencyKey: key,
		Amount:         req.TargetAmount,
		Status:         domain.ActionStatusPending,
		Mode:           e.modeOf(req),
		Environment:    domain.EnvTest,
		RequestedAt:    e.now(),
		// A notify-only action acts on a resource that already exists, so the
		// reserved row names it before the call. That makes a stranded 'pending'
		// row self-describing: the reconciler can then tell a notification whose
		// delivery it cannot verify from a payment link it can look up by
		// reference id (SRS 20.2).
		ExternalID: notifyResourceID(req),
	}
	created, err := e.store.ReserveAction(ctx, action)
	if err != nil {
		return res, fmt.Errorf("reserve action for case %s: %w", req.CaseID, err)
	}
	res.ActionID = action.ID

	if !created {
		// The key already existed. Return what the first request produced rather
		// than performing the side effect again — a duplicate request must yield
		// the original result, not a second payment link (SRS 22.4).
		_ = e.store.IncrCounter(ctx, counterDuplicateActionRequests)
		_ = e.store.Audit(ctx, "action_executor", "action", action.ID, req.CaseID,
			"duplicate_action_request", map[string]any{
				"idempotency_key": key,
				"existing_status": string(action.Status),
			})
		return Result{
			ActionID:    action.ID,
			Action:      action.ActionType,
			Status:      action.Status,
			ExternalID:  action.ExternalID,
			ExternalURL: action.ExternalURL,
			Amount:      action.Amount,
			LatencyMS:   action.LatencyMS,
			Duplicate:   true,
			Gateway:     e.gatewayName(),
		}, nil
	}

	// --- Stage 3: the one external call.
	callCtx, cancel := context.WithTimeout(ctx, e.cfg.Timeout)
	defer cancel()

	started := e.now()
	externalID, externalURL, callErr := e.perform(callCtx, req, key)
	latency := time.Since(started).Milliseconds()
	res.LatencyMS = latency
	_ = e.store.AddCounter(ctx, counterActionLatency, 1, latency)

	if callErr != nil {
		return e.recordFailure(ctx, req, action, res, callErr, latency)
	}

	// --- Stage 4: record success.
	if err := e.store.MarkActionExecuted(ctx, action.ID, externalID, externalURL, latency); err != nil {
		// The side effect happened but the record failed. Surfacing the error is
		// correct: the reserved row still exists, so the reconciler will find it.
		return res, fmt.Errorf("action %s executed but could not be recorded: %w", action.ID, err)
	}
	_ = e.store.IncrementCaseActionCount(ctx, req.CaseID)
	_ = e.store.IncrCounter(ctx, counterActionsExecuted)

	// The attempt is counted now, not at verification. Counting attempts only
	// when they succeed would make every strategy look perfect (SRS FR-053).
	if req.Segment != "" && req.SourceType != "" {
		_ = e.store.RecordStrategyAttempt(ctx, req.Segment, req.SourceType, req.Action)
	}
	if req.Action == domain.ActionReminder && req.InvoiceID != "" {
		_ = e.store.IncrementInvoiceReminder(ctx, req.InvoiceID)
	}

	_ = e.store.Audit(ctx, "action_executor", "action", action.ID, req.CaseID,
		"action_executed", map[string]any{
			"action":          string(req.Action),
			"external_id":     externalID,
			"amount":          int64(req.TargetAmount),
			"gateway":         e.gatewayName(),
			"latency_ms":      latency,
			"idempotency_key": key,
			"policy_version":  req.PolicyVersion,
		})

	res.Status = domain.ActionStatusExecuted
	res.ExternalID = externalID
	res.ExternalURL = externalURL
	return res, nil
}

// validate is the executor's independent gate. It repeats checks the policy
// engine already made, on purpose: two independent controls that must both agree
// is the point, not redundancy to be optimised away (SRS 19.3, 22.4).
func (e *Executor) validate(req Request) string {
	if req.CaseID == "" {
		return "case id is required"
	}
	if !req.Action.Valid() {
		return fmt.Sprintf("action %q is not on the executor allow-list", string(req.Action))
	}
	if !req.Approved {
		return "action was not approved by the policy engine"
	}
	if req.Action.IsExternal() {
		if req.TargetAmount <= 0 {
			return "target amount must be positive"
		}
		// Amount integrity: the amount to act on and the amount on the source
		// record are gathered independently and must match exactly. Money is
		// int64 paise, so this is an exact comparison with no tolerance.
		if req.TrustedAmount <= 0 {
			return "no trusted amount available for this case"
		}
		if req.TargetAmount != req.TrustedAmount {
			return fmt.Sprintf("target amount %d does not match trusted amount %d",
				req.TargetAmount, req.TrustedAmount)
		}
		if req.Action == domain.ActionRetry && req.RazorpayResourceID == "" {
			return "retry requires the original razorpay resource id"
		}
		if req.CustomerEmail == "" && req.CustomerContact == "" {
			return "customer has no email or contact; the action cannot be delivered"
		}
		// Simulation mode must never reach an external endpoint (SRS AC-009).
		// This is checked here as well as at wiring time, because a
		// misconfiguration that pointed a simulation run at the live-test gateway
		// would otherwise send real notifications to real addresses.
		if req.Mode == domain.ModeSimulation && e.gateway != nil && e.gateway.External() {
			return "simulation mode cannot use an external gateway"
		}
	}
	return ""
}

// recordNonExternal persists escalate / no_action without touching a gateway.
func (e *Executor) recordNonExternal(ctx context.Context, req Request) (Result, error) {
	key := req.IdempotencyKey
	if key == "" {
		key = idem.ActionKey(req.CaseID, req.Action, req.Attempt)
	}
	action := &domain.RecoveryAction{
		CaseID:         req.CaseID,
		DecisionID:     req.DecisionID,
		ActionType:     req.Action,
		IdempotencyKey: key,
		Amount:         0,
		Status:         domain.ActionStatusSkipped,
		Mode:           e.modeOf(req),
		Environment:    domain.EnvTest,
		RequestedAt:    e.now(),
	}
	created, err := e.store.ReserveAction(ctx, action)
	if err != nil {
		return Result{Action: req.Action, Gateway: e.gatewayName()}, err
	}
	reason := "escalated for human review"
	if req.Action == domain.ActionNoAction {
		reason = "no action taken: no positive expected value"
	}
	if created {
		_ = e.store.MarkActionSkipped(ctx, action.ID, reason)
		_ = e.store.Audit(ctx, "action_executor", "action", action.ID, req.CaseID,
			"action_recorded", map[string]any{"action": string(req.Action), "reason": reason})
	}
	return Result{
		ActionID:  action.ID,
		Action:    req.Action,
		Status:    domain.ActionStatusSkipped,
		Duplicate: !created,
		Gateway:   e.gatewayName(),
	}, nil
}

// perform dispatches to the gateway. This switch is the complete set of external
// side effects LEDGERFLOW can produce — there is no default branch that could
// grow into one by accident.
func (e *Executor) perform(ctx context.Context, req Request, key string) (externalID, externalURL string, err error) {
	if e.gateway == nil {
		return "", "", &razorpay.APIError{Code: "gateway_unavailable", Description: "no gateway configured"}
	}

	switch req.Action {
	case domain.ActionRetry, domain.ActionPaymentLink:
		// A retry is not a silent re-charge of a stored instrument: LEDGERFLOW has
		// no mandate to pull money, so both actions collect through a Payment
		// Link the customer completes. Doing otherwise would be a destructive
		// financial action, which is explicitly out of scope (SRS 5.2, 19.1).
		link, err := e.gateway.CreatePaymentLink(ctx, razorpay.PaymentLinkRequest{
			Amount:          req.TargetAmount,
			Currency:        "INR",
			Description:     e.describe(req),
			ReferenceID:     key,
			CustomerName:    req.CustomerName,
			CustomerEmail:   req.CustomerEmail,
			CustomerContact: req.CustomerContact,
			NotifyEmail:     e.cfg.NotifyEmail && req.CustomerEmail != "",
			NotifySMS:       e.cfg.NotifySMS && req.CustomerContact != "",
			ReminderEnable:  true,
			ExpireBy:        e.now().Add(time.Duration(e.cfg.LinkExpiryHours) * time.Hour),
			CallbackURL:     e.cfg.CallbackURL,
			Notes: map[string]string{
				"ledgerflow_case":   req.CaseID,
				"ledgerflow_action": string(req.Action),
				"idempotency_key":   key,
			},
		})
		if err != nil {
			return "", "", err
		}
		return link.ID, link.ShortURL, nil

	case domain.ActionReminder:
		// An invoice reminder uses Razorpay's own notify endpoint against the
		// existing invoice, so the customer receives one document rather than a
		// second parallel demand for the same money.
		if notifyOnly(req) {
			medium := "email"
			if req.CustomerEmail == "" {
				medium = "sms"
			}
			if err := e.gateway.NotifyInvoice(ctx, req.RazorpayResourceID, medium); err != nil {
				return "", "", err
			}
			return req.RazorpayResourceID, "", nil
		}
		// Otherwise the reminder is a fresh payment link with reminders enabled.
		link, err := e.gateway.CreatePaymentLink(ctx, razorpay.PaymentLinkRequest{
			Amount:          req.TargetAmount,
			Currency:        "INR",
			Description:     e.describe(req),
			ReferenceID:     key,
			CustomerName:    req.CustomerName,
			CustomerEmail:   req.CustomerEmail,
			CustomerContact: req.CustomerContact,
			NotifyEmail:     e.cfg.NotifyEmail && req.CustomerEmail != "",
			NotifySMS:       e.cfg.NotifySMS && req.CustomerContact != "",
			ReminderEnable:  true,
			ExpireBy:        e.now().Add(time.Duration(e.cfg.LinkExpiryHours) * time.Hour),
			CallbackURL:     e.cfg.CallbackURL,
			Notes: map[string]string{
				"ledgerflow_case":   req.CaseID,
				"ledgerflow_action": "reminder",
				"idempotency_key":   key,
			},
		})
		if err != nil {
			return "", "", err
		}
		return link.ID, link.ShortURL, nil
	}

	// Unreachable: validate() rejects anything not handled above. Returning an
	// error rather than panicking keeps a future action type from taking the
	// process down.
	return "", "", &razorpay.APIError{
		Code:        "unsupported_action",
		Description: "no execution path for action " + string(req.Action),
	}
}

// notifyOnly reports whether an action notifies an existing Razorpay resource
// instead of creating a new one. Exactly one action does: an invoice reminder,
// which goes through Razorpay's notify endpoint on the invoice the customer
// already has. It is a predicate rather than an inline condition because the
// reserve path and the perform path must agree about it — if they disagreed, a
// stranded row would be reconciled by the wrong method.
func notifyOnly(req Request) bool {
	return req.Action == domain.ActionReminder &&
		req.SourceType == domain.SourceInvoiceOverdue &&
		req.RazorpayResourceID != ""
}

// notifyResourceID is the resource id to record at reserve time, or empty when the
// action will create its own resource.
func notifyResourceID(req Request) string {
	if notifyOnly(req) {
		return req.RazorpayResourceID
	}
	return ""
}

// recordFailure classifies a failed call. The distinction that matters is
// whether a resource may exist despite the error: a definite failure can be
// retried, an ambiguous one must be reconciled first (SRS 20.2, 20.3).
func (e *Executor) recordFailure(ctx context.Context, req Request, action *domain.RecoveryAction,
	res Result, callErr error, latency int64) (Result, error) {

	code, message, ambiguous := classify(callErr)
	_ = e.store.IncrCounter(ctx, counterActionAPIFailures)

	if ambiguous {
		_ = e.store.MarkActionAmbiguous(ctx, action.ID, code, message, latency)
		_ = e.store.IncrCounter(ctx, counterActionsAmbiguous)
		_ = e.store.Audit(ctx, "action_executor", "action", action.ID, req.CaseID,
			"action_ambiguous", map[string]any{
				"action": string(req.Action), "error_code": code, "error": message,
			})
		res.Status = domain.ActionStatusAmbiguous
		return res, fmt.Errorf("action %s outcome unknown, awaiting reconciliation: %w", action.ID, callErr)
	}

	_ = e.store.MarkActionFailed(ctx, action.ID, code, message, latency)
	_ = e.store.Audit(ctx, "action_executor", "action", action.ID, req.CaseID,
		"action_failed", map[string]any{
			"action": string(req.Action), "error_code": code, "error": message,
		})
	res.Status = domain.ActionStatusFailed
	return res, fmt.Errorf("action %s failed: %w", action.ID, callErr)
}

// classify extracts a stable code and the ambiguity flag from a gateway error.
func classify(err error) (code, message string, ambiguous bool) {
	var apiErr *razorpay.APIError
	if errors.As(err, &apiErr) {
		code = apiErr.Code
		if code == "" {
			code = fmt.Sprintf("http_%d", apiErr.StatusCode)
		}
		return code, apiErr.Error(), apiErr.Ambiguous
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// A timeout is the canonical ambiguous case: the request may well have
		// been processed after we stopped waiting.
		return "timeout", err.Error(), true
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled", err.Error(), true
	}
	return "transport_error", err.Error(), true
}

func (e *Executor) describe(req Request) string {
	switch req.SourceType {
	case domain.SourceInvoiceOverdue:
		return firstNonEmpty(req.Description, "Payment for your outstanding invoice")
	case domain.SourceCheckoutAbandonment:
		return firstNonEmpty(req.Description, "Complete your purchase")
	case domain.SourceSubscriptionFailure:
		return firstNonEmpty(req.Description, "Update payment for your subscription")
	default:
		return firstNonEmpty(req.Description, "Complete your pending payment")
	}
}

func (e *Executor) modeOf(req Request) domain.RunMode {
	if req.Mode != "" {
		return req.Mode
	}
	return domain.ModeLiveTest
}

func (e *Executor) gatewayName() string {
	if e.gateway == nil {
		return "none"
	}
	return e.gateway.Name()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
