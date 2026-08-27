package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ledgerflow/ledgerflow/internal/auth"
	"github.com/ledgerflow/ledgerflow/internal/config"
	"github.com/ledgerflow/ledgerflow/internal/domain"
	"github.com/ledgerflow/ledgerflow/internal/events"
	"github.com/ledgerflow/ledgerflow/internal/razorpay"
	"github.com/ledgerflow/ledgerflow/internal/store"
)

// Integration tests for webhook ingestion (SRS 22.2).
//
// These drive real HTTP requests through the real router, the real signature
// verification and the real ingestor, into a store that is in-memory but enforces
// the two constraints the design depends on: events.external_event_id is unique,
// and a source record can carry only one open case. Those are the constraints that
// make duplicate delivery safe, so a fake that did not enforce them would let
// these tests pass while the real system double-counted (SRS AC-006).
//
// The two §22.2 items covered here are webhook receipt and signature validation,
// and duplicate event delivery. Payment links, invoices, subscriptions and the
// timeout/retry budget are covered in internal/razorpay.

const testWebhookSecret = "whsec_integration_test"

// TestMain puts gin in test mode.
//
// Without this every harness dumps the whole route table to stdout, and a genuine
// failure message is then buried under thirty lines of debug output.
func TestMain(m *testing.M) {
	gin.SetMode(gin.TestMode)
	os.Exit(m.Run())
}

// --- in-memory store ---

// memStore implements the persistence surface the API and the ingestor need.
//
// Everything is guarded by one mutex. The unique-constraint behaviour is
// deliberate rather than incidental: RecordEvent returns created=false on a
// repeated external id exactly as the partial unique index does, which is what
// the duplicate-delivery test is actually asserting about.
type memStore struct {
	mu sync.Mutex

	users     map[string]*domain.User // by lowercased email
	policy    domain.Policy
	policies  []domain.Policy
	customers map[string]*domain.Customer // by id
	byEmail   map[string]string           // email -> customer id

	events   []*domain.Event
	eventIDs map[string]string // external_event_id -> event id

	txns       map[string]*domain.Transaction // by id
	txnByRzpID map[string]string
	invoices   map[string]*domain.Invoice
	invByRzpID map[string]string
	subs       map[string]*domain.Subscription
	subByRzpID map[string]string
	sessions   map[string]*domain.CheckoutSession

	cases map[string]*domain.RiskCase
	// openBySource keys on sourceType|sourceID and holds the one open case for
	// that record, mirroring the partial unique index.
	openBySource map[string]string

	audits   []domain.AuditLog
	counters map[string]store.CounterValue

	seq int
}

func newMemStore() *memStore {
	return &memStore{
		users:        map[string]*domain.User{},
		policy:       domain.DefaultPolicy(),
		customers:    map[string]*domain.Customer{},
		byEmail:      map[string]string{},
		eventIDs:     map[string]string{},
		txns:         map[string]*domain.Transaction{},
		txnByRzpID:   map[string]string{},
		invoices:     map[string]*domain.Invoice{},
		invByRzpID:   map[string]string{},
		subs:         map[string]*domain.Subscription{},
		subByRzpID:   map[string]string{},
		sessions:     map[string]*domain.CheckoutSession{},
		cases:        map[string]*domain.RiskCase{},
		openBySource: map[string]string{},
		counters:     map[string]store.CounterValue{},
	}
}

// nextID mints a deterministic id. Callers hold the lock.
func (m *memStore) nextID(prefix string) string {
	m.seq++
	return fmt.Sprintf("%s_%04d", prefix, m.seq)
}

func (m *memStore) Ping(context.Context) error { return nil }

// --- users and policy ---

func (m *memStore) addUser(email, password string, role domain.Role) *domain.User {
	hash, err := auth.HashPassword(password)
	if err != nil {
		panic(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	u := &domain.User{
		ID: m.nextID("usr"), Email: strings.ToLower(email), Name: email,
		Role: role, PasswordHash: hash, CreatedAt: time.Now().UTC(),
	}
	m.users[u.Email] = u
	return u
}

func (m *memStore) FindUserByEmail(_ context.Context, email string) (*domain.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[strings.ToLower(strings.TrimSpace(email))]
	if !ok {
		return nil, fmt.Errorf("%w: user", domain.ErrNotFound)
	}
	copied := *u
	return &copied, nil
}

func (m *memStore) GetUser(_ context.Context, id string) (*domain.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.users {
		if u.ID == id {
			copied := *u
			return &copied, nil
		}
	}
	return nil, fmt.Errorf("%w: user %s", domain.ErrNotFound, id)
}

func (m *memStore) ActivePolicyOrDefault(context.Context) domain.Policy {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.policy
}

func (m *memStore) ListPolicies(context.Context) ([]domain.Policy, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]domain.Policy(nil), m.policies...), nil
}

func (m *memStore) SavePolicy(_ context.Context, p *domain.Policy, activate bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p.UpdatedAt = time.Now().UTC()
	m.policies = append(m.policies, *p)
	if activate {
		m.policy = *p
	}
	return nil
}

// --- customers ---

