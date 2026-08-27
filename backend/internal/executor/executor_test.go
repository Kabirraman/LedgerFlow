package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ledgerflow/ledgerflow/internal/domain"
	"github.com/ledgerflow/ledgerflow/internal/idem"
	"github.com/ledgerflow/ledgerflow/internal/razorpay"
)

// journal records every call made to the fakes below, in order, across both the
// store and the gateway.
//
// A shared log rather than two separate ones is what makes the ordering assertions
// possible: "the row was reserved before the external call" is a claim about the
// interleaving of two different dependencies, and it is the property that makes a
// crashed execution reconcilable (SRS FR-043).
type journal struct{ calls []string }

func (j *journal) record(name string) { j.calls = append(j.calls, name) }

func (j *journal) has(prefix string) bool {
	for _, c := range j.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (j *journal) indexOf(prefix string) int {
	for i, c := range j.calls {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

func (j *journal) String() string { return strings.Join(j.calls, " → ") }

// gatewayCalls returns only the external-transport calls. Everything the spy
// gateway implements is a network call in production, so any entry here is a
// side effect that reached outside the process.
func (j *journal) gatewayCalls() []string {
	var out []string
	for _, c := range j.calls {
		if strings.HasPrefix(c, "gateway.") {
			out = append(out, c)
		}
	}
	return out
}

// fakeStore is the executor's persistence surface. It behaves like the real one in
// the single respect the executor depends on: ReserveAction reports whether the
// idempotency key was new, and hands back the existing row when it was not.
type fakeStore struct {
	j *journal

	// existing, when set, is the row a duplicate reserve returns.
	existing *domain.RecoveryAction
	// reserveErr forces the reserve to fail.
	reserveErr error
	// markExecutedErr simulates a write failure after the side effect happened.
	markExecutedErr error

	reserved []domain.RecoveryAction
	counters map[string]int
	audits   []string
}

func newFakeStore(j *journal) *fakeStore {
	return &fakeStore{j: j, counters: map[string]int{}}
}

func (s *fakeStore) ReserveAction(_ context.Context, a *domain.RecoveryAction) (bool, error) {
	s.j.record("store.ReserveAction:" + a.IdempotencyKey)
	if s.reserveErr != nil {
		return false, s.reserveErr
	}
	if s.existing != nil && s.existing.IdempotencyKey == a.IdempotencyKey {
		// The real store overwrites the caller's struct with the persisted row, so
		// the caller reads the first request's result rather than its own input.
		*a = *s.existing
		return false, nil
	}
	a.ID = "action-" + a.IdempotencyKey
	s.reserved = append(s.reserved, *a)
	return true, nil
}

func (s *fakeStore) MarkActionExecuted(_ context.Context, id, externalID, externalURL string, latencyMS int64) error {
	s.j.record("store.MarkActionExecuted:" + id + ":" + externalID)
	return s.markExecutedErr
}

func (s *fakeStore) MarkActionFailed(_ context.Context, id, code, message string, latencyMS int64) error {
	s.j.record("store.MarkActionFailed:" + id + ":" + code)
	return nil
}

func (s *fakeStore) MarkActionAmbiguous(_ context.Context, id, code, message string, latencyMS int64) error {
	s.j.record("store.MarkActionAmbiguous:" + id + ":" + code)
	return nil
}

func (s *fakeStore) MarkActionSkipped(_ context.Context, id, reason string) error {
	s.j.record("store.MarkActionSkipped:" + id)
	return nil
}

func (s *fakeStore) IncrementCaseActionCount(_ context.Context, caseID string) error {
	s.j.record("store.IncrementCaseActionCount:" + caseID)
	return nil
}

func (s *fakeStore) IncrementInvoiceReminder(_ context.Context, id string) error {
	s.j.record("store.IncrementInvoiceReminder:" + id)
	return nil
}

func (s *fakeStore) RecordStrategyAttempt(_ context.Context, seg domain.Segment,
	st domain.SourceType, at domain.ActionType) error {
	s.j.record("store.RecordStrategyAttempt:" + string(at))
	return nil
}

func (s *fakeStore) Audit(_ context.Context, actor, entityType, entityID, caseID, eventType string, detail any) error {
	s.j.record("store.Audit:" + eventType)
	s.audits = append(s.audits, eventType)
	return nil
}

func (s *fakeStore) IncrCounter(_ context.Context, name string) error {
	s.counters[name]++
	return nil
}

func (s *fakeStore) AddCounter(_ context.Context, name string, n, sum int64) error {
	s.counters[name] += int(n)
	return nil
}

// spyGateway implements the full razorpay.Gateway surface and records every call.
//
// It reports External() == true — that is deliberate. Several tests below assert
// that the executor refuses a simulation-mode request rather than calling out, and
// a fake that claimed to be internal would make those tests pass for the wrong
// reason.
type spyGateway struct {
	j *journal

	external  bool
	linkErr   error
	notifyErr error
	link      *razorpay.PaymentLink
	// lastLinkRequest captures what was actually sent to the gateway, so the
	// amount and reference id on the wire can be checked rather than inferred.
	lastLinkRequest razorpay.PaymentLinkRequest
}

func newSpyGateway(j *journal) *spyGateway {
	return &spyGateway{
		j:        j,
		external: true,
		link: &razorpay.PaymentLink{
			ID:       "plink_TEST1",
			ShortURL: "https://rzp.io/i/TEST1",
			Status:   "created",
		},
	}
}

func (g *spyGateway) Name() string   { return "spy" }
func (g *spyGateway) External() bool { return g.external }

func (g *spyGateway) CreatePaymentLink(_ context.Context, req razorpay.PaymentLinkRequest) (*razorpay.PaymentLink, error) {
	g.j.record("gateway.CreatePaymentLink")
	g.lastLinkRequest = req
	if g.linkErr != nil {
		return nil, g.linkErr
	}
	link := *g.link
	link.Amount = req.Amount
	link.ReferenceID = req.ReferenceID
	return &link, nil
}

func (g *spyGateway) NotifyInvoice(_ context.Context, id, medium string) error {
	g.j.record("gateway.NotifyInvoice:" + id + ":" + medium)
	return g.notifyErr
}

// The remaining methods are not on any path this package exercises. They record
// the call and fail loudly rather than returning a zero value, so a future change
// that starts using one shows up as a named failure instead of a nil dereference.
func (g *spyGateway) FetchPaymentLink(_ context.Context, id string) (*razorpay.PaymentLink, error) {
	g.j.record("gateway.FetchPaymentLink")
	return nil, errors.New("spy: unexpected call")
}

func (g *spyGateway) FindPaymentLinkByReference(_ context.Context, ref string) (*razorpay.PaymentLink, error) {
	g.j.record("gateway.FindPaymentLinkByReference")
	return nil, errors.New("spy: unexpected call")
}

func (g *spyGateway) NotifyPaymentLink(_ context.Context, id, medium string) error {
	g.j.record("gateway.NotifyPaymentLink")
	return errors.New("spy: unexpected call")
}

func (g *spyGateway) CancelPaymentLink(_ context.Context, id string) (*razorpay.PaymentLink, error) {
	g.j.record("gateway.CancelPaymentLink")
	return nil, errors.New("spy: unexpected call")
}

func (g *spyGateway) FetchPayment(_ context.Context, id string) (*razorpay.Payment, error) {
	g.j.record("gateway.FetchPayment")
	return nil, errors.New("spy: unexpected call")
}

func (g *spyGateway) ListPayments(_ context.Context, from, to time.Time, count int) ([]razorpay.Payment, error) {
	g.j.record("gateway.ListPayments")
	return nil, errors.New("spy: unexpected call")
}

func (g *spyGateway) CreateInvoice(_ context.Context, req razorpay.InvoiceRequest) (*razorpay.Invoice, error) {
	g.j.record("gateway.CreateInvoice")
	return nil, errors.New("spy: unexpected call")
}

func (g *spyGateway) FetchInvoice(_ context.Context, id string) (*razorpay.Invoice, error) {
	g.j.record("gateway.FetchInvoice")
	return nil, errors.New("spy: unexpected call")
}

func (g *spyGateway) IssueInvoice(_ context.Context, id string) (*razorpay.Invoice, error) {
	g.j.record("gateway.IssueInvoice")
	return nil, errors.New("spy: unexpected call")
}

func (g *spyGateway) FetchSubscription(_ context.Context, id string) (*razorpay.Subscription, error) {
	g.j.record("gateway.FetchSubscription")
	return nil, errors.New("spy: unexpected call")
}

func (g *spyGateway) CancelSubscription(_ context.Context, id string, atCycleEnd bool) (*razorpay.Subscription, error) {
	g.j.record("gateway.CancelSubscription")
	return nil, errors.New("spy: unexpected call")
}

// harness bundles an executor with its fakes and the shared journal.
type harness struct {
	exec    *Executor
	store   *fakeStore
	gateway *spyGateway
	j       *journal
}

var fixedNow = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

func newHarness(t *testing.T) *harness {
	t.Helper()
	j := &journal{}
	store := newFakeStore(j)
	gw := newSpyGateway(j)
	e := New(store, gw, Config{CallbackURL: "https://demo.local/callback", NotifyEmail: true})
	e.SetClock(func() time.Time { return fixedNow })
	return &harness{exec: e, store: store, gateway: gw, j: j}
}

// validRequest is an approved payment link that executes cleanly. Every test
// mutates one field so a failure identifies the guard that changed behaviour.
func validRequest() Request {
	return Request{
		CaseID:        "case-1",
		Action:        domain.ActionPaymentLink,
		Approved:      true,
		PolicyVersion: "v1",
		TargetAmount:  domain.Money(250_000),
		TrustedAmount: domain.Money(250_000),
		DecisionID:    "dec-1",
		Mode:          domain.ModeLiveTest,
		CustomerID:    "cust-1",
		CustomerName:  "Test Customer",
		CustomerEmail: "customer@example.com",
		Segment:       domain.SegmentRepeat,
		SourceType:    domain.SourcePaymentFailure,
		Attempt:       1,
	}
}

func TestExecuteHappyPath(t *testing.T) {
	h := newHarness(t)
	res, err := h.exec.Execute(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("Execute: %v\njournal: %s", err, h.j)
	}
	if !res.Executed() {
		t.Fatalf("Executed() = false, status %s rejected %v", res.Status, res.Rejected)
	}
	if res.ExternalID != "plink_TEST1" || res.ExternalURL == "" {
		t.Errorf("external id/url = %q/%q, want the gateway's values", res.ExternalID, res.ExternalURL)
	}
	if res.Duplicate {
		t.Error("Duplicate is true on a first execution")
	}
	if h.store.counters[counterActionsExecuted] != 1 {
		t.Errorf("actions_executed = %d, want 1", h.store.counters[counterActionsExecuted])
	}
	// The attempt is counted whether or not it later recovers, or every strategy
	// would show a 100% success rate (SRS FR-053).
	if !h.j.has("store.RecordStrategyAttempt") {
		t.Error("no strategy attempt recorded")
	}
	if !h.j.has("store.Audit:action_executed") {
		t.Errorf("no audit entry for the side effect (AC-005)\njournal: %s", h.j)
	}
}

// TestReserveHappensBeforeTheExternalCall is SRS FR-043 as an ordering assertion.
//
// If the call went first, a crash or a lost response would leave money-moving
// state at the gateway with nothing in the database pointing at it — unauditable
// and invisible to the reconciler. Reserving first means the worst case is a
// stranded 'pending' row, which is recoverable.
func TestReserveHappensBeforeTheExternalCall(t *testing.T) {
	h := newHarness(t)
	if _, err := h.exec.Execute(context.Background(), validRequest()); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	reserve := h.j.indexOf("store.ReserveAction")
	call := h.j.indexOf("gateway.CreatePaymentLink")
	mark := h.j.indexOf("store.MarkActionExecuted")

	if reserve < 0 || call < 0 || mark < 0 {
		t.Fatalf("expected reserve, call and mark; got %s", h.j)
	}
	if reserve > call {
		t.Errorf("the gateway was called before the action row was reserved: %s", h.j)
	}
	if call > mark {
		t.Errorf("the action was marked executed before the call was made: %s", h.j)
	}
}

// TestSimulationModeCannotReachRazorpay is AC-009 and the last item of the
// SRS 22.4 safety list.
//
// The assertion is not that the request fails — it is that no gateway method was
// invoked at all. A test that only checked the error could still pass while a
// payment link was created and then discarded.
func TestSimulationModeCannotReachRazorpay(t *testing.T) {
	for _, action := range []domain.ActionType{domain.ActionPaymentLink, domain.ActionRetry, domain.ActionReminder} {
		h := newHarness(t)
		h.gateway.external = true // an external transport wired into a simulation run

		req := validRequest()
		req.Action = action
		req.Mode = domain.ModeSimulation
		req.RazorpayResourceID = "pay_TEST1" // so the retry guard is not what refuses it

		res, err := h.exec.Execute(context.Background(), req)
		if err == nil {
			t.Fatalf("%s: simulation mode executed against an external gateway", action)
		}
		if !errors.Is(err, domain.ErrActionNotAllowed) {
			t.Errorf("%s: error %v does not wrap ErrActionNotAllowed", action, err)
		}
		if !res.Rejected {
			t.Errorf("%s: Rejected = false", action)
		}
		if calls := h.j.gatewayCalls(); len(calls) != 0 {
			t.Errorf("%s: simulation run made %d gateway calls: %v", action, len(calls), calls)
		}
		if !strings.Contains(res.RejectReason, "simulation") {
			t.Errorf("%s: reject reason %q does not name the simulation boundary", action, res.RejectReason)
		}
	}

	// The corollary: a simulation run against a non-external gateway is fine. The
	// boundary is about the transport, not about refusing to simulate.
	h := newHarness(t)
	h.gateway.external = false
	req := validRequest()
	req.Mode = domain.ModeSimulation
	if res, err := h.exec.Execute(context.Background(), req); err != nil || !res.Executed() {
		t.Errorf("simulation against an internal gateway failed: %v (status %s)", err, res.Status)
	}
}

// TestDuplicateActionReturnsTheExistingResult is AC-006 and the SRS 22.4
// duplicate-action item.
//
// The second request must return the first request's payment link, not a new one.
// Getting this wrong bills the customer's attention twice for one debt, which is
// the failure mode the whole idempotency design exists to prevent.
func TestDuplicateActionReturnsTheExistingResult(t *testing.T) {
	h := newHarness(t)
	req := validRequest()
	key := idem.ActionKey(req.CaseID, req.Action, req.Attempt)

	// The row a previous request already committed.
	executedAt := fixedNow.Add(-time.Minute)
	h.store.existing = &domain.RecoveryAction{
		ID:             "action-original",
		CaseID:         req.CaseID,
		ActionType:     req.Action,
		IdempotencyKey: key,
		ExternalID:     "plink_ORIGINAL",
		ExternalURL:    "https://rzp.io/i/ORIGINAL",
		Amount:         req.TargetAmount,
		Status:         domain.ActionStatusExecuted,
		LatencyMS:      412,
		ExecutedAt:     &executedAt,
	}

	res, err := h.exec.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("a duplicate request returned an error: %v", err)
	}
	if !res.Duplicate {
		t.Error("Duplicate = false on a replayed request")
	}
	if res.ActionID != "action-original" {
		t.Errorf("ActionID = %q, want the original action", res.ActionID)
	}
	if res.ExternalID != "plink_ORIGINAL" || res.ExternalURL != "https://rzp.io/i/ORIGINAL" {
		t.Errorf("returned %q/%q, want the original link", res.ExternalID, res.ExternalURL)
	}
	if res.Status != domain.ActionStatusExecuted {
		t.Errorf("Status = %s, want the original executed status", res.Status)
	}

	// The important half: no second side effect, and no second count.
	if calls := h.j.gatewayCalls(); len(calls) != 0 {
		t.Errorf("a duplicate request made %d gateway calls: %v", len(calls), calls)
	}
	if h.j.has("store.IncrementCaseActionCount") {
		t.Error("a duplicate request incremented the case action count")
	}
	if h.store.counters[counterActionsExecuted] != 0 {
		t.Error("a duplicate request counted a second execution, inflating the reported action total")
	}
	if h.store.counters[counterDuplicateActionRequests] != 1 {
		t.Errorf("duplicate_action_requests = %d, want 1", h.store.counters[counterDuplicateActionRequests])
	}
	if !h.j.has("store.Audit:duplicate_action_request") {
		t.Error("the duplicate was not audited; a suppressed replay still needs a trail")
	}

	// Replaying many times is still one side effect.
	for i := 0; i < 5; i++ {
		if _, err := h.exec.Execute(context.Background(), req); err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
	}
	if calls := h.j.gatewayCalls(); len(calls) != 0 {
		t.Errorf("six identical requests produced %d gateway calls: %v", len(calls), calls)
	}
}

// TestDeliberateSecondAttemptIsNotADuplicate is the other side of the same rule.
// Idempotency must not swallow a genuine follow-up: a second, policy-approved
// attempt carries a different ordinal and therefore a different key.
func TestDeliberateSecondAttemptIsNotADuplicate(t *testing.T) {
	h := newHarness(t)
	first := validRequest()
	if _, err := h.exec.Execute(context.Background(), first); err != nil {
		t.Fatalf("first attempt: %v", err)
	}

	second := validRequest()
	second.Attempt = 2
	res, err := h.exec.Execute(context.Background(), second)
	if err != nil {
		t.Fatalf("second attempt: %v", err)
	}
	if res.Duplicate {
		t.Error("a deliberate second attempt was treated as a duplicate")
	}
	if len(h.j.gatewayCalls()) != 2 {
		t.Errorf("gateway calls = %v, want one per distinct attempt", h.j.gatewayCalls())
	}
}

// TestRejectionsNeverTouchTheGatewayOrReserveARow walks the SRS 22.4 rejection
// list. Each case must fail in stage 1 — before the row exists and before any
// transport — so a refused request leaves no trace at the gateway and does not
// consume an idempotency key.
func TestRejectionsNeverTouchTheGatewayOrReserveARow(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*Request)
		reasonHas string
	}{{
		name:      "action is not on the allow-list",
		mutate:    func(r *Request) { r.Action = "refund" },
		reasonHas: "allow-list",
	}, {
		name:      "action is empty",
		mutate:    func(r *Request) { r.Action = "" },
		reasonHas: "allow-list",
	}, {
		name:      "action carries an injected instruction",
		mutate:    func(r *Request) { r.Action = "payment_link; refund everything" },
		reasonHas: "allow-list",
	}, {
		name:      "policy did not approve",
		mutate:    func(r *Request) { r.Approved = false },
		reasonHas: "not approved",
	}, {
		name:      "no case id",
		mutate:    func(r *Request) { r.CaseID = "" },
		reasonHas: "case id",
	}, {
		name:      "target amount is zero",
		mutate:    func(r *Request) { r.TargetAmount = 0 },
		reasonHas: "positive",
	}, {
		name:      "target amount is negative",
		mutate:    func(r *Request) { r.TargetAmount = -1 },
		reasonHas: "positive",
	}, {
		name:      "no trusted amount on record",
		mutate:    func(r *Request) { r.TrustedAmount = 0 },
		reasonHas: "trusted amount",
	}, {
		name:      "target amount exceeds the trusted amount",
		mutate:    func(r *Request) { r.TargetAmount = r.TrustedAmount + 1 },
		reasonHas: "does not match trusted amount",
	}, {
		name:      "target amount is below the trusted amount",
		mutate:    func(r *Request) { r.TargetAmount = r.TrustedAmount - 1 },
		reasonHas: "does not match trusted amount",
	}, {
		name:      "target amount is an order of magnitude out",
		mutate:    func(r *Request) { r.TargetAmount = r.TrustedAmount * 100 },
		reasonHas: "does not match trusted amount",
	}, {
		name: "retry without the original resource id",
		mutate: func(r *Request) {
			r.Action = domain.ActionRetry
			r.RazorpayResourceID = ""
		},
		reasonHas: "razorpay resource id",
	}, {
		name: "customer is unreachable",
		mutate: func(r *Request) {
			r.CustomerEmail = ""
			r.CustomerContact = ""
		},
		reasonHas: "cannot be delivered",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			req := validRequest()
			tc.mutate(&req)

			res, err := h.exec.Execute(context.Background(), req)
			if err == nil {
				t.Fatalf("Execute returned no error\njournal: %s", h.j)
			}
			if !errors.Is(err, domain.ErrActionNotAllowed) {
				t.Errorf("error %v does not wrap ErrActionNotAllowed", err)
			}
			if !res.Rejected {
				t.Error("Rejected = false")
			}
			if res.Status != domain.ActionStatusSkipped {
				t.Errorf("Status = %s, want skipped", res.Status)
			}
			if res.Executed() {
				t.Error("Executed() = true on a rejected request")
			}
			if !strings.Contains(res.RejectReason, tc.reasonHas) {
				t.Errorf("RejectReason = %q, want it to mention %q", res.RejectReason, tc.reasonHas)
			}
			if calls := h.j.gatewayCalls(); len(calls) != 0 {
				t.Errorf("a rejected request made gateway calls: %v", calls)
			}
			if h.j.has("store.ReserveAction") {
				t.Error("a rejected request reserved an action row, consuming an idempotency key")
			}
			if h.store.counters[counterActionsRejected] != 1 {
				t.Errorf("actions_rejected = %d, want 1", h.store.counters[counterActionsRejected])
			}
			if !h.j.has("store.Audit:action_rejected") {
				t.Error("the rejection was not audited")
			}
		})
	}
}

