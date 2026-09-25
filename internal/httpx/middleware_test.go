package httpx

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/huy205-dev/ticketrush/internal/platform/logging"
	"github.com/huy205-dev/ticketrush/internal/platform/timing"
)

func TestRequestID(t *testing.T) {
	tests := []struct {
		name     string
		incoming string
		keep     bool
	}{
		{"generated when missing", "", false},
		{"kept when valid", "abc-123_x.y:z", true},
		{"replaced when too long", strings.Repeat("a", maxRequestIDLen+1), false},
		{"replaced when it could forge log lines", "id\nlevel=ERROR", false},
		{"replaced when it contains spaces", "a b", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var seen string
			h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = RequestIDFrom(r.Context())
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.incoming != "" {
				req.Header[RequestIDHeader] = []string{tc.incoming}
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			echoed := rec.Header().Get(RequestIDHeader)
			if echoed == "" || echoed != seen {
				t.Fatalf("response header %q, context %q: want same non-empty ID", echoed, seen)
			}
			if tc.keep != (echoed == tc.incoming) {
				t.Errorf("ID = %q, incoming %q, keep = %v", echoed, tc.incoming, tc.keep)
			}
			if !validRequestID(echoed) {
				t.Errorf("returned ID %q is not valid", echoed)
			}
		})
	}
}

// newTestRouter wires the middleware in the same order as cmd/booking.
func newTestRouter(logger *slog.Logger) *chi.Mux {
	r := chi.NewRouter()
	r.Use(RequestID, AccessLog(logger, "/healthz"), Recover(logger))
	r.NotFound(NotFound)
	r.MethodNotAllowed(MethodNotAllowed)
	r.Get("/items/{id}", func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, http.StatusOK, map[string]string{"id": chi.URLParam(r, "id")})
	})
	r.Get("/boom", func(http.ResponseWriter, *http.Request) { panic("boom") })
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {})
	return r
}

func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("log line is not JSON: %s", l)
		}
		lines = append(lines, m)
	}
	return lines
}

func TestAccessLogUsesRoutePatternAndRequestID(t *testing.T) {
	var buf bytes.Buffer
	r := newTestRouter(logging.New(&buf, slog.LevelInfo))

	req := httptest.NewRequest(http.MethodGet, "/items/42", nil)
	req.Header.Set(RequestIDHeader, "req-1")
	r.ServeHTTP(httptest.NewRecorder(), req)

	lines := logLines(t, &buf)
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want 1: %s", len(lines), buf.String())
	}
	l := lines[0]
	if l["route"] != "/items/{id}" || l["path"] != "/items/42" {
		t.Errorf("route/path = %v/%v, want pattern and raw path", l["route"], l["path"])
	}
	if l["request_id"] != "req-1" {
		t.Errorf("request_id = %v, want req-1", l["request_id"])
	}
	if l["status"] != float64(200) {
		t.Errorf("status = %v, want 200", l["status"])
	}
}

func TestAccessLogQuietPaths(t *testing.T) {
	var buf bytes.Buffer
	r := newTestRouter(logging.New(&buf, slog.LevelInfo))
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if buf.Len() != 0 {
		t.Errorf("health probe logged at info level: %s", buf.String())
	}
}

func TestRecoverReturnsStandardError(t *testing.T) {
	var buf bytes.Buffer
	r := newTestRouter(logging.New(&buf, slog.LevelInfo))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boom", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var body errorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error.Code != CodeInternal {
		t.Errorf("body = %s, want error code %s", rec.Body.String(), CodeInternal)
	}
	if strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("panic value leaked to client: %s", rec.Body.String())
	}

	var sawPanic, sawAccess bool
	for _, l := range logLines(t, &buf) {
		switch l["msg"] {
		case "panic in handler":
			sawPanic = l["stack"] != nil && l["request_id"] != nil
		case "http request":
			sawAccess = l["status"] == float64(500) && l["level"] == "ERROR"
		}
	}
	if !sawPanic || !sawAccess {
		t.Errorf("want panic log with stack and request_id plus a 500 access log, got: %s", buf.String())
	}
}

func TestRouterFallbacksUseErrorEnvelope(t *testing.T) {
	r := newTestRouter(discardLogger())
	tests := []struct {
		method, path string
		status       int
		code         string
	}{
		{http.MethodGet, "/nope", http.StatusNotFound, CodeNotFound},
		{http.MethodPost, "/items/1", http.StatusMethodNotAllowed, CodeMethodNotAllowed},
	}
	for _, tc := range tests {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		var body errorEnvelope
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != tc.status || body.Error.Code != tc.code {
			t.Errorf("%s %s = %d %q, want %d %q", tc.method, tc.path, rec.Code, body.Error.Code, tc.status, tc.code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s %s Content-Type = %q, want JSON", tc.method, tc.path, ct)
		}
	}
}

func TestAccessLogIncludesDependencyTimings(t *testing.T) {
	var buf bytes.Buffer
	logger := logging.New(&buf, slog.LevelInfo)
	h := AccessLog(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		timing.AddDBAcquire(r.Context(), 4*time.Millisecond)
		timing.AddRedis(r.Context(), time.Millisecond)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/orders", nil))

	l := logLines(t, &buf)[0]
	if l["db_acquire_ms"] != 4.0 || l["db_acquires"] != 1.0 || l["redis_ms"] != 1.0 {
		t.Errorf("timings missing from access log: %v", l)
	}
	if _, ok := l["db_query_ms"]; ok {
		t.Errorf("unused dependency logged: %v", l)
	}
}
