package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jmoiron/sqlx"
	"github.com/rs/zerolog"

	"github.com/monorepo/internal/shared/postgres"
)

// notifyChannel is the Postgres LISTEN/NOTIFY channel the relay waits on and
// Publish signals.
const notifyChannel = "outbox"

const reconnectDelay = 2 * time.Second

// jitterFraction is the ±fraction applied to backoff delays to spread out
// retries across instances when many events fail simultaneously.
const jitterFraction = 0.25

// Config tunes the relay's retry behaviour.
type Config struct {
	Interval    time.Duration // safety-net poll interval if a NOTIFY is missed or for delayed retries
	BatchSize   int           // max rows dispatched per cycle
	MaxAttempts int           // total deliveries (initial + retries) before move to outbox_dead
	BaseBackoff time.Duration // first retry delay; grows exponentially up to MaxBackoff
	MaxBackoff  time.Duration
}

// Engine is the default Publisher + Subscriber implementation: events are
// persisted in the outbox table inside the caller's transaction and a
// background relay dispatches them to in-process handlers with retry +
// dead-letter semantics.
type Engine struct {
	db  *sqlx.DB
	dsn string
	log zerolog.Logger
	cfg Config

	mu   sync.RWMutex
	subs map[string][]Handler
}

func NewEngine(db *sqlx.DB, dsn string, log zerolog.Logger, cfg Config) *Engine {
	return &Engine{
		db:   db,
		dsn:  dsn,
		log:  log.With().Str("component", "messaging").Logger(),
		cfg:  cfg,
		subs: make(map[string][]Handler),
	}
}

// Publish writes the event into the outbox table using the caller's Querier so
// it commits atomically with the business data, then issues a NOTIFY to wake
// the relay. The NOTIFY is itself transactional: the relay only sees the event
// after the caller's transaction commits.
func (e *Engine) Publish(ctx context.Context, q postgres.Querier, topic string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	const insertSQL = `INSERT INTO public.outbox (id, topic, payload) VALUES ($1, $2, $3)`
	if _, err := q.ExecContext(ctx, insertSQL, uuid.New(), topic, data); err != nil {
		return fmt.Errorf("insert outbox: %w", err)
	}
	if _, err := q.ExecContext(ctx, "NOTIFY "+notifyChannel); err != nil {
		return fmt.Errorf("notify outbox: %w", err)
	}
	return nil
}

// Subscribe registers a handler for a topic. Wiring-time only — not safe to
// call concurrently with dispatch.
func (e *Engine) Subscribe(topic string, h Handler) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.subs[topic] = append(e.subs[topic], h)
}

// Run drives the outbox relay until ctx is cancelled.
func (e *Engine) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := e.listenAndDispatch(ctx); err != nil && ctx.Err() == nil {
			e.log.Error().Err(err).Msg("relay connection lost, reconnecting")
			select {
			case <-ctx.Done():
			case <-time.After(reconnectDelay):
			}
		}
	}
}

// listenAndDispatch holds a dedicated connection (LISTEN/NOTIFY cannot run on
// a pooled connection) and dispatches batches until that connection fails.
// The safety-net poll (cfg.Interval) is what wakes us for *retried* events:
// NOTIFY only fires on Publish, not when next_retry_at expires.
func (e *Engine) listenAndDispatch(ctx context.Context) error {
	conn, err := pgx.Connect(ctx, e.dsn)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())

	if _, err := conn.Exec(ctx, "LISTEN "+notifyChannel); err != nil {
		return err
	}

	if err := e.drain(ctx); err != nil {
		e.log.Error().Err(err).Msg("drain failed")
	}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		waitCtx, cancel := context.WithTimeout(ctx, e.cfg.Interval)
		_, err := conn.WaitForNotification(waitCtx)
		cancel()

		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil && !errors.Is(err, context.DeadlineExceeded):
			return err
		}
		if err := e.drain(ctx); err != nil {
			e.log.Error().Err(err).Msg("drain failed")
		}
	}
}

func (e *Engine) drain(ctx context.Context) error {
	for {
		n, err := e.dispatchBatch(ctx)
		if err != nil {
			return err
		}
		if n < e.cfg.BatchSize {
			return nil
		}
	}
}

type outboxRow struct {
	ID        uuid.UUID `db:"id"`
	Topic     string    `db:"topic"`
	Payload   []byte    `db:"payload"`
	Attempts  int       `db:"attempts"`
	CreatedAt time.Time `db:"created_at"`
}

