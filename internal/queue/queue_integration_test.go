//go:build integration

package queue_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/huy205-dev/ticketrush/internal/auth"
	"github.com/huy205-dev/ticketrush/internal/platform/testenv"
	"github.com/huy205-dev/ticketrush/internal/queue"
)

var containers testenv.Env

func TestMain(m *testing.M) {
	testenv.Main(m, testenv.Needs{Redis: true}, &containers)
}

const eventID = 1

// openEvents is a fixed list of events with an open sale.
type openEvents []int64

func (e openEvents) OpenEventIDs(context.Context) ([]int64, error) { return e, nil }

var tokens = auth.NewAdmissionTokens(strings.Repeat("a", 32), 5*time.Minute)

func newWorker(rdb *redis.Client, batch int) *queue.Worker {
	return queue.NewWorker(queue.New(rdb), tokens, openEvents{eventID}, batch, slog.New(slog.DiscardHandler))
}

func join(t *testing.T, q *queue.Queue, userID int64) queue.Entry {
	t.Helper()
	e, err := q.Join(context.Background(), eventID, userID)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func status(t *testing.T, q *queue.Queue, id uuid.UUID) queue.Status {
	t.Helper()
	st, err := q.Status(context.Background(), eventID, id)
	if err != nil {
		t.Fatalf("status of %s: %v", id, err)
	}
	return st
}

func TestJoin(t *testing.T) {
	rdb := containers.Redis.New(t)
	q := queue.New(rdb)

	a := join(t, q, 1)
	b := join(t, q, 2)
	if a.Position != 1 || b.Position != 2 || a.QueueID == b.QueueID {
		t.Fatalf("joins = %+v, %+v; want positions 1 and 2", a, b)
	}
	// Joining again keeps the place (SPEC.md 7.2).
	if again := join(t, q, 1); again != a {
		t.Errorf("second join = %+v, want %+v", again, a)
	}
	if st := status(t, q, b.QueueID); st.Position != 2 || st.Token != "" {
		t.Errorf("status = %+v, want position 2", st)
	}
	if _, err := q.Status(context.Background(), eventID, uuid.New()); !errors.Is(err, queue.ErrNotQueued) {
		t.Errorf("status of an unknown id: %v, want ErrNotQueued", err)
	}
}

// SPEC.md M5: tokens are issued in join order, batch by batch.
func TestAdmitInJoinOrder(t *testing.T) {
	rdb := containers.Redis.New(t)
	q := queue.New(rdb)
	w := newWorker(rdb, 10)

	const buyers = 25
	entries := make([]queue.Entry, buyers)
	for i := range entries {
		entries[i] = join(t, q, int64(100+i))
	}

	for tick, wantAdmitted := range []int{10, 20, 25} {
		n, err := w.Tick(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if want := min(10, buyers-tick*10); n != want {
			t.Errorf("tick %d admitted %d, want %d", tick+1, n, want)
		}
		for i, e := range entries {
			st := status(t, q, e.QueueID)
			if i < wantAdmitted {
				adm, err := tokens.Verify(st.Token)
				if err != nil || adm.UserID != int64(100+i) || adm.EventID != eventID || adm.QueueID != e.QueueID {
					t.Errorf("after tick %d, buyer %d: admission %+v, %v", tick+1, i, adm, err)
				}
				if st.ExpiresIn <= 0 || st.ExpiresIn > 5*time.Minute {
					t.Errorf("buyer %d: token expires in %v", i, st.ExpiresIn)
				}
				continue
			}
			if st.Token != "" || st.Position != int64(i-wantAdmitted+1) {
				t.Errorf("after tick %d, buyer %d = %+v, want still waiting at %d", tick+1, i, st, i-wantAdmitted+1)
			}
		}
	}
	if n, err := q.Length(context.Background(), eventID); err != nil || n != 0 {
		t.Errorf("queue length = %d, %v; want 0", n, err)
	}
}

// Joining again after being admitted returns the same entry; after the
// admission runs out, the buyer joins again at the back.
func TestJoinAfterAdmission(t *testing.T) {
	rdb := containers.Redis.New(t)
	q := queue.New(rdb)
	w := newWorker(rdb, 1)

	first := join(t, q, 1)
	if _, err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	join(t, q, 2)
	if again := join(t, q, 1); again.QueueID != first.QueueID || again.Position != 0 {
		t.Errorf("join while admitted = %+v, want the same id at position 0", again)
	}

	rdb.Del(context.Background(), queue.AdmittedKey(first.QueueID)) // the admission ran out
	back := join(t, q, 1)
	if back.QueueID == first.QueueID || back.Position != 2 {
		t.Errorf("join after the admission ran out = %+v, want a new id behind user 2", back)
	}
}

// SPEC.md 7.2: only one instance issues tokens at a time (admit:lock:{eid}),
// and a stopped holder is replaced once its lease runs out.
func TestOneInstanceAdmitsAtATime(t *testing.T) {
	rdb := containers.Redis.New(t)
	q := queue.New(rdb)
	a, b := newWorker(rdb, 2), newWorker(rdb, 2)
	for i := range 6 {
		join(t, q, int64(i+1))
	}

	na, err := a.Admit(context.Background(), eventID)
	if err != nil {
		t.Fatal(err)
	}
	nb, err := b.Admit(context.Background(), eventID)
	if err != nil {
		t.Fatal(err)
	}
	if na != 2 || nb != 0 {
		t.Fatalf("first tick: a admitted %d, b %d; want 2 and 0", na, nb)
	}
	// The holder renews its lease every tick.
	if na, _ = a.Admit(context.Background(), eventID); na != 2 {
		t.Errorf("holder's second tick admitted %d, want 2", na)
	}

	// a stops; its lease runs out after 3 s and b takes over.
	time.Sleep(3100 * time.Millisecond)
	if nb, err = b.Admit(context.Background(), eventID); err != nil || nb != 2 {
		t.Errorf("after the lease ran out b admitted %d, %v; want 2", nb, err)
	}
	if na, _ = a.Admit(context.Background(), eventID); na != 0 {
		t.Errorf("old holder admitted %d after losing the lease, want 0", na)
	}
}

// A crash between storing tokens and removing the buyers from the queue
// admits them again on the next tick, with the token they already have.
func TestAdmitKeepsTheFirstToken(t *testing.T) {
	rdb := containers.Redis.New(t)
	q := queue.New(rdb)
	w := newWorker(rdb, 5)
	e := join(t, q, 1)
	if _, err := w.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := status(t, q, e.QueueID).Token

	// Put the buyer back as if the removal never happened.
	ctx := context.Background()
	rdb.ZAdd(ctx, "queue:{1}", redis.Z{Score: 1, Member: e.QueueID.String()})
	rdb.HSet(ctx, "queue:{1}:owner", e.QueueID.String(), "1")
	if _, err := w.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := status(t, q, e.QueueID).Token; got != first {
		t.Error("second admission replaced the buyer's token")
	}
}

func TestLedger(t *testing.T) {
	ctx := context.Background()
	rdb := containers.Redis.New(t)
	l := queue.NewLedger(rdb, time.Minute)
	jti, a, b := uuid.New(), uuid.New(), uuid.New()

	if ok, err := l.Claim(ctx, jti, a); err != nil || !ok {
		t.Fatalf("first claim = %v, %v", ok, err)
	}
	if ok, _ := l.Claim(ctx, jti, a); !ok {
		t.Error("claiming again for the same order failed")
	}
	if ok, _ := l.Claim(ctx, jti, b); ok {
		t.Error("another order claimed a used token")
	}
	if err := l.Unclaim(ctx, jti, b); err != nil {
		t.Fatal(err)
	}
	if ok, _ := l.Claim(ctx, jti, b); ok {
		t.Error("an unclaim by another order freed the token")
	}
	if ttl := rdb.PTTL(ctx, queue.UsedKey(jti)).Val(); ttl <= 0 || ttl > time.Minute {
		t.Errorf("claim TTL = %v, want up to 1m", ttl)
	}
	if err := l.Unclaim(ctx, jti, a); err != nil {
		t.Fatal(err)
	}
	if ok, _ := l.Claim(ctx, jti, b); !ok {
		t.Error("token not free after its order gave it back")
	}
}

// Ten joins of one user at once (double clicks, several tabs) give one place.
func TestJoinConcurrently(t *testing.T) {
	rdb := containers.Redis.New(t)
	q := queue.New(rdb)
	results := make(chan queue.Entry, 10)
	for range 10 {
		go func() {
			e, err := q.Join(context.Background(), eventID, 7)
			if err != nil {
				t.Error(err)
			}
			results <- e
		}()
	}
	first := <-results
	for range 9 {
		if e := <-results; e.QueueID != first.QueueID {
			t.Errorf("concurrent joins got %s and %s", e.QueueID, first.QueueID)
		}
	}
	if n, _ := q.Length(context.Background(), eventID); n != 1 {
		t.Errorf("queue length = %d, want 1", n)
	}
}

// BenchmarkAdmit measures one worker tick admitting 200 buyers (the default
// ADMIT_BATCH): reading the queue head, signing 200 tokens and storing them.
// Run: go test -tags=integration -run '^$' -bench Admit ./internal/queue
func BenchmarkAdmit(b *testing.B) {
	const batch = 200
	rdb := containers.Redis.New(b)
	q := queue.New(rdb)
	w := newWorker(rdb, batch)
	ctx := context.Background()
	for i := 0; b.Loop(); i++ {
		b.StopTimer()
		for u := range batch {
			if _, err := q.Join(ctx, eventID, int64(i*batch+u+1)); err != nil {
				b.Fatal(err)
			}
		}
		b.StartTimer()
		n, err := w.Admit(ctx, eventID)
		if err != nil || n != batch {
			b.Fatalf("admitted %d, %v; want %d", n, err, batch)
		}
	}
}
