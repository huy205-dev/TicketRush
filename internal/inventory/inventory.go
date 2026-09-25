// Package inventory decides which seats are free, held or sold.
//
// Two backends implement Inventory: PG (the baseline, row locks on the seats
// table) and Redis (M2, Lua scripts). The tickets table's UNIQUE constraint
// is the final guard against double selling regardless of backend.
package inventory

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// SeatStatus is the public state of a seat.
type SeatStatus string

const (
	Available SeatStatus = "AVAILABLE"
	Held      SeatStatus = "HELD"
	Sold      SeatStatus = "SOLD"
)

// ErrUnknownSeat is returned when a seat id does not exist for the event.
// Callers validate seat ids first, so this indicates a bug or stale cache.
var ErrUnknownSeat = errors.New("unknown seat")

// ErrConfirmMismatch means Confirm found a seat that is not ticketed for the
// order. It should never happen; reconcile repairs it.
var ErrConfirmMismatch = errors.New("seat not ticketed for order")

// Inventory is the seat-holding contract from SPEC.md section 9.2.
// seatIDs passed to any method must not contain duplicates.
type Inventory interface {
	// Hold holds every seat for orderID for ttl, or none of them. It returns
	// the seats that are taken by someone else; a nil slice means success.
	// Holding again with the same orderID succeeds and extends the hold.
	Hold(ctx context.Context, eventID int64, seatIDs []string, orderID uuid.UUID, ttl time.Duration) (taken []string, err error)
	// Release frees the seats that orderID currently holds. Seats held by
	// another order are left alone.
	Release(ctx context.Context, eventID int64, seatIDs []string, orderID uuid.UUID) error
	// Confirm marks the seats sold after the tickets have been committed.
	Confirm(ctx context.Context, eventID int64, seatIDs []string, orderID uuid.UUID) error
	// Status reports the state of each requested seat.
	Status(ctx context.Context, eventID int64, seatIDs []string) (map[string]SeatStatus, error)
}
