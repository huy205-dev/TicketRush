// Package redisx creates the go-redis client shared by a process.
package redisx

import (
	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
)

// NewClient returns a client for addr. Like the pgx pool it connects lazily,
// so an unreachable Redis shows up in /readyz rather than at startup.
func NewClient(addr string) *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr: addr,
		// Maintenance notifications are a Redis Enterprise/Cloud feature. In
		// the default "auto" mode every new connection sends
		// CLIENT MAINT_NOTIFICATIONS and falls back when the server rejects
		// it; plain Redis 7 always rejects it, so skip that round trip.
		MaintNotificationsConfig: &maintnotifications.Config{
			Mode: maintnotifications.ModeDisabled,
		},
	})
}
