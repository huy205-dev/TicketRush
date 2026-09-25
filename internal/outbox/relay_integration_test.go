//go:build integration

package outbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/huy205-dev/ticketrush/internal/catalog"
	"github.com/huy205-dev/ticketrush/internal/events"
	"github.com/huy205-dev/ticketrush/internal/outbox"
	"github.com/huy205-dev/ticketrush/internal/platform/kafka"
	"github.com/huy205-dev/ticketrush/internal/platform/testenv"
)

var containers testenv.Env

func TestMain(m *testing.M) {
	testenv.Main(m, testenv.Needs{Postgres: true, Kafka: true}, &containers)
}

var topicSeq atomic.Int64

// newTopic creates a topic private to the test so tests do not see each
// other's messages.
func newTopic(t *testing.T) string {
	t.Helper()
	topic := fmt.Sprintf("test.relay.%d.%d", time.Now().UnixNano(), topicSeq.Add(1))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := kafka.EnsureTopics(ctx, containers.Kafka.Client(t), map[string]int32{topic: 3}); err != nil {
		t.Fatal(err)
	}
	return topic
}

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := containers.DB.New(t, 20)
	// outbox rows need no other data, but seed keeps the schema realistic.
	if _, err := catalog.Seed(context.Background(), pool, catalog.DemoEvent()); err != nil {
		t.Fatal(err)
	}
	return pool
}

type message struct{ topic, key, eventType string }

