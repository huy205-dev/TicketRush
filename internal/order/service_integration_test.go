//go:build integration

package order_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/huy205-dev/ticketrush/internal/catalog"
	"github.com/huy205-dev/ticketrush/internal/events"
	"github.com/huy205-dev/ticketrush/internal/inventory"
	"github.com/huy205-dev/ticketrush/internal/order"
	"github.com/huy205-dev/ticketrush/internal/order/orderdb"
	"github.com/huy205-dev/ticketrush/internal/platform/testdb"
)

var db *testdb.DB

func TestMain(m *testing.M) { testdb.Main(m, &db) }

const (
	holdTTL   = 10 * time.Minute
	holdGrace = 30 * time.Second
)

type env struct {
	svc     *order.Service
	pool    *pgxpool.Pool
	inv     inventory.Inventory
	eventID int64
}

func setupWith(t *testing.T, spec catalog.EventSpec) env {
	t.Helper()
	pool := db.New(t, 20)
	eventID, err := catalog.Seed(context.Background(), pool, spec)
	if err != nil {
		t.Fatal(err)
	}
	inv := inventory.NewPG(pool)
	cat := catalog.NewService(pool, inv)
	svc := order.NewService(pool, cat, inv, holdTTL, holdGrace, slog.New(slog.DiscardHandler))
	return env{svc: svc, pool: pool, inv: inv, eventID: eventID}
}

func setup(t *testing.T) env { return setupWith(t, catalog.DemoEvent()) }

func (e env) req(seats ...string) order.CreateRequest {
	return order.CreateRequest{EventID: e.eventID, SeatIDs: seats}
}

