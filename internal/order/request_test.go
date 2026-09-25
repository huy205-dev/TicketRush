package order

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/huy205-dev/ticketrush/internal/catalog"
)

// testEvent is a small event built from a real layout so Seat lookups work.
func testEvent(t *testing.T) *catalog.Event {
	t.Helper()
	spec := catalog.EventSpec{
		MaxSeatsPerOrder: 4,
		Zones:            []catalog.ZoneLayout{{Zone: "VIP", Rows: 1, SeatsPerRow: 10, PriceVND: 3_500_000}},
	}
	return catalog.NewTestEvent(1, spec)
}

func TestNormalizeValidation(t *testing.T) {
	ev := testEvent(t)
	tests := []struct {
		name   string
		seats  []string
		reason string // empty means valid
	}{
		{"one seat", []string{"VIP-A-1"}, ""},
		{"max seats", []string{"VIP-A-1", "VIP-A-2", "VIP-A-3", "VIP-A-4"}, ""},
		{"no seats", nil, "at least one seat"},
		{"too many seats", []string{"VIP-A-1", "VIP-A-2", "VIP-A-3", "VIP-A-4", "VIP-A-5"}, "at most 4 seats"},
		{"duplicate", []string{"VIP-A-1", "VIP-A-2", "VIP-A-1"}, "duplicate seat VIP-A-1"},
		{"unknown seat", []string{"VIP-A-1", "VIP-Z-99"}, "unknown seat VIP-Z-99"},
		{"empty id", []string{""}, "unknown seat"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := CreateRequest{EventID: 1, SeatIDs: tc.seats}.normalize(ev)
			if tc.reason == "" {
				if err != nil {
					t.Fatalf("normalize: %v", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) || !errors.Is(err, ErrValidation) {
				t.Fatalf("error = %v, want *ValidationError", err)
			}
			if ve.Field != "seat_ids" || !strings.Contains(ve.Reason, tc.reason) {
				t.Errorf("got %s/%q, want seat_ids/%q", ve.Field, ve.Reason, tc.reason)
			}
		})
	}
}

func TestNormalizeSortsWithoutMutatingInput(t *testing.T) {
	in := []string{"VIP-A-3", "VIP-A-1", "VIP-A-2"}
	got, err := CreateRequest{EventID: 1, SeatIDs: in}.normalize(testEvent(t))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got.SeatIDs, ",") != "VIP-A-1,VIP-A-2,VIP-A-3" {
		t.Errorf("seat ids not sorted: %v", got.SeatIDs)
	}
	if in[0] != "VIP-A-3" {
		t.Errorf("caller's slice was modified: %v", in)
	}
}

func hashOf(t *testing.T, body string) string {
	t.Helper()
	var req CreateRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	norm, err := req.normalize(testEvent(t))
	if err != nil {
		t.Fatal(err)
	}
	return requestHash(norm)
}

func TestRequestHashIsStable(t *testing.T) {
	base := hashOf(t, `{"event_id":1,"seat_ids":["VIP-A-1","VIP-A-2"]}`)

	same := []string{
		`{"seat_ids":["VIP-A-1","VIP-A-2"],"event_id":1}`,            // key order
		`{ "event_id" : 1 , "seat_ids" : [ "VIP-A-1", "VIP-A-2" ] }`, // whitespace
		`{"event_id":1,"seat_ids":["VIP-A-2","VIP-A-1"]}`,            // seat order
	}
	for _, body := range same {
		if got := hashOf(t, body); got != base {
			t.Errorf("hash of %s = %s, want %s", body, got, base)
		}
	}

	different := []string{
		`{"event_id":1,"seat_ids":["VIP-A-1"]}`,
		`{"event_id":1,"seat_ids":["VIP-A-1","VIP-A-3"]}`,
		`{"event_id":2,"seat_ids":["VIP-A-1","VIP-A-2"]}`,
	}
	for _, body := range different {
		if got := hashOf(t, body); got == base {
			t.Errorf("hash of %s collides with the base request", body)
		}
	}
	if len(base) != 64 {
		t.Errorf("hash %q is not hex sha256", base)
	}
}