// TestAmountOnTheWireIsTheTrustedAmount is SRS 19.2 checked at the boundary.
//
// The previous test proves a mismatched amount is refused. This one proves the
// accepted path does not transform the amount on its way out: what reaches
// Razorpay is exactly the integer paise from the source record.
func TestAmountOnTheWireIsTheTrustedAmount(t *testing.T) {
	for _, amount := range []domain.Money{1, 99, 250_000, 4_999_999, 90_000_000} {
		h := newHarness(t)
		req := validRequest()
		req.TargetAmount, req.TrustedAmount = amount, amount

		if _, err := h.exec.Execute(context.Background(), req); err != nil {
			t.Fatalf("amount %d: %v", amount, err)
		}
		if got := h.gateway.lastLinkRequest.Amount; got != amount {
			t.Errorf("gateway received amount %d, want the trusted %d", got, amount)
		}
		if got := h.gateway.lastLinkRequest.Currency; got != "INR" {
			t.Errorf("currency = %q, want INR", got)
		}
	}
}

// TestIdempotencyKeyTravelsToRazorpay closes the loop on duplicate prevention.
//
// The local unique index stops a second row. The reference id stops a second
// resource at the gateway even if the row is somehow lost — so the key has to be
// on the wire, not only in the database.
func TestIdempotencyKeyTravelsToRazorpay(t *testing.T) {
	h := newHarness(t)
	req := validRequest()
	if _, err := h.exec.Execute(context.Background(), req); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	want := idem.ActionKey(req.CaseID, req.Action, req.Attempt)
	if got := h.gateway.lastLinkRequest.ReferenceID; got != want {
		t.Errorf("reference id on the wire = %q, want the idempotency key %q", got, want)
	}
	if got := h.gateway.lastLinkRequest.Notes["idempotency_key"]; got != want {
		t.Errorf("notes[idempotency_key] = %q, want %q", got, want)
	}
	if got := h.gateway.lastLinkRequest.Notes["ledgerflow_case"]; got != req.CaseID {
		t.Errorf("notes[ledgerflow_case] = %q, want %q", got, req.CaseID)
	}

	// An explicitly supplied key wins, so the orchestrator can pin one across a
	// retry of its own workflow step.
	h2 := newHarness(t)
	req2 := validRequest()
	req2.IdempotencyKey = "ledgerflow_pinned_key_1"
	if _, err := h2.exec.Execute(context.Background(), req2); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := h2.gateway.lastLinkRequest.ReferenceID; got != "ledgerflow_pinned_key_1" {
		t.Errorf("reference id = %q, want the supplied key", got)
	}
}

