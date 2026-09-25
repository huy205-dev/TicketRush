//go:build integration

package inventory_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/huy205-dev/ticketrush/internal/catalog"
	"github.com/huy205-dev/ticketrush/internal/inventory"
	"github.com/huy205-dev/ticketrush/internal/platform/testenv"
)

var containers testenv.Env

func TestMain(m *testing.M) {
	testenv.Main(m, testenv.Needs{Postgres: true, Redis: true}, &containers)
}

const holdTTL = 10 * time.Minute

// backend is one Inventory implementation plus a way to see who holds a
// seat in its own storage, independent of the code under test.
type backend struct {
	name   string
	inv    inventory.Inventory
	holder func(t *testing.T, seat string) (uuid.UUID, bool)
}

type fixture struct {
	pool    *pgxpool.Pool
	rdb     *redis.Client
	eventID int64
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	pool := containers.DB.New(t, 20)
	eventID, err := catalog.Seed(context.Background(), pool, catalog.DemoEvent())
	if err != nil {
		t.Fatal(err)
	}
	return fixture{pool: pool, rdb: containers.Redis.New(t), eventID: eventID}
}

func (f fixture) pg() backend {
	return backend{name: "pg", inv: inventory.NewPG(f.pool), holder: func(t *testing.T, seat string) (uuid.UUID, bool) {
		t.Helper()
		var id uuid.UUID
		err := f.pool.QueryRow(context.Background(),
			`SELECT order_id FROM seat_holds WHERE event_id = $1 AND seat_id = $2 AND expires_at > now()`,
			f.eventID, seat).Scan(&id)
		return id, err == nil
	}}
}

func (f fixture) redis() backend {
	return backend{name: "redis", inv: inventory.NewRedis(f.rdb, slog.New(slog.DiscardHandler)), holder: func(t *testing.T, seat string) (uuid.UUID, bool) {
		t.Helper()
		v, err := f.rdb.Get(context.Background(), fmt.Sprintf("seat:{%d}:%s", f.eventID, seat)).Result()
		if err != nil {
			return uuid.Nil, false
		}
		raw, ok := strings.CutPrefix(v, "held:")
		if !ok {
			return uuid.Nil, false
		}
		id, err := uuid.Parse(raw)
		return id, err == nil
	}}
}

// forEachBackend runs the same contract test against both backends, each on
// fresh storage.
func forEachBackend(t *testing.T, test func(t *testing.T, f fixture, b backend)) {
	for _, name := range []string{"pg", "redis"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			b := f.pg()
			if name == "redis" {
				b = f.redis()
			}
			test(t, f, b)
		})
	}
}

