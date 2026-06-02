-- +goose Up
-- account.processed_events: consumer-side dedup table. Handlers wrapped with
-- consumers.Dedup insert into this table inside the same transaction as the
-- business write; a conflict on (event_id) means the message is a redelivery
-- and the handler is short-circuited.
--
-- One row per (event_id) — globally unique across producers because outbox
-- ids are UUIDs.
CREATE TABLE IF NOT EXISTS account.processed_events (
    event_id     uuid PRIMARY KEY,
    topic        text NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now()
);

-- Supports periodic GC by age (DELETE WHERE processed_at < now() - interval ...).
CREATE INDEX IF NOT EXISTS idx_account_processed_events_processed_at
    ON account.processed_events (processed_at);

-- +goose Down
DROP TABLE IF EXISTS account.processed_events;