// TestNonExternalActionsAreRecordedWithoutAGatewayCall covers escalate and
// no_action. Both must leave a row — "we decided not to act" is a decision an
// operator has to be able to see — and neither may produce a side effect.
func TestNonExternalActionsAreRecordedWithoutAGatewayCall(t *testing.T) {
	for _, action := range []domain.ActionType{domain.ActionEscalate, domain.ActionNoAction} {
		h := newHarness(t)
		req := validRequest()
		req.Action = action
		// Deliberately hostile inputs for the money rules: none of them apply to a
		// non-external action, and none may cause a call either.
		req.TargetAmount, req.TrustedAmount = 0, 0
		req.CustomerEmail, req.CustomerContact = "", ""

		res, err := h.exec.Execute(context.Background(), req)
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		if res.Rejected {
			t.Errorf("%s: rejected (%s)", action, res.RejectReason)
		}
		if res.Status != domain.ActionStatusSkipped {
			t.Errorf("%s: Status = %s, want skipped", action, res.Status)
		}
		if res.Executed() {
			t.Errorf("%s: Executed() = true, but nothing external happened", action)
		}
		if calls := h.j.gatewayCalls(); len(calls) != 0 {
			t.Errorf("%s: made gateway calls: %v", action, calls)
		}
		if !h.j.has("store.ReserveAction") {
			t.Errorf("%s: no row was recorded; the decision would be invisible", action)
		}
		if !h.j.has("store.MarkActionSkipped") {
			t.Errorf("%s: the row was left pending rather than marked skipped", action)
		}
		if res.Amount != 0 {
			t.Errorf("%s: Amount = %d, want 0 for an action that moves no money", action, res.Amount)
		}
	}
}

