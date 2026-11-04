// Package notifier tells people that tickets were issued or money refunded:
// through Telegram when configured, otherwise only in the log.
package notifier

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/huy205-dev/ticketrush/internal/events"
	"github.com/huy205-dev/ticketrush/internal/money"
	"github.com/huy205-dev/ticketrush/internal/platform/kafka"
)

// Sender delivers one message.
type Sender interface {
	Send(ctx context.Context, text string) error
}

// Seen remembers which events were already notified, so a redelivered
// record does not send the same message twice. Keys come from DedupeKey.
type Seen interface {
	Seen(ctx context.Context, key string) (bool, error)
	Mark(ctx context.Context, key string) error
}

// Notifier turns ticket.issued and order.refunded into messages.
type Notifier struct {
	sender Sender
	seen   Seen
	logger *slog.Logger
}

// New returns a notifier sending through sender.
func New(sender Sender, seen Seen, logger *slog.Logger) *Notifier {
	return &Notifier{sender: sender, seen: seen, logger: logger}
}

// Handler consumes ticket.issued and order.refunded (consumer group
// "notifier").
func (n *Notifier) Handler() kafka.Handler {
	return events.Handler(n.Notify, events.TicketIssued, events.OrderRefunded)
}

// Notify sends the message for ev unless it was sent already. The event is
// marked after sending: a crash in between sends it again, which is better
// than never telling the buyer.
func (n *Notifier) Notify(ctx context.Context, ev events.OrderEvent) error {
	key := DedupeKey(ev)
	seen, err := n.seen.Seen(ctx, key)
	if err != nil {
		return err
	}
	if seen {
		return nil
	}
	text := Message(ev)
	if text == "" {
		return nil
	}
	if err := n.sender.Send(ctx, text); err != nil {
		return err
	}
	if err := n.seen.Mark(ctx, key); err != nil {
		n.logger.WarnContext(ctx, "remember notified event failed; a redelivery would notify again", "err", err)
	}
	return nil
}

// DedupeKey names what a message is about: the tickets of an order, or one
// refunded payment. It is built from ids that survive a database reset or
// restore (order ids are UUIDv7), unlike the outbox event_id, whose sequence
// starts again at 1 and would make new events look already notified.
func DedupeKey(ev events.OrderEvent) string {
	key := ev.EventType + ":" + ev.OrderID.String()
	if ev.ProviderTxnID != "" {
		key += ":" + ev.ProviderTxnID
	}
	return key
}

// Message is the text sent for ev, in Vietnamese for the buyer, or "" for
// events that are not notified.
func Message(ev events.OrderEvent) string {
	switch ev.EventType {
	case events.TicketIssued:
		return fmt.Sprintf("Đã xuất %d vé cho đơn %s (sự kiện %d): %s. Tổng %s đ.",
			len(ev.SeatIDs), ev.OrderID, ev.Event, strings.Join(ev.SeatIDs, ", "), money.FormatVND(ev.TotalVND))
	case events.OrderRefunded:
		return fmt.Sprintf("Đã hoàn %s đ cho đơn %s (giao dịch %s, lý do %s).",
			money.FormatVND(ev.AmountVND), ev.OrderID, ev.ProviderTxnID, ev.Reason)
	}
	return ""
}

// LogSender only logs the message; used when Telegram is not configured.
type LogSender struct{ Logger *slog.Logger }

// Send implements Sender.
func (s LogSender) Send(ctx context.Context, text string) error {
	s.Logger.InfoContext(ctx, "notification", "text", text)
	return nil
}

// TelegramAPI is the Bot API base URL.
const TelegramAPI = "https://api.telegram.org"

// Telegram sends messages to one chat through the Telegram Bot API.
type Telegram struct {
	client  *http.Client
	baseURL string
	token   string
	chatID  string
}

// NewTelegram sends as the bot with token to chatID. baseURL is TelegramAPI
// except in tests.
func NewTelegram(baseURL, token, chatID string, timeout time.Duration) *Telegram {
	return &Telegram{client: &http.Client{Timeout: timeout}, baseURL: baseURL, token: token, chatID: chatID}
}

// Send implements Sender. Errors never contain the request URL, which holds
// the bot token.
func (t *Telegram) Send(ctx context.Context, text string) error {
	form := url.Values{"chat_id": {t.chatID}, "text": {text}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+"/bot"+t.token+"/sendMessage",
		strings.NewReader(form.Encode()))
	if err != nil {
		return errors.New("telegram: build request failed")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := t.client.Do(req)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return fmt.Errorf("telegram send: %w", err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram send: status %d", res.StatusCode)
	}
	return nil
}

// seenTTL is far longer than any redelivery: a restart or rebalance
// redelivers at most the batch that was not committed yet.
const seenTTL = 7 * 24 * time.Hour

// RedisSeen keeps "notified:<DedupeKey>" keys.
type RedisSeen struct{ rdb *redis.Client }

// NewRedisSeen returns a Seen backed by rdb.
func NewRedisSeen(rdb *redis.Client) RedisSeen { return RedisSeen{rdb: rdb} }

func seenKey(key string) string { return "notified:" + key }

// Seen implements Seen.
func (s RedisSeen) Seen(ctx context.Context, key string) (bool, error) {
	n, err := s.rdb.Exists(ctx, seenKey(key)).Result()
	if err != nil {
		return false, fmt.Errorf("check notified: %w", err)
	}
	return n > 0, nil
}

// Mark implements Seen.
func (s RedisSeen) Mark(ctx context.Context, key string) error {
	if err := s.rdb.Set(ctx, seenKey(key), 1, seenTTL).Err(); err != nil {
		return fmt.Errorf("mark notified: %w", err)
	}
	return nil
}
