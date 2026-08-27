package razorpay

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ledgerflow/ledgerflow/internal/domain"
)

const testSecret = "whsec_test_1a2b3c4d5e6f"

// rawBody is a webhook payload with the awkward properties real ones have:
// unicode, an escaped sequence, and key ordering that no JSON library will
// reproduce byte-for-byte.
var rawBody = []byte(`{"event":"payment.failed","account_id":"acc_1","payload":{"payment":{"entity":{"id":"pay_Nz1","amount":45000,"currency":"INR","status":"failed","error_code":"BAD_REQUEST_ERROR","error_description":"Issuer declined – insufficient funds","notes":{"note":"line1\nline2"}}}}}`)

// TestVerifyWebhookSignatureAcceptsAValidSignature is the happy path for
// SRS 12.6: HMAC-SHA256 over the raw body under the webhook secret.
func TestVerifyWebhookSignatureAcceptsAValidSignature(t *testing.T) {
	sig := ComputeSignature(rawBody, testSecret)
	if err := VerifyWebhookSignature(rawBody, sig, testSecret); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	// Razorpay sends lowercase hex; accepting the uppercase spelling as well costs
	// nothing and avoids a spurious rejection if a proxy upcases the header.
	if err := VerifyWebhookSignature(rawBody, strings.ToUpper(sig), testSecret); err != nil {
		t.Errorf("uppercase hex signature rejected: %v", err)
	}
}

// TestVerifyWebhookSignatureRejectsTampering is the security property. Any change
// to the body, however small, must invalidate the signature — otherwise an
// attacker could rewrite the amount on a captured webhook and have the system
// treat it as a verified gateway fact.
func TestVerifyWebhookSignatureRejectsTampering(t *testing.T) {
	sig := ComputeSignature(rawBody, testSecret)

	tampered := [][]byte{
		bytes.Replace(rawBody, []byte(`"amount":45000`), []byte(`"amount":45001`), 1),
		bytes.Replace(rawBody, []byte(`"status":"failed"`), []byte(`"status":"captured"`), 1),
		bytes.Replace(rawBody, []byte(`pay_Nz1`), []byte(`pay_Nz2`), 1),
		append(append([]byte{}, rawBody...), ' '), // trailing whitespace
		append([]byte{' '}, rawBody...),           // leading whitespace
		rawBody[:len(rawBody)-1],                  // truncated
		[]byte{},                                  // empty
	}
	for i, body := range tampered {
		err := VerifyWebhookSignature(body, sig, testSecret)
		if err == nil {
			t.Errorf("tamper case %d accepted a modified body", i)
			continue
		}
		if !errors.Is(err, domain.ErrInvalidSignature) {
			t.Errorf("tamper case %d returned %v, want ErrInvalidSignature so the handler answers 400", i, err)
		}
	}
}

// TestVerifyWebhookSignatureRejectsWrongSecret covers key rotation and the
// misconfiguration where the API key secret is supplied instead of the webhook
// secret — the two are different values and mixing them up would otherwise
// present as "all webhooks are forged".
func TestVerifyWebhookSignatureRejectsWrongSecret(t *testing.T) {
	sig := ComputeSignature(rawBody, testSecret)
	for _, secret := range []string{"whsec_test_1a2b3c4d5e6g", "WHSEC_TEST_1A2B3C4D5E6F", "rzp_test_keysecret"} {
		if err := VerifyWebhookSignature(rawBody, sig, secret); !errors.Is(err, domain.ErrInvalidSignature) {
			t.Errorf("secret %q returned %v, want ErrInvalidSignature", secret, err)
		}
	}
}

