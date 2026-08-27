package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// listCases serves the recovery queue (SRS 16.2).
func (s *Server) listCases(c *gin.Context) {
	filter, valid := s.parseCaseFilter(c)
	if !valid {
		return
	}
	page, err := s.deps.Store.ListCases(c.Request.Context(), filter)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"page": page, "data_label": "razorpay test mode"})
}

// caseDetail serves one case with its full decision trail (SRS 16.2, AC-005).
//
// The response includes the SRS AC-010 explanation block, built from the stored
// reason codes, policy checks and alternatives. The raw records travel alongside it
// so a reviewer can check the explanation against the facts rather than take it on
// trust.
func (s *Server) caseDetail(c *gin.Context) {
	id, valid := pathID(c, "id")
	if !valid {
		return
	}
	detail, err := s.deps.Store.CaseDetail(c.Request.Context(), id)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{
		"case":        detail,
		"explanation": buildExplanation(detail),
		// The next legal states are computed from the same transition table the
		// workers obey, so a UI cannot offer a button the server would refuse
		// (SRS 14.2).
		"allowed_transitions": allowedTransitions(detail.Case.Status),
		"data_label":          "razorpay test mode",
	})
}

// allowedTransitions lists where this case can legally go next.
func allowedTransitions(from domain.CaseStatus) []domain.CaseStatus {
	out := []domain.CaseStatus{}
	for _, to := range domain.AllCaseStatuses {
		if to != from && domain.CanTransition(from, to) {
			out = append(out, to)
		}
	}
	return out
}

// reanalyzeCase pushes a case back through the agent pipeline (SRS 15.2).
//
// It does not force a status. The transition table permits ANALYZING only from NEW,
// RETRYING or itself, so a case elsewhere is advanced from where it actually is
// instead. A case waiting on a human is refused outright: re-running the agents
// there would discard the reviewer's pending decision, which is exactly what the
// approval gate exists to prevent.
func (s *Server) reanalyzeCase(c *gin.Context) {
	id, valid := pathID(c, "id")
	if !valid {
		return
	}
	if s.deps.Orchestrator == nil {
		notConfigured(c, "the agent orchestrator")
		return
	}
	ctx := c.Request.Context()

	rc, err := s.deps.Store.GetCase(ctx, id)
	if err != nil {
		fail(c, err)
		return
	}
	switch {
	case rc.Status == domain.StatusWaitingHuman:
		failWith(c, http.StatusConflict, "awaiting_review",
			"this case is waiting for a human decision; approve or reject it before re-analysing")
		return
	case domain.IsTerminal(rc.Status):
		failWith(c, http.StatusConflict, "case_closed",
			"this case is "+string(rc.Status)+" and cannot be re-analysed")
		return
	}

	actor := actorOf(c)
	reset := false
	if domain.CanTransition(rc.Status, domain.StatusAnalyzing) {
		if err := s.deps.Store.UpdateCaseStatus(ctx, id, domain.StatusAnalyzing, ""); err != nil {
			fail(c, err)
			return
		}
		reset = true
	}
	_ = s.deps.Store.Audit(ctx, actor, "case", id, id, "reanalyze_requested",
		map[string]any{"from_status": rc.Status, "reset_to_analyzing": reset})

	prog, err := s.deps.Orchestrator.AdvanceCase(ctx, id)
	if err != nil {
		fail(c, err)
		return
	}
	accepted(c, gin.H{
		"progress": prog,
		// Reported so the caller knows whether the pipeline restarted or merely
		// continued. A UI that claimed "re-analysed" for a resumed case would be
		// overstating what happened.
		"restarted": reset,
	})
}

// verifyCase confirms recovery for the case's most recent executed action, so an
// operator can resolve one case without waiting for the verification poll.
//
// It never invents an outcome. Verification reads the gateway; if there is nothing
// executed to verify, or the action produced no external resource, the request is
// refused rather than answered with a guess (SRS FR-049, 20.2).
func (s *Server) verifyCase(c *gin.Context) {
	id, valid := pathID(c, "id")
	if !valid {
		return
	}
	if s.deps.Verifier == nil {
		notConfigured(c, "the recovery verifier")
		return
	}
	ctx := c.Request.Context()

	detail, err := s.deps.Store.CaseDetail(ctx, id)
	if err != nil {
		fail(c, err)
		return
	}

	action := latestVerifiable(detail.Actions)
	if action == nil {
		failWith(c, http.StatusConflict, "nothing_to_verify",
			"this case has no executed action with an external reference to verify")
		return
	}

	rep, err := s.deps.Verifier.VerifyCase(ctx, action.ID)
	if err != nil {
		fail(c, err)
		return
	}
	_ = s.deps.Store.Audit(ctx, actorOf(c), "action", action.ID, id, "verification_requested",
		map[string]any{"external_id": action.ExternalID, "report": rep})

	ok(c, gin.H{"action_id": action.ID, "report": rep})
}

// latestVerifiable finds the newest executed action that produced something a
// gateway can be asked about. Actions are returned newest-first by the store.
func latestVerifiable(actions []domain.RecoveryAction) *domain.RecoveryAction {
	for i := range actions {
		a := actions[i]
		if a.Status == domain.ActionStatusExecuted && a.ExternalID != "" {
			return &a
		}
	}
	return nil
}

// approvalRequest is a reviewer's verdict. The note is required on rejection and
// optional on approval (SRS FR-046).
type approvalRequest struct {
	Note string `json:"note"`
}

