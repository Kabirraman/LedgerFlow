// Package auth implements operator authentication for the internal API
// (SRS 15.1, 19.1).
//
// Two decisions here are worth stating plainly, because both trade convenience
// for safety:
//
//   - Tokens are short-lived and stateless. There is no server-side session store
//     to lose on restart, and a leaked token expires on its own. The cost is that
//     revocation is not instant; for an internal tool with a small operator set,
//     that is the right trade.
//   - Passwords are bcrypt-hashed with a work factor, never stored or logged in any
//     recoverable form. The plaintext exists only inside one function call.
//
// Authorization is a three-level ordering — operator < reviewer < admin — so a
// route declares the *minimum* role it needs rather than enumerating who may call
// it. Enumeration is how a new role silently gains access to everything.
package auth

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// Errors callers distinguish. Login deliberately reports the same failure for an
// unknown email and a wrong password, so the API cannot be used to enumerate which
// addresses have accounts.
var (
	// ErrInvalidCredentials covers both an unknown user and a bad password.
	ErrInvalidCredentials = errors.New("auth: invalid email or password")
	// ErrInvalidToken covers a malformed, mis-signed or expired token.
	ErrInvalidToken = errors.New("auth: invalid or expired token")
	// ErrForbidden means the caller is authenticated but not permitted.
	ErrForbidden = errors.New("auth: insufficient role")
	// ErrWeakPassword rejects a password below the minimum length.
	ErrWeakPassword = errors.New("auth: password must be at least 10 characters")
)

// MinPasswordLength is enforced on every password this system creates. It is not a
// substitute for the operator choosing well, but it removes the worst cases.
const MinPasswordLength = 10

// bcryptCost is the work factor. bcrypt.DefaultCost (10) is the floor for anything
// holding real credentials; it is named here so the choice is visible rather than
// inherited silently.
const bcryptCost = bcrypt.DefaultCost + 2

