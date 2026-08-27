package events

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ledgerflow/ledgerflow/internal/domain"
	"github.com/ledgerflow/ledgerflow/internal/razorpay"
	"github.com/ledgerflow/ledgerflow/internal/risk"
)

// BackfillReport summarises one sync pass.
type BackfillReport struct {
	Fetched     int      `json:"fetched"`
	Recorded    int      `json:"recorded"`
	Duplicates  int      `json:"duplicates"`
	CasesOpened int      `json:"cases_opened"`
	Ignored     int      `json:"ignored"`
	From        string   `json:"from"`
	To          string   `json:"to"`
	Errors      []string `json:"errors,omitempty"`
}

// BackfillPayments records payments already fetched from the Razorpay API and
// opens cases for the ones that represent revenue at risk (SRS FR-005).
//
// Backfill exists because webhook delivery is not a complete record: a delivery
// can be missed while the service is down, and a merchant onboarding LEDGERFLOW
// has failures that predate it entirely.
//
// The payments are passed in rather than fetched here. Reading the payments API is
// the caller's business; this package's business is turning facts into cases, and
// keeping the fetch outside it means the ingestor holds no gateway and no
// credentials.
//
// Every payment goes through the same upsertPayment and openCase used by the
// webhook path, so a case discovered by polling is scored by the same SRS 9.1
// formula, carries the same reason codes, and is deduplicated by the same
// source-record index as one discovered by webhook. A separate scoring path here
// would mean two sets of numbers for the same money.
func (i *Ingestor) BackfillPayments(ctx context.Context, payments []razorpay.Payment) (BackfillReport, error) {
	rep := BackfillReport{Fetched: len(payments)}
	if i == nil || i.store == nil {
		return rep, fmt.Errorf("%w: ingestor is not configured", domain.ErrValidation)
	}

	for idx := range payments {
		res, err := i.BackfillPayment(ctx, payments[idx])
		switch {
		case err != nil:
			rep.Errors = append(rep.Errors, fmt.Sprintf("payment %s: %v", payments[idx].ID, err))
		case res.Duplicate:
			rep.Duplicates++
		default:
			rep.Recorded++
			if res.CaseCreated {
				rep.CasesOpened++
			}
			if res.Ignored {
				rep.Ignored++
			}
		}
	}
	return rep, nil
}

// BackfillPayment records one fetched payment.
//
// An unchanged payment seen by a second sync is reported as a duplicate and does
// no further work: the external event id includes the status, so a payment whose
// status has since moved is a genuinely new fact and is processed, while a
// re-listing of the same failure is not.
func (i *Ingestor) BackfillPayment(ctx context.Context, p razorpay.Payment) (Result, error) {
	res := Result{EventType: "payment.backfill", SourceType: domain.SourcePaymentFailure}
	if p.ID == "" {
		return res, fmt.Errorf("%w: payment carried no id", domain.ErrValidation)
	}

	ent := paymentEntity(p)
	payload, err := json.Marshal(ent)
	if err != nil {
		return res, fmt.Errorf("encode backfilled payment: %w", err)
	}

	ev := &domain.Event{
		Source:          "razorpay_api",
		ExternalEventID: "backfill:" + p.ID + ":" + p.Status,
		EventType:       res.EventType,
		PayloadJSON:     payload,
		// A backfilled payment was read from an authenticated API call rather than
		// received unsolicited, so there is no HMAC to verify and nothing was
		// trusted on the strength of a signature. Recording it as verified would
		// claim a check that never ran.
		SignatureValid:  false,
		RejectionReason: "",
		EntityID:        p.ID,
		Environment:     domain.EnvTest,
	}
	if !p.CreatedAt.IsZero() {
		at := p.CreatedAt.UTC()
		ev.EntityTimestamp = &at
	}

	created, err := i.store.RecordEvent(ctx, ev)
	if err != nil {
		return res, fmt.Errorf("record backfill event: %w", err)
	}
	res.EventID = ev.ID
	if !created {
		res.Duplicate = true
		res.Reason = "payment already synced at this status"
		_ = i.store.IncrCounter(ctx, "duplicate_events")
		return res, nil
	}

	cust, err := i.customerForPayment(ctx, ent, domain.SourcePaymentFailure)
	if err != nil {
		return res, err
	}
	txn, err := i.upsertPayment(ctx, ent, cust.ID)
	if err != nil {
		return res, err
	}
	res.Accepted = true

	// Successful payments are still recorded — they are what customer success rate
	// and lifetime value are computed from — but they open no case.
	if isSuccessfulStatus(p.Status) {
		res.Ignored = true
		res.Reason = "payment status is " + p.Status
		if err := i.store.MarkEventProcessed(ctx, ev.ID); err != nil {
			return res, err
		}
		return res, nil
	}

	err = i.openCase(ctx, caseSeed{
		SourceType: domain.SourcePaymentFailure,
		Customer:   cust,
		SourceID:   txn.ID,
		Amount:     txn.Amount,
		Features: risk.Features{
			SourceType:          domain.SourcePaymentFailure,
			Amount:              txn.Amount,
			ErrorCode:           txn.ErrorCode,
			FailureReason:       txn.FailureReason,
			AttemptCount:        txn.AttemptCount,
			Segment:             cust.Segment,
			CustomerSuccessRate: cust.SuccessRate,
			LifetimeValue:       cust.LifetimeValue,
			TotalPayments:       cust.TotalPayments,
			AgeMinutes:          i.ageMinutes(p.CreatedAt),
		},
	}, &res)
	if err != nil {
		return res, err
	}
	if err := i.store.MarkEventProcessed(ctx, ev.ID); err != nil {
		return res, err
	}
	return res, nil
}

// ageMinutes is how long ago the payment failed, which drives the time-sensitivity
// and recovery-window terms of the risk score. A backfill is usually looking at
// older failures than a webhook, and pretending they just happened would inflate
// their urgency.
func (i *Ingestor) ageMinutes(at time.Time) int {
	if at.IsZero() {
		return 0
	}
	mins := int(i.now().Sub(at).Minutes())
	if mins < 0 {
		return 0
	}
	return mins
}

// paymentEntity converts the API's payment shape into the webhook entity shape, so
// both paths share one code path from here on.
func paymentEntity(p razorpay.Payment) *PaymentEntity {
	ent := &PaymentEntity{
		ID:               p.ID,
		OrderID:          p.OrderID,
		Amount:           int64(p.Amount),
		Currency:         defaultStr(p.Currency, "INR"),
		Status:           p.Status,
		Method:           p.Method,
		Captured:         p.Captured,
		Description:      p.Description,
		Email:            p.Email,
		Contact:          p.Contact,
		CustomerID:       p.CustomerID,
		ErrorCode:        p.ErrorCode,
		ErrorDescription: p.ErrorDescription,
		ErrorReason:      p.ErrorReason,
	}
	if !p.CreatedAt.IsZero() {
		ent.CreatedAt = p.CreatedAt.Unix()
	}
	return ent
}
