package payment

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestSignAndVerify(t *testing.T) {
	secret := strings.Repeat("s", 32)
	body := []byte(`{"provider_txn_id":"fp_1","amount_vnd":7000000}`)
	sig := Sign(secret, body)

	if !Verify(secret, body, sig) {
		t.Fatal("valid signature rejected")
	}
	if !Verify(secret, body, strings.ToUpper(sig)) {
		t.Error("hex case must not matter")
	}
	for name, tc := range map[string]struct {
		secret, sig string
		body        []byte
	}{
		"other secret":    {strings.Repeat("x", 32), sig, body},
		"tampered body":   {secret, sig, []byte(`{"provider_txn_id":"fp_1","amount_vnd":1}`)},
		"truncated sig":   {secret, sig[:len(sig)-2], body},
		"not hex":         {secret, "zz" + sig[2:], body},
		"empty signature": {secret, "", body},
	} {
		if Verify(tc.secret, tc.body, tc.sig) {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestWebhookValidate(t *testing.T) {
	ok := Webhook{ProviderTxnID: "fp_1", OrderID: uuid.New(), AmountVND: 1, Status: StatusSucceeded}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid webhook: %v", err)
	}
	bad := map[string]func(w *Webhook){
		"no txn":       func(w *Webhook) { w.ProviderTxnID = "" },
		"long txn":     func(w *Webhook) { w.ProviderTxnID = strings.Repeat("a", 129) },
		"no order":     func(w *Webhook) { w.OrderID = uuid.Nil },
		"negative":     func(w *Webhook) { w.AmountVND = -1 },
		"odd status":   func(w *Webhook) { w.Status = "PENDING" },
		"empty status": func(w *Webhook) { w.Status = "" },
	}
	for name, mutate := range bad {
		w := ok
		mutate(&w)
		if err := w.Validate(); !errors.Is(err, ErrInvalidWebhook) {
			t.Errorf("%s: error = %v, want ErrInvalidWebhook", name, err)
		}
	}
}
