// Package simulation is LEDGERFLOW's Simulation Lab and benchmark harness
// (SRS 17, 22.3, 25.2).
//
// It answers one question honestly: does the four-agent system recover more
// revenue than a simple rule would, on the same cases, under the same controls?
//
// Three properties make the answer defensible, and each one is a deliberate
// constraint on this package:
//
//   - Reproducibility. The dataset is a pure function of (version, seed, size),
//     and every outcome is a pure function of (seed, case, action, attempt). Two
//     people running the same benchmark get identical numbers, so a claimed uplift
//     can be checked rather than believed (SRS NFR-008, 25.2).
//   - Fairness. All strategies face one shared world model. A customer's
//     willingness to pay on their n-th contact is fixed before any strategy runs,
//     so strategies are compared on what they chose, not on luckier dice.
//   - Isolation. This package holds no gateway and no executor. There is no wire
//     to Razorpay to accidentally call — not a flag that could be misconfigured,
//     but an absence (SRS AC-009, FR-054).
//
// Ground truth lives only in domain.BenchmarkCase and is never copied into the
// Scenario handed to a strategy, so a strategy cannot read the answer it is being
// graded on (SRS 17.2).
package simulation

import (
	"fmt"
	"math/rand"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// Dataset identity. Changing any generation rule below requires bumping
// DatasetVersion: results from two different generators must never be compared
// under one label (SRS 25.2).
const (
	// DatasetVersion labels the generation rules in this file.
	DatasetVersion = "v1"
	// DefaultSeed is the published seed for the demo benchmark.
	DefaultSeed = int64(20260824)
	// DefaultSize is the SRS 17.1 target of 200 cases.
	DefaultSize = 200
	// DemoSize is the smaller batch the SRS permits for the live demo, kept as a
	// named constant so a shortened run is still a declared configuration rather
	// than an arbitrary number typed at the time.
	DemoSize = 100
)

// Mix is the SRS 17.1 case distribution.
type Mix struct {
	PaymentFailures      int `json:"payment_failures"`
	CheckoutAbandonment  int `json:"checkout_abandonment"`
	OverdueInvoices      int `json:"overdue_invoices"`
	SubscriptionFailures int `json:"subscription_failures"`
	EdgeCases            int `json:"edge_cases"`
}

// DefaultMix is the distribution given in SRS 17.1 for a 200-case benchmark.
var DefaultMix = Mix{
	PaymentFailures:      70,
	CheckoutAbandonment:  40,
	OverdueInvoices:      40,
	SubscriptionFailures: 30,
	EdgeCases:            20,
}

// Total is the number of cases the mix produces.
func (m Mix) Total() int {
	return m.PaymentFailures + m.CheckoutAbandonment + m.OverdueInvoices +
		m.SubscriptionFailures + m.EdgeCases
}

// Map renders the mix for storage alongside the dataset, so a stored result
// records the distribution it was measured on.
func (m Mix) Map() map[string]int {
	return map[string]int{
		string(domain.SourcePaymentFailure):      m.PaymentFailures,
		string(domain.SourceCheckoutAbandonment): m.CheckoutAbandonment,
		string(domain.SourceInvoiceOverdue):      m.OverdueInvoices,
		string(domain.SourceSubscriptionFailure): m.SubscriptionFailures,
		"edge_cases":                             m.EdgeCases,
	}
}

// scaleTo rescales the mix to a different total, preserving the SRS proportions.
//
// Edge cases are allocated first and never rounded away: they are the cases that
// prove the safety rules work, so a benchmark without them would be easier and
// less informative. The remainder lands on payment failures, the largest bucket,
// where one case either way changes the proportions least.
func (m Mix) scaleTo(size int) Mix {
	total := m.Total()
	if size <= 0 || size == total {
		return m
	}
	ratio := float64(size) / float64(total)
	scaled := Mix{
		CheckoutAbandonment:  int(float64(m.CheckoutAbandonment) * ratio),
		OverdueInvoices:      int(float64(m.OverdueInvoices) * ratio),
		SubscriptionFailures: int(float64(m.SubscriptionFailures) * ratio),
		EdgeCases:            int(float64(m.EdgeCases) * ratio),
	}
	if scaled.EdgeCases < 1 && size >= len(edgeKinds) {
		scaled.EdgeCases = 1
	}
	if scaled.EdgeCases > len(edgeKinds) {
		scaled.EdgeCases = len(edgeKinds)
	}
	scaled.PaymentFailures = size - scaled.CheckoutAbandonment - scaled.OverdueInvoices -
		scaled.SubscriptionFailures - scaled.EdgeCases
	if scaled.PaymentFailures < 0 {
		scaled.PaymentFailures = 0
	}
	return scaled
}

// GenerateDataset builds the benchmark deterministically.
//
// The only randomness is a seeded PRNG drawn in a fixed order, so the same
// (version, seed, size) always produces byte-identical cases. Nothing here reads
// the clock: case ages are expressed as minutes and days relative to evaluation
// time, so a dataset generated today and replayed next month is the same dataset
// (SRS NFR-008).
func GenerateDataset(version string, seed int64, size int) domain.BenchmarkDataset {
	if version == "" {
		version = DatasetVersion
	}
	if size <= 0 {
		size = DefaultSize
	}
	mix := DefaultMix.scaleTo(size)

	g := &generator{rnd: rand.New(rand.NewSource(seed))}
	cases := make([]domain.BenchmarkCase, 0, mix.Total())
	for i := 0; i < mix.PaymentFailures; i++ {
		cases = append(cases, g.paymentFailure())
	}
	for i := 0; i < mix.CheckoutAbandonment; i++ {
		cases = append(cases, g.checkoutAbandonment())
	}
	for i := 0; i < mix.OverdueInvoices; i++ {
		cases = append(cases, g.overdueInvoice())
	}
	for i := 0; i < mix.SubscriptionFailures; i++ {
		cases = append(cases, g.subscriptionFailure())
	}
	for i := 0; i < mix.EdgeCases; i++ {
		cases = append(cases, g.edgeCase(edgeKinds[i%len(edgeKinds)]))
	}

	return domain.BenchmarkDataset{
		Version: version,
		Seed:    seed,
		Size:    len(cases),
		Mix:     mix.Map(),
		Cases:   cases,
	}
}

// generator holds the seeded PRNG and the case counter. Every draw goes through
// it, so generation order is the only thing that determines the stream.
type generator struct {
	rnd *rand.Rand
	seq int
}

// nextID assigns stable, sortable case ids. The id is also the world model's
// correlation key, so it must not depend on anything but position.
func (g *generator) nextID() string {
	g.seq++
	return fmt.Sprintf("SIM-%04d", g.seq)
}

// pick returns one element of a slice.
func pick[T any](g *generator, in []T) T { return in[g.rnd.Intn(len(in))] }

// money returns an amount in paise, rounded to whole rupees so generated
// figures read like real prices rather than arbitrary fractions.
func (g *generator) money(minRupees, maxRupees int) domain.Money {
	if maxRupees <= minRupees {
		return domain.Money(minRupees * 100)
	}
	return domain.Money((minRupees + g.rnd.Intn(maxRupees-minRupees)) * 100)
}

// probability returns a value in [lo,hi] rounded to two places, so the stored
// ground-truth response curve is readable in the dataset JSON.
func (g *generator) probability(lo, hi float64) float64 {
	if hi <= lo {
		return lo
	}
	v := lo + g.rnd.Float64()*(hi-lo)
	return float64(int(v*100+0.5)) / 100
}

// segments in generation-weight order: repeat buyers dominate a real book of
// business, high-value and B2B are the minority that drives the escalation path.
var paymentSegments = []domain.Segment{
	domain.SegmentRepeat, domain.SegmentRepeat, domain.SegmentRepeat,
	domain.SegmentNew, domain.SegmentNew,
	domain.SegmentHighValue, domain.SegmentB2B,
}

// failureProfile ties a Razorpay-style error code to what it actually means for
// recovery. Keeping the three together prevents the dataset from containing an
// incoherent case — a transient network error that is somehow unrecoverable, or
// a stolen card that a retry fixes.
type failureProfile struct {
	code        string
	reason      string
	rootCause   domain.RootCause
	recoverable bool
	// best is the action a well-informed operator would choose, and alt the set
	// a reviewer would accept as equally reasonable (SRS 3.2).
	best domain.ActionType
	alt  []domain.ActionType
	// retryOdds / linkOdds / reminderOdds are the ground-truth response curve
	// for a first contact, before attempt decay.
	retryOdds, linkOdds, reminderOdds float64
}

// paymentProfiles is the failure taxonomy the benchmark draws from. The odds
// encode the domain fact that drives this whole product: a transient failure is
// fixed by retrying, while a funds or authentication problem needs the customer
// back at a checkout, and no amount of retrying will help.
var paymentProfiles = []failureProfile{
	{
		code: "GATEWAY_ERROR", reason: "Payment processing failed at the gateway",
		rootCause: domain.RootCauseTransientFailure, recoverable: true,
		best: domain.ActionRetry, alt: []domain.ActionType{domain.ActionPaymentLink},
		retryOdds: 0.72, linkOdds: 0.55, reminderOdds: 0.20,
	},
	{
		code: "NETWORK_ERROR", reason: "Connection to the bank timed out",
		rootCause: domain.RootCauseTransientFailure, recoverable: true,
		best: domain.ActionRetry, alt: []domain.ActionType{domain.ActionPaymentLink},
		retryOdds: 0.68, linkOdds: 0.52, reminderOdds: 0.18,
	},
	{
		code: "BAD_REQUEST_ERROR", reason: "Issuer declined: insufficient funds",
		rootCause: domain.RootCauseInsufficientFunds, recoverable: true,
		best: domain.ActionPaymentLink, alt: []domain.ActionType{domain.ActionReminder},
		retryOdds: 0.12, linkOdds: 0.44, reminderOdds: 0.38,
	},
	{
		code: "AUTHENTICATION_ERROR", reason: "3D Secure authentication failed",
		rootCause: domain.RootCauseAuthenticationFailed, recoverable: true,
		best: domain.ActionPaymentLink, alt: []domain.ActionType{domain.ActionRetry},
		retryOdds: 0.28, linkOdds: 0.61, reminderOdds: 0.16,
	},
	{
		code: "CARD_DECLINED", reason: "Card declined by issuing bank",
		rootCause: domain.RootCauseInsufficientFunds, recoverable: true,
		best: domain.ActionPaymentLink, alt: []domain.ActionType{domain.ActionReminder},
		retryOdds: 0.15, linkOdds: 0.47, reminderOdds: 0.31,
	},
	{
		code: "PAYMENT_TIMEOUT", reason: "Customer did not complete the payment in time",
		rootCause: domain.RootCauseTransientFailure, recoverable: true,
		best: domain.ActionPaymentLink, alt: []domain.ActionType{domain.ActionRetry, domain.ActionReminder},
		retryOdds: 0.34, linkOdds: 0.58, reminderOdds: 0.29,
	},
	{
		code: "CARD_STOLEN", reason: "Card reported lost or stolen",
		rootCause: domain.RootCauseUnknown, recoverable: false,
		best: domain.ActionNoAction, alt: []domain.ActionType{domain.ActionEscalate},
	},
}

var methods = []string{"card", "upi", "netbanking", "wallet"}

// paymentFailure builds a failed-payment case (SRS 6.1).
func (g *generator) paymentFailure() domain.BenchmarkCase {
	p := pick(g, paymentProfiles)
	seg := pick(g, paymentSegments)
	c := domain.BenchmarkCase{
		ID:            g.nextID(),
		SourceType:    domain.SourcePaymentFailure,
		Amount:        g.amountFor(seg),
		Method:        pick(g, methods),
		ErrorCode:     p.code,
		FailureReason: p.reason,
		AttemptCount:  1 + g.rnd.Intn(2),
		AgeMinutes:    5 + g.rnd.Intn(180),
		Segment:       seg,
	}
	g.applyCustomer(&c, seg)
	g.applyProfile(&c, p)
	return c
}

// checkoutAbandonment builds an abandoned-cart case (SRS 6.2).
//
// Intent is the whole signal here: page views and minutes since abandonment
// decide whether this is a distracted buyer worth one nudge or a browser who was
// never going to purchase.
func (g *generator) checkoutAbandonment() domain.BenchmarkCase {
	seg := pick(g, []domain.Segment{domain.SegmentNew, domain.SegmentNew, domain.SegmentRepeat, domain.SegmentHighValue})
	views := 1 + g.rnd.Intn(9)
	idle := 10 + g.rnd.Intn(600)
	c := domain.BenchmarkCase{
		ID:                  g.nextID(),
		SourceType:          domain.SourceCheckoutAbandonment,
		Amount:              g.amountFor(seg),
		AttemptCount:        0,
		AgeMinutes:          idle,
		Segment:             seg,
		CheckoutViews:       views,
		MinutesSinceAbandon: idle,
		TrueRootCause:       domain.RootCauseCheckoutAbandonment,
	}
	g.applyCustomer(&c, seg)

	// High intent: repeated views and a recent exit. Low intent: one view, hours
	// ago. The middle is recoverable but not reliably.
	highIntent := views >= 5 && idle <= 180
	lowIntent := views <= 2 && idle > 360
	switch {
	case lowIntent:
		c.Recoverable = false
		c.BenchmarkBestAction = domain.ActionNoAction
		c.AcceptableActions = []domain.ActionType{domain.ActionNoAction}
		c.RecoveryProbabilityByAction = zeroCurve()
	case highIntent:
		c.Recoverable = true
		c.BenchmarkBestAction = domain.ActionReminder
		c.AcceptableActions = []domain.ActionType{domain.ActionPaymentLink}
		c.RecoveryProbabilityByAction = map[domain.ActionType]float64{
			domain.ActionReminder:    g.probability(0.44, 0.62),
			domain.ActionPaymentLink: g.probability(0.40, 0.58),
			domain.ActionRetry:       0,
		}
	default:
		c.Recoverable = true
		c.BenchmarkBestAction = domain.ActionReminder
		c.AcceptableActions = []domain.ActionType{domain.ActionPaymentLink, domain.ActionNoAction}
		c.RecoveryProbabilityByAction = map[domain.ActionType]float64{
			domain.ActionReminder:    g.probability(0.18, 0.34),
			domain.ActionPaymentLink: g.probability(0.16, 0.30),
			domain.ActionRetry:       0,
		}
	}
	// An abandoned checkout has no authorised payment to re-present, so a retry
	// is meaningless regardless of intent. The curve says so explicitly rather
	// than leaving the key absent, because absent would read as "not measured".
	return c
}

// overdueInvoice builds an unpaid-receivable case (SRS 6.3).
func (g *generator) overdueInvoice() domain.BenchmarkCase {
	seg := pick(g, []domain.Segment{domain.SegmentB2B, domain.SegmentB2B, domain.SegmentRepeat, domain.SegmentHighValue})
	overdue := 1 + g.rnd.Intn(60)
	c := domain.BenchmarkCase{
		ID:            g.nextID(),
		SourceType:    domain.SourceInvoiceOverdue,
		Amount:        g.amountFor(seg),
		AgeMinutes:    overdue * 24 * 60,
		Segment:       seg,
		DaysOverdue:   overdue,
		ReminderCount: g.rnd.Intn(2),
		TrueRootCause: domain.RootCauseOverdueReceivable,
	}
	g.applyCustomer(&c, seg)

	// Collectability decays with age. Past two months a receivable needs a
	// person, not another automated email.
	switch {
	case overdue > 45:
		c.Recoverable = true
		c.BenchmarkBestAction = domain.ActionEscalate
		c.AcceptableActions = []domain.ActionType{domain.ActionReminder}
		c.RecoveryProbabilityByAction = map[domain.ActionType]float64{
			domain.ActionReminder:    g.probability(0.10, 0.20),
			domain.ActionPaymentLink: g.probability(0.12, 0.24),
			domain.ActionRetry:       0,
		}
	case overdue > 14:
		c.Recoverable = true
		c.BenchmarkBestAction = domain.ActionReminder
		c.AcceptableActions = []domain.ActionType{domain.ActionPaymentLink, domain.ActionEscalate}
		c.RecoveryProbabilityByAction = map[domain.ActionType]float64{
			domain.ActionReminder:    g.probability(0.28, 0.44),
			domain.ActionPaymentLink: g.probability(0.30, 0.46),
			domain.ActionRetry:       0,
		}
	default:
		c.Recoverable = true
		c.BenchmarkBestAction = domain.ActionReminder
		c.AcceptableActions = []domain.ActionType{domain.ActionPaymentLink}
		c.RecoveryProbabilityByAction = map[domain.ActionType]float64{
			domain.ActionReminder:    g.probability(0.48, 0.66),
			domain.ActionPaymentLink: g.probability(0.42, 0.60),
			domain.ActionRetry:       0,
		}
	}
	return c
}

// subscriptionFailure builds a failed recurring charge (SRS 6.4).
func (g *generator) subscriptionFailure() domain.BenchmarkCase {
	failed := 1 + g.rnd.Intn(3)
	c := domain.BenchmarkCase{
		ID:            g.nextID(),
		SourceType:    domain.SourceSubscriptionFailure,
		Amount:        g.money(299, 4999),
		Method:        "card",
		ErrorCode:     "BAD_REQUEST_ERROR",
		FailureReason: "Recurring charge declined by the issuing bank",
		AttemptCount:  failed,
		AgeMinutes:    60 + g.rnd.Intn(2880),
		Segment:       domain.SegmentSubscription,
		TrueRootCause: domain.RootCauseSubscriptionFailure,
	}
	g.applyCustomer(&c, domain.SegmentSubscription)

	// A subscriber who has failed once is usually a card that needs updating; a
	// subscriber who has failed three times is usually gone.
	if failed >= 3 {
		c.Recoverable = false
		c.BenchmarkBestAction = domain.ActionNoAction
		c.AcceptableActions = []domain.ActionType{domain.ActionEscalate}
		c.RecoveryProbabilityByAction = zeroCurve()
		return c
	}
	c.Recoverable = true
	c.BenchmarkBestAction = domain.ActionPaymentLink
	c.AcceptableActions = []domain.ActionType{domain.ActionRetry, domain.ActionReminder}
	c.RecoveryProbabilityByAction = map[domain.ActionType]float64{
		domain.ActionRetry:       g.probability(0.24, 0.38),
		domain.ActionPaymentLink: g.probability(0.46, 0.64),
		domain.ActionReminder:    g.probability(0.22, 0.36),
	}
	return c
}

// amountFor draws an amount in the range the segment plausibly transacts in.
// The spread matters: it is what puts a meaningful minority of cases above the
// human-approval threshold so the escalation path is actually exercised.
func (g *generator) amountFor(seg domain.Segment) domain.Money {
	switch seg {
	case domain.SegmentB2B:
		return g.money(8000, 180000)
	case domain.SegmentHighValue:
		return g.money(15000, 90000)
	case domain.SegmentSubscription:
		return g.money(299, 4999)
	case domain.SegmentRepeat:
		return g.money(499, 24000)
	default:
		return g.money(199, 12000)
	}
}

// applyCustomer fills the customer and history facts for a segment.
func (g *generator) applyCustomer(c *domain.BenchmarkCase, seg domain.Segment) {
	switch seg {
	case domain.SegmentB2B:
		c.CustomerSuccessRate = g.probability(0.70, 0.96)
		c.LifetimeValue = g.money(50000, 900000)
		c.RecencyDays = g.rnd.Intn(90)
		c.PriorRecoveries = g.rnd.Intn(3)
		c.TotalPayments = 6 + g.rnd.Intn(40)
	case domain.SegmentHighValue:
		c.CustomerSuccessRate = g.probability(0.78, 0.98)
		c.LifetimeValue = g.money(80000, 600000)
		c.RecencyDays = g.rnd.Intn(45)
		c.PriorRecoveries = g.rnd.Intn(3)
		c.TotalPayments = 8 + g.rnd.Intn(30)
	case domain.SegmentSubscription:
		c.CustomerSuccessRate = g.probability(0.72, 0.97)
		c.LifetimeValue = g.money(3000, 60000)
		c.RecencyDays = g.rnd.Intn(35)
		c.PriorRecoveries = g.rnd.Intn(2)
		c.TotalPayments = 3 + g.rnd.Intn(24)
	case domain.SegmentRepeat:
		c.CustomerSuccessRate = g.probability(0.62, 0.95)
		c.LifetimeValue = g.money(4000, 120000)
		c.RecencyDays = g.rnd.Intn(120)
		c.PriorRecoveries = g.rnd.Intn(2)
		c.TotalPayments = 2 + g.rnd.Intn(18)
	default: // new
		// A new customer genuinely has no history. Leaving success rate at zero
		// would be a lie the risk scorer would act on, so recency is marked
		// unknown with -1 and payments with zero, which the scorer reads as
		// "no signal" rather than "bad signal".
		c.CustomerSuccessRate = 0
		c.LifetimeValue = 0
		c.RecencyDays = -1
		c.PriorRecoveries = 0
		c.TotalPayments = 0
	}
	c.PriorFailedActions = 0
}

// applyProfile copies a failure profile's ground truth onto a case.
func (g *generator) applyProfile(c *domain.BenchmarkCase, p failureProfile) {
	c.TrueRootCause = p.rootCause
	c.Recoverable = p.recoverable
	c.BenchmarkBestAction = p.best
	c.AcceptableActions = p.alt
	if !p.recoverable {
		c.RecoveryProbabilityByAction = zeroCurve()
		return
	}
	c.RecoveryProbabilityByAction = map[domain.ActionType]float64{
		domain.ActionRetry:       p.retryOdds,
		domain.ActionPaymentLink: p.linkOdds,
		domain.ActionReminder:    p.reminderOdds,
	}
}

// zeroCurve is the response curve of a case that cannot be recovered: every
// action, however well chosen, returns nothing.
func zeroCurve() map[domain.ActionType]float64 {
	return map[domain.ActionType]float64{
		domain.ActionRetry:       0,
		domain.ActionPaymentLink: 0,
		domain.ActionReminder:    0,
	}
}
