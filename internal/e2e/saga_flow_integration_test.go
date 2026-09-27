//go:build integration

package e2e_test

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/huy205-dev/ticketrush/internal/catalog"
	"github.com/huy205-dev/ticketrush/internal/events"
	"github.com/huy205-dev/ticketrush/internal/fakepay"
	"github.com/huy205-dev/ticketrush/internal/inventory"
	"github.com/huy205-dev/ticketrush/internal/notifier"
	"github.com/huy205-dev/ticketrush/internal/order"
	"github.com/huy205-dev/ticketrush/internal/outbox"
	"github.com/huy205-dev/ticketrush/internal/payment"
	"github.com/huy205-dev/ticketrush/internal/platform/kafka"
	"github.com/huy205-dev/ticketrush/internal/platform/redisx"
)

// recorder is a notifier.Sender that keeps the messages.
type recorder struct {
	mu   sync.Mutex
	sent []string
}

func (r *recorder) Send(_ context.Context, text string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, text)
	return nil
}

func (r *recorder) messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sent...)
}

// SPEC.md M4, the saga end to end over Kafka: a paid order is ticketed by
// the ticket consumer, a payment after cancelling and a second payment are
// refunded through fakepay by the refunder, and the notifier announces the
// tickets and both refunds. Relay and consumers run in their own loops, as
// in production.
func TestPaymentSagaOverKafka(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.DiscardHandler)

	pool := containers.DB.New(t, 20)
	rdb := containers.Redis.New(t)
	eventID, err := catalog.Seed(ctx, pool, catalog.DemoEvent())
	if err != nil {
		t.Fatal(err)
	}
	inv, err := inventory.New("redis", pool, rdb, logger)
	if err != nil {
		t.Fatal(err)
	}
	svc := order.NewService(pool, catalog.NewService(pool, inv), inv, redisx.NewLocker(rdb),
		order.Config{HoldTTL: 10 * time.Minute, HoldGrace: 30 * time.Second}, logger)

	// fakepay only serves refunds here; payments are applied directly.
	gwCtx, gwCancel := context.WithCancel(context.Background())
	defer gwCancel()
	gw := fakepay.New(gwCtx, fakepay.Config{WebhookSecret: strings.Repeat("w", 32)}, logger)
	router := chi.NewRouter()
	gw.Routes(router)
	fp := httptest.NewServer(router)
	defer fp.Close()

	producer := containers.Kafka.Client(t)
	if err := kafka.EnsureTopics(ctx, producer, events.TopicPartitions()); err != nil {
		t.Fatal(err)
	}

	sent := &recorder{}
	// Group names are unique per run: the topics are shared with other tests
	// in this binary, whose records name orders this database does not have
	// and are skipped as permanent failures.
	suffix := "-" + uuid.NewString()[:8]
	brokers := containers.Kafka.Brokers
	workers := map[string]func(context.Context) error{
		"relay": outbox.NewRelay(pool, producer, logger).Run,
		"ticket": func(ctx context.Context) error {
			h := order.NewTicketIssuer(pool, strings.Repeat("t", 32), logger).Handler()
			return kafka.Run(ctx, logger, brokers, "ticket"+suffix, []string{events.TopicOrders}, h)
		},
		"refunder": func(ctx context.Context) error {
			h := order.NewRefundProcessor(pool, payment.NewClient(fp.URL, 5*time.Second), logger).Handler()
			return kafka.Run(ctx, logger, brokers, "refunder"+suffix, []string{events.TopicOrders}, h)
		},
		"notifier": func(ctx context.Context) error {
			h := notifier.New(sent, notifier.NewRedisSeen(rdb), logger).Handler()
			return kafka.Run(ctx, logger, brokers, "notifier"+suffix, []string{events.TopicOrders, events.TopicTickets}, h)
		},
	}
	done := make(map[string]chan error, len(workers))
	for name, run := range workers {
		ch := make(chan error, 1)
		done[name] = ch
		go func() { ch <- run(ctx) }()
	}

	status := func(id uuid.UUID) order.Status {
		var s string
		if err := pool.QueryRow(context.Background(), `SELECT status FROM orders WHERE id = $1`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return order.Status(s)
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	pay := func(o *order.Order) payment.Webhook {
		t.Helper()
		wh := payment.Webhook{ProviderTxnID: "fp_" + uuid.NewString(), OrderID: o.ID, AmountVND: o.TotalVND,
			Status: payment.StatusSucceeded, PaidAt: time.Now().UTC()}
		if _, err := svc.ApplyPayment(ctx, wh); err != nil {
			t.Fatal(err)
		}
		return wh
	}

	// Happy path: paid, then ticketed by the consumer.
	bought, _, err := svc.Create(ctx, 1, uuid.New(), order.CreateRequest{EventID: eventID, SeatIDs: []string{"VIP-A-1", "VIP-A-2"}})
	if err != nil {
		t.Fatal(err)
	}
	pay(bought)
	waitFor("the order to be TICKETED", func() bool { return status(bought.ID) == order.StatusTicketed })
	tickets, err := svc.Tickets(ctx, 1, bought.ID)
	if err != nil || len(tickets) != 2 || tickets[0].QRPayload == "" || tickets[1].QRPayload == "" {
		t.Fatalf("tickets = %+v, %v", tickets, err)
	}

	// Compensation: paid after cancelling, refunded.
	cancelled, _, err := svc.Create(ctx, 2, uuid.New(), order.CreateRequest{EventID: eventID, SeatIDs: []string{"VIP-A-3"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Cancel(ctx, 2, cancelled.ID); err != nil {
		t.Fatal(err)
	}
	pay(cancelled)
	waitFor("the cancelled order to be REFUNDED", func() bool { return status(cancelled.ID) == order.StatusRefunded })

	// Paying the ticketed order again: refunded, the order stays TICKETED.
	again := pay(bought)
	waitFor("the double payment's refund", func() bool {
		var s string
		_ = pool.QueryRow(context.Background(), `SELECT status FROM refunds WHERE provider_txn_id = $1`, again.ProviderTxnID).Scan(&s)
		return s == order.RefundSucceeded
	})
	if s := status(bought.ID); s != order.StatusTicketed {
		t.Errorf("order is %s after its double payment was refunded, want TICKETED", s)
	}
	if n := gw.Refunds(); n != 2 {
		t.Errorf("fakepay refunded %d payments, want 2", n)
	}

	waitFor("three notifications", func() bool { return len(sent.messages()) >= 3 })
	var issued, refunded int
	for _, m := range sent.messages() {
		switch {
		case strings.HasPrefix(m, "Đã xuất 2 vé cho đơn "+bought.ID.String()):
			issued++
		case strings.HasPrefix(m, "Đã hoàn "):
			refunded++
		default:
			t.Errorf("unexpected notification %q", m)
		}
	}
	if issued != 1 || refunded != 2 {
		t.Errorf("notifications = %q, want one for the tickets and two refunds", sent.messages())
	}

	cancel()
	for name, ch := range done {
		select {
		case err := <-ch:
			if err != nil {
				t.Errorf("%s stopped with %v", name, err)
			}
		case <-time.After(15 * time.Second):
			t.Fatalf("%s did not stop", name)
		}
	}
}
