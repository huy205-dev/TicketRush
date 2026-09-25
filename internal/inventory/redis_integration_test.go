//go:build integration

package inventory_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/huy205-dev/ticketrush/internal/inventory"
)

func newRedisInventory(t *testing.T) (*inventory.Redis, *redis.Client) {
	t.Helper()
	rdb := containers.Redis.New(t)
	return inventory.NewRedis(rdb, slog.New(slog.DiscardHandler)), rdb
}

func get(t *testing.T, rdb *redis.Client, key string) string {
	t.Helper()
	v, err := rdb.Get(context.Background(), key).Result()
	if errors.Is(err, redis.Nil) {
		return "<none>"
	}
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// SPEC.md M2: the Lua scripts against a real Redis.
func TestRedisScripts(t *testing.T) {
	inv, rdb := newRedisInventory(t)
	ctx := context.Background()
	const event = 7
	a, b := newID(t), newID(t)
	key := func(seat string) string { return fmt.Sprintf("seat:{%d}:%s", event, seat) }

	t.Run("hold writes held:<order> with ttl", func(t *testing.T) {
		taken, err := inv.Hold(ctx, event, []string{"VIP-A-1", "VIP-A-2"}, a, 90*time.Second)
		if err != nil || len(taken) != 0 {
			t.Fatalf("Hold = %v, %v", taken, err)
		}
		if v := get(t, rdb, key("VIP-A-1")); v != "held:"+a.String() {
			t.Errorf("value = %q", v)
		}
		ttl := rdb.PTTL(ctx, key("VIP-A-2")).Val()
		if ttl <= 85*time.Second || ttl > 90*time.Second {
			t.Errorf("PTTL = %v, want about 90s", ttl)
		}
	})

	t.Run("hold again with the same order extends the ttl", func(t *testing.T) {
		if taken, err := inv.Hold(ctx, event, []string{"VIP-A-1"}, a, 10*time.Minute); err != nil || len(taken) != 0 {
			t.Fatalf("Hold = %v, %v", taken, err)
		}
		if ttl := rdb.PTTL(ctx, key("VIP-A-1")).Val(); ttl < 9*time.Minute {
			t.Errorf("PTTL = %v, want the new 10m ttl", ttl)
		}
	})

	t.Run("hold of a taken seat returns seat ids, not keys", func(t *testing.T) {
		taken, err := inv.Hold(ctx, event, []string{"VIP-A-3", "VIP-A-2", "VIP-A-1"}, b, time.Minute)
		if err != nil || !slices.Equal(taken, []string{"VIP-A-2", "VIP-A-1"}) {
			t.Fatalf("Hold = %v, %v; want [VIP-A-2 VIP-A-1]", taken, err)
		}
		if v := get(t, rdb, key("VIP-A-3")); v != "<none>" {
			t.Errorf("VIP-A-3 = %q, want untouched", v)
		}
	})

	t.Run("release by the wrong owner does nothing", func(t *testing.T) {
		if err := inv.Release(ctx, event, []string{"VIP-A-1", "VIP-A-2"}, b); err != nil {
			t.Fatal(err)
		}
		if v := get(t, rdb, key("VIP-A-1")); v != "held:"+a.String() {
			t.Errorf("value = %q, want still held by a", v)
		}
	})

	t.Run("confirm turns held into sold without ttl", func(t *testing.T) {
		if err := inv.Confirm(ctx, event, []string{"VIP-A-1", "VIP-A-2"}, a); err != nil {
			t.Fatal(err)
		}
		if v := get(t, rdb, key("VIP-A-1")); v != "sold:"+a.String() {
			t.Errorf("value = %q", v)
		}
		if ttl := rdb.PTTL(ctx, key("VIP-A-1")).Val(); ttl != -1 {
			t.Errorf("PTTL = %v, want -1 (no expiry)", ttl)
		}
		if err := inv.Confirm(ctx, event, []string{"VIP-A-1"}, a); err != nil {
			t.Errorf("confirm is not idempotent: %v", err)
		}
	})

	t.Run("sold seats are taken for everyone, the buyer included", func(t *testing.T) {
		taken, err := inv.Hold(ctx, event, []string{"VIP-A-1"}, a, time.Minute)
		if err != nil || len(taken) != 1 {
			t.Errorf("Hold on own sold seat = %v, %v; want taken", taken, err)
		}
		if err := inv.Release(ctx, event, []string{"VIP-A-1"}, a); err != nil {
			t.Fatal(err)
		}
		if v := get(t, rdb, key("VIP-A-1")); v != "sold:"+a.String() {
			t.Errorf("release removed a sold seat: %q", v)
		}
	})

	t.Run("confirm of a free seat marks it sold", func(t *testing.T) {
		// Late payment after the hold expired (SPEC.md 6.5 allows v == false).
		if err := inv.Confirm(ctx, event, []string{"VIP-B-1"}, b); err != nil {
			t.Fatal(err)
		}
		if v := get(t, rdb, key("VIP-B-1")); v != "sold:"+b.String() {
			t.Errorf("value = %q", v)
		}
	})

	t.Run("confirm of a seat owned by another order is reported", func(t *testing.T) {
		err := inv.Confirm(ctx, event, []string{"VIP-A-2"}, b)
		if !errors.Is(err, inventory.ErrConfirmMismatch) {
			t.Errorf("error = %v, want ErrConfirmMismatch", err)
		}
	})

	t.Run("status and version", func(t *testing.T) {
		st, err := inv.Status(ctx, event, []string{"VIP-A-1", "VIP-A-3", "VIP-B-1"})
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]inventory.SeatStatus{"VIP-A-1": inventory.Sold, "VIP-A-3": inventory.Available, "VIP-B-1": inventory.Sold}
		for seat, w := range want {
			if st[seat] != w {
				t.Errorf("Status[%s] = %s, want %s", seat, st[seat], w)
			}
		}
		v, err := inv.SeatMapVersion(ctx, event)
		// hold, hold again, confirm, confirm again, confirm free seat = 5;
		// failed holds, no-op releases and failed confirms do not count.
		if err != nil || v != 5 {
			t.Errorf("SeatMapVersion = %d, %v; want 5", v, err)
		}
	})
}

// Status must cover more seats than one MGET batch (500).
func TestRedisStatusManySeats(t *testing.T) {
	inv, _ := newRedisInventory(t)
	ctx := context.Background()
	seats := make([]string, 1234)
	for i := range seats {
		seats[i] = fmt.Sprintf("CAT2-X-%d", i)
	}
	if taken, err := inv.Hold(ctx, 1, []string{seats[0], seats[777], seats[1233]}, newID(t), time.Minute); err != nil || len(taken) != 0 {
		t.Fatal(taken, err)
	}
	st, err := inv.Status(ctx, 1, seats)
	if err != nil || len(st) != len(seats) {
		t.Fatalf("Status: %d entries, %v", len(st), err)
	}
	held := 0
	for _, s := range st {
		if s == inventory.Held {
			held++
		}
	}
	if held != 3 || st[seats[777]] != inventory.Held || st[seats[1]] != inventory.Available {
		t.Errorf("held=%d, [777]=%s, [1]=%s", held, st[seats[777]], st[seats[1]])
	}
}
