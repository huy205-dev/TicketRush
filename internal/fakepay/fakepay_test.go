package fakepay

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/huy205-dev/ticketrush/internal/payment"
)

const secret = "wwwwwwwwwwwwwwwwwwwwwwwwwwwwwwww"

// booking records the webhooks it receives and answers with statuses from
// a script (200 once the script runs out).
type booking struct {
	mu       sync.Mutex
	received []payment.Webhook
	at       []time.Time
	script   []int
	badSigs  atomic.Int64
	srv      *httptest.Server
}

func newBooking(t *testing.T, script ...int) *booking {
	b := &booking{script: script}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !payment.Verify(secret, body, r.Header.Get(payment.SignatureHeader)) {
			b.badSigs.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var wh payment.Webhook
		_ = json.Unmarshal(body, &wh)
		b.mu.Lock()
		b.received = append(b.received, wh)
		b.at = append(b.at, time.Now())
		status := http.StatusOK
		if len(b.script) > 0 {
			status, b.script = b.script[0], b.script[1:]
		}
		b.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *booking) webhooks() []payment.Webhook {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]payment.Webhook(nil), b.received...)
}

type gateway struct {
	s   *Server
	srv *httptest.Server
}

func newGateway(t *testing.T, cfg Config) gateway {
	t.Helper()
	cfg.WebhookSecret = secret
	cfg.RetryBase = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	s := New(ctx, cfg, slog.New(slog.DiscardHandler))
	r := chi.NewRouter()
	s.Routes(r)
	srv := httptest.NewServer(r)
	s.cfg.PublicURL = srv.URL
	t.Cleanup(func() { srv.Close(); cancel(); s.Wait() })
	return gateway{s: s, srv: srv}
}

func (g gateway) post(t *testing.T, path string, body any) (int, []byte) {
	t.Helper()
	raw, _ := json.Marshal(body)
	res, err := http.Post(g.srv.URL+path, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}

func (g gateway) create(t *testing.T, b *booking, amount int64) (uuid.UUID, string) {
	t.Helper()
	id := uuid.New()
	status, body := g.post(t, "/v1/payments", payment.CreatePaymentRequest{OrderID: id, AmountVND: amount, CallbackURL: b.srv.URL})
	if status != 200 {
		t.Fatalf("create = %d %s", status, body)
	}
	var resp payment.CreatePaymentResponse
	_ = json.Unmarshal(body, &resp)
	return id, resp.PaymentURL
}

func (g gateway) complete(t *testing.T, id uuid.UUID, result string) string {
	t.Helper()
	status, body := g.post(t, "/pay/"+id.String()+"/complete", map[string]string{"result": result})
	if status != 200 {
		t.Fatalf("complete = %d %s", status, body)
	}
	var resp completeResponse
	_ = json.Unmarshal(body, &resp)
	return resp.ProviderTxnID
}

// waitFor polls until cond holds or fails the test after 5s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPaymentPageAndSignedWebhook(t *testing.T) {
	b := newBooking(t)
	g := newGateway(t, Config{})
	id, url := g.create(t, b, 7_000_000)

	if want := g.srv.URL + "/pay/" + id.String(); url != want {
		t.Errorf("payment_url = %s, want %s", url, want)
	}

	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	html, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(html), "7.000.000 đ") || !strings.Contains(string(html), "Thanh toán thành công") {
		t.Errorf("payment page = %d:\n%s", res.StatusCode, html)
	}

	txn := g.complete(t, id, payment.StatusSucceeded)
	waitFor(t, "the webhook", func() bool { return len(b.webhooks()) == 1 })
	wh := b.webhooks()[0]
	if wh.ProviderTxnID != txn || wh.OrderID != id || wh.AmountVND != 7_000_000 || wh.Status != payment.StatusSucceeded || wh.PaidAt.IsZero() {
		t.Errorf("webhook = %+v", wh)
	}
	if b.badSigs.Load() != 0 {
		t.Error("webhook signature did not verify")
	}

	// Paying again is a second, separate transaction.
	if txn2 := g.complete(t, id, payment.StatusFailed); txn2 == txn {
		t.Error("second payment reused the transaction id")
	}
}

func TestRegisteringTwiceKeepsTheFirstPayment(t *testing.T) {
	b := newBooking(t)
	g := newGateway(t, Config{})
	id := uuid.New()
	for _, amount := range []int64{100, 999} {
		if status, _ := g.post(t, "/v1/payments", payment.CreatePaymentRequest{OrderID: id, AmountVND: amount, CallbackURL: b.srv.URL}); status != 200 {
			t.Fatal(status)
		}
	}
	g.complete(t, id, payment.StatusSucceeded)
	waitFor(t, "the webhook", func() bool { return len(b.webhooks()) == 1 })
	if got := b.webhooks()[0].AmountVND; got != 100 {
		t.Errorf("amount = %d, want the first registration's 100", got)
	}
}

