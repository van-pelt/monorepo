-- +goose Up
-- account.outbox: transactional outbox owned by the account module. Events
-- are inserted in the same transaction as the business write; the relay in
-- platform/outbox forwards them to the Dispatcher and deletes the row on
-- success.
CREATE TABLE IF NOT EXISTS account.outbox (
    id            uuid PRIMARY KEY,
    topic         text NOT NULL,
    payload       jsonb NOT NULL,
    -- trace_context carries the W3C TraceContext propagation map captured at
    -- Publish time. The relay restores it before calling the Dispatcher so
    -- the producer's trace continues across the async boundary.
    trace_context jsonb,
    created_at    timestamptz NOT NULL DEFAULT now(),
    -- attempts increments on each failed dispatch; next_retry_at delays the
    -- next pick-up after a failure (exponential backoff + jitter).
    attempts      integer NOT NULL DEFAULT 0,
    next_retry_at timestamptz NOT NULL DEFAULT now(),
    last_error    text
);

-- Drives SELECT ... WHERE next_retry_at <= now() ORDER BY created_at.
CREATE INDEX IF NOT EXISTS idx_account_outbox_next_retry
    ON account.outbox (next_retry_at, created_at);

-- account.outbox_dead: events that exceeded max_attempts. Kept indefinitely
-- for operator inspection; no relay reads it.
CREATE TABLE IF NOT EXISTS account.outbox_dead (
    id            uuid PRIMARY KEY,
    topic         text NOT NULL,
    payload       jsonb NOT NULL,
    trace_context jsonb,
    created_at    timestamptz NOT NULL,
    attempts      integer NOT NULL,
    last_error    text,
    failed_at     timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS account.outbox_dead;
DROP TABLE IF EXISTS account.outbox;
