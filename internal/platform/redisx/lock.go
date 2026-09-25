package redisx

import (
	"context"
	"crypto/rand"
	_ "embed"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

//go:embed unlock.lua
var unlockLua string

// Locker hands out short-lived exclusive locks stored in Redis. A lock is a
// key holding a random token; it expires on its own if the holder dies.
//
// The lock is advisory: it decides which of several concurrent requests does
// the work, while correctness still rests on PostgreSQL constraints.
type Locker struct {
	rdb    redis.UniversalClient
	unlock *redis.Script
}

// NewLocker returns a Locker using rdb.
func NewLocker(rdb redis.UniversalClient) *Locker {
	return &Locker{rdb: rdb, unlock: redis.NewScript(unlockLua)}
}

// TryLock takes key for ttl if nobody holds it. ok is false when the lock is
// held by someone else; err is set only when Redis could not be asked.
// The returned unlock releases the lock if it is still ours.
func (l *Locker) TryLock(ctx context.Context, key string, ttl time.Duration) (unlock func(context.Context) error, ok bool, err error) {
	token := rand.Text()
	ok, err = l.rdb.SetNX(ctx, key, token, ttl).Result()
	if err != nil {
		return nil, false, fmt.Errorf("lock %s: %w", key, err)
	}
	if !ok {
		return nil, false, nil
	}
	return func(ctx context.Context) error {
		if err := l.unlock.Run(ctx, l.rdb, []string{key}, token).Err(); err != nil {
			return fmt.Errorf("unlock %s: %w", key, err)
		}
		return nil
	}, true, nil
}
