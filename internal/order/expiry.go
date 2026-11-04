package order

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/huy205-dev/ticketrush/internal/events"
	"github.com/huy205-dev/ticketrush/internal/inventory"
	"github.com/huy205-dev/ticketrush/internal/order/orderdb"
	"github.com/huy205-dev/ticketrush/internal/platform/logging"
)

// ExpiryBatchSize is how many orders one expiry transaction handles
// (SPEC.md 9.4).
const ExpiryBatchSize = 500

// batchTimeout bounds one expiry batch. Batches run detached from the
// shutdown signal so SIGTERM never aborts one halfway.
const batchTimeout = 30 * time.Second

// Expirer moves HELD orders past their deadline to EXPIRED and releases
// their seats (SPEC.md 9.4). Several expirers can run at once: each batch
// locks its orders with SKIP LOCKED.
type Expirer struct {
	pool   *pgxpool.Pool
	inv    inventory.Inventory
	logger *slog.Logger
}

// NewExpirer returns an expirer releasing seats through inv.
func NewExpirer(pool *pgxpool.Pool, inv inventory.Inventory, logger *slog.Logger) *Expirer {
	return &Expirer{pool: pool, inv: inv, logger: logger}
}

// RunOnce expires one batch and returns how many orders it expired. The
// status change and the order.expired outbox rows commit together; seats
// are released after the commit, once PostgreSQL has made the decision
// (see releaseExpired).
func (x *Expirer) RunOnce(ctx context.Context) (int, error) {
	if !CanTransition(StatusHeld, StatusExpired) {
		return 0, fmt.Errorf("state machine forbids %s -> %s", StatusHeld, StatusExpired)
	}

	var expired []*Order
	err := pgx.BeginFunc(ctx, x.pool, func(tx pgx.Tx) error {
		q := orderdb.New(tx)
		rows, err := q.ExpireDueOrders(ctx, ExpiryBatchSize)
		if err != nil {
			return fmt.Errorf("expire orders: %w", err)
		}
		if len(rows) == 0 {
			return nil
		}

		ids := make([]uuid.UUID, len(rows))
		byID := make(map[uuid.UUID]*Order, len(rows))
		for i, r := range rows {
			ids[i] = r.ID
			o := &Order{ID: r.ID, EventID: r.EventID, UserID: r.UserID, Status: StatusExpired, TotalVND: r.TotalVnd}
			byID[r.ID] = o
			expired = append(expired, o)
		}
		seats, err := q.ListSeatsOfOrders(ctx, ids)
		if err != nil {
			return fmt.Errorf("list seats of expired orders: %w", err)
		}
		for _, st := range seats {
			o := byID[st.OrderID]
			o.Seats = append(o.Seats, Seat{SeatID: st.SeatID, PriceVND: st.PriceVnd})
		}

		for _, o := range expired {
			if err := writeOrderEvent(ctx, tx, events.OrderExpired, o); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}

	if len(expired) > 0 {
		if err := x.releaseExpired(ctx, expired); err != nil {
			// The holds run out on their own shortly: HOLD_GRACE in Redis,
			// expires_at in seat_holds.
			x.logger.WarnContext(ctx, "release seats of expired orders failed; they free up when the holds run out",
				"err", err, "orders", len(expired))
		}
	}
	return len(expired), nil
}

// releaseExpired gives back the seats of orders the batch has just expired.
// It runs after the expiry commit, as SPEC.md 9.4 says, but first locks the
// orders again and skips those a late payment has meanwhile turned PAID.
//
// Without the lock the release races with ApplyPayment's late branch: that
// branch re-holds an expired order's seats under the same owner value while
// it holds the order lock, so an unlocked release could delete the new hold
// and let another buyer take a seat PostgreSQL is about to ticket (ADR-003).
func (x *Expirer) releaseExpired(ctx context.Context, expired []*Order) error {
	ids := make([]uuid.UUID, len(expired))
	for i, o := range expired {
		ids[i] = o.ID
	}
	return pgx.BeginFunc(ctx, x.pool, func(tx pgx.Tx) error {
		rows, err := orderdb.New(tx).LockOrderStatuses(ctx, ids)
		if err != nil {
			return fmt.Errorf("lock expired orders: %w", err)
		}
		status := make(map[uuid.UUID]Status, len(rows))
		for _, r := range rows {
			status[r.ID] = Status(r.Status)
		}
		for _, o := range expired {
			ctx := logging.With(ctx, slog.String("order_id", o.ID.String()))
			if st := status[o.ID]; st == StatusPaid || st == StatusTicketed {
				x.logger.InfoContext(ctx, "late payment kept the seats of an expired order", "status", st)
				continue
			}
			releaseSeats(ctx, x.inv, x.logger, o.EventID, o.SeatIDs(), o.ID)
			x.logger.DebugContext(ctx, "order expired", "seat_ids", o.SeatIDs())
		}
		return nil
	})
}

// Run expires orders every interval until ctx is cancelled. A full batch
// means more may be waiting, so the next one starts right away.
func (x *Expirer) Run(ctx context.Context, interval time.Duration) error {
	for {
		batchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), batchTimeout)
		n, err := x.RunOnce(batchCtx)
		cancel()
		switch {
		case err != nil:
			x.logger.ErrorContext(ctx, "expiry batch failed", "err", err)
		case n > 0:
			x.logger.InfoContext(ctx, "orders expired", "count", n)
		}

		wait := interval
		if err == nil && n == ExpiryBatchSize {
			wait = 0
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}
