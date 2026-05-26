-- +goose Up
CREATE TABLE IF NOT EXISTS account.accounts (
    id         uuid PRIMARY KEY,
    owner_id   uuid NOT NULL,
    currency   char(3) NOT NULL,
    balance    bigint NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_accounts_owner ON account.accounts (owner_id);

-- +goose Down
DROP TABLE IF EXISTS account.accounts;
