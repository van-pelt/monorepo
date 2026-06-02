package outbox

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jmoiron/sqlx"
	"github.com/rs/zerolog"

	"github.com/monorepo/internal/platform/observability/metrics"
	"github.com/monorepo/internal/platform/observability/tracing"
)

// Reconnect backoff applied when the relay's dedicated LISTEN connection
// drops (Postgres restart, network blip). Exponential growth up to a cap
// with ±jitterFraction so N pods don't all reconnect on the same
// millisecond — flat fixed delay would mean a thundering herd hitting PG
// the moment it comes back.
const (
	relayReconnectBase = 1 * time.Second
	relayReconnectMax  = 30 * time.Second
)

// jitterFraction is the ±fraction applied to backoff delays to spread out
// retries across instances when many events fail simultaneously.
const jitterFraction = 0.25

// backlogPollInterval controls how often the relay refreshes the
// outbox_backlog gauge. Independent of the dispatch loop's cfg.Interval —
// dispatch reacts to NOTIFY immediately, gauge can be lazier.
const backlogPollInterval = 30 * time.Second

// Config tunes the relay's retry behaviour.
type Config struct {
	Interval    time.Duration // safety-net poll interval if a NOTIFY is missed or for delayed retries
	BatchSize   int           // max rows dispatched per schema per cycle
	MaxAttempts int           // total deliveries (initial + retries) before move to <schema>.outbox_dead
	BaseBackoff time.Duration // first retry delay; grows exponentially up to MaxBackoff
	MaxBackoff  time.Duration
}

// Dispatcher delivers a payload outward — to an in-process EventBus or to a
// broker (RabbitMQ). Returning nil means the destination has accepted the
// message; the relay then deletes the outbox row. Any error triggers
// retry/dead-letter according to Config.
//
// eventID is the outbox row's primary key, passed downstream so consumers
// can dedup against <schema>.processed_events. AMQP-based dispatchers set
// it as amqp.Publishing.MessageId; in-process EventBus puts it on the
// messaging.Event passed to handlers.
type Dispatcher interface {
	Dispatch(ctx context.Context, eventID uuid.UUID, topic string, payload []byte) error
}

// Relay drives the outbox loop: LISTENs for NOTIFY, drains each schema's
// outbox in batches with FOR UPDATE SKIP LOCKED, calls Dispatcher per row
// and ack/retry/dead in the same transaction. Holding the transaction while
// the Dispatcher runs is acceptable for a single-relay monolith; multi-relay
// scale-out should switch to a lease-based design.
type Relay struct {
	db         *sqlx.DB
	dsn        string
	schemas    []string
	dispatcher Dispatcher
	log        zerolog.Logger
	cfg        Config
}

func NewRelay(db *sqlx.DB, dsn string, schemas []string, dispatcher Dispatcher, log zerolog.Logger, cfg Config) *Relay {
	return &Relay{
		db:         db,
		dsn:        dsn,
		schemas:    schemas,
		dispatcher: dispatcher,
		log:        log.With().Str("component", "outbox-relay").Logger(),
		cfg:        cfg,
	}
}

// Run drives the relay until ctx is cancelled.
func (r *Relay) Run(ctx context.Context) {
	go r.refreshBacklog(ctx)
	attempt := 0
	for ctx.Err() == nil {
		err := r.listenAndDispatch(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			attempt++
			wait := relayReconnectBackoff(attempt)
			r.log.Error().Err(err).Int("attempt", attempt).Dur("retry_in", wait).
				Msg("relay connection lost, reconnecting")
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			continue
		}
		attempt = 0
	}
}

// relayReconnectBackoff returns relayReconnectBase * 2^(attempt-1) capped
// at relayReconnectMax, with ±jitterFraction randomness.
func relayReconnectBackoff(attempt int) time.Duration {
	d := relayReconnectBase << (attempt - 1)
	if d <= 0 || d > relayReconnectMax {
		d = relayReconnectMax
	}
	jitter := 1 + jitterFraction*(2*rand.Float64()-1)
	return time.Duration(float64(d) * jitter)
}

