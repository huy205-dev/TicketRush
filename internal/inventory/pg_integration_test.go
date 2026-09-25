//go:build integration

package inventory_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/huy205-dev/ticketrush/internal/catalog"
	"github.com/huy205-dev/ticketrush/internal/inventory"
	"github.com/huy205-dev/ticketrush/internal/platform/testdb"
)

var db *testdb.DB

func TestMain(m *testing.M) { testdb.Main(m, &db) }

const holdTTL = 10 * time.Minute

func setup(t *testing.T) (*inventory.PG, *pgxpool.Pool, int64) {
	t.Helper()
	pool := db.New(t, 20)
	eventID, err := catalog.Seed(context.Background(), pool, catalog.DemoEvent())
	if err != nil {
		t.Fatal(err)
	}
	return inventory.NewPG(pool), pool, eventID
}

func newID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func holder(t *testing.T, pool *pgxpool.Pool, eventID int64, seat string) (uuid.UUID, bool) {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(),
		`SELECT order_id FROM seat_holds WHERE event_id = $1 AND seat_id = $2 AND expires_at > now()`,
		eventID, seat).Scan(&id)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

// SPEC.md 13.2: 1,000 goroutines hold the same seat, exactly one wins.
func TestPGHoldSameSeatConcurrently(t *testing.T) {
	inv, pool, eventID := setup(t)
	ctx := context.Background()

	const workers = 1000
	var (
		wins   atomic.Int64
		winner atomic.Value
		wg     sync.WaitGroup
		start  = make(chan struct{})
		errs   = make(chan error, workers)
	)
	for range workers {
		orderID := newID(t)
		wg.Go(func() {
			<-start
			taken, err := inv.Hold(ctx, eventID, []string{"VIP-A-1"}, orderID, holdTTL)
			switch {
			case err != nil:
				errs <- err
			case len(taken) == 0:
				wins.Add(1)
				winner.Store(orderID)
			case !slices.Equal(taken, []string{"VIP-A-1"}):
				errs <- fmt.Errorf("taken = %v, want [VIP-A-1]", taken)
			}
		})
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}
	if got := wins.Load(); got != 1 {
		t.Fatalf("%d goroutines held VIP-A-1, want exactly 1", got)
	}
	if id, ok := holder(t, pool, eventID, "VIP-A-1"); !ok || id != winner.Load().(uuid.UUID) {
		t.Errorf("seat_holds says %v holds VIP-A-1, want the winner %v", id, winner.Load())
	}
}

// SPEC.md 13.2: 200 goroutines each hold 2 random seats out of 20; no seat
// may end up in two successful holds.
func TestPGHoldOverlappingSeatsConcurrently(t *testing.T) {
	inv, pool, eventID := setup(t)
	ctx := context.Background()

	pool20 := make([]string, 20)
	for i := range pool20 {
		pool20[i] = fmt.Sprintf("VIP-A-%d", i+1)
	}

	const workers = 200
	type result struct {
		orderID uuid.UUID
		seats   []string
	}
	var (
		mu    sync.Mutex
		won   []result
		wg    sync.WaitGroup
		start = make(chan struct{})
	)
	for range workers {
		orderID := newID(t)
		perm := rand.Perm(len(pool20))
		seats := []string{pool20[perm[0]], pool20[perm[1]]}
		wg.Go(func() {
			<-start
			taken, err := inv.Hold(ctx, eventID, seats, orderID, holdTTL)
			if err != nil {
				t.Errorf("hold %v: %v", seats, err)
				return
			}
			if len(taken) == 0 {
				mu.Lock()
				won = append(won, result{orderID, seats})
				mu.Unlock()
			}
		})
	}
	close(start)
	wg.Wait()

	owner := map[string]uuid.UUID{}
	for _, r := range won {
		for _, s := range r.seats {
			if prev, dup := owner[s]; dup {
				t.Fatalf("seat %s held by both %v and %v", s, prev, r.orderID)
			}
			owner[s] = r.orderID
		}
	}
	if len(won) == 0 {
		t.Fatal("no goroutine managed to hold any seats")
	}
	for seat, want := range owner {
		if got, ok := holder(t, pool, eventID, seat); !ok || got != want {
			t.Errorf("seat_holds for %s = %v, want %v", seat, got, want)
		}
	}
	t.Logf("%d of %d goroutines won, %d seats held", len(won), workers, len(owner))
}

