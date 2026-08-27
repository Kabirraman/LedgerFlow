package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ledgerflow/ledgerflow/internal/domain"
	"github.com/ledgerflow/ledgerflow/internal/store"
)

// Router-level tests for authentication and role gating (SRS 15.1, 22.2).
//
// The property under test is that authorisation is enforced by the router rather
// than by the handlers. A handler that forgets its own check is a plausible bug; a
// route registered in the wrong group is a visible one. So these tests drive real
// HTTP requests with real tokens and assert the boundary route by route, which is
// the only way to catch a route that was moved into the wrong group.

// login exchanges credentials for a bearer token through the real endpoint.
func (h *harness) login(email, password string) (int, string) {
	h.t.Helper()
	body, err := json.Marshal(map[string]string{"email": email, "password": password})
	if err != nil {
		h.t.Fatalf("marshal login: %v", err)
	}
	resp, err := h.srv.Client().Post(h.srv.URL+"/api/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		h.t.Fatalf("post login: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, ""
	}
	var decoded struct {
		Token struct {
			AccessToken string `json:"access_token"`
			TokenType   string `json:"token_type"`
			ExpiresIn   int    `json:"expires_in"`
		} `json:"token"`
		User struct {
			UserID string      `json:"user_id"`
			Email  string      `json:"email"`
			Role   domain.Role `json:"role"`
		} `json:"user"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		h.t.Fatalf("decode login response %q: %v", raw, err)
	}
	return resp.StatusCode, decoded.Token.AccessToken
}

// do sends an authenticated request. An empty token omits the header entirely.
func (h *harness) do(method, path, token string, body any) (int, map[string]any) {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, reader)
	if err != nil {
		h.t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return resp.StatusCode, decoded
}

// seedUsers adds one account per role and returns their tokens.
func (h *harness) seedUsers() (operator, reviewer, admin string) {
	h.t.Helper()
	h.store.addUser("operator@ledgerflow.test", "operator-password", domain.RoleOperator)
	h.store.addUser("reviewer@ledgerflow.test", "reviewer-password", domain.RoleReviewer)
	h.store.addUser("admin@ledgerflow.test", "admin-password", domain.RoleAdmin)

	for _, tc := range []struct {
		email, password string
		into            *string
	}{
		{"operator@ledgerflow.test", "operator-password", &operator},
		{"reviewer@ledgerflow.test", "reviewer-password", &reviewer},
		{"admin@ledgerflow.test", "admin-password", &admin},
	} {
		status, token := h.login(tc.email, tc.password)
		if status != http.StatusOK {
			h.t.Fatalf("login %s: status = %d, want 200", tc.email, status)
		}
		if token == "" {
			h.t.Fatalf("login %s returned no token", tc.email)
		}
		*tc.into = token
	}
	return operator, reviewer, admin
}

func TestLoginIssuesTokenAndRejectsBadCredentials(t *testing.T) {
	h := newHarness(t, testWebhookSecret)
	h.store.addUser("operator@ledgerflow.test", "correct-horse-battery", domain.RoleOperator)

	status, token := h.login("operator@ledgerflow.test", "correct-horse-battery")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if token == "" {
		t.Fatal("no access token issued")
	}
	// Three segments: a JWT, not an opaque string the middleware would have to
	// look up.
	if n := strings.Count(token, "."); n != 2 {
		t.Errorf("token has %d dots, want 2 (a JWS compact serialization)", n+1)
	}

	// The address is normalized, so a capitalised login is the same account.
	if s, _ := h.login("OPERATOR@LedgerFlow.TEST", "correct-horse-battery"); s != http.StatusOK {
		t.Errorf("status for a differently-cased email = %d, want 200", s)
	}

	rejected := []struct {
		name, email, password string
		want                  int
	}{
		{"wrong password", "operator@ledgerflow.test", "wrong", http.StatusUnauthorized},
		{"unknown account", "nobody@ledgerflow.test", "anything", http.StatusUnauthorized},
		{"empty password", "operator@ledgerflow.test", "", http.StatusBadRequest},
		{"empty email", "", "correct-horse-battery", http.StatusBadRequest},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			if s, _ := h.login(tc.email, tc.password); s != tc.want {
				t.Errorf("status = %d, want %d", s, tc.want)
			}
		})
	}

	// A wrong password and an unknown account must be indistinguishable to the
	// caller, or login becomes an account-enumeration oracle (SRS 19.1).
	var bodies []string
	for _, email := range []string{"operator@ledgerflow.test", "nobody@ledgerflow.test"} {
		_, decoded := h.do(http.MethodPost, "/api/auth/login", "",
			map[string]string{"email": email, "password": "definitely-wrong"})
		raw, _ := json.Marshal(decoded)
		bodies = append(bodies, string(raw))
	}
	if bodies[0] != bodies[1] {
		t.Errorf("a wrong password and an unknown account produce different responses:\n  %s\n  %s",
			bodies[0], bodies[1])
	}

	// Every failed attempt is audited, including the one against an account that
	// does not exist (SRS 21.2).
	failures := 0
	for _, e := range h.store.auditEvents() {
		if e == "login_failed" {
			failures++
		}
	}
	if failures < 2 {
		t.Errorf("login_failed audit records = %d, want at least 2; events = %v",
			failures, h.store.auditEvents())
	}
}

func TestAuthenticatedRoutesRequireABearerToken(t *testing.T) {
	h := newHarness(t, testWebhookSecret)
	operator, _, _ := h.seedUsers()

	// Every authenticated route, exercised with no token at all.
	protected := []struct{ method, path string }{
		{http.MethodGet, "/api/auth/me"},
		{http.MethodGet, "/api/dashboard/summary"},
		{http.MethodGet, "/api/cases"},
		{http.MethodGet, "/api/cases/case_0001"},
		{http.MethodPost, "/api/cases/case_0001/reanalyze"},
		{http.MethodPost, "/api/cases/case_0001/verify"},
		{http.MethodPost, "/api/cases/case_0001/approve"},
		{http.MethodPost, "/api/cases/case_0001/reject"},
		{http.MethodGet, "/api/approvals"},
		{http.MethodGet, "/api/audit/case_0001"},
		{http.MethodGet, "/api/analytics/strategies"},
		{http.MethodGet, "/api/ops/metrics"},
		{http.MethodGet, "/api/ops/events"},
		{http.MethodGet, "/api/simulations"},
		{http.MethodGet, "/api/simulations/run_0001"},
		{http.MethodPost, "/api/simulations/run"},
		{http.MethodGet, "/api/datasets"},
		{http.MethodGet, "/api/policies"},
		{http.MethodPut, "/api/policies"},
		{http.MethodPost, "/api/sync/payments"},
		{http.MethodPost, "/api/demo/checkout"},
		{http.MethodPost, "/api/demo/checkout/chk_0001/activity"},
		{http.MethodPost, "/api/demo/checkout/chk_0001/abandon"},
		{http.MethodPost, "/api/demo/checkout/chk_0001/convert"},
	}
	for _, r := range protected {
		status, _ := h.do(r.method, r.path, "", nil)
		if status != http.StatusUnauthorized {
			t.Errorf("%s %s with no token = %d, want 401", r.method, r.path, status)
		}
	}

	// Malformed and forged tokens are rejected the same way.
	bad := []struct{ name, header string }{
		{"empty bearer", "Bearer "},
		{"not a JWT", "Bearer not-a-token"},
		{"a JWT signed with another key", "Bearer " + forgeToken(t)},
		{"the token with its signature stripped", "Bearer " + strings.Join(strings.Split(operator, ".")[:2], ".")},
		{"a tampered payload", "Bearer " + tamperToken(operator)},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, h.srv.URL+"/api/auth/me", nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			req.Header.Set("Authorization", tc.header)
			resp, err := h.srv.Client().Do(req)
			if err != nil {
				t.Fatalf("get me: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
		})
	}

	// And the valid token works, so the assertions above are about the token and
	// not about the route being broken for everyone.
	if status, _ := h.do(http.MethodGet, "/api/auth/me", operator, nil); status != http.StatusOK {
		t.Errorf("GET /api/auth/me with a valid operator token = %d, want 200", status)
	}
}

// forgeToken mints a structurally valid JWT signed with a different secret.
func forgeToken(t *testing.T) string {
	t.Helper()
	header := b64url(`{"alg":"HS256","typ":"JWT"}`)
	claims := b64url(fmt.Sprintf(`{"sub":"usr_0001","email":"attacker@example.com","role":"admin","exp":%d}`,
		time.Now().Add(time.Hour).Unix()))
	// A signature over the right bytes with the wrong key. Verification must fail
	// on the key, not on the shape.
	return header + "." + claims + "." + b64url("not-the-real-signature")
}

// tamperToken flips a character in the payload segment, leaving the signature
// intact. A verifier that decoded the claims before checking the MAC would accept
// this.
func tamperToken(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(parts[1]) == 0 {
		return token
	}
	body := []byte(parts[1])
	if body[0] == 'e' {
		body[0] = 'f'
	} else {
		body[0] = 'e'
	}
	parts[1] = string(body)
	return strings.Join(parts, ".")
}

func b64url(s string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var out strings.Builder
	data := []byte(s)
	for i := 0; i < len(data); i += 3 {
		var chunk [3]byte
		n := copy(chunk[:], data[i:])
		v := uint32(chunk[0])<<16 | uint32(chunk[1])<<8 | uint32(chunk[2])
		out.WriteByte(alphabet[(v>>18)&0x3f])
		out.WriteByte(alphabet[(v>>12)&0x3f])
		if n > 1 {
			out.WriteByte(alphabet[(v>>6)&0x3f])
		}
		if n > 2 {
			out.WriteByte(alphabet[v&0x3f])
		}
	}
	return out.String()
}

// TestRoleGatingIsEnforcedByTheRouter is the core authorisation test (SRS 15.1).
//
// Roles are ordered — operator < reviewer < admin — and every route declares a
// minimum. So the assertion is two-sided for each route: below the minimum must be
// 403, and at or above it must not be 401 or 403. Asserting only the reject side
// would pass a router that rejected everyone.
func TestRoleGatingIsEnforcedByTheRouter(t *testing.T) {
	h := newHarness(t, testWebhookSecret)
	operator, reviewer, admin := h.seedUsers()

	routes := []struct {
		method, path string
		minimum      domain.Role
		body         any
	}{
		{http.MethodGet, "/api/auth/me", domain.RoleOperator, nil},
		{http.MethodGet, "/api/dashboard/summary", domain.RoleOperator, nil},
		{http.MethodGet, "/api/cases", domain.RoleOperator, nil},
		{http.MethodGet, "/api/analytics/strategies", domain.RoleOperator, nil},
		{http.MethodGet, "/api/ops/metrics", domain.RoleOperator, nil},
		{http.MethodGet, "/api/ops/events", domain.RoleOperator, nil},
		{http.MethodGet, "/api/simulations", domain.RoleOperator, nil},
		{http.MethodGet, "/api/datasets", domain.RoleOperator, nil},

		{http.MethodGet, "/api/approvals", domain.RoleReviewer, nil},
		{http.MethodGet, "/api/audit/case_missing", domain.RoleReviewer, nil},
		{http.MethodPost, "/api/cases/case_missing/approve", domain.RoleReviewer, map[string]string{"note": "ok"}},
		{http.MethodPost, "/api/cases/case_missing/reject", domain.RoleReviewer, map[string]string{"reason": "not now"}},

		{http.MethodGet, "/api/policies", domain.RoleAdmin, nil},
		{http.MethodPost, "/api/sync/payments", domain.RoleAdmin, nil},
	}

	tokens := map[domain.Role]string{
		domain.RoleOperator: operator,
		domain.RoleReviewer: reviewer,
		domain.RoleAdmin:    admin,
	}

	for _, r := range routes {
		for _, role := range []domain.Role{domain.RoleOperator, domain.RoleReviewer, domain.RoleAdmin} {
			status, decoded := h.do(r.method, r.path, tokens[role], r.body)
			permitted := role.Permits(r.minimum)

			switch {
			case !permitted && status != http.StatusForbidden:
				t.Errorf("%s %s as %s = %d, want 403 (route requires %s); body = %v",
					r.method, r.path, role, status, r.minimum, decoded)
			case permitted && (status == http.StatusForbidden || status == http.StatusUnauthorized):
				// A permitted caller may still get a 404 or a 503 — the route's own
				// business logic runs. What must never happen is an authorisation
				// refusal.
				t.Errorf("%s %s as %s = %d, want anything but 401/403 (route requires %s); body = %v",
					r.method, r.path, role, status, r.minimum, decoded)
			}
		}
	}
}

func TestRoleOrderingIsMonotonic(t *testing.T) {
	// The gating test above depends on this ordering being what the router means
	// by "minimum role", so it is worth pinning directly (SRS 15.1).
	cases := []struct {
		holder, required domain.Role
		want             bool
	}{
		{domain.RoleOperator, domain.RoleOperator, true},
		{domain.RoleOperator, domain.RoleReviewer, false},
		{domain.RoleOperator, domain.RoleAdmin, false},
		{domain.RoleReviewer, domain.RoleOperator, true},
		{domain.RoleReviewer, domain.RoleReviewer, true},
		{domain.RoleReviewer, domain.RoleAdmin, false},
		{domain.RoleAdmin, domain.RoleOperator, true},
		{domain.RoleAdmin, domain.RoleReviewer, true},
		{domain.RoleAdmin, domain.RoleAdmin, true},
	}
	for _, tc := range cases {
		if got := tc.holder.Permits(tc.required); got != tc.want {
			t.Errorf("%s.Permits(%s) = %v, want %v", tc.holder, tc.required, got, tc.want)
		}
	}
}

// TestMeReportsPermissionsMatchingTheRole checks the hint the frontend uses to
// hide controls. It is a convenience only — the server re-checks every request —
// but a wrong hint would show a reviewer an admin screen that then 403s.
func TestMeReportsPermissionsMatchingTheRole(t *testing.T) {
	h := newHarness(t, testWebhookSecret)
	operator, reviewer, admin := h.seedUsers()

	cases := []struct {
		role                            domain.Role
		token                           string
		canOperate, canReview, canAdmin bool
	}{
		{domain.RoleOperator, operator, true, false, false},
		{domain.RoleReviewer, reviewer, true, true, false},
		{domain.RoleAdmin, admin, true, true, true},
	}

	for _, tc := range cases {
		status, decoded := h.do(http.MethodGet, "/api/auth/me", tc.token, nil)
		if status != http.StatusOK {
			t.Fatalf("GET /api/auth/me as %s = %d", tc.role, status)
		}
		user, _ := decoded["user"].(map[string]any)
		if user == nil {
			t.Fatalf("no user object for %s: %v", tc.role, decoded)
		}
		if got := user["role"]; got != string(tc.role) {
			t.Errorf("role = %v, want %s", got, tc.role)
		}
		// The password hash must never leave the server, whatever the role.
		if _, leaked := user["password_hash"]; leaked {
			t.Error("the password hash is present in the /auth/me response")
		}

		perms, _ := decoded["permissions"].(map[string]any)
		if perms == nil {
			t.Fatalf("no permissions object for %s: %v", tc.role, decoded)
		}
		for name, want := range map[string]bool{
			"can_operate": tc.canOperate,
			"can_review":  tc.canReview,
			"can_admin":   tc.canAdmin,
		} {
			if got := perms[name]; got != want {
				t.Errorf("%s.%s = %v, want %v", tc.role, name, got, want)
			}
		}
	}
}

// --- transport-level behaviour ---

// errorCode reads the code out of the API's single error envelope,
// {"error":{"code":...,"message":...}}.
func errorCode(body map[string]any) string {
	detail, _ := body["error"].(map[string]any)
	code, _ := detail["code"].(string)
	return code
}

func TestRouterRejectsUnknownRoutesAndMethods(t *testing.T) {
	h := newHarness(t, testWebhookSecret)
	_, _, admin := h.seedUsers()

	status, decoded := h.do(http.MethodGet, "/api/does-not-exist", admin, nil)
	if status != http.StatusNotFound {
		t.Errorf("unknown route = %d, want 404", status)
	}
	if code := errorCode(decoded); code != "not_found" {
		// A JSON error body rather than gin's default plain text, so the frontend
		// has one shape to handle for every failure.
		t.Errorf("code = %q, want not_found; body = %v", code, decoded)
	}

	status, decoded = h.do(http.MethodDelete, "/api/policies", admin, nil)
	if status != http.StatusMethodNotAllowed {
		t.Errorf("DELETE /api/policies = %d, want 405", status)
	}
	if code := errorCode(decoded); code != "method_not_allowed" {
		t.Errorf("code = %q, want method_not_allowed; body = %v", code, decoded)
	}

	// A trailing slash is not a silent redirect: /api/cases/ would otherwise 301
	// to /api/cases and drop the Authorization header on some clients.
	status, _ = h.do(http.MethodGet, "/api/cases/", admin, nil)
	if status == http.StatusMovedPermanently || status == http.StatusTemporaryRedirect {
		t.Errorf("GET /api/cases/ = %d, want no redirect", status)
	}
}

func TestSecurityHeadersArePresent(t *testing.T) {
	h := newHarness(t, testWebhookSecret)

	resp, err := h.srv.Client().Get(h.srv.URL + "/api/health")
	if err != nil {
		t.Fatalf("get health: %v", err)
	}
	defer resp.Body.Close()

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}
	for header, value := range want {
		if got := resp.Header.Get(header); got != value {
			t.Errorf("%s = %q, want %q", header, got, value)
		}
	}
	// A request id on every response, so a user reporting a failure can quote
	// something that finds the log line (SRS 21.2).
	if resp.Header.Get("X-Request-ID") == "" {
		t.Error("no X-Request-ID header")
	}
}

func TestCORSAllowsOnlyConfiguredOrigins(t *testing.T) {
	h := newHarness(t, testWebhookSecret)

	// The configured origin is echoed, never "*": with credentials in play a
	// wildcard would let any page read an authenticated response.
	req, err := http.NewRequest(http.MethodOptions, h.srv.URL+"/api/cases", nil)
	if err != nil {
		t.Fatalf("build preflight: %v", err)
	}
	req.Header.Set("Origin", "http://localhost:3000")
	req.Header.Set("Access-Control-Request-Method", "GET")
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:3000" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the configured origin", got)
	}

	// An unconfigured origin gets no allow header, so the browser blocks the read.
	req2, err := http.NewRequest(http.MethodOptions, h.srv.URL+"/api/cases", nil)
	if err != nil {
		t.Fatalf("build preflight: %v", err)
	}
	req2.Header.Set("Origin", "https://evil.example.com")
	req2.Header.Set("Access-Control-Request-Method", "GET")
	resp2, err := h.srv.Client().Do(req2)
	if err != nil {
		t.Fatalf("preflight: %v", err)
	}
	defer resp2.Body.Close()
	if got := resp2.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q for an unconfigured origin, want empty", got)
	}
}

func TestOversizedBodiesAreRefused(t *testing.T) {
	h := newHarness(t, testWebhookSecret)
	_, _, admin := h.seedUsers()

	// A JSON body past maxJSONBody. The limit exists so an unauthenticated caller
	// cannot make the process allocate without bound (SRS NFR-013).
	huge := map[string]string{"note": strings.Repeat("x", maxJSONBody+1024)}
	status, _ := h.do(http.MethodPut, "/api/policies", admin, huge)
	if status == http.StatusOK {
		t.Error("an oversized policy body was accepted")
	}
	if status/100 != 4 {
		t.Errorf("status = %d, want a 4xx for an oversized body", status)
	}

	// The webhook has its own, larger limit, and a body past it is refused before
	// signature verification — there is nothing to verify if the bytes are
	// truncated.
	oversized := bytes.Repeat([]byte("a"), maxWebhookBody+1024)
	resp, _ := h.postWebhook(oversized, testWebhookSecret, "evt_huge")
	if resp.StatusCode/100 != 4 {
		t.Errorf("oversized webhook = %d, want a 4xx", resp.StatusCode)
	}
	if n := h.store.casesOpened(); n != 0 {
		t.Errorf("cases opened = %d, want 0", n)
	}
}

// --- capability reporting ---

func TestHealthAndVersionAreHonestAboutConfiguration(t *testing.T) {
	h := newHarness(t, testWebhookSecret)

	status, decoded := h.do(http.MethodGet, "/api/health", "", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /api/health = %d, want 200", status)
	}
	if got := decoded["status"]; got != "ok" {
		t.Errorf("status = %v, want ok", got)
	}

	status, decoded = h.do(http.MethodGet, "/api/version", "", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /api/version = %d, want 200", status)
	}

	// The version endpoint states which mode the deployment is in and which
	// integrations are actually wired. A demo audience is entitled to know whether
	// the numbers on screen came from Razorpay or from a sandbox, and whether the
	// agents ran on a model or on their fallbacks (SRS 24.3, 25.2).
	for _, key := range []string{"version", "environment", "razorpay_mode"} {
		if _, present := decoded[key]; !present {
			t.Errorf("GET /api/version omits %q; body = %v", key, decoded)
		}
	}
	if got := decoded["razorpay_mode"]; got != "test" {
		t.Errorf("razorpay_mode = %v, want test — nothing in this build may report live", got)
	}
}

// TestOpsMetricsReportsTheCountersWeIncrement ties the ops screen to the counters
// the ingestor writes (SRS 21.3).
//
// This route serves the raw counter map rather than a fixed struct, so a counter
// added later shows up without a change here. The shaped SRS 18.3 view — with the
// derived rates — rides on the dashboard summary instead, which is where a number
// that needs a denominator belongs.
func TestOpsMetricsReportsTheCountersWeIncrement(t *testing.T) {
	h := newHarness(t, testWebhookSecret)
	operator, _, _ := h.seedUsers()

	// Two accepted deliveries and one forged one, so all three counters are
	// non-zero and distinguishable.
	for i, id := range []string{"evt_metrics_1", "evt_metrics_2"} {
		body := paymentFailedBody(fmt.Sprintf("pay_METRICS%04d", i), "metrics@example.com",
			int64(600_000+i*10_000), time.Now().UTC())
		if resp, decoded := h.postWebhook(body, testWebhookSecret, id); resp.StatusCode != http.StatusOK {
			t.Fatalf("delivery %s = %d; body = %v", id, resp.StatusCode, decoded)
		}
	}
	replay := paymentFailedBody("pay_METRICS0000", "metrics@example.com", 600_000, time.Now().UTC())
	h.postWebhook(replay, testWebhookSecret, "evt_metrics_1")
	h.postWebhookRaw(replay, strings.Repeat("cd", 32), "evt_metrics_forged")

	status, decoded := h.do(http.MethodGet, "/api/ops/metrics", operator, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /api/ops/metrics = %d, want 200; body = %v", status, decoded)
	}
	if _, present := decoded["as_of"]; !present {
		t.Error("the metrics response carries no as_of timestamp, so a stale screen looks current")
	}

	counters, _ := decoded["counters"].(map[string]any)
	if counters == nil {
		t.Fatalf("no counters object; body = %v", decoded)
	}
	count := func(key string) float64 {
		entry, _ := counters[key].(map[string]any)
		if entry == nil {
			t.Errorf("no counter %q; counters = %v", key, counters)
			return 0
		}
		v, _ := entry["count"].(float64)
		return v
	}

	if got := count("webhooks_received"); got != 4 {
		t.Errorf("webhooks_received = %v, want 4", got)
	}
	if got := count("duplicate_events"); got != 1 {
		t.Errorf("duplicate_events = %v, want 1", got)
	}
	if got := count("webhook_signature_failures"); got != 1 {
		t.Errorf("webhook_signature_failures = %v, want 1", got)
	}
}

// TestPolicyLimitsAreServedForTheAdminForm checks that the bounds the API
// publishes are the bounds the API enforces.
//
// The admin screen renders its inputs from these, so a published maximum that
// differed from the validated one would let an operator save a value the server
// then rejects — or, worse, suggest a ceiling that is not real (SRS 10.1, 16.6).
func TestPolicyLimitsAreServedForTheAdminForm(t *testing.T) {
	h := newHarness(t, testWebhookSecret)
	_, _, admin := h.seedUsers()

	status, decoded := h.do(http.MethodGet, "/api/policies", admin, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /api/policies = %d, want 200; body = %v", status, decoded)
	}

	limits, _ := decoded["limits"].(map[string]any)
	if limits == nil {
		t.Fatalf("GET /api/policies published no limits object; body = %v", decoded)
	}

	expected := store.PolicyLimits()
	if len(limits) != len(expected) {
		t.Errorf("published limits = %d fields, want %d", len(limits), len(expected))
	}
	for field, bound := range expected {
		published, _ := limits[field].(map[string]any)
		if published == nil {
			t.Errorf("no published bound for %q", field)
			continue
		}
		if got, _ := published["min"].(float64); got != bound.Min {
			t.Errorf("%s min = %v, want %v", field, got, bound.Min)
		}
		switch {
		case bound.Max == nil:
			// An unbounded field publishes a null maximum rather than a sentinel
			// number, so the form renders no upper stop instead of a fake one.
			if v, present := published["max"]; present && v != nil {
				t.Errorf("%s max = %v, want null for an unbounded field", field, v)
			}
		default:
			got, ok := published["max"].(float64)
			if !ok {
				t.Errorf("%s max = %v, want %v", field, published["max"], *bound.Max)
				continue
			}
			if got != *bound.Max {
				t.Errorf("%s max = %v, want %v", field, got, *bound.Max)
			}
		}
	}
}