// Every record write below forces domain.EnvTest, because the real store does:
// each INSERT in internal/store/records.go writes the environment column as the
// literal 'test' rather than binding whatever the caller passed. That is the
// mechanism behind "live-mode transactions are out of scope" (SRS 5.2) — no code
// path, including a buggy one, can produce a live-labelled row. A fake that
// honoured the caller's field instead would let a test pass while the real system
// behaved differently.
func (m *memStore) UpsertCustomer(_ context.Context, c *domain.Customer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c.ID == "" {
		c.ID = m.nextID("cus")
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	c.Environment = domain.EnvTest
	copied := *c
	m.customers[c.ID] = &copied
	if c.Email != "" {
		m.byEmail[strings.ToLower(c.Email)] = c.ID
	}
	return nil
}

func (m *memStore) GetCustomer(_ context.Context, id string) (*domain.Customer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.customers[id]
	if !ok {
		return nil, fmt.Errorf("%w: customer %s", domain.ErrNotFound, id)
	}
	copied := *c
	return &copied, nil
}

func (m *memStore) FindOrCreateCustomerByEmail(_ context.Context, email, contact, name string,
	seg domain.Segment) (*domain.Customer, error) {

	m.mu.Lock()
	defer m.mu.Unlock()
	key := strings.ToLower(strings.TrimSpace(email))
	if id, ok := m.byEmail[key]; ok {
		copied := *m.customers[id]
		return &copied, nil
	}
	c := &domain.Customer{
		ID: m.nextID("cus"), Email: key, Contact: contact, Name: name,
		Segment: seg, Environment: domain.EnvTest, CreatedAt: time.Now().UTC(),
	}
	m.customers[c.ID] = c
	if key != "" {
		m.byEmail[key] = c.ID
	}
	copied := *c
	return &copied, nil
}

func (m *memStore) ListCustomers(_ context.Context, limit int) ([]domain.Customer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.Customer{}
	for _, c := range m.customers {
		if len(out) >= limit && limit > 0 {
			break
		}
		out = append(out, *c)
	}
	return out, nil
}

// --- events ---

// RecordEvent enforces uniqueness on ExternalEventID.
//
// This is the whole duplicate-delivery defence, and it lives here rather than in
// a handler on purpose: a cache or an if-statement in the ingestor could be
// bypassed by two concurrent deliveries, where a unique index cannot be.
func (m *memStore) RecordEvent(_ context.Context, e *domain.Event) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.eventIDs[e.ExternalEventID]; ok {
		e.ID = existing
		return false, nil
	}
	if e.ID == "" {
		e.ID = m.nextID("evt")
	}
	if e.ReceivedAt.IsZero() {
		e.ReceivedAt = time.Now().UTC()
	}
	m.eventIDs[e.ExternalEventID] = e.ID
	copied := *e
	m.events = append(m.events, &copied)
	return true, nil
}

func (m *memStore) MarkEventProcessed(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	for _, e := range m.events {
		if e.ID == id {
			e.ProcessedAt = &now
			return nil
		}
	}
	return fmt.Errorf("%w: event %s", domain.ErrNotFound, id)
}

func (m *memStore) LatestEntityTimestamp(_ context.Context, entityID string) (*time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var latest *time.Time
	for _, e := range m.events {
		if e.EntityID != entityID || e.EntityTimestamp == nil || e.ProcessedAt == nil {
			continue
		}
		if latest == nil || e.EntityTimestamp.After(*latest) {
			ts := *e.EntityTimestamp
			latest = &ts
		}
	}
	return latest, nil
}

func (m *memStore) ListEvents(_ context.Context, limit int) ([]domain.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.Event{}
	for i := len(m.events) - 1; i >= 0 && (limit <= 0 || len(out) < limit); i-- {
		out = append(out, *m.events[i])
	}
	return out, nil
}

func (m *memStore) IncrCounter(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cv := m.counters[name]
	cv.Count++
	m.counters[name] = cv
	return nil
}

func (m *memStore) AddCounter(_ context.Context, name string, n, sum int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cv := m.counters[name]
	cv.Count += n
	cv.Sum += sum
	m.counters[name] = cv
	return nil
}

func (m *memStore) Counters(context.Context) (map[string]store.CounterValue, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]store.CounterValue{}
	for k, v := range m.counters {
		out[k] = v
	}
	return out, nil
}

func (m *memStore) counter(name string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counters[name].Count
}

// --- records ---

func (m *memStore) UpsertTransaction(_ context.Context, t *domain.Transaction) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.RazorpayPaymentID != "" {
		if id, ok := m.txnByRzpID[t.RazorpayPaymentID]; ok {
			t.ID = id
		}
	}
	if t.ID == "" {
		t.ID = m.nextID("txn")
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	t.Environment = domain.EnvTest
	copied := *t
	m.txns[t.ID] = &copied
	if t.RazorpayPaymentID != "" {
		m.txnByRzpID[t.RazorpayPaymentID] = t.ID
	}
	return nil
}

func (m *memStore) GetTransaction(_ context.Context, id string) (*domain.Transaction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.txns[id]
	if !ok {
		return nil, fmt.Errorf("%w: transaction %s", domain.ErrNotFound, id)
	}
	copied := *t
	return &copied, nil
}

func (m *memStore) FindTransactionByRazorpayID(_ context.Context, rzpID string) (*domain.Transaction, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.txnByRzpID[rzpID]
	if !ok {
		return nil, fmt.Errorf("%w: transaction %s", domain.ErrNotFound, rzpID)
	}
	copied := *m.txns[id]
	return &copied, nil
}

func (m *memStore) CountCustomerAttempts(_ context.Context, customerID, orderID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, t := range m.txns {
		if t.CustomerID == customerID && (orderID == "" || t.RazorpayOrderID == orderID) {
			n++
		}
	}
	return n, nil
}

func (m *memStore) UpsertInvoice(_ context.Context, inv *domain.Invoice) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if inv.RazorpayInvoiceID != "" {
		if id, ok := m.invByRzpID[inv.RazorpayInvoiceID]; ok {
			inv.ID = id
		}
	}
	if inv.ID == "" {
		inv.ID = m.nextID("inv")
	}
	inv.Environment = domain.EnvTest
	copied := *inv
	m.invoices[inv.ID] = &copied
	if inv.RazorpayInvoiceID != "" {
		m.invByRzpID[inv.RazorpayInvoiceID] = inv.ID
	}
	return nil
}

func (m *memStore) FindInvoiceByRazorpayID(_ context.Context, rzpID string) (*domain.Invoice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.invByRzpID[rzpID]
	if !ok {
		return nil, fmt.Errorf("%w: invoice %s", domain.ErrNotFound, rzpID)
	}
	copied := *m.invoices[id]
	return &copied, nil
}

func (m *memStore) MarkInvoicePaid(_ context.Context, id string, amountPaid domain.Money) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	inv, ok := m.invoices[id]
	if !ok {
		return fmt.Errorf("%w: invoice %s", domain.ErrNotFound, id)
	}
	inv.Status = "paid"
	inv.AmountPaid = amountPaid
	return nil
}

