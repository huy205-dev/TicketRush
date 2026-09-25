package order

import (
	"errors"
	"fmt"
)

// Business errors. internal/httpx maps them to HTTP status codes.
var (
	ErrValidation           = errors.New("validation failed")
	ErrSeatsUnavailable     = errors.New("seats unavailable")
	ErrActiveOrderExists    = errors.New("active order exists")
	ErrIdempotencyKeyReused = errors.New("idempotency key reused with a different request")
	ErrNotFound             = errors.New("order not found")
	ErrNotCancellable       = errors.New("order cannot be cancelled")

	// ErrIdempotencyKeyInProgress means another request with the same
	// Idempotency-Key is still being processed (ADR-005).
	ErrIdempotencyKeyInProgress = errors.New("a request with this idempotency key is in progress")
	// ErrTemporarilyUnavailable means a dependency needed to process the
	// request safely did not answer; the client should retry later.
	ErrTemporarilyUnavailable = errors.New("temporarily unavailable")
)

// ValidationError explains which input was rejected. It matches
// ErrValidation with errors.Is.
type ValidationError struct {
	Field  string
	Reason string
}

// NewValidationError returns a *ValidationError.
func NewValidationError(field, reason string) *ValidationError {
	return &ValidationError{Field: field, Reason: reason}
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s: %s", ErrValidation, e.Field, e.Reason)
}

func (e *ValidationError) Is(target error) bool { return target == ErrValidation }

// ErrorDetails is rendered as the "details" field of the error response.
func (e *ValidationError) ErrorDetails() map[string]any {
	return map[string]any{"field": e.Field, "reason": e.Reason}
}

// SeatsUnavailableError lists the seats held or sold by someone else. It
// matches ErrSeatsUnavailable with errors.Is.
type SeatsUnavailableError struct {
	SeatIDs []string
}

func (e *SeatsUnavailableError) Error() string {
	return fmt.Sprintf("%s: %v", ErrSeatsUnavailable, e.SeatIDs)
}

func (e *SeatsUnavailableError) Is(target error) bool { return target == ErrSeatsUnavailable }

// ErrorDetails is rendered as the "details" field of the error response.
func (e *SeatsUnavailableError) ErrorDetails() map[string]any {
	return map[string]any{"seat_ids": e.SeatIDs}
}

// NotCancellableError carries the status that prevented cancelling. It
// matches ErrNotCancellable with errors.Is.
type NotCancellableError struct {
	Status Status
}

func (e *NotCancellableError) Error() string {
	return fmt.Sprintf("%s: status is %s", ErrNotCancellable, e.Status)
}

func (e *NotCancellableError) Is(target error) bool { return target == ErrNotCancellable }

// ErrorDetails is rendered as the "details" field of the error response.
func (e *NotCancellableError) ErrorDetails() map[string]any {
	return map[string]any{"status": e.Status}
}
