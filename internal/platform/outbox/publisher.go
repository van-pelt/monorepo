// Package outbox is the transactional outbox: a per-module Publisher that
// writes events to the caller's transaction, plus a Relay goroutine that
// reads each module's outbox table and forwards events to a Dispatcher
// (in-process EventBus or, in production, a broker like RabbitMQ).
//
// Per-module ownership: each module gets its own <schema>.outbox table.
// Modules own their events; no cross-schema reads.
package outbox

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/monorepo/internal/platform/observability/tracing"
	"github.com/monorepo/internal/platform/postgres"
)

// notifyChannel is the single Postgres LISTEN/NOTIFY channel the relay waits
// on. All schemas share it: a notify on any module's outbox wakes the relay,
// which then drains every schema's outbox in turn. Per-schema channels would
// require the relay to LISTEN to N channels, with no real benefit.
const notifyChannel = "outbox"

// Publisher writes an event into a single module's outbox table using the
// caller's Querier — so the event commits atomically with the business data
// (transactional outbox). It also issues NOTIFY to wake the relay.
//
// Each module gets its own Publisher instance scoped to its schema at the
// composition root. payment.service injects "payment", account.service (if it
// ever publishes) injects "account".
type Publisher interface {
	Publish(ctx context.Context, q postgres.Querier, topic string, payload any) error
}

func NewPublisher(schema string) Publisher {
	return &publisher{schema: schema}
}

type publisher struct {
	schema string
}

func (p *publisher) Publish(ctx context.Context, q postgres.Querier, topic string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	traceCtx, err := tracing.MarshalContext(ctx)
	if err != nil {
		return fmt.Errorf("marshal trace context: %w", err)
	}
	insertSQL := fmt.Sprintf(`INSERT INTO %s.outbox (id, topic, payload, trace_context) VALUES ($1, $2, $3, $4)`, p.schema)
	if _, err := q.ExecContext(ctx, insertSQL, uuid.New(), topic, data, traceCtx); err != nil {
		return fmt.Errorf("insert %s.outbox: %w", p.schema, err)
	}
	if _, err := q.ExecContext(ctx, "NOTIFY "+notifyChannel); err != nil {
		return fmt.Errorf("notify outbox: %w", err)
	}
	return nil
}