// write inserts messages in one transaction and returns their outbox ids.
func write(t *testing.T, pool *pgxpool.Pool, msgs ...message) []int64 {
	t.Helper()
	var ids []int64
	err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		for _, m := range msgs {
			id, err := outbox.Write(context.Background(), tx, outbox.Message{
				Topic: m.topic, Key: m.key, EventType: m.eventType,
				Payload: map[string]any{"order_id": m.key},
			})
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func unpublished(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM outbox WHERE published_at IS NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// consume reads want records from the start of topic.
func consume(t *testing.T, topic string, want int) []*kgo.Record {
	t.Helper()
	cl := containers.Kafka.Client(t, kgo.ConsumeTopics(topic), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var got []*kgo.Record
	for len(got) < want {
		fetches := cl.PollFetches(ctx)
		if ctx.Err() != nil {
			t.Fatalf("got %d of %d records before timeout", len(got), want)
		}
		fetches.EachRecord(func(r *kgo.Record) { got = append(got, r) })
	}
	return got
}

func header(r *kgo.Record, key string) string {
	for _, h := range r.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}

// SPEC.md 9.5: rows are produced with the order id as key and the event
// type and outbox id as headers, marked published, and not sent twice.
func TestRelayPublishes(t *testing.T) {
	pool := newPool(t)
	topic := newTopic(t)
	ids := write(t, pool,
		message{topic, "order-a", "order.held"},
		message{topic, "order-b", "order.held"},
		message{topic, "order-a", "order.cancelled"},
	)

	relay := outbox.NewRelay(pool, containers.Kafka.Client(t), slog.New(slog.DiscardHandler))
	if n, err := relay.RunOnce(context.Background()); err != nil || n != 3 {
		t.Fatalf("RunOnce = %d, %v; want 3", n, err)
	}
	if n := unpublished(t, pool); n != 0 {
		t.Errorf("%d rows still unpublished", n)
	}
	if n, err := relay.RunOnce(context.Background()); err != nil || n != 0 {
		t.Errorf("second RunOnce = %d, %v; want nothing to do", n, err)
	}

	records := consume(t, topic, 3)
	if len(records) != 3 {
		t.Fatalf("got %d records, want exactly 3", len(records))
	}
	var orderA []*kgo.Record
	for _, r := range records {
		id, _ := strconv.ParseInt(header(r, events.HeaderEventID), 10, 64)
		var payload struct {
			EventID   int64  `json:"event_id"`
			EventType string `json:"event_type"`
		}
		if err := json.Unmarshal(r.Value, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.EventID != id || payload.EventType != header(r, events.HeaderEventType) {
			t.Errorf("record %s: headers (%d, %s) disagree with payload %+v", r.Key, id, header(r, events.HeaderEventType), payload)
		}
		if string(r.Key) == "order-a" {
			orderA = append(orderA, r)
		}
	}
	// Same key, same partition, and the order of the outbox is kept.
	if len(orderA) != 2 || orderA[0].Partition != orderA[1].Partition ||
		header(orderA[0], events.HeaderEventType) != "order.held" || header(orderA[1], events.HeaderEventType) != "order.cancelled" {
		t.Errorf("order-a records out of order or split across partitions")
	}
	if header(orderA[0], events.HeaderEventID) != strconv.FormatInt(ids[0], 10) {
		t.Errorf("first order-a record has event_id %s, want %d", header(orderA[0], events.HeaderEventID), ids[0])
	}
}

// failingProducer rejects every record, like an unreachable Kafka.
type failingProducer struct{}

func (failingProducer) ProduceSync(_ context.Context, rs ...*kgo.Record) kgo.ProduceResults {
	out := make(kgo.ProduceResults, len(rs))
	for i, r := range rs {
		out[i] = kgo.ProduceResult{Record: r, Err: errors.New("broker unavailable")}
	}
	return out
}

// A failed produce leaves the rows unpublished for the next attempt.
func TestRelayKeepsRowsWhenProduceFails(t *testing.T) {
	pool := newPool(t)
	topic := newTopic(t)
	write(t, pool, message{topic, "order-a", "order.held"})

	if _, err := outbox.NewRelay(pool, failingProducer{}, slog.New(slog.DiscardHandler)).RunOnce(context.Background()); err == nil {
		t.Fatal("RunOnce succeeded with a failing producer")
	}
	if n := unpublished(t, pool); n != 1 {
		t.Fatalf("%d rows unpublished after the failure, want 1", n)
	}

	relay := outbox.NewRelay(pool, containers.Kafka.Client(t), slog.New(slog.DiscardHandler))
	if n, err := relay.RunOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("retry = %d, %v; want the row published", n, err)
	}
	if records := consume(t, topic, 1); string(records[0].Key) != "order-a" {
		t.Errorf("unexpected record %q", records[0].Key)
	}
}

// countingProducer acknowledges every record and remembers the event ids.
type countingProducer struct {
	mu   sync.Mutex
	seen map[string]int
}

func (p *countingProducer) ProduceSync(_ context.Context, rs ...*kgo.Record) kgo.ProduceResults {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(kgo.ProduceResults, len(rs))
	for i, r := range rs {
		p.seen[header(r, events.HeaderEventID)]++
		out[i] = kgo.ProduceResult{Record: r}
	}
	return out
}

// Several relays at once must hand each row to exactly one of them.
func TestRelaysInParallel(t *testing.T) {
	pool := newPool(t)
	const rows = 1_200 // more than two batches
	msgs := make([]message, rows)
	for i := range msgs {
		msgs[i] = message{"unused", fmt.Sprintf("order-%d", i%50), "order.held"}
	}
	write(t, pool, msgs...)

	p := &countingProducer{seen: map[string]int{}}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 3 {
		relay := outbox.NewRelay(pool, p, slog.New(slog.DiscardHandler))
		wg.Go(func() {
			<-start
			for {
				n, err := relay.RunOnce(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 {
					return
				}
			}
		})
	}
	close(start)
	wg.Wait()

	dups := 0
	for _, c := range p.seen {
		if c != 1 {
			dups++
		}
	}
	if len(p.seen) != rows || dups != 0 || unpublished(t, pool) != 0 {
		t.Errorf("produced %d distinct rows (%d more than once), %d left; want %d once each", len(p.seen), dups, unpublished(t, pool), rows)
	}
}

// Cleanup removes rows published more than 7 days ago and nothing else.
func TestRelayCleanup(t *testing.T) {
	pool := newPool(t)
	ids := write(t, pool,
		message{"t", "a", "order.held"}, // published 8 days ago
		message{"t", "b", "order.held"}, // published 6 days ago
		message{"t", "c", "order.held"}, // not published
	)
	ctx := context.Background()
	for id, age := range map[int64]string{ids[0]: "8 days", ids[1]: "6 days"} {
		if _, err := pool.Exec(ctx, `UPDATE outbox SET published_at = now() - $2::interval WHERE id = $1`, id, age); err != nil {
			t.Fatal(err)
		}
	}
	n, err := outbox.NewRelay(pool, failingProducer{}, slog.New(slog.DiscardHandler)).Cleanup(ctx)
	if err != nil || n != 1 {
		t.Fatalf("Cleanup = %d, %v; want 1", n, err)
	}
	var left []int64
	rows, _ := pool.Query(ctx, `SELECT id FROM outbox ORDER BY id`)
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		left = append(left, id)
	}
	if len(left) != 2 || left[0] != ids[1] || left[1] != ids[2] {
		t.Errorf("rows left = %v, want %v", left, ids[1:])
	}
}
