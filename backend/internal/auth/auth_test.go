package auth

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// testSecret is 48 characters, comfortably over the 32-character floor New
// enforces.
const testSecret = "test-jwt-secret-for-ledgerflow-unit-tests-01234567"

var fixedNow = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

func newIssuer(t *testing.T) *Issuer {
	t.Helper()
	is, err := New(Config{Secret: testSecret, TTL: time.Hour, Issuer: "ledgerflow-test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	is.SetClock(func() time.Time { return fixedNow })
	return is
}

func testUser(role domain.Role) domain.User {
	return domain.User{
		ID:    "user-1",
		Email: "operator@ledgerflow.test",
		Name:  "Test Operator",
		Role:  role,
	}
}

// TestNewRejectsAWeakSecret is the boot-time guard from SRS 19.1. The signing key
// is the only thing between an unauthenticated request and full operator access, so
// a deployment configured with a weak or absent secret must refuse to start rather
// than start insecurely.
func TestNewRejectsAWeakSecret(t *testing.T) {
	weak := []string{
		"",
		"short",
		"changeme",
		strings.Repeat("a", 31),
		strings.Repeat(" ", 64),               // whitespace does not count as entropy
		"  " + strings.Repeat("a", 30) + "  ", // 30 real characters, padded
	}
	for _, secret := range weak {
		if _, err := New(Config{Secret: secret}); err == nil {
			t.Errorf("New accepted a %d-character secret %q", len(secret), secret)
		}
	}

	if _, err := New(Config{Secret: strings.Repeat("a", 32)}); err != nil {
		t.Errorf("New rejected a 32-character secret: %v", err)
	}
}

// TestNewDefaultsAreUsable covers the two optional fields. A zero TTL must become a
// bounded lifetime, never an unlimited one.
func TestNewDefaultsAreUsable(t *testing.T) {
	is, err := New(Config{Secret: testSecret})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if is.TTL() <= 0 {
		t.Fatalf("TTL = %v; a non-positive lifetime would mint tokens that never expire", is.TTL())
	}
	if is.TTL() > 24*time.Hour {
		t.Errorf("default TTL = %v, longer than a working day (SRS 15.1 asks for short-lived tokens)", is.TTL())
	}
	// The default issuer must still be non-empty, or the issuer check in Verify
	// would be vacuous.
	tok, err := is.Mint(testUser(domain.RoleOperator))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if _, err := is.Verify(tok.AccessToken); err != nil {
		t.Errorf("a token minted with default config did not verify: %v", err)
	}
}

// TestMintAndVerifyRoundTrip is the happy path: the identity that comes back is the
// one that went in, and nothing else.
func TestMintAndVerifyRoundTrip(t *testing.T) {
	is := newIssuer(t)
	for _, role := range domain.AllRoles {
		u := testUser(role)
		tok, err := is.Mint(u)
		if err != nil {
			t.Fatalf("Mint(%s): %v", role, err)
		}
		if tok.TokenType != "Bearer" {
			t.Errorf("TokenType = %q, want Bearer", tok.TokenType)
		}
		if tok.ExpiresIn != int(time.Hour.Seconds()) {
			t.Errorf("ExpiresIn = %d, want %d", tok.ExpiresIn, int(time.Hour.Seconds()))
		}
		if !tok.ExpiresAt.Equal(fixedNow.Add(time.Hour)) {
			t.Errorf("ExpiresAt = %s, want %s", tok.ExpiresAt, fixedNow.Add(time.Hour))
		}

		id, err := is.Verify(tok.AccessToken)
		if err != nil {
			t.Fatalf("Verify(%s): %v", role, err)
		}
		if id.UserID != u.ID || id.Email != u.Email || id.Name != u.Name || id.Role != role {
			t.Errorf("identity = %+v, want %+v", id, u)
		}
	}
}

// TestMintRefusesAnUnusableUser covers the two ways a caller can hand over a user
// that must not receive a token. An empty subject would authenticate as nobody; an
// unknown role would be checked against the role ordering and rank zero, which
// fails every route — but minting it at all would hide the misconfiguration until
// someone tried to use it.
func TestMintRefusesAnUnusableUser(t *testing.T) {
	is := newIssuer(t)

	if _, err := is.Mint(domain.User{Email: "x@y.z", Role: domain.RoleAdmin}); err == nil {
		t.Error("Mint issued a token for a user with no id")
	}
	for _, role := range []domain.Role{"", "superadmin", "OPERATOR", "root"} {
		u := testUser(role)
		if _, err := is.Mint(u); err == nil {
			t.Errorf("Mint issued a token for invalid role %q", role)
		}
	}
}

// TestVerifyRejectsAnExpiredToken pins the expiry behaviour that makes stateless
// tokens acceptable: a leaked token stops working on its own, because there is no
// session store to revoke it in.
func TestVerifyRejectsAnExpiredToken(t *testing.T) {
	is := newIssuer(t)
	tok, err := is.Mint(testUser(domain.RoleReviewer))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	// Still valid one second before expiry.
	is.SetClock(func() time.Time { return fixedNow.Add(time.Hour - time.Second) })
	if _, err := is.Verify(tok.AccessToken); err != nil {
		t.Errorf("token rejected before its expiry: %v", err)
	}

	// Invalid after it.
	for _, offset := range []time.Duration{time.Hour + time.Minute, 24 * time.Hour, 365 * 24 * time.Hour} {
		is.SetClock(func() time.Time { return fixedNow.Add(offset) })
		if _, err := is.Verify(tok.AccessToken); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("token %v past expiry returned %v, want ErrInvalidToken", offset, err)
		}
	}
}

// TestVerifyRejectsATokenFromTheFuture covers the nbf claim. A clock skew large
// enough to matter is a sign of a forged or replayed token, not of a slow server.
func TestVerifyRejectsATokenFromTheFuture(t *testing.T) {
	is := newIssuer(t)
	tok, err := is.Mint(testUser(domain.RoleOperator))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	is.SetClock(func() time.Time { return fixedNow.Add(-time.Hour) })
	if _, err := is.Verify(tok.AccessToken); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("a not-yet-valid token returned %v, want ErrInvalidToken", err)
	}
}

