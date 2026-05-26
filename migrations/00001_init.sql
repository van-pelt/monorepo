-- +goose Up
-- One schema per module: logical isolation within a single database.
CREATE SCHEMA IF NOT EXISTS account;
CREATE SCHEMA IF NOT EXISTS payment;

-- Shared transactional outbox: events are written here in the same
-- transaction as the business data and dispatched later by the relay.
-- The relay deletes rows on successful delivery, so the table only ever
-- holds the unpublished/failing backlog.
CREATE TABLE IF NOT EXISTS public.outbox (
    id            uuid PRIMARY KEY,
    topic         text NOT NULL,
    payload       jsonb NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    -- attempts increments on each failed dispatch; next_retry_at delays the
    -- next pick-up after a failure with exponential backoff + jitter.
    attempts      integer NOT NULL DEFAULT 0,
    next_retry_at timestamptz NOT NULL DEFAULT now(),
    last_error    text
);

-- Index drives SELECT ... WHERE next_retry_at <= now() ORDER BY created_at.
CREATE INDEX IF NOT EXISTS idx_outbox_next_retry ON public.outbox (next_retry_at, created_at);

-- Dead-letter table: events that exceeded max_attempts. Kept indefinitely
-- for operator inspection; no relay reads it.
CREATE TABLE IF NOT EXISTS public.outbox_dead (
    id          uuid PRIMARY KEY,
    topic       text NOT NULL,
    payload     jsonb NOT NULL,
    created_at  timestamptz NOT NULL,
    attempts    integer NOT NULL,
    last_error  text,
    failed_at   timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS public.outbox_dead;
DROP TABLE IF EXISTS public.outbox;
DROP SCHEMA IF EXISTS payment;
DROP SCHEMA IF EXISTS account;
