// Package redisx creates the go-redis client shared by a process.
package redisx

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"

	"github.com/huy205-dev/ticketrush/internal/platform/timing"
)

// NewClient returns a client for addr. Like the pgx pool it connects lazily,
// so an unreachable Redis shows up in /readyz rather than at startup.
func NewClient(addr string) *redis.Client {
	rdb := redis.NewClient(&redis.Options{
		Addr: addr,
		// Maintenance notifications are a Redis Enterprise/Cloud feature. In
		// the default "auto" mode every new connection sends
		// CLIENT MAINT_NOTIFICATIONS and falls back when the server rejects
		// it; plain Redis 7 always rejects it, so skip that round trip.
		MaintNotificationsConfig: &maintnotifications.Config{
			Mode: maintnotifications.ModeDisabled,
		},
	})
	rdb.AddHook(timingHook{})
	return rdb
}

// timingHook reports the time of each command and pipeline, including the
// wait for a pooled connection, to the request's timing.Stats.
type timingHook struct{}

func (timingHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (timingHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmd)
		timing.AddRedis(ctx, time.Since(start))
		return err
	}
}

func (timingHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		start := time.Now()
		err := next(ctx, cmds)
		timing.AddRedis(ctx, time.Since(start))
		return err
	}
}
