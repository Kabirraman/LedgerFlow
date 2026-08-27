// Package config loads LEDGERFLOW configuration from the environment.
//
// Secrets (Razorpay key secret, webhook secret, Gemini key, JWT secret) are
// read here and never leave the server process (SRS 19.1, NFR-001).
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully-resolved application configuration.
type Config struct {
	AppEnv      string // local | staging | production
	Port        string
	LogLevel    string
	CORSOrigins []string

	DatabaseURL      string
	DBMaxConns       int32
	DBConnectTimeout time.Duration
	RunMigrations    bool

	JWTSecret         string
	JWTTTL            time.Duration
	SeedAdminEmail    string
	SeedAdminPassword string

	Razorpay RazorpayConfig
	Gemini   GeminiConfig

	// AutoRunPipeline lets the ingestion path immediately drive the agent
	// pipeline for a newly created case. Disable it to step through the
	// workflow manually during a demo.
	AutoRunPipeline bool
	// AutoExecuteApproved executes actions whose policy verdict is PASS
	// without operator input. ESCALATE always waits for a human regardless.
	AutoExecuteApproved bool
	// SeedDemoData loads a small test-mode dataset on first boot so the
	// dashboard is not empty in a fresh deployment.
	SeedDemoData bool

	// VerificationPollInterval controls how often VERIFYING cases are
	// reconciled against external state (SRS 20.3 "webhook delayed").
	VerificationPollInterval time.Duration
	// VerificationTimeout is how long a case may stay in VERIFYING before it
	// is treated as not recovered.
	VerificationTimeout time.Duration
}

// RazorpayConfig holds the payment gateway credentials and behaviour.
type RazorpayConfig struct {
	KeyID         string
	KeySecret     string
	WebhookSecret string
	BaseURL       string
	Timeout       time.Duration
	MaxRetries    int
	// Mode is "test" or "live". A live value is rejected outright: the
	// prototype must never hold live credentials (SRS 23.4).
	Mode string
	// CallbackBaseURL is where payment links send the customer after payment.
	CallbackBaseURL string
}

// Configured reports whether real Razorpay calls are possible. When false the
// action service uses its recorded-stub transport so the app still runs
// end-to-end without credentials.
func (r RazorpayConfig) Configured() bool {
	return r.KeyID != "" && r.KeySecret != ""
}

// GeminiConfig holds the AI provider settings.
type GeminiConfig struct {
	APIKey  string
	Model   string
	BaseURL string
	Timeout time.Duration
	// MaxOutputTokens bounds cost per agent call.
	MaxOutputTokens int
	Temperature     float64
	// Retries is the number of extra attempts on transport/parse failure
	// before falling back to the deterministic engine (SRS 20.4).
	Retries int
}

// Configured reports whether the Gemini adapter should be used at all.
func (g GeminiConfig) Configured() bool { return g.APIKey != "" }

