package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ledgerflow/ledgerflow/internal/auth"
	"github.com/ledgerflow/ledgerflow/internal/config"
	"github.com/ledgerflow/ledgerflow/internal/domain"
	"github.com/ledgerflow/ledgerflow/internal/events"
	"github.com/ledgerflow/ledgerflow/internal/orchestrator"
	"github.com/ledgerflow/ledgerflow/internal/razorpay"
	"github.com/ledgerflow/ledgerflow/internal/simulation"
	"github.com/ledgerflow/ledgerflow/internal/store"
	"github.com/ledgerflow/ledgerflow/internal/verify"
)

// Store is the persistence surface the HTTP layer needs (SRS NFR-007).
//
// It is declared here, by the consumer, rather than imported as one large
// interface from the store package. The list is long because the API is the read
// surface for the whole system, but it is still a list: a handler cannot reach a
// query this layer has no business making, and the compiler says so.
type Store interface {
	// Health.
	Ping(ctx context.Context) error

	// Users and policy (SRS 15.1, 10.1).
	FindUserByEmail(ctx context.Context, email string) (*domain.User, error)
	GetUser(ctx context.Context, id string) (*domain.User, error)
	ActivePolicyOrDefault(ctx context.Context) domain.Policy
	ListPolicies(ctx context.Context) ([]domain.Policy, error)
	SavePolicy(ctx context.Context, p *domain.Policy, activate bool) error

	// Dashboard and analytics (SRS 16.1, 18).
	DashboardSummary(ctx context.Context) (*domain.DashboardSummary, error)
	ListStrategyMetrics(ctx context.Context) ([]domain.StrategyMetric, error)
	Counters(ctx context.Context) (map[string]store.CounterValue, error)
	ListEvents(ctx context.Context, limit int) ([]domain.Event, error)

	// Cases (SRS 16.2).
	ListCases(ctx context.Context, f domain.CaseFilter) (*domain.CasePage, error)
	CaseDetail(ctx context.Context, caseID string) (*domain.CaseDetail, error)
	GetCase(ctx context.Context, id string) (*domain.RiskCase, error)
	UpdateCaseStatus(ctx context.Context, caseID string, to domain.CaseStatus, stopReason string) error

	// Approvals (SRS 16.3).
	ApprovalQueue(ctx context.Context, limit int, hideExecuted bool) ([]domain.ApprovalQueueItem, error)
	PendingApprovalCount(ctx context.Context, hideExecuted bool) (int, error)
	ListApprovals(ctx context.Context, decision domain.ApprovalDecision, limit int) ([]domain.Approval, error)
	ListApprovalsForCase(ctx context.Context, caseID string) ([]domain.Approval, error)
	DecideApproval(ctx context.Context, id string, decision domain.ApprovalDecision,
		reviewer, note string) (*domain.Approval, error)
	GetDecision(ctx context.Context, id string) (*domain.AgentDecision, error)

	// Audit (SRS 21.2).
	ListAuditForCase(ctx context.Context, caseID string) ([]domain.AuditLog, error)
	Audit(ctx context.Context, actor, entityType, entityID, caseID, eventType string, detail any) error

	// Simulation (SRS 16.4, 17).
	GetRun(ctx context.Context, id string) (*domain.SimulationRun, error)
	ListRuns(ctx context.Context, limit int) ([]domain.SimulationRun, error)
	ListDatasets(ctx context.Context, limit int) ([]domain.BenchmarkDataset, error)

	// Demo checkout (SRS 11.2).
	FindOrCreateCustomerByEmail(ctx context.Context, email, contact, name string, seg domain.Segment) (*domain.Customer, error)
	UpsertCheckoutSession(ctx context.Context, cs *domain.CheckoutSession) error
	GetCheckoutSession(ctx context.Context, id string) (*domain.CheckoutSession, error)
	MarkCheckoutStatus(ctx context.Context, id, status string) error
}

// Deps are the collaborators the API needs. Everything is injected so the router
// can be built against fakes in a test without a database or a network.
type Deps struct {
	Config *config.Config
	Store  Store
	Issuer *auth.Issuer
	Logger *slog.Logger

	// Orchestrator drives the agent pipeline for reanalyze and post-approval
	// execution. Optional: without it those routes report 503 rather than the
	// process refusing to start.
	Orchestrator *orchestrator.Orchestrator
	// Ingestor handles webhooks and payment backfill.
	Ingestor *events.Ingestor
	// Scanner opens cases for the silent workflows. The API needs it for one
	// route: the demo checkout's abandon button, which must open a case at once
	// rather than wait out the idle sweep (SRS 11.2).
	Scanner *events.Scanner
	// Verifier confirms recovery for a single case on demand.
	Verifier *verify.Verifier
	// Simulator runs the synthetic benchmark. It holds no gateway, which is what
	// makes the simulation route incapable of reaching Razorpay (SRS AC-009).
	Simulator *simulation.Runner
	// Gateway is used only for the read-only payment backfill. Money-moving calls
	// belong to the executor, which the API layer does not hold (SRS 19.2, 5.2).
	Gateway razorpay.Gateway
}

