package consumers

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/monorepo/internal/platform/messaging"
	"github.com/monorepo/internal/platform/postgres"
)

// ErrNoEventID is returned by Dedup-wrapped handlers when a delivery arrives
// without a propagated event id (Event.ID is uuid.Nil). At-least-once dedup
// has no way to detect a redelivery in that case, so we fail the handler
// (→ DLQ) rather than risk double processing.
var ErrNoEventID = errors.New("consumers: event has no id; cannot dedup")

// TxHandler is a handler that performs its business work inside the
// dedup transaction. q is the *sqlx.Tx the wrapper opened; using it
// guarantees the business write commits if and only if the
// processed_events row also commits — exactly-once effect against this
// database.
//
// Handlers that need to commit work to other systems (HTTP calls, external
// queues) cannot use this helper — those side effects are not transactional
// and require a different idempotency strategy (the caller's own dedup key).
type TxHandler func(ctx context.Context, q postgres.Querier, e messaging.Event) error

// Dedup wraps a TxHandler with consumer-side idempotency backed by
// <schema>.processed_events.
//
// Per delivery, in one transaction:
//
//  1. INSERT INTO <schema>.processed_events (event_id, topic) VALUES (...)
//     ON CONFLICT DO NOTHING
//  2. If 0 rows affected → already processed; commit empty tx → handler is
//     skipped, message acked.
//  3. Else → run TxHandler with the same tx; handler error rolls back both
//     the dedup mark and any business writes, so the message is redelivered.
//
// Requires Event.ID to be set; messages without one return ErrNoEventID
// (→ DLQ) rather than risk double processing silently.
//
// `schema` is the consumer-side schema that owns the processed_events
// table — typically the same as the consumer module's name.
func Dedup(uow *postgres.UnitOfWork, schema string, h TxHandler) messaging.Handler {
	insertSQL := fmt.Sprintf(
		`INSERT INTO %s.processed_events (event_id, topic) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		schema,
	)
	return func(ctx context.Context, e messaging.Event) error {
		if e.ID == uuid.Nil {
			return ErrNoEventID
		}
		return uow.Do(ctx, func(ctx context.Context, q postgres.Querier) error {
			res, err := q.ExecContext(ctx, insertSQL, e.ID, e.Topic)
			if err != nil {
				return fmt.Errorf("processed_events insert: %w", err)
			}
			n, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("processed_events rows affected: %w", err)
			}
			if n == 0 {
				return nil
			}
			return h(ctx, q, e)
		})
	}
}