func (m *memStore) UpsertSubscription(_ context.Context, sub *domain.Subscription) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sub.RazorpaySubscriptionID != "" {
		if id, ok := m.subByRzpID[sub.RazorpaySubscriptionID]; ok {
			sub.ID = id
		}
	}
	if sub.ID == "" {
		sub.ID = m.nextID("sub")
	}
	if sub.Status == "" {
		sub.Status = "active"
	}
	sub.Environment = domain.EnvTest
	copied := *sub
	m.subs[sub.ID] = &copied
	if sub.RazorpaySubscriptionID != "" {
		m.subByRzpID[sub.RazorpaySubscriptionID] = sub.ID
	}
	return nil
}

func (m *memStore) FindSubscriptionByRazorpayID(_ context.Context, rzpID string) (*domain.Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.subByRzpID[rzpID]
	if !ok {
		return nil, fmt.Errorf("%w: subscription %s", domain.ErrNotFound, rzpID)
	}
	copied := *m.subs[id]
	return &copied, nil
}

// --- checkout sessions ---

func (m *memStore) UpsertCheckoutSession(_ context.Context, cs *domain.CheckoutSession) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cs.ID == "" {
		cs.ID = m.nextID("chk")
	}
	cs.Environment = domain.EnvTest
	copied := *cs
	m.sessions[cs.ID] = &copied
	return nil
}

func (m *memStore) GetCheckoutSession(_ context.Context, id string) (*domain.CheckoutSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cs, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("%w: checkout session %s", domain.ErrNotFound, id)
	}
	copied := *cs
	return &copied, nil
}

func (m *memStore) MarkCheckoutStatus(_ context.Context, id, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cs, ok := m.sessions[id]
	if !ok {
		return fmt.Errorf("%w: checkout session %s", domain.ErrNotFound, id)
	}
	cs.Status = status
	return nil
}

func (m *memStore) FindAbandonedCheckouts(_ context.Context, idleFor time.Duration, limit int) ([]domain.CheckoutSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := time.Now().UTC().Add(-idleFor)
	out := []domain.CheckoutSession{}
	for _, cs := range m.sessions {
		if cs.Status == "active" && !cs.LastActivityAt.After(cutoff) {
			out = append(out, *cs)
		}
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (m *memStore) FindOverdueInvoices(_ context.Context, graceDays, limit int) ([]domain.Invoice, error) {
	return []domain.Invoice{}, nil
}

// --- cases ---

func sourceKey(st domain.SourceType, sourceID string) string {
	return string(st) + "|" + sourceID
}

// CreateCase mirrors the partial unique index on (source_type, source_id) for
// non-terminal cases: a second open case for the same record is rejected with
// ErrDuplicateEvent, which is the error the ingestor knows how to recover from.
func (m *memStore) CreateCase(_ context.Context, c *domain.RiskCase) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	sourceID := c.TransactionID + c.CheckoutSessionID + c.InvoiceID + c.SubscriptionID
	key := sourceKey(c.SourceType, sourceID)
	if _, exists := m.openBySource[key]; exists {
		return fmt.Errorf("%w: a case is already open for this record", domain.ErrDuplicateEvent)
	}
	if c.ID == "" {
		c.ID = m.nextID("case")
	}
	if c.Reference == "" {
		c.Reference = fmt.Sprintf("REV-%04d", len(m.cases)+1)
	}
	now := time.Now().UTC()
	c.CreatedAt, c.UpdatedAt = now, now
	if c.Environment == "" {
		c.Environment = domain.EnvTest
	}
	copied := *c
	m.cases[c.ID] = &copied
	m.openBySource[key] = c.ID
	return nil
}

func (m *memStore) FindOpenCaseBySource(_ context.Context, st domain.SourceType, sourceID string) (*domain.RiskCase, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.openBySource[sourceKey(st, sourceID)]
	if !ok {
		return nil, fmt.Errorf("%w: no open case", domain.ErrNotFound)
	}
	copied := *m.cases[id]
	return &copied, nil
}

func (m *memStore) GetCase(_ context.Context, id string) (*domain.RiskCase, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.cases[id]
	if !ok {
		return nil, fmt.Errorf("%w: case %s", domain.ErrNotFound, id)
	}
	copied := *c
	return &copied, nil
}

func (m *memStore) UpdateCaseStatus(_ context.Context, caseID string, to domain.CaseStatus, stopReason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.cases[caseID]
	if !ok {
		return fmt.Errorf("%w: case %s", domain.ErrNotFound, caseID)
	}
	if !domain.CanTransition(c.Status, to) {
		return fmt.Errorf("%w: %s -> %s", domain.ErrInvalidTransition, c.Status, to)
	}
	c.Status = to
	c.StopReason = stopReason
	c.UpdatedAt = time.Now().UTC()
	if domain.IsTerminal(to) {
		delete(m.openBySource, sourceKey(c.SourceType,
			c.TransactionID+c.CheckoutSessionID+c.InvoiceID+c.SubscriptionID))
	}
	return nil
}

func (m *memStore) ListCases(_ context.Context, f domain.CaseFilter) (*domain.CasePage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	items := []domain.CaseListItem{}
	for _, c := range m.cases {
		if f.Status != "" && c.Status != f.Status {
			continue
		}
		items = append(items, domain.CaseListItem{RiskCase: *c})
	}
	return &domain.CasePage{Items: items, Total: len(items), Limit: f.Limit, Offset: f.Offset}, nil
}

func (m *memStore) CaseDetail(ctx context.Context, caseID string) (*domain.CaseDetail, error) {
	c, err := m.GetCase(ctx, caseID)
	if err != nil {
		return nil, err
	}
	return &domain.CaseDetail{Case: *c}, nil
}

func (m *memStore) casesOpened() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.cases)
}

// --- approvals, decisions, analytics, simulation: read stubs ---
//
// These exist so the fake satisfies the API's Store interface. The webhook tests
// never reach them, and returning empty values rather than panicking means a
// future test that does touch one gets a clear empty result instead of a crash in
// unrelated code.

func (m *memStore) ApprovalQueue(context.Context, int, bool) ([]domain.ApprovalQueueItem, error) {
	return []domain.ApprovalQueueItem{}, nil
}

func (m *memStore) PendingApprovalCount(context.Context, bool) (int, error) { return 0, nil }

