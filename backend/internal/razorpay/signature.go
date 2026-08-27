// Package razorpay is the only package that holds Razorpay credentials and the
// only one that performs money-related external calls (SRS FR-042, 13.1).
//
// Two safety properties are enforced structurally rather than by convention:
//
//   - Simulation mode has no transport. A simulated run uses SimulatedClient,
//     which cannot reach the network at all, so SRS AC-009 holds by
//     construction rather than by a runtime flag check.
//   - Signature verification uses the raw request body. The webhook handler
//     passes bytes straight from the socket, before any JSON parsing, because
//     re-serialising the body would change the bytes the HMAC was computed over
//     (SRS 19.3).
package razorpay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

// SignatureHeader is the header Razorpay sends the webhook HMAC in.
const SignatureHeader = "X-Razorpay-Signature"

// EventIDHeader is the header carrying Razorpay's unique event identifier,
// used for deduplication.
const EventIDHeader = "X-Razorpay-Event-Id"

// VerifyWebhookSignature validates an inbound webhook against the configured
// secret using HMAC-SHA256 over the raw body (SRS 12.6, 19.3).
//
// body must be the exact bytes received. secret must be the webhook secret
// configured in the Razorpay dashboard, not the API key secret.
func VerifyWebhookSignature(body []byte, signature, secret string) error {
	if secret == "" {
		return errors.New("webhook secret is not configured")
	}
	if signature == "" {
		return fmt.Errorf("%w: missing %s header", domain.ErrInvalidSignature, SignatureHeader)
	}
	expected := ComputeSignature(body, secret)
	// hmac.Equal is constant time; a plain == would leak the prefix length of a
	// forged signature through timing.
	if !hmac.Equal([]byte(strings.ToLower(signature)), []byte(expected)) {
		return fmt.Errorf("%w: HMAC-SHA256 mismatch", domain.ErrInvalidSignature)
	}
	return nil
}

// ComputeSignature returns the lowercase hex HMAC-SHA256 of body under secret.
// Exported so tests and the webhook simulator can produce valid signatures.
func ComputeSignature(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyPaymentSignature validates a Razorpay Checkout callback, where the
// signed payload is "<order_id>|<payment_id>" and the key is the API key
// secret (not the webhook secret).
func VerifyPaymentSignature(orderID, paymentID, signature, keySecret string) error {
	if keySecret == "" {
		return errors.New("key secret is not configured")
	}
	payload := orderID + "|" + paymentID
	expected := ComputeSignature([]byte(payload), keySecret)
	if !hmac.Equal([]byte(strings.ToLower(signature)), []byte(expected)) {
		return fmt.Errorf("%w: payment signature mismatch", domain.ErrInvalidSignature)
	}
	return nil
}
