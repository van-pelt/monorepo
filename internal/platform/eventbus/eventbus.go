// Package eventbus is the in-process pub/sub. It implements
// messaging.Subscriber so modules can register handlers, and exposes
// Dispatch which satisfies outbox.Dispatcher for the relay to deliver
// outbox events synchronously to local subscribers.
//
// This is the dispatch backend used in the template / local-dev. In
// production it is replaced at the composition root by a broker-backed
// dispatcher (platform/rabbitmq) — modules don't change.
package eventbus

import (
	"context"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"github.com/monorepo/internal/platform/messaging"
)

// Bus is a topic-keyed registry of handlers.
type Bus struct {
	log zerolog.Logger

	mu   sync.RWMutex
	subs map[string][]messaging.Handler
}

func New(log zerolog.Logger) *Bus {
	return &Bus{
		log:  log.With().Str("component", "eventbus").Logger(),
		subs: make(map[string][]messaging.Handler),
	}
}

// Subscribe registers a handler for a topic. Wiring-time only — not safe to
// call concurrently with Dispatch.
func (b *Bus) Subscribe(topic string, h messaging.Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[topic] = append(b.subs[topic], h)
}

// Dispatch runs every subscriber for the topic in parallel goroutines, waits
// for all and returns the first non-nil error. This is the contract for
// outbox.Dispatcher: returning nil means the event was delivered to every
// subscriber successfully and the relay may ack (delete) the outbox row.
//
// We wait for all subscribers because fire-and-forget would silently lose
// failures and break at-least-once. All handlers must be idempotent: a single
// failure causes every subscriber to be re-invoked on the next retry.
func (b *Bus) Dispatch(ctx context.Context, eventID uuid.UUID, topic string, payload []byte) error {
	b.mu.RLock()
	handlers := b.subs[topic]
	b.mu.RUnlock()

	if len(handlers) == 0 {
		return nil
	}

	ev := messaging.Event{ID: eventID, Topic: topic, Payload: payload}
	results := make(chan error, len(handlers))
	for _, h := range handlers {
		go func(h messaging.Handler) {
			var err error
			defer func() {
				if p := recover(); p != nil {
					err = fmt.Errorf("handler panic: %v", p)
				}
				results <- err
			}()
			err = h(ctx, ev)
		}(h)
	}

	var firstErr error
	for range handlers {
		if err := <-results; err != nil {
			if firstErr == nil {
				firstErr = err
			}
			b.log.Error().Err(err).Str("topic", topic).Msg("event handler failed")
		}
	}
	return firstErr
}
