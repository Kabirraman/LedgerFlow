package razorpay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ledgerflow/ledgerflow/internal/config"
	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// HTTPGateway talks to the Razorpay REST API at https://api.razorpay.com/v1
// using HTTP Basic auth with key_id as username and key_secret as password
// (SRS 12.1).
type HTTPGateway struct {
	cfg    config.RazorpayConfig
	client *http.Client
	// onCall is an optional hook used to record latency and failure metrics.
	onCall func(op string, latency time.Duration, err error)
}

// NewHTTPGateway builds a live test-mode gateway. It refuses live credentials:
// the prototype must never be able to move real money (SRS 23.4).
func NewHTTPGateway(cfg config.RazorpayConfig) (*HTTPGateway, error) {
	if !cfg.Configured() {
		return nil, errors.New("razorpay credentials are not configured")
	}
	if strings.HasPrefix(cfg.KeyID, "rzp_live") || cfg.Mode != "test" {
		return nil, errors.New("refusing to construct a live-mode Razorpay gateway")
	}
	return &HTTPGateway{
		cfg:    cfg,
		client: &http.Client{Timeout: cfg.Timeout},
	}, nil
}

// SetCallObserver registers a metrics hook.
func (g *HTTPGateway) SetCallObserver(fn func(op string, latency time.Duration, err error)) {
	g.onCall = fn
}

func (g *HTTPGateway) Name() string   { return "razorpay_test" }
func (g *HTTPGateway) External() bool { return true }

// do performs a request with bounded retry and exponential backoff.
//
// Retry policy (SRS 20.3):
//   - transport error / timeout   → retry, marked ambiguous so the caller
//     reconciles external state before a further attempt
//   - 429 and 5xx                 → bounded retry with backoff
//   - 4xx                         → never retried; recorded and escalated
//
// The same reference_id (idempotency key) is sent on every attempt, so a retry
// after an ambiguous failure cannot create a second resource.
func (g *HTTPGateway) do(ctx context.Context, op, method, path string, body any, out any) error {
	start := time.Now()
	err := g.doOnce(ctx, method, path, body, out, 0)
	if g.onCall != nil {
		g.onCall(op, time.Since(start), err)
	}
	return err
}

func (g *HTTPGateway) doOnce(ctx context.Context, method, path string, body any, out any, attempt int) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, g.cfg.BaseURL+path, reader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.SetBasicAuth(g.cfg.KeyID, g.cfg.KeySecret)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("User-Agent", "LEDGERFLOW/1.0 (Razorpay AI Buildathon Track 03)")

	resp, err := g.client.Do(req)
	if err != nil {
		// The request may have reached Razorpay and succeeded. Treat it as
		// ambiguous rather than failed (SRS 20.2).
		apiErr := &APIError{Code: "transport_error", Description: err.Error(), Ambiguous: true}
		if retry, wait := g.shouldRetry(apiErr, attempt); retry {
			if sleepErr := sleepCtx(ctx, wait); sleepErr != nil {
				return sleepErr
			}
			return g.doOnce(ctx, method, path, body, out, attempt+1)
		}
		return apiErr
	}
	defer resp.Body.Close()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		return &APIError{StatusCode: resp.StatusCode, Code: "read_error", Description: readErr.Error(), Ambiguous: true}
	}

	if resp.StatusCode >= 400 {
		apiErr := parseAPIError(resp.StatusCode, raw)
		if retry, wait := g.shouldRetry(apiErr, attempt); retry {
			if sleepErr := sleepCtx(ctx, wait); sleepErr != nil {
				return sleepErr
			}
			return g.doOnce(ctx, method, path, body, out, attempt+1)
		}
		return apiErr
	}

	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return &APIError{StatusCode: resp.StatusCode, Code: "decode_error", Description: err.Error()}
		}
	}
	return nil
}

