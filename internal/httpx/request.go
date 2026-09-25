package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/huy205-dev/ticketrush/internal/auth"
	"github.com/huy205-dev/ticketrush/internal/platform/logging"
)

// maxBodyBytes caps JSON request bodies. The largest legitimate body is an
// order with a handful of seat ids.
const maxBodyBytes = 16 << 10

// DecodeJSON decodes a single JSON object from the body into dst. Unknown
// fields and trailing data are rejected so that nothing the client sent is
// silently ignored.
func DecodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%w: %w", ErrBadRequest, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing data after JSON object", ErrBadRequest)
	}
	return nil
}

// RequireUser rejects requests without a valid "Authorization: Bearer"
// access token and stores the user id in the request context.
func RequireUser(tokens *auth.Tokens, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || raw == "" {
				WriteErr(w, r, logger, fmt.Errorf("%w: missing bearer token", auth.ErrUnauthorized))
				return
			}
			userID, err := tokens.Verify(raw)
			if err != nil {
				WriteErr(w, r, logger, err)
				return
			}
			ctx := auth.WithUserID(r.Context(), userID)
			ctx = logging.With(ctx, slog.Int64("user_id", userID))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
