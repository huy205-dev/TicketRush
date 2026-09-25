package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/huy205-dev/ticketrush/internal/platform/kafka"
	"github.com/huy205-dev/ticketrush/internal/platform/logging"
)

// Handler returns a Kafka handler that passes the records whose event type
// is one of types to fn, decoded, and skips the others. The type is read
// from the relay's header when present so skipped records are not decoded.
// A record that does not decode is skipped as a permanent failure.
func Handler(fn func(context.Context, OrderEvent) error, types ...string) kafka.Handler {
	return func(ctx context.Context, r *kgo.Record) error {
		if t := kafka.Header(r, HeaderEventType); t != "" && !slices.Contains(types, t) {
			return nil
		}
		var ev OrderEvent
		if err := json.Unmarshal(r.Value, &ev); err != nil {
			return kafka.Permanent(fmt.Errorf("decode event: %w", err))
		}
		if !slices.Contains(types, ev.EventType) {
			return nil
		}
		ctx = logging.With(ctx,
			slog.Int64("event_id", ev.EventID),
			slog.String("event_type", ev.EventType),
			slog.String("order_id", ev.OrderID.String()))
		return fn(ctx, ev)
	}
}