func TestPGHoldSemantics(t *testing.T) {
	inv, pool, eventID := setup(t)
	ctx := context.Background()
	a, b := newID(t), newID(t)

	mustHold := func(order uuid.UUID, seats ...string) {
		t.Helper()
		taken, err := inv.Hold(ctx, eventID, seats, order, holdTTL)
		if err != nil || len(taken) != 0 {
			t.Fatalf("Hold(%v) = %v, %v; want success", seats, taken, err)
		}
	}

	mustHold(a, "VIP-A-1", "VIP-A-2")

	t.Run("same order can hold again", func(t *testing.T) {
		mustHold(a, "VIP-A-1", "VIP-A-2")
	})

	t.Run("all or nothing", func(t *testing.T) {
		taken, err := inv.Hold(ctx, eventID, []string{"VIP-A-2", "VIP-A-3"}, b, holdTTL)
		if err != nil || !slices.Equal(taken, []string{"VIP-A-2"}) {
			t.Fatalf("Hold = %v, %v; want [VIP-A-2]", taken, err)
		}
		if _, held := holder(t, pool, eventID, "VIP-A-3"); held {
			t.Error("VIP-A-3 was held although the request failed")
		}
	})

	t.Run("release by another order is ignored", func(t *testing.T) {
		if err := inv.Release(ctx, eventID, []string{"VIP-A-1"}, b); err != nil {
			t.Fatal(err)
		}
		if id, _ := holder(t, pool, eventID, "VIP-A-1"); id != a {
			t.Errorf("VIP-A-1 holder = %v, want %v", id, a)
		}
	})

	t.Run("release by owner frees the seat", func(t *testing.T) {
		if err := inv.Release(ctx, eventID, []string{"VIP-A-1"}, a); err != nil {
			t.Fatal(err)
		}
		mustHold(b, "VIP-A-1")
	})

	t.Run("expired hold is free", func(t *testing.T) {
		c := newID(t)
		if taken, err := inv.Hold(ctx, eventID, []string{"VIP-B-1"}, c, time.Millisecond); err != nil || len(taken) != 0 {
			t.Fatalf("short hold: %v %v", taken, err)
		}
		time.Sleep(20 * time.Millisecond)
		mustHold(a, "VIP-B-1")
	})

	t.Run("unknown seat", func(t *testing.T) {
		_, err := inv.Hold(ctx, eventID, []string{"VIP-A-1", "NOPE-1"}, newID(t), holdTTL)
		if !errors.Is(err, inventory.ErrUnknownSeat) {
			t.Errorf("error = %v, want ErrUnknownSeat", err)
		}
	})
}

// insertTicket creates the order and ticket rows that payment will create in
// M4, so Status and Confirm can be exercised now.
func insertTicket(t *testing.T, pool *pgxpool.Pool, eventID int64, orderID uuid.UUID, seat string) {
	t.Helper()
	ctx := context.Background()
	_, err := pool.Exec(ctx, `INSERT INTO orders (id, event_id, user_id, status, total_vnd, idempotency_key, request_hash, hold_expires_at)
		VALUES ($1, $2, 1, 'PAID', 1, $3, 'x', now()) ON CONFLICT DO NOTHING`, orderID, eventID, orderID.String())
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO tickets (id, order_id, event_id, seat_id) VALUES ($1, $2, $3, $4)`,
		newID(t), orderID, eventID, seat)
	if err != nil {
		t.Fatal(err)
	}
}

func TestPGStatusAndConfirm(t *testing.T) {
	inv, pool, eventID := setup(t)
	ctx := context.Background()
	held, sold := newID(t), newID(t)

	if taken, err := inv.Hold(ctx, eventID, []string{"VIP-A-1"}, held, holdTTL); err != nil || len(taken) != 0 {
		t.Fatal(taken, err)
	}
	if taken, err := inv.Hold(ctx, eventID, []string{"VIP-A-2", "VIP-A-3"}, sold, holdTTL); err != nil || len(taken) != 0 {
		t.Fatal(taken, err)
	}

	// Confirm before the tickets exist must refuse.
	if err := inv.Confirm(ctx, eventID, []string{"VIP-A-2", "VIP-A-3"}, sold); !errors.Is(err, inventory.ErrConfirmMismatch) {
		t.Fatalf("Confirm without tickets = %v, want ErrConfirmMismatch", err)
	}
	insertTicket(t, pool, eventID, sold, "VIP-A-2")
	insertTicket(t, pool, eventID, sold, "VIP-A-3")
	if err := inv.Confirm(ctx, eventID, []string{"VIP-A-2", "VIP-A-3"}, sold); err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	got, err := inv.Status(ctx, eventID, []string{"VIP-A-1", "VIP-A-2", "VIP-A-3", "VIP-A-4"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]inventory.SeatStatus{
		"VIP-A-1": inventory.Held,
		"VIP-A-2": inventory.Sold,
		"VIP-A-3": inventory.Sold,
		"VIP-A-4": inventory.Available,
	}
	for seat, w := range want {
		if got[seat] != w {
			t.Errorf("Status[%s] = %s, want %s", seat, got[seat], w)
		}
	}

	// A sold seat stays taken even for the order that bought it.
	if taken, err := inv.Hold(ctx, eventID, []string{"VIP-A-2"}, sold, holdTTL); err != nil || len(taken) != 1 {
		t.Errorf("Hold on sold seat = %v, %v; want it taken", taken, err)
	}
}