// HashPassword produces a bcrypt hash. The plaintext is never returned, stored or
// logged, and it is not retained after this call.
func HashPassword(plain string) (string, error) {
	if len(plain) < MinPasswordLength {
		return "", ErrWeakPassword
	}
	// bcrypt silently truncates beyond 72 bytes, which would make two different
	// long passwords interchangeable. Rejecting is better than pretending.
	if len(plain) > 72 {
		return "", fmt.Errorf("auth: password must be at most 72 bytes")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(plain), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return string(h), nil
}

// CheckPassword verifies a plaintext against a stored hash.
//
// bcrypt's comparison is already constant-time with respect to the hash, and an
// empty stored hash is rejected explicitly so a user row created without a password
// can never be logged into.
func CheckPassword(hash, plain string) bool {
	if hash == "" || plain == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// Claims is the token payload. It carries identity and role only: no case data, no
// permissions list, nothing the client could edit to widen its own access. The role
// is re-checked against the route requirement on every request.
type Claims struct {
	jwt.RegisteredClaims
	Email string      `json:"email"`
	Name  string      `json:"name"`
	Role  domain.Role `json:"role"`
}

// Identity is the authenticated caller, as the API layer sees it.
type Identity struct {
	UserID string      `json:"user_id"`
	Email  string      `json:"email"`
	Name   string      `json:"name"`
	Role   domain.Role `json:"role"`
}

// Actor is how this caller appears in the audit trail. Email rather than id,
// because an audit log a human has to join against another table to read does not
// get read (SRS FR-052).
func (i Identity) Actor() string {
	if i.Email != "" {
		return i.Email
	}
	if i.UserID != "" {
		return i.UserID
	}
	return "anonymous"
}

// Permits reports whether this identity satisfies a minimum role.
func (i Identity) Permits(required domain.Role) bool { return i.Role.Permits(required) }

// Config configures the token issuer.
type Config struct {
	// Secret signs tokens. It is required: an empty secret would produce tokens
	// anyone could forge, so New refuses rather than defaulting.
	Secret string
	// TTL is the access-token lifetime. Short by design (SRS 15.1).
	TTL time.Duration
	// Issuer identifies this deployment in the token, so a token minted for one
	// environment cannot be replayed against another.
	Issuer string
}

// Issuer mints and verifies access tokens.
type Issuer struct {
	secret []byte
	ttl    time.Duration
	issuer string
	now    func() time.Time
}

// New builds an Issuer. A short secret is rejected outright: the signing key is the
// only thing standing between a request and full operator access, and a deployment
// that boots with a weak one is worse than one that refuses to boot (SRS 19.1).
func New(cfg Config) (*Issuer, error) {
	if len(strings.TrimSpace(cfg.Secret)) < 32 {
		return nil, errors.New("auth: JWT secret must be at least 32 characters")
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 8 * time.Hour
	}
	if cfg.Issuer == "" {
		cfg.Issuer = "ledgerflow"
	}
	return &Issuer{
		secret: []byte(cfg.Secret),
		ttl:    cfg.TTL,
		issuer: cfg.Issuer,
		now:    func() time.Time { return time.Now().UTC() },
	}, nil
}

// SetClock overrides the clock for deterministic tests, including expiry tests.
func (is *Issuer) SetClock(fn func() time.Time) { is.now = fn }

// TTL exposes the configured lifetime so the API can tell the client when to
// re-authenticate instead of the client guessing.
func (is *Issuer) TTL() time.Duration { return is.ttl }

// Token is a minted access token.
type Token struct {
	AccessToken string    `json:"access_token"`
	TokenType   string    `json:"token_type"`
	ExpiresIn   int       `json:"expires_in"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// Mint issues an access token for a user.
func (is *Issuer) Mint(u domain.User) (Token, error) {
	if u.ID == "" {
		return Token{}, errors.New("auth: cannot mint a token without a user id")
	}
	if !u.Role.Valid() {
		return Token{}, fmt.Errorf("auth: user %s has invalid role %q", u.ID, u.Role)
	}

	now := is.now()
	exp := now.Add(is.ttl)
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   u.ID,
			Issuer:    is.issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
		Email: u.Email,
		Name:  u.Name,
		Role:  u.Role,
	}

	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(is.secret)
	if err != nil {
		return Token{}, fmt.Errorf("sign token: %w", err)
	}
	return Token{
		AccessToken: signed,
		TokenType:   "Bearer",
		ExpiresIn:   int(is.ttl.Seconds()),
		ExpiresAt:   exp,
	}, nil
}

// Verify parses and validates a token, returning the caller's identity.
//
// The signing method is pinned to HS256. Accepting whatever the token's own header
// declares is the classic JWT vulnerability: a token can then claim "none" and
// verify against nothing.
func (is *Issuer) Verify(raw string) (Identity, error) {
	var claims Claims
	_, err := jwt.ParseWithClaims(raw, &claims, func(t *jwt.Token) (any, error) {
		if t.Method.Alg() != jwt.SigningMethodHS256.Alg() {
			return nil, fmt.Errorf("unexpected signing method %q", t.Method.Alg())
		}
		return is.secret, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(is.issuer),
		jwt.WithTimeFunc(is.now),
	)
	if err != nil {
		// The underlying reason is deliberately not returned to the caller: it
		// distinguishes "expired" from "bad signature", which tells an attacker
		// whether a forged token was structurally correct.
		return Identity{}, ErrInvalidToken
	}
	if claims.Subject == "" || !claims.Role.Valid() {
		return Identity{}, ErrInvalidToken
	}
	return Identity{
		UserID: claims.Subject,
		Email:  claims.Email,
		Name:   claims.Name,
		Role:   claims.Role,
	}, nil
}

// BearerToken extracts a token from an Authorization header value. The scheme
// comparison is case-insensitive per RFC 7235 and constant-time, so the header
// parser cannot be used as an oracle.
func BearerToken(header string) string {
	const prefix = "bearer "
	if len(header) <= len(prefix) {
		return ""
	}
	if subtle.ConstantTimeCompare([]byte(strings.ToLower(header[:len(prefix)])), []byte(prefix)) != 1 {
		return ""
	}
	return strings.TrimSpace(header[len(prefix):])
}

// Authenticate verifies credentials and mints a token.
//
// The user is passed in rather than looked up here, so this package never touches
// the database: the API layer performs the lookup and hands over whatever it found,
// including nil.
//
// The password check runs even when the user does not exist, against a fixed dummy
// hash. Without that, a missing account returns visibly faster than a wrong
// password, which turns login timing into an account-enumeration oracle.
func (is *Issuer) Authenticate(u *domain.User, password string) (Token, error) {
	if u == nil {
		_ = bcrypt.CompareHashAndPassword([]byte(dummyHash), []byte(password))
		return Token{}, ErrInvalidCredentials
	}
	if !CheckPassword(u.PasswordHash, password) {
		return Token{}, ErrInvalidCredentials
	}
	return is.Mint(*u)
}

// dummyHash is a valid bcrypt hash of an unguessable value, used only to spend the
// same time on a missing account as on a real one.
const dummyHash = "$2a$12$C6UzMDM.H6dfI/f/IKcEe.rlQVWiGkAiVdMc4/mZzUEO9ILtBBpZ."
