package order

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/huy205-dev/ticketrush/internal/events"
	"github.com/huy205-dev/ticketrush/internal/order/orderdb"
	"github.com/huy205-dev/ticketrush/internal/platform/kafka"
	"github.com/huy205-dev/ticketrush/internal/platform/logging"
	"github.com/huy205-dev/ticketrush/internal/ticket"
)

// Ticket is a seat's ticket. QRPayload and IssuedAt are empty until the
// ticket service has issued it.
type Ticket struct {
	ID        uuid.UUID
	SeatID    string
	QRPayload string
	IssuedAt  *time.Time
}

// TicketIssuer turns PAID orders into TICKETED ones by filling each ticket's
// QR payload (SPEC.md 9.6).
type TicketIssuer struct {
	pool   *pgxpool.Pool
	secret string
	logger *slog.Logger
}

// NewTicketIssuer signs QR payloads with secret (TICKET_SECRET).
func NewTicketIssuer(pool *pgxpool.Pool, secret string, logger *slog.Logger) *TicketIssuer {
	return &TicketIssuer{pool: pool, secret: secret, logger: logger}
}

// Issue handles order.paid for orderID. It is idempotent: an order that is
// already TICKETED is left as it is, so a redelivered event does nothing.
// It returns ErrNotFound for an unknown order.
func (ti *TicketIssuer) Issue(ctx context.Context, orderID uuid.UUID) (Status, error) {
	ctx = logging.With(ctx, slog.String("order_id", orderID.String()))
	var status Status
	err := pgx.BeginFunc(ctx, ti.pool, func(tx pgx.Tx) error {
		q := orderdb.New(tx)
		row, err := q.GetOrderForUpdate(ctx, orderID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("lock order: %w", err)
		}
		status = Status(row.Status)
		if status != StatusPaid {
			if status != StatusTicketed {
				ti.logger.WarnContext(ctx, "order.paid for an order that is not PAID; skipped", "status", status)
			}
			return nil
		}

		tickets, err := q.ListOrderTickets(ctx, orderID)
		if err != nil {
			return fmt.Errorf("list tickets: %w", err)
		}
		if len(tickets) == 0 {
			return fmt.Errorf("PAID order has no tickets")
		}
		for _, t := range tickets {
			if t.QrPayload != nil {
				continue
			}
			qr := ticket.Payload(ti.secret, t.ID)
			err := q.SetTicketQR(ctx, orderdb.SetTicketQRParams{TicketID: t.ID, QrPayload: &qr})
			if err != nil {
				return fmt.Errorf("issue ticket %s: %w", t.ID, err)
			}
		}

		row, err = transition(ctx, q, orderID, StatusPaid, StatusTicketed)
		if err != nil {
			return err
		}
		status = StatusTicketed
		seats, err := q.ListOrderSeats(ctx, orderID)
		if err != nil {
			return fmt.Errorf("list order seats: %w", err)
		}
		return writeEvent(ctx, tx, events.TopicTickets, events.TicketIssued, orderEvent(fromRow(row, seats)))
	})
	if err != nil {
		return "", err
	}
	return status, nil
}

// Handler consumes order.paid (consumer group "ticket"). An event for an
// order that does not exist is skipped; anything else is retried.
func (ti *TicketIssuer) Handler() kafka.Handler {
	return events.Handler(func(ctx context.Context, ev events.OrderEvent) error {
		status, err := ti.Issue(ctx, ev.OrderID)
		if errors.Is(err, ErrNotFound) {
			return kafka.Permanent(err)
		}
		if err != nil {
			return err
		}
		ti.logger.InfoContext(ctx, "order.paid handled", "status", status)
		return nil
	}, events.OrderPaid)
}

// Tickets returns the tickets of an order owned by userID.
func (s *Service) Tickets(ctx context.Context, userID int64, orderID uuid.UUID) ([]Ticket, error) {
	if _, err := s.Get(ctx, userID, orderID); err != nil {
		return nil, err
	}
	rows, err := orderdb.New(s.pool).ListOrderTickets(ctx, orderID)
	if err != nil {
		return nil, fmt.Errorf("list tickets: %w", err)
	}
	out := make([]Ticket, len(rows))
	for i, r := range rows {
		out[i] = Ticket{ID: r.ID, SeatID: r.SeatID, IssuedAt: r.IssuedAt}
		if r.QrPayload != nil {
			out[i].QRPayload = *r.QrPayload
		}
	}
	return out, nil
}