// TestVerifyRejectsTampering is the integrity property. Every byte of the token is
// covered by the signature, so no field can be edited in transit.
func TestVerifyRejectsTampering(t *testing.T) {
	is := newIssuer(t)
	tok, err := is.Mint(testUser(domain.RoleOperator))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	raw := tok.AccessToken
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3", len(parts))
	}

	tampered := map[string]string{
		"empty":                  "",
		"not a token":            "hello",
		"header only":            parts[0],
		"missing signature":      parts[0] + "." + parts[1],
		"empty signature":        parts[0] + "." + parts[1] + ".",
		"truncated signature":    raw[:len(raw)-4],
		"extended signature":     raw + "AAAA",
		"swapped segments":       parts[1] + "." + parts[0] + "." + parts[2],
		"extra segment":          raw + "." + parts[2],
		"flipped payload byte":   parts[0] + "." + flipLastChar(parts[1]) + "." + parts[2],
		"flipped signature byte": parts[0] + "." + parts[1] + "." + flipLastChar(parts[2]),
		"leading whitespace":     " " + raw,
		"trailing whitespace":    raw + " ",
	}
	for name, bad := range tampered {
		if _, err := is.Verify(bad); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: Verify returned %v, want ErrInvalidToken", name, err)
		}
	}
}

// TestVerifyRejectsPrivilegeEscalation is the attack the role claim invites: an
// operator edits "operator" to "admin" in the payload and keeps the signature.
//
// It has to fail on the signature, not on any check of the role's plausibility —
// the point is that the role is authenticated data, not a client-supplied hint.
func TestVerifyRejectsPrivilegeEscalation(t *testing.T) {
	is := newIssuer(t)
	tok, err := is.Mint(testUser(domain.RoleOperator))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	parts := strings.Split(tok.AccessToken, ".")

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if !strings.Contains(string(payload), `"role":"operator"`) {
		t.Fatalf("payload does not contain the expected role claim: %s", payload)
	}
	escalated := strings.Replace(string(payload), `"role":"operator"`, `"role":"admin"`, 1)
	forged := parts[0] + "." + base64.RawURLEncoding.EncodeToString([]byte(escalated)) + "." + parts[2]

	if id, err := is.Verify(forged); err == nil {
		t.Fatalf("an edited role claim verified successfully as %s", id.Role)
	}

	// The same escalation signed with the real secret does verify — which is why the
	// secret is the thing that must be protected, and why an invalid role is
	// rejected separately below.
	signed := signClaims(t, Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "user-1",
			Issuer:    "ledgerflow-test",
			IssuedAt:  jwt.NewNumericDate(fixedNow),
			NotBefore: jwt.NewNumericDate(fixedNow),
			ExpiresAt: jwt.NewNumericDate(fixedNow.Add(time.Hour)),
		},
		Email: "attacker@example.com",
		Role:  "superadmin",
	})
	if _, err := is.Verify(signed); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("a validly signed token with role %q returned %v, want ErrInvalidToken", "superadmin", err)
	}
}

