package postgres

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/huy205-dev/ticketrush/internal/platform/timing"
)

// tracer reports connection waits and statement durations to the request's
// timing.Stats. pgxpool calls the acquire methods; pgx calls the query ones.
type tracer struct{}

type acquireStartKey struct{}
type queryStartKey struct{}

var (
	_ pgxpool.AcquireTracer = tracer{}
	_ pgx.QueryTracer       = tracer{}
)

func (tracer) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	return context.WithValue(ctx, acquireStartKey{}, time.Now())
}

func (tracer) TraceAcquireEnd(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireEndData) {
	if start, ok := ctx.Value(acquireStartKey{}).(time.Time); ok {
		timing.AddDBAcquire(ctx, time.Since(start))
	}
}

func (tracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, queryStartKey{}, time.Now())
}

func (tracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if start, ok := ctx.Value(queryStartKey{}).(time.Time); ok {
		timing.AddDBQuery(ctx, time.Since(start))
	}
}

// LogStats logs the pool's activity every interval until ctx is cancelled:
// how many acquires happened, how many found the pool empty and had to wait,
// and the total time they waited. Quiet intervals are not logged.
func LogStats(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger, interval time.Duration) {
	prev := pool.Stat()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s := pool.Stat()
		acquires := s.AcquireCount() - prev.AcquireCount()
		if acquires > 0 {
			logger.LogAttrs(ctx, slog.LevelInfo, "pgxpool stats",
				slog.Int64("acquires", acquires),
				slog.Int64("empty_acquires", s.EmptyAcquireCount()-prev.EmptyAcquireCount()),
				slog.Float64("empty_acquire_wait_ms", float64((s.EmptyAcquireWaitTime()-prev.EmptyAcquireWaitTime()).Microseconds())/1000),
				slog.Int("acquired_conns", int(s.AcquiredConns())),
				slog.Int("idle_conns", int(s.IdleConns())),
				slog.Int("max_conns", int(s.MaxConns())),
			)
		}
		prev = s
	}
}
