// Package order owns the order lifecycle: creating held orders, reading and
// cancelling them. Every status change is a conditional UPDATE plus an
// outbox row in the same transaction.
package order

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/huy205-dev/ticketrush/internal/catalog"
	"github.com/huy205-dev/ticketrush/internal/events"
	"github.com/huy205-dev/ticketrush/internal/inventory"
	"github.com/huy205-dev/ticketrush/internal/order/orderdb"
	"github.com/huy205-dev/ticketrush/internal/outbox"
	"github.com/huy205-dev/ticketrush/internal/platform/logging"
)

// Unique constraints whose violation is part of the normal flow.
const (
	constraintOneHeldPerUser = "orders_one_held_per_user"
	constraintIdempotencyKey = "orders_user_id_idempotency_key_key" // default name of UNIQUE (user_id, idempotency_key)
)

// releaseTimeout bounds the best-effort release after a failed create or a
// cancel, and the unlock of the idempotency lock. Both run detached from the
// request so a client disconnect does not leave seats or the key locked.
const releaseTimeout = 5 * time.Second

// idempotencyLockTTL bounds how long a crashed request can keep its
// Idempotency-Key locked. It must exceed the longest request (booking's
// WriteTimeout is 15s).
const idempotencyLockTTL = 30 * time.Second

// Locker grants short exclusive locks; see redisx.Locker.
type Locker interface {
	TryLock(ctx context.Context, key string, ttl time.Duration) (unlock func(context.Context) error, ok bool, err error)
}

// Config holds the timing settings of the service.
type Config struct {
	// HoldTTL is how long PostgreSQL accepts payment for a held order.
	HoldTTL time.Duration
	// HoldGrace is added to the inventory hold so no one can re-hold a seat
	// before PostgreSQL considers the old order expired (SPEC.md 6.2).
	HoldGrace time.Duration
}

// Order is an order with its seats.
type Order struct {
	ID            uuid.UUID
	EventID       int64
	UserID        int64
	Status        Status
	TotalVND      int64
	HoldExpiresAt time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
	Seats         []Seat
}

// Seat is a seat in an order with the price paid for it.
type Seat struct {
	SeatID   string
	PriceVND int64
}

// SeatIDs returns the ids of the order's seats.
func (o *Order) SeatIDs() []string {
	ids := make([]string, len(o.Seats))
	for i, s := range o.Seats {
		ids[i] = s.SeatID
	}
	return ids
}

// Service implements the order use cases.
type Service struct {
	pool      *pgxpool.Pool
	catalog   *catalog.Service
	inv       inventory.Inventory
	locks     Locker
	holdTTL   time.Duration
	holdGrace time.Duration
	logger    *slog.Logger
}

// NewService wires the order service. locks guards Idempotency-Keys against
// concurrent use (ADR-005).
func NewService(pool *pgxpool.Pool, cat *catalog.Service, inv inventory.Inventory, locks Locker, cfg Config, logger *slog.Logger) *Service {
	return &Service{
		pool: pool, catalog: cat, inv: inv, locks: locks,
		holdTTL: cfg.HoldTTL, holdGrace: cfg.HoldGrace, logger: logger,
	}
}

