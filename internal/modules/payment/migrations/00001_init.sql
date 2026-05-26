-- +goose Up
-- Note: from_account_id / to_account_id are intentionally NOT foreign keys to
-- account.accounts. Modules reference each other only by ID and validate via
-- the owning module's port — this keeps the schema boundary clean and ready
-- for a future extraction into a separate service.
CREATE TABLE IF NOT EXISTS payment.payments (
    id              uuid PRIMARY KEY,
    from_account_id uuid NOT NULL,
    to_account_id   uuid NOT NULL,
    amount          bigint NOT NULL CHECK (amount > 0),
    currency        char(3) NOT NULL,
    status          text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_payments_from ON payment.payments (from_account_id);

-- +goose Down
DROP TABLE IF EXISTS payment.payments;
