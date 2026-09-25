package events

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestOrderEventWireFormat(t *testing.T) {
	ev := OrderEvent{
		EventID:    1042,
		EventType:  OrderPaid,
		OccurredAt: time.Date(2026, 10, 1, 20, 0, 0, 0, time.UTC),
		OrderID:    uuid.MustParse("0192f7a0-0000-7000-8000-000000000001"),
		Event:      1,
		UserID:     123,
		SeatIDs:    []string{"VIP-A-12"},
		TotalVND:   3_500_000,
	}

	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"event_id":1042,"event_type":"order.paid","occurred_at":"2026-10-01T20:00:00Z",` +
		`"order_id":"0192f7a0-0000-7000-8000-000000000001","event":1,"user_id":123,` +
		`"seat_ids":["VIP-A-12"],"total_vnd":3500000}`
	if string(raw) != want {
		t.Errorf("wire format changed\n got: %s\nwant: %s", raw, want)
	}

	var back OrderEvent
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, ev) {
		t.Errorf("round trip mismatch\n got: %+v\nwant: %+v", back, ev)
	}
}

// PostgreSQL renders now() inside jsonb as "2026-09-25T08:51:32.842993+00:00".
func TestOrderEventDecodesDatabaseTimestamp(t *testing.T) {
	raw := `{"event_id":7,"event_type":"order.held","occurred_at":"2026-09-25T08:51:32.842993+00:00",` +
		`"order_id":"0192f7a0-0000-7000-8000-000000000001","event":1,"user_id":5,"seat_ids":["CAT1-A-1","CAT1-A-2"],"total_vnd":4000000}`
	var ev OrderEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := time.Date(2026, 9, 25, 8, 51, 32, 842993000, time.UTC)
	if !ev.OccurredAt.Equal(want) {
		t.Errorf("OccurredAt = %v, want %v", ev.OccurredAt, want)
	}
}
