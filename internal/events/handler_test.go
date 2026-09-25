package events

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/huy205-dev/ticketrush/internal/platform/kafka"
)

func record(t *testing.T, header string, ev OrderEvent) *kgo.Record {
	t.Helper()
	value, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	r := &kgo.Record{Value: value}
	if header != "" {
		r.Headers = []kgo.RecordHeader{{Key: HeaderEventType, Value: []byte(header)}}
	}
	return r
}

func TestHandlerFiltersAndDecodes(t *testing.T) {
	var got []OrderEvent
	h := Handler(func(_ context.Context, ev OrderEvent) error {
		got = append(got, ev)
		return nil
	}, OrderPaid, OrderRefunded)

	paid := OrderEvent{EventID: 7, EventType: OrderPaid, OrderID: uuid.New(), SeatIDs: []string{"A-1"}}
	for _, r := range []*kgo.Record{
		record(t, OrderPaid, paid),
		record(t, OrderHeld, OrderEvent{EventType: OrderHeld}),
		record(t, "", OrderEvent{EventType: OrderExpired}),                                                       // no header: payload decides
		record(t, "", OrderEvent{EventID: 8, EventType: OrderRefunded}),                                          // no header, wanted
		{Headers: []kgo.RecordHeader{{Key: HeaderEventType, Value: []byte(OrderCancelled)}}, Value: []byte("{")}, // skipped before decoding
	} {
		if err := h(context.Background(), r); err != nil {
			t.Fatalf("handler: %v", err)
		}
	}
	if len(got) != 2 || got[0].EventID != 7 || got[0].OrderID != paid.OrderID || got[1].EventID != 8 {
		t.Errorf("handled %+v, want the paid and refunded events", got)
	}
}

func TestHandlerMalformedRecordIsPermanent(t *testing.T) {
	h := Handler(func(context.Context, OrderEvent) error { return nil }, OrderPaid)
	err := h(context.Background(), &kgo.Record{
		Headers: []kgo.RecordHeader{{Key: HeaderEventType, Value: []byte(OrderPaid)}},
		Value:   []byte("not json"),
	})
	if err == nil || !kafka.IsPermanent(err) {
		t.Errorf("err = %v, want a permanent error", err)
	}
}

func TestHandlerPassesErrorsThrough(t *testing.T) {
	boom := errors.New("boom")
	h := Handler(func(context.Context, OrderEvent) error { return boom }, OrderPaid)
	if err := h(context.Background(), record(t, OrderPaid, OrderEvent{EventType: OrderPaid})); !errors.Is(err, boom) || kafka.IsPermanent(err) {
		t.Errorf("err = %v, want boom, retryable", err)
	}
}