// TestGatewayFailureClassification is the SRS 20.2 distinction that decides
// whether a retry is safe.
//
// A definite 4xx means no resource was created, so the case can be retried. A
// timeout or a 5xx means a payment link may exist that we hold no id for, and
// retrying would create a second one — so the action is parked as ambiguous for
// the reconciler instead.
func TestGatewayFailureClassification(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus domain.ActionStatus
		wantMark   string
	}{{
		name:       "definite validation error",
		err:        &razorpay.APIError{StatusCode: 400, Code: "BAD_REQUEST_ERROR", Description: "amount is invalid"},
		wantStatus: domain.ActionStatusFailed,
		wantMark:   "store.MarkActionFailed",
	}, {
		name:       "server error flagged ambiguous",
		err:        &razorpay.APIError{StatusCode: 502, Code: "SERVER_ERROR", Ambiguous: true},
		wantStatus: domain.ActionStatusAmbiguous,
		wantMark:   "store.MarkActionAmbiguous",
	}, {
		name:       "deadline exceeded",
		err:        context.DeadlineExceeded,
		wantStatus: domain.ActionStatusAmbiguous,
		wantMark:   "store.MarkActionAmbiguous",
	}, {
		name:       "context cancelled",
		err:        context.Canceled,
		wantStatus: domain.ActionStatusAmbiguous,
		wantMark:   "store.MarkActionAmbiguous",
	}, {
		name:       "unclassified transport error",
		err:        errors.New("dial tcp: connection reset by peer"),
		wantStatus: domain.ActionStatusAmbiguous,
		wantMark:   "store.MarkActionAmbiguous",
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.gateway.linkErr = tc.err

			res, err := h.exec.Execute(context.Background(), validRequest())
			if err == nil {
				t.Fatal("Execute returned no error after a gateway failure")
			}
			if res.Status != tc.wantStatus {
				t.Errorf("Status = %s, want %s", res.Status, tc.wantStatus)
			}
			if !h.j.has(tc.wantMark) {
				t.Errorf("expected %s\njournal: %s", tc.wantMark, h.j)
			}
			// A failed action must never be counted as executed, and the case
			// action count must not move: both feed the reported recovery numbers.
			if h.store.counters[counterActionsExecuted] != 0 {
				t.Error("a failed action was counted as executed")
			}
			if h.j.has("store.IncrementCaseActionCount") {
				t.Error("a failed action incremented the case action count")
			}
			if h.store.counters[counterActionAPIFailures] != 1 {
				t.Errorf("action_api_failures = %d, want 1", h.store.counters[counterActionAPIFailures])
			}
			// The row was reserved before the call, so it survives to be reconciled.
			if !h.j.has("store.ReserveAction") {
				t.Error("no row exists for the failed attempt; it cannot be reconciled")
			}
		})
	}
}