// Create places a HELD order (SPEC.md 9.1). The bool is false when the
// Idempotency-Key was already used with the same request and the existing
// order is returned instead.
func (s *Service) Create(ctx context.Context, userID int64, key uuid.UUID, req CreateRequest) (*Order, bool, error) {
	ev, err := s.catalog.Event(ctx, req.EventID)
	if errors.Is(err, catalog.ErrEventNotFound) {
		return nil, false, NewValidationError("event_id", "unknown event")
	}
	if err != nil {
		return nil, false, err
	}
	norm, err := req.normalize(ev)
	if err != nil {
		return nil, false, err
	}
	open, err := s.catalog.SaleOpen(ctx, ev.ID)
	if err != nil {
		return nil, false, err
	}
	if !open {
		return nil, false, NewValidationError("event_id", "sale has not opened yet")
	}

	hash := requestHash(norm)
	idemKey := key.String()

	// Only one request per key may run the create flow (ADR-005). If Redis
	// cannot tell us whether another one is running, refuse rather than risk
	// answering SEATS_UNAVAILABLE for the buyer's own seats (fail closed).
	unlock, locked, err := s.locks.TryLock(ctx, idempotencyLockKey(userID, idemKey), idempotencyLockTTL)
	if err != nil {
		return nil, false, fmt.Errorf("%w: idempotency lock: %w", ErrTemporarilyUnavailable, err)
	}
	if !locked {
		// Another request holds the key, or has just finished with it.
		existing, existingHash, err := s.findByKey(ctx, userID, idemKey)
		switch {
		case err == nil && existingHash == hash:
			return existing, false, nil
		case err == nil:
			return nil, false, ErrIdempotencyKeyReused
		case errors.Is(err, pgx.ErrNoRows):
			return nil, false, ErrIdempotencyKeyInProgress
		default:
			return nil, false, err
		}
	}
	defer s.unlock(ctx, unlock)

	// The lock is advisory: if it expired mid-request or Redis lost it, the
	// UNIQUE (user_id, idempotency_key) constraint still admits one order and
	// the second round below turns the loser into a replay.
	for range 2 {
		existing, existingHash, err := s.findByKey(ctx, userID, idemKey)
		switch {
		case err == nil && existingHash == hash:
			return existing, false, nil
		case err == nil:
			return nil, false, ErrIdempotencyKeyReused
		case !errors.Is(err, pgx.ErrNoRows):
			return nil, false, err
		}

		orderID, err := uuid.NewV7()
		if err != nil {
			return nil, false, fmt.Errorf("generate order id: %w", err)
		}
		ctx := logging.With(ctx, slog.String("order_id", orderID.String()))

		taken, err := s.inv.Hold(ctx, ev.ID, norm.SeatIDs, orderID, s.holdTTL+s.holdGrace)
		if err != nil {
			return nil, false, fmt.Errorf("hold seats: %w", err)
		}
		if len(taken) > 0 {
			return nil, false, &SeatsUnavailableError{SeatIDs: taken}
		}

		o, err := s.insertHeld(ctx, orderID, userID, idemKey, hash, ev, norm)
		if err == nil {
			return o, true, nil
		}
		s.release(ctx, ev.ID, norm.SeatIDs, orderID)

		switch violatedConstraint(err) {
		case constraintOneHeldPerUser:
			return nil, false, ErrActiveOrderExists
		case constraintIdempotencyKey:
			continue
		default:
			return nil, false, fmt.Errorf("create order: %w", err)
		}
	}
	return nil, false, fmt.Errorf("create order: idempotency key %s still conflicting after retry", idemKey)
}

func (s *Service) insertHeld(ctx context.Context, orderID uuid.UUID, userID int64, idemKey, hash string, ev *catalog.Event, req CreateRequest) (*Order, error) {
	prices := make([]int64, len(req.SeatIDs))
	var total int64
	for i, id := range req.SeatIDs {
		seat, _ := ev.Seat(id) // existence checked by normalize
		prices[i] = seat.PriceVND
		total += seat.PriceVND
	}

	var o *Order
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := orderdb.New(tx)
		row, err := q.InsertHeldOrder(ctx, orderdb.InsertHeldOrderParams{
			OrderID:        orderID,
			EventID:        ev.ID,
			UserID:         userID,
			TotalVnd:       total,
			IdempotencyKey: idemKey,
			RequestHash:    hash,
			HoldTtl:        s.holdTTL,
		})
		if err != nil {
			return fmt.Errorf("insert order: %w", err)
		}
		err = q.InsertOrderSeats(ctx, orderdb.InsertOrderSeatsParams{
			OrderID: orderID, EventID: ev.ID, SeatIds: req.SeatIDs, Prices: prices,
		})
		if err != nil {
			return fmt.Errorf("insert order seats: %w", err)
		}
		o = fromRow(row, nil)
		for i, id := range req.SeatIDs {
			o.Seats = append(o.Seats, Seat{SeatID: id, PriceVND: prices[i]})
		}
		return s.writeEvent(ctx, tx, events.OrderHeld, o)
	})
	if err != nil {
		return nil, err
	}
	return o, nil
}

func (s *Service) findByKey(ctx context.Context, userID int64, idemKey string) (*Order, string, error) {
	q := orderdb.New(s.pool)
	row, err := q.GetOrderByKey(ctx, orderdb.GetOrderByKeyParams{UserID: userID, IdempotencyKey: idemKey})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", err
		}
		return nil, "", fmt.Errorf("find order by idempotency key: %w", err)
	}
	seats, err := q.ListOrderSeats(ctx, row.ID)
	if err != nil {
		return nil, "", fmt.Errorf("list order seats: %w", err)
	}
	return fromRow(row, seats), row.RequestHash, nil
}

