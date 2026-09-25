package order

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/huy205-dev/ticketrush/internal/catalog"
)

// CreateRequest is the body of POST /v1/orders.
type CreateRequest struct {
	EventID int64    `json:"event_id"`
	SeatIDs []string `json:"seat_ids"`
}

// normalize validates r against ev and returns a copy with the seat ids
// sorted, which is the form that gets hashed and stored.
func (r CreateRequest) normalize(ev *catalog.Event) (CreateRequest, error) {
	n := len(r.SeatIDs)
	if n == 0 {
		return CreateRequest{}, NewValidationError("seat_ids", "at least one seat is required")
	}
	if n > ev.MaxSeatsPerOrder {
		return CreateRequest{}, NewValidationError("seat_ids",
			fmt.Sprintf("at most %d seats per order", ev.MaxSeatsPerOrder))
	}

	seats := slices.Clone(r.SeatIDs)
	slices.Sort(seats)
	for i, id := range seats {
		if i > 0 && seats[i-1] == id {
			return CreateRequest{}, NewValidationError("seat_ids", "duplicate seat "+id)
		}
		if _, ok := ev.Seat(id); !ok {
			return CreateRequest{}, NewValidationError("seat_ids", "unknown seat "+id)
		}
	}
	return CreateRequest{EventID: r.EventID, SeatIDs: seats}, nil
}

// requestHash is sha256 over the canonical JSON of a normalized request.
//
// Canonical means: fixed key order (struct field order) and sorted seat ids,
// so bodies that differ only in key order, whitespace or seat order hash the
// same. They would create an identical order, so reusing an Idempotency-Key
// with any of them is a replay, not a conflict. Unknown fields are rejected
// before this point, so nothing in the body is left out of the hash.
func requestHash(normalized CreateRequest) string {
	canonical, err := json.Marshal(normalized)
	if err != nil {
		// A struct of an int64 and a []string always encodes.
		panic(fmt.Sprintf("encode canonical request: %v", err))
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}
