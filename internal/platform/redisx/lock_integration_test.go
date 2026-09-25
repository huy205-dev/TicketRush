//go:build integration

package redisx_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/huy205-dev/ticketrush/internal/platform/redisx"
	"github.com/huy205-dev/ticketrush/internal/platform/testredis"
)

var rds *testredis.Redis

func TestMain(m *testing.M) { testredis.Main(m, &rds) }

func TestLocker(t *testing.T) {
	rdb := rds.New(t)
	l := redisx.NewLocker(rdb)
	ctx := context.Background()

	unlock, ok, err := l.TryLock(ctx, "k", time.Minute)
	if err != nil || !ok {
		t.Fatalf("first TryLock = %v, %v", ok, err)
	}
	if _, ok, err := l.TryLock(ctx, "k", time.Minute); err != nil || ok {
		t.Fatalf("second TryLock = %v, %v; want held", ok, err)
	}
	if err := unlock(ctx); err != nil {
		t.Fatal(err)
	}
	if n := rdb.Exists(ctx, "k").Val(); n != 0 {
		t.Fatalf("lock key still exists after unlock")
	}
	if _, ok, _ := l.TryLock(ctx, "k", time.Minute); !ok {
		t.Fatal("cannot take the lock after unlock")
	}
}

func TestLockerExpiredHolderCannotUnlockTheNextOne(t *testing.T) {
	rdb := rds.New(t)
	l := redisx.NewLocker(rdb)
	ctx := context.Background()

	staleUnlock, ok, _ := l.TryLock(ctx, "k", 20*time.Millisecond)
	if !ok {
		t.Fatal("TryLock failed")
	}
	time.Sleep(50 * time.Millisecond) // the first holder's lock expires
	_, ok, _ = l.TryLock(ctx, "k", time.Minute)
	if !ok {
		t.Fatal("lock did not expire")
	}
	if err := staleUnlock(ctx); err != nil {
		t.Fatal(err)
	}
	if n := rdb.Exists(ctx, "k").Val(); n != 1 {
		t.Error("the stale holder deleted the new holder's lock")
	}
}

func TestLockerOneWinner(t *testing.T) {
	l := redisx.NewLocker(rds.New(t))
	var wins atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 200 {
		wg.Go(func() {
			<-start
			if _, ok, err := l.TryLock(context.Background(), "race", time.Minute); err != nil {
				t.Error(err)
			} else if ok {
				wins.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 {
		t.Errorf("%d goroutines got the lock, want 1", wins.Load())
	}
}

func TestLockerReportsRedisDown(t *testing.T) {
	l := redisx.NewLocker(redisx.NewClient("127.0.0.1:1")) // nothing listens there
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, ok, err := l.TryLock(ctx, "k", time.Minute); err == nil || ok {
		t.Errorf("TryLock with Redis down = %v, %v; want an error", ok, err)
	}
}
