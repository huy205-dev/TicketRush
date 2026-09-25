//go:build integration

package order_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/huy205-dev/ticketrush/internal/events"
	"github.com/huy205-dev/ticketrush/internal/inventory"
	"github.com/huy205-dev/ticketrush/internal/order"
	"github.com/huy205-dev/ticketrush/internal/payment"
)

// Every branch of the payment webhook (SPEC.md 9.3), on both inventory
// backends. The seats in DemoEvent's VIP zone cost 3.500.000 đ each.

func paid(o *order.Order, amount int64) payment.Webhook {
	return payment.Webhook{
		ProviderTxnID: "fp_" + uuid.NewString(), OrderID: o.ID, AmountVND: amount,
		Status: payment.StatusSucceeded, PaidAt: time.Now().UTC(),
	}
}

func (e env) apply(t *testing.T, wh payment.Webhook) order.PaymentResult {
	t.Helper()
	res, err := e.svc.ApplyPayment(context.Background(), wh)
	if err != nil {
		t.Fatalf("ApplyPayment(%s): %v", wh.ProviderTxnID, err)
	}
	return res
}

func (e env) create(t *testing.T, svc *order.Service, userID int64, seats ...string) *order.Order {
	t.Helper()
	o, _, err := svc.Create(context.Background(), userID, uuid.New(), e.req(seats...))
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// paymentOutcome is payments.outcome for txn, or "" if no row was recorded.
func (e env) paymentOutcome(t *testing.T, txn string) string {
	t.Helper()
	var outcome string
	err := e.pool.QueryRow(context.Background(), `SELECT outcome FROM payments WHERE provider_txn_id = $1`, txn).Scan(&outcome)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

type refundRow struct {
	OrderID   uuid.UUID
	AmountVND int64
	Reason    string
	Status    string
}

func (e env) refunds(t *testing.T) map[string]refundRow {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT provider_txn_id, order_id, amount_vnd, reason, status FROM refunds`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]refundRow{}
	for rows.Next() {
		var txn string
		var r refundRow
		if err := rows.Scan(&txn, &r.OrderID, &r.AmountVND, &r.Reason, &r.Status); err != nil {
			t.Fatal(err)
		}
		out[txn] = r
	}
	return out
}

// ticketSeats lists the seats ticketed to orderID.
func (e env) ticketSeats(t *testing.T, orderID uuid.UUID) []string {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT seat_id FROM tickets WHERE order_id = $1 ORDER BY seat_id`, orderID)
	if err != nil {
		t.Fatal(err)
	}
	seats, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return seats
}

func eventTypes(msgs []outboxRow) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.EventType
	}
	return out
}

func (e env) wantSeats(t *testing.T, want inventory.SeatStatus, seats ...string) {
	t.Helper()
	for seat, st := range e.status(t, seats...) {
		if st != want {
			t.Errorf("%s is %s, want %s", seat, st, want)
		}
	}
}

// wantRefund checks the refund recorded for wh and the order.refund_requested
// event announcing it.
func (e env) wantRefund(t *testing.T, o *order.Order, wh payment.Webhook, reason string) {
	t.Helper()
	r, ok := e.refunds(t)[wh.ProviderTxnID]
	if !ok || r.OrderID != o.ID || r.AmountVND != wh.AmountVND || r.Reason != reason || r.Status != order.RefundPending {
		t.Errorf("refund for %s = %+v (found %v), want %s of %d PENDING", wh.ProviderTxnID, r, ok, reason, wh.AmountVND)
	}
	msgs := e.outbox(t, o.ID)
	last := msgs[len(msgs)-1]
	p := last.Payload
	if last.EventType != events.OrderRefundRequested || last.Topic != events.TopicOrders ||
		p.ProviderTxnID != wh.ProviderTxnID || p.AmountVND != wh.AmountVND || p.Reason != reason {
		t.Errorf("last outbox row = %s %+v, want order.refund_requested for %s", last.EventType, p, wh.ProviderTxnID)
	}
}

