package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func ok(context.Context) error { return nil }

func failing(context.Context) error { return errors.New("dial tcp 10.0.0.5:5432: connection refused") }

// blocking only returns when the readiness timeout cancels its context.
func blocking(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func serveReady(t *testing.T, h *Health) (int, healthResponse, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Ready(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	raw, _ := io.ReadAll(rec.Body)
	var body healthResponse
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("readyz body is not JSON: %v: %s", err, raw)
	}
	return rec.Code, body, string(raw)
}

func TestReady(t *testing.T) {
	tests := []struct {
		name       string
		checks     []Check
		wantStatus int
		wantChecks map[string]string
	}{
		{
			name:       "all dependencies up",
			checks:     []Check{{"postgres", ok}, {"redis", ok}},
			wantStatus: http.StatusOK,
			wantChecks: map[string]string{"postgres": "ok", "redis": "ok"},
		},
		{
			name:       "postgres down",
			checks:     []Check{{"postgres", failing}, {"redis", ok}},
			wantStatus: http.StatusServiceUnavailable,
			wantChecks: map[string]string{"postgres": "fail", "redis": "ok"},
		},
		{
			name:       "redis hangs past timeout",
			checks:     []Check{{"postgres", ok}, {"redis", blocking}},
			wantStatus: http.StatusServiceUnavailable,
			wantChecks: map[string]string{"postgres": "ok", "redis": "fail"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHealth(discardLogger(), 50*time.Millisecond, tc.checks...)
			code, body, _ := serveReady(t, h)
			if code != tc.wantStatus {
				t.Errorf("status = %d, want %d", code, tc.wantStatus)
			}
			for name, want := range tc.wantChecks {
				if body.Checks[name] != want {
					t.Errorf("checks[%s] = %q, want %q", name, body.Checks[name], want)
				}
			}
		})
	}
}

func TestReadyDoesNotLeakErrorDetails(t *testing.T) {
	h := NewHealth(discardLogger(), time.Second, Check{"postgres", failing})
	_, _, raw := serveReady(t, h)
	if want := "10.0.0.5"; strings.Contains(raw, want) {
		t.Errorf("readyz body leaks internal error %q: %s", want, raw)
	}
}

func TestLiveIgnoresDependencies(t *testing.T) {
	h := NewHealth(discardLogger(), time.Second, Check{"postgres", failing})
	rec := httptest.NewRecorder()
	h.Live(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("healthz status = %d, want 200 even when postgres is down", rec.Code)
	}
}
