//go:build integration

package order_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/huy205-dev/ticketrush/internal/auth"
	"github.com/huy205-dev/ticketrush/internal/catalog"
	"github.com/huy205-dev/ticketrush/internal/inventory"
	"github.com/huy205-dev/ticketrush/internal/order"
	"github.com/huy205-dev/ticketrush/internal/platform/redisx"
	"github.com/huy205-dev/ticketrush/internal/queue"
)

// admittedService shares e's storage and records admissions in Redis.
func admittedService(e env, ledger order.AdmissionLedger) *order.Service {
	return order.NewService(e.pool, catalog.NewService(e.pool, e.inv), e.inv, redisx.NewLocker(e.rdb),
		order.Config{HoldTTL: holdTTL, HoldGrace: holdGrace, Admissions: ledger}, slog.New(slog.DiscardHandler))
}

func admission(userID, eventID int64) auth.Admission {
	return auth.Admission{UserID: userID, EventID: eventID, QueueID: uuid.New(), JTI: uuid.New(), ExpiresAt: time.Now().Add(time.Minute)}
}

func (e env) usedBy(t *testing.T, jti uuid.UUID) string {
	t.Helper()
	v, err := e.rdb.Get(context.Background(), queue.UsedKey(jti)).Result()
	if err != nil {
		return ""
	}
	return v
}

// SPEC.md 9.1 steps 1 and 5: an admission is for one user and one event and
// creates at most one order; a replay does not use it again.
func TestCreateAdmitted(t *testing.T) {
	forEachBackend(t, func(t *testing.T, e env) {
		ctx := context.Background()
		svc := admittedService(e, queue.NewLedger(e.rdb, time.Minute))
		adm := admission(1, e.eventID)

		for name, bad := range map[string]auth.Admission{
			"another user's":  admission(2, e.eventID),
			"another event's": admission(1, e.eventID+1),
		} {
			if _, _, err := svc.CreateAdmitted(ctx, 1, uuid.New(), e.req("VIP-A-1"), bad); !errors.Is(err, order.ErrAdmissionRequired) {
				t.Errorf("%s admission: err = %v, want ErrAdmissionRequired", name, err)
			}
		}

		key := uuid.New()
		o, created, err := svc.CreateAdmitted(ctx, 1, key, e.req("VIP-A-1"), adm)
		if err != nil || !created {
			t.Fatalf("CreateAdmitted = %v, %v", created, err)
		}
		if got := e.usedBy(t, adm.JTI); got != o.ID.String() {
			t.Errorf("admission:used = %q, want the new order %s", got, o.ID)
		}

		// The same request again is a replay and does not need a new token.
		again, created, err := svc.CreateAdmitted(ctx, 1, key, e.req("VIP-A-1"), adm)
		if err != nil || created || again.ID != o.ID {
			t.Errorf("replay = %v, %v, %v; want the same order", again, created, err)
		}

		// A new request with the used token is refused.
		if _, err := svc.Cancel(ctx, 1, o.ID); err != nil {
			t.Fatal(err)
		}
		if _, _, err := svc.CreateAdmitted(ctx, 1, uuid.New(), e.req("VIP-A-2"), adm); !errors.Is(err, order.ErrAdmissionAlreadyUsed) {
			t.Errorf("second order with one token: err = %v, want ErrAdmissionAlreadyUsed", err)
		}
	})
}

// A request that creates no order gives the admission back (SPEC.md 9.1
// step 6), so the buyer can pick other seats without queueing again.
func TestCreateAdmittedGivesTheTokenBack(t *testing.T) {
	forEachBackend(t, func(t *testing.T, e env) {
		ctx := context.Background()
		svc := admittedService(e, queue.NewLedger(e.rdb, time.Minute))
		e.create(t, e.svc, 9, "VIP-A-1") // someone else holds VIP-A-1

		adm := admission(1, e.eventID)
		if _, _, err := svc.CreateAdmitted(ctx, 1, uuid.New(), e.req("VIP-A-1", "VIP-A-2"), adm); !errors.Is(err, order.ErrSeatsUnavailable) {
			t.Fatalf("taken seat: err = %v, want SEATS_UNAVAILABLE", err)
		}
		if got := e.usedBy(t, adm.JTI); got != "" {
			t.Errorf("admission still claimed by %s after SEATS_UNAVAILABLE", got)
		}
		e.wantSeats(t, inventory.Available, "VIP-A-2")
		first, created, err := svc.CreateAdmitted(ctx, 1, uuid.New(), e.req("VIP-A-2"), adm)
		if err != nil || !created {
			t.Fatalf("retry with other seats = %v, %v", created, err)
		}

		// The buyer already holds an order: the new admission is not spent.
		other := admission(1, e.eventID)
		if _, _, err := svc.CreateAdmitted(ctx, 1, uuid.New(), e.req("VIP-A-3"), other); !errors.Is(err, order.ErrActiveOrderExists) {
			t.Fatalf("second active order: err = %v, want ACTIVE_ORDER_EXISTS", err)
		}
		if got := e.usedBy(t, other.JTI); got != "" {
			t.Errorf("admission claimed by %s after ACTIVE_ORDER_EXISTS", got)
		}
		if got := e.usedBy(t, adm.JTI); got != first.ID.String() {
			t.Errorf("the used admission now points at %q, want %s", got, first.ID)
		}
	})
}

type brokenLedger struct{}

func (brokenLedger) Claim(context.Context, uuid.UUID, uuid.UUID) (bool, error) {
	return false, errors.New("redis: connection refused")
}
func (brokenLedger) Unclaim(context.Context, uuid.UUID, uuid.UUID) error { return nil }

// Without Redis the single use of a token cannot be guaranteed: refuse.
func TestCreateAdmittedFailsClosed(t *testing.T) {
	forEachBackend(t, func(t *testing.T, e env) {
		svc := admittedService(e, brokenLedger{})
		_, _, err := svc.CreateAdmitted(context.Background(), 1, uuid.New(), e.req("VIP-A-1"), admission(1, e.eventID))
		if !errors.Is(err, order.ErrTemporarilyUnavailable) {
			t.Fatalf("err = %v, want ErrTemporarilyUnavailable", err)
		}
		e.wantSeats(t, inventory.Available, "VIP-A-1")
	})
}
