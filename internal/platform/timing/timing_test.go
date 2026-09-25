package timing

import (
	"context"
	"testing"
	"time"
)

func TestStats(t *testing.T) {
	ctx, s := With(context.Background())
	AddDBAcquire(ctx, 2*time.Millisecond)
	AddDBAcquire(ctx, 1500*time.Microsecond)
	AddDBQuery(ctx, 700*time.Microsecond)
	AddRedis(ctx, 300*time.Microsecond)

	got := map[string]any{}
	for _, a := range s.LogAttrs() {
		got[a.Key] = a.Value.Any()
	}
	want := map[string]any{
		"db_acquire_ms": 3.5, "db_acquires": int64(2),
		"db_query_ms": 0.7, "db_queries": int64(1),
		"redis_ms": 0.3, "redis_cmds": int64(1),
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

func TestNoStatsInContextIsANoOp(t *testing.T) {
	AddDBAcquire(context.Background(), time.Second) // must not panic
	_, s := With(context.Background())
	if attrs := s.LogAttrs(); len(attrs) != 0 {
		t.Errorf("unused stats produced attrs %v", attrs)
	}
}
