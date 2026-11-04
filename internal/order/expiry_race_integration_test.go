//go:build integration

package order_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/huy205-dev/ticketrush/internal/catalog"
	"github.com/huy205-dev/ticketrush/internal/inventory"
	"github.com/huy205-dev/ticketrush/internal/order"
	"github.com/huy205-dev/ticketrush/internal/platform/redisx"
)

// gatedInventory pauses the first Release and the first Confirm until the
// test lets them go, so a test can force one interleaving of two workers.
type gatedInventory struct {
	inventory.Inventory

	releaseEntered, releaseGo chan struct{}
	confirmEntered, confirmGo chan struct{}
	releaseOnce, confirmOnce  sync.Once
}

func newGatedInventory(inv inventory.Inventory) *gatedInventory {
	return &gatedInventory{
		Inventory:      inv,
		releaseEntered: make(chan struct{}), releaseGo: make(chan struct{}),
		confirmEntered: make(chan struct{}), confirmGo: make(chan struct{}),
	}
}

func (g *gatedInventory) Release(ctx context.Context, eventID int64, seatIDs []string, orderID uuid.UUID) error {
	g.releaseOnce.Do(func() { close(g.releaseEntered); <-g.releaseGo })
	return g.Inventory.Release(ctx, eventID, seatIDs, orderID)
}

func (g *gatedInventory) Confirm(ctx context.Context, eventID int64, seatIDs []string, orderID uuid.UUID) error {
	g.confirmOnce.Do(func() { close(g.confirmEntered); <-g.confirmGo })
	return g.Inventory.Confirm(ctx, eventID, seatIDs, orderID)
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// The expiry worker releases an order's seats after its commit. A late
// payment for that order re-holds the same seats under the same owner value.
// If the release lands between that re-hold and the payment's confirm, the
// re-hold is deleted and another buyer can take a seat PostgreSQL is about
// to ticket to the late payer (review of M4). The expirer must release under
// the order lock and keep the seats of an order a late payment has made PAID.
func TestExpiryReleaseDoesNotUndoLatePayment(t *testing.T) {
	forEachBackend(t, func(t *testing.T, e env) {
		ctx := context.Background()
		logger := slog.New(slog.DiscardHandler)
		late := e.create(t, shortHoldService(e, 200*time.Millisecond), 1, "VIP-A-1")
		time.Sleep(300 * time.Millisecond)

		gated := newGatedInventory(e.inv)
		payer := order.NewService(e.pool, catalog.NewService(e.pool, gated), gated, redisx.NewLocker(e.rdb),
			order.Config{HoldTTL: holdTTL, HoldGrace: holdGrace}, logger)

		expired := make(chan error, 1)
		go func() {
			_, err := order.NewExpirer(e.pool, gated, logger).RunOnce(ctx)
			expired <- err
		}()
		// The expiry transaction has committed and the release has started.
		waitClosed(t, gated.releaseEntered, "the expirer's release")

		type applied struct {
			res order.PaymentResult
			err error
		}
		paidCh := make(chan applied, 1)
		go func() {
			res, err := payer.ApplyPayment(ctx, paid(late, 3_500_000))
			paidCh <- applied{res, err}
		}()

		// Without the fix the payment runs to its confirm while the release
		// is still paused; with it, the payment waits for the order lock.
		select {
		case <-gated.confirmEntered:
		case <-time.After(500 * time.Millisecond):
		}
		close(gated.releaseGo)
		if err := <-expired; err != nil {
			t.Fatal(err)
		}
		waitClosed(t, gated.confirmEntered, "the late payment's confirm")

		// Another buyer tries the seat between the payment's commit and its
		// confirm: it must still be taken.
		_, _, err := e.svc.Create(ctx, 2, uuid.New(), e.req("VIP-A-1"))
		if !errors.Is(err, order.ErrSeatsUnavailable) {
			t.Errorf("another buyer's create = %v, want SEATS_UNAVAILABLE", err)
		}

		close(gated.confirmGo)
		got := <-paidCh
		if got.err != nil || got.res.Outcome != order.OutcomeApplied || got.res.Status != order.StatusPaid {
			t.Fatalf("late payment = %+v, %v; want APPLIED and PAID", got.res, got.err)
		}
		e.wantSeats(t, inventory.Sold, "VIP-A-1")
	})
}