// Get returns the order if it belongs to userID. Orders of other users are
// reported as not found so their existence is not revealed.
func (s *Service) Get(ctx context.Context, userID int64, id uuid.UUID) (*Order, error) {
	q := orderdb.New(s.pool)
	row, err := q.GetOrder(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && row.UserID != userID) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get order: %w", err)
	}
	seats, err := q.ListOrderSeats(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("list order seats: %w", err)
	}
	return fromRow(row, seats), nil
}

// Cancel moves a HELD order to CANCELLED and releases its seats. Cancelling
// an already cancelled order returns it unchanged.
func (s *Service) Cancel(ctx context.Context, userID int64, id uuid.UUID) (*Order, error) {
	ctx = logging.With(ctx, slog.String("order_id", id.String()))

	var o *Order
	var cancelled bool
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := orderdb.New(tx)
		row, err := q.GetOrderForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && row.UserID != userID) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock order: %w", err)
		}
		switch Status(row.Status) {
		case StatusCancelled:
		case StatusHeld:
			row, err = transition(ctx, q, id, StatusHeld, StatusCancelled)
			if err != nil {
				return err
			}
			cancelled = true
		default:
			return &NotCancellableError{Status: Status(row.Status)}
		}

		seats, err := q.ListOrderSeats(ctx, id)
		if err != nil {
			return fmt.Errorf("list order seats: %w", err)
		}
		o = fromRow(row, seats)
		if cancelled {
			return s.writeEvent(ctx, tx, events.OrderCancelled, o)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if cancelled {
		s.release(ctx, o.EventID, o.SeatIDs(), o.ID)
	}
	return o, nil
}

// transition performs the conditional status update required for every
// state change. The caller must hold the row lock or handle a lost race.
func transition(ctx context.Context, q *orderdb.Queries, id uuid.UUID, from, to Status) (orderdb.Order, error) {
	if !CanTransition(from, to) {
		return orderdb.Order{}, fmt.Errorf("illegal order transition %s -> %s", from, to)
	}
	row, err := q.TransitionOrder(ctx, orderdb.TransitionOrderParams{OrderID: id, FromStatus: string(from), ToStatus: string(to)})
	if err != nil {
		return orderdb.Order{}, fmt.Errorf("transition order %s -> %s: %w", from, to, err)
	}
	return row, nil
}

func (s *Service) writeEvent(ctx context.Context, tx pgx.Tx, eventType string, o *Order) error {
	_, err := outbox.Write(ctx, tx, outbox.Message{
		Topic:     events.TopicOrders,
		Key:       o.ID.String(),
		EventType: eventType,
		Payload: events.OrderEvent{
			OrderID:  o.ID,
			Event:    o.EventID,
			UserID:   o.UserID,
			SeatIDs:  o.SeatIDs(),
			TotalVND: o.TotalVND,
		},
	})
	return err
}

// release frees seats after a failed create or a cancel. Failure is logged,
// not returned: PostgreSQL already reflects the outcome and the hold expires
// on its own.
func (s *Service) release(ctx context.Context, eventID int64, seatIDs []string, orderID uuid.UUID) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	if err := s.inv.Release(ctx, eventID, seatIDs, orderID); err != nil {
		s.logger.WarnContext(ctx, "release seats failed; they free up when the hold expires",
			"err", err, "seat_ids", seatIDs)
	}
}

func (s *Service) unlock(ctx context.Context, unlock func(context.Context) error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	if err := unlock(ctx); err != nil {
		// The lock expires on its own after idempotencyLockTTL.
		s.logger.WarnContext(ctx, "release idempotency lock failed", "err", err)
	}
}

// idempotencyLockKey is "idem:<userId>:<key>"; keys are scoped per user.
func idempotencyLockKey(userID int64, idemKey string) string {
	return "idem:" + strconv.FormatInt(userID, 10) + ":" + idemKey
}

func violatedConstraint(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
		return pgErr.ConstraintName
	}
	return ""
}

func fromRow(row orderdb.Order, seats []orderdb.ListOrderSeatsRow) *Order {
	o := &Order{
		ID:            row.ID,
		EventID:       row.EventID,
		UserID:        row.UserID,
		Status:        Status(row.Status),
		TotalVND:      row.TotalVnd,
		HoldExpiresAt: row.HoldExpiresAt,
		CreatedAt:     row.CreatedAt,
		UpdatedAt:     row.UpdatedAt,
	}
	for _, st := range seats {
		o.Seats = append(o.Seats, Seat{SeatID: st.SeatID, PriceVND: st.PriceVnd})
	}
	return o
}
