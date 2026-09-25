// Package payment holds what booking and fakepay share about payments: the
// webhook payload and its HMAC signature, and the HTTP client booking and
// the refunder use to talk to fakepay.
package payment

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// SignatureHeader carries hex(hmac_sha256(WEBHOOK_SECRET, raw_body)).
const SignatureHeader = "X-Signature"

// Payment statuses reported by the webhook.
const (
	StatusSucceeded = "SUCCEEDED"
	StatusFailed    = "FAILED"
)

// Webhook is the body fakepay posts to booking (SPEC.md 7.1).
type Webhook struct {
	ProviderTxnID string    `json:"provider_txn_id"`
	OrderID       uuid.UUID `json:"order_id"`
	AmountVND     int64     `json:"amount_vnd"`
	Status        string    `json:"status"`
	PaidAt        time.Time `json:"paid_at"`
}

// ErrInvalidWebhook means a correctly signed webhook has an unusable body.
var ErrInvalidWebhook = errors.New("invalid webhook payload")

// ErrBadSignature means X-Signature does not match the body.
var ErrBadSignature = errors.New("webhook signature does not match")

// Validate checks the fields booking relies on.
func (w Webhook) Validate() error {
	switch {
	case w.ProviderTxnID == "" || len(w.ProviderTxnID) > 128:
		return fmt.Errorf("%w: provider_txn_id must be 1-128 characters", ErrInvalidWebhook)
	case w.OrderID == uuid.Nil:
		return fmt.Errorf("%w: order_id is required", ErrInvalidWebhook)
	case w.AmountVND < 0:
		return fmt.Errorf("%w: amount_vnd must not be negative", ErrInvalidWebhook)
	case w.Status != StatusSucceeded && w.Status != StatusFailed:
		return fmt.Errorf("%w: status must be SUCCEEDED or FAILED", ErrInvalidWebhook)
	}
	return nil
}

// Sign returns the signature of body.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// Verify reports whether signature is the signature of body. The comparison
// takes constant time.
func Verify(secret string, body []byte, signature string) bool {
	got, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}
