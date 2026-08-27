package razorpay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ledgerflow/ledgerflow/internal/config"
	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// Integration tests for the Razorpay transport (SRS 22.2).
//
// These exercise HTTPGateway — the real client, the real JSON encoding, the real
// auth header, the real retry loop — against an httptest server that speaks
// Razorpay's documented request and response shapes. That is deliberately a
// different thing from the sandbox gateway: the sandbox proves the recovery loop
// runs without credentials, while these tests prove the code that will talk to
// api.razorpay.com sends what Razorpay expects and survives what Razorpay does.
//
// The four §22.2 items covered here:
//
//   - payment-link creation and verification
//   - invoice lifecycle
//   - subscription workflow
//   - API timeout and retry behaviour
//
// Webhook receipt and duplicate delivery are integration-tested in
// internal/integration, where a webhook can be driven through the router and the
// ingestor into a case.

// --- a stand-in for api.razorpay.com ---

// fakeRazorpay implements the endpoints under test with Razorpay's own payload
// shapes: amounts in paise, timestamps as unix seconds, errors in the
// {"error":{"code":...}} envelope.
//
// It keeps resources keyed by reference_id as well as id, because that is what
// makes the idempotency assertions meaningful: a create replayed with the same
// reference_id returns the original resource rather than a second one, which is
// the behaviour LEDGERFLOW's ambiguous-failure handling depends on (SRS 20.1,
// 20.2).
type fakeRazorpay struct {
	mu  sync.Mutex
	srv *httptest.Server

	// calls is the ordered request log: "POST /payment_links". Ordering matters
	// for the lifecycle tests, where "issued before notified" is the claim.
	calls []string
	// auth records the credentials seen on the most recent request.
	authUser, authPass string
	authOK             bool

	links  map[string]*paymentLinkResponse // by id
	byRef  map[string]*paymentLinkResponse // by reference_id
	linkNo int

	invoices map[string]*invoiceResponse
	invNo    int

	subs map[string]*subscriptionResponse

	// faults programs the next N responses. Each is consumed by one request.
	faults []fault
}

// fault is one programmed failure. status > 0 returns that HTTP status with a
// Razorpay error envelope; delay > 0 sleeps before responding, which is how a
// client-side timeout is provoked.
type fault struct {
	status int
	code   string
	delay  time.Duration
	// recordFirst applies the request's side effect before sleeping, which
	// reproduces the case that matters most: the call reached Razorpay, the
	// resource exists, and the client never learned about it (SRS 20.2).
	recordFirst bool
}

func newFakeRazorpay(t *testing.T) *fakeRazorpay {
	t.Helper()
	f := &fakeRazorpay{
		links:    map[string]*paymentLinkResponse{},
		byRef:    map[string]*paymentLinkResponse{},
		invoices: map[string]*invoiceResponse{},
		subs:     map[string]*subscriptionResponse{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

// gateway builds an HTTPGateway pointed at the fake, with test-mode credentials.
func (f *fakeRazorpay) gateway(t *testing.T, timeout time.Duration, maxRetries int) *HTTPGateway {
	t.Helper()
	g, err := NewHTTPGateway(config.RazorpayConfig{
		KeyID:      "rzp_test_integration",
		KeySecret:  "integration_secret",
		BaseURL:    f.srv.URL,
		Timeout:    timeout,
		MaxRetries: maxRetries,
		Mode:       "test",
	})
	if err != nil {
		t.Fatalf("build gateway: %v", err)
	}
	return g
}

func (f *fakeRazorpay) program(faults ...fault) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.faults = append(f.faults, faults...)
}

func (f *fakeRazorpay) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// countCalls counts logged requests whose entry contains sub.
func (f *fakeRazorpay) countCalls(sub string) int {
	n := 0
	for _, c := range f.log() {
		if strings.Contains(c, sub) {
			n++
		}
	}
	return n
}

func (f *fakeRazorpay) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.authUser, f.authPass, f.authOK = r.BasicAuth()
	var pending *fault
	if len(f.faults) > 0 {
		pending = &f.faults[0]
		f.faults = f.faults[1:]
	}
	f.mu.Unlock()

	if pending != nil && pending.status > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(pending.status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"code":        pending.code,
				"description": "programmed failure",
			},
		})
		return
	}

	// A delayed response either applies its side effect first (so the resource
	// exists despite the client's timeout) or not at all.
	if pending != nil && pending.delay > 0 {
		if pending.recordFirst {
			f.route(w, r, true)
		}
		time.Sleep(pending.delay)
		if pending.recordFirst {
			return
		}
	}
	f.route(w, r, false)
}

