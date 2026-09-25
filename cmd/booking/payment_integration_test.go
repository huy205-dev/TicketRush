//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/huy205-dev/ticketrush/internal/fakepay"
	"github.com/huy205-dev/ticketrush/internal/httpx"
	"github.com/huy205-dev/ticketrush/internal/order"
	"github.com/huy205-dev/ticketrush/internal/payment"
	"github.com/huy205-dev/ticketrush/internal/ticket"
)

var testTicketSecret = strings.Repeat("t", 32)

func (c *client) createOrder(eventID int64, seats ...string) orderResponse {
	c.t.Helper()
	ids, _ := json.Marshal(seats)
	r := c.do("POST", "/v1/orders", `{"event_id":`+jsonInt(eventID)+`,"seat_ids":`+string(ids)+`}`,
		map[string]string{"Idempotency-Key": uuid.NewString()})
	if r.status != 201 {
		c.t.Fatalf("create = %d %s", r.status, r.body)
	}
	return decode[orderResponse](c.t, r)
}

func (c *client) signedWebhook(body []byte, sig string) response {
	c.t.Helper()
	return c.do("POST", "/v1/webhooks/fakepay", string(body), map[string]string{payment.SignatureHeader: sig})
}

// SPEC.md 7.1: 401 for a wrong signature, 200 for anything correctly signed
// and recorded (duplicates and unknown orders included), 400 for a signed
// body that makes no sense.
func TestWebhookOverHTTP(t *testing.T) {
	s := startFull(t, "redis", nil)
	s.login(5)
	o := s.createOrder(s.eventID, "VIP-A-1")
	wh, _ := json.Marshal(payment.Webhook{
		ProviderTxnID: "fp_http_1", OrderID: o.OrderID, AmountVND: 3_500_000,
		Status: payment.StatusSucceeded, PaidAt: time.Now().UTC(),
	})
	good := payment.Sign(testWebhookSecret, wh)
	s.token = "" // fakepay does not log in

	errCode := func(r response) string { return decode[struct{ Error httpx.ErrorBody }](t, r).Error.Code }
	for name, sig := range map[string]string{
		"no signature":         "",
		"not hex":              "zz",
		"other secret":         payment.Sign(strings.Repeat("x", 32), wh),
		"signature of another": payment.Sign(testWebhookSecret, bytes.Replace(wh, []byte("3500000"), []byte("1"), 1)),
	} {
		if r := s.signedWebhook(wh, sig); r.status != 401 || errCode(r) != "INVALID_SIGNATURE" {
			t.Errorf("%s: %d %s, want 401 INVALID_SIGNATURE", name, r.status, r.body)
		}
	}
	if st := s.orderStatus(t, o.OrderID); st != order.StatusHeld {
		t.Fatalf("a rejected webhook changed the order to %s", st)
	}

	for _, body := range []string{`{"provider_txn_id":`, `{"provider_txn_id":"fp_x","order_id":"` + o.OrderID.String() + `","amount_vnd":1,"status":"MAYBE"}`} {
		if r := s.signedWebhook([]byte(body), payment.Sign(testWebhookSecret, []byte(body))); r.status != 400 {
			t.Errorf("signed malformed body %s: %d, want 400", body, r.status)
		}
	}

	for i, want := range []order.Outcome{order.OutcomeApplied, order.OutcomeDuplicate, order.OutcomeDuplicate} {
		r := s.signedWebhook(wh, good)
		if got := decode[webhookResponse](t, r).Outcome; r.status != 200 || got != want {
			t.Errorf("delivery %d: %d %s, want 200 %s", i+1, r.status, r.body, want)
		}
	}
	if st := s.orderStatus(t, o.OrderID); st != order.StatusPaid {
		t.Errorf("order is %s, want PAID", st)
	}

	unknown, _ := json.Marshal(payment.Webhook{ProviderTxnID: "fp_http_2", OrderID: uuid.New(), AmountVND: 1, Status: payment.StatusSucceeded})
	if r := s.signedWebhook(unknown, payment.Sign(testWebhookSecret, unknown)); r.status != 200 ||
		decode[webhookResponse](t, r).Outcome != order.OutcomeUnknownOrder {
		t.Errorf("unknown order: %d %s, want 200 UNKNOWN_ORDER", r.status, r.body)
	}
}