func (m *memStore) ListApprovals(context.Context, domain.ApprovalDecision, int) ([]domain.Approval, error) {
	return []domain.Approval{}, nil
}

func (m *memStore) ListApprovalsForCase(context.Context, string) ([]domain.Approval, error) {
	return []domain.Approval{}, nil
}

func (m *memStore) DecideApproval(context.Context, string, domain.ApprovalDecision,
	string, string) (*domain.Approval, error) {
	return nil, fmt.Errorf("%w: approval", domain.ErrNotFound)
}

func (m *memStore) GetDecision(context.Context, string) (*domain.AgentDecision, error) {
	return nil, fmt.Errorf("%w: decision", domain.ErrNotFound)
}

func (m *memStore) DashboardSummary(context.Context) (*domain.DashboardSummary, error) {
	return &domain.DashboardSummary{}, nil
}

func (m *memStore) ListStrategyMetrics(context.Context) ([]domain.StrategyMetric, error) {
	return []domain.StrategyMetric{}, nil
}

func (m *memStore) GetRun(context.Context, string) (*domain.SimulationRun, error) {
	return nil, fmt.Errorf("%w: run", domain.ErrNotFound)
}

func (m *memStore) ListRuns(context.Context, int) ([]domain.SimulationRun, error) {
	return []domain.SimulationRun{}, nil
}

func (m *memStore) ListDatasets(context.Context, int) ([]domain.BenchmarkDataset, error) {
	return []domain.BenchmarkDataset{}, nil
}

func (m *memStore) FindActionByExternalID(context.Context, string) (*domain.RecoveryAction, error) {
	return nil, fmt.Errorf("%w: action", domain.ErrNotFound)
}

func (m *memStore) GetActionByIdempotencyKey(context.Context, string) (*domain.RecoveryAction, error) {
	return nil, fmt.Errorf("%w: action", domain.ErrNotFound)
}

// --- audit ---

func (m *memStore) Audit(_ context.Context, actor, entityType, entityID, caseID, eventType string, detail any) error {
	raw, _ := json.Marshal(detail)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audits = append(m.audits, domain.AuditLog{
		ID: m.nextID("aud"), Actor: actor, EntityType: entityType, EntityID: entityID,
		CaseID: caseID, EventType: eventType, PayloadJSON: raw, Timestamp: time.Now().UTC(),
	})
	return nil
}

func (m *memStore) ListAuditForCase(_ context.Context, caseID string) ([]domain.AuditLog, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []domain.AuditLog{}
	for _, a := range m.audits {
		if a.CaseID == caseID {
			out = append(out, a)
		}
	}
	return out, nil
}

// auditEvents returns the event types recorded, in order.
func (m *memStore) auditEvents() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.audits))
	for _, a := range m.audits {
		out = append(out, a.EventType)
	}
	return out
}

func (m *memStore) hasAudit(eventType string) bool {
	for _, e := range m.auditEvents() {
		if e == eventType {
			return true
		}
	}
	return false
}

// Compile-time proof that one fake covers both consumer interfaces. If either
// grows a method, this fails to build rather than failing at run time in a
// confusing place.
var (
	_ Store        = (*memStore)(nil)
	_ events.Store = (*memStore)(nil)
)

// --- harness ---

type harness struct {
	t     *testing.T
	store *memStore
	srv   *httptest.Server
	ing   *events.Ingestor
}

// newHarness wires the real router over the in-memory store.
//
// The ingestor is constructed with a nil settler: these tests are about receipt,
// verification and deduplication, and a webhook must not be able to bank revenue
// anyway — that is the verifier's job (SRS FR-050).
func newHarness(t *testing.T, secret string) *harness {
	t.Helper()

	st := newMemStore()
	cfg := &config.Config{
		AppEnv:      "test",
		Port:        "0",
		LogLevel:    "error",
		CORSOrigins: []string{"http://localhost:3000"},
		JWTSecret:   "integration-test-signing-secret-not-a-real-one",
		JWTTTL:      time.Hour,
		Razorpay: config.RazorpayConfig{
			Mode:          "test",
			WebhookSecret: secret,
			Timeout:       time.Second,
		},
	}

	issuer, err := auth.New(auth.Config{Secret: cfg.JWTSecret, TTL: cfg.JWTTTL, Issuer: "ledgerflow:test"})
	if err != nil {
		t.Fatalf("build issuer: %v", err)
	}

	ing := events.NewIngestor(st, nil, events.Config{WebhookSecret: secret, MaxClockSkew: 10 * time.Minute})

	api, err := New(Deps{
		Config:   cfg,
		Store:    st,
		Issuer:   issuer,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Ingestor: ing,
	})
	if err != nil {
		t.Fatalf("build api: %v", err)
	}

	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return &harness{t: t, store: st, srv: srv, ing: ing}
}

// postWebhook sends a body with the signature computed over those exact bytes.
func (h *harness) postWebhook(body []byte, secret, eventID string) (*http.Response, map[string]any) {
	h.t.Helper()
	sig := ""
	if secret != "" {
		sig = razorpay.ComputeSignature(body, secret)
	}
	return h.postWebhookRaw(body, sig, eventID)
}