// route dispatches to the endpoint handlers. discard suppresses the response
// body, which is used when a side effect must be applied without the client
// seeing the result.
func (f *fakeRazorpay) route(w http.ResponseWriter, r *http.Request, discard bool) {
	out := w
	if discard {
		out = nil
	}
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && path == "/payment_links":
		f.createLink(out, r)
	case r.Method == http.MethodGet && path == "/payment_links":
		f.listLinks(out, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/payment_links/"):
		f.fetchLink(out, strings.TrimPrefix(path, "/payment_links/"))
	case r.Method == http.MethodPost && strings.Contains(path, "/notify_by/"):
		writeJSON(out, http.StatusOK, map[string]bool{"success": true})
	case r.Method == http.MethodPost && path == "/invoices":
		f.createInvoice(out, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/invoices/"):
		f.fetchInvoice(out, strings.TrimPrefix(path, "/invoices/"))
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/issue"):
		f.issueInvoice(out, strings.TrimSuffix(strings.TrimPrefix(path, "/invoices/"), "/issue"))
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/subscriptions/"):
		f.fetchSubscription(out, strings.TrimPrefix(path, "/subscriptions/"))
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/cancel"):
		f.cancelSubscription(out, strings.TrimSuffix(strings.TrimPrefix(path, "/subscriptions/"), "/cancel"), r)
	default:
		writeJSON(out, http.StatusNotFound, map[string]any{
			"error": map[string]string{"code": "BAD_REQUEST_ERROR", "description": "no such route " + path},
		})
	}
}

func (f *fakeRazorpay) createLink(w http.ResponseWriter, r *http.Request) {
	var in paymentLinkPayload
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"code": "BAD_REQUEST_ERROR", "description": "unparseable body"},
		})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	// Razorpay treats reference_id as unique per link. Replaying a create with the
	// same reference returns the original, which is exactly the property a retry
	// after an ambiguous failure relies on.
	if in.ReferenceID != "" {
		if existing, ok := f.byRef[in.ReferenceID]; ok {
			writeJSON(w, http.StatusOK, existing)
			return
		}
	}
	f.linkNo++
	link := &paymentLinkResponse{
		ID:          fmt.Sprintf("plink_TEST%04d", f.linkNo),
		ShortURL:    "https://rzp.io/i/TEST" + fmt.Sprint(f.linkNo),
		Status:      "created",
		Amount:      in.Amount,
		Currency:    in.Currency,
		ReferenceID: in.ReferenceID,
		Description: in.Description,
		CreatedAt:   time.Now().Unix(),
		Notes:       in.Notes,
	}
	f.links[link.ID] = link
	if in.ReferenceID != "" {
		f.byRef[in.ReferenceID] = link
	}
	writeJSON(w, http.StatusOK, link)
}

func (f *fakeRazorpay) fetchLink(w http.ResponseWriter, id string) {
	f.mu.Lock()
	link, ok := f.links[id]
	f.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"code": "BAD_REQUEST_ERROR", "description": "no such payment link"},
		})
		return
	}
	writeJSON(w, http.StatusOK, link)
}

func (f *fakeRazorpay) listLinks(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("reference_id")
	f.mu.Lock()
	items := []paymentLinkResponse{}
	for _, l := range f.links {
		if ref == "" || l.ReferenceID == ref {
			items = append(items, *l)
		}
	}
	f.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"count": len(items), "payment_links": items})
}

// markLinkPaid simulates the customer paying, which is the state change
// verification is looking for.
func (f *fakeRazorpay) markLinkPaid(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if l, ok := f.links[id]; ok {
		l.Status = "paid"
		l.AmountPaid = l.Amount
	}
}

