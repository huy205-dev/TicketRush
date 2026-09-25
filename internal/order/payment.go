package order

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/huy205-dev/ticketrush/internal/events"
	"github.com/huy205-dev/ticketrush/internal/order/orderdb"
	"github.com/huy205-dev/ticketrush/internal/payment"
	"github.com/huy205-dev/ticketrush/internal/platform/logging"
)

// Outcome is what a payment webhook did. The first three are stored in
// payments.outcome; the last two record nothing.
type Outcome string

const (
	OutcomeApplied        Outcome = "APPLIED"
	OutcomeRefundRequired Outcome = "REFUND_REQUIRED"
	OutcomeIgnoredFailed  Outcome = "IGNORED_FAILED"
	OutcomeDuplicate      Outcome = "DUPLICATE"
	OutcomeUnknownOrder   Outcome = "UNKNOWN_ORDER"
)

// Refund reasons (refunds.reason).
const (
	ReasonAmountMismatch       = "AMOUNT_MISMATCH"
	ReasonLatePaymentSeatTaken = "LATE_PAYMENT_SEAT_TAKEN"
	ReasonPaidAfterCancel      = "PAID_AFTER_CANCEL"
	ReasonDoublePayment        = "DOUBLE_PAYMENT"
	ReasonSeatConflict         = "SEAT_CONFLICT"
)

// latePaymentHoldTTL is the short hold taken when money arrives after the
// hold ended, long enough to commit the tickets (SPEC.md 9.3 step 6).
const latePaymentHoldTTL = 60 * time.Second

// PaymentResult reports what ApplyPayment decided.
type PaymentResult struct {
	Outcome Outcome
	// Reason is the refund reason when Outcome is OutcomeRefundRequired.
	Reason string
	// Status is the order's status after the webhook ("" if unknown order).
	Status Status
}

// afterCommit lists the inventory work to do once PostgreSQL has committed
// the decision: PostgreSQL is the source of truth, Redis follows it.
type afterCommit struct {
	confirm bool // seats become sold
	release bool // seats become free again
}

// ApplyPayment processes a verified payment webhook (SPEC.md 9.3). It is
// idempotent per provider_txn_id: a repeated webhook changes nothing and
// reports OutcomeDuplicate. Any error is an infrastructure error; the caller
// answers 500 so fakepay retries.
//
// Branch order differs from SPEC.md 9.3 in one place: an order that is
// already PAID, TICKETED, REFUNDING or REFUNDED is handled as a double
// payment (step 8) before the amount check (step 4), because the state
// machine does not allow those orders to move to REFUNDING (ADR-003).
func (s *Service) ApplyPayment(ctx context.Context, p payment.Webhook) (PaymentResult, error) {
	ctx = logging.With(ctx, slog.String("order_id", p.OrderID.String()), slog.String("provider_txn_id", p.ProviderTxnID))

	var (
		res   PaymentResult
		o     *Order
		after afterCommit
	)
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		res, o, after = PaymentResult{}, nil, afterCommit{}
		q := orderdb.New(tx)

		row, err := q.LockOrderForPayment(ctx, p.OrderID)
		if errors.Is(err, pgx.ErrNoRows) {
			res.Outcome = OutcomeUnknownOrder
			return nil
		}
		if err != nil {
			return fmt.Errorf("lock order: %w", err)
		}

		_, err = q.InsertPayment(ctx, orderdb.InsertPaymentParams{
			ProviderTxnID: p.ProviderTxnID, OrderID: p.OrderID, AmountVnd: p.AmountVND, Status: p.Status,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			res.Outcome, res.Status = OutcomeDuplicate, Status(row.Status)
			return nil
		}
		if err != nil {
			return fmt.Errorf("record payment: %w", err)
		}

		seats, err := q.ListOrderSeats(ctx, p.OrderID)
		if err != nil {
			return fmt.Errorf("list order seats: %w", err)
		}
		o = fromRow(orderdb.Order{
			ID: row.ID, EventID: row.EventID, UserID: row.UserID, Status: row.Status, TotalVnd: row.TotalVnd,
			IdempotencyKey: row.IdempotencyKey, RequestHash: row.RequestHash, HoldExpiresAt: row.HoldExpiresAt,
			CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		}, seats)

		pt := paymentTx{s: s, tx: tx, q: q, p: p, o: o}
		res, after, err = pt.decide(ctx, row.HoldActive)
		if err != nil {
			return err
		}
		if err := q.SetPaymentOutcome(ctx, orderdb.SetPaymentOutcomeParams{ProviderTxnID: p.ProviderTxnID, Outcome: string(res.Outcome)}); err != nil {
			return fmt.Errorf("set payment outcome: %w", err)
		}
		return nil
	})
	if err != nil {
		return PaymentResult{}, err
	}

	if res.Outcome == OutcomeUnknownOrder {
		s.logger.WarnContext(ctx, "payment webhook for an unknown order ignored")
	}
	if after.confirm {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
		defer cancel()
		if err := s.inv.Confirm(ctx, o.EventID, o.SeatIDs(), o.ID); err != nil {
			// PostgreSQL already has the tickets; reconcile (M8) repairs Redis.
			s.logger.WarnContext(ctx, "confirm seats in inventory failed", "err", err, "seat_ids", o.SeatIDs())
		}
	}
	if after.release {
		releaseSeats(ctx, s.inv, s.logger, o.EventID, o.SeatIDs(), o.ID)
	}
	return res, nil
}