// postWebhookRaw sends a body with a caller-supplied signature header, which is
// how a forged or tampered delivery is expressed.
func (h *harness) postWebhookRaw(body []byte, signature, eventID string) (*http.Response, map[string]any) {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/api/webhooks/razorpay", strings.NewReader(string(body)))
	if err != nil {
		h.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if signature != "" {
		req.Header.Set(razorpay.SignatureHeader, signature)
	}
	if eventID != "" {
		req.Header.Set(razorpay.EventIDHeader, eventID)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatalf("post webhook: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return resp, decoded
}

// paymentFailedBody builds a payment.failed envelope in Razorpay's shape.
func paymentFailedBody(paymentID, email string, amountPaise int64, at time.Time) []byte {
	env := map[string]any{
		"entity":     "event",
		"account_id": "acc_TESTINTEGRATION",
		"event":      "payment.failed",
		"contains":   []string{"payment"},
		"created_at": at.Unix(),
		"payload": map[string]any{
			"payment": map[string]any{
				"entity": map[string]any{
					"id":           paymentID,
					"order_id":     "order_" + paymentID,
					"amount":       amountPaise,
					"currency":     "INR",
					"status":       "failed",
					"method":       "card",
					"captured":     false,
					"description":  "Order #LF-9001",
					"email":        email,
					"contact":      "+919810000099",
					"error_code":   "GATEWAY_ERROR",
					"error_reason": "payment_failed",
					"error_source": "gateway",
					"error_step":   "authorization",
					"created_at":   at.Unix(),
				},
			},
		},
	}
	raw, err := json.Marshal(env)
	if err != nil {
		panic(err)
	}
	return raw
}

// subscriptionHaltedBody builds a subscription.halted envelope: a recurring
// charge failed and Razorpay stopped retrying (SRS 11.4).
func subscriptionHaltedBody(subID string, amountPaise int64, at time.Time) []byte {
	env := map[string]any{
		"entity":     "event",
		"account_id": "acc_TESTINTEGRATION",
		"event":      "subscription.halted",
		"contains":   []string{"subscription", "payment"},
		"created_at": at.Unix(),
		"payload": map[string]any{
			"subscription": map[string]any{
				"entity": map[string]any{
					"id":              subID,
					"plan_id":         "plan_TESTINTEGRATION",
					"customer_id":     "cust_TESTINTEGRATION",
					"status":          "halted",
					"paid_count":      11,
					"remaining_count": 1,
					"current_start":   at.AddDate(0, 0, -24).Unix(),
					"current_end":     at.AddDate(0, 0, 6).Unix(),
					"charge_at":       at.AddDate(0, 0, 6).Unix(),
				},
			},
			"payment": map[string]any{
				"entity": map[string]any{
					"id":           "pay_SUBFAIL0001",
					"amount":       amountPaise,
					"currency":     "INR",
					"status":       "failed",
					"method":       "card",
					"email":        "ananya.desai@example.com",
					"contact":      "+919810000006",
					"error_code":   "BAD_REQUEST_ERROR",
					"error_reason": "payment_declined_by_bank",
					"created_at":   at.Unix(),
				},
			},
		},
	}
	raw, err := json.Marshal(env)
	if err != nil {
		panic(err)
	}
	return raw
}

// --- SRS 22.2: webhook receipt and signature validation ---

func TestIntegrationWebhookAcceptedWithValidSignature(t *testing.T) {
	h := newHarness(t, testWebhookSecret)
	body := paymentFailedBody("pay_INTEG0001", "webhook.customer@example.com", 1_249_900, time.Now().UTC())

	resp, decoded := h.postWebhook(body, testWebhookSecret, "evt_INTEG0001")

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, decoded)
	}
	if got := decoded["status"]; got != "accepted" {
		t.Errorf("status = %v, want accepted", got)
	}
	if got := decoded["event_type"]; got != "payment.failed" {
		t.Errorf("event_type = %v, want payment.failed", got)
	}
	if decoded["case_created"] != true {
		t.Errorf("case_created = %v, want true — a failed ₹12,499 payment is above the at-risk threshold", decoded["case_created"])
	}
	caseID, _ := decoded["case_id"].(string)
	if caseID == "" {
		t.Fatal("no case id returned")
	}
	if ref, _ := decoded["case_reference"].(string); !strings.HasPrefix(ref, "REV-") {
		t.Errorf("case_reference = %q, want a REV- reference an operator can quote", ref)
	}

	// The case exists, is NEW, and carries the amount from the payload rather than
	// anything derived or rounded.
	rc, err := h.store.GetCase(context.Background(), caseID)
	if err != nil {
		t.Fatalf("case %s was reported created but is not in the store: %v", caseID, err)
	}
	if rc.Status != domain.StatusNew {
		t.Errorf("case status = %s, want %s", rc.Status, domain.StatusNew)
	}
	if rc.RevenueAtRisk != domain.Money(1_249_900) {
		t.Errorf("revenue at risk = %d paise, want 1249900", int64(rc.RevenueAtRisk))
	}
	if rc.SourceType != domain.SourcePaymentFailure {
		t.Errorf("source type = %s, want %s", rc.SourceType, domain.SourcePaymentFailure)
	}
	if rc.TransactionID == "" {
		t.Error("the case does not point at the transaction it was opened from")
	}
	if rc.Environment != domain.EnvTest {
		t.Errorf("environment = %s, want test — nothing here may be labelled live", rc.Environment)
	}

	// The event was stored with the signature marked valid, and marked processed.
	stored, err := h.store.ListEvents(context.Background(), 10)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored events = %d, want 1", len(stored))
	}
	if !stored[0].SignatureValid {
		t.Error("the accepted event is not marked signature_valid")
	}
	if stored[0].ProcessedAt == nil {
		t.Error("the accepted event was never marked processed, so a replay would reprocess it")
	}
	if len(stored[0].PayloadJSON) == 0 {
		t.Error("the verified payload was not retained, so the case has no evidence behind it")
	}

	// SRS AC-005: the side effect is auditable and linked to the case.
	if !h.store.hasAudit("case_opened") {
		t.Errorf("no case_opened audit record; audit events = %v", h.store.auditEvents())
	}
	trail, err := h.store.ListAuditForCase(context.Background(), caseID)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(trail) == 0 {
		t.Error("the audit trail is not queryable by case id")
	}
	if h.store.counter("webhooks_received") != 1 {
		t.Errorf("webhooks_received = %d, want 1", h.store.counter("webhooks_received"))
	}
}

