// Package events defines the Kafka topics, event types and payloads
// (SPEC.md section 10). Payloads are written to the outbox and published
// unchanged by the relay.
package events

import (
	"time"

	"github.com/google/uuid"
)

// Topics.
const (
	TopicOrders  = "orders.v1"
	TopicTickets = "tickets.v1"
)

// Event types published on TopicOrders.
const (
	OrderHeld            = "order.held"
	OrderPaid            = "order.paid"
	OrderExpired         = "order.expired"
	OrderCancelled       = "order.cancelled"
	OrderRefundRequested = "order.refund_requested"
	OrderRefunded        = "order.refunded"
)

// TicketIssued is published on TopicTickets.
const TicketIssued = "ticket.issued"

// OrderEvent is the common payload of every order and ticket event.
//
// EventID, EventType and OccurredAt are filled in by the database when the
// outbox row is inserted; producers only set the order fields.
type OrderEvent struct {
	// EventID is the outbox row id; consumers deduplicate on it.
	EventID    int64     `json:"event_id"`
	EventType  string    `json:"event_type"`
	OccurredAt time.Time `json:"occurred_at"`
	OrderID    uuid.UUID `json:"order_id"`
	// Event is the id of the ticketed event (concert), not of this message.
	Event    int64    `json:"event"`
	UserID   int64    `json:"user_id"`
	SeatIDs  []string `json:"seat_ids"`
	TotalVND int64    `json:"total_vnd"`
}