// paymentTx is one webhook transaction with the order row locked.
type paymentTx struct {
	s  *Service
	tx pgx.Tx
	q  *orderdb.Queries
	p  payment.Webhook
	o  *Order
}

func (pt paymentTx) decide(ctx context.Context, holdActive bool) (PaymentResult, afterCommit, error) {
	status := pt.o.Status
	switch {
	case pt.p.Status == payment.StatusFailed: // step 3
		return PaymentResult{Outcome: OutcomeIgnoredFailed, Status: status}, afterCommit{}, nil

	case status == StatusPaid || status == StatusTicketed || status == StatusRefunding || status == StatusRefunded: // step 8
		// A second transaction for an order that is already paid for or
		// being refunded: refund this money, leave the order alone.
		return pt.refund(ctx, ReasonDoublePayment, "", afterCommit{})

	case pt.p.AmountVND != pt.o.TotalVND: // step 4
		after := afterCommit{release: status == StatusHeld}
		return pt.refund(ctx, ReasonAmountMismatch, StatusRefunding, after)

	case status == StatusHeld && holdActive: // step 5, the happy path
		ok, err := pt.insertTickets(ctx)
		if err != nil {
			return PaymentResult{}, afterCommit{}, err
		}
		if !ok {
			// Should never happen while the inventory is right: the tickets
			// UNIQUE constraint just stopped a double sale.
			pt.s.logger.ErrorContext(ctx, "double sell prevented by tickets UNIQUE constraint",
				"seat_ids", pt.o.SeatIDs())
			return pt.refund(ctx, ReasonSeatConflict, StatusRefunding, afterCommit{release: true})
		}
		return pt.pay(ctx)

	case status == StatusHeld || status == StatusExpired: // step 6, late payment
		taken, err := pt.s.inv.Hold(ctx, pt.o.EventID, pt.o.SeatIDs(), pt.o.ID, latePaymentHoldTTL)
		if err != nil {
			return PaymentResult{}, afterCommit{}, fmt.Errorf("re-hold seats for late payment: %w", err)
		}
		if len(taken) > 0 {
			return pt.refund(ctx, ReasonLatePaymentSeatTaken, StatusRefunding, afterCommit{})
		}
		ok, err := pt.insertTickets(ctx)
		if err != nil {
			return PaymentResult{}, afterCommit{}, err
		}
		if !ok {
			// Re-held in the inventory but ticketed elsewhere: give the short
			// hold back and refund.
			return pt.refund(ctx, ReasonLatePaymentSeatTaken, StatusRefunding, afterCommit{release: true})
		}
		return pt.pay(ctx)

	case status == StatusCancelled: // step 7
		return pt.refund(ctx, ReasonPaidAfterCancel, StatusRefunding, afterCommit{})

	default:
		return PaymentResult{}, afterCommit{}, fmt.Errorf("payment for order in unexpected status %s", status)
	}
}

