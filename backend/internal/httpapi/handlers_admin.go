package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ledgerflow/ledgerflow/internal/domain"
	"github.com/ledgerflow/ledgerflow/internal/store"
)

// getPolicies returns the active policy and its history (SRS 16.5).
func (s *Server) getPolicies(c *gin.Context) {
	ctx := c.Request.Context()
	active := s.deps.Store.ActivePolicyOrDefault(ctx)
	history, err := s.deps.Store.ListPolicies(ctx)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{
		"active":  active,
		"history": history,
		"default": domain.DefaultPolicy(),
		// The bounds the store enforces, read from the store so the form shows the
		// real limits rather than a copy of them that has since drifted.
		"limits": store.PolicyLimits(),
	})
}

// policyRequest is an admin's proposed policy. Amounts are paise, matching the
// storage unit, so nothing is converted between the form and the ledger.
type policyRequest struct {
	Version                     string   `json:"version"`
	MaxRetryCount               *int     `json:"max_retry_count"`
	MaxAutomatedAmount          *int64   `json:"max_automated_amount"`
	MinActionConfidence         *float64 `json:"min_action_confidence"`
	CooldownMinutes             *int     `json:"cooldown_minutes"`
	MaxActionsPerCustomerPerDay *int     `json:"max_actions_per_customer_per_day"`
	RequireHumanApprovalAbove   *int64   `json:"require_human_approval_above"`
	MaxRemindersPerCase         *int     `json:"max_reminders_per_case"`
	MaxActionsPerCase           *int     `json:"max_actions_per_case"`
	// Activate makes this version the one the policy engine reads. Saving without
	// activating lets an admin stage a change and switch to it deliberately.
	Activate bool `json:"activate"`
}

// updatePolicy saves a policy version (SRS FR-062, 16.5).
//
// Fields are pointers, and an omitted field keeps its current value rather than
// resetting to a zero. That distinction matters here more than usual: a JSON body
// missing max_retry_count would otherwise silently set the retry cap to zero, which
// looks like a working policy and quietly stops all retries.
//
// A new version is a new row. Editing history in place would leave an executed
// action referring to a policy version whose contents had since changed, and the
// audit trail's answer to "what rule allowed this?" has to stay true after the fact
// (SRS 21.1).
func (s *Server) updatePolicy(c *gin.Context) {
	var req policyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		failWith(c, http.StatusBadRequest, "invalid_body", "expected a JSON object of policy fields")
		return
	}
	version := strings.TrimSpace(req.Version)
	if version == "" {
		failValidation(c, map[string]string{"version": "a policy version label is required"})
		return
	}
	if len(version) > 32 {
		failValidation(c, map[string]string{"version": "a version label must be 32 characters or fewer"})
		return
	}

	ctx := c.Request.Context()
	ident, _ := identityOf(c)

	// Start from the active policy so an omitted field is "unchanged", not "zero".
	p := s.deps.Store.ActivePolicyOrDefault(ctx)
	p.Version = version
	p.UpdatedBy = ident.Actor()
	p.UpdatedAt = time.Time{}

	if req.MaxRetryCount != nil {
		p.MaxRetryCount = *req.MaxRetryCount
	}
	if req.MaxAutomatedAmount != nil {
		p.MaxAutomatedAmount = domain.Money(*req.MaxAutomatedAmount)
	}
	if req.MinActionConfidence != nil {
		p.MinActionConfidence = *req.MinActionConfidence
	}
	if req.CooldownMinutes != nil {
		p.CooldownMinutes = *req.CooldownMinutes
	}
	if req.MaxActionsPerCustomerPerDay != nil {
		p.MaxActionsPerCustomerPerDay = *req.MaxActionsPerCustomerPerDay
	}
	if req.RequireHumanApprovalAbove != nil {
		p.RequireHumanApprovalAbove = domain.Money(*req.RequireHumanApprovalAbove)
	}
	if req.MaxRemindersPerCase != nil {
		p.MaxRemindersPerCase = *req.MaxRemindersPerCase
	}
	if req.MaxActionsPerCase != nil {
		p.MaxActionsPerCase = *req.MaxActionsPerCase
	}

	// The store validates the bounds. Re-checking them here would be a second copy
	// of the rules, and the copy that drifts is the one that lets a bad value in.
	if err := s.deps.Store.SavePolicy(ctx, &p, req.Activate); err != nil {
		fail(c, err)
		return
	}
	_ = s.deps.Store.Audit(ctx, ident.Actor(), "policy", p.Version, "", "policy_updated",
		map[string]any{"policy": p, "activated": req.Activate})

	ok(c, gin.H{"policy": p, "activated": req.Activate})
}

// syncPayments backfills failed payments from the Razorpay API (SRS FR-005).
//
// This is the one place the API layer touches the gateway, and it is a read: it
// lists payments and hands them to the ingestor, which turns them into cases using
// the same scoring path as the webhook. No money-moving call is reachable from
// here — those belong to the action executor, which this layer does not hold
// (SRS 19.2, 5.2).
func (s *Server) syncPayments(c *gin.Context) {
	if s.deps.Gateway == nil || s.deps.Ingestor == nil {
		notConfigured(c, "payment backfill")
		return
	}
	if !s.deps.Gateway.External() {
		// A sandbox or simulation gateway has nothing to sync. Reporting that plainly
		// beats returning an empty success that reads as "there were no failures".
		failWith(c, http.StatusServiceUnavailable, "no_gateway",
			"this deployment has no external Razorpay gateway configured, so there is nothing to sync")
		return
	}

	hours := intQuery(c, "hours", 24, 1, 24*30)
	count := intQuery(c, "count", 100, 1, 100)
	to := s.now()
	from := to.Add(-time.Duration(hours) * time.Hour)

	ctx := c.Request.Context()
	payments, err := s.deps.Gateway.ListPayments(ctx, from, to, count)
	if err != nil {
		fail(c, err)
		return
	}

	report, err := s.deps.Ingestor.BackfillPayments(ctx, payments)
	if err != nil {
		fail(c, err)
		return
	}
	report.From, report.To = from.Format(time.RFC3339), to.Format(time.RFC3339)

	ident, _ := identityOf(c)
	_ = s.deps.Store.Audit(ctx, ident.Actor(), "sync", "", "", "payments_synced",
		map[string]any{"from": report.From, "to": report.To, "report": report})

	ok(c, gin.H{"report": report})
}
