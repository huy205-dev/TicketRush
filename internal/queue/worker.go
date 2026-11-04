package queue

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/huy205-dev/ticketrush/internal/auth"
)

// leaseTTL is how long an instance keeps an event's token-issuing lease
// without renewing it (SPEC.md 7.2: SET admit:lock:{eid} ... NX PX 3000).
const leaseTTL = 3 * time.Second

// Issuer signs admission tokens (auth.AdmissionTokens).
type Issuer interface {
	Issue(userID, eventID int64, queueID uuid.UUID) (string, auth.Admission, error)
	TTL() time.Duration
}

// Events lists the events whose sale is open (catalog.Service).
type Events interface {
	OpenEventIDs(ctx context.Context) ([]int64, error)
}

// Worker admits waiting buyers: every interval, for each open event whose
// lease it holds, it issues tokens to the first batch buyers in join order.
type Worker struct {
	q        *Queue
	tokens   Issuer
	events   Events
	batch    int
	instance string
	logger   *slog.Logger
}

// NewWorker admits up to batch buyers per event per tick (ADMIT_BATCH).
func NewWorker(q *Queue, tokens Issuer, events Events, batch int, logger *slog.Logger) *Worker {
	return &Worker{q: q, tokens: tokens, events: events, batch: batch, instance: rand.Text(), logger: logger}
}

// Run admits buyers every interval (ADMIT_INTERVAL) until ctx is cancelled.
func (w *Worker) Run(ctx context.Context, interval time.Duration) error {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := w.Tick(ctx); err != nil && ctx.Err() == nil {
			w.logger.ErrorContext(ctx, "admission tick failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Tick admits one batch for every open event this instance holds the lease
// of, and returns how many buyers it admitted.
func (w *Worker) Tick(ctx context.Context) (int, error) {
	ids, err := w.events.OpenEventIDs(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, eventID := range ids {
		n, err := w.Admit(ctx, eventID)
		if err != nil {
			return total, fmt.Errorf("admit event %d: %w", eventID, err)
		}
		if n > 0 {
			w.logger.InfoContext(ctx, "buyers admitted", "event_id", eventID, "count", n)
		}
		total += n
	}
	return total, nil
}

// Admit issues tokens to the first batch buyers of eventID, in join order,
// if this instance holds (or can take) the event's lease.
//
// SPEC.md 7.2 says ZPOPMIN; this reads the head with ZRANGE and removes it
// only after the tokens are stored, so a crash in between admits the same
// buyers again on the next tick instead of losing them. SET NX keeps the
// first token, so nobody gets two.
func (w *Worker) Admit(ctx context.Context, eventID int64) (int, error) {
	held, err := w.q.lease.Run(ctx, w.q.rdb, []string{lockKey(eventID)}, w.instance, leaseTTL.Milliseconds()).Int()
	if err != nil {
		return 0, fmt.Errorf("take admission lease: %w", err)
	}
	if held != 1 {
		return 0, nil
	}

	head, err := w.q.rdb.ZRange(ctx, queueKey(eventID), 0, int64(w.batch)-1).Result()
	if err != nil {
		return 0, fmt.Errorf("read queue head: %w", err)
	}
	if len(head) == 0 {
		return 0, nil
	}
	owners, err := w.q.rdb.HMGet(ctx, ownerKey(eventID), head...).Result()
	if err != nil {
		return 0, fmt.Errorf("read queue owners: %w", err)
	}

	ttl := w.tokens.TTL()
	admitted := 0
	members := make([]any, len(head))
	_, err = w.q.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i, member := range head {
			members[i] = member
			qid, err := uuid.Parse(member)
			uid, ok := owners[i].(string)
			if err != nil || !ok {
				w.logger.WarnContext(ctx, "dropping a queue entry without owner", "event_id", eventID, "queue_id", member)
				continue
			}
			userID, err := strconv.ParseInt(uid, 10, 64)
			if err != nil {
				w.logger.WarnContext(ctx, "dropping a queue entry with a bad owner", "event_id", eventID, "queue_id", member)
				continue
			}
			token, _, err := w.tokens.Issue(userID, eventID, qid)
			if err != nil {
				return err
			}
			p.SetNX(ctx, AdmittedKey(qid), token, ttl)
			admitted++
		}
		// Queued after the tokens, so a buyer leaves the queue only once
		// their token is stored.
		p.ZRem(ctx, queueKey(eventID), members...)
		p.HDel(ctx, ownerKey(eventID), head...)
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("store admissions: %w", err)
	}
	return admitted, nil
}