// TestUnrecordableSuccessSurfacesAnError covers the nastiest ordering: the side
// effect happened and the write recording it failed.
//
// Returning nil here would be the worst possible answer — the caller would believe
// nothing happened while a live payment link sits at the gateway. The error is what
// leaves the reserved row pending for the reconciler to find (SRS 20.2).
func TestUnrecordableSuccessSurfacesAnError(t *testing.T) {
	h := newHarness(t)
	h.store.markExecutedErr = errors.New("database write failed")

	res, err := h.exec.Execute(context.Background(), validRequest())
	if err == nil {
		t.Fatal("a side effect that could not be recorded returned no error")
	}
	if !strings.Contains(err.Error(), "executed but could not be recorded") {
		t.Errorf("error %q does not say the action executed", err)
	}
	if res.Executed() {
		t.Error("Executed() = true, but the record failed; the caller must not treat this as complete")
	}
	if !h.j.has("gateway.CreatePaymentLink") {
		t.Error("expected the gateway call to have happened")
	}
}

// TestReserveFailureMakesNoExternalCall guards the other write failure. If the row
// cannot be created, the action must not proceed — an unreserved action is an
// unauditable one, and AC-005 requires every side effect to have a case and action
// id behind it.
func TestReserveFailureMakesNoExternalCall(t *testing.T) {
	h := newHarness(t)
	h.store.reserveErr = errors.New("database unavailable")

	if _, err := h.exec.Execute(context.Background(), validRequest()); err == nil {
		t.Fatal("Execute succeeded despite a failed reserve")
	}
	if calls := h.j.gatewayCalls(); len(calls) != 0 {
		t.Errorf("the gateway was called without a reserved row: %v", calls)
	}
}