// Load resolves configuration from the environment, applying defaults that are
// safe for local development. It returns an error only for values that cannot
// be defaulted safely.
func Load() (*Config, error) {
	cfg := &Config{
		AppEnv:      env("APP_ENV", "local"),
		Port:        env("PORT", "8080"),
		LogLevel:    env("LOG_LEVEL", "info"),
		CORSOrigins: splitList(env("CORS_ORIGINS", "http://localhost:3000")),

		DatabaseURL:      env("DATABASE_URL", "postgres://ledgerflow:ledgerflow@localhost:5432/ledgerflow?sslmode=disable"),
		DBMaxConns:       int32(envInt("DB_MAX_CONNS", 10)),
		DBConnectTimeout: envDuration("DB_CONNECT_TIMEOUT", 30*time.Second),
		RunMigrations:    envBool("RUN_MIGRATIONS", true),

		JWTSecret:         env("JWT_SECRET", ""),
		JWTTTL:            envDuration("JWT_TTL", 12*time.Hour),
		SeedAdminEmail:    env("SEED_ADMIN_EMAIL", "admin@ledgerflow.test"),
		SeedAdminPassword: env("SEED_ADMIN_PASSWORD", ""),

		AutoRunPipeline:     envBool("AUTO_RUN_PIPELINE", true),
		AutoExecuteApproved: envBool("AUTO_EXECUTE_APPROVED", true),
		SeedDemoData:        envBool("SEED_DEMO_DATA", true),

		VerificationPollInterval: envDuration("VERIFICATION_POLL_INTERVAL", 30*time.Second),
		VerificationTimeout:      envDuration("VERIFICATION_TIMEOUT", 30*time.Minute),

		Razorpay: RazorpayConfig{
			KeyID:           env("RAZORPAY_KEY_ID", ""),
			KeySecret:       env("RAZORPAY_KEY_SECRET", ""),
			WebhookSecret:   env("RAZORPAY_WEBHOOK_SECRET", ""),
			BaseURL:         env("RAZORPAY_BASE_URL", "https://api.razorpay.com/v1"),
			Timeout:         envDuration("RAZORPAY_TIMEOUT", 15*time.Second),
			MaxRetries:      envInt("RAZORPAY_MAX_RETRIES", 2),
			Mode:            strings.ToLower(env("RAZORPAY_MODE", "test")),
			CallbackBaseURL: env("PUBLIC_APP_URL", "http://localhost:3000"),
		},
		Gemini: GeminiConfig{
			APIKey:          env("GEMINI_API_KEY", ""),
			Model:           env("GEMINI_MODEL", "gemini-2.0-flash"),
			BaseURL:         env("GEMINI_BASE_URL", "https://generativelanguage.googleapis.com/v1beta"),
			Timeout:         envDuration("GEMINI_TIMEOUT", 20*time.Second),
			MaxOutputTokens: envInt("GEMINI_MAX_OUTPUT_TOKENS", 1024),
			Temperature:     envFloat("GEMINI_TEMPERATURE", 0.1),
			Retries:         envInt("GEMINI_RETRIES", 1),
		},
	}

	if cfg.JWTSecret == "" {
		if cfg.AppEnv != "local" {
			return nil, errors.New("JWT_SECRET is required outside local development")
		}
		// A fixed development secret keeps local logins stable across restarts.
		// It is never used when APP_ENV != local.
		cfg.JWTSecret = "ledgerflow-local-development-secret-do-not-use-in-production"
	}
	if len(cfg.JWTSecret) < 32 && cfg.AppEnv != "local" {
		return nil, errors.New("JWT_SECRET must be at least 32 characters")
	}
	if cfg.SeedAdminPassword == "" {
		if cfg.AppEnv != "local" {
			return nil, errors.New("SEED_ADMIN_PASSWORD is required outside local development")
		}
		cfg.SeedAdminPassword = "ledgerflow"
	}

	// SRS 23.4: live keys must never be present in the prototype. Fail closed
	// rather than risk moving real money.
	if cfg.Razorpay.Mode != "test" {
		return nil, fmt.Errorf("RAZORPAY_MODE must be \"test\": LEDGERFLOW refuses to start in %q mode", cfg.Razorpay.Mode)
	}
	if strings.HasPrefix(cfg.Razorpay.KeyID, "rzp_live") {
		return nil, errors.New("a live Razorpay key was supplied; LEDGERFLOW only accepts rzp_test keys")
	}

	return cfg, nil
}

// Environment returns the domain environment tag applied to every record.
func (c *Config) Environment() string { return "test" }

// IsProduction reports whether hardened defaults should apply.
func (c *Config) IsProduction() bool { return c.AppEnv == "production" }

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(env(key, "")); err == nil {
		return v
	}
	return def
}

func envFloat(key string, def float64) float64 {
	if v, err := strconv.ParseFloat(env(key, ""), 64); err == nil {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, err := strconv.ParseBool(env(key, "")); err == nil {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(env(key, "")); err == nil {
		return v
	}
	return def
}

func splitList(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