// shouldRetry implements the bounded-retry budget with exponential backoff.
func (g *HTTPGateway) shouldRetry(e *APIError, attempt int) (bool, time.Duration) {
	if attempt >= g.cfg.MaxRetries || !e.Retryable() {
		return false, 0
	}
	// 400ms, 800ms, 1.6s ...
	wait := time.Duration(400*(1<<attempt)) * time.Millisecond
	return true, wait
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// razorpayErrorEnvelope matches Razorpay's documented error shape.
type razorpayErrorEnvelope struct {
	Error struct {
		Code        string `json:"code"`
		Description string `json:"description"`
		Source      string `json:"source"`
		Step        string `json:"step"`
		Reason      string `json:"reason"`
		Field       string `json:"field"`
	} `json:"error"`
}

func parseAPIError(status int, raw []byte) *APIError {
	var env razorpayErrorEnvelope
	e := &APIError{StatusCode: status}
	if err := json.Unmarshal(raw, &env); err == nil && env.Error.Code != "" {
		e.Code = env.Error.Code
		e.Description = env.Error.Description
		e.Source = env.Error.Source
		e.Step = env.Error.Step
		e.Reason = env.Error.Reason
		e.Field = env.Error.Field
		return e
	}
	e.Code = "http_" + strconv.Itoa(status)
	e.Description = strings.TrimSpace(string(raw))
	if len(e.Description) > 500 {
		e.Description = e.Description[:500]
	}
	return e
}

// --- Payment Links (SRS 12.3) ---

type paymentLinkPayload struct {
	Amount         int64             `json:"amount"`
	Currency       string            `json:"currency"`
	AcceptPartial  bool              `json:"accept_partial"`
	Description    string            `json:"description,omitempty"`
	ReferenceID    string            `json:"reference_id,omitempty"`
	Customer       *linkCustomer     `json:"customer,omitempty"`
	NotifyBy       *linkNotify       `json:"notify,omitempty"`
	ReminderEnable bool              `json:"reminder_enable"`
	ExpireBy       int64             `json:"expire_by,omitempty"`
	CallbackURL    string            `json:"callback_url,omitempty"`
	CallbackMethod string            `json:"callback_method,omitempty"`
	Notes          map[string]string `json:"notes,omitempty"`
}

type linkCustomer struct {
	Name    string `json:"name,omitempty"`
	Email   string `json:"email,omitempty"`
	Contact string `json:"contact,omitempty"`
}

type linkNotify struct {
	SMS   bool `json:"sms"`
	Email bool `json:"email"`
}

type paymentLinkResponse struct {
	ID          string            `json:"id"`
	ShortURL    string            `json:"short_url"`
	Status      string            `json:"status"`
	Amount      int64             `json:"amount"`
	AmountPaid  int64             `json:"amount_paid"`
	Currency    string            `json:"currency"`
	ReferenceID string            `json:"reference_id"`
	Description string            `json:"description"`
	CreatedAt   int64             `json:"created_at"`
	Notes       map[string]string `json:"notes"`
}

func (r paymentLinkResponse) normalize() *PaymentLink {
	return &PaymentLink{
		ID:          r.ID,
		ShortURL:    r.ShortURL,
		Status:      r.Status,
		Amount:      domain.Money(r.Amount),
		AmountPaid:  domain.Money(r.AmountPaid),
		Currency:    r.Currency,
		ReferenceID: r.ReferenceID,
		Description: r.Description,
		CreatedAt:   unix(r.CreatedAt),
		Notes:       r.Notes,
	}
}

func (g *HTTPGateway) CreatePaymentLink(ctx context.Context, req PaymentLinkRequest) (*PaymentLink, error) {
	payload := paymentLinkPayload{
		Amount:         int64(req.Amount),
		Currency:       defaultStr(req.Currency, "INR"),
		AcceptPartial:  false,
		Description:    req.Description,
		ReferenceID:    req.ReferenceID,
		ReminderEnable: req.ReminderEnable,
		CallbackURL:    req.CallbackURL,
		Notes:          req.Notes,
	}
	if req.CallbackURL != "" {
		payload.CallbackMethod = "get"
	}
	if req.CustomerName != "" || req.CustomerEmail != "" || req.CustomerContact != "" {
		payload.Customer = &linkCustomer{Name: req.CustomerName, Email: req.CustomerEmail, Contact: req.CustomerContact}
	}
	if req.NotifyEmail || req.NotifySMS {
		payload.NotifyBy = &linkNotify{SMS: req.NotifySMS, Email: req.NotifyEmail}
	}
	if !req.ExpireBy.IsZero() {
		payload.ExpireBy = req.ExpireBy.Unix()
	}

	var resp paymentLinkResponse
	if err := g.do(ctx, "create_payment_link", http.MethodPost, "/payment_links", payload, &resp); err != nil {
		return nil, err
	}
	return resp.normalize(), nil
}

func (g *HTTPGateway) FetchPaymentLink(ctx context.Context, id string) (*PaymentLink, error) {
	var resp paymentLinkResponse
	if err := g.do(ctx, "fetch_payment_link", http.MethodGet, "/payment_links/"+url.PathEscape(id), nil, &resp); err != nil {
		return nil, err
	}
	return resp.normalize(), nil
}

// FindPaymentLinkByReference lists payment links filtered by reference_id.
// Razorpay treats reference_id as unique per link, so at most one is expected;
// if more than one comes back the newest wins, which is the one our latest
// attempt would have created.
func (g *HTTPGateway) FindPaymentLinkByReference(ctx context.Context, referenceID string) (*PaymentLink, error) {
	if strings.TrimSpace(referenceID) == "" {
		return nil, fmt.Errorf("%w: reference id is required", domain.ErrValidation)
	}
	q := url.Values{}
	q.Set("reference_id", referenceID)
	var resp struct {
		Count int                   `json:"count"`
		Items []paymentLinkResponse `json:"payment_links"`
	}
	if err := g.do(ctx, "find_payment_link_by_reference", http.MethodGet,
		"/payment_links?"+q.Encode(), nil, &resp); err != nil {
		return nil, err
	}
	var newest *PaymentLink
	for i := range resp.Items {
		link := resp.Items[i].normalize()
		if newest == nil || link.CreatedAt.After(newest.CreatedAt) {
			newest = link
		}
	}
	return newest, nil
}

func (g *HTTPGateway) NotifyPaymentLink(ctx context.Context, id, medium string) error {
	if medium != "email" && medium != "sms" {
		return fmt.Errorf("%w: notification medium must be email or sms", domain.ErrValidation)
	}
	path := fmt.Sprintf("/payment_links/%s/notify_by/%s", url.PathEscape(id), medium)
	return g.do(ctx, "notify_payment_link", http.MethodPost, path, struct{}{}, nil)
}

func (g *HTTPGateway) CancelPaymentLink(ctx context.Context, id string) (*PaymentLink, error) {
	var resp paymentLinkResponse
	path := fmt.Sprintf("/payment_links/%s/cancel", url.PathEscape(id))
	if err := g.do(ctx, "cancel_payment_link", http.MethodPost, path, struct{}{}, &resp); err != nil {
		return nil, err
	}
	return resp.normalize(), nil
}

// --- Payments (SRS 12.2) ---

type paymentResponse struct {
	ID               string `json:"id"`
	OrderID          string `json:"order_id"`
	Amount           int64  `json:"amount"`
	Currency         string `json:"currency"`
	Status           string `json:"status"`
	Method           string `json:"method"`
	Captured         bool   `json:"captured"`
	Description      string `json:"description"`
	Email            string `json:"email"`
	Contact          string `json:"contact"`
	CustomerID       string `json:"customer_id"`
	ErrorCode        string `json:"error_code"`
	ErrorDescription string `json:"error_description"`
	ErrorReason      string `json:"error_reason"`
	CreatedAt        int64  `json:"created_at"`
}

func (r paymentResponse) normalize() *Payment {
	return &Payment{
		ID: r.ID, OrderID: r.OrderID, Amount: domain.Money(r.Amount), Currency: r.Currency,
		Status: r.Status, Method: r.Method, Captured: r.Captured, Description: r.Description,
		Email: r.Email, Contact: r.Contact, CustomerID: r.CustomerID,
		ErrorCode: r.ErrorCode, ErrorDescription: r.ErrorDescription, ErrorReason: r.ErrorReason,
		CreatedAt: unix(r.CreatedAt),
	}
}

func (g *HTTPGateway) FetchPayment(ctx context.Context, id string) (*Payment, error) {
	var resp paymentResponse
	if err := g.do(ctx, "fetch_payment", http.MethodGet, "/payments/"+url.PathEscape(id), nil, &resp); err != nil {
		return nil, err
	}
	return resp.normalize(), nil
}

// ListPayments pages through the payments API for backfill (SRS FR-005).
func (g *HTTPGateway) ListPayments(ctx context.Context, from, to time.Time, count int) ([]Payment, error) {
	if count <= 0 || count > 100 {
		count = 100
	}
	var out []Payment
	skip := 0
	// Bound the walk so a wide window cannot spin indefinitely.
	for page := 0; page < 20; page++ {
		q := url.Values{}
		q.Set("count", strconv.Itoa(count))
		q.Set("skip", strconv.Itoa(skip))
		if !from.IsZero() {
			q.Set("from", strconv.FormatInt(from.Unix(), 10))
		}
		if !to.IsZero() {
			q.Set("to", strconv.FormatInt(to.Unix(), 10))
		}
		var resp struct {
			Count int               `json:"count"`
			Items []paymentResponse `json:"items"`
		}
		if err := g.do(ctx, "list_payments", http.MethodGet, "/payments?"+q.Encode(), nil, &resp); err != nil {
			return out, err
		}
		for _, item := range resp.Items {
			out = append(out, *item.normalize())
		}
		if len(resp.Items) < count {
			break
		}
		skip += count
	}
	return out, nil
}

// --- Invoices (SRS 12.4) ---

type invoicePayload struct {
	Type          string            `json:"type"`
	Description   string            `json:"description,omitempty"`
	Customer      *linkCustomer     `json:"customer,omitempty"`
	Amount        int64             `json:"amount"`
	Currency      string            `json:"currency"`
	ReferenceID   string            `json:"reference_id,omitempty"`
	InvoiceNumber string            `json:"invoice_number,omitempty"`
	ExpireBy      int64             `json:"expire_by,omitempty"`
	SMSNotify     int               `json:"sms_notify,omitempty"`
	EmailNotify   int               `json:"email_notify,omitempty"`
	Notes         map[string]string `json:"notes,omitempty"`
}

type invoiceResponse struct {
	ID            string `json:"id"`
	InvoiceNumber string `json:"invoice_number"`
	Status        string `json:"status"`
	Amount        int64  `json:"amount"`
	AmountPaid    int64  `json:"amount_paid"`
	AmountDue     int64  `json:"amount_due"`
	Currency      string `json:"currency"`
	ShortURL      string `json:"short_url"`
	ReferenceID   string `json:"reference_id"`
	DueBy         int64  `json:"due_by"`
	CreatedAt     int64  `json:"created_at"`
}

func (r invoiceResponse) normalize() *Invoice {
	return &Invoice{
		ID: r.ID, InvoiceNumber: r.InvoiceNumber, Status: r.Status,
		Amount: domain.Money(r.Amount), AmountPaid: domain.Money(r.AmountPaid),
		AmountDue: domain.Money(r.AmountDue), Currency: r.Currency, ShortURL: r.ShortURL,
		ReferenceID: r.ReferenceID, DueBy: unix(r.DueBy), CreatedAt: unix(r.CreatedAt),
	}
}

func (g *HTTPGateway) CreateInvoice(ctx context.Context, req InvoiceRequest) (*Invoice, error) {
	payload := invoicePayload{
		Type:          "invoice",
		Description:   req.Description,
		Amount:        int64(req.Amount),
		Currency:      defaultStr(req.Currency, "INR"),
		ReferenceID:   req.ReferenceID,
		InvoiceNumber: req.InvoiceNumber,
		Notes:         req.Notes,
	}
	if req.CustomerName != "" || req.CustomerEmail != "" || req.CustomerContact != "" {
		payload.Customer = &linkCustomer{Name: req.CustomerName, Email: req.CustomerEmail, Contact: req.CustomerContact}
	}
	if !req.ExpireBy.IsZero() {
		payload.ExpireBy = req.ExpireBy.Unix()
	}
	if req.SMSNotify {
		payload.SMSNotify = 1
	}
	if req.EmailNotify {
		payload.EmailNotify = 1
	}
	var resp invoiceResponse
	if err := g.do(ctx, "create_invoice", http.MethodPost, "/invoices", payload, &resp); err != nil {
		return nil, err
	}
	return resp.normalize(), nil
}

func (g *HTTPGateway) FetchInvoice(ctx context.Context, id string) (*Invoice, error) {
	var resp invoiceResponse
	if err := g.do(ctx, "fetch_invoice", http.MethodGet, "/invoices/"+url.PathEscape(id), nil, &resp); err != nil {
		return nil, err
	}
	return resp.normalize(), nil
}

func (g *HTTPGateway) IssueInvoice(ctx context.Context, id string) (*Invoice, error) {
	var resp invoiceResponse
	path := fmt.Sprintf("/invoices/%s/issue", url.PathEscape(id))
	if err := g.do(ctx, "issue_invoice", http.MethodPost, path, struct{}{}, &resp); err != nil {
		return nil, err
	}
	return resp.normalize(), nil
}

func (g *HTTPGateway) NotifyInvoice(ctx context.Context, id, medium string) error {
	if medium != "email" && medium != "sms" {
		return fmt.Errorf("%w: notification medium must be email or sms", domain.ErrValidation)
	}
	path := fmt.Sprintf("/invoices/%s/notify_by/%s", url.PathEscape(id), medium)
	return g.do(ctx, "notify_invoice", http.MethodPost, path, struct{}{}, nil)
}

// --- Subscriptions (SRS 12.5) ---

type subscriptionResponse struct {
	ID             string `json:"id"`
	PlanID         string `json:"plan_id"`
	Status         string `json:"status"`
	CustomerID     string `json:"customer_id"`
	ShortURL       string `json:"short_url"`
	PaidCount      int    `json:"paid_count"`
	TotalCount     int    `json:"total_count"`
	RemainingCount int    `json:"remaining_count"`
	CurrentStart   int64  `json:"current_start"`
	CurrentEnd     int64  `json:"current_end"`
	ChargeAt       int64  `json:"charge_at"`
}

func (r subscriptionResponse) normalize() *Subscription {
	return &Subscription{
		ID: r.ID, PlanID: r.PlanID, Status: r.Status, CustomerID: r.CustomerID,
		ShortURL: r.ShortURL, PaidCount: r.PaidCount, TotalCount: r.TotalCount,
		RemainingCount: r.RemainingCount, CurrentStart: unix(r.CurrentStart),
		CurrentEnd: unix(r.CurrentEnd), ChargeAt: unix(r.ChargeAt),
	}
}

func (g *HTTPGateway) FetchSubscription(ctx context.Context, id string) (*Subscription, error) {
	var resp subscriptionResponse
	if err := g.do(ctx, "fetch_subscription", http.MethodGet, "/subscriptions/"+url.PathEscape(id), nil, &resp); err != nil {
		return nil, err
	}
	return resp.normalize(), nil
}

func (g *HTTPGateway) CancelSubscription(ctx context.Context, id string, atCycleEnd bool) (*Subscription, error) {
	body := map[string]int{"cancel_at_cycle_end": 0}
	if atCycleEnd {
		body["cancel_at_cycle_end"] = 1
	}
	var resp subscriptionResponse
	path := fmt.Sprintf("/subscriptions/%s/cancel", url.PathEscape(id))
	if err := g.do(ctx, "cancel_subscription", http.MethodPost, path, body, &resp); err != nil {
		return nil, err
	}
	return resp.normalize(), nil
}

func defaultStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func unix(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0).UTC()
}
