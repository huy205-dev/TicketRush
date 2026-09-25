package inventory

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// statusBatchSize bounds each MGET issued by Status (SPEC.md 7.1).
const statusBatchSize = 500

var (
	//go:embed lua/hold.lua
	holdLua string
	//go:embed lua/release.lua
	releaseLua string
	//go:embed lua/confirm.lua
	confirmLua string
)

// Redis keeps seat state in Redis (SPEC.md section 6): no key means
// available, "held:<orderId>" expires with the hold, "sold:<orderId>" stays.
// Each operation is one Lua script, so checking and writing all the seats of
// a request is atomic without any lock.
//
// Redis is not the source of truth: it can be rebuilt from PostgreSQL
// (reconcile, M8), and the tickets UNIQUE constraint still guards payment.
type Redis struct {
	rdb     redis.UniversalClient
	hold    *redis.Script
	release *redis.Script
	confirm *redis.Script
	logger  *slog.Logger
}

// NewRedis returns the Redis backend. rdb may be a single node or a cluster
// client.
func NewRedis(rdb redis.UniversalClient, logger *slog.Logger) *Redis {
	return &Redis{
		rdb:     rdb,
		hold:    redis.NewScript(holdLua),
		release: redis.NewScript(releaseLua),
		confirm: redis.NewScript(confirmLua),
		logger:  logger,
	}
}

var (
	_ Inventory = (*Redis)(nil)
	_ Versioner = (*Redis)(nil)
)

// seatKeyPrefix is "seat:{<eventId>}:". The braces are a Redis Cluster hash
// tag: every key of an event lands in the same slot, which multi-key scripts
// require.
func seatKeyPrefix(eventID int64) string {
	return "seat:{" + strconv.FormatInt(eventID, 10) + "}:"
}

func seatKeys(eventID int64, seatIDs []string) []string {
	prefix := seatKeyPrefix(eventID)
	keys := make([]string, len(seatIDs))
	for i, id := range seatIDs {
		keys[i] = prefix + id
	}
	return keys
}

func versionKey(eventID int64) string {
	return "seatmap:{" + strconv.FormatInt(eventID, 10) + "}:version"
}

// Hold implements Inventory with hold.lua.
func (r *Redis) Hold(ctx context.Context, eventID int64, seatIDs []string, orderID uuid.UUID, ttl time.Duration) ([]string, error) {
	ttlMs := max(ttl.Milliseconds(), 1) // PX 0 is an error
	res, err := r.hold.Run(ctx, r.rdb, seatKeys(eventID, seatIDs), orderID.String(), ttlMs).Slice()
	if err != nil {
		return nil, fmt.Errorf("redis hold: %w", err)
	}
	if ok, _ := res[0].(int64); ok == 1 {
		r.bumpVersion(ctx, eventID)
		return nil, nil
	}
	prefix := seatKeyPrefix(eventID)
	taken := make([]string, 0, len(res)-1)
	for _, k := range res[1:] {
		key, _ := k.(string)
		taken = append(taken, strings.TrimPrefix(key, prefix))
	}
	return taken, nil
}

// Release implements Inventory with release.lua.
func (r *Redis) Release(ctx context.Context, eventID int64, seatIDs []string, orderID uuid.UUID) error {
	n, err := r.release.Run(ctx, r.rdb, seatKeys(eventID, seatIDs), orderID.String()).Int64()
	if err != nil {
		return fmt.Errorf("redis release: %w", err)
	}
	if n > 0 {
		r.bumpVersion(ctx, eventID)
	}
	return nil
}

// Confirm implements Inventory with confirm.lua.
func (r *Redis) Confirm(ctx context.Context, eventID int64, seatIDs []string, orderID uuid.UUID) error {
	res, err := r.confirm.Run(ctx, r.rdb, seatKeys(eventID, seatIDs), orderID.String()).Slice()
	if err != nil {
		return fmt.Errorf("redis confirm: %w", err)
	}
	if ok, _ := res[0].(int64); ok != 1 {
		return fmt.Errorf("redis confirm: %w: %v is %v", ErrConfirmMismatch, res[1], res[2])
	}
	r.bumpVersion(ctx, eventID)
	return nil
}

// Status implements Inventory with MGET in batches of 500, sent as one
// pipeline.
func (r *Redis) Status(ctx context.Context, eventID int64, seatIDs []string) (map[string]SeatStatus, error) {
	pipe := r.rdb.Pipeline()
	keys := seatKeys(eventID, seatIDs)
	var cmds []*redis.SliceCmd
	for start := 0; start < len(keys); start += statusBatchSize {
		end := min(start+statusBatchSize, len(keys))
		cmds = append(cmds, pipe.MGet(ctx, keys[start:end]...))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, fmt.Errorf("redis status: %w", err)
	}

	out := make(map[string]SeatStatus, len(seatIDs))
	i := 0
	for _, cmd := range cmds {
		for _, v := range cmd.Val() {
			out[seatIDs[i]] = statusOf(v)
			i++
		}
	}
	return out, nil
}

func statusOf(v any) SeatStatus {
	s, _ := v.(string)
	switch {
	case strings.HasPrefix(s, "sold:"):
		return Sold
	case strings.HasPrefix(s, "held:"):
		return Held
	default:
		return Available
	}
}

// SeatMapVersion implements Versioner.
func (r *Redis) SeatMapVersion(ctx context.Context, eventID int64) (int64, error) {
	v, err := r.rdb.Get(ctx, versionKey(eventID)).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("redis seat map version: %w", err)
	}
	return v, nil
}

// bumpVersion increments the seat map version after a seat changed state
// (SPEC.md 6.1, 9.1 step 10). The version only tells clients the map
// changed, so a failure is logged rather than failing the operation that
// already succeeded.
func (r *Redis) bumpVersion(ctx context.Context, eventID int64) {
	if err := r.rdb.Incr(ctx, versionKey(eventID)).Err(); err != nil {
		r.logger.WarnContext(ctx, "bump seat map version failed", "event_id", eventID, "err", err)
	}
}