func (e env) status(t *testing.T, seats ...string) map[string]inventory.SeatStatus {
	t.Helper()
	st, err := e.inv.Status(context.Background(), e.eventID, seats)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

type outboxRow struct {
	ID        int64
	Topic     string
	Key       string
	EventType string
	Payload   events.OrderEvent
}

func (e env) outbox(t *testing.T, orderID uuid.UUID) []outboxRow {
	t.Helper()
	rows, err := e.pool.Query(context.Background(),
		`SELECT id, topic, msg_key, event_type, payload FROM outbox WHERE msg_key = $1 ORDER BY id`, orderID.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []outboxRow
	for rows.Next() {
		var r outboxRow
		var payload []byte
		if err := rows.Scan(&r.ID, &r.Topic, &r.Key, &r.EventType, &payload); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(payload, &r.Payload); err != nil {
			t.Fatalf("outbox payload: %v: %s", err, payload)
		}
		out = append(out, r)
	}
	return out
}

func (e env) countOrders(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM orders`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCreateHeldOrder(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	key := uuid.New()

	before := time.Now()
	o, created, err := e.svc.Create(ctx, 7, key, e.req("VIP-A-2", "VIP-A-1"))
	if err != nil || !created {
		t.Fatalf("Create = %v, %v", created, err)
	}

	if o.Status != order.StatusHeld || o.UserID != 7 || o.EventID != e.eventID {
		t.Errorf("order = %+v", o)
	}
	if o.TotalVND != 7_000_000 {
		t.Errorf("TotalVND = %d, want 7000000", o.TotalVND)
	}
	if !slices.Equal(o.SeatIDs(), []string{"VIP-A-1", "VIP-A-2"}) {
		t.Errorf("seats = %v, want sorted [VIP-A-1 VIP-A-2]", o.SeatIDs())
	}
	// hold_expires_at comes from the database clock: roughly now + HOLD_TTL.
	if d := o.HoldExpiresAt.Sub(before); d < holdTTL-time.Minute || d > holdTTL+time.Minute {
		t.Errorf("hold expires in %v, want about %v", d, holdTTL)
	}
	for seat, st := range e.status(t, "VIP-A-1", "VIP-A-2") {
		if st != inventory.Held {
			t.Errorf("%s is %s, want HELD", seat, st)
		}
	}

	msgs := e.outbox(t, o.ID)
	if len(msgs) != 1 {
		t.Fatalf("got %d outbox rows, want 1", len(msgs))
	}
	m := msgs[0]
	if m.Topic != events.TopicOrders || m.EventType != events.OrderHeld || m.Key != o.ID.String() {
		t.Errorf("outbox row = %s/%s/%s", m.Topic, m.EventType, m.Key)
	}
	p := m.Payload
	if p.EventID != m.ID || p.EventType != events.OrderHeld || p.OrderID != o.ID || p.Event != e.eventID ||
		p.UserID != 7 || p.TotalVND != 7_000_000 || !slices.Equal(p.SeatIDs, o.SeatIDs()) || p.OccurredAt.IsZero() {
		t.Errorf("payload = %+v", p)
	}
}

func TestCreateIdempotency(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	key := uuid.New()

	first, created, err := e.svc.Create(ctx, 7, key, e.req("CAT1-A-1", "CAT1-A-2"))
	if err != nil || !created {
		t.Fatalf("first Create = %v, %v", created, err)
	}

	t.Run("same body returns the same order", func(t *testing.T) {
		again, created, err := e.svc.Create(ctx, 7, key, e.req("CAT1-A-2", "CAT1-A-1")) // seat order differs
		if err != nil || created || again.ID != first.ID {
			t.Fatalf("replay = %v, %v, %v; want existing order %v", again.ID, created, err, first.ID)
		}
	})

	t.Run("different body is rejected", func(t *testing.T) {
		_, _, err := e.svc.Create(ctx, 7, key, e.req("CAT1-A-3"))
		if !errors.Is(err, order.ErrIdempotencyKeyReused) {
			t.Fatalf("error = %v, want ErrIdempotencyKeyReused", err)
		}
	})

	t.Run("key is scoped per user", func(t *testing.T) {
		other, created, err := e.svc.Create(ctx, 8, key, e.req("CAT1-A-3"))
		if err != nil || !created || other.ID == first.ID {
			t.Fatalf("other user with same key = %v, %v", created, err)
		}
	})

	if n := len(e.outbox(t, first.ID)); n != 1 {
		t.Errorf("replays wrote %d outbox rows for the order, want 1", n)
	}
}

// SPEC.md 13.2: the same request sent 10 times in parallel creates one order,
// and once the winner has finished, a retry with the key returns that order.
func TestCreateSameKeyInParallel(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	key := uuid.New()

	const workers = 10
	var (
		mu       sync.Mutex
		winners  []uuid.UUID
		replays  []uuid.UUID
		inFlight atomic.Int64
		wg       sync.WaitGroup
		start    = make(chan struct{})
	)
	for range workers {
		wg.Go(func() {
			<-start
			o, isNew, err := e.svc.Create(ctx, 7, key, e.req("VIP-C-1", "VIP-C-2"))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil && isNew:
				winners = append(winners, o.ID)
			case err == nil:
				replays = append(replays, o.ID)
			case errors.Is(err, order.ErrSeatsUnavailable):
				// Reached the seats while the winner was still creating the
				// order. ADR-005: from M2 this becomes 409
				// IDEMPOTENCY_KEY_IN_PROGRESS instead.
				inFlight.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
	close(start)
	wg.Wait()

	if len(winners) != 1 {
		t.Fatalf("%d requests created an order, want exactly 1", len(winners))
	}
	winner := winners[0]
	for _, id := range replays {
		if id != winner {
			t.Errorf("a replay returned order %v, want the winner %v", id, winner)
		}
	}
	if n := e.countOrders(t); n != 1 {
		t.Errorf("%d orders in the database, want 1", n)
	}
	t.Logf("created=1 replayed=%d in_flight_conflicts=%d", len(replays), inFlight.Load())

	// The winner has finished: every retry with the key is a replay of it.
	for range 3 {
		o, isNew, err := e.svc.Create(ctx, 7, key, e.req("VIP-C-2", "VIP-C-1"))
		if err != nil || isNew || o.ID != winner {
			t.Fatalf("retry after the race = %v, new=%v, %v; want replay of %v", o, isNew, err, winner)
		}
	}
}

// SPEC.md M1: 1,000 buyers race for VIP-A-1 through the full create flow.
func TestCreateSameSeatConcurrently(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	const workers = 1000
	var (
		wins, conflicts atomic.Int64
		wg              sync.WaitGroup
		start           = make(chan struct{})
	)
	for i := range workers {
		userID := int64(1000 + i)
		wg.Go(func() {
			<-start
			_, _, err := e.svc.Create(ctx, userID, uuid.New(), e.req("VIP-A-1"))
			var su *order.SeatsUnavailableError
			switch {
			case err == nil:
				wins.Add(1)
			case errors.As(err, &su) && slices.Equal(su.SeatIDs, []string{"VIP-A-1"}):
				conflicts.Add(1)
			default:
				t.Errorf("user %d: %v", userID, err)
			}
		})
	}
	close(start)
	wg.Wait()

	if wins.Load() != 1 || conflicts.Load() != workers-1 {
		t.Fatalf("wins=%d conflicts=%d, want 1 and %d", wins.Load(), conflicts.Load(), workers-1)
	}
	var orders, seats int
	err := e.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM orders), (SELECT count(*) FROM order_seats WHERE seat_id = 'VIP-A-1')`).Scan(&orders, &seats)
	if err != nil {
		t.Fatal(err)
	}
	if orders != 1 || seats != 1 {
		t.Errorf("database has %d orders and %d order_seats for VIP-A-1, want 1 and 1", orders, seats)
	}
}

func TestCreateRejections(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	if _, _, err := e.svc.Create(ctx, 7, uuid.New(), e.req("VIP-A-1", "VIP-A-2")); err != nil {
		t.Fatal(err)
	}

	t.Run("seats held by someone else", func(t *testing.T) {
		_, _, err := e.svc.Create(ctx, 8, uuid.New(), e.req("VIP-A-2", "VIP-A-3"))
		var su *order.SeatsUnavailableError
		if !errors.As(err, &su) || !slices.Equal(su.SeatIDs, []string{"VIP-A-2"}) {
			t.Fatalf("error = %v, want SeatsUnavailable [VIP-A-2]", err)
		}
		if st := e.status(t, "VIP-A-3"); st["VIP-A-3"] != inventory.Available {
			t.Errorf("VIP-A-3 is %s after the failed request, want AVAILABLE", st["VIP-A-3"])
		}
	})

	t.Run("one held order per user and event", func(t *testing.T) {
		_, _, err := e.svc.Create(ctx, 7, uuid.New(), e.req("CAT2-A-1"))
		if !errors.Is(err, order.ErrActiveOrderExists) {
			t.Fatalf("error = %v, want ErrActiveOrderExists", err)
		}
		if st := e.status(t, "CAT2-A-1"); st["CAT2-A-1"] != inventory.Available {
			t.Errorf("CAT2-A-1 is %s, want it released", st["CAT2-A-1"])
		}
	})

	t.Run("unknown event", func(t *testing.T) {
		_, _, err := e.svc.Create(ctx, 7, uuid.New(), order.CreateRequest{EventID: 999, SeatIDs: []string{"VIP-A-1"}})
		var ve *order.ValidationError
		if !errors.As(err, &ve) || ve.Field != "event_id" {
			t.Fatalf("error = %v, want validation error on event_id", err)
		}
	})

	t.Run("invalid seats", func(t *testing.T) {
		_, _, err := e.svc.Create(ctx, 9, uuid.New(), e.req("VIP-A-1", "VIP-A-2", "VIP-A-3", "VIP-A-4", "VIP-A-5"))
		if !errors.Is(err, order.ErrValidation) {
			t.Fatalf("error = %v, want ErrValidation", err)
		}
	})
}

func TestCreateBeforeSaleOpens(t *testing.T) {
	spec := catalog.DemoEvent()
	spec.SaleOpensIn = time.Hour
	e := setupWith(t, spec)

	_, _, err := e.svc.Create(context.Background(), 7, uuid.New(), e.req("VIP-A-1"))
	var ve *order.ValidationError
	if !errors.As(err, &ve) || ve.Field != "event_id" {
		t.Fatalf("error = %v, want validation error: sale not open", err)
	}
	if st := e.status(t, "VIP-A-1"); st["VIP-A-1"] != inventory.Available {
		t.Errorf("seat was held before the sale opened")
	}
}

func TestGet(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	o, _, err := e.svc.Create(ctx, 7, uuid.New(), e.req("VIP-A-1"))
	if err != nil {
		t.Fatal(err)
	}

	got, err := e.svc.Get(ctx, 7, o.ID)
	if err != nil || got.ID != o.ID || !slices.Equal(got.SeatIDs(), []string{"VIP-A-1"}) || got.TotalVND != 3_500_000 {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	if _, err := e.svc.Get(ctx, 8, o.ID); !errors.Is(err, order.ErrNotFound) {
		t.Errorf("other user's Get error = %v, want ErrNotFound", err)
	}
	if _, err := e.svc.Get(ctx, 7, uuid.New()); !errors.Is(err, order.ErrNotFound) {
		t.Errorf("unknown order error = %v, want ErrNotFound", err)
	}
}

func TestCancel(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	o, _, err := e.svc.Create(ctx, 7, uuid.New(), e.req("VIP-A-1", "VIP-A-2"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := e.svc.Cancel(ctx, 8, o.ID); !errors.Is(err, order.ErrNotFound) {
		t.Fatalf("cancel by another user = %v, want ErrNotFound", err)
	}

	c, err := e.svc.Cancel(ctx, 7, o.ID)
	if err != nil || c.Status != order.StatusCancelled {
		t.Fatalf("Cancel = %+v, %v", c, err)
	}
	for seat, st := range e.status(t, "VIP-A-1", "VIP-A-2") {
		if st != inventory.Available {
			t.Errorf("%s is %s after cancel, want AVAILABLE", seat, st)
		}
	}

	again, err := e.svc.Cancel(ctx, 7, o.ID)
	if err != nil || again.Status != order.StatusCancelled {
		t.Fatalf("second Cancel = %+v, %v; want idempotent success", again, err)
	}

	msgs := e.outbox(t, o.ID)
	if len(msgs) != 2 || msgs[0].EventType != events.OrderHeld || msgs[1].EventType != events.OrderCancelled {
		t.Errorf("outbox for order = %+v, want held then cancelled once", msgs)
	}

	// The held-order slot is free again.
	if _, _, err := e.svc.Create(ctx, 7, uuid.New(), e.req("VIP-A-1")); err != nil {
		t.Errorf("create after cancel: %v", err)
	}
}

func TestCancelRejectsOtherStatuses(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	o, _, err := e.svc.Create(ctx, 7, uuid.New(), e.req("VIP-A-1"))
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the expiry worker (M3).
	if _, err := orderdb.New(e.pool).TransitionOrder(ctx, orderdb.TransitionOrderParams{
		OrderID: o.ID, FromStatus: string(order.StatusHeld), ToStatus: string(order.StatusExpired),
	}); err != nil {
		t.Fatal(err)
	}

	_, err = e.svc.Cancel(ctx, 7, o.ID)
	var nc *order.NotCancellableError
	if !errors.As(err, &nc) || nc.Status != order.StatusExpired {
		t.Fatalf("Cancel of expired order = %v, want NotCancellable(EXPIRED)", err)
	}
}