// TestNoGatewayConfiguredFailsClosed is SRS 20.4 at the transport layer. A missing
// gateway is a misconfiguration, and the safe response is a recorded failure — not
// a silent success that would report a recovery which never happened.
func TestNoGatewayConfiguredFailsClosed(t *testing.T) {
	j := &journal{}
	store := newFakeStore(j)
	e := New(store, nil, Config{})
	e.SetClock(func() time.Time { return fixedNow })

	res, err := e.Execute(context.Background(), validRequest())
	if err == nil {
		t.Fatal("Execute succeeded with no gateway configured")
	}
	if res.Executed() {
		t.Error("Executed() = true with no gateway")
	}
	if !j.has("store.MarkActionAmbiguous") && !j.has("store.MarkActionFailed") {
		t.Errorf("the attempt was not recorded as failed\njournal: %s", j)
	}
}

// TestInvoiceReminderNotifiesTheExistingInvoice covers the one action that acts on
// a resource the customer already has. Creating a second payment link for an
// overdue invoice would be a parallel demand for the same money, so this path uses
// Razorpay's notify endpoint on the invoice itself.
func TestInvoiceReminderNotifiesTheExistingInvoice(t *testing.T) {
	h := newHarness(t)
	req := validRequest()
	req.Action = domain.ActionReminder
	req.SourceType = domain.SourceInvoiceOverdue
	req.RazorpayResourceID = "inv_TEST1"
	req.InvoiceID = "invoice-row-1"

	res, err := h.exec.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v\njournal: %s", err, h.j)
	}
	if !res.Executed() {
		t.Fatalf("Status = %s", res.Status)
	}
	if !h.j.has("gateway.NotifyInvoice:inv_TEST1") {
		t.Errorf("the existing invoice was not notified\njournal: %s", h.j)
	}
	if h.j.has("gateway.CreatePaymentLink") {
		t.Error("a second payment link was created for an invoice the customer already has")
	}
	if res.ExternalID != "inv_TEST1" {
		t.Errorf("ExternalID = %q, want the invoice id", res.ExternalID)
	}
	if !h.j.has("store.IncrementInvoiceReminder:invoice-row-1") {
		t.Error("the invoice reminder counter was not incremented")
	}

	// The reserved row names the resource before the call, so a stranded pending
	// row tells the reconciler which method to verify it with (SRS 20.2).
	if len(h.store.reserved) != 1 {
		t.Fatalf("reserved %d rows, want 1", len(h.store.reserved))
	}
	if h.store.reserved[0].ExternalID != "inv_TEST1" {
		t.Errorf("reserved row ExternalID = %q, want the invoice id recorded before the call",
			h.store.reserved[0].ExternalID)
	}

	// A reminder for any other source type has no existing resource to notify, so
	// it falls back to a payment link.
	h2 := newHarness(t)
	req2 := validRequest()
	req2.Action = domain.ActionReminder
	req2.SourceType = domain.SourceCheckoutAbandonment
	if _, err := h2.exec.Execute(context.Background(), req2); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !h2.j.has("gateway.CreatePaymentLink") {
		t.Error("a checkout reminder did not create a payment link")
	}
	if h2.j.has("gateway.NotifyInvoice") {
		t.Error("a checkout reminder tried to notify an invoice")
	}
}