// approveCase records a human approval and lets the case proceed (SRS FR-045).
//
// Approval downgrades an ESCALATE to a pass; it can never override a BLOCK. That
// asymmetry lives in the policy engine, and this handler does not restate it — it
// moves the case to APPROVED and hands it back to the orchestrator, which
// re-evaluates policy before anything is executed (SRS 10.3).
func (s *Server) approveCase(c *gin.Context) {
	s.decideCase(c, domain.ApprovalApproved)
}

// rejectCase records a human rejection, which ends the case.
func (s *Server) rejectCase(c *gin.Context) {
	s.decideCase(c, domain.ApprovalRejected)
}

func (s *Server) decideCase(c *gin.Context, decision domain.ApprovalDecision) {
	id, valid := pathID(c, "id")
	if !valid {
		return
	}
	var req approvalRequest
	// An empty body is acceptable for an approval, so a bind failure is only fatal
	// when it is not simply "there was nothing to bind".
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			failWith(c, http.StatusBadRequest, "invalid_body", "expected a JSON object with an optional note")
			return
		}
	}
	note := strings.TrimSpace(req.Note)
	if decision == domain.ApprovalRejected && note == "" {
		// Required, and required here as well as in the store: a rejection with no
		// stated reason leaves the audit trail unable to answer why the money was
		// abandoned (SRS FR-046, 21.2).
		failValidation(c, map[string]string{"note": "a reason is required when rejecting"})
		return
	}

	ctx := c.Request.Context()
	ident, _ := identityOf(c)
	reviewer := ident.Actor()

	pending, err := s.pendingApproval(ctx, id)
	if err != nil {
		fail(c, err)
		return
	}
	if pending == nil {
		failWith(c, http.StatusConflict, "no_pending_approval",
			"this case has no pending approval to decide")
		return
	}

	approval, err := s.deps.Store.DecideApproval(ctx, pending.ID, decision, reviewer, note)
	if err != nil {
		fail(c, err)
		return
	}

	to := domain.StatusApproved
	stopReason := ""
	if decision == domain.ApprovalRejected {
		to, stopReason = domain.StatusRejected, "rejected by "+reviewer+": "+truncate(note, 200)
	}
	if err := s.deps.Store.UpdateCaseStatus(ctx, id, to, stopReason); err != nil {
		fail(c, err)
		return
	}
	_ = s.deps.Store.Audit(ctx, reviewer, "approval", approval.ID, id, "approval_"+string(decision),
		map[string]any{"decision_id": approval.DecisionID, "note": note, "case_status": to})

	body := gin.H{"approval": approval, "case_status": to}

	// Executing straight after approval is configurable. When it is off, the case
	// sits at APPROVED for the orchestrator's next pass — which is the same outcome,
	// just not inside this request.
	if decision == domain.ApprovalApproved && s.deps.Config.AutoExecuteApproved && s.deps.Orchestrator != nil {
		prog, advErr := s.deps.Orchestrator.AdvanceCase(ctx, id)
		if advErr != nil {
			// The approval is already recorded and durable. Reporting the execution
			// problem without failing the request keeps those two facts separate: the
			// reviewer's decision stands even if the gateway call did not.
			s.log.ErrorContext(ctx, "execute after approval failed",
				"case_id", id, "approval_id", approval.ID, "error", advErr)
			body["execution_error"] = "the approval was recorded but execution did not complete; it will be retried"
		} else {
			body["progress"] = prog
		}
	}
	ok(c, body)
}

// pendingApproval finds the case's open review request, if any.
func (s *Server) pendingApproval(ctx context.Context, caseID string) (*domain.Approval, error) {
	list, err := s.deps.Store.ListApprovalsForCase(ctx, caseID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	for i := range list {
		if list[i].Decision == domain.ApprovalPending {
			return &list[i], nil
		}
	}
	return nil, nil
}

// listApprovals serves the review queue, highest value and least confident first
// (SRS 16.3).
//
// Requests whose action has already been executed are hidden by default. Showing
// them would invite a reviewer to authorise something that already happened, and
// an approval granted after the fact is not a control.
func (s *Server) listApprovals(c *gin.Context) {
	limit := intQuery(c, "limit", 50, 1, 200)
	includeExecuted := c.Query("include_executed") == "true"

	items, err := s.deps.Store.ApprovalQueue(c.Request.Context(), limit, !includeExecuted)
	if err != nil {
		fail(c, err)
		return
	}
	// Counted separately from len(items) on purpose: the returned slice is capped by
	// limit, so reporting its length as the backlog would understate a queue longer
	// than one page.
	total, err := s.deps.Store.PendingApprovalCount(c.Request.Context(), !includeExecuted)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{
		"approvals":       items,
		"limit":           limit,
		"returned":        len(items),
		"hiding_executed": !includeExecuted,
		"total_pending":   total,
	})
}

// caseAudit returns the append-only audit trail for one case (SRS 21.2, AC-005).
//
// Every external side effect is linked to a case id and an action id, and this is
// where that linkage is read back. The trail is returned whole rather than
// paginated: an audit view that silently truncates is not an audit view.
func (s *Server) caseAudit(c *gin.Context) {
	id, valid := pathID(c, "caseId")
	if !valid {
		return
	}
	entries, err := s.deps.Store.ListAuditForCase(c.Request.Context(), id)
	if err != nil {
		fail(c, err)
		return
	}
	ok(c, gin.H{"case_id": id, "audit": entries, "count": len(entries)})
}
