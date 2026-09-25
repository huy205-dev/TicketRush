//go:build integration

package order_test

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/huy205-dev/ticketrush/internal/events"
	"github.com/huy205-dev/ticketrush/internal/order"
	"github.com/huy205-dev/ticketrush/internal/payment"
	"github.com/huy205-dev/ticketrush/internal/ticket"
)

// The consumers that finish the saga: the ticket service (SPEC.md 9.6) and
// the refunder (9.7).

var ticketSecret = strings.Repeat("t", 32)

func (e env) paidOrder(t *testing.T, userID int64, seats ...string) *order.Order {
	t.Helper()
	o := e.create(t, e.svc, userID, seats...)
	if r := e.apply(t, paid(o, 3_500_000*int64(len(seats)))); r.Outcome != order.OutcomeApplied {
		t.Fatalf("payment = %+v", r)
	}
	return o
}

func TestIssueTickets(t *testing.T) {
	forEachBackend(t, func(t *testing.T, e env) {
		ctx := context.Background()
		ti := order.NewTicketIssuer(e.pool, ticketSecret, slog.New(slog.DiscardHandler))
		o := e.paidOrder(t, 1, "VIP-A-1", "VIP-A-2")

		// Before issuing, the buyer sees tickets without QR codes.
		pending, err := e.svc.Tickets(ctx, 1, o.ID)
		if err != nil || len(pending) != 2 || pending[0].QRPayload != "" || pending[0].IssuedAt != nil {
			t.Fatalf("tickets before issuing = %+v, %v", pending, err)
		}

		st, err := ti.Issue(ctx, o.ID)
		if err != nil || st != order.StatusTicketed {
			t.Fatalf("Issue = %s, %v; want TICKETED", st, err)
		}
		tickets, err := e.svc.Tickets(ctx, 1, o.ID)
		if err != nil || len(tickets) != 2 {
			t.Fatalf("Tickets = %+v, %v", tickets, err)
		}
		for i, tk := range tickets {
			id, ok := ticket.Verify(ticketSecret, tk.QRPayload)
			if !ok || id != tk.ID || tk.IssuedAt == nil || tk.SeatID != []string{"VIP-A-1", "VIP-A-2"}[i] {
				t.Errorf("ticket %d = %+v (QR valid %v for %s)", i, tk, ok, id)
			}
		}
		msgs := e.outbox(t, o.ID)
		last := msgs[len(msgs)-1]
		if last.EventType != events.TicketIssued || last.Topic != events.TopicTickets || last.Payload.OrderID != o.ID {
			t.Errorf("last outbox row = %s on %s", last.EventType, last.Topic)
		}

		// A redelivered order.paid does nothing.
		if st, err := ti.Issue(ctx, o.ID); err != nil || st != order.StatusTicketed {
			t.Fatalf("second Issue = %s, %v", st, err)
		}
		again, _ := e.svc.Tickets(ctx, 1, o.ID)
		if again[0].QRPayload != tickets[0].QRPayload {
			t.Error("second Issue changed a QR payload")
		}
		if n := len(e.outbox(t, o.ID)); n != len(msgs) {
			t.Errorf("second Issue wrote %d more outbox rows", n-len(msgs))
		}

		if _, err := e.svc.Tickets(ctx, 2, o.ID); !errors.Is(err, order.ErrNotFound) {
			t.Errorf("another user's tickets: %v, want ErrNotFound", err)
		}
		if _, err := ti.Issue(ctx, uuid.New()); !errors.Is(err, order.ErrNotFound) {
			t.Errorf("Issue unknown order: %v, want ErrNotFound", err)
		}
		held := e.create(t, e.svc, 3, "VIP-A-3")
		if st, err := ti.Issue(ctx, held.ID); err != nil || st != order.StatusHeld {
			t.Errorf("Issue on a HELD order = %s, %v; want it left HELD", st, err)
		}
	})
}

