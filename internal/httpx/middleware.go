package httpx

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/huy205-dev/ticketrush/internal/platform/logging"
	"github.com/huy205-dev/ticketrush/internal/platform/timing"
)

// RequestIDHeader carries the request ID in both directions.
const RequestIDHeader = "X-Request-Id"

const maxRequestIDLen = 128

type requestIDKey struct{}

// RequestIDFrom returns the request ID stored by the RequestID middleware.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// RequestID reuses the caller's X-Request-Id when it looks sane, otherwise
// generates one. The ID is echoed in the response and attached to every log
// record written with the request context.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(RequestIDHeader)
		if !validRequestID(id) {
			id = rand.Text()
		}
		w.Header().Set(RequestIDHeader, id)

		ctx := context.WithValue(r.Context(), requestIDKey{}, id)
		ctx = logging.With(ctx, slog.String("request_id", id))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// validRequestID rejects IDs that are empty, overly long or contain
// characters that could forge log lines or headers.
func validRequestID(id string) bool {
	if id == "" || len(id) > maxRequestIDLen {
		return false
	}
	for _, c := range []byte(id) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '_', c == '.', c == ':':
		default:
			return false
		}
	}
	return true
}

// AccessLog logs one line per request. Requests to quietPaths (health probes)
// are logged at debug level so they do not drown the useful lines.
func AccessLog(logger *slog.Logger, quietPaths ...string) func(http.Handler) http.Handler {
	quiet := make(map[string]bool, len(quietPaths))
	for _, p := range quietPaths {
		quiet[p] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			ctx, stats := timing.With(r.Context())
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r.WithContext(ctx))

			status := ww.Status()
			if status == 0 {
				// Handler wrote nothing; net/http sends 200.
				status = http.StatusOK
			}
			level := slog.LevelInfo
			switch {
			case status >= 500:
				level = slog.LevelError
			case quiet[r.URL.Path]:
				level = slog.LevelDebug
			}
			attrs := append([]slog.Attr{
				slog.String("method", r.Method),
				slog.String("route", routePattern(r)),
				slog.String("path", r.URL.Path),
				slog.Int("status", status),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
			}, stats.LogAttrs()...)
			logger.LogAttrs(r.Context(), level, "http request", attrs...)
		})
	}
}

// routePattern returns the matched chi route (e.g. /v1/orders/{orderId}),
// which keeps log and metric cardinality bounded.
func routePattern(r *http.Request) string {
	if rctx := chi.RouteContext(r.Context()); rctx != nil {
		return rctx.RoutePattern()
	}
	return ""
}

// Recover turns a panic in a handler into a 500 with the standard error body
// and logs the stack trace.
func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				v := recover()
				if v == nil {
					return
				}
				if v == http.ErrAbortHandler {
					// Deliberate abort; let net/http handle it.
					panic(v)
				}
				logger.ErrorContext(r.Context(), "panic in handler",
					"panic", v,
					"stack", string(debug.Stack()),
				)
				WriteError(w, http.StatusInternalServerError, ErrorBody{Code: CodeInternal, Message: "Lỗi hệ thống"})
			}()
			next.ServeHTTP(w, r)
		})
	}
}