func (f *fakeRazorpay) createInvoice(w http.ResponseWriter, r *http.Request) {
	var in invoicePayload
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"code": "BAD_REQUEST_ERROR", "description": "unparseable body"},
		})
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invNo++
	inv := &invoiceResponse{
		ID:            fmt.Sprintf("inv_TEST%04d", f.invNo),
		InvoiceNumber: in.InvoiceNumber,
		// A created invoice is a draft until it is issued. Getting this wrong in
		// the fake would hide the fact that the executor must issue before the
		// customer can pay (SRS 12.4).
		Status:      "draft",
		Amount:      in.Amount,
		AmountDue:   in.Amount,
		Currency:    in.Currency,
		ReferenceID: in.ReferenceID,
		DueBy:       in.ExpireBy,
		CreatedAt:   time.Now().Unix(),
	}
	f.invoices[inv.ID] = inv
	writeJSON(w, http.StatusOK, inv)
}

func (f *fakeRazorpay) fetchInvoice(w http.ResponseWriter, id string) {
	f.mu.Lock()
	inv, ok := f.invoices[id]
	f.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"code": "BAD_REQUEST_ERROR", "description": "no such invoice"},
		})
		return
	}
	writeJSON(w, http.StatusOK, inv)
}

func (f *fakeRazorpay) issueInvoice(w http.ResponseWriter, id string) {
	f.mu.Lock()
	inv, ok := f.invoices[id]
	if ok {
		inv.Status = "issued"
		inv.ShortURL = "https://rzp.io/i/INV" + id
	}
	f.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"code": "BAD_REQUEST_ERROR", "description": "no such invoice"},
		})
		return
	}
	f.fetchInvoice(w, id)
}

func (f *fakeRazorpay) fetchSubscription(w http.ResponseWriter, id string) {
	f.mu.Lock()
	sub, ok := f.subs[id]
	f.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"code": "BAD_REQUEST_ERROR", "description": "no such subscription"},
		})
		return
	}
	writeJSON(w, http.StatusOK, sub)
}