func TestIntegrationWebhookRejectsBadSignature(t *testing.T) {
	h := newHarness(t, testWebhookSecret)
	body := paymentFailedBody("pay_INTEG0002", "forged@example.com", 999_900, time.Now().UTC())

	cases := []struct {
		name      string
		signature string
	}{
		{"no signature header at all", ""},
		{"a signature computed with the wrong secret", razorpay.ComputeSignature(body, "whsec_attacker")},
		{"a syntactically valid but wrong hex digest", strings.Repeat("ab", 32)},
		{"a truncated digest", razorpay.ComputeSignature(body, testWebhookSecret)[:32]},
		{"garbage", "not-even-hex"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, decoded := h.postWebhookRaw(body, tc.signature, "evt_forged")
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body = %v", resp.StatusCode, decoded)
			}
		})
	}

	// No case was opened by any of them.
	if n := h.store.casesOpened(); n != 0 {
		t.Errorf("cases opened = %d, want 0 — an unverified webhook must not create work", n)
	}

	// Each rejection is persisted and counted, so a misconfigured secret is
	// visible on the ops screen rather than silently dropping every event
	// (SRS FR-002).
	if got, want := h.store.counter("webhook_signature_failures"), int64(len(cases)); got != want {
		t.Errorf("webhook_signature_failures = %d, want %d", got, want)
	}
	if !h.store.hasAudit("webhook_rejected") {
		t.Errorf("no webhook_rejected audit record; audit events = %v", h.store.auditEvents())
	}

	// The unverified bytes are recorded as an event but the payload is not
	// retained: storing attacker-controlled JSON as a payload would make it look
	// like data the system had accepted.
	stored, err := h.store.ListEvents(context.Background(), 20)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(stored) == 0 {
		t.Fatal("rejected webhooks left no trace at all")
	}
	for _, e := range stored {
		if e.SignatureValid {
			t.Errorf("event %s from a rejected delivery is marked signature_valid", e.ID)
		}
		if len(e.PayloadJSON) != 0 {
			t.Errorf("event %s retained an unverified payload", e.ID)
		}
		if e.RejectionReason == "" {
			t.Errorf("event %s has no rejection reason", e.ID)
		}
		if !strings.HasPrefix(e.ExternalEventID, "invalid:") {
			t.Errorf("external event id = %q, want an invalid: prefix so a forged id cannot "+
				"squat on a legitimate event's key", e.ExternalEventID)
		}
	}
}

// TestIntegrationWebhookRejectsTamperedBody is the test that would catch a
// verify-after-parse mistake.
//
// The body is signed, then a single digit of the amount is changed. The HMAC is
// computed over raw bytes, so this must fail — and if it ever passes, an attacker
// can rewrite the amount on an event the system trusts (SRS 19.3).
func TestIntegrationWebhookRejectsTamperedBody(t *testing.T) {
	h := newHarness(t, testWebhookSecret)

	original := paymentFailedBody("pay_INTEG0003", "tamper@example.com", 100_000, time.Now().UTC())
	signature := razorpay.ComputeSignature(original, testWebhookSecret)

	tampered := strings.Replace(string(original), `"amount":100000`, `"amount":900000`, 1)
	if tampered == string(original) {
		t.Fatal("the test could not find the amount to tamper with; the payload shape changed")
	}

	resp, decoded := h.postWebhookRaw([]byte(tampered), signature, "evt_tampered")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for a body that does not match its signature; body = %v",
			resp.StatusCode, decoded)
	}
	if n := h.store.casesOpened(); n != 0 {
		t.Errorf("cases opened = %d, want 0", n)
	}

	// Whitespace-only reserialisation must fail too. This is the specific mistake
	// of verifying against a re-marshalled struct instead of the wire bytes: the
	// JSON is semantically identical and the signature still must not verify.
	reindented := append([]byte(" "), original...)
	resp2, _ := h.postWebhookRaw(reindented, signature, "evt_reindented")
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d for a semantically identical but byte-different body, want 401", resp2.StatusCode)
	}
}

// TestIntegrationWebhookWithoutConfiguredSecret pins the fail-closed behaviour
// for a deployment that forgot the secret.
//
// Accepting the event would mean acting on unauthenticated instructions about
// money. 503 rather than 401 is chosen for the sender: Razorpay redelivers on a
// 503 once the secret is in place, where a 401 would say "never send this again"
// (SRS 20.4).
func TestIntegrationWebhookWithoutConfiguredSecret(t *testing.T) {
	h := newHarness(t, "")
	body := paymentFailedBody("pay_INTEG0004", "nosecret@example.com", 500_000, time.Now().UTC())

	resp, decoded := h.postWebhookRaw(body, razorpay.ComputeSignature(body, "anything"), "evt_nosecret")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %v", resp.StatusCode, decoded)
	}
	if n := h.store.casesOpened(); n != 0 {
		t.Errorf("cases opened = %d, want 0 — an unverifiable event must not create work", n)
	}
}

func TestIntegrationWebhookRejectsMalformedBody(t *testing.T) {
	h := newHarness(t, testWebhookSecret)

	// Correctly signed, but not a usable event. A 400 says "do not retry": a
	// redelivery of the same broken body would fail identically.
	cases := []struct {
		name string
		body string
	}{
		{"not JSON", `{"event":`},
		{"JSON with no event type", `{"entity":"event","payload":{}}`},
		{"an empty object", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, decoded := h.postWebhook([]byte(tc.body), testWebhookSecret, "evt_malformed")
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %v", resp.StatusCode, decoded)
			}
		})
	}

	// An empty body cannot be signed meaningfully, and is refused before anything
	// tries to parse it.
	resp, _ := h.postWebhook([]byte(""), testWebhookSecret, "evt_empty")
	if resp.StatusCode/100 != 4 {
		t.Errorf("status for an empty body = %d, want a 4xx", resp.StatusCode)
	}

	if n := h.store.casesOpened(); n != 0 {
		t.Errorf("cases opened = %d, want 0", n)
	}
}

// TestIntegrationWebhookIgnoresUnknownEventTypes checks the closed mapping.
//
// Razorpay adds event types over time. An unrecognised one is accepted with a 200
// and recorded, but no workflow runs — guessing at a partially-understood event is
// worse than ignoring it.
func TestIntegrationWebhookIgnoresUnknownEventTypes(t *testing.T) {
	h := newHarness(t, testWebhookSecret)

	body, err := json.Marshal(map[string]any{
		"entity":     "event",
		"event":      "refund.speed_changed",
		"created_at": time.Now().Unix(),
		"payload":    map[string]any{},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	resp, decoded := h.postWebhook(body, testWebhookSecret, "evt_unknown")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — refusing an unknown event would make Razorpay retry it forever; body = %v",
			resp.StatusCode, decoded)
	}
	if got := decoded["status"]; got != "ignored" {
		t.Errorf("status = %v, want ignored", got)
	}
	if decoded["reason"] == "" || decoded["reason"] == nil {
		t.Error("an ignored event carries no reason, so the log cannot explain the decision")
	}
	if n := h.store.casesOpened(); n != 0 {
		t.Errorf("cases opened = %d, want 0", n)
	}
}

