package httpapi

import (
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// razorpayWebhook receives Razorpay test-mode events (SRS FR-001, FR-002, 19.3).
//
// The body is read as raw bytes and handed to the ingestor unparsed. Nothing in
// this handler decodes it, because the HMAC is computed over the exact bytes on the
// wire: a JSON round-trip through a struct would reorder keys and change
// whitespace, and the signature would then be verified against a body that is not
// the one that was sent. gin's ShouldBindJSON is deliberately not used here.
//
// The status codes are chosen for the sender rather than for us:
//
//   - 200 for a duplicate. Razorpay redelivers on any non-2xx, and a redelivery of
//     an event we already handled correctly would be an infinite retry loop over a
//     non-problem (SRS AC-006).
//   - 400 for a body we cannot use. Retrying will not fix it.
//   - 401 for a bad signature, which is a real rejection and is persisted so a
//     misconfigured secret is visible instead of silent (SRS FR-002).
//   - 500 only for our own failures, which is the one case where a redelivery is
//     genuinely wanted.
func (s *Server) razorpayWebhook(c *gin.Context) {
	if s.deps.Ingestor == nil {
		notConfigured(c, "webhook ingestion")
		return
	}
	if s.deps.Config.Razorpay.WebhookSecret == "" {
		// Without a secret there is no signature to verify, and accepting the event
		// anyway would mean acting on unauthenticated instructions about money. The
		// refusal is reported as a misconfiguration rather than as a rejected
		// signature, because that is what it is — and 503 tells Razorpay to redeliver
		// once the secret is in place, where a 401 would say "never send this again"
		// (SRS 19.3, FR-002).
		notConfigured(c, "webhook signature verification (RAZORPAY_WEBHOOK_SECRET is unset)")
		return
	}

	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		// A truncated body includes the case where limitBody's MaxBytesReader tripped.
		// Either way the bytes are not the bytes that were signed, so there is nothing
		// to verify.
		failWith(c, http.StatusBadRequest, "unreadable_body", "the request body could not be read in full")
		return
	}

	signature := c.GetHeader("X-Razorpay-Signature")
	eventID := c.GetHeader("X-Razorpay-Event-Id")

	res, err := s.deps.Ingestor.IngestWebhook(c.Request.Context(), body, signature, eventID)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrInvalidSignature):
			// Logged at warn: one of these is a misconfiguration, many is an attack,
			// and neither should be invisible.
			s.log.WarnContext(c.Request.Context(), "webhook signature rejected",
				"event_id_header", eventID, "bytes", len(body))
			failWith(c, http.StatusUnauthorized, "invalid_signature", "the webhook signature did not verify")
		case errors.Is(err, domain.ErrDuplicateEvent):
			ok(c, gin.H{"status": "duplicate", "event_id": res.EventID})
		case errors.Is(err, domain.ErrValidation):
			failWith(c, http.StatusBadRequest, "invalid_webhook", err.Error())
		default:
			fail(c, err)
		}
		return
	}

	if res.CaseID != "" {
		// Recorded on the context so requestLogger attaches the case id to this
		// request's log line, which is what makes the ingestion of a specific case
		// searchable afterwards (SRS 21.2).
		c.Set(caseIDKey, res.CaseID)
	}

	status := "accepted"
	switch {
	case res.Duplicate:
		status = "duplicate"
	case res.Stale:
		status = "stale"
	case res.Ignored:
		status = "ignored"
	}

	// Always 200 from here. Everything below this line is an event we successfully
	// took responsibility for, including the ones we decided not to act on.
	ok(c, gin.H{
		"status":         status,
		"event_id":       res.EventID,
		"event_type":     res.EventType,
		"case_id":        res.CaseID,
		"case_reference": res.CaseReference,
		"case_created":   res.CaseCreated,
		"reason":         res.Reason,
	})
}