func TestDuplicatesAndRetries(t *testing.T) {
	t.Run("duplicate rate 1 sends 2 to 4 copies", func(t *testing.T) {
		b := newBooking(t)
		g := newGateway(t, Config{DuplicateRate: 1})
		id, _ := g.create(t, b, 1)
		txn := g.complete(t, id, payment.StatusSucceeded)
		g.s.wg.Wait()
		got := b.webhooks()
		if len(got) < 2 || len(got) > 4 {
			t.Fatalf("received %d copies, want 2-4", len(got))
		}
		for _, wh := range got {
			if wh.ProviderTxnID != txn {
				t.Errorf("copy for %s, want %s", wh.ProviderTxnID, txn)
			}
		}
	})

	t.Run("retries until booking answers 200", func(t *testing.T) {
		b := newBooking(t, 500, 503)
		g := newGateway(t, Config{})
		id, _ := g.create(t, b, 1)
		g.complete(t, id, payment.StatusSucceeded)
		g.s.wg.Wait()
		if n := len(b.webhooks()); n != 3 {
			t.Errorf("attempts = %d, want 3 (two failures then success)", n)
		}
	})

	t.Run("5xx rate 1 resends until the attempts run out", func(t *testing.T) {
		b := newBooking(t)
		g := newGateway(t, Config{FailRate: 1, MaxAttempts: 4})
		id, _ := g.create(t, b, 1)
		g.complete(t, id, payment.StatusSucceeded)
		g.s.wg.Wait()
		if n := len(b.webhooks()); n != 4 {
			t.Errorf("attempts = %d, want 4", n)
		}
	})

	t.Run("late payments wait the extra delay", func(t *testing.T) {
		b := newBooking(t)
		g := newGateway(t, Config{LateRate: 1, LateExtra: 80 * time.Millisecond})
		id, _ := g.create(t, b, 1)
		start := time.Now()
		g.complete(t, id, payment.StatusSucceeded)
		g.s.wg.Wait()
		if len(b.at) != 1 || b.at[0].Sub(start) < 80*time.Millisecond {
			t.Errorf("late webhook arrived after %v, want at least 80ms", b.at[0].Sub(start))
		}
	})
}

func TestAutoPayment(t *testing.T) {
	b := newBooking(t)
	g := newGateway(t, Config{})
	id := uuid.New()
	status, body := g.post(t, "/v1/payments/auto", map[string]any{"order_id": id, "amount_vnd": 5, "callback_url": b.srv.URL})
	if status != 200 || !strings.Contains(string(body), "fp_") {
		t.Fatalf("auto = %d %s", status, body)
	}
	g.s.wg.Wait()
	if got := b.webhooks(); len(got) != 1 || got[0].Status != payment.StatusSucceeded || got[0].OrderID != id {
		t.Errorf("webhooks = %+v", got)
	}
}

func TestRefundsAreIdempotent(t *testing.T) {
	g := newGateway(t, Config{})
	req := payment.RefundRequest{OrderID: uuid.New(), ProviderTxnID: "fp_x", AmountVND: 10}
	for range 3 {
		if status, body := g.post(t, "/v1/refunds", req); status != 200 || !strings.Contains(string(body), "SUCCEEDED") {
			t.Fatalf("refund = %d %s", status, body)
		}
	}
	if g.s.Refunds() != 1 {
		t.Errorf("refunds recorded = %d, want 1", g.s.Refunds())
	}
	if status, _ := g.post(t, "/v1/refunds", payment.RefundRequest{OrderID: uuid.New()}); status != 400 {
		t.Errorf("invalid refund = %d, want 400", status)
	}
}

func TestValidation(t *testing.T) {
	g := newGateway(t, Config{})
	for name, body := range map[string]any{
		"no order":     map[string]any{"amount_vnd": 1, "callback_url": "http://x"},
		"zero amount":  map[string]any{"order_id": uuid.New(), "amount_vnd": 0, "callback_url": "http://x"},
		"relative url": map[string]any{"order_id": uuid.New(), "amount_vnd": 1, "callback_url": "/hook"},
	} {
		if status, _ := g.post(t, "/v1/payments", body); status != 400 {
			t.Errorf("%s: status %d, want 400", name, status)
		}
	}
	if status, _ := g.post(t, "/pay/"+uuid.NewString()+"/complete", map[string]string{"result": "SUCCEEDED"}); status != 404 {
		t.Errorf("complete unknown order = %d, want 404", status)
	}
	res, _ := http.Get(g.srv.URL + "/pay/" + uuid.NewString())
	if res.StatusCode != 404 {
		t.Errorf("page for unknown order = %d, want 404", res.StatusCode)
	}
}