func (f *fakeRazorpay) cancelSubscription(w http.ResponseWriter, id string, r *http.Request) {
	var body map[string]int
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	sub, ok := f.subs[id]
	if ok {
		if body["cancel_at_cycle_end"] == 1 {
			// Razorpay keeps the subscription active until the period ends.
			sub.Status = "active"
		} else {
			sub.Status = "cancelled"
		}
	}
	f.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"code": "BAD_REQUEST_ERROR", "description": "no such subscription"},
		})
		return
	}
	f.fetchSubscription(w, id)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	if w == nil {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// --- SRS 22.2: payment-link creation and verification ---

func TestIntegrationPaymentLinkCreateAndVerify(t *testing.T) {
	f := newFakeRazorpay(t)
	g := f.gateway(t, 2*time.Second, 1)
	ctx := context.Background()

	const amount = domain.Money(1_249_900) // ₹12,499.00
	const ref = "idem_case123_payment_link_1"

	link, err := g.CreatePaymentLink(ctx, PaymentLinkRequest{
		Amount:        amount,
		Currency:      "INR",
		Description:   "Order #LF-4471",
		ReferenceID:   ref,
		CustomerEmail: "aarav.mehta@example.com",
		NotifyEmail:   true,
		ExpireBy:      time.Now().Add(48 * time.Hour),
		CallbackURL:   "http://localhost:3000/recovered",
		Notes:         map[string]string{"case_id": "case123", "idempotency_key": ref},
	})
	if err != nil {
		t.Fatalf("create payment link: %v", err)
	}

	if link.ID == "" || !strings.HasPrefix(link.ID, "plink_") {
		t.Errorf("link id = %q, want a plink_ id", link.ID)
	}
	// The amount must survive the round trip as an exact paise integer. A float
	// anywhere in this path is how recovery totals start drifting by a rupee.
	if link.Amount != amount {
		t.Errorf("link amount = %d paise, want %d", int64(link.Amount), int64(amount))
	}
	if link.ReferenceID != ref {
		t.Errorf("reference_id = %q, want %q — without it a paid link cannot be traced to its action", link.ReferenceID, ref)
	}
	if link.Status != "created" {
		t.Errorf("status = %q, want created", link.Status)
	}
	if link.Paid() {
		t.Error("a freshly created link reports Paid(); nothing has been paid yet")
	}

	// Test-mode credentials must travel as HTTP Basic auth (SRS 12.1).
	f.mu.Lock()
	user, pass, okAuth := f.authUser, f.authPass, f.authOK
	f.mu.Unlock()
	if !okAuth || user != "rzp_test_integration" || pass != "integration_secret" {
		t.Errorf("basic auth = (%q, %q, ok=%v), want the configured test key pair", user, pass, okAuth)
	}

	// --- verification ---

	// Before payment, a fetch reports the link is still unpaid. Verification that
	// banked revenue at this point would be inventing a recovery.
	fetched, err := g.FetchPaymentLink(ctx, link.ID)
	if err != nil {
		t.Fatalf("fetch payment link: %v", err)
	}
	if fetched.Paid() {
		t.Fatal("fetch reports the link paid before any payment was made")
	}

	f.markLinkPaid(link.ID)

	paid, err := g.FetchPaymentLink(ctx, link.ID)
	if err != nil {
		t.Fatalf("fetch after payment: %v", err)
	}
	if !paid.Paid() {
		t.Errorf("status = %q, want paid", paid.Status)
	}
	if paid.AmountPaid != amount {
		t.Errorf("amount_paid = %d paise, want %d", int64(paid.AmountPaid), int64(amount))
	}
	if paid.ReferenceID != ref {
		t.Errorf("reference_id on the paid link = %q, want %q", paid.ReferenceID, ref)
	}
}

// TestIntegrationFindPaymentLinkByReference pins the contract that makes
// reconciliation possible: "it was never created" is a value, not an error.
//
// The reconciler's whole job is distinguishing those two, and it can only do so
// if the lookup returns (nil, nil) for a missing link. Collapsing that into an
// error would leave a timed-out create permanently unresolvable (SRS 20.2).
func TestIntegrationFindPaymentLinkByReference(t *testing.T) {
	f := newFakeRazorpay(t)
	g := f.gateway(t, 2*time.Second, 1)
	ctx := context.Background()

	missing, err := g.FindPaymentLinkByReference(ctx, "idem_never_created")
	if err != nil {
		t.Fatalf("lookup of a never-created reference errored: %v", err)
	}
	if missing != nil {
		t.Fatalf("lookup of a never-created reference returned %+v, want nil", missing)
	}

	const ref = "idem_case456_payment_link_1"
	created, err := g.CreatePaymentLink(ctx, PaymentLinkRequest{
		Amount: 50_000, Currency: "INR", ReferenceID: ref,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	found, err := g.FindPaymentLinkByReference(ctx, ref)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if found == nil || found.ID != created.ID {
		t.Fatalf("lookup returned %+v, want the created link %s", found, created.ID)
	}

	// An empty reference is a caller bug, not a "not found": returning nil here
	// would let a lookup with a missing key be read as "nothing was created".
	if _, err := g.FindPaymentLinkByReference(ctx, "   "); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("empty reference error = %v, want ErrValidation", err)
	}
}

// --- SRS 22.2: API timeout and retry behaviour ---

func TestIntegrationRetriesTransientFailures(t *testing.T) {
	f := newFakeRazorpay(t)
	g := f.gateway(t, 2*time.Second, 2)
	ctx := context.Background()

	// Two 5xx responses, then the real handler. Both should be retried.
	f.program(
		fault{status: http.StatusBadGateway, code: "SERVER_ERROR"},
		fault{status: http.StatusServiceUnavailable, code: "SERVER_ERROR"},
	)

	link, err := g.CreatePaymentLink(ctx, PaymentLinkRequest{
		Amount: 99_900, Currency: "INR", ReferenceID: "idem_retry_ok",
	})
	if err != nil {
		t.Fatalf("create should have succeeded on the third attempt: %v", err)
	}
	if link.ID == "" {
		t.Error("succeeded with no link id")
	}
	if n := f.countCalls("POST /payment_links"); n != 3 {
		t.Errorf("attempts = %d, want 3 (initial + 2 retries)", n)
	}
}

func TestIntegrationRetryBudgetIsBounded(t *testing.T) {
	f := newFakeRazorpay(t)
	g := f.gateway(t, 2*time.Second, 1)
	ctx := context.Background()

	// More failures than the budget allows. The gateway must give up rather than
	// hammer the API: an unbounded retry loop against a payment provider is worse
	// than a failed action (SRS 20.3).
	f.program(
		fault{status: http.StatusInternalServerError, code: "SERVER_ERROR"},
		fault{status: http.StatusInternalServerError, code: "SERVER_ERROR"},
		fault{status: http.StatusInternalServerError, code: "SERVER_ERROR"},
	)

	_, err := g.CreatePaymentLink(ctx, PaymentLinkRequest{
		Amount: 99_900, Currency: "INR", ReferenceID: "idem_retry_exhausted",
	})
	if err == nil {
		t.Fatal("create succeeded despite every attempt failing")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", apiErr.StatusCode)
	}
	if !apiErr.Retryable() {
		t.Error("a 500 should still report Retryable() so the caller can reconcile and try later")
	}
	if apiErr.Ambiguous {
		t.Error("a clean 500 response is not ambiguous: the request definitely did not create a resource")
	}
	if n := f.countCalls("POST /payment_links"); n != 2 {
		t.Errorf("attempts = %d, want 2 (initial + 1 retry, MaxRetries=1)", n)
	}
}

func TestIntegrationValidationErrorsAreNeverRetried(t *testing.T) {
	f := newFakeRazorpay(t)
	g := f.gateway(t, 2*time.Second, 3)
	ctx := context.Background()

	f.program(fault{status: http.StatusBadRequest, code: "BAD_REQUEST_ERROR"})

	_, err := g.CreatePaymentLink(ctx, PaymentLinkRequest{
		Amount: 0, Currency: "INR", ReferenceID: "idem_bad_request",
	})
	if err == nil {
		t.Fatal("create succeeded on a 400")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if apiErr.Retryable() {
		t.Error("a 400 reports Retryable(); retrying a rejected request only repeats the rejection")
	}
	// One attempt, despite MaxRetries=3.
	if n := f.countCalls("POST /payment_links"); n != 1 {
		t.Errorf("attempts = %d, want 1 — a 4xx must not be retried", n)
	}
}

// TestIntegrationTimeoutIsAmbiguousAndReconcilable is the most important test in
// this file.
//
// It reproduces the failure that can double-charge a customer: the create call
// reaches Razorpay, Razorpay creates the link, and the response never comes back.
// Three things must hold afterwards.
//
//   - The error is marked Ambiguous, so the caller knows the request may have
//     succeeded and must not simply treat the action as failed.
//   - Every retry carries the same reference_id, so the retry cannot create a
//     second link even though the client has no resource id to deduplicate on.
//   - The link is findable by that reference afterwards, which is how the
//     reconciler resolves the action instead of leaving it stuck (SRS 20.2).
func TestIntegrationTimeoutIsAmbiguousAndReconcilable(t *testing.T) {
	f := newFakeRazorpay(t)
	// A client timeout well below the server's delay, so the client gives up
	// while the server is still working.
	g := f.gateway(t, 120*time.Millisecond, 1)
	ctx := context.Background()

	const ref = "idem_case789_payment_link_1"

	// Both attempts hang, and both create the resource server-side before
	// hanging — the worst case, where the client learns nothing at all.
	f.program(
		fault{delay: 400 * time.Millisecond, recordFirst: true},
		fault{delay: 400 * time.Millisecond, recordFirst: true},
	)

	_, err := g.CreatePaymentLink(ctx, PaymentLinkRequest{
		Amount: 1_500_000, Currency: "INR", ReferenceID: ref,
	})
	if err == nil {
		t.Fatal("create returned success despite timing out")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error type = %T, want *APIError", err)
	}
	if !apiErr.Ambiguous {
		t.Fatal("a timed-out create is not marked Ambiguous; the caller would record it as a clean failure " +
			"and could create a second payment link for the same case")
	}
	if !apiErr.Retryable() {
		t.Error("an ambiguous failure should be Retryable — with the same key, after reconciliation")
	}
	if n := f.countCalls("POST /payment_links"); n != 2 {
		t.Fatalf("attempts = %d, want 2", n)
	}

	// The retry sent the same reference_id, so despite two create attempts
	// exactly one link exists. This is the property that keeps an ambiguous
	// failure from becoming a duplicate charge.
	f.mu.Lock()
	total := len(f.links)
	f.mu.Unlock()
	if total != 1 {
		t.Errorf("links created = %d, want 1 — the retry must reuse the reference id, not mint a new resource", total)
	}

	// And the reconciler can now discover what actually happened.
	found, err := g.FindPaymentLinkByReference(ctx, ref)
	if err != nil {
		t.Fatalf("reconciliation lookup failed: %v", err)
	}
	if found == nil {
		t.Fatal("the link exists at Razorpay but the reference lookup did not find it; " +
			"the action would stay unresolved forever")
	}
	if found.Amount != domain.Money(1_500_000) {
		t.Errorf("recovered link amount = %d paise, want 1500000", int64(found.Amount))
	}
}

// TestIntegrationContextCancellationStopsRetrying checks that a cancelled caller
// context aborts the backoff sleep instead of waiting it out. A shutdown that has
// to sit through a retry schedule is a shutdown that gets killed.
func TestIntegrationContextCancellationStopsRetrying(t *testing.T) {
	f := newFakeRazorpay(t)
	g := f.gateway(t, 2*time.Second, 3)

	f.program(
		fault{status: http.StatusInternalServerError, code: "SERVER_ERROR"},
		fault{status: http.StatusInternalServerError, code: "SERVER_ERROR"},
		fault{status: http.StatusInternalServerError, code: "SERVER_ERROR"},
		fault{status: http.StatusInternalServerError, code: "SERVER_ERROR"},
	)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel while the first backoff (400ms) is in progress.
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := g.CreatePaymentLink(ctx, PaymentLinkRequest{
		Amount: 10_000, Currency: "INR", ReferenceID: "idem_cancelled",
	})
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	// The full schedule would be 400+800+1600ms. Returning inside the first
	// backoff proves the sleep is interruptible.
	if elapsed > 400*time.Millisecond {
		t.Errorf("returned after %v; the backoff sleep did not observe cancellation", elapsed)
	}
}

// --- SRS 22.2: invoice lifecycle ---

func TestIntegrationInvoiceLifecycle(t *testing.T) {
	f := newFakeRazorpay(t)
	g := f.gateway(t, 2*time.Second, 1)
	ctx := context.Background()

	// The overdue-receivable workflow bills the outstanding balance, not the
	// original invoice total (SRS 11.3). Here: ₹34,500 invoiced, ₹10,000 already
	// paid, so the reissue is for ₹24,500.
	const outstanding = domain.Money(2_450_000)
	const ref = "idem_caseB2B_invoice_1"

	inv, err := g.CreateInvoice(ctx, InvoiceRequest{
		Amount:        outstanding,
		Currency:      "INR",
		Description:   "Outstanding balance on LF-INV-2042",
		ReferenceID:   ref,
		InvoiceNumber: "LF-INV-2042-R1",
		CustomerEmail: "accounts@northwind.example.com",
		CustomerName:  "Northwind Logistics Pvt Ltd",
		EmailNotify:   true,
		ExpireBy:      time.Now().Add(7 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("create invoice: %v", err)
	}
	if inv.Amount != outstanding {
		t.Errorf("invoice amount = %d paise, want %d (the outstanding balance, not the original total)",
			int64(inv.Amount), int64(outstanding))
	}
	if inv.Status != "draft" {
		t.Errorf("status after create = %q, want draft — an unissued invoice cannot be paid", inv.Status)
	}
	if inv.ShortURL != "" {
		t.Error("a draft invoice has a payable short_url; nothing should be collectable before it is issued")
	}
	if inv.ReferenceID != ref {
		t.Errorf("reference_id = %q, want %q", inv.ReferenceID, ref)
	}

	issued, err := g.IssueInvoice(ctx, inv.ID)
	if err != nil {
		t.Fatalf("issue invoice: %v", err)
	}
	if issued.Status != "issued" {
		t.Errorf("status after issue = %q, want issued", issued.Status)
	}
	if issued.ShortURL == "" {
		t.Error("an issued invoice has no short_url, so the customer has nothing to pay")
	}
	if issued.Amount != outstanding {
		t.Errorf("issuing changed the amount to %d paise, want %d", int64(issued.Amount), int64(outstanding))
	}

	if err := g.NotifyInvoice(ctx, inv.ID, "email"); err != nil {
		t.Fatalf("notify invoice: %v", err)
	}

	// A medium the API does not support is rejected locally rather than sent.
	if err := g.NotifyInvoice(ctx, inv.ID, "carrier_pigeon"); !errors.Is(err, domain.ErrValidation) {
		t.Errorf("notify with an unsupported medium: err = %v, want ErrValidation", err)
	}

	refetched, err := g.FetchInvoice(ctx, inv.ID)
	if err != nil {
		t.Fatalf("fetch invoice: %v", err)
	}
	if refetched.Status != "issued" || refetched.AmountDue != outstanding {
		t.Errorf("refetched invoice = %s / due %d paise, want issued / %d",
			refetched.Status, int64(refetched.AmountDue), int64(outstanding))
	}
	if refetched.Paid() {
		t.Error("an issued, unpaid invoice reports Paid()")
	}

	// Ordering: issue must precede notify. Notifying a draft would send the
	// customer a link to something they cannot pay.
	calls := f.log()
	issueAt, notifyAt := -1, -1
	for i, c := range calls {
		switch {
		case strings.HasSuffix(c, "/issue"):
			issueAt = i
		case strings.Contains(c, "/notify_by/email") && notifyAt == -1:
			notifyAt = i
		}
	}
	if issueAt == -1 || notifyAt == -1 || issueAt > notifyAt {
		t.Errorf("call order = %v; issue must come before notify", calls)
	}

	// The unsupported medium never reached the network.
	if n := f.countCalls("carrier_pigeon"); n != 0 {
		t.Errorf("an unsupported notification medium produced %d API calls, want 0", n)
	}
}

// --- SRS 22.2: subscription workflow ---

func TestIntegrationSubscriptionWorkflow(t *testing.T) {
	f := newFakeRazorpay(t)
	g := f.gateway(t, 2*time.Second, 1)
	ctx := context.Background()

	// A halted subscription: the recurring charge failed and Razorpay stopped
	// trying, which is the state the recurring workflow reacts to (SRS 11.4).
	f.mu.Lock()
	f.subs["sub_TEST0001"] = &subscriptionResponse{
		ID:             "sub_TEST0001",
		PlanID:         "plan_TEST0001",
		CustomerID:     "cust_TEST0001",
		Status:         "halted",
		PaidCount:      11,
		TotalCount:     12,
		RemainingCount: 1,
		CurrentStart:   time.Now().AddDate(0, 0, -24).Unix(),
		CurrentEnd:     time.Now().AddDate(0, 0, 6).Unix(),
		ChargeAt:       time.Now().AddDate(0, 0, 6).Unix(),
	}
	f.mu.Unlock()

	sub, err := g.FetchSubscription(ctx, "sub_TEST0001")
	if err != nil {
		t.Fatalf("fetch subscription: %v", err)
	}
	if sub.Status != "halted" {
		t.Errorf("status = %q, want halted", sub.Status)
	}
	if sub.PaidCount != 11 || sub.RemainingCount != 1 {
		t.Errorf("counts = paid %d / remaining %d, want 11 / 1", sub.PaidCount, sub.RemainingCount)
	}
	if sub.CurrentEnd.IsZero() {
		t.Error("current_end did not decode; the recovery window cannot be computed without it")
	}
	if !sub.CurrentEnd.After(sub.CurrentStart) {
		t.Errorf("current_end %v is not after current_start %v", sub.CurrentEnd, sub.CurrentStart)
	}

	// Cancelling at cycle end is the non-destructive option: the customer keeps
	// the period they paid for. LEDGERFLOW never cancels autonomously — this is
	// exercised as a reachable operator action, not an agent one (SRS 19.3).
	atEnd, err := g.CancelSubscription(ctx, "sub_TEST0001", true)
	if err != nil {
		t.Fatalf("cancel at cycle end: %v", err)
	}
	if atEnd.Status == "cancelled" {
		t.Error("cancel_at_cycle_end took effect immediately; the paid-for period was cut short")
	}

	immediate, err := g.CancelSubscription(ctx, "sub_TEST0001", false)
	if err != nil {
		t.Fatalf("cancel immediately: %v", err)
	}
	if immediate.Status != "cancelled" {
		t.Errorf("status after immediate cancel = %q, want cancelled", immediate.Status)
	}

	if _, err := g.FetchSubscription(ctx, "sub_DOES_NOT_EXIST"); err == nil {
		t.Error("fetching an unknown subscription succeeded")
	}
}

// --- construction guards (SRS 23.4) ---

// TestHTTPGatewayRefusesLiveCredentials is the compile-time-adjacent guard for
// the one constraint in this package that must never be relaxed: the prototype
// cannot be pointed at live keys. Live-mode monetary transactions are out of
// scope, and the refusal happens at construction so no code path can reach a
// live-capable transport at all.
func TestHTTPGatewayRefusesLiveCredentials(t *testing.T) {
	base := config.RazorpayConfig{
		KeyID: "rzp_test_ok", KeySecret: "s", BaseURL: "https://example.invalid",
		Timeout: time.Second, Mode: "test",
	}

	cases := []struct {
		name   string
		mutate func(c *config.RazorpayConfig)
	}{
		{"a live key id", func(c *config.RazorpayConfig) { c.KeyID = "rzp_live_abcdef" }},
		{"mode set to live", func(c *config.RazorpayConfig) { c.Mode = "live" }},
		{"an empty mode", func(c *config.RazorpayConfig) { c.Mode = "" }},
		{"a live key with test mode declared", func(c *config.RazorpayConfig) {
			c.KeyID = "rzp_live_abcdef"
			c.Mode = "test"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			g, err := NewHTTPGateway(cfg)
			if err == nil {
				t.Fatalf("NewHTTPGateway accepted %s and returned a usable gateway", tc.name)
			}
			if g != nil {
				t.Error("a rejected configuration still produced a gateway value")
			}
		})
	}

	// Missing credentials are also refused, rather than producing a gateway that
	// fails on every call with an opaque 401.
	if _, err := NewHTTPGateway(config.RazorpayConfig{Mode: "test"}); err == nil {
		t.Error("NewHTTPGateway accepted empty credentials")
	}

	// And the honest case still works.
	if _, err := NewHTTPGateway(base); err != nil {
		t.Errorf("NewHTTPGateway rejected valid test credentials: %v", err)
	}
}

// TestSandboxGatewayIsNotExternal pins the label the audit trail depends on.
//
// Every action record carries the gateway name, which is what stops a sandbox
// result from being presented as a Razorpay test-mode result (SRS 25.2). If the
// sandbox ever reported External() true, the /api/sync route would also try to
// use it as a real API.
func TestSandboxGatewayIsNotExternal(t *testing.T) {
	s := NewSandboxGateway()
	if s.External() {
		t.Error("the sandbox gateway reports External() true")
	}
	if s.Name() == "razorpay_test" {
		t.Error("the sandbox gateway uses the same name as the real test-mode gateway, " +
			"so an audit record cannot distinguish them")
	}
	if !strings.Contains(s.Name(), "sandbox") {
		t.Errorf("sandbox gateway name = %q, want something that says sandbox", s.Name())
	}
}
