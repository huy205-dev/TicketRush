package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/huy205-dev/ticketrush/internal/httpx"
	"github.com/huy205-dev/ticketrush/internal/platform/config"
)

func TestRunRejectsInvalidConfig(t *testing.T) {
	var out bytes.Buffer
	noEnv := func(string) (string, bool) { return "", false }

	err := run(context.Background(), noEnv, &out)

	var ve *config.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("run error = %v, want *config.ValidationError", err)
	}
	log := out.String()
	for _, want := range []string{`"level":"ERROR"`, "invalid configuration", "DATABASE_URL is required", "JWT_SECRET is required"} {
		if !strings.Contains(log, want) {
			t.Errorf("startup log missing %q: %s", want, log)
		}
	}
}

func TestParseIdempotencyKey(t *testing.T) {
	canonical := "0192f7a0-1b2c-7d3e-8f40-123456789abc"
	for raw, ok := range map[string]bool{
		canonical:                              true,
		strings.ToUpper(canonical):             true,
		"{" + canonical + "}":                  true,
		"":                                     false,
		"not-a-uuid":                           false,
		"00000000-0000-0000-0000-000000000000": false,
		canonical + "0":                        false,
	} {
		key, err := parseIdempotencyKey(raw)
		if ok != (err == nil) {
			t.Errorf("parseIdempotencyKey(%q) error = %v, want ok=%v", raw, err, ok)
			continue
		}
		if ok && key.String() != canonical {
			t.Errorf("parseIdempotencyKey(%q) = %s, want canonical %s", raw, key, canonical)
		}
	}
}

// Booking serves the frontend (SPEC.md 3.2).
func TestRouterServesFrontend(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	h := newRouter(logger, httpx.NewHealth(logger, time.Second), newAPI(apiDeps{logger: logger}))
	for path, want := range map[string]string{"/": "<title>TicketRush</title>", "/app.js": "EventSource", "/style.css": ".seat"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("GET %s = %d, body without %q", path, rec.Code, want)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/embed.go", nil))
	if rec.Code != 404 {
		t.Errorf("GET /embed.go = %d, want 404", rec.Code)
	}
}
