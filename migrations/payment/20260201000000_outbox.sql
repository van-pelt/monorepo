-- +goose Up
-- payment.outbox: transactional outbox owned by the payment module. See
-- comments in migrations/account/20260201000000_outbox.sql for the contract.
CREATE TABLE IF NOT EXISTS payment.outbox (
    id            uuid PRIMARY KEY,
    topic         text NOT NULL,
    payload       jsonb NOT NULL,
    trace_context jsonb,
    created_at    timestamptz NOT NULL DEFAULT now(),
    attempts      integer NOT NULL DEFAULT 0,
    next_retry_at timestamptz NOT NULL DEFAULT now(),
    last_error    text
);

CREATE INDEX IF NOT EXISTS idx_payment_outbox_next_retry
    ON payment.outbox (next_retry_at, created_at);

CREATE TABLE IF NOT EXISTS payment.outbox_dead (
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
DROP TABLE IF EXISTS payment.outbox_dead;
DROP TABLE IF EXISTS payment.outbox;