// TestVerifyRejectsAnAlgorithmSwap covers the classic JWT flaw. A verifier that
// trusts the token's own alg header can be handed alg=none and will verify a token
// against nothing at all.
func TestVerifyRejectsAnAlgorithmSwap(t *testing.T) {
	is := newIssuer(t)
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "user-1",
			Issuer:    "ledgerflow-test",
			IssuedAt:  jwt.NewNumericDate(fixedNow),
			NotBefore: jwt.NewNumericDate(fixedNow),
			ExpiresAt: jwt.NewNumericDate(fixedNow.Add(time.Hour)),
		},
		Email: "attacker@example.com",
		Role:  domain.RoleAdmin,
	}

	// alg=none, no signature at all.
	unsigned, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).
		SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("build alg=none token: %v", err)
	}
	if _, err := is.Verify(unsigned); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("an alg=none token returned %v, want ErrInvalidToken", err)
	}

	// A different HMAC width, correctly signed with the real secret. Pinning the
	// method means only HS256 is accepted, so this is refused even though whoever
	// produced it held the key.
	hs512, err := jwt.NewWithClaims(jwt.SigningMethodHS512, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("build HS512 token: %v", err)
	}
	if _, err := is.Verify(hs512); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("an HS512 token returned %v, want ErrInvalidToken", err)
	}
}

