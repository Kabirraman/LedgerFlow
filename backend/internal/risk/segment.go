package risk

import (
	"strings"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// HighValueLifetimeThreshold is the lifetime value above which a customer is
// treated as high value: ₹1,00,000 in paise. Segment membership changes which
// strategy priors apply and which policy ceiling is compared, so the threshold
// is a named constant rather than a literal buried in a branch (SRS 9.3).
const HighValueLifetimeThreshold = domain.Money(10_000_000)

// RepeatPaymentThreshold is the number of prior payments that distinguishes a
// repeat customer from a new one.
const RepeatPaymentThreshold = 2

// SegmentInput is the trusted fact set for segmentation.
type SegmentInput struct {
	SourceType    domain.SourceType
	LifetimeValue domain.Money
	TotalPayments int
	Email         string
	// HasSubscription is true when the customer has any subscription record,
	// which makes recurring-billing strategies applicable to them.
	HasSubscription bool
}

// freeEmailDomains are consumer mail providers. A customer paying from one of
// these is not treated as B2B regardless of amount, because invoice-style
// collection language and net-terms assumptions do not apply to them.
var freeEmailDomains = map[string]bool{
	"gmail.com": true, "googlemail.com": true, "yahoo.com": true, "yahoo.in": true,
	"outlook.com": true, "hotmail.com": true, "live.com": true, "icloud.com": true,
	"me.com": true, "aol.com": true, "proton.me": true, "protonmail.com": true,
	"rediffmail.com": true, "zoho.com": true, "yandex.com": true, "mail.com": true,
}

// Classify assigns the SRS 9.3 segment.
//
// The order of the checks is the definition, not an implementation detail: a
// customer can satisfy several conditions at once, and the first match wins.
// Subscription comes before high value because the recurring workflow has its
// own recovery dynamics (a failed mandate behaves differently from a failed
// one-off payment of the same size), and B2B comes before value because net
// terms change what an appropriate intervention looks like.
func Classify(in SegmentInput) domain.Segment {
	switch {
	case in.HasSubscription || in.SourceType == domain.SourceSubscriptionFailure:
		return domain.SegmentSubscription
	case in.SourceType == domain.SourceInvoiceOverdue || isBusinessEmail(in.Email):
		return domain.SegmentB2B
	case in.LifetimeValue >= HighValueLifetimeThreshold:
		return domain.SegmentHighValue
	case in.TotalPayments >= RepeatPaymentThreshold:
		return domain.SegmentRepeat
	default:
		return domain.SegmentNew
	}
}

// isBusinessEmail reports whether the address is on a custom domain. This is a
// heuristic and is deliberately conservative: an unparseable or empty address
// returns false rather than guessing B2B, since B2B changes the collection
// approach and a wrong guess sends invoice language to a consumer.
func isBusinessEmail(email string) bool {
	at := strings.LastIndex(email, "@")
	if at < 1 || at == len(email)-1 {
		return false
	}
	domainPart := strings.ToLower(strings.TrimSpace(email[at+1:]))
	if domainPart == "" || !strings.Contains(domainPart, ".") {
		return false
	}
	return !freeEmailDomains[domainPart]
}
