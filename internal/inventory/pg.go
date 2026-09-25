package inventory

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/huy205-dev/ticketrush/internal/inventory/inventorydb"
)

// PG keeps holds in the seat_holds table. Hold serialises competing requests
// with SELECT ... FOR UPDATE on the seats rows, so correctness comes entirely
// from PostgreSQL row locks.
type PG struct {
	pool *pgxpool.Pool
}

// NewPG returns the PostgreSQL backend.
func NewPG(pool *pgxpool.Pool) *PG {
	return &PG{pool: pool}
}

var _ Inventory = (*PG)(nil)

// Hold implements Inventory. The expiry is computed by the database clock.
func (p *PG) Hold(ctx context.Context, eventID int64, seatIDs []string, orderID uuid.UUID, ttl time.Duration) ([]string, error) {
	var taken []string
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		q := inventorydb.New(tx)

		// Lock first, then read holds in a new statement: under READ COMMITTED
		// that statement sees any hold committed by whoever held the lock
		// before us.
		locked, err := q.LockSeats(ctx, inventorydb.LockSeatsParams{EventID: eventID, SeatIds: seatIDs})
		if err != nil {
			return fmt.Errorf("lock seats: %w", err)
		}
		if len(locked) != len(seatIDs) {
			return fmt.Errorf("%w: event %d has %d of %d requested seats", ErrUnknownSeat, eventID, len(locked), len(seatIDs))
		}

		taken, err = q.TakenSeats(ctx, inventorydb.TakenSeatsParams{EventID: eventID, SeatIds: seatIDs, OrderID: orderID})
		if err != nil {
			return fmt.Errorf("check seat holds: %w", err)
		}
		if len(taken) > 0 {
			return nil // commit an empty transaction; nothing was written
		}

		err = q.UpsertHolds(ctx, inventorydb.UpsertHoldsParams{EventID: eventID, SeatIds: seatIDs, OrderID: orderID, Ttl: ttl})
		if err != nil {
			return fmt.Errorf("write seat holds: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("pg hold: %w", err)
	}
	return taken, nil
}

// Release implements Inventory.
func (p *PG) Release(ctx context.Context, eventID int64, seatIDs []string, orderID uuid.UUID) error {
	_, err := inventorydb.New(p.pool).DeleteHolds(ctx, inventorydb.DeleteHoldsParams{EventID: eventID, SeatIds: seatIDs, OrderID: orderID})
	if err != nil {
		return fmt.Errorf("pg release: %w", err)
	}
	return nil
}

// Confirm implements Inventory. In this backend the tickets rows already
// mark the seats sold, so Confirm only checks that they exist and drops the
// now redundant holds.
func (p *PG) Confirm(ctx context.Context, eventID int64, seatIDs []string, orderID uuid.UUID) error {
	q := inventorydb.New(p.pool)
	n, err := q.CountOrderTickets(ctx, inventorydb.CountOrderTicketsParams{EventID: eventID, SeatIds: seatIDs, OrderID: orderID})
	if err != nil {
		return fmt.Errorf("pg confirm: %w", err)
	}
	if int(n) != len(seatIDs) {
		return fmt.Errorf("pg confirm: %w: %d of %d seats ticketed", ErrConfirmMismatch, n, len(seatIDs))
	}
	if _, err := q.DeleteHolds(ctx, inventorydb.DeleteHoldsParams{EventID: eventID, SeatIds: seatIDs, OrderID: orderID}); err != nil {
		return fmt.Errorf("pg confirm: drop holds: %w", err)
	}
	return nil
}

// Status implements Inventory.
func (p *PG) Status(ctx context.Context, eventID int64, seatIDs []string) (map[string]SeatStatus, error) {
	rows, err := inventorydb.New(p.pool).SeatStatuses(ctx, inventorydb.SeatStatusesParams{EventID: eventID, SeatIds: seatIDs})
	if err != nil {
		return nil, fmt.Errorf("pg status: %w", err)
	}
	out := make(map[string]SeatStatus, len(seatIDs))
	for _, id := range seatIDs {
		out[id] = Available
	}
	for _, r := range rows {
		if out[r.SeatID] == Sold {
			continue // a ticket outranks a stale hold
		}
		out[r.SeatID] = SeatStatus(r.Status)
	}
	return out, nil
}
