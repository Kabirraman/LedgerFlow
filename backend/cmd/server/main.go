// Command server is the LEDGERFLOW backend: API, event processor, agent
// orchestrator, policy engine and action service in one process (SRS 13.1).
//
// One process rather than five services is a deliberate choice for a prototype.
// The components are already separate packages with narrow interfaces between
// them, so splitting them later is a deployment change rather than a rewrite —
// and in the meantime a single binary means a demo has one thing to start and one
// place to read logs.
//
// Composition happens here and only here. Every package below is constructed with
// its dependencies passed in, which is what makes the safety boundaries visible in
// one screen: the simulation runner is built without a gateway, so a simulated run
// has no object in its call graph that could reach Razorpay (SRS AC-009); and the
// executor is the only component handed one at all (SRS 5.2).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	// Loads backend/.env into the process environment before config.Load() runs,
	// if the file exists. A real OS environment variable always wins over a
	// value from .env — this only fills in what isn't already set, which is
	// what lets a deployment (Docker, systemd, a CI runner) override it with
	// real environment injection without editing or deleting the file.
	_ "github.com/joho/godotenv/autoload"

	"github.com/ledgerflow/ledgerflow/internal/agents"
	"github.com/ledgerflow/ledgerflow/internal/auth"
	"github.com/ledgerflow/ledgerflow/internal/config"
	"github.com/ledgerflow/ledgerflow/internal/domain"
	"github.com/ledgerflow/ledgerflow/internal/events"
	"github.com/ledgerflow/ledgerflow/internal/executor"
	"github.com/ledgerflow/ledgerflow/internal/httpapi"
	"github.com/ledgerflow/ledgerflow/internal/orchestrator"
	"github.com/ledgerflow/ledgerflow/internal/razorpay"
	"github.com/ledgerflow/ledgerflow/internal/seed"
	"github.com/ledgerflow/ledgerflow/internal/simulation"
	"github.com/ledgerflow/ledgerflow/internal/store"
	"github.com/ledgerflow/ledgerflow/internal/verify"
)

// buildVersion is stamped at link time. See the Makefile.
var buildVersion = "dev"