func (s server) orderStatus(t *testing.T, id uuid.UUID) order.Status {
	t.Helper()
	var st string
	if err := s.pool.QueryRow(context.Background(), `SELECT status FROM orders WHERE id = $1`, id).Scan(&st); err != nil {
		t.Fatal(err)
	}
	return order.Status(st)
}

// A buyer pays on fakepay's page while fakepay duplicates the webhook and
// resends deliveries that succeeded: the order is paid once, and after the
// ticket service ran the buyer gets QR codes.
func TestPayThroughFakepay(t *testing.T) {
	for _, backend := range []string{"pg", "redis"} {
		t.Run(backend, func(t *testing.T) {
			s := startFull(t, backend, &fakepay.Config{
				DelayMax: 20 * time.Millisecond, DuplicateRate: 1, FailRate: 0.5, RetryBase: 10 * time.Millisecond,
			})
			s.login(8)
			o := s.createOrder(s.eventID, "VIP-A-1", "VIP-A-2")
			if !strings.HasSuffix(o.PaymentURL, "/pay/"+o.OrderID.String()) {
				t.Fatalf("payment_url = %q", o.PaymentURL)
			}

			page, err := http.Get(o.PaymentURL)
			if err != nil {
				t.Fatal(err)
			}
			html, _ := io.ReadAll(page.Body)
			_ = page.Body.Close()
			if page.StatusCode != 200 || !strings.Contains(string(html), "7.000.000 đ") {
				t.Fatalf("payment page = %d:\n%s", page.StatusCode, html)
			}
			res, err := http.Post(o.PaymentURL+"/complete", "application/json", strings.NewReader(`{"result":"SUCCEEDED"}`))
			if err != nil || res.StatusCode != 200 {
				t.Fatalf("complete = %v, %v", res, err)
			}
			_ = res.Body.Close()

			s.fakepay.Wait() // every copy and every resend delivered
			path := "/v1/orders/" + o.OrderID.String()
			if got := decode[orderResponse](t, s.do("GET", path, "", nil)); got.Status != order.StatusPaid || got.PaymentURL != "" {
				t.Fatalf("order after payment = %+v, want PAID without a payment link", got)
			}
			var payments, paidEvents int
			_ = s.pool.QueryRow(context.Background(), `SELECT count(*) FROM payments WHERE order_id = $1`, o.OrderID).Scan(&payments)
			_ = s.pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox WHERE msg_key = $1 AND event_type = 'order.paid'`,
				o.OrderID.String()).Scan(&paidEvents)
			if payments != 1 || paidEvents != 1 {
				t.Errorf("%d payments and %d order.paid events, want 1 and 1", payments, paidEvents)
			}

			// Before the ticket service runs there is nothing to show.
			if got := decode[ticketsResponse](t, s.do("GET", path+"/tickets", "", nil)); len(got.Tickets) != 0 {
				t.Errorf("tickets before issuing = %+v", got.Tickets)
			}
			issuer := order.NewTicketIssuer(s.pool, testTicketSecret, slog.New(slog.DiscardHandler))
			if _, err := issuer.Issue(context.Background(), o.OrderID); err != nil {
				t.Fatal(err)
			}

			r := s.do("GET", path+"/tickets", "", nil)
			got := decode[ticketsResponse](t, r)
			if r.status != 200 || len(got.Tickets) != 2 {
				t.Fatalf("tickets = %d %s", r.status, r.body)
			}
			for _, tk := range got.Tickets {
				if id, ok := ticket.Verify(testTicketSecret, tk.QRPayload); !ok || id != tk.TicketID {
					t.Errorf("ticket %s: QR payload does not verify", tk.TicketID)
				}
				raw, err := base64.StdEncoding.DecodeString(tk.QRPNGBase64)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := png.Decode(bytes.NewReader(raw)); err != nil {
					t.Errorf("ticket %s: QR is not a PNG: %v", tk.TicketID, err)
				}
			}
			view := decode[orderResponse](t, s.do("GET", path, "", nil))
			if view.Status != order.StatusTicketed || len(view.Tickets) != 2 || view.Tickets[0].QRPNGBase64 != "" ||
				view.Tickets[0].QRPayload != got.Tickets[0].QRPayload {
				t.Errorf("order view = %+v, want TICKETED with the tickets and no images", view)
			}

			other := &client{t: t, base: s.base}
			other.login(9)
			if r := other.do("GET", path+"/tickets", "", nil); r.status != 404 {
				t.Errorf("another user's tickets = %d, want 404", r.status)
			}
		})
	}
}