// TestIntegrationWebhookRejectsFutureTimestamps guards against replay with a
// forward-dated envelope, which would otherwise let an attacker's event win the
// ordering check against every legitimate one that followed.
func TestIntegrationWebhookRejectsFutureTimestamps(t *testing.T) {
	h := newHarness(t, testWebhookSecret)

	future := time.Now().UTC().Add(2 * time.Hour)
	body := paymentFailedBody("pay_INTEG0005", "future@example.com", 300_000, future)

	resp, decoded := h.postWebhook(body, testWebhookSecret, "evt_future")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %v", resp.StatusCode, decoded)
	}
	if n := h.store.casesOpened(); n != 0 {
		t.Errorf("cases opened = %d, want 0", n)
	}
}

// --- SRS 22.2: duplicate event delivery (SRS AC-006) ---

func TestIntegrationDuplicateWebhookDeliveryIsIdempotent(t *testing.T) {
	h := newHarness(t, testWebhookSecret)
	body := paymentFailedBody("pay_INTEG0010", "duplicate@example.com", 2_500_000, time.Now().UTC())

	first, firstBody := h.postWebhook(body, testWebhookSecret, "evt_INTEG0010")
	if first.StatusCode != http.StatusOK {
		t.Fatalf("first delivery status = %d, want 200; body = %v", first.StatusCode, firstBody)
	}
	if firstBody["case_created"] != true {
		t.Fatalf("first delivery did not open a case: %v", firstBody)
	}
	firstCaseID, _ := firstBody["case_id"].(string)

	// Redeliver the identical bytes four more times, as Razorpay does when it does
	// not see a 2xx in time.
	for i := 0; i < 4; i++ {
		resp, decoded := h.postWebhook(body, testWebhookSecret, "evt_INTEG0010")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("redelivery %d status = %d, want 200 — a non-2xx would make Razorpay retry forever "+
				"over an event we already handled; body = %v", i+1, resp.StatusCode, decoded)
		}
		if got := decoded["status"]; got != "duplicate" {
			t.Errorf("redelivery %d status = %v, want duplicate", i+1, got)
		}
		if decoded["case_created"] == true {
			t.Errorf("redelivery %d reported creating a case", i+1)
		}
	}

	// One case, one stored event, four counted duplicates.
	if n := h.store.casesOpened(); n != 1 {
		t.Errorf("cases = %d, want 1 — five deliveries of one event produced %d cases", n, n)
	}
	stored, err := h.store.ListEvents(context.Background(), 20)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(stored) != 1 {
		t.Errorf("stored events = %d, want 1", len(stored))
	}
	if got := h.store.counter("duplicate_events"); got != 4 {
		t.Errorf("duplicate_events = %d, want 4", got)
	}
	if got := h.store.counter("webhooks_received"); got != 5 {
		t.Errorf("webhooks_received = %d, want 5", got)
	}

	// Exactly one case_opened audit record, so the trail does not claim the case
	// was opened five times.
	opened := 0
	for _, e := range h.store.auditEvents() {
		if e == "case_opened" {
			opened++
		}
	}
	if opened != 1 {
		t.Errorf("case_opened audit records = %d, want 1", opened)
	}
	if firstCaseID == "" {
		t.Error("the first delivery returned no case id")
	}
}

// TestIntegrationDuplicateEventWithDifferentEventIDHeader is the harder duplicate
// case.
//
// Razorpay's X-Razorpay-Event-Id is the primary dedup key, but a redelivery that
// arrives with a different id — or none at all — must still not open a second
// case. The second line of defence is the source record: one open case per
// transaction, enforced by a unique index (SRS FR-003, AC-006).
func TestIntegrationDuplicateEventWithDifferentEventIDHeader(t *testing.T) {
	h := newHarness(t, testWebhookSecret)
	at := time.Now().UTC()

	// The same payment, delivered three times: with an event id, with a different
	// one, and with none.
	deliveries := []string{"evt_first", "evt_second_different_id", ""}
	caseIDs := map[string]bool{}

	for i, eventID := range deliveries {
		// A distinct created_at each time, so the bodies differ and the body hash
		// cannot be what deduplicates them. This is deliberately the hostile case.
		body := paymentFailedBody("pay_INTEG0011", "samepayment@example.com", 1_800_000, at.Add(time.Duration(i)*time.Second))
		resp, decoded := h.postWebhook(body, testWebhookSecret, eventID)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("delivery %d status = %d; body = %v", i, resp.StatusCode, decoded)
		}
		if id, _ := decoded["case_id"].(string); id != "" {
			caseIDs[id] = true
		}
	}

	if len(caseIDs) != 1 {
		t.Errorf("distinct case ids = %d (%v), want 1", len(caseIDs), caseIDs)
	}
	if n := h.store.casesOpened(); n != 1 {
		t.Errorf("cases = %d, want 1 — the same failed payment must map to one case however it is delivered", n)
	}

	// And only one transaction row, since the payment id is the same.
	if _, err := h.store.FindTransactionByRazorpayID(context.Background(), "pay_INTEG0011"); err != nil {
		t.Fatalf("the transaction was not recorded: %v", err)
	}
	h.store.mu.Lock()
	txnCount := len(h.store.txns)
	h.store.mu.Unlock()
	if txnCount != 1 {
		t.Errorf("transaction rows = %d, want 1", txnCount)
	}
}