// pay moves the order to PAID and announces it.
func (pt paymentTx) pay(ctx context.Context) (PaymentResult, afterCommit, error) {
	if err := pt.moveTo(ctx, StatusPaid); err != nil {
		return PaymentResult{}, afterCommit{}, err
	}
	if err := writeEvent(ctx, pt.tx, events.TopicOrders, events.OrderPaid, orderEvent(pt.o)); err != nil {
		return PaymentResult{}, afterCommit{}, err
	}
	return PaymentResult{Outcome: OutcomeApplied, Status: StatusPaid}, afterCommit{confirm: true}, nil
}

// refund records a refund of this payment and asks the refunder for it.
// to is the order's new status, or "" to keep it (double payment).
func (pt paymentTx) refund(ctx context.Context, reason string, to Status, after afterCommit) (PaymentResult, afterCommit, error) {
	if to != "" {
		if err := pt.moveTo(ctx, to); err != nil {
			return PaymentResult{}, afterCommit{}, err
		}
	}
	err := pt.q.InsertRefund(ctx, orderdb.InsertRefundParams{
		ProviderTxnID: pt.p.ProviderTxnID, OrderID: pt.o.ID, AmountVnd: pt.p.AmountVND, Reason: reason,
	})
	if err != nil {
		return PaymentResult{}, afterCommit{}, fmt.Errorf("record refund: %w", err)
	}
	ev := orderEvent(pt.o)
	ev.ProviderTxnID, ev.AmountVND, ev.Reason = pt.p.ProviderTxnID, pt.p.AmountVND, reason
	if err := writeEvent(ctx, pt.tx, events.TopicOrders, events.OrderRefundRequested, ev); err != nil {
		return PaymentResult{}, afterCommit{}, err
	}
	pt.s.logger.InfoContext(ctx, "payment will be refunded", "reason", reason, "amount_vnd", pt.p.AmountVND)
	return PaymentResult{Outcome: OutcomeRefundRequired, Reason: reason, Status: pt.o.Status}, after, nil
}

func (pt paymentTx) moveTo(ctx context.Context, to Status) error {
	if _, err := transition(ctx, pt.q, pt.o.ID, pt.o.Status, to); err != nil {
		return err
	}
	pt.o.Status = to
	return nil
}

// insertTickets creates one ticket per seat inside a savepoint. It returns
// false, with the savepoint rolled back, when a seat already has a ticket.
func (pt paymentTx) insertTickets(ctx context.Context) (bool, error) {
	seats := pt.o.SeatIDs()
	ids := make([]uuid.UUID, len(seats))
	for i := range ids {
		id, err := uuid.NewV7()
		if err != nil {
			return false, fmt.Errorf("generate ticket id: %w", err)
		}
		ids[i] = id
	}

	sp, err := pt.tx.Begin(ctx) // SAVEPOINT
	if err != nil {
		return false, fmt.Errorf("savepoint: %w", err)
	}
	err = orderdb.New(sp).InsertTickets(ctx, orderdb.InsertTicketsParams{
		Ids: ids, OrderID: pt.o.ID, EventID: pt.o.EventID, SeatIds: seats,
	})
	if err != nil {
		_ = sp.Rollback(ctx)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return false, nil
		}
		return false, fmt.Errorf("insert tickets: %w", err)
	}
	if err := sp.Commit(ctx); err != nil { // RELEASE SAVEPOINT
		return false, fmt.Errorf("release savepoint: %w", err)
	}
	return true, nil
}
