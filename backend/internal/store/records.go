package store

import (
	"context"
	"time"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// --- customers ---

// UpsertCustomer inserts or updates a customer keyed on razorpay_customer_id
// when present, otherwise on id. Used by both the demo seeder and the backfill
// path (SRS FR-005).
func (s *Store) UpsertCustomer(ctx context.Context, c *domain.Customer) error {
	if c.ID == "" {
		c.ID = NewID("cust")
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = s.now()
	}
	if c.Segment == "" {
		c.Segment = domain.SegmentNew
	}
	const q = `
		INSERT INTO customers (id, razorpay_customer_id, name, email, contact, segment,
		                       lifetime_value, success_rate, total_payments, environment, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'test',$10)
		ON CONFLICT (id) DO UPDATE SET
			name = EXCLUDED.name,
			email = EXCLUDED.email,
			contact = EXCLUDED.contact,
			segment = EXCLUDED.segment,
			lifetime_value = EXCLUDED.lifetime_value,
			success_rate = EXCLUDED.success_rate,
			total_payments = EXCLUDED.total_payments
		RETURNING id`
	return s.pool.QueryRow(ctx, q, c.ID, nullString(c.RazorpayCustomerID), c.Name, c.Email, c.Contact,
		c.Segment, c.LifetimeValue, c.SuccessRate, c.TotalPayments, c.CreatedAt).Scan(&c.ID)
}

const customerCols = `id, COALESCE(razorpay_customer_id,''), name, email, contact, segment,
	lifetime_value, success_rate, total_payments, environment, created_at`

func scanCustomer(row rowScanner) (*domain.Customer, error) {
	var c domain.Customer
	err := row.Scan(&c.ID, &c.RazorpayCustomerID, &c.Name, &c.Email, &c.Contact, &c.Segment,
		&c.LifetimeValue, &c.SuccessRate, &c.TotalPayments, &c.Environment, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// rowScanner covers both pgx.Row and pgx.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// GetCustomer loads one customer.
func (s *Store) GetCustomer(ctx context.Context, id string) (*domain.Customer, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+customerCols+` FROM customers WHERE id = $1`, id)
	c, err := scanCustomer(row)
	if err != nil {
		return nil, notFound(err, "customer "+id)
	}
	return c, nil
}

// FindCustomerByRazorpayID resolves a Razorpay customer id to a local record.
func (s *Store) FindCustomerByRazorpayID(ctx context.Context, rzpID string) (*domain.Customer, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+customerCols+` FROM customers WHERE razorpay_customer_id = $1`, rzpID)
	c, err := scanCustomer(row)
	if err != nil {
		return nil, notFound(err, "customer for razorpay id "+rzpID)
	}
	return c, nil
}

// FindOrCreateCustomerByEmail resolves a customer from webhook contact details,
// creating a minimal record when the customer is unknown. Only the fields
// needed for recovery logic are stored (SRS 19.4 data minimisation).
func (s *Store) FindOrCreateCustomerByEmail(ctx context.Context, email, contact, name string, seg domain.Segment) (*domain.Customer, error) {
	if email != "" {
		row := s.pool.QueryRow(ctx, `SELECT `+customerCols+` FROM customers WHERE email = $1 LIMIT 1`, email)
		if c, err := scanCustomer(row); err == nil {
			return c, nil
		}
	}
	c := &domain.Customer{Name: name, Email: email, Contact: contact, Segment: seg}
	if err := s.UpsertCustomer(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

// ListCustomers returns customers for the demo/admin views.
func (s *Store) ListCustomers(ctx context.Context, limit int) ([]domain.Customer, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT `+customerCols+` FROM customers ORDER BY lifetime_value DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Customer
	for rows.Next() {
		c, err := scanCustomer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// --- transactions ---

// UpsertTransaction records a payment fact, keyed on the Razorpay payment id
// when available. Amount and status always come from the gateway or the
// synthetic dataset — never from model output (SRS 19.2).
func (s *Store) UpsertTransaction(ctx context.Context, t *domain.Transaction) error {
	if t.ID == "" {
		t.ID = NewID("txn")
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = s.now()
	}
	if t.Currency == "" {
		t.Currency = "INR"
	}
	if t.AttemptCount < 1 {
		t.AttemptCount = 1
	}

	// When a Razorpay payment id is present, it is the natural key: a webhook
	// redelivery or a backfill overlap must update the existing row rather than
	// insert a second one.
	if t.RazorpayPaymentID != "" {
		const q = `
			INSERT INTO transactions (id, razorpay_payment_id, razorpay_order_id, customer_id, amount,
			                          currency, status, method, failure_reason, error_code, attempt_count,
			                          environment, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'test',$12)
			ON CONFLICT (razorpay_payment_id) DO UPDATE SET
				status = EXCLUDED.status,
				method = COALESCE(NULLIF(EXCLUDED.method,''), transactions.method),
				failure_reason = COALESCE(NULLIF(EXCLUDED.failure_reason,''), transactions.failure_reason),
				error_code = COALESCE(NULLIF(EXCLUDED.error_code,''), transactions.error_code),
				attempt_count = GREATEST(transactions.attempt_count, EXCLUDED.attempt_count)
			RETURNING id`
		return s.pool.QueryRow(ctx, q, t.ID, t.RazorpayPaymentID, nullString(t.RazorpayOrderID), t.CustomerID,
			t.Amount, t.Currency, t.Status, t.Method, t.FailureReason, t.ErrorCode, t.AttemptCount, t.CreatedAt).Scan(&t.ID)
	}

	const q = `
		INSERT INTO transactions (id, razorpay_payment_id, razorpay_order_id, customer_id, amount,
		                          currency, status, method, failure_reason, error_code, attempt_count,
		                          environment, created_at)
		VALUES ($1,NULL,$2,$3,$4,$5,$6,$7,$8,$9,$10,'test',$11)
		ON CONFLICT (id) DO UPDATE SET status = EXCLUDED.status
		RETURNING id`
	return s.pool.QueryRow(ctx, q, t.ID, nullString(t.RazorpayOrderID), t.CustomerID, t.Amount, t.Currency,
		t.Status, t.Method, t.FailureReason, t.ErrorCode, t.AttemptCount, t.CreatedAt).Scan(&t.ID)
}

const txnCols = `id, COALESCE(razorpay_payment_id,''), COALESCE(razorpay_order_id,''), customer_id,
	amount, currency, status, method, failure_reason, error_code, attempt_count, environment, created_at`

func scanTransaction(row rowScanner) (*domain.Transaction, error) {
	var t domain.Transaction
	err := row.Scan(&t.ID, &t.RazorpayPaymentID, &t.RazorpayOrderID, &t.CustomerID, &t.Amount,
		&t.Currency, &t.Status, &t.Method, &t.FailureReason, &t.ErrorCode, &t.AttemptCount,
		&t.Environment, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// GetTransaction loads one transaction.
func (s *Store) GetTransaction(ctx context.Context, id string) (*domain.Transaction, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+txnCols+` FROM transactions WHERE id = $1`, id)
	t, err := scanTransaction(row)
	if err != nil {
		return nil, notFound(err, "transaction "+id)
	}
	return t, nil
}

// FindTransactionByRazorpayID resolves a gateway payment id.
func (s *Store) FindTransactionByRazorpayID(ctx context.Context, rzpID string) (*domain.Transaction, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+txnCols+` FROM transactions WHERE razorpay_payment_id = $1`, rzpID)
	t, err := scanTransaction(row)
	if err != nil {
		return nil, notFound(err, "transaction for payment "+rzpID)
	}
	return t, nil
}

// CountCustomerAttempts counts prior payment attempts by the same customer for
// the same order, which is the attempt-count signal the risk model uses.
func (s *Store) CountCustomerAttempts(ctx context.Context, customerID, orderID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM transactions
		WHERE customer_id = $1 AND ($2 = '' OR razorpay_order_id = $2)`, customerID, orderID).Scan(&n)
	return n, err
}

// --- checkout sessions ---

// UpsertCheckoutSession records demo-checkout activity (SRS 11.2).
func (s *Store) UpsertCheckoutSession(ctx context.Context, cs *domain.CheckoutSession) error {
	if cs.ID == "" {
		cs.ID = NewID("chk")
	}
	if cs.StartedAt.IsZero() {
		cs.StartedAt = s.now()
	}
	if cs.LastActivityAt.IsZero() {
		cs.LastActivityAt = cs.StartedAt
	}
	if cs.Status == "" {
		cs.Status = "active"
	}
	if cs.ItemCount < 1 {
		cs.ItemCount = 1
	}
	if cs.PageViews < 1 {
		cs.PageViews = 1
	}
	const q = `
		INSERT INTO checkout_sessions (id, customer_id, cart_amount, item_count, page_views,
		                               started_at, last_activity_at, status, environment)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'test')
		ON CONFLICT (id) DO UPDATE SET
			cart_amount = EXCLUDED.cart_amount,
			item_count = EXCLUDED.item_count,
			page_views = GREATEST(checkout_sessions.page_views, EXCLUDED.page_views),
			last_activity_at = GREATEST(checkout_sessions.last_activity_at, EXCLUDED.last_activity_at),
			status = EXCLUDED.status
		RETURNING id`
	return s.pool.QueryRow(ctx, q, cs.ID, cs.CustomerID, cs.CartAmount, cs.ItemCount, cs.PageViews,
		cs.StartedAt, cs.LastActivityAt, cs.Status).Scan(&cs.ID)
}

const checkoutCols = `id, customer_id, cart_amount, item_count, page_views, started_at,
	last_activity_at, status, environment`

func scanCheckout(row rowScanner) (*domain.CheckoutSession, error) {
	var cs domain.CheckoutSession
	err := row.Scan(&cs.ID, &cs.CustomerID, &cs.CartAmount, &cs.ItemCount, &cs.PageViews,
		&cs.StartedAt, &cs.LastActivityAt, &cs.Status, &cs.Environment)
	if err != nil {
		return nil, err
	}
	return &cs, nil
}

// GetCheckoutSession loads one session.
func (s *Store) GetCheckoutSession(ctx context.Context, id string) (*domain.CheckoutSession, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+checkoutCols+` FROM checkout_sessions WHERE id = $1`, id)
	cs, err := scanCheckout(row)
	if err != nil {
		return nil, notFound(err, "checkout session "+id)
	}
	return cs, nil
}

// FindAbandonedCheckouts returns active sessions idle beyond the threshold.
// This is the abandonment detector: an inactivity threshold over first-party
// events, not an inferred Razorpay event (SRS 11.2).
func (s *Store) FindAbandonedCheckouts(ctx context.Context, idleFor time.Duration, limit int) ([]domain.CheckoutSession, error) {
	if limit <= 0 {
		limit = 50
	}
	cutoff := s.now().Add(-idleFor)
	rows, err := s.pool.Query(ctx, `
		SELECT `+checkoutCols+` FROM checkout_sessions
		WHERE status = 'active' AND last_activity_at < $1
		ORDER BY cart_amount DESC LIMIT $2`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.CheckoutSession
	for rows.Next() {
		cs, err := scanCheckout(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *cs)
	}
	return out, rows.Err()
}

// MarkCheckoutStatus transitions a session.
func (s *Store) MarkCheckoutStatus(ctx context.Context, id, status string) error {
	_, err := s.pool.Exec(ctx, `UPDATE checkout_sessions SET status = $2 WHERE id = $1`, id, status)
	return err
}

// --- invoices ---

// UpsertInvoice records a receivable (SRS 11.3).
func (s *Store) UpsertInvoice(ctx context.Context, inv *domain.Invoice) error {
	if inv.ID == "" {
		inv.ID = NewID("inv")
	}
	if inv.CreatedAt.IsZero() {
		inv.CreatedAt = s.now()
	}
	if inv.Status == "" {
		inv.Status = "issued"
	}
	const q = `
		INSERT INTO invoices (id, razorpay_invoice_id, customer_id, invoice_number, amount, amount_paid,
		                      status, due_date, reminder_count, environment, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'test',$10)
		ON CONFLICT (id) DO UPDATE SET
			status = EXCLUDED.status,
			amount_paid = EXCLUDED.amount_paid,
			reminder_count = GREATEST(invoices.reminder_count, EXCLUDED.reminder_count),
			razorpay_invoice_id = COALESCE(EXCLUDED.razorpay_invoice_id, invoices.razorpay_invoice_id)
		RETURNING id`
	return s.pool.QueryRow(ctx, q, inv.ID, nullString(inv.RazorpayInvoiceID), inv.CustomerID, inv.InvoiceNumber,
		inv.Amount, inv.AmountPaid, inv.Status, inv.DueDate, inv.ReminderCount, inv.CreatedAt).Scan(&inv.ID)
}

const invoiceCols = `id, COALESCE(razorpay_invoice_id,''), customer_id, invoice_number, amount,
	amount_paid, status, due_date, reminder_count, environment, created_at`

func scanInvoice(row rowScanner) (*domain.Invoice, error) {
	var inv domain.Invoice
	err := row.Scan(&inv.ID, &inv.RazorpayInvoiceID, &inv.CustomerID, &inv.InvoiceNumber, &inv.Amount,
		&inv.AmountPaid, &inv.Status, &inv.DueDate, &inv.ReminderCount, &inv.Environment, &inv.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &inv, nil
}

// GetInvoice loads one invoice.
func (s *Store) GetInvoice(ctx context.Context, id string) (*domain.Invoice, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+invoiceCols+` FROM invoices WHERE id = $1`, id)
	inv, err := scanInvoice(row)
	if err != nil {
		return nil, notFound(err, "invoice "+id)
	}
	return inv, nil
}

// FindInvoiceByRazorpayID resolves a gateway invoice id.
func (s *Store) FindInvoiceByRazorpayID(ctx context.Context, rzpID string) (*domain.Invoice, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+invoiceCols+` FROM invoices WHERE razorpay_invoice_id = $1`, rzpID)
	inv, err := scanInvoice(row)
	if err != nil {
		return nil, notFound(err, "invoice for razorpay id "+rzpID)
	}
	return inv, nil
}

// FindOverdueInvoices returns unpaid invoices past their due date.
func (s *Store) FindOverdueInvoices(ctx context.Context, limit int) ([]domain.Invoice, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+invoiceCols+` FROM invoices
		WHERE status NOT IN ('paid','cancelled') AND due_date < $1
		ORDER BY amount DESC LIMIT $2`, s.now(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Invoice
	for rows.Next() {
		inv, err := scanInvoice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *inv)
	}
	return out, rows.Err()
}

// IncrementInvoiceReminder bumps the reminder counter used by the stopping rule.
func (s *Store) IncrementInvoiceReminder(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `UPDATE invoices SET reminder_count = reminder_count + 1 WHERE id = $1`, id)
	return err
}

// MarkInvoicePaid settles an invoice.
func (s *Store) MarkInvoicePaid(ctx context.Context, id string, amountPaid domain.Money) error {
	_, err := s.pool.Exec(ctx, `UPDATE invoices SET status = 'paid', amount_paid = $2 WHERE id = $1`, id, amountPaid)
	return err
}

// --- subscriptions ---

// UpsertSubscription records a recurring-billing record (SRS 11.4).
func (s *Store) UpsertSubscription(ctx context.Context, sub *domain.Subscription) error {
	if sub.ID == "" {
		sub.ID = NewID("sub")
	}
	if sub.CreatedAt.IsZero() {
		sub.CreatedAt = s.now()
	}
	if sub.Status == "" {
		sub.Status = "active"
	}
	const q = `
		INSERT INTO subscriptions (id, razorpay_subscription_id, customer_id, plan_id, amount, status,
		                           failed_charge_count, current_end, environment, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'test',$9)
		ON CONFLICT (id) DO UPDATE SET
			status = EXCLUDED.status,
			failed_charge_count = GREATEST(subscriptions.failed_charge_count, EXCLUDED.failed_charge_count),
			current_end = EXCLUDED.current_end
		RETURNING id`
	return s.pool.QueryRow(ctx, q, sub.ID, nullString(sub.RazorpaySubscriptionID), sub.CustomerID, sub.PlanID,
		sub.Amount, sub.Status, sub.FailedChargeCount, timePtr(sub.CurrentEnd), sub.CreatedAt).Scan(&sub.ID)
}

const subCols = `id, COALESCE(razorpay_subscription_id,''), customer_id, plan_id, amount, status,
	failed_charge_count, current_end, environment, created_at`

func scanSubscription(row rowScanner) (*domain.Subscription, error) {
	var sub domain.Subscription
	var currentEnd *time.Time
	err := row.Scan(&sub.ID, &sub.RazorpaySubscriptionID, &sub.CustomerID, &sub.PlanID, &sub.Amount,
		&sub.Status, &sub.FailedChargeCount, &currentEnd, &sub.Environment, &sub.CreatedAt)
	if err != nil {
		return nil, err
	}
	sub.CurrentEnd = derefTime(currentEnd)
	return &sub, nil
}

// GetSubscription loads one subscription.
func (s *Store) GetSubscription(ctx context.Context, id string) (*domain.Subscription, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+subCols+` FROM subscriptions WHERE id = $1`, id)
	sub, err := scanSubscription(row)
	if err != nil {
		return nil, notFound(err, "subscription "+id)
	}
	return sub, nil
}

// FindSubscriptionByRazorpayID resolves a gateway subscription id.
func (s *Store) FindSubscriptionByRazorpayID(ctx context.Context, rzpID string) (*domain.Subscription, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+subCols+` FROM subscriptions WHERE razorpay_subscription_id = $1`, rzpID)
	sub, err := scanSubscription(row)
	if err != nil {
		return nil, notFound(err, "subscription for razorpay id "+rzpID)
	}
	return sub, nil
}
