//go:build integration

package order_test

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/huy205-dev/ticketrush/internal/catalog"
	"github.com/huy205-dev/ticketrush/internal/events"
	"github.com/huy205-dev/ticketrush/internal/inventory"
	"github.com/huy205-dev/ticketrush/internal/order"
	"github.com/huy205-dev/ticketrush/internal/platform/redisx"
)

// shortHoldService shares e's storage but gives orders a hold of ttl. The
// long grace keeps the Redis key alive past the deadline, so the test sees
// the expirer release the seats rather than the key's own TTL.
func shortHoldService(e env, ttl time.Duration) *order.Service {
	logger := slog.New(slog.DiscardHandler)
	return order.NewService(e.pool, catalog.NewService(e.pool, e.inv), e.inv, redisx.NewLocker(e.rdb),
		order.Config{HoldTTL: ttl, HoldGrace: time.Minute}, logger)
}

func (e env) orderStatus(t *testing.T, id uuid.UUID) order.Status {
	t.Helper()
	var s string
	if err := e.pool.QueryRow(context.Background(), `SELECT status FROM orders WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return order.Status(s)
}

// SPEC.md 9.4: HELD orders past hold_expires_at become EXPIRED, their seats
// are released and order.expired is written in the same transaction.
func TestExpirer(t *testing.T) {
	forEachBackend(t, func(t *testing.T, e env) {
		ctx := context.Background()
		short := shortHoldService(e, 300*time.Millisecond)

		due, _, err := short.Create(ctx, 1, uuid.New(), e.req("VIP-A-1", "VIP-A-2"))
		if err != nil {
			t.Fatal(err)
		}
		live, _, err := e.svc.Create(ctx, 2, uuid.New(), e.req("VIP-A-3"))
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(400 * time.Millisecond)

		x := order.NewExpirer(e.pool, e.inv, slog.New(slog.DiscardHandler))
		n, err := x.RunOnce(ctx)
		if err != nil || n != 1 {
			t.Fatalf("RunOnce = %d, %v; want 1 order expired", n, err)
		}

		if s := e.orderStatus(t, due.ID); s != order.StatusExpired {
			t.Errorf("due order is %s, want EXPIRED", s)
		}
		if s := e.orderStatus(t, live.ID); s != order.StatusHeld {
			t.Errorf("order still in its hold is %s, want HELD", s)
		}
		st := e.status(t, "VIP-A-1", "VIP-A-2", "VIP-A-3")
		if st["VIP-A-1"] != inventory.Available || st["VIP-A-2"] != inventory.Available || st["VIP-A-3"] != inventory.Held {
			t.Errorf("seat status after expiry = %v", st)
		}

		msgs := e.outbox(t, due.ID)
		if len(msgs) != 2 || msgs[1].EventType != events.OrderExpired {
			t.Fatalf("outbox for the expired order = %+v, want held then expired", msgs)
		}
		p := msgs[1].Payload
		if p.EventID != msgs[1].ID || p.OrderID != due.ID || p.UserID != 1 || p.TotalVND != 7_000_000 ||
			!slices.Equal(p.SeatIDs, []string{"VIP-A-1", "VIP-A-2"}) {
			t.Errorf("order.expired payload = %+v", p)
		}

		if n, err := x.RunOnce(ctx); err != nil || n != 0 {
			t.Errorf("second RunOnce = %d, %v; want nothing left", n, err)
		}
		// The buyer's held-order slot and the seats are free again.
		if _, created, err := e.svc.Create(ctx, 1, uuid.New(), e.req("VIP-A-1")); err != nil || !created {
			t.Errorf("create after expiry = %v, %v", created, err)
		}
	})
}

// Several expiry workers at once must expire each order exactly once
// (FOR UPDATE SKIP LOCKED).
func TestExpirersInParallel(t *testing.T) {
	forEachBackend(t, func(t *testing.T, e env) {
		ctx := context.Background()
		short := shortHoldService(e, 200*time.Millisecond)
		const orders = 60
		for i := range orders {
			if _, _, err := short.Create(ctx, int64(100+i), uuid.New(), e.req(fmt.Sprintf("CAT2-A-%d", i+1))); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(300 * time.Millisecond)

		var total atomic.Int64
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range 4 {
			x := order.NewExpirer(e.pool, e.inv, slog.New(slog.DiscardHandler))
			wg.Go(func() {
				<-start
				for {
					n, err := x.RunOnce(ctx)
					if err != nil {
						t.Error(err)
						return
					}
					if n == 0 {
						return
					}
					total.Add(int64(n))
				}
			})
		}
		close(start)
		wg.Wait()

		var expiredRows, events int
		err := e.pool.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM orders WHERE status = 'EXPIRED'),
			(SELECT count(*) FROM outbox WHERE event_type = 'order.expired')`).Scan(&expiredRows, &events)
		if err != nil {
			t.Fatal(err)
		}
		if total.Load() != orders || expiredRows != orders || events != orders {
			t.Errorf("workers reported %d, database has %d EXPIRED orders and %d order.expired events; want %d each",
				total.Load(), expiredRows, events, orders)
		}
	})
}

// Run stops when its context is cancelled and expires orders meanwhile.
func TestExpirerRun(t *testing.T) {
	forEachBackend(t, func(t *testing.T, e env) {
		ctx, cancel := context.WithCancel(context.Background())
		o, _, err := shortHoldService(e, 100*time.Millisecond).Create(ctx, 1, uuid.New(), e.req("VIP-A-1"))
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			done <- order.NewExpirer(e.pool, e.inv, slog.New(slog.DiscardHandler)).Run(ctx, 50*time.Millisecond)
		}()

		deadline := time.Now().Add(5 * time.Second)
		for e.orderStatus(t, o.ID) != order.StatusExpired {
			if time.Now().After(deadline) {
				t.Fatal("order not expired by the running worker")
			}
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not stop after cancel")
		}
	})
}
