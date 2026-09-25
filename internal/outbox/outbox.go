// Package outbox writes messages to the transactional outbox. A message is
// inserted in the same transaction as the state change it describes; the
// relay (M3) publishes it to Kafka later. Nothing in the request path talks
// to Kafka directly.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/huy205-dev/ticketrush/internal/outbox/outboxdb"
)

// Message is one row to publish.
type Message struct {
	Topic     string
	Key       string // Kafka message key; the order id, to keep per-order ordering
	EventType string
	Payload   any // JSON-encoded; event_id, event_type and occurred_at are overwritten
}

// Write inserts m using db, which must be the transaction that performs the
// state change. It returns the outbox id.
func Write(ctx context.Context, db outboxdb.DBTX, m Message) (int64, error) {
	payload, err := json.Marshal(m.Payload)
	if err != nil {
		return 0, fmt.Errorf("encode %s payload: %w", m.EventType, err)
	}
	id, err := outboxdb.New(db).InsertMessage(ctx, outboxdb.InsertMessageParams{
		Topic:     m.Topic,
		MsgKey:    m.Key,
		EventType: m.EventType,
		Payload:   payload,
	})
	if err != nil {
		return 0, fmt.Errorf("insert outbox %s: %w", m.EventType, err)
	}
	return id, nil
}