func TestPaymentOnTime(t *testing.T) {
	forEachBackend(t, func(t *testing.T, e env) {
		o := e.create(t, e.svc, 1, "VIP-A-1", "VIP-A-2")
		wh := paid(o, 7_000_000)

		res := e.apply(t, wh)
		if res.Outcome != order.OutcomeApplied || res.Status != order.StatusPaid {
			t.Fatalf("result = %+v, want APPLIED and PAID", res)
		}
		if s := e.orderStatus(t, o.ID); s != order.StatusPaid {
			t.Errorf("order is %s, want PAID", s)
		}
		if got := e.ticketSeats(t, o.ID); !slices.Equal(got, []string{"VIP-A-1", "VIP-A-2"}) {
			t.Errorf("tickets for %v, want both seats", got)
		}
		if got := e.paymentOutcome(t, wh.ProviderTxnID); got != string(order.OutcomeApplied) {
			t.Errorf("payments.outcome = %q", got)
		}
		e.wantSeats(t, inventory.Sold, "VIP-A-1", "VIP-A-2")

		msgs := e.outbox(t, o.ID)
		if got := eventTypes(msgs); !slices.Equal(got, []string{events.OrderHeld, events.OrderPaid}) {
			t.Fatalf("outbox = %v, want held then paid", got)
		}
		if p := msgs[1].Payload; p.OrderID != o.ID || p.TotalVND != 7_000_000 || !slices.Equal(p.SeatIDs, []string{"VIP-A-1", "VIP-A-2"}) {
			t.Errorf("order.paid payload = %+v", p)
		}
		if len(e.refunds(t)) != 0 {
			t.Error("a refund was recorded for a normal payment")
		}
	})
}

// SPEC.md M4: the same webhook three times, here all at once, is applied
// exactly once.
func TestPaymentDuplicateWebhook(t *testing.T) {
	forEachBackend(t, func(t *testing.T, e env) {
		o := e.create(t, e.svc, 1, "VIP-A-1", "VIP-A-2")
		wh := paid(o, 7_000_000)

		results := make([]order.PaymentResult, 3)
		errs := make([]error, 3)
		var wg sync.WaitGroup
		for i := range results {
			wg.Go(func() { results[i], errs[i] = e.svc.ApplyPayment(context.Background(), wh) })
		}
		wg.Wait()

		var applied, dup int
		for i, r := range results {
			if errs[i] != nil {
				t.Fatal(errs[i])
			}
			switch r.Outcome {
			case order.OutcomeApplied:
				applied++
			case order.OutcomeDuplicate:
				dup++
				if r.Status != order.StatusPaid {
					t.Errorf("duplicate reports status %s, want PAID", r.Status)
				}
			}
		}
		if applied != 1 || dup != 2 {
			t.Errorf("outcomes = %+v, want 1 APPLIED and 2 DUPLICATE", results)
		}
		// A fourth copy after the fact changes nothing either.
		if r := e.apply(t, wh); r.Outcome != order.OutcomeDuplicate {
			t.Errorf("late copy = %+v, want DUPLICATE", r)
		}
		if got := eventTypes(e.outbox(t, o.ID)); !slices.Equal(got, []string{events.OrderHeld, events.OrderPaid}) {
			t.Errorf("outbox = %v, want exactly one order.paid", got)
		}
		if n := len(e.ticketSeats(t, o.ID)); n != 2 {
			t.Errorf("%d tickets, want 2", n)
		}
	})
}

func TestPaymentFailed(t *testing.T) {
	forEachBackend(t, func(t *testing.T, e env) {
		o := e.create(t, e.svc, 1, "VIP-A-1")
		failed := paid(o, 3_500_000)
		failed.Status = payment.StatusFailed

		if r := e.apply(t, failed); r.Outcome != order.OutcomeIgnoredFailed || r.Status != order.StatusHeld {
			t.Fatalf("result = %+v, want IGNORED_FAILED with the order still HELD", r)
		}
		if got := e.paymentOutcome(t, failed.ProviderTxnID); got != string(order.OutcomeIgnoredFailed) {
			t.Errorf("payments.outcome = %q", got)
		}
		e.wantSeats(t, inventory.Held, "VIP-A-1")

		// The buyer tries again and this time the payment goes through.
		if r := e.apply(t, paid(o, 3_500_000)); r.Outcome != order.OutcomeApplied {
			t.Errorf("second attempt = %+v, want APPLIED", r)
		}
	})
}

