package outbox

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/huy205-dev/ticketrush/internal/events"
	"github.com/huy205-dev/ticketrush/internal/outbox/outboxdb"
)

// Relay settings from SPEC.md 9.5.
const (
	RelayBatchSize = 500
	idleWait       = 100 * time.Millisecond
	// errorWait slows the loop down while Kafka or PostgreSQL is failing.
	errorWait = time.Second
	// batchTimeout bounds one batch, which runs detached from the shutdown
	// signal so SIGTERM never interrupts a batch between produce and commit.
	batchTimeout = 30 * time.Second
)

// Retention of published rows before cleanup deletes them (SPEC.md 9.5).
const (
	Retention       = 7 * 24 * time.Hour
	cleanupEvery    = 10 * time.Minute
	cleanupMaxBatch = 10_000
)

// Producer is the part of *kgo.Client the relay needs.
type Producer interface {
	ProduceSync(ctx context.Context, rs ...*kgo.Record) kgo.ProduceResults
}

// Relay publishes outbox rows to Kafka (SPEC.md 9.5). Delivery is
// at-least-once: a crash between produce and commit publishes the batch
// again, and consumers deduplicate on the event_id header.
type Relay struct {
	pool     *pgxpool.Pool
	producer Producer
	logger   *slog.Logger
}

// NewRelay returns a relay producing through p.
func NewRelay(pool *pgxpool.Pool, p Producer, logger *slog.Logger) *Relay {
	return &Relay{pool: pool, producer: p, logger: logger}
}

// RunOnce publishes one batch and returns its size. The rows stay locked
// while producing; they are marked published only if every record was
// acknowledged, in the same transaction.
func (r *Relay) RunOnce(ctx context.Context) (int, error) {
	var n int
	err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		q := outboxdb.New(tx)
		rows, err := q.LockUnpublished(ctx, RelayBatchSize)
		if err != nil {
			return fmt.Errorf("lock outbox batch: %w", err)
		}
		if len(rows) == 0 {
			return nil
		}

		records := make([]*kgo.Record, len(rows))
		ids := make([]int64, len(rows))
		for i, row := range rows {
			ids[i] = row.ID
			records[i] = &kgo.Record{
				Topic: row.Topic,
				Key:   []byte(row.MsgKey),
				Value: row.Payload,
				Headers: []kgo.RecordHeader{
					{Key: events.HeaderEventType, Value: []byte(row.EventType)},
					{Key: events.HeaderEventID, Value: []byte(strconv.FormatInt(row.ID, 10))},
				},
			}
		}
		if err := r.producer.ProduceSync(ctx, records...).FirstErr(); err != nil {
			return fmt.Errorf("produce %d records: %w", len(records), err)
		}
		if err := q.MarkPublished(ctx, ids); err != nil {
			return fmt.Errorf("mark outbox published: %w", err)
		}
		n = len(rows)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// Run relays until ctx is cancelled: back to back while batches are full,
// every 100 ms when the outbox is empty, and every second after an error.
// Published rows older than Retention are deleted every ten minutes.
func (r *Relay) Run(ctx context.Context) error {
	nextCleanup := time.Now().Add(cleanupEvery)
	for {
		batchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), batchTimeout)
		n, err := r.RunOnce(batchCtx)
		cancel()

		wait := time.Duration(0)
		switch {
		case err != nil:
			r.logger.ErrorContext(ctx, "relay batch failed; retrying", "err", err)
			wait = errorWait
		case n > 0:
			r.logger.DebugContext(ctx, "outbox batch published", "count", n)
			if n < RelayBatchSize {
				wait = idleWait
			}
		default:
			wait = idleWait
		}

		if time.Now().After(nextCleanup) {
			if n, err := r.Cleanup(ctx); err != nil {
				r.logger.WarnContext(ctx, "outbox cleanup failed", "err", err)
			} else if n > 0 {
				r.logger.InfoContext(ctx, "old outbox rows deleted", "count", n)
			}
			nextCleanup = time.Now().Add(cleanupEvery)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

// Cleanup deletes rows published more than Retention ago, a bounded chunk
// at a time so the delete never holds many locks, and returns how many it
// deleted.
func (r *Relay) Cleanup(ctx context.Context) (int64, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), batchTimeout)
	defer cancel()
	q := outboxdb.New(r.pool)
	var total int64
	for {
		n, err := q.DeletePublishedBefore(ctx, outboxdb.DeletePublishedBeforeParams{Retention: Retention, BatchSize: cleanupMaxBatch})
		if err != nil {
			return total, fmt.Errorf("delete published outbox rows: %w", err)
		}
		total += n
		if n < cleanupMaxBatch {
			return total, nil
		}
	}
}
