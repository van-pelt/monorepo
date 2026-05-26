// Package messaging is the asynchronous, decoupled communication subsystem
// for modules. Producers write events through Publisher; consumers register
// handlers through Subscriber. The default implementation is a transactional
// outbox feeding an in-process bus; when the system is split into services
// the same interfaces can be backed by a real broker without changing module
// code.
package messaging

import (
	"context"
	"encoding/json"

	"github.com/monorepo/internal/shared/postgres"
)

// Event is the message delivered to subscribers.
type Event struct {
	Topic   string
	Payload json.RawMessage
}

// Handler consumes an event. A returned error is logged by the dispatcher.
// Handlers must be idempotent: delivery is at-least-once.
type Handler func(ctx context.Context, e Event) error

// Publisher is the producer side. The Querier argument keeps the write in the
// same transaction as the business data (transactional outbox), so an event is
// never published without its data being committed.
type Publisher interface {
	Publish(ctx context.Context, q postgres.Querier, topic string, payload any) error
}

// Subscriber is the consumer side. Subscribe is wiring-time only: it is not
// safe to call concurrently with dispatch.
type Subscriber interface {
	Subscribe(topic string, h Handler)
}
