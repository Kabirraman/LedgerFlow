package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ledgerflow/ledgerflow/internal/domain"
	"github.com/ledgerflow/ledgerflow/internal/razorpay"
)

// Reconciler resolves actions whose outcome was never learned (SRS 20.2).
//
// An unresolved action is the dangerous state in a payment system: a link may or
// may not exist, so retrying risks a duplicate demand and abandoning risks
// losing a recovery that already happened. Neither guess is acceptable, so the
// resource is looked up by the reference id we sent — the one piece of
// correlation that survives a timeout — and the local record is corrected to
// match whatever the gateway actually holds.
type Reconciler struct {
	store   ReconcileStore
	gateway razorpay.Gateway
	cfg     ReconcileConfig
}

// ReconcileConfig tunes reconciliation.
type ReconcileConfig struct {
	// PendingStaleAfter is how long an action may sit in 'pending' before it is
	// treated as unresolved. It must comfortably exceed the gateway timeout, or a
	// call still in flight would be reconciled underneath itself.
	PendingStaleAfter time.Duration
	// Limit bounds one pass.
	Limit int
}

func (c ReconcileConfig) withDefaults() ReconcileConfig {
	if c.PendingStaleAfter <= 0 {
		c.PendingStaleAfter = 5 * time.Minute
	}
	if c.Limit <= 0 || c.Limit > 200 {
		c.Limit = 50
	}
	return c
}

// ReconcileStore is the persistence surface reconciliation needs.
type ReconcileStore interface {
	ListUnresolvedActions(ctx context.Context, staleAfter time.Duration, limit int) ([]domain.RecoveryAction, error)
	MarkActionExecuted(ctx context.Context, id, externalID, externalURL string, latencyMS int64) error
	MarkActionFailed(ctx context.Context, id, code, message string, latencyMS int64) error
	IncrementCaseActionCount(ctx context.Context, caseID string) error
	Audit(ctx context.Context, actor, entityType, entityID, caseID, eventType string, detail any) error
	IncrCounter(ctx context.Context, name string) error
}

// NewReconciler builds a reconciler.
func NewReconciler(s ReconcileStore, g razorpay.Gateway, cfg ReconcileConfig) *Reconciler {
	return &Reconciler{store: s, gateway: g, cfg: cfg.withDefaults()}
}

// ReconcileReport summarises one pass.
type ReconcileReport struct {
	Examined    int `json:"examined"`
	Confirmed   int `json:"confirmed"`
	Discarded   int `json:"discarded"`
	StillUnsure int `json:"still_unsure"`
}

// Run reconciles up to limit unresolved actions.
//
// A lookup failure leaves the action unresolved rather than resolving it either
// way. That is the whole point: an unresolved record is visible and retried
// later, while a wrong resolution is invisible and permanent.
func (r *Reconciler) Run(ctx context.Context, limit int) (ReconcileReport, error) {
	var rep ReconcileReport
	if r == nil || r.store == nil {
		return rep, errors.New("reconciler: not configured")
	}
	if limit <= 0 || limit > 200 {
		limit = r.cfg.Limit
	}

	actions, err := r.store.ListUnresolvedActions(ctx, r.cfg.PendingStaleAfter, limit)
	if err != nil {
		return rep, fmt.Errorf("list unresolved actions: %w", err)
	}

	for i := range actions {
		a := actions[i]
		rep.Examined++

		if r.gateway == nil || !r.gateway.External() {
			// A sandbox or simulation gateway cannot have a resource we failed to
			// hear about, because the call never left the process.
			rep.StillUnsure++
			continue
		}

		// An invoice reminder sends through Razorpay's notify endpoint and creates
		// no resource of its own, so a reference lookup can only ever come back
		// empty and would be misread as "nothing happened". Razorpay exposes no
		// per-notification record to check, so the delivery genuinely cannot be
		// determined. It is recorded as executed-with-unconfirmed-delivery: that
		// risks the customer having been reminded once and thinking nothing of it,
		// whereas the alternative risks reminding them twice about the same
		// invoice. If the money never arrives the verifier returns the case to the
		// pipeline, which sends a fresh reminder under a new key — so the recovery
		// is delayed rather than lost.
		if r.notifyOnly(a) {
			if err := r.store.MarkActionExecuted(ctx, a.ID, a.ExternalID, "", a.LatencyMS); err != nil {
				return rep, err
			}
			_ = r.store.IncrementCaseActionCount(ctx, a.CaseID)
			_ = r.store.IncrCounter(ctx, counterActionsExecuted)
			rep.Confirmed++
			_ = r.store.Audit(ctx, "reconciler", "action", a.ID, a.CaseID,
				"reconcile_delivery_unconfirmed", map[string]any{
					"action_type":     a.ActionType,
					"idempotency_key": a.IdempotencyKey,
					"note":            "notify endpoint leaves no resource to look up",
				})
			continue
		}

		link, lookupErr := r.gateway.FindPaymentLinkByReference(ctx, a.IdempotencyKey)
		if lookupErr != nil {
			rep.StillUnsure++
			_ = r.store.Audit(ctx, "reconciler", "action", a.ID, a.CaseID,
				"reconcile_deferred", map[string]any{"error": lookupErr.Error()})
			continue
		}

		if link == nil {
			// The resource does not exist, so nothing happened and the action can
			// be honestly marked failed. Only now is a fresh attempt safe.
			if err := r.store.MarkActionFailed(ctx, a.ID, "reconciled_not_created",
				"no resource exists for this idempotency key", a.LatencyMS); err != nil {
				return rep, err
			}
			rep.Discarded++
			_ = r.store.Audit(ctx, "reconciler", "action", a.ID, a.CaseID,
				"reconcile_not_created", map[string]any{"idempotency_key": a.IdempotencyKey})
			continue
		}

		// The resource does exist: the side effect happened and the local record
		// was simply never updated. Confirm it rather than repeating it.
		if err := r.store.MarkActionExecuted(ctx, a.ID, link.ID, link.ShortURL, a.LatencyMS); err != nil {
			return rep, err
		}
		_ = r.store.IncrementCaseActionCount(ctx, a.CaseID)
		_ = r.store.IncrCounter(ctx, counterActionsExecuted)
		rep.Confirmed++
		_ = r.store.Audit(ctx, "reconciler", "action", a.ID, a.CaseID,
			"reconcile_confirmed", map[string]any{
				"external_id": link.ID,
				"status":      link.Status,
				"amount_paid": int64(link.AmountPaid),
			})
	}
	return rep, nil
}

// notifyOnly reports whether the action's side effect was a notification against
// an existing Razorpay resource rather than the creation of a new one. It mirrors
// the invoice-reminder branch in perform, and the two must stay in step: if that
// branch ever creates a resource, the reference lookup becomes meaningful again.
func (r *Reconciler) notifyOnly(a domain.RecoveryAction) bool {
	return a.ActionType == domain.ActionReminder && a.ExternalID != ""
}

// StartReconcileWorker runs Run on a ticker until ctx is cancelled. Errors are
// reported through onError rather than stopping the loop: a transient database
// or gateway problem must not disable reconciliation for the rest of the process
// lifetime.
func (r *Reconciler) StartReconcileWorker(ctx context.Context, every time.Duration,
	limit int, onError func(error)) {

	if every <= 0 {
		every = time.Minute
	}
	ticker := time.NewTicker(every)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := r.Run(ctx, limit); err != nil && onError != nil {
					onError(err)
				}
			}
		}
	}()
}