// TestVerifyWebhookSignatureRejectsMissingInputs checks the two ways this can be
// called wrong. A missing header is a client error; an unconfigured secret is a
// server misconfiguration, and it must never be treated as "nothing to verify
// against, so allow it" — that would fail open on every webhook (SRS 19.3).
func TestVerifyWebhookSignatureRejectsMissingInputs(t *testing.T) {
	if err := VerifyWebhookSignature(rawBody, "", testSecret); !errors.Is(err, domain.ErrInvalidSignature) {
		t.Errorf("missing signature header returned %v, want ErrInvalidSignature", err)
	}

	err := VerifyWebhookSignature(rawBody, ComputeSignature(rawBody, ""), "")
	if err == nil {
		t.Fatal("an unconfigured webhook secret verified successfully: the endpoint would fail open")
	}
	if errors.Is(err, domain.ErrInvalidSignature) {
		t.Error("an unconfigured secret reported ErrInvalidSignature, which reads as a forged request; " +
			"it is a server misconfiguration and must be distinguishable so it answers 500 rather than 400")
	}
}

// TestSignatureMustBeVerifiedOverTheRawBody is SRS 19.3 stated as a test.
//
// Verifying after a parse/reserialise round trip is the single most likely way to
// break webhook verification, and it fails in the worst possible manner: it works
// on simple payloads and rejects exactly the ones with unicode, escapes or
// unusual key order. This asserts the reserialised bytes differ and that they do
// not verify, so the handler has to keep reading the body before parsing it.
func TestSignatureMustBeVerifiedOverTheRawBody(t *testing.T) {
	sig := ComputeSignature(rawBody, testSecret)

	var parsed map[string]any
	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
	reserialised, err := json.Marshal(parsed)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Equal(rawBody, reserialised) {
		t.Skip("this JSON round-trips byte-identically, so it cannot demonstrate the hazard")
	}
	if err := VerifyWebhookSignature(reserialised, sig, testSecret); err == nil {
		t.Error("a reserialised body verified against the original signature; the HMAC is not over the raw bytes")
	}
}

// TestComputeSignatureIsDeterministicAndLowercaseHex pins the output format,
// since the demo webhook simulator signs requests with this function and Razorpay
// compares against lowercase hex.
func TestComputeSignatureIsDeterministicAndLowercaseHex(t *testing.T) {
	first := ComputeSignature(rawBody, testSecret)
	if len(first) != 64 {
		t.Errorf("signature length %d, want 64 hex chars for SHA-256", len(first))
	}
	if first != strings.ToLower(first) {
		t.Errorf("signature %q is not lowercase hex", first)
	}
	for i := 0; i < 50; i++ {
		if got := ComputeSignature(rawBody, testSecret); got != first {
			t.Fatalf("signature changed between calls")
		}
	}
}

// TestVerifyPaymentSignature covers the Checkout callback, which signs
// "<order_id>|<payment_id>" under the API key secret rather than the webhook
// secret. The demo checkout page depends on this to confirm a recovered payment.
func TestVerifyPaymentSignature(t *testing.T) {
	const keySecret = "rzp_test_keysecret"
	const order, payment = "order_Nz1", "pay_Nz1"
	sig := ComputeSignature([]byte(order+"|"+payment), keySecret)

	if err := VerifyPaymentSignature(order, payment, sig, keySecret); err != nil {
		t.Fatalf("valid payment signature rejected: %v", err)
	}

	// The two ids must not be interchangeable: signing them in the wrong order
	// would let a callback for one payment be replayed as another.
	if err := VerifyPaymentSignature(payment, order, sig, keySecret); !errors.Is(err, domain.ErrInvalidSignature) {
		t.Errorf("swapped order/payment ids returned %v, want ErrInvalidSignature", err)
	}
	for _, tc := range []struct{ name, order, payment, sig, secret string }{
		{"wrong order id", "order_Nz2", payment, sig, keySecret},
		{"wrong payment id", order, "pay_Nz2", sig, keySecret},
		{"empty signature", order, payment, "", keySecret},
		{"garbage signature", order, payment, "deadbeef", keySecret},
		{"wrong secret", order, payment, sig, "rzp_test_othersecret"},
	} {
		if err := VerifyPaymentSignature(tc.order, tc.payment, tc.sig, tc.secret); !errors.Is(err, domain.ErrInvalidSignature) {
			t.Errorf("%s: returned %v, want ErrInvalidSignature", tc.name, err)
		}
	}

	if err := VerifyPaymentSignature(order, payment, sig, ""); err == nil {
		t.Error("an unconfigured key secret verified a payment callback")
	}
}
