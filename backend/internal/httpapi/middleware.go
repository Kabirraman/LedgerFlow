package httpapi

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ledgerflow/ledgerflow/internal/auth"
	"github.com/ledgerflow/ledgerflow/internal/domain"
	"github.com/ledgerflow/ledgerflow/internal/store"
)

// identityKey is where the authenticated caller is stored on the request
// context. Handlers read it through identityOf, never by string key.
const identityKey = "ledgerflow.identity"

// requestIDKey is where the correlation id lives.
const requestIDKey = "ledgerflow.request_id"

// caseIDKey and actionIDKey let a handler tell the logger which case or action a
// request turned out to concern, for the requests where that is not in the path.
// Every external side effect is traceable to both (SRS 21.2, AC-005).
const caseIDKey = "ledgerflow.case_id"
const actionIDKey = "ledgerflow.action_id"

// maxWebhookBody bounds the webhook body independently of the ingestor's own
// limit, so an oversized POST is rejected before it is read into memory.
const maxWebhookBody = 1 << 20 // 1 MiB

// maxJSONBody bounds ordinary API bodies. These are small: filters and reasons.
const maxJSONBody = 64 << 10 // 64 KiB

// requestID assigns a correlation id to every request and echoes it back, so a
// UI error report can be tied to a server log line.
func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := strings.TrimSpace(c.GetHeader("X-Request-ID"))
		if id == "" || len(id) > 64 {
			id = store.NewID("req")
		}
		c.Set(requestIDKey, id)
		c.Writer.Header().Set("X-Request-ID", id)
		c.Next()
	}
}

// requestLogger emits one structured line per request.
//
// case_id and action_id are included when the route carries them, which is what
// makes the log searchable by the identifiers the audit trail uses (SRS 21.2,
// AC-005). Query strings are logged but bodies are not: a login body contains a
// password and a webhook body contains customer data.
func requestLogger(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		started := time.Now()
		c.Next()

		attrs := []any{
			slog.String("method", c.Request.Method),
			slog.String("path", c.FullPath()),
			slog.Int("status", c.Writer.Status()),
			slog.Int64("duration_ms", time.Since(started).Milliseconds()),
			slog.String("request_id", c.GetString(requestIDKey)),
		}
		if id := c.Param("id"); id != "" {
			attrs = append(attrs, slog.String("case_id", id))
		}
		if id := c.Param("caseId"); id != "" {
			attrs = append(attrs, slog.String("case_id", id))
		}
		// Set by handlers whose case id is not in the path — the webhook learns which
		// case it opened only after ingestion.
		if id := c.GetString(caseIDKey); id != "" {
			attrs = append(attrs, slog.String("case_id", id))
		}
		if id := c.GetString(actionIDKey); id != "" {
			attrs = append(attrs, slog.String("action_id", id))
		}
		if ident, ok := identityOf(c); ok {
			attrs = append(attrs, slog.String("actor", ident.Actor()))
		}
		if q := c.Request.URL.RawQuery; q != "" {
			attrs = append(attrs, slog.String("query", q))
		}

		switch {
		case len(c.Errors) > 0:
			attrs = append(attrs, slog.String("error", c.Errors.String()))
			log.Error("request failed", attrs...)
		case c.Writer.Status() >= 500:
			log.Error("request failed", attrs...)
		case c.Writer.Status() >= 400:
			log.Warn("request rejected", attrs...)
		default:
			log.Info("request", attrs...)
		}
	}
}

// recoverPanics turns a panic into a 500 without taking the process down. A
// panic in one request handler must not stop the recovery workers.
func recoverPanics(log *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("panic in handler",
					slog.Any("panic", r),
					slog.String("path", c.FullPath()),
					slog.String("request_id", c.GetString(requestIDKey)),
				)
				if !c.Writer.Written() {
					failWith(c, http.StatusInternalServerError, "internal_error", "an unexpected error occurred")
					return
				}
				c.Abort()
			}
		}()
		c.Next()
	}
}

// corsMiddleware allows the configured frontend origins only.
//
// The allow-list is explicit rather than reflecting the request's Origin header,
// because credentials are sent with these requests and a reflected origin would
// make every site a permitted caller.
func corsMiddleware(origins []string) gin.HandlerFunc {
	allowed := make(map[string]bool, len(origins))
	for _, o := range origins {
		allowed[strings.TrimRight(strings.TrimSpace(o), "/")] = true
	}
	return func(c *gin.Context) {
		origin := strings.TrimRight(c.GetHeader("Origin"), "/")
		if origin != "" && allowed[origin] {
			h := c.Writer.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-ID")
			h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			h.Set("Access-Control-Max-Age", "600")
			h.Add("Vary", "Origin")
		}
		if c.Request.Method == http.MethodOptions {
			// Answer the preflight without running the route's auth middleware:
			// a preflight carries no Authorization header by design.
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// securityHeaders sets the response headers that cost nothing and remove whole
// classes of browser-side problems. The API returns JSON only, so a document
// loaded from it should be inert.
func securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		c.Next()
	}
}

// limitBody caps the request body. Applied before JSON binding so an oversized
// body is refused rather than buffered.
func limitBody(max int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, max)
		}
		c.Next()
	}
}

// authenticate verifies the bearer token and attaches the caller's identity.
//
// It does not check the role. Authorization is a separate, per-route decision so
// that "who are you" and "may you do this" cannot be conflated.
func authenticate(issuer *auth.Issuer) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := auth.BearerToken(c.GetHeader("Authorization"))
		if token == "" {
			failWith(c, http.StatusUnauthorized, "unauthenticated", "a bearer token is required")
			return
		}
		ident, err := issuer.Verify(token)
		if err != nil {
			// The reason is deliberately not reported: expired, forged and
			// issued-elsewhere must be indistinguishable to the caller.
			failWith(c, http.StatusUnauthorized, "unauthenticated", "the token is not valid")
			return
		}
		c.Set(identityKey, ident)
		c.Next()
	}
}

// requireRole gates a route on a minimum role (SRS 15.1).
//
// Roles are ordered, so a route open to Operator is also open to Reviewer and
// Admin. Declaring the minimum rather than a set means adding a role cannot
// silently lock an existing route.
func requireRole(min domain.Role) gin.HandlerFunc {
	return func(c *gin.Context) {
		ident, ok := identityOf(c)
		if !ok {
			// Reached only if requireRole is mounted without authenticate, which
			// is a wiring bug. Fail closed rather than assume a caller.
			failWith(c, http.StatusUnauthorized, "unauthenticated", "a bearer token is required")
			return
		}
		if !ident.Permits(min) {
			failWith(c, http.StatusForbidden, "forbidden",
				"this operation requires the "+string(min)+" role")
			return
		}
		c.Next()
	}
}

// identityOf returns the authenticated caller, if any.
func identityOf(c *gin.Context) (auth.Identity, bool) {
	v, exists := c.Get(identityKey)
	if !exists {
		return auth.Identity{}, false
	}
	ident, ok := v.(auth.Identity)
	return ident, ok
}

// actorOf names the caller for an audit record, falling back to the system when
// the route is unauthenticated (the webhook).
func actorOf(c *gin.Context) string {
	if ident, ok := identityOf(c); ok {
		return ident.Actor()
	}
	return "system"
}