// SPEC.md M4: wrong amount.
func TestPaymentAmountMismatch(t *testing.T) {
	forEachBackend(t, func(t *testing.T, e env) {
		o := e.create(t, e.svc, 1, "VIP-A-1", "VIP-A-2")
		wh := paid(o, 3_500_000) // half

		r := e.apply(t, wh)
		if r.Outcome != order.OutcomeRefundRequired || r.Reason != order.ReasonAmountMismatch || r.Status != order.StatusRefunding {
			t.Fatalf("result = %+v, want REFUND_REQUIRED/AMOUNT_MISMATCH and REFUNDING", r)
		}
		if s := e.orderStatus(t, o.ID); s != order.StatusRefunding {
			t.Errorf("order is %s, want REFUNDING", s)
		}
		if got := e.paymentOutcome(t, wh.ProviderTxnID); got != string(order.OutcomeRefundRequired) {
			t.Errorf("payments.outcome = %q", got)
		}
		e.wantRefund(t, o, wh, order.ReasonAmountMismatch)
		// The order was HELD, so its seats go back on sale.
		e.wantSeats(t, inventory.Available, "VIP-A-1", "VIP-A-2")
		if n := len(e.ticketSeats(t, o.ID)); n != 0 {
			t.Errorf("%d tickets issued for a wrong amount", n)
		}
	})
}

// SPEC.md M4: money arrives after the hold, the seats are still free. Both
// before the expiry worker ran (order still HELD) and after (EXPIRED).
func TestLatePaymentSeatsStillFree(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "held past deadline"
		if expired {
			name = "expired"
		}
		t.Run(name, func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, e env) {
				ctx := context.Background()
				o := e.create(t, shortHoldService(e, 200*time.Millisecond), 1, "VIP-A-1")
				time.Sleep(300 * time.Millisecond)
				want := order.StatusHeld
				if expired {
					if _, err := order.NewExpirer(e.pool, e.inv, slog.New(slog.DiscardHandler)).RunOnce(ctx); err != nil {
						t.Fatal(err)
					}
					want = order.StatusExpired
					e.wantSeats(t, inventory.Available, "VIP-A-1")
				}
				if s := e.orderStatus(t, o.ID); s != want {
					t.Fatalf("order is %s before the payment, want %s", s, want)
				}

				if r := e.apply(t, paid(o, 3_500_000)); r.Outcome != order.OutcomeApplied || r.Status != order.StatusPaid {
					t.Fatalf("result = %+v, want APPLIED and PAID", r)
				}
				if got := e.ticketSeats(t, o.ID); !slices.Equal(got, []string{"VIP-A-1"}) {
					t.Errorf("tickets = %v", got)
				}
				e.wantSeats(t, inventory.Sold, "VIP-A-1")
				if len(e.refunds(t)) != 0 {
					t.Error("refund recorded although the seats could be kept")
				}
			})
		})
	}
}

// SPEC.md M4: money arrives after the hold and someone else has the seat,
// either still holding it or already ticketed.
func TestLatePaymentSeatTaken(t *testing.T) {
	for _, otherPays := range []bool{false, true} {
		name := "held by another buyer"
		if otherPays {
			name = "sold to another buyer"
		}
		t.Run(name, func(t *testing.T) {
			forEachBackend(t, func(t *testing.T, e env) {
				ctx := context.Background()
				late := e.create(t, shortHoldService(e, 200*time.Millisecond), 1, "VIP-A-1", "VIP-A-2")
				time.Sleep(300 * time.Millisecond)
				if _, err := order.NewExpirer(e.pool, e.inv, slog.New(slog.DiscardHandler)).RunOnce(ctx); err != nil {
					t.Fatal(err)
				}
				other := e.create(t, e.svc, 2, "VIP-A-2")
				if otherPays {
					if r := e.apply(t, paid(other, 3_500_000)); r.Outcome != order.OutcomeApplied {
						t.Fatalf("other buyer's payment = %+v", r)
					}
				}

				wh := paid(late, 7_000_000)
				r := e.apply(t, wh)
				if r.Outcome != order.OutcomeRefundRequired || r.Reason != order.ReasonLatePaymentSeatTaken || r.Status != order.StatusRefunding {
					t.Fatalf("result = %+v, want REFUND_REQUIRED/LATE_PAYMENT_SEAT_TAKEN and REFUNDING", r)
				}
				e.wantRefund(t, late, wh, order.ReasonLatePaymentSeatTaken)
				if n := len(e.ticketSeats(t, late.ID)); n != 0 {
					t.Errorf("late order got %d tickets", n)
				}
				// The late order holds nothing, the other buyer keeps the seat.
				e.wantSeats(t, inventory.Available, "VIP-A-1")
				want := inventory.Held
				if otherPays {
					want = inventory.Sold
				}
				e.wantSeats(t, want, "VIP-A-2")
				if s := e.orderStatus(t, other.ID); otherPays && s != order.StatusPaid || !otherPays && s != order.StatusHeld {
					t.Errorf("other buyer's order is %s", s)
				}
			})
		})
	}
}

