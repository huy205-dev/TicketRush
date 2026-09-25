package notifier

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/huy205-dev/ticketrush/internal/events"
)

type memSeen map[int64]bool

func (m memSeen) Seen(_ context.Context, id int64) (bool, error) { return m[id], nil }
func (m memSeen) Mark(_ context.Context, id int64) error         { m[id] = true; return nil }

type recorder struct {
	sent []string
	err  error
}

func (r *recorder) Send(_ context.Context, text string) error {
	if r.err != nil {
		return r.err
	}
	r.sent = append(r.sent, text)
	return nil
}

var orderID = uuid.MustParse("0199a3b2-0000-7000-8000-000000000001")

func TestMessage(t *testing.T) {
	issued := Message(events.OrderEvent{
		EventType: events.TicketIssued, OrderID: orderID, Event: 3,
		SeatIDs: []string{"VIP-A-1", "VIP-A-2"}, TotalVND: 7_000_000,
	})
	if want := "Đã xuất 2 vé cho đơn " + orderID.String() + " (sự kiện 3): VIP-A-1, VIP-A-2. Tổng 7.000.000 đ."; issued != want {
		t.Errorf("ticket.issued message = %q\nwant %q", issued, want)
	}
	refunded := Message(events.OrderEvent{
		EventType: events.OrderRefunded, OrderID: orderID, ProviderTxnID: "fp_1", AmountVND: 3_500_000, Reason: "PAID_AFTER_CANCEL",
	})
	if want := "Đã hoàn 3.500.000 đ cho đơn " + orderID.String() + " (giao dịch fp_1, lý do PAID_AFTER_CANCEL)."; refunded != want {
		t.Errorf("order.refunded message = %q\nwant %q", refunded, want)
	}
	if m := Message(events.OrderEvent{EventType: events.OrderHeld}); m != "" {
		t.Errorf("order.held message = %q, want none", m)
	}
}

func TestNotifyOncePerEvent(t *testing.T) {
	rec := &recorder{}
	n := New(rec, memSeen{}, slog.New(slog.DiscardHandler))
	ev := events.OrderEvent{EventID: 10, EventType: events.TicketIssued, OrderID: orderID, SeatIDs: []string{"A-1"}}
	for range 3 {
		if err := n.Notify(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	if len(rec.sent) != 1 {
		t.Errorf("sent %d messages, want 1", len(rec.sent))
	}
}

func TestNotifyRetriesAfterSendFailure(t *testing.T) {
	rec := &recorder{err: errors.New("down")}
	seen := memSeen{}
	n := New(rec, seen, slog.New(slog.DiscardHandler))
	ev := events.OrderEvent{EventID: 11, EventType: events.OrderRefunded, OrderID: orderID}
	if err := n.Notify(context.Background(), ev); err == nil {
		t.Fatal("Notify succeeded while the sender is down")
	}
	if seen[11] {
		t.Fatal("event marked as notified although sending failed")
	}
	rec.err = nil
	if err := n.Notify(context.Background(), ev); err != nil || len(rec.sent) != 1 {
		t.Errorf("retry: err %v, sent %d", err, len(rec.sent))
	}
}

func TestTelegram(t *testing.T) {
	var got url.Values
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		got, _ = url.ParseQuery(string(body))
		if got.Get("chat_id") == "fail" {
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	tg := NewTelegram(srv.URL, "123:SECRET", "42", time.Second)
	if err := tg.Send(context.Background(), "xin chào"); err != nil {
		t.Fatal(err)
	}
	if path != "/bot123:SECRET/sendMessage" || got.Get("chat_id") != "42" || got.Get("text") != "xin chào" {
		t.Errorf("request = %s %v", path, got)
	}

	if err := NewTelegram(srv.URL, "123:SECRET", "fail", time.Second).Send(context.Background(), "x"); err == nil {
		t.Error("status 400 was not an error")
	}
	// A transport error must not leak the token through the URL.
	err := NewTelegram("http://127.0.0.1:1", "123:SECRET", "42", time.Second).Send(context.Background(), "x")
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("transport error = %v, want an error without the token", err)
	}
}
