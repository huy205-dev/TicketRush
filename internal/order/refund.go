package order

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/huy205-dev/ticketrush/internal/events"
	"github.com/huy205-dev/ticketrush/internal/order/orderdb"
	"github.com/huy205-dev/ticketrush/internal/payment"
	"github.com/huy205-dev/ticketrush/internal/platform/kafka"
	"github.com/huy205-dev/ticketrush/internal/platform/logging"
)

// Refund statuses (refunds.status).
const (
	RefundPending   = "PENDING"
	RefundSucceeded = "SUCCEEDED"
)

// ErrRefundNotFound means an order.refund_requested event names a refund
// that does not exist, which the outbox makes impossible.
var ErrRefundNotFound = errors.New("refund not found")

// RefundGateway is the part of the fakepay client the refunder needs.
type RefundGateway interface {
	Refund(ctx context.Context, req payment.RefundRequest) error
}

// RefundProcessor carries out requested refunds (SPEC.md 9.7).
type RefundProcessor struct {
	pool    *pgxpool.Pool
	gateway RefundGateway
	logger  *slog.Logger
}

// NewRefundProcessor refunds through gateway.
func NewRefundProcessor(pool *pgxpool.Pool, gateway RefundGateway, logger *slog.Logger) *RefundProcessor {
	return &RefundProcessor{pool: pool, gateway: gateway, logger: logger}
}

// Handler consumes order.refund_requested (consumer group "refunder").
func (rp *RefundProcessor) Handler() kafka.Handler {
	return events.Handler(func(ctx context.Context, ev events.OrderEvent) error {
		if ev.ProviderTxnID == "" {
			return kafka.Permanent(errors.New("order.refund_requested without provider_txn_id"))
		}
		err := rp.Process(ctx, ev.ProviderTxnID)
		if errors.Is(err, ErrRefundNotFound) {
			return kafka.Permanent(err)
		}
		return err
	}, events.OrderRefundRequested)
}

// Process refunds one payment. It is idempotent per provider_txn_id: a refund
// already SUCCEEDED is skipped, and the gateway call is itself idempotent,
// so a crash between the call and the commit is repaired by the retry.
//
// The order moves from REFUNDING to REFUNDED, except for DOUBLE_PAYMENT
// refunds, which never changed the order's status.
func (rp *RefundProcessor) Process(ctx context.Context, providerTxnID string) error {
	ctx = logging.With(ctx, slog.String("provider_txn_id", providerTxnID))
	ref, err := orderdb.New(rp.pool).GetRefund(ctx, providerTxnID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrRefundNotFound, providerTxnID)
	}
	if err != nil {
		return fmt.Errorf("get refund: %w", err)
	}
	ctx = logging.With(ctx, slog.String("order_id", ref.OrderID.String()))
	if ref.Status == RefundSucceeded {
		return nil
	}

	err = rp.gateway.Refund(ctx, payment.RefundRequest{
		OrderID: ref.OrderID, ProviderTxnID: providerTxnID, AmountVND: ref.AmountVnd,
	})
	if err != nil {
		return err
	}

	return pgx.BeginFunc(ctx, rp.pool, func(tx pgx.Tx) error {
		q := orderdb.New(tx)
		done, err := q.CompleteRefund(ctx, providerTxnID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // completed concurrently by another delivery
		}
		if err != nil {
			return fmt.Errorf("complete refund: %w", err)
		}
		row, err := q.GetOrderForUpdate(ctx, done.OrderID)
		if err != nil {
			return fmt.Errorf("lock order: %w", err)
		}
		if done.Reason != ReasonDoublePayment && Status(row.Status) == StatusRefunding {
			if row, err = transition(ctx, q, done.OrderID, StatusRefunding, StatusRefunded); err != nil {
				return err
			}
		}
		seats, err := q.ListOrderSeats(ctx, done.OrderID)
		if err != nil {
			return fmt.Errorf("list order seats: %w", err)
		}
		ev := orderEvent(fromRow(row, seats))
		ev.ProviderTxnID, ev.AmountVND, ev.Reason = providerTxnID, done.AmountVnd, done.Reason
		if err := writeEvent(ctx, tx, events.TopicOrders, events.OrderRefunded, ev); err != nil {
			return err
		}
		rp.logger.InfoContext(ctx, "refund completed", "reason", done.Reason, "amount_vnd", done.AmountVnd, "order_status", row.Status)
		return nil
	})
}
