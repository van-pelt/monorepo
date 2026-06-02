-- +goose Up
-- payment.processed_events: consumer-side dedup table. See
-- migrations/account/20260301000000_processed_events.sql for the contract.
CREATE TABLE IF NOT EXISTS payment.processed_events (
    event_id     uuid PRIMARY KEY,
    topic        text NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_payment_processed_events_processed_at
    ON payment.processed_events (processed_at);

-- +goose Down
DROP TABLE IF EXISTS payment.processed_events;