// TestRetryCollectsThroughAPaymentLink pins a deliberate design decision, so it
// cannot be quietly changed into a stored-instrument charge.
//
// LEDGERFLOW has no mandate to pull money from a customer's card. A "retry" here
// means asking again through a link the customer completes, which is why the retry
// path and the payment-link path converge on the same gateway call (SRS 5.2, 19.1).
func TestRetryCollectsThroughAPaymentLink(t *testing.T) {
	h := newHarness(t)
	req := validRequest()
	req.Action = domain.ActionRetry
	req.RazorpayResourceID = "pay_FAILED1"

	res, err := h.exec.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Executed() {
		t.Fatalf("Status = %s", res.Status)
	}
	if !h.j.has("gateway.CreatePaymentLink") {
		t.Errorf("a retry did not go through a payment link\njournal: %s", h.j)
	}
	// The one thing that must never appear on this path.
	for _, call := range h.j.gatewayCalls() {
		if strings.Contains(call, "Capture") || strings.Contains(call, "Charge") {
			t.Errorf("a retry attempted a direct charge: %s", call)
		}
	}
	if got := h.gateway.lastLinkRequest.Notes["ledgerflow_action"]; got != "retry" {
		t.Errorf("notes[ledgerflow_action] = %q, want retry so the link is attributable", got)
	}
}

// TestModeDefaultsToLiveTest checks the recorded mode. Every row carries the mode
// it ran under so simulated and test-mode results can never be aggregated together
// (SRS 25.2), and the default must be the more conservative of the two.
func TestModeDefaultsToLiveTest(t *testing.T) {
	h := newHarness(t)
	req := validRequest()
	req.Mode = ""
	if _, err := h.exec.Execute(context.Background(), req); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(h.store.reserved) != 1 {
		t.Fatalf("reserved %d rows, want 1", len(h.store.reserved))
	}
	if got := h.store.reserved[0].Mode; got != domain.ModeLiveTest {
		t.Errorf("Mode = %q, want %q", got, domain.ModeLiveTest)
	}
	// Live-mode monetary transactions are out of scope, so every row is test
	// environment regardless of mode (SRS 5.2, 19.1).
	if got := h.store.reserved[0].Environment; got != domain.EnvTest {
		t.Errorf("Environment = %q, want %q", got, domain.EnvTest)
	}
}