// Server owns the router and its dependencies.
type Server struct {
	deps Deps
	log  *slog.Logger
	now  func() time.Time
}

// New builds a server.
//
// The three dependencies it refuses to default are configuration, the store and
// the token issuer: an API with no configuration, no data or no way to
// authenticate a caller has no safe behaviour to fall back on. The optional
// workers degrade per route instead.
func New(d Deps) (*Server, error) {
	switch {
	case d.Config == nil:
		return nil, errors.New("httpapi: config is required")
	case d.Store == nil:
		return nil, errors.New("httpapi: store is required")
	case d.Issuer == nil:
		return nil, errors.New("httpapi: token issuer is required")
	}
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Server{deps: d, log: log, now: func() time.Time { return time.Now().UTC() }}, nil
}

// SetClock overrides the clock for deterministic tests.
func (s *Server) SetClock(fn func() time.Time) { s.now = fn }

// Router builds the route table from SRS 15.2.
//
// Route order reflects three authentication regimes, and they are kept visibly
// separate because mounting a route under the wrong one is the mistake that
// matters most here:
//
//   - Public: health and login. No token.
//   - Signature-authenticated: the Razorpay webhook. It carries no bearer token
//     by definition, and is authenticated by HMAC over the raw body instead
//     (SRS 19.3, FR-002).
//   - Bearer-authenticated: everything else, each with a minimum role.
func (s *Server) Router() *gin.Engine {
	if s.deps.Config.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
	}

	r := gin.New()
	r.RedirectTrailingSlash = false
	// Gin's method-not-allowed handling is off by default, which reports a
	// mistyped method as 404. Turning it on makes a client bug diagnosable.
	r.HandleMethodNotAllowed = true

	r.Use(
		requestID(),
		recoverPanics(s.log),
		requestLogger(s.log),
		securityHeaders(),
		corsMiddleware(s.deps.Config.CORSOrigins),
	)

	r.NoRoute(func(c *gin.Context) {
		failWith(c, http.StatusNotFound, "not_found", "no such endpoint")
	})
	r.NoMethod(func(c *gin.Context) {
		failWith(c, http.StatusMethodNotAllowed, "method_not_allowed", "that method is not supported on this endpoint")
	})

	api := r.Group("/api")

	// --- public ---
	api.GET("/health", s.health)
	api.GET("/version", s.version)
	api.POST("/auth/login", limitBody(maxJSONBody), s.login)

	// --- signature-authenticated (SRS 15.2 "System") ---
	api.POST("/webhooks/razorpay", limitBody(maxWebhookBody), s.razorpayWebhook)

	// --- bearer-authenticated ---
	authed := api.Group("", authenticate(s.deps.Issuer), limitBody(maxJSONBody))

	authed.GET("/auth/me", requireRole(domain.RoleOperator), s.me)

	operator := authed.Group("", requireRole(domain.RoleOperator))
	operator.GET("/dashboard/summary", s.dashboardSummary)
	operator.GET("/cases", s.listCases)
	operator.GET("/cases/:id", s.caseDetail)
	operator.POST("/cases/:id/reanalyze", s.reanalyzeCase)
	operator.POST("/cases/:id/verify", s.verifyCase)
	operator.POST("/simulations/run", s.runSimulation)
	operator.GET("/simulations", s.listSimulations)
	operator.GET("/simulations/:id", s.getSimulation)
	operator.GET("/datasets", s.listDatasets)
	operator.GET("/analytics/strategies", s.strategyPerformance)
	operator.GET("/ops/metrics", s.opsMetrics)
	operator.GET("/ops/events", s.listEvents)

	// Demo checkout. Operator-level because it is a demonstration surface, and it
	// writes only first-party checkout intent — never a payment record
	// (SRS 11.2).
	operator.POST("/demo/checkout", s.startCheckout)
	operator.POST("/demo/checkout/:id/activity", s.checkoutActivity)
	operator.POST("/demo/checkout/:id/abandon", s.abandonCheckout)
	operator.POST("/demo/checkout/:id/convert", s.convertCheckout)

	reviewer := authed.Group("", requireRole(domain.RoleReviewer))
	reviewer.POST("/cases/:id/approve", s.approveCase)
	reviewer.POST("/cases/:id/reject", s.rejectCase)
	reviewer.GET("/approvals", s.listApprovals)
	reviewer.GET("/audit/:caseId", s.caseAudit)

	admin := authed.Group("", requireRole(domain.RoleAdmin))
	admin.POST("/sync/payments", s.syncPayments)
	admin.GET("/policies", s.getPolicies)
	admin.PUT("/policies", s.updatePolicy)

	return r
}

// Handler exposes the router as a plain http.Handler.
func (s *Server) Handler() http.Handler { return s.Router() }