// refreshBacklog periodically counts unpublished rows in each schema's
// outbox and writes the values into the OutboxBacklog gauge. A sustained
// non-zero backlog is the primary signal that the dispatcher is stuck.
func (r *Relay) refreshBacklog(ctx context.Context) {
	ticker := time.NewTicker(backlogPollInterval)
	defer ticker.Stop()
	for {
		r.updateBacklog(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Relay) updateBacklog(ctx context.Context) {
	for _, schema := range r.schemas {
		var n int
		sql := fmt.Sprintf(`SELECT count(*) FROM %s.outbox`, schema)
		if err := r.db.GetContext(ctx, &n, sql); err != nil {
			if ctx.Err() == nil {
				r.log.Warn().Err(err).Str("schema", schema).Msg("backlog query failed")
			}
			continue
		}
		metrics.OutboxBacklog.WithLabelValues(schema).Set(float64(n))
	}
}

// listenAndDispatch holds a dedicated pgx connection (LISTEN/NOTIFY cannot
// run on a pooled sqlx connection) and dispatches batches until that
// connection fails. The safety-net poll (cfg.Interval) is what wakes us for
// *retried* events: NOTIFY only fires on Publish, not when next_retry_at
// expires.
func (r *Relay) listenAndDispatch(ctx context.Context) error {
	conn, err := pgx.Connect(ctx, r.dsn)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())

	if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
		return err
	}

	if err := r.drainAll(ctx); err != nil {
		r.log.Error().Err(err).Msg("drain failed")
	}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		waitCtx, cancel := context.WithTimeout(ctx, r.cfg.Interval)
		_, err := conn.WaitForNotification(waitCtx)
		cancel()

		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil && !errors.Is(err, context.DeadlineExceeded):
			return err
		}
		if err := r.drainAll(ctx); err != nil {
			r.log.Error().Err(err).Msg("drain failed")
		}
	}
}

// drainAll cycles through every schema until each pass returns less than
// BatchSize rows (i.e. each module's queue is below the threshold for now).
func (r *Relay) drainAll(ctx context.Context) error {
	for {
		more := false
		for _, schema := range r.schemas {
			n, err := r.dispatchBatch(ctx, schema)
			if err != nil {
				return err
			}
			if n >= r.cfg.BatchSize {
				more = true
			}
		}
		if !more {
			return nil
		}
	}
}

type outboxRow struct {
	ID           uuid.UUID `db:"id"`
	Topic        string    `db:"topic"`
	Payload      []byte    `db:"payload"`
	TraceContext []byte    `db:"trace_context"`
	Attempts     int       `db:"attempts"`
	CreatedAt    time.Time `db:"created_at"`
}

