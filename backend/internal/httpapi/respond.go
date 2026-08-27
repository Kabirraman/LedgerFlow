// Package httpapi is the REST surface described in SRS 15.2.
//
// Three rules hold across every handler here:
//
//  1. Authorization is declared per route as a *minimum* role, checked by
//     middleware before the handler runs. A handler never decides who may call
//     it (SRS 15.1).
//  2. Money and state come from the store. Request bodies supply intent —
//     which case, which decision, why — never amounts, statuses or policy
//     verdicts (SRS 19.2).
//  3. Errors are mapped from domain sentinels to status codes in one place, so
//     a new handler cannot invent its own error vocabulary.
package httpapi

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// errorBody is the single error envelope the API returns.
type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Details carries field-level validation messages. It is omitted rather
	// than sent empty so a client can test for its presence.
	Details map[string]string `json:"details,omitempty"`
}

// fail writes a mapped error response and aborts the handler chain.
//
// The mapping is deliberately total: an unrecognised error becomes a 500 with a
// generic message rather than leaking an internal string to the client. The real
// error still reaches the log through the request logger.
func fail(c *gin.Context, err error) {
	status, code, msg := classify(err)
	if status >= http.StatusInternalServerError {
		// Attach the real error for the logger, but do not send it.
		_ = c.Error(err)
	}
	c.AbortWithStatusJSON(status, errorBody{errorDetail{Code: code, Message: msg}})
}

// failWith writes an explicit status and message, for conditions that are not
// domain errors (a malformed query parameter, an unsupported sort field).
func failWith(c *gin.Context, status int, code, msg string) {
	c.AbortWithStatusJSON(status, errorBody{errorDetail{Code: code, Message: msg}})
}

// failValidation reports field-level problems so the form that produced them can
// mark the offending inputs.
func failValidation(c *gin.Context, details map[string]string) {
	c.AbortWithStatusJSON(http.StatusBadRequest, errorBody{errorDetail{
		Code:    "validation_failed",
		Message: "one or more fields are invalid",
		Details: details,
	}})
}

func classify(err error) (status int, code, msg string) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return http.StatusNotFound, "not_found", "the requested resource does not exist"
	case errors.Is(err, domain.ErrValidation):
		return http.StatusBadRequest, "validation_failed", err.Error()
	case errors.Is(err, domain.ErrUnauthorized):
		return http.StatusForbidden, "forbidden", "your role does not permit this operation"
	case errors.Is(err, domain.ErrInvalidTransition):
		// The case moved since the client read it. 409 rather than 400: the
		// request was well-formed, the world changed underneath it.
		return http.StatusConflict, "invalid_transition", err.Error()
	case errors.Is(err, domain.ErrPolicyBlocked):
		return http.StatusForbidden, "policy_blocked", err.Error()
	case errors.Is(err, domain.ErrApprovalRequired):
		return http.StatusForbidden, "approval_required", err.Error()
	case errors.Is(err, domain.ErrActionNotAllowed):
		return http.StatusForbidden, "action_not_allowed", err.Error()
	case errors.Is(err, domain.ErrDuplicateEvent):
		return http.StatusOK, "duplicate", err.Error()
	case errors.Is(err, domain.ErrInvalidSignature):
		return http.StatusUnauthorized, "invalid_signature", "webhook signature verification failed"
	case errors.Is(err, domain.ErrSimulationBoundary):
		// This is a bug in our own wiring, not a client error, and it means a
		// safety boundary was nearly crossed.
		return http.StatusInternalServerError, "simulation_boundary", "simulation attempted an external call"
	case errors.Is(err, domain.ErrAgentUnavailable):
		return http.StatusServiceUnavailable, "agent_unavailable", "the AI layer is unavailable; the deterministic path was used"
	default:
		return http.StatusInternalServerError, "internal_error", "an unexpected error occurred"
	}
}

// ok writes a successful JSON response.
func ok(c *gin.Context, body any) { c.JSON(http.StatusOK, body) }

// created writes a 201 with the new resource.
func created(c *gin.Context, body any) { c.JSON(http.StatusCreated, body) }

// accepted writes a 202 for work that was queued rather than completed.
func accepted(c *gin.Context, body any) { c.JSON(http.StatusAccepted, body) }