// TestIntegrationConcurrentDuplicateDeliveries drives the race.
//
// Razorpay can have several redelivery attempts in flight at once. If dedup were
// a read-then-write in application code, two of these would both find nothing and
// both insert. Because it is a uniqueness constraint, exactly one wins.
func TestIntegrationConcurrentDuplicateDeliveries(t *testing.T) {
	h := newHarness(t, testWebhookSecret)
	body := paymentFailedBody("pay_INTEG0012", "concurrent@example.com", 3_000_000, time.Now().UTC())

	const n = 8
	var wg sync.WaitGroup
	statuses := make([]int, n)
	created := make([]bool, n)

	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			resp, decoded := h.postWebhook(body, testWebhookSecret, "evt_INTEG0012")
			statuses[idx] = resp.StatusCode
			created[idx], _ = decoded["case_created"].(bool)
		}(i)
	}
	wg.Wait()

	for i, s := range statuses {
		if s != http.StatusOK {
			t.Errorf("concurrent delivery %d status = %d, want 200", i, s)
		}
	}
	wins := 0
	for _, c := range created {
		if c {
			wins++
		}
	}
	if wins != 1 {
		t.Errorf("deliveries reporting case_created = %d, want exactly 1", wins)
	}
	if got := h.store.casesOpened(); got != 1 {
		t.Errorf("cases = %d, want 1", got)
	}
	if got := h.store.counter("duplicate_events"); got != n-1 {
		t.Errorf("duplicate_events = %d, want %d", got, n-1)
	}
}

// TestIntegrationOutOfOrderDeliveryIsRecordedNotApplied covers SRS FR-004.
//
// A redelivery describing an older state than one already applied must not
// un-capture a payment. It is stored — dropping the fact silently would be worse —
// but not acted on.
func TestIntegrationOutOfOrderDeliveryIsRecordedNotApplied(t *testing.T) {
	h := newHarness(t, testWebhookSecret)
	now := time.Now().UTC()

	// The newer event first.
	newer := paymentFailedBody("pay_INTEG0013", "ordering@example.com", 1_000_000, now)
	resp, decoded := h.postWebhook(newer, testWebhookSecret, "evt_newer")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("newer delivery status = %d; body = %v", resp.StatusCode, decoded)
	}

	// Then one describing the same entity an hour earlier.
	older := paymentFailedBody("pay_INTEG0013", "ordering@example.com", 1_000_000, now.Add(-time.Hour))
	resp2, decoded2 := h.postWebhook(older, testWebhookSecret, "evt_older")
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("older delivery status = %d; body = %v", resp2.StatusCode, decoded2)
	}
	if got := decoded2["status"]; got != "stale" {
		t.Errorf("status = %v, want stale", got)
	}

	// Both events retained, one case.
	stored, err := h.store.ListEvents(context.Background(), 20)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(stored) != 2 {
		t.Errorf("stored events = %d, want 2 — the stale event must still be recorded", len(stored))
	}
	if n := h.store.casesOpened(); n != 1 {
		t.Errorf("cases = %d, want 1", n)
	}
	if !h.store.hasAudit("event_out_of_order") {
		t.Errorf("no event_out_of_order audit record; audit events = %v", h.store.auditEvents())
	}
}

// --- SRS 22.2: subscription test workflow ---

func TestIntegrationSubscriptionHaltedOpensCase(t *testing.T) {
	h := newHarness(t, testWebhookSecret)
	body := subscriptionHaltedBody("sub_INTEG0020", 499_900, time.Now().UTC())

	resp, decoded := h.postWebhook(body, testWebhookSecret, "evt_INTEG0020")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %v", resp.StatusCode, decoded)
	}
	if decoded["case_created"] != true {
		t.Fatalf("a halted subscription did not open a case: %v", decoded)
	}
	caseID, _ := decoded["case_id"].(string)

	rc, err := h.store.GetCase(context.Background(), caseID)
	if err != nil {
		t.Fatalf("get case: %v", err)
	}
	if rc.SourceType != domain.SourceSubscriptionFailure {
		t.Errorf("source type = %s, want %s", rc.SourceType, domain.SourceSubscriptionFailure)
	}
	if rc.SubscriptionID == "" {
		t.Error("the case does not point at the subscription it was opened from")
	}
	if rc.RevenueAtRisk != domain.Money(499_900) {
		t.Errorf("revenue at risk = %d paise, want 499900 — the amount must come from the "+
			"subscription record, not from anything derived", int64(rc.RevenueAtRisk))
	}

	// The subscription record was normalized into the local store.
	sub, err := h.store.FindSubscriptionByRazorpayID(context.Background(), "sub_INTEG0020")
	if err != nil {
		t.Fatalf("the subscription was not recorded: %v", err)
	}
	if sub.Status != "halted" {
		t.Errorf("stored subscription status = %q, want halted", sub.Status)
	}
	if sub.Environment != domain.EnvTest {
		t.Errorf("stored subscription environment = %s, want test", sub.Environment)
	}

	// Redelivery opens nothing further.
	resp2, decoded2 := h.postWebhook(body, testWebhookSecret, "evt_INTEG0020")
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("redelivery status = %d; body = %v", resp2.StatusCode, decoded2)
	}
	if n := h.store.casesOpened(); n != 1 {
		t.Errorf("cases after redelivery = %d, want 1", n)
	}
}

// --- the webhook route's place in the auth model ---

// TestIntegrationWebhookRouteTakesNoBearerToken states the design explicitly.
//
// The webhook is authenticated by HMAC over the raw body, not by a bearer token,
// because Razorpay has no way to hold one. That means the route sits outside the
// authenticated group — and the risk of that arrangement is a route accidentally
// mounted there later, so this test asserts the boundary from the other side:
// the webhook works with no Authorization header, and a protected route does not.
func TestIntegrationWebhookRouteTakesNoBearerToken(t *testing.T) {
	h := newHarness(t, testWebhookSecret)

	body := paymentFailedBody("pay_INTEG0030", "noauth@example.com", 750_000, time.Now().UTC())
	resp, decoded := h.postWebhook(body, testWebhookSecret, "evt_INTEG0030")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the webhook required authentication: status = %d; body = %v", resp.StatusCode, decoded)
	}

	// The same absence of a token on an operator route is a 401.
	protected, err := h.srv.Client().Get(h.srv.URL + "/api/cases")
	if err != nil {
		t.Fatalf("get cases: %v", err)
	}
	defer protected.Body.Close()
	if protected.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /api/cases without a token = %d, want 401", protected.StatusCode)
	}

	// Health and version stay public, so a load balancer and a demo audience can
	// both read them without credentials.
	for _, path := range []string{"/api/health", "/api/version"} {
		r, err := h.srv.Client().Get(h.srv.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		_ = r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, r.StatusCode)
		}
	}
}
