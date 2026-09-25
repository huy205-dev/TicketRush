//go:build integration

package e2e_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/huy205-dev/ticketrush/internal/catalog"
	"github.com/huy205-dev/ticketrush/internal/events"
	"github.com/huy205-dev/ticketrush/internal/inventory"
	"github.com/huy205-dev/ticketrush/internal/order"
	"github.com/huy205-dev/ticketrush/internal/outbox"
	"github.com/huy205-dev/ticketrush/internal/platform/kafka"
	"github.com/huy205-dev/ticketrush/internal/platform/redisx"
	"github.com/huy205-dev/ticketrush/internal/platform/testenv"
)

var containers testenv.Env

func TestMain(m *testing.M) {
	testenv.Main(m, testenv.Needs{Postgres: true, Redis: true, Kafka: true}, &containers)
}

// SPEC.md M3: an order created with HOLD_TTL=2s becomes EXPIRED, its seats
// are free again, and order.expired reaches Kafka through the outbox relay.
// The expiry worker and the relay run as they do in production: in their
// own loops, concurrently.
func TestHeldOrderExpiresAndIsPublished(t *testing.T) {
	for _, backend := range []string{"redis", "pg"} {
		t.Run(backend, func(t *testing.T) { testExpiryFlow(t, backend) })
	}
}

func testExpiryFlow(t *testing.T, backend string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.DiscardHandler)

	pool := containers.DB.New(t, 20)
	rdb := containers.Redis.New(t)
	eventID, err := catalog.Seed(ctx, pool, catalog.DemoEvent())
	if err != nil {
		t.Fatal(err)
	}
	inv, err := inventory.New(backend, pool, rdb, logger)
	if err != nil {
		t.Fatal(err)
	}
	svc := order.NewService(pool, catalog.NewService(pool, inv), inv, redisx.NewLocker(rdb),
		order.Config{HoldTTL: 2 * time.Second, HoldGrace: 30 * time.Second}, logger)

	producer := containers.Kafka.Client(t)
	if err := kafka.EnsureTopics(ctx, producer, events.TopicPartitions()); err != nil {
		t.Fatal(err)
	}
	relayDone := make(chan error, 1)
	go func() { relayDone <- outbox.NewRelay(pool, producer, logger).Run(ctx) }()
	expiryDone := make(chan error, 1)
	go func() { expiryDone <- order.NewExpirer(pool, inv, logger).Run(ctx, 200*time.Millisecond) }()

	seats := []string{"VIP-A-7", "VIP-A-8"}
	o, _, err := svc.Create(ctx, 42, uuid.New(), order.CreateRequest{EventID: eventID, SeatIDs: seats})
	if err != nil {
		t.Fatal(err)
	}
	created := time.Now()

	// Wait for the worker to expire the order: not before 2s, soon after.
	var status string
	deadline := time.Now().Add(10 * time.Second)
	for status != string(order.StatusExpired) {
		if time.Now().After(deadline) {
			t.Fatalf("order still %s after 10s", status)
		}
		time.Sleep(50 * time.Millisecond)
		if err := pool.QueryRow(ctx, `SELECT status FROM orders WHERE id = $1`, o.ID).Scan(&status); err != nil {
			t.Fatal(err)
		}
	}
	if waited := time.Since(created); waited < 2*time.Second {
		t.Errorf("expired after %v, before the 2s hold ended", waited)
	}
	t.Logf("expired %v after creation", time.Since(created).Round(10*time.Millisecond))

	st, err := inv.Status(ctx, eventID, seats)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range seats {
		if st[s] != inventory.Available {
			t.Errorf("%s is %s after expiry, want AVAILABLE", s, st[s])
		}
	}

	// Both events of the order arrive on orders.v1, in order, keyed by it.
	records := consumeOrder(t, o.ID, 2)
	types := []string{header(records[0], events.HeaderEventType), header(records[1], events.HeaderEventType)}
	if !slices.Equal(types, []string{events.OrderHeld, events.OrderExpired}) {
		t.Fatalf("events for the order = %v, want [order.held order.expired]", types)
	}
	var ev events.OrderEvent
	if err := json.Unmarshal(records[1].Value, &ev); err != nil {
		t.Fatal(err)
	}
	if strconv.FormatInt(ev.EventID, 10) != header(records[1], events.HeaderEventID) ||
		ev.EventType != events.OrderExpired || ev.OrderID != o.ID || ev.Event != eventID ||
		ev.UserID != 42 || ev.TotalVND != 7_000_000 || !slices.Equal(ev.SeatIDs, seats) {
		t.Errorf("order.expired payload = %+v", ev)
	}

	var unpublished int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE published_at IS NULL`).Scan(&unpublished); err != nil {
		t.Fatal(err)
	}
	if unpublished != 0 {
		t.Errorf("%d outbox rows left unpublished", unpublished)
	}

	cancel()
	for name, done := range map[string]chan error{"relay": relayDone, "expiry": expiryDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("%s stopped with %v", name, err)
			}
		case <-time.After(10 * time.Second):
			t.Errorf("%s did not stop", name)
		}
	}
}

// consumeOrder returns the first n records keyed by orderID on orders.v1.
// Other tests publish to the same topic, so records are filtered by key.
func consumeOrder(t *testing.T, orderID uuid.UUID, n int) []*kgo.Record {
	t.Helper()
	cl := containers.Kafka.Client(t,
		kgo.ConsumeTopics(events.TopicOrders),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var got []*kgo.Record
	for len(got) < n {
		fetches := cl.PollFetches(ctx)
		if ctx.Err() != nil {
			t.Fatalf("got %d of %d records for order %v before timeout", len(got), n, orderID)
		}
		fetches.EachRecord(func(r *kgo.Record) {
			if string(r.Key) == orderID.String() {
				got = append(got, r)
			}
		})
	}
	return got
}

func header(r *kgo.Record, key string) string {
	for _, h := range r.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}