// dispatchBatch claims a batch with FOR UPDATE SKIP LOCKED, runs subscribers
// synchronously per row, and updates the row's fate (delete / retry / dead)
// inside the same transaction. Holding the transaction while handlers run is
// acceptable for a single-relay monolith; under multi-relay scale-out switch
// to a lease-based design (claim → process → ack in separate tx).
func (e *Engine) dispatchBatch(ctx context.Context) (int, error) {
	tx, err := e.db.BeginTxx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	const selectSQL = `
		SELECT id, topic, payload, attempts, created_at
		FROM public.outbox
		WHERE next_retry_at <= now()
		ORDER BY created_at
		LIMIT $1
		FOR UPDATE SKIP LOCKED`
	var rows []outboxRow
	if err := sqlx.SelectContext(ctx, tx, &rows, selectSQL, e.cfg.BatchSize); err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, tx.Commit()
	}

	for _, row := range rows {
		dispatchErr := e.dispatch(ctx, Event{Topic: row.Topic, Payload: row.Payload})
		if dispatchErr == nil {
			if err := e.ack(ctx, tx, row.ID); err != nil {
				return 0, err
			}
			continue
		}

		newAttempts := row.Attempts + 1
		if newAttempts >= e.cfg.MaxAttempts {
			if err := e.moveToDead(ctx, tx, row, dispatchErr); err != nil {
				return 0, err
			}
			e.log.Error().Err(dispatchErr).Str("topic", row.Topic).
				Str("id", row.ID.String()).Int("attempts", newAttempts).
				Msg("event moved to dead-letter")
			continue
		}

		delay := e.backoff(newAttempts)
		if err := e.scheduleRetry(ctx, tx, row.ID, dispatchErr, delay); err != nil {
			return 0, err
		}
		e.log.Warn().Err(dispatchErr).Str("topic", row.Topic).
			Str("id", row.ID.String()).Int("attempts", newAttempts).
			Dur("retry_in", delay).Msg("event scheduled for retry")
	}
	return len(rows), tx.Commit()
}

// dispatch runs every subscriber for ev in parallel and returns the first
// non-nil error. We wait for all handlers before returning so the relay can
// decide ack vs retry — fire-and-forget would silently lose failures and
// break at-least-once semantics.
//
// All handlers must be idempotent: a single failure causes every subscriber
// to be re-invoked on the next retry.
func (e *Engine) dispatch(ctx context.Context, ev Event) error {
	e.mu.RLock()
	handlers := e.subs[ev.Topic]
	e.mu.RUnlock()

	if len(handlers) == 0 {
		return nil
	}

	results := make(chan error, len(handlers))
	for _, h := range handlers {
		go func(h Handler) {
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
			e.log.Error().Err(err).Str("topic", ev.Topic).Msg("event handler failed")
		}
	}
	return firstErr
}

func (e *Engine) ack(ctx context.Context, tx *sqlx.Tx, id uuid.UUID) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM public.outbox WHERE id = $1`, id); err != nil {
		return fmt.Errorf("ack delete: %w", err)
	}
	return nil
}

func (e *Engine) scheduleRetry(ctx context.Context, tx *sqlx.Tx, id uuid.UUID, cause error, delay time.Duration) error {
	const sql = `
		UPDATE public.outbox
		SET attempts = attempts + 1,
		    next_retry_at = now() + make_interval(secs => $2),
		    last_error = $3
		WHERE id = $1`
	if _, err := tx.ExecContext(ctx, sql, id, delay.Seconds(), cause.Error()); err != nil {
		return fmt.Errorf("schedule retry: %w", err)
	}
	return nil
}

func (e *Engine) moveToDead(ctx context.Context, tx *sqlx.Tx, row outboxRow, cause error) error {
	const insertSQL = `
		INSERT INTO public.outbox_dead (id, topic, payload, created_at, attempts, last_error)
		VALUES ($1, $2, $3, $4, $5, $6)`
	if _, err := tx.ExecContext(ctx, insertSQL,
		row.ID, row.Topic, row.Payload, row.CreatedAt, row.Attempts+1, cause.Error()); err != nil {
		return fmt.Errorf("dead-letter insert: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM public.outbox WHERE id = $1`, row.ID); err != nil {
		return fmt.Errorf("dead-letter delete: %w", err)
	}
	return nil
}

// backoff returns BaseBackoff * 2^(attempts-1), capped at MaxBackoff, with
// ±jitterFraction jitter to avoid synchronised retry storms.
func (e *Engine) backoff(attempts int) time.Duration {
	d := e.cfg.BaseBackoff << (attempts - 1)
	if d <= 0 || d > e.cfg.MaxBackoff {
		d = e.cfg.MaxBackoff
	}
	jitter := 1 + jitterFraction*(2*rand.Float64()-1)
	return time.Duration(float64(d) * jitter)
}
