package httpx

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/huy205-dev/ticketrush/internal/auth"
	"github.com/huy205-dev/ticketrush/internal/catalog"
	"github.com/huy205-dev/ticketrush/internal/order"
)

// ErrBadRequest means the request body could not be parsed.
var ErrBadRequest = errors.New("malformed request body")

// errorMapping ties a business sentinel error to its HTTP response.
type errorMapping struct {
	target  error
	status  int
	code    string
	message string
	// retryAfter, when set, is sent as the Retry-After header (seconds).
	retryAfter string
}

// errorMappings is the single place where business errors become HTTP
// responses (SPEC.md section 7). The first match wins.
var errorMappings = []errorMapping{
	{ErrBadRequest, http.StatusBadRequest, "BAD_REQUEST", "Body không phải JSON hợp lệ", ""},
	{auth.ErrUnauthorized, http.StatusUnauthorized, "UNAUTHORIZED", "Thiếu hoặc sai access token", ""},
	{order.ErrValidation, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "Dữ liệu không hợp lệ", ""},
	{order.ErrIdempotencyKeyReused, http.StatusUnprocessableEntity, "IDEMPOTENCY_KEY_REUSED", "Idempotency-Key đã dùng cho một request khác", ""},
	{order.ErrIdempotencyKeyInProgress, http.StatusConflict, "IDEMPOTENCY_KEY_IN_PROGRESS", "Request với Idempotency-Key này đang được xử lý", "1"},
	{order.ErrSeatsUnavailable, http.StatusConflict, "SEATS_UNAVAILABLE", "Một số ghế đã có người giữ", ""},
	{order.ErrActiveOrderExists, http.StatusConflict, "ACTIVE_ORDER_EXISTS", "Bạn đang có một đơn giữ ghế cho sự kiện này", ""},
	{order.ErrNotCancellable, http.StatusConflict, "ORDER_NOT_CANCELLABLE", "Đơn không ở trạng thái có thể hủy", ""},
	{order.ErrNotFound, http.StatusNotFound, "ORDER_NOT_FOUND", "Không tìm thấy đơn hàng", ""},
	{catalog.ErrEventNotFound, http.StatusNotFound, "EVENT_NOT_FOUND", "Không tìm thấy sự kiện", ""},
	{order.ErrTemporarilyUnavailable, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Hệ thống tạm thời không xử lý được, vui lòng thử lại", "1"},
}

// detailer is implemented by errors that carry response details, such as
// the seats that were unavailable.
type detailer interface {
	ErrorDetails() map[string]any
}

// WriteErr writes the response for err. Business errors get their mapped
// status; anything else is logged and answered with a generic 500 so
// internals never leak to the client.
func WriteErr(w http.ResponseWriter, r *http.Request, logger *slog.Logger, err error) {
	for _, m := range errorMappings {
		if !errors.Is(err, m.target) {
			continue
		}
		body := ErrorBody{Code: m.code, Message: m.message}
		var d detailer
		if errors.As(err, &d) {
			body.Details = d.ErrorDetails()
		}
		if m.retryAfter != "" {
			w.Header().Set("Retry-After", m.retryAfter)
		}
		if m.status >= 500 {
			// Mapped, but still worth an operator's attention.
			logger.WarnContext(r.Context(), "request failed", "err", err)
		}
		WriteError(w, m.status, body)
		return
	}

	if errors.Is(err, context.Canceled) && r.Context().Err() != nil {
		// The client went away; nobody reads this response.
		logger.InfoContext(r.Context(), "request cancelled by client", "err", err)
	} else {
		logger.ErrorContext(r.Context(), "request failed", "err", err)
	}
	WriteError(w, http.StatusInternalServerError, ErrorBody{Code: CodeInternal, Message: "Lỗi hệ thống"})
}