// dispatchBatch claims a batch from <schema>.outbox with FOR UPDATE SKIP
// LOCKED, runs the Dispatcher synchronously per row, and updates the row's
// fate (delete / retry / dead) inside the same transaction.
func (r *Relay) dispatchBatch(ctx context.Context, schema string) (int, error) {
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	selectSQL := fmt.Sprintf(`
		SELECT id, topic, payload, trace_context, attempts, created_at
		FROM %s.outbox
		WHERE next_retry_at <= now()
		ORDER BY created_at
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, schema)
	var rows []outboxRow
	if err := sqlx.SelectContext(ctx, tx, &rows, selectSQL, r.cfg.BatchSize); err != nil {
		return 0, fmt.Errorf("select %s.outbox: %w", schema, err)
	}
	if len(rows) == 0 {
		return 0, tx.Commit()
	}

	for _, row := range rows {
		// Restore the producer's trace context so the Dispatcher and any
		// downstream consumer's span are children of the original request
		// span, continuing the trace across the async boundary.
		dispatchCtx := tracing.UnmarshalContext(ctx, row.TraceContext)
		start := time.Now()
		dispatchErr := r.dispatcher.Dispatch(dispatchCtx, row.ID, row.Topic, row.Payload)
		metrics.OutboxDispatchDuration.WithLabelValues(schema, row.Topic).Observe(time.Since(start).Seconds())
		if dispatchErr == nil {
			if err := r.ack(ctx, tx, schema, row.ID); err != nil {
				return 0, err
			}
			continue
		}

		newAttempts := row.Attempts + 1
		if newAttempts >= r.cfg.MaxAttempts {
			if err := r.moveToDead(ctx, tx, schema, row, dispatchErr); err != nil {
				return 0, err
			}
			metrics.OutboxDeadTotal.WithLabelValues(schema, row.Topic).Inc()
			r.log.Error().Err(dispatchErr).
				Str("schema", schema).Str("topic", row.Topic).
				Str("id", row.ID.String()).Int("attempts", newAttempts).
				Msg("event moved to dead-letter")
			continue
		}

		delay := r.backoff(newAttempts)
		if err := r.scheduleRetry(ctx, tx, schema, row.ID, dispatchErr, delay); err != nil {
			return 0, err
		}
		r.log.Warn().Err(dispatchErr).
			Str("schema", schema).Str("topic", row.Topic).
			Str("id", row.ID.String()).Int("attempts", newAttempts).
			Dur("retry_in", delay).Msg("event scheduled for retry")
	}
	return len(rows), tx.Commit()
}

func (r *Relay) ack(ctx context.Context, tx *sqlx.Tx, schema string, id uuid.UUID) error {
	sql := fmt.Sprintf(`DELETE FROM %s.outbox WHERE id = $1`, schema)
	if _, err := tx.ExecContext(ctx, sql, id); err != nil {
		return fmt.Errorf("ack delete: %w", err)
	}
	return nil
}

func (r *Relay) scheduleRetry(ctx context.Context, tx *sqlx.Tx, schema string, id uuid.UUID, cause error, delay time.Duration) error {
	sql := fmt.Sprintf(`
		UPDATE %s.outbox
		SET attempts = attempts + 1,
		    next_retry_at = now() + make_interval(secs => $2),
		    last_error = $3
		WHERE id = $1`, schema)
	if _, err := tx.ExecContext(ctx, sql, id, delay.Seconds(), cause.Error()); err != nil {
		return fmt.Errorf("schedule retry: %w", err)
	}
	return nil
}

func (r *Relay) moveToDead(ctx context.Context, tx *sqlx.Tx, schema string, row outboxRow, cause error) error {
	insertSQL := fmt.Sprintf(`
		INSERT INTO %s.outbox_dead (id, topic, payload, trace_context, created_at, attempts, last_error)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, schema)
	if _, err := tx.ExecContext(ctx, insertSQL,
		row.ID, row.Topic, row.Payload, row.TraceContext, row.CreatedAt, row.Attempts+1, cause.Error()); err != nil {
		return fmt.Errorf("dead-letter insert: %w", err)
	}
	delSQL := fmt.Sprintf(`DELETE FROM %s.outbox WHERE id = $1`, schema)
	if _, err := tx.ExecContext(ctx, delSQL, row.ID); err != nil {
		return fmt.Errorf("dead-letter delete: %w", err)
	}
	return nil
}

// backoff returns BaseBackoff * 2^(attempts-1), capped at MaxBackoff, with
// ±jitterFraction jitter to avoid synchronised retry storms.
func (r *Relay) backoff(attempts int) time.Duration {
	d := r.cfg.BaseBackoff << (attempts - 1)
	if d <= 0 || d > r.cfg.MaxBackoff {
		d = r.cfg.MaxBackoff
	}
	jitter := 1 + jitterFraction*(2*rand.Float64()-1)
	return time.Duration(float64(d) * jitter)
}