// SPEC.md M4: money arrives after the buyer cancelled.
func TestPaymentAfterCancel(t *testing.T) {
	forEachBackend(t, func(t *testing.T, e env) {
		o := e.create(t, e.svc, 1, "VIP-A-1")
		if _, err := e.svc.Cancel(context.Background(), 1, o.ID); err != nil {
			t.Fatal(err)
		}
		wh := paid(o, 3_500_000)
		r := e.apply(t, wh)
		if r.Outcome != order.OutcomeRefundRequired || r.Reason != order.ReasonPaidAfterCancel || r.Status != order.StatusRefunding {
			t.Fatalf("result = %+v, want REFUND_REQUIRED/PAID_AFTER_CANCEL and REFUNDING", r)
		}
		e.wantRefund(t, o, wh, order.ReasonPaidAfterCancel)
		e.wantSeats(t, inventory.Available, "VIP-A-1")
	})
}

// A second transaction for an order already paid is refunded and leaves the
// order alone, whatever its amount (ADR-003).
func TestDoublePayment(t *testing.T) {
	forEachBackend(t, func(t *testing.T, e env) {
		o := e.create(t, e.svc, 1, "VIP-A-1")
		if r := e.apply(t, paid(o, 3_500_000)); r.Outcome != order.OutcomeApplied {
			t.Fatalf("first payment = %+v", r)
		}
		for _, amount := range []int64{3_500_000, 100} {
			wh := paid(o, amount)
			r := e.apply(t, wh)
			if r.Outcome != order.OutcomeRefundRequired || r.Reason != order.ReasonDoublePayment || r.Status != order.StatusPaid {
				t.Fatalf("second payment of %d = %+v, want DOUBLE_PAYMENT with the order still PAID", amount, r)
			}
			e.wantRefund(t, o, wh, order.ReasonDoublePayment)
		}
		if s := e.orderStatus(t, o.ID); s != order.StatusPaid {
			t.Errorf("order is %s, want PAID", s)
		}
		if n := len(e.ticketSeats(t, o.ID)); n != 1 {
			t.Errorf("%d tickets, want 1", n)
		}
		e.wantSeats(t, inventory.Sold, "VIP-A-1")
	})
}

// If the inventory ever let two orders hold one seat, the tickets UNIQUE
// constraint stops the second sale and the money is refunded (SPEC.md 9.3
// step 5). The broken inventory is simulated by ticketing the seat to
// another order behind the inventory's back.
func TestPaymentSeatConflict(t *testing.T) {
	forEachBackend(t, func(t *testing.T, e env) {
		ctx := context.Background()
		o := e.create(t, e.svc, 1, "VIP-A-1", "VIP-A-2")
		other := e.create(t, e.svc, 2, "VIP-B-1")
		_, err := e.pool.Exec(ctx, `INSERT INTO tickets (id, order_id, event_id, seat_id) VALUES ($1, $2, $3, 'VIP-A-2')`,
			uuid.New(), other.ID, e.eventID)
		if err != nil {
			t.Fatal(err)
		}

		wh := paid(o, 7_000_000)
		r := e.apply(t, wh)
		if r.Outcome != order.OutcomeRefundRequired || r.Reason != order.ReasonSeatConflict || r.Status != order.StatusRefunding {
			t.Fatalf("result = %+v, want REFUND_REQUIRED/SEAT_CONFLICT and REFUNDING", r)
		}
		e.wantRefund(t, o, wh, order.ReasonSeatConflict)
		// The savepoint was rolled back: not even the free seat got a ticket.
		if n := len(e.ticketSeats(t, o.ID)); n != 0 {
			t.Errorf("%d tickets kept after the conflict", n)
		}
		e.wantSeats(t, inventory.Available, "VIP-A-1")
	})
}

func TestPaymentUnknownOrder(t *testing.T) {
	forEachBackend(t, func(t *testing.T, e env) {
		wh := paid(&order.Order{ID: uuid.New()}, 1)
		if r := e.apply(t, wh); r.Outcome != order.OutcomeUnknownOrder {
			t.Errorf("result = %+v, want UNKNOWN_ORDER", r)
		}
		if got := e.paymentOutcome(t, wh.ProviderTxnID); got != "" {
			t.Errorf("payment recorded for an unknown order: %q", got)
		}
	})
}
