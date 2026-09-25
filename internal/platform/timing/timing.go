// Package timing records, per request, the time spent waiting for a
// PostgreSQL connection, running PostgreSQL queries and talking to Redis, so
// that access logs can show where a request's latency comes from.
//
// The postgres and redisx packages feed it through a pgx tracer and a
// go-redis hook; httpx.AccessLog attaches a Stats to each request and logs
// it. Outside a request (workers, tests) nothing is recorded.
package timing

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// Stats accumulates the time one request spent in each dependency. It is
// safe for concurrent use.
type Stats struct {
	dbAcquire, dbAcquires atomic.Int64
	dbQuery, dbQueries    atomic.Int64
	redis, redisCmds      atomic.Int64
}

type ctxKey struct{}

// With returns a context carrying a fresh Stats, and that Stats.
func With(ctx context.Context) (context.Context, *Stats) {
	s := &Stats{}
	return context.WithValue(ctx, ctxKey{}, s), s
}

func from(ctx context.Context) *Stats {
	s, _ := ctx.Value(ctxKey{}).(*Stats)
	return s
}

// AddDBAcquire records the time spent waiting for a pool connection.
func AddDBAcquire(ctx context.Context, d time.Duration) {
	if s := from(ctx); s != nil {
		s.dbAcquire.Add(int64(d))
		s.dbAcquires.Add(1)
	}
}

// AddDBQuery records one PostgreSQL statement (including BEGIN and COMMIT).
func AddDBQuery(ctx context.Context, d time.Duration) {
	if s := from(ctx); s != nil {
		s.dbQuery.Add(int64(d))
		s.dbQueries.Add(1)
	}
}

// AddRedis records one Redis command or pipeline.
func AddRedis(ctx context.Context, d time.Duration) {
	if s := from(ctx); s != nil {
		s.redis.Add(int64(d))
		s.redisCmds.Add(1)
	}
}

func ms(ns int64) float64 { return float64(ns/1000) / 1000 }

// LogAttrs returns the recorded totals as log attributes, omitting the
// dependencies the request did not use.
func (s *Stats) LogAttrs() []slog.Attr {
	var attrs []slog.Attr
	if n := s.dbAcquires.Load(); n > 0 {
		attrs = append(attrs, slog.Float64("db_acquire_ms", ms(s.dbAcquire.Load())), slog.Int64("db_acquires", n))
	}
	if n := s.dbQueries.Load(); n > 0 {
		attrs = append(attrs, slog.Float64("db_query_ms", ms(s.dbQuery.Load())), slog.Int64("db_queries", n))
	}
	if n := s.redisCmds.Load(); n > 0 {
		attrs = append(attrs, slog.Float64("redis_ms", ms(s.redis.Load())), slog.Int64("redis_cmds", n))
	}
	return attrs
}
