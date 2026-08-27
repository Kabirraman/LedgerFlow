package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/ledgerflow/ledgerflow/internal/auth"
	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// loginRequest is the login body. Nothing else is accepted — a role in the
// request would be a privilege-escalation vector, so the role is read from the
// user record and never from the client (SRS 15.1).
type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginResponse struct {
	Token auth.Token    `json:"token"`
	User  auth.Identity `json:"user"`
}

// login exchanges credentials for an access token.
//
// A missing account and a wrong password produce the same response and take the
// same time: the lookup failure is carried into Authenticate as a nil user, which
// still runs a bcrypt comparison against a dummy hash. Short-circuiting here would
// turn login latency into an account-enumeration oracle.
func (s *Server) login(c *gin.Context) {
	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		failWith(c, http.StatusBadRequest, "invalid_body", "expected a JSON object with email and password")
		return
	}

	email := strings.ToLower(strings.TrimSpace(req.Email))
	details := map[string]string{}
	if email == "" {
		details["email"] = "an email address is required"
	}
	if req.Password == "" {
		details["password"] = "a password is required"
	}
	if len(details) > 0 {
		failValidation(c, details)
		return
	}

	user, err := s.deps.Store.FindUserByEmail(c.Request.Context(), email)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		fail(c, err)
		return
	}

	token, err := s.deps.Issuer.Authenticate(user, req.Password)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			// One audit line per failed attempt, keyed on the submitted address
			// rather than a user id, so attempts against non-existent accounts are
			// visible too (SRS 21.2).
			_ = s.deps.Store.Audit(c.Request.Context(), email, "user", "", "", "login_failed",
				map[string]any{"email": email, "ip": c.ClientIP()})
			failWith(c, http.StatusUnauthorized, "invalid_credentials", "the email or password is incorrect")
			return
		}
		fail(c, err)
		return
	}

	ident := auth.Identity{UserID: user.ID, Email: user.Email, Name: user.Name, Role: user.Role}
	_ = s.deps.Store.Audit(c.Request.Context(), ident.Actor(), "user", user.ID, "", "login",
		map[string]any{"role": user.Role, "ip": c.ClientIP()})

	ok(c, loginResponse{Token: token, User: ident})
}

// me returns the caller's own identity, so a reloaded frontend can restore its
// session state without decoding the token itself.
func (s *Server) me(c *gin.Context) {
	ident, exists := identityOf(c)
	if !exists {
		failWith(c, http.StatusUnauthorized, "unauthenticated", "a bearer token is required")
		return
	}
	ok(c, gin.H{
		"user": ident,
		// The frontend hides controls the caller cannot use. It is a convenience,
		// not a control: the server re-checks the role on every request.
		"permissions": gin.H{
			"can_review":  ident.Permits(domain.RoleReviewer),
			"can_admin":   ident.Permits(domain.RoleAdmin),
			"can_operate": ident.Permits(domain.RoleOperator),
		},
	})
}
