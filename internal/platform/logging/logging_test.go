package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("log line is not JSON: %v: %s", err, buf.String())
	}
	return m
}

func TestContextAttrsAreLogged(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, slog.LevelInfo).With("service", "booking")

	ctx := With(context.Background(), slog.String("request_id", "req-1"))
	ctx = With(ctx, slog.String("order_id", "ord-1"))
	logger.InfoContext(ctx, "hello", "k", "v")

	m := decode(t, &buf)
	for key, want := range map[string]string{
		"msg":        "hello",
		"service":    "booking",
		"request_id": "req-1",
		"order_id":   "ord-1",
		"k":          "v",
	} {
		if m[key] != want {
			t.Errorf("%s = %v, want %q", key, m[key], want)
		}
	}
}

func TestWithDoesNotMutateParent(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, slog.LevelInfo)

	parent := With(context.Background(), slog.String("request_id", "req-1"))
	_ = With(parent, slog.String("order_id", "ord-1"))
	logger.InfoContext(parent, "parent")

	if m := decode(t, &buf); m["order_id"] != nil {
		t.Errorf("child attribute leaked into parent context: %v", m)
	}
}

func TestLevelFilter(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, slog.LevelInfo).Debug("hidden")
	if buf.Len() != 0 {
		t.Errorf("debug record written at info level: %s", buf.String())
	}
}
