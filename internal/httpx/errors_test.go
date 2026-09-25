package httpx

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/huy205-dev/ticketrush/internal/auth"
	"github.com/huy205-dev/ticketrush/internal/catalog"
	"github.com/huy205-dev/ticketrush/internal/order"
	"github.com/huy205-dev/ticketrush/internal/platform/logging"
)

type errorResponse struct {
	Error ErrorBody `json:"error"`
}

func writeErr(t *testing.T, err error) (int, errorResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	WriteErr(rec, httptest.NewRequest(http.MethodGet, "/", nil), discardLogger(), err)
	var body errorResponse
	if e := json.Unmarshal(rec.Body.Bytes(), &body); e != nil {
		t.Fatalf("error body is not JSON: %s", rec.Body.String())
	}
	return rec.Code, body
}

func TestWriteErrMapsBusinessErrors(t *testing.T) {
	tests := []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("decode: %w", ErrBadRequest), 400, "BAD_REQUEST"},
		{fmt.Errorf("verify: %w", auth.ErrUnauthorized), 401, "UNAUTHORIZED"},
		{order.NewValidationError("seat_ids", "too many"), 422, "VALIDATION_ERROR"},
		{order.ErrIdempotencyKeyReused, 422, "IDEMPOTENCY_KEY_REUSED"},
		{&order.SeatsUnavailableError{SeatIDs: []string{"VIP-A-1"}}, 409, "SEATS_UNAVAILABLE"},
		{order.ErrActiveOrderExists, 409, "ACTIVE_ORDER_EXISTS"},
		{&order.NotCancellableError{Status: order.StatusPaid}, 409, "ORDER_NOT_CANCELLABLE"},
		{order.ErrNotFound, 404, "ORDER_NOT_FOUND"},
		{fmt.Errorf("load: %w", catalog.ErrEventNotFound), 404, "EVENT_NOT_FOUND"},
		{order.ErrIdempotencyKeyInProgress, 409, "IDEMPOTENCY_KEY_IN_PROGRESS"},
		{fmt.Errorf("%w: lock: dial tcp: refused", order.ErrTemporarilyUnavailable), 503, "SERVICE_UNAVAILABLE"},
		{errors.New("connection reset"), 500, CodeInternal},
	}
	for _, tc := range tests {
		t.Run(tc.code, func(t *testing.T) {
			status, body := writeErr(t, tc.err)
			if status != tc.status || body.Error.Code != tc.code {
				t.Errorf("got %d %s, want %d %s", status, body.Error.Code, tc.status, tc.code)
			}
			if body.Error.Message == "" {
				t.Error("message is empty")
			}
		})
	}
}

func TestWriteErrIncludesDetails(t *testing.T) {
	_, body := writeErr(t, fmt.Errorf("create: %w", &order.SeatsUnavailableError{SeatIDs: []string{"VIP-A-1", "VIP-A-2"}}))
	ids, _ := body.Error.Details["seat_ids"].([]any)
	if len(ids) != 2 || ids[0] != "VIP-A-1" {
		t.Errorf("details = %v, want seat_ids [VIP-A-1 VIP-A-2]", body.Error.Details)
	}

	_, body = writeErr(t, order.NewValidationError("seat_ids", "duplicate seat VIP-A-1"))
	if body.Error.Details["field"] != "seat_ids" || body.Error.Details["reason"] != "duplicate seat VIP-A-1" {
		t.Errorf("validation details = %v", body.Error.Details)
	}
}

func TestWriteErrHidesInternalErrors(t *testing.T) {
	var logs bytes.Buffer
	rec := httptest.NewRecorder()
	WriteErr(rec, httptest.NewRequest(http.MethodGet, "/", nil), logging.New(&logs, slog.LevelInfo),
		errors.New("dial tcp 10.0.0.5:5432: connection refused"))
	if strings.Contains(rec.Body.String(), "10.0.0.5") {
		t.Errorf("internal error leaked: %s", rec.Body.String())
	}
	if !strings.Contains(logs.String(), "10.0.0.5") {
		t.Errorf("internal error not logged: %s", logs.String())
	}
}

func TestRequireUser(t *testing.T) {
	tokens := auth.NewTokens(strings.Repeat("s", 32), time.Hour)
	good, err := tokens.Issue(42)
	if err != nil {
		t.Fatal(err)
	}
	var seen int64
	h := RequireUser(tokens, discardLogger())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen, _ = auth.UserIDFrom(r.Context())
	}))

	tests := []struct {
		name   string
		header string
		status int
	}{
		{"valid", "Bearer " + good, http.StatusOK},
		{"missing", "", http.StatusUnauthorized},
		{"wrong scheme", "Basic " + good, http.StatusUnauthorized},
		{"empty token", "Bearer ", http.StatusUnauthorized},
		{"bad token", "Bearer nope", http.StatusUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			seen = 0
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d", rec.Code, tc.status)
			}
			if tc.status == http.StatusOK && seen != 42 {
				t.Errorf("user id in context = %d, want 42", seen)
			}
		})
	}
}

func TestDecodeJSON(t *testing.T) {
	type body struct {
		A int `json:"a"`
	}
	tests := []struct {
		name string
		in   string
		ok   bool
	}{
		{"valid", `{"a":1}`, true},
		{"unknown field", `{"a":1,"b":2}`, false},
		{"trailing data", `{"a":1}{"a":2}`, false},
		{"not json", `a=1`, false},
		{"empty", ``, false},
		{"too large", `{"a":1,"pad":"` + strings.Repeat("x", maxBodyBytes) + `"}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var dst body
			err := DecodeJSON(httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", strings.NewReader(tc.in)), &dst)
			if tc.ok != (err == nil) {
				t.Fatalf("DecodeJSON error = %v, want ok=%v", err, tc.ok)
			}
			if !tc.ok && !errors.Is(err, ErrBadRequest) {
				t.Errorf("error %v does not wrap ErrBadRequest", err)
			}
		})
	}
}

func TestWriteErrSetsRetryAfter(t *testing.T) {
	for err, want := range map[error]string{
		order.ErrIdempotencyKeyInProgress: "1",
		order.ErrTemporarilyUnavailable:   "1",
		order.ErrSeatsUnavailable:         "",
		errors.New("boom"):                "",
	} {
		rec := httptest.NewRecorder()
		WriteErr(rec, httptest.NewRequest(http.MethodGet, "/", nil), discardLogger(), err)
		if got := rec.Header().Get("Retry-After"); got != want {
			t.Errorf("%v: Retry-After = %q, want %q", err, got, want)
		}
	}
}
