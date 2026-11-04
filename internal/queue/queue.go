package queue

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

var (
	//go:embed lua/join.lua
	joinLua string
	//go:embed lua/lease.lua
	leaseLua string
)

// ErrNotQueued means a queue id is neither waiting nor admitted: it never
// existed, or its admission ran out. The buyer has to join again.
var ErrNotQueued = errors.New("not in the queue")

// userKeyTTL is how long a user keeps their place (SPEC.md 6.1).
const userKeyTTL = 2 * time.Hour

// Redis keys of one event. The {eventId} hash tag keeps them in one Redis
// Cluster slot, so the scripts can use them together.
func queueKey(eventID int64) string { return "queue:{" + strconv.FormatInt(eventID, 10) + "}" }
func userKey(eventID, userID int64) string {
	return queueKey(eventID) + ":user:" + strconv.FormatInt(userID, 10)
}
func ownerKey(eventID int64) string { return queueKey(eventID) + ":owner" }
func lockKey(eventID int64) string  { return "admit:lock:{" + strconv.FormatInt(eventID, 10) + "}" }

// AdmittedKey holds the admission token of a queue id (SPEC.md 6.1).
func AdmittedKey(queueID uuid.UUID) string { return "admitted:" + queueID.String() }

// Queue is the Redis side of the waiting room.
type Queue struct {
	rdb   redis.UniversalClient
	join  *redis.Script
	lease *redis.Script
}

// New returns a queue stored in rdb.
func New(rdb redis.UniversalClient) *Queue {
	return &Queue{rdb: rdb, join: redis.NewScript(joinLua), lease: redis.NewScript(leaseLua)}
}

// Entry is a buyer's place: Position is 1-based while waiting and 0 once
// admitted.
type Entry struct {
	QueueID  uuid.UUID
	Position int64
}

// Join puts userID in eventID's queue. Joining again while waiting or
// admitted returns the same queue id; a user whose admission ran out joins
// again at the back with a new one.
func (q *Queue) Join(ctx context.Context, eventID, userID int64) (Entry, error) {
	spent := ""
	cur, err := q.rdb.Get(ctx, userKey(eventID, userID)).Result()
	switch {
	case errors.Is(err, redis.Nil):
	case err != nil:
		return Entry{}, fmt.Errorf("read queue entry: %w", err)
	default:
		if id, perr := uuid.Parse(cur); perr == nil {
			st, err := q.Status(ctx, eventID, id)
			if err == nil {
				return Entry{QueueID: id, Position: st.Position}, nil
			}
			if !errors.Is(err, ErrNotQueued) {
				return Entry{}, err
			}
		}
		spent = cur
	}

	// UUIDv7: entries that join in the same millisecond (one score) are
	// ordered by id, which follows creation order within an instance.
	id, err := uuid.NewV7()
	if err != nil {
		return Entry{}, fmt.Errorf("generate queue id: %w", err)
	}
	res, err := q.join.Run(ctx, q.rdb,
		[]string{queueKey(eventID), userKey(eventID, userID), ownerKey(eventID)},
		id.String(), userID, int64(userKeyTTL.Seconds()), spent).Slice()
	if err != nil {
		return Entry{}, fmt.Errorf("join queue: %w", err)
	}
	got, err := uuid.Parse(fmt.Sprint(res[0]))
	if err != nil {
		return Entry{}, fmt.Errorf("join queue: bad queue id %v", res[0])
	}
	rank, _ := res[1].(int64)
	return Entry{QueueID: got, Position: rank + 1}, nil
}

// Status is where a queue id stands.
type Status struct {
	// Position is 1-based while waiting, 0 once admitted.
	Position int64
	// Token and ExpiresIn are set once admitted.
	Token     string
	ExpiresIn time.Duration
}

// Status reports a queue id's place or its admission. It returns
// ErrNotQueued when the id is neither waiting nor admitted.
func (q *Queue) Status(ctx context.Context, eventID int64, queueID uuid.UUID) (Status, error) {
	var token *redis.StringCmd
	var ttl *redis.DurationCmd
	var rank *redis.IntCmd
	_, err := q.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		token = p.Get(ctx, AdmittedKey(queueID))
		ttl = p.PTTL(ctx, AdmittedKey(queueID))
		rank = p.ZRank(ctx, queueKey(eventID), queueID.String())
		return nil
	})
	if err != nil && !errors.Is(err, redis.Nil) {
		return Status{}, fmt.Errorf("queue status: %w", err)
	}
	if t, err := token.Result(); err == nil {
		return Status{Token: t, ExpiresIn: max(ttl.Val(), 0)}, nil
	}
	if r, err := rank.Result(); err == nil {
		return Status{Position: r + 1}, nil
	}
	return Status{}, ErrNotQueued
}

// Length is how many buyers wait for eventID.
func (q *Queue) Length(ctx context.Context, eventID int64) (int64, error) {
	n, err := q.rdb.ZCard(ctx, queueKey(eventID)).Result()
	if err != nil {
		return 0, fmt.Errorf("queue length: %w", err)
	}
	return n, nil
}