func newID(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// SPEC.md 13.2: 1,000 goroutines hold the same seat, exactly one wins.
// Run for both backends.
func TestHoldSameSeatConcurrently(t *testing.T) {
	forEachBackend(t, func(t *testing.T, f fixture, b backend) {
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
				taken, err := b.inv.Hold(ctx, f.eventID, []string{"VIP-A-1"}, orderID, holdTTL)
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
		if id, ok := b.holder(t, "VIP-A-1"); !ok || id != winner.Load().(uuid.UUID) {
			t.Errorf("storage says %v holds VIP-A-1, want the winner %v", id, winner.Load())
		}
	})
}

// SPEC.md 13.2: 200 goroutines each hold 2 random seats out of 20; no seat
// may end up in two successful holds. Run for both backends.
func TestHoldOverlappingSeatsConcurrently(t *testing.T) {
	forEachBackend(t, func(t *testing.T, f fixture, b backend) {
		ctx := context.Background()
		seats20 := make([]string, 20)
		for i := range seats20 {
			seats20[i] = fmt.Sprintf("VIP-A-%d", i+1)
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
			perm := rand.Perm(len(seats20))
			seats := []string{seats20[perm[0]], seats20[perm[1]]}
			wg.Go(func() {
				<-start
				taken, err := b.inv.Hold(ctx, f.eventID, seats, orderID, holdTTL)
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
			if got, ok := b.holder(t, seat); !ok || got != want {
				t.Errorf("storage holder of %s = %v, want %v", seat, got, want)
			}
		}
		t.Logf("%d of %d goroutines won, %d seats held", len(won), workers, len(owner))
	})
}

// The behaviour every backend must share: hold, hold again with the same
// order, hold a taken seat, release by a non-owner, release, expiry, status.
func TestHoldContract(t *testing.T) {
	forEachBackend(t, func(t *testing.T, f fixture, b backend) {
		ctx := context.Background()
		a, other := newID(t), newID(t)

		mustHold := func(order uuid.UUID, seats ...string) {
			t.Helper()
			taken, err := b.inv.Hold(ctx, f.eventID, seats, order, holdTTL)
			if err != nil || len(taken) != 0 {
				t.Fatalf("Hold(%v) = %v, %v; want success", seats, taken, err)
			}
		}

		mustHold(a, "VIP-A-1", "VIP-A-2")

		t.Run("same order can hold again", func(t *testing.T) {
			mustHold(a, "VIP-A-1", "VIP-A-2")
		})

		t.Run("taken seat is reported and nothing is held", func(t *testing.T) {
			taken, err := b.inv.Hold(ctx, f.eventID, []string{"VIP-A-2", "VIP-A-3"}, other, holdTTL)
			if err != nil || !slices.Equal(taken, []string{"VIP-A-2"}) {
				t.Fatalf("Hold = %v, %v; want [VIP-A-2]", taken, err)
			}
			if _, held := b.holder(t, "VIP-A-3"); held {
				t.Error("VIP-A-3 was held although the request failed")
			}
		})

		t.Run("status", func(t *testing.T) {
			st, err := b.inv.Status(ctx, f.eventID, []string{"VIP-A-1", "VIP-A-3"})
			if err != nil || st["VIP-A-1"] != inventory.Held || st["VIP-A-3"] != inventory.Available {
				t.Fatalf("Status = %v, %v", st, err)
			}
		})

		t.Run("release by another order is ignored", func(t *testing.T) {
			if err := b.inv.Release(ctx, f.eventID, []string{"VIP-A-1"}, other); err != nil {
				t.Fatal(err)
			}
			if id, _ := b.holder(t, "VIP-A-1"); id != a {
				t.Errorf("VIP-A-1 holder = %v, want %v", id, a)
			}
		})

		t.Run("release by owner frees the seat", func(t *testing.T) {
			if err := b.inv.Release(ctx, f.eventID, []string{"VIP-A-1"}, a); err != nil {
				t.Fatal(err)
			}
			mustHold(other, "VIP-A-1")
		})

		t.Run("expired hold is free", func(t *testing.T) {
			c := newID(t)
			if taken, err := b.inv.Hold(ctx, f.eventID, []string{"VIP-B-1"}, c, time.Millisecond); err != nil || len(taken) != 0 {
				t.Fatalf("short hold: %v %v", taken, err)
			}
			time.Sleep(20 * time.Millisecond)
			mustHold(a, "VIP-B-1")
		})
	})
}

// ---- pg only ----------------------------------------------------------------

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
	f := newFixture(t)
	inv := f.pg().inv
	ctx := context.Background()
	held, sold := newID(t), newID(t)

	if taken, err := inv.Hold(ctx, f.eventID, []string{"VIP-A-1"}, held, holdTTL); err != nil || len(taken) != 0 {
		t.Fatal(taken, err)
	}
	if taken, err := inv.Hold(ctx, f.eventID, []string{"VIP-A-2", "VIP-A-3"}, sold, holdTTL); err != nil || len(taken) != 0 {
		t.Fatal(taken, err)
	}

	// Confirm before the tickets exist must refuse.
	if err := inv.Confirm(ctx, f.eventID, []string{"VIP-A-2", "VIP-A-3"}, sold); !errors.Is(err, inventory.ErrConfirmMismatch) {
		t.Fatalf("Confirm without tickets = %v, want ErrConfirmMismatch", err)
	}
	insertTicket(t, f.pool, f.eventID, sold, "VIP-A-2")
	insertTicket(t, f.pool, f.eventID, sold, "VIP-A-3")
	if err := inv.Confirm(ctx, f.eventID, []string{"VIP-A-2", "VIP-A-3"}, sold); err != nil {
		t.Fatalf("Confirm: %v", err)
	}

	got, err := inv.Status(ctx, f.eventID, []string{"VIP-A-1", "VIP-A-2", "VIP-A-3", "VIP-A-4"})
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
	if taken, err := inv.Hold(ctx, f.eventID, []string{"VIP-A-2"}, sold, holdTTL); err != nil || len(taken) != 1 {
		t.Errorf("Hold on sold seat = %v, %v; want it taken", taken, err)
	}
}

func TestPGUnknownSeat(t *testing.T) {
	f := newFixture(t)
	_, err := f.pg().inv.Hold(context.Background(), f.eventID, []string{"VIP-A-1", "NOPE-1"}, newID(t), holdTTL)
	if !errors.Is(err, inventory.ErrUnknownSeat) {
		t.Errorf("error = %v, want ErrUnknownSeat", err)
	}
}
