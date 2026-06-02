-- +goose Up
-- One schema per module: logical isolation within a single database.
-- Cross-schema foreign keys and JOINs are forbidden — references travel by
-- ID and are resolved through each module's api package (or, when split,
-- across the wire). This is the contract that keeps the codebase ready for
-- a microservice extraction.
--
-- Outbox tables themselves live inside each module's schema (see the
-- per-module migrations under migrations/<module>/) so events are owned by
-- the module that emits them.
CREATE SCHEMA IF NOT EXISTS account;
CREATE SCHEMA IF NOT EXISTS payment;

-- +goose Down
DROP SCHEMA IF EXISTS payment;
DROP SCHEMA IF EXISTS account;