// Two deliveries of order.paid at once (a rebalance) issue once.
func TestIssueTicketsConcurrently(t *testing.T) {
	forEachBackend(t, func(t *testing.T, e env) {
		ti := order.NewTicketIssuer(e.pool, ticketSecret, slog.New(slog.DiscardHandler))
		o := e.paidOrder(t, 1, "VIP-A-1")
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				if _, err := ti.Issue(context.Background(), o.ID); err != nil {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		n := 0
		for _, m := range e.outbox(t, o.ID) {
			if m.EventType == events.TicketIssued {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%d ticket.issued events, want 1", n)
		}
	})
}

// fakeGateway records refunds and fails while down is set.
type fakeGateway struct {
	mu    sync.Mutex
	calls []payment.RefundRequest
	down  bool
}

func (g *fakeGateway) Refund(_ context.Context, req payment.RefundRequest) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.down {
		return errors.New("fakepay unavailable")
	}
	g.calls = append(g.calls, req)
	return nil
}

func TestRefund(t *testing.T) {
	forEachBackend(t, func(t *testing.T, e env) {
		ctx := context.Background()
		gw := &fakeGateway{down: true}
		rp := order.NewRefundProcessor(e.pool, gw, slog.New(slog.DiscardHandler))

		o := e.create(t, e.svc, 1, "VIP-A-1")
		if _, err := e.svc.Cancel(ctx, 1, o.ID); err != nil {
			t.Fatal(err)
		}
		wh := paid(o, 3_500_000)
		e.apply(t, wh)

		// While fakepay is down the refund stays pending and is retried.
		if err := rp.Process(ctx, wh.ProviderTxnID); err == nil {
			t.Fatal("Process succeeded while the gateway is down")
		}
		if r := e.refunds(t)[wh.ProviderTxnID]; r.Status != order.RefundPending {
			t.Fatalf("refund is %s after a failed call, want PENDING", r.Status)
		}

		gw.down = false
		if err := rp.Process(ctx, wh.ProviderTxnID); err != nil {
			t.Fatal(err)
		}
		if len(gw.calls) != 1 || gw.calls[0] != (payment.RefundRequest{OrderID: o.ID, ProviderTxnID: wh.ProviderTxnID, AmountVND: 3_500_000}) {
			t.Errorf("gateway calls = %+v", gw.calls)
		}
		if r := e.refunds(t)[wh.ProviderTxnID]; r.Status != order.RefundSucceeded {
			t.Errorf("refund is %s, want SUCCEEDED", r.Status)
		}
		if s := e.orderStatus(t, o.ID); s != order.StatusRefunded {
			t.Errorf("order is %s, want REFUNDED", s)
		}
		msgs := e.outbox(t, o.ID)
		last := msgs[len(msgs)-1]
		if last.EventType != events.OrderRefunded || last.Payload.ProviderTxnID != wh.ProviderTxnID ||
			last.Payload.AmountVND != 3_500_000 || last.Payload.Reason != order.ReasonPaidAfterCancel {
			t.Errorf("last outbox row = %s %+v, want order.refunded", last.EventType, last.Payload)
		}

		// Redelivery: nothing is refunded twice.
		if err := rp.Process(ctx, wh.ProviderTxnID); err != nil {
			t.Fatal(err)
		}
		if len(gw.calls) != 1 || len(e.outbox(t, o.ID)) != len(msgs) {
			t.Errorf("second Process: %d gateway calls, %d outbox rows (was %d)", len(gw.calls), len(e.outbox(t, o.ID)), len(msgs))
		}

		if err := rp.Process(ctx, "fp_unknown"); !errors.Is(err, order.ErrRefundNotFound) {
			t.Errorf("unknown refund: %v, want ErrRefundNotFound", err)
		}
	})
}

// Refunding a double payment leaves the order PAID (or TICKETED).
func TestRefundDoublePaymentKeepsOrder(t *testing.T) {
	forEachBackend(t, func(t *testing.T, e env) {
		ctx := context.Background()
		gw := &fakeGateway{}
		rp := order.NewRefundProcessor(e.pool, gw, slog.New(slog.DiscardHandler))
		o := e.paidOrder(t, 1, "VIP-A-1")
		second := paid(o, 3_500_000)
		e.apply(t, second)

		if err := rp.Process(ctx, second.ProviderTxnID); err != nil {
			t.Fatal(err)
		}
		if s := e.orderStatus(t, o.ID); s != order.StatusPaid {
			t.Errorf("order is %s, want PAID", s)
		}
		msgs := e.outbox(t, o.ID)
		if got := eventTypes(msgs); !slices.Equal(got, []string{events.OrderHeld, events.OrderPaid, events.OrderRefundRequested, events.OrderRefunded}) {
			t.Errorf("outbox = %v", got)
		}
	})
}