func main() {
	if err := run(); err != nil {
		// The logger may not exist yet when configuration fails, so this one
		// message goes to stderr directly.
		fmt.Fprintf(os.Stderr, "ledgerflow: fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := newLogger(cfg)
	slog.SetDefault(log)

	// The process lives until an interrupt. Cancelling this context is what stops
	// every background worker, so shutdown is one signal rather than a list of
	// things to remember to stop.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("starting ledgerflow",
		"version", buildVersion,
		"env", cfg.AppEnv,
		"razorpay_mode", cfg.Razorpay.Mode,
		"razorpay_configured", cfg.Razorpay.Configured(),
		"model_configured", cfg.Gemini.Configured(),
	)

	// --- persistence ---

	st, err := store.Open(ctx, cfg.DatabaseURL, cfg.DBMaxConns, cfg.DBConnectTimeout)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer st.Close()
	log.Info("database connected")

	if cfg.RunMigrations {
		if err := st.Migrate(ctx); err != nil {
			return fmt.Errorf("apply migrations: %w", err)
		}
		log.Info("migrations applied")
	}

	if err := bootstrap(ctx, log, cfg, st); err != nil {
		return err
	}

	// --- external transports ---

	gateway, err := newGateway(log, cfg)
	if err != nil {
		return err
	}
	log.Info("payment gateway ready", "gateway", gateway.Name(), "external", gateway.External())

	model := agents.NewGeminiClient(agents.GeminiConfig{
		APIKey:      cfg.Gemini.APIKey,
		Model:       cfg.Gemini.Model,
		BaseURL:     cfg.Gemini.BaseURL,
		Temperature: cfg.Gemini.Temperature,
		MaxTokens:   cfg.Gemini.MaxOutputTokens,
		MaxRetries:  cfg.Gemini.Retries,
		Timeout:     cfg.Gemini.Timeout,
	})
	// Model latency and failures become operational counters, so a degraded
	// provider is visible on the ops screen instead of only in the logs
	// (SRS 21.3, 20.4).
	model.SetObserver(func(name string, latency time.Duration, err error) {
		// A detached context: the observer fires from inside an agent call whose
		// own context may already be cancelled by a timeout, and that is exactly
		// the failure worth counting.
		bg := context.WithoutCancel(ctx)
		_ = st.AddCounter(bg, "model_latency_ms", 1, latency.Milliseconds())
		if err != nil {
			_ = st.IncrCounter(bg, "model_errors")
		}
	})
	if !model.Enabled() {
		// Stated plainly at Warn. A demo audience is entitled to know which path
		// produced the decisions they are looking at (SRS 24.3, 25.2).
		log.Warn("no Gemini API key configured; all four agents will use their deterministic fallbacks")
	}

	// --- the recovery loop ---

	// The executor holds the gateway. Nothing else that follows does, apart from
	// the read-only verifier and reconciler (SRS 5.2, 19.2).
	exec := executor.New(st, gateway, executor.Config{
		LinkExpiryHours: 48,
		CallbackURL:     strings.TrimRight(cfg.Razorpay.CallbackBaseURL, "/") + "/recovered",
		Timeout:         cfg.Razorpay.Timeout,
		NotifyEmail:     true,
		NotifySMS:       true,
	})

	orch := orchestrator.New(st,
		agents.NewDetectionAgent(model),
		agents.NewDiagnosisAgent(model),
		agents.NewPlannerAgent(model),
		exec,
		orchestrator.Config{StageTimeout: cfg.Gemini.Timeout + 10*time.Second},
	)

	verifier := verify.New(st, gateway, verify.Config{
		VerifyAfter: cfg.VerificationPollInterval,
		GiveUpAfter: cfg.VerificationTimeout,
	})

	// The verifier is the ingestor's settler: a confirmed payment is attributed and
	// banked by one component, so a webhook cannot write a revenue total directly
	// (SRS FR-050).
	ingestor := events.NewIngestor(st, verifier, events.Config{
		WebhookSecret: cfg.Razorpay.WebhookSecret,
		MaxClockSkew:  10 * time.Minute,
	})

	scanner := events.NewScanner(st, ingestor, events.ScanConfig{
		AbandonAfter: 30 * time.Minute,
		GraceDays:    1,
	})

	reconciler := executor.NewReconciler(st, gateway, executor.ReconcileConfig{
		PendingStaleAfter: cfg.Razorpay.Timeout + 2*time.Minute,
	})

	// The simulation runner is constructed with the store and the model only. There
	// is no gateway argument to pass, which is why AC-009 holds structurally: a
	// simulated run cannot reach Razorpay because nothing in its call graph knows
	// how to.
	simulator := simulation.NewRunner(model, st)

	if cfg.SeedDemoData {
		rep, err := seed.New(st, ingestor).Run(ctx)
		switch {
		case err != nil:
			// Not fatal. An empty dashboard is a worse first impression than a
			// missing dataset, but neither is a reason to refuse to serve.
			log.Error("demo data seeding failed", "error", err)
		case rep.Skipped:
			log.Info("demo data seeding skipped", "reason", rep.Reason)
		default:
			log.Info("demo data seeded",
				"customers", rep.Customers, "failed_payments", rep.Payments,
				"abandoned_checkouts", rep.Checkouts, "overdue_invoices", rep.Invoices,
				"subscriptions", rep.Subscriptions, "cases_opened", rep.CasesOpened,
				"errors", len(rep.Errors))
			for _, e := range rep.Errors {
				log.Warn("demo data seeding problem", "detail", e)
			}
		}
	}

	// --- API ---

	issuer, err := auth.New(auth.Config{
		Secret: cfg.JWTSecret,
		TTL:    cfg.JWTTTL,
		Issuer: "ledgerflow:" + cfg.AppEnv,
	})
	if err != nil {
		return err
	}

	api, err := httpapi.New(httpapi.Deps{
		Config:       cfg,
		Store:        st,
		Issuer:       issuer,
		Logger:       log,
		Orchestrator: orch,
		Ingestor:     ingestor,
		Scanner:      scanner,
		Verifier:     verifier,
		Simulator:    simulator,
		Gateway:      gateway,
	})
	if err != nil {
		return err
	}

	// --- background workers ---

	startWorkers(ctx, log, cfg, orch, scanner, verifier, reconciler)

	srv := &http.Server{
		Addr:    ":" + cfg.Port,
		Handler: api.Handler(),
		// Generous but finite. A request that has not finished in a minute is
		// wedged, and holding the connection open only hides that.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("http server listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	// Graceful shutdown on a fresh context: ctx is already cancelled, and reusing
	// it would abort the in-flight requests this is meant to let finish.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", "error", err)
	}
	log.Info("stopped")
	return nil
}

// bootstrap seeds the records the system cannot run without: an administrator to
// log in as, and an active policy version.
//
// Both are idempotent. Restarting the stack must never reset a password an
// operator has changed or silently replace the limits they configured
// (SRS 23.3, 10.1).
func bootstrap(ctx context.Context, log *slog.Logger, cfg *config.Config, st *store.Store) error {
	hash, err := auth.HashPassword(cfg.SeedAdminPassword)
	if err != nil {
		return fmt.Errorf("hash seed admin password: %w", err)
	}
	admin := &domain.User{
		Email:        cfg.SeedAdminEmail,
		Name:         "LEDGERFLOW Administrator",
		Role:         domain.RoleAdmin,
		PasswordHash: hash,
	}
	created, err := st.EnsureUser(ctx, admin)
	if err != nil {
		return fmt.Errorf("seed administrator: %w", err)
	}
	if created {
		log.Info("administrator created", "email", admin.Email)
		if cfg.AppEnv == "local" {
			// Only in local development, and only the fact that the default is in
			// use — never the value, which would put a credential in the log file.
			log.Warn("the administrator password came from SEED_ADMIN_PASSWORD; change it before exposing this deployment")
		}
	}

	policy, err := st.EnsureDefaultPolicy(ctx)
	if err != nil {
		return fmt.Errorf("seed default policy: %w", err)
	}
	log.Info("active policy",
		"version", policy.Version,
		"max_automated_amount_paise", int64(policy.MaxAutomatedAmount),
		"require_human_approval_above_paise", int64(policy.RequireHumanApprovalAbove),
		"min_action_confidence", policy.MinActionConfidence,
		"max_retry_count", policy.MaxRetryCount,
	)
	return nil
}

// newGateway chooses the payment transport.
//
// Without credentials the sandbox gateway is used, which records what would have
// been sent and returns plausible responses without a network call. That keeps the
// whole loop runnable for reviewers who have no Razorpay account, and the
// gateway's name travels into every audit record so a sandbox result can never be
// presented as a Razorpay test-mode result (SRS 25.2, 24.3).
func newGateway(log *slog.Logger, cfg *config.Config) (razorpay.Gateway, error) {
	if !cfg.Razorpay.Configured() {
		log.Warn("no Razorpay credentials configured; using the in-process sandbox gateway",
			"note", "no external calls will be made and results are labelled as sandbox")
		return razorpay.NewSandboxGateway(), nil
	}
	gw, err := razorpay.NewHTTPGateway(cfg.Razorpay)
	if err != nil {
		return nil, fmt.Errorf("build razorpay gateway: %w", err)
	}
	return gw, nil
}

// startWorkers launches the background loops.
//
// Each takes ctx and returns immediately, so cancelling ctx stops all four. Errors
// are logged rather than fatal: a failing sweep must not take down the API, since
// the API is how an operator would find out what is wrong.
func startWorkers(ctx context.Context, log *slog.Logger, cfg *config.Config,
	orch *orchestrator.Orchestrator, scanner *events.Scanner,
	verifier *verify.Verifier, reconciler *executor.Reconciler) {

	// Detection sweeps for the two silent workflows (SRS 11.2, 11.3).
	scanner.Start(ctx, 2*time.Minute, func(err error) {
		log.Error("detection sweep failed", "error", err)
	})

	// Verification polls executed actions whose webhook has not arrived
	// (SRS 20.3).
	verifier.Start(ctx, cfg.VerificationPollInterval, func(err error) {
		log.Error("verification sweep failed", "error", err)
	})

	// Reconciliation resolves actions whose outcome we never learned, which is the
	// timed-out create that may or may not have produced a live payment link
	// (SRS 20.2).
	reconciler.StartReconcileWorker(ctx, time.Minute, 50, func(err error) {
		log.Error("reconciliation failed", "error", err)
	})

	// The agent pipeline is last, and it is the one worker that is optional: with
	// AUTO_RUN_PIPELINE off, cases are detected and queued but advanced only when
	// an operator asks. That is what makes a live demo steppable (SRS 24.1).
	if cfg.AutoRunPipeline {
		orch.Start(ctx, 20*time.Second, func(err error) {
			log.Error("recovery pipeline failed", "error", err)
		})
		log.Info("recovery pipeline worker started")
	} else {
		log.Info("recovery pipeline worker disabled", "reason", "AUTO_RUN_PIPELINE is false")
	}
}

// newLogger builds the structured logger.
//
// JSON outside local development so a log aggregator can parse it; text locally so
// a person can read it.
func newLogger(cfg *config.Config) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}

	var h slog.Handler
	if cfg.AppEnv == "local" {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(h).With("service", "ledgerflow", "version", buildVersion)
}