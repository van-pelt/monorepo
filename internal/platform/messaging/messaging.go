// Package messaging is the thin contract package that lets modules subscribe
// to events without knowing where they come from (in-process EventBus,
// RabbitMQ consumer, or a future broker). Only types live here; concrete
// implementations live in platform/eventbus, platform/outbox and
// platform/consumers.
package messaging

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

// Event is the message delivered to subscribers. Payload is the raw JSON
// that was stored in the outbox (or the broker body). ID is the producer's
// outbox row id, propagated end-to-end (outbox → AMQP MessageId →
// consumer) so handlers can dedup against <schema>.processed_events. ID
// is uuid.Nil when the source did not provide one (legacy publish path
// or a non-outbox dispatcher).
type Event struct {
	ID      uuid.UUID
	Topic   string
	Payload json.RawMessage
}

// Handler consumes an event. A returned error signals the delivery system
// (eventbus / RabbitMQ consumer) to retry or move to dead-letter. Handlers
// must be idempotent: delivery is at-least-once.
type Handler func(ctx context.Context, e Event) error

// Subscriber is what modules receive at the composition root. The concrete
// implementation differs by deployment shape (in-proc bus vs. broker
// consumer) but the surface is identical.
type Subscriber interface {
	Subscribe(topic string, h Handler)
}