// TestVerifyRejectsATokenFromAnotherDeployment is why the issuer claim is checked.
// A token minted by a staging instance that happens to share a secret must not be
// accepted in production, and vice versa.
func TestVerifyRejectsATokenFromAnotherDeployment(t *testing.T) {
	other, err := New(Config{Secret: testSecret, TTL: time.Hour, Issuer: "some-other-service"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	other.SetClock(func() time.Time { return fixedNow })
	tok, err := other.Mint(testUser(domain.RoleAdmin))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	if _, err := newIssuer(t).Verify(tok.AccessToken); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("a token from another issuer returned %v, want ErrInvalidToken", err)
	}
}

// TestVerifyRejectsAWrongSecret covers key rotation: tokens minted under the old
// key stop working, which is the intended effect of rotating it.
func TestVerifyRejectsAWrongSecret(t *testing.T) {
	is := newIssuer(t)
	tok, err := is.Mint(testUser(domain.RoleOperator))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	rotated, err := New(Config{
		Secret: "a-completely-different-secret-value-0123456789",
		TTL:    time.Hour,
		Issuer: "ledgerflow-test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rotated.SetClock(func() time.Time { return fixedNow })
	if _, err := rotated.Verify(tok.AccessToken); !errors.Is(err, ErrInvalidToken) {
		t.Errorf("a token signed with the old secret returned %v, want ErrInvalidToken", err)
	}
}

// TestVerifyErrorRevealsNothing is a deliberate ergonomics sacrifice. Every failure
// returns the same error, so the API cannot be used to learn whether a forged token
// was structurally correct or merely stale.
func TestVerifyErrorRevealsNothing(t *testing.T) {
	is := newIssuer(t)
	expired, err := is.Mint(testUser(domain.RoleOperator))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	is.SetClock(func() time.Time { return fixedNow.Add(48 * time.Hour) })

	_, expiredErr := is.Verify(expired.AccessToken)
	_, garbageErr := is.Verify("not.a.token")
	if expiredErr == nil || garbageErr == nil {
		t.Fatal("expected both to fail")
	}
	if expiredErr.Error() != garbageErr.Error() {
		t.Errorf("expiry and forgery return distinguishable errors:\n  expired: %v\n  garbage: %v",
			expiredErr, garbageErr)
	}
}

// TestHashPasswordAndCheckPassword covers the credential store. The hash must not
// contain the plaintext, must differ per call for the same input, and must not be
// verifiable against anything else.
func TestHashPasswordAndCheckPassword(t *testing.T) {
	const plain = "correct-horse-battery-staple"

	first, err := HashPassword(plain)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if strings.Contains(first, plain) {
		t.Fatal("the hash contains the plaintext password")
	}
	if !strings.HasPrefix(first, "$2") {
		t.Errorf("hash %q is not a bcrypt hash", first)
	}

	// Distinct salts: two hashes of the same password must differ, or an attacker
	// could tell which operators share a password.
	second, err := HashPassword(plain)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if first == second {
		t.Error("two hashes of the same password are identical; the salt is not random")
	}

	if !CheckPassword(first, plain) || !CheckPassword(second, plain) {
		t.Error("a correct password did not verify")
	}
	for _, wrong := range []string{
		"", "correct-horse-battery-stapl", "correct-horse-battery-staple ",
		"Correct-Horse-Battery-Staple", "wrong",
	} {
		if CheckPassword(first, wrong) {
			t.Errorf("wrong password %q verified", wrong)
		}
	}

	// An empty stored hash must never authenticate. A user row created without a
	// password would otherwise accept any input, or none.
	if CheckPassword("", plain) || CheckPassword("", "") {
		t.Error("an empty stored hash authenticated")
	}
	if CheckPassword("not-a-bcrypt-hash", plain) {
		t.Error("a malformed stored hash authenticated")
	}
}

// TestHashPasswordEnforcesLengthBounds covers both ends. The minimum is policy; the
// maximum exists because bcrypt silently ignores input past 72 bytes, which would
// make two different long passwords interchangeable.
func TestHashPasswordEnforcesLengthBounds(t *testing.T) {
	for _, short := range []string{"", "a", "short", strings.Repeat("x", MinPasswordLength-1)} {
		if _, err := HashPassword(short); !errors.Is(err, ErrWeakPassword) {
			t.Errorf("HashPassword(%d chars) returned %v, want ErrWeakPassword", len(short), err)
		}
	}
	if _, err := HashPassword(strings.Repeat("x", MinPasswordLength)); err != nil {
		t.Errorf("HashPassword rejected a password at the minimum length: %v", err)
	}
	if _, err := HashPassword(strings.Repeat("x", 72)); err != nil {
		t.Errorf("HashPassword rejected a 72-byte password: %v", err)
	}
	if _, err := HashPassword(strings.Repeat("x", 73)); err == nil {
		t.Error("HashPassword accepted a 73-byte password, which bcrypt would silently truncate")
	}
}

// TestAuthenticateDoesNotLeakAccountExistence is SRS 19.1 as a test. An unknown
// email and a wrong password must be indistinguishable to the caller, or the login
// endpoint becomes a way to enumerate operator accounts.
func TestAuthenticateDoesNotLeakAccountExistence(t *testing.T) {
	is := newIssuer(t)
	hash, err := HashPassword("a-real-password-here")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	u := testUser(domain.RoleOperator)
	u.PasswordHash = hash

	// Correct credentials mint a usable token.
	tok, err := is.Authenticate(&u, "a-real-password-here")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if _, err := is.Verify(tok.AccessToken); err != nil {
		t.Errorf("the minted token did not verify: %v", err)
	}

	_, wrongPassword := is.Authenticate(&u, "not-the-password")
	_, noSuchUser := is.Authenticate(nil, "not-the-password")

	if !errors.Is(wrongPassword, ErrInvalidCredentials) {
		t.Errorf("wrong password returned %v, want ErrInvalidCredentials", wrongPassword)
	}
	if !errors.Is(noSuchUser, ErrInvalidCredentials) {
		t.Errorf("unknown user returned %v, want ErrInvalidCredentials", noSuchUser)
	}
	if wrongPassword.Error() != noSuchUser.Error() {
		t.Errorf("the two failures are distinguishable:\n  wrong password: %v\n  no such user: %v",
			wrongPassword, noSuchUser)
	}

	// A user row with no password hash must not be able to log in with an empty
	// password, which is the shape a seeding bug would produce.
	noPassword := testUser(domain.RoleAdmin)
	if _, err := is.Authenticate(&noPassword, ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("a user with no password hash returned %v, want ErrInvalidCredentials", err)
	}
}

// TestBearerToken covers the header parser. Everything that is not exactly a bearer
// credential must yield an empty string, so a malformed header cannot be mistaken
// for a token.
func TestBearerToken(t *testing.T) {
	tests := map[string]string{
		"Bearer abc123":     "abc123",
		"bearer abc123":     "abc123", // RFC 7235: the scheme is case-insensitive
		"BEARER abc123":     "abc123",
		"BeArEr abc123":     "abc123",
		"Bearer   abc123  ": "abc123", // surrounding whitespace is stripped
		"":                  "",
		"abc123":            "",
		"Bearer":            "",
		"Bearer ":           "",
		"Basic abc123":      "",
		"Token abc123":      "",
		"Bearerabc123":      "",
		" Bearer abc123":    "", // the scheme must start the header
	}
	for header, want := range tests {
		if got := BearerToken(header); got != want {
			t.Errorf("BearerToken(%q) = %q, want %q", header, got, want)
		}
	}
}

// TestRoleOrdering pins the three-level ordering from SRS 15.1. Routes declare a
// minimum role, so the ordering is the authorization model: getting it wrong either
// locks admins out of operator routes or lets operators into admin ones.
func TestRoleOrdering(t *testing.T) {
	permits := map[domain.Role][]domain.Role{
		domain.RoleOperator: {domain.RoleOperator},
		domain.RoleReviewer: {domain.RoleOperator, domain.RoleReviewer},
		domain.RoleAdmin:    {domain.RoleOperator, domain.RoleReviewer, domain.RoleAdmin},
	}
	for have, allowed := range permits {
		for _, required := range domain.AllRoles {
			want := false
			for _, a := range allowed {
				if a == required {
					want = true
				}
			}
			id := Identity{Role: have}
			if got := id.Permits(required); got != want {
				t.Errorf("%s accessing a %s route = %v, want %v", have, required, got, want)
			}
		}
	}

	// An unknown role must satisfy nothing. This is the fail-closed direction: a
	// role added to the database but not to the enum gets no access, rather than
	// being treated as permissive.
	unknown := Identity{Role: "superadmin"}
	for _, required := range domain.AllRoles {
		if unknown.Permits(required) {
			t.Errorf("unknown role satisfied the %s requirement", required)
		}
	}
	if (Identity{}).Permits(domain.RoleOperator) {
		t.Error("an empty identity satisfied the operator requirement")
	}
}

// TestIdentityActor covers the string that lands in the audit trail. It has to be
// human-readable without a join, or the trail does not get read (SRS FR-052).
func TestIdentityActor(t *testing.T) {
	tests := []struct {
		id   Identity
		want string
	}{
		{Identity{Email: "ops@ledgerflow.test", UserID: "user-1"}, "ops@ledgerflow.test"},
		{Identity{UserID: "user-1"}, "user-1"},
		{Identity{}, "anonymous"},
	}
	for _, tc := range tests {
		if got := tc.id.Actor(); got != tc.want {
			t.Errorf("Actor() for %+v = %q, want %q", tc.id, got, tc.want)
		}
	}
}

// signClaims signs claims with the real test secret, for forging tokens that are
// cryptographically valid but semantically wrong.
func signClaims(t *testing.T, claims Claims) string {
	t.Helper()
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	if err != nil {
		t.Fatalf("sign claims: %v", err)
	}
	return signed
}

// flipLastChar changes the final character of a base64url segment, which is enough
// to invalidate the signature without changing the token's shape.
func flipLastChar(s string) string {
	if s == "" {
		return "A"
	}
	last := s[len(s)-1]
	replacement := byte('A')
	if last == 'A' {
		replacement = 'B'
	}
	return s[:len(s)-1] + string(replacement)
}
