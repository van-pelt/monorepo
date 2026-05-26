# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```sh
make config    # one-time: copy config/config.example.yaml → config/config.yaml
make up        # start local Postgres via deploy/docker-compose.yaml
make tidy      # go mod tidy
make run       # go run ./cmd/app
make build     # build into bin/app
make test      # go test ./...
make lint      # golangci-lint run
```

Run a single test: `go test ./internal/shared/postgres -run TestUnitOfWork_Do_Commit -v`

Config is loaded from `config/config.yaml` (gitignored; bootstrap from `config/config.example.yaml`). Any field is overridable via `APP_*` env vars (key separator `_`), e.g. `APP_DB_DSN`. Default `auto_migrate: false` — when developing migrations, set `db.auto_migrate: true` or run goose manually.

## Architecture overview

Modular monolith in Go (1.25), HTTP via Fiber v2, Postgres via sqlx + pgx/v5. Each module under `internal/modules/<name>/` is a vertical slice with layers `transport → service → domain ← repository`. The composition root is `internal/app/app.go`.

**README.md** is the canonical architecture doc — read it before making non-trivial changes. Below are conventions that are easy to miss or contradict common Go idioms.

### Module boundaries (enforced by depguard)

A module is visible to others **only** through its leaf port package (`<name>port/`). Importing another module's `domain`/`service`/`repository` will fail `golangci-lint`. The rules are in `.golangci.yml`; when adding a new module, extend that file with a matching block.

Between schemas there are **no foreign keys** — references are by ID, validated through ports. This is a deliberate contract for the microservice-split path; don't add cross-schema FKs.

### Service layer is provider-side interface (deliberate)

Each module's `service` package declares `type Service interface { ... }` next to an **unexported** `type service struct`; `New(...)` returns the interface. Consumers (HTTP handler, event handler, port adapter) all depend on `service.Service`.

This contradicts the Go-idiomatic "consumer defines a narrow interface" pattern — the user has explicitly chosen this and rejected the alternative. Do **not** propose moving the interface into the consumer package "for testability". See `memory/feedback_service_interface_location.md`.

### Transport layer split

In each `transport/` directory: `handler.go` holds only `Handler` + `Register` + route methods; `dto.go` holds request/response structs and mappers. Keep handlers slim — DTOs do not belong in `handler.go`.

### Cross-module communication

- **Synchronous** — through the port: caller needs an immediate answer for a decision (e.g. `payment` validates accounts via `accountport.AccountProvider`).
- **Asynchronous** — through `messaging.Publisher` / `Subscriber`: side effects with at-least-once delivery (e.g. `payment.created` triggers `account.Transfer`).

The decision matrix is in README §«Когда использовать порт / outbox». Don't use ports for side effects, don't use outbox when you need a return value.

### Database / transactions

- `internal/shared/postgres/querier.go` aliases `sqlx.ExtContext` as `Querier`. Repository methods take `Querier`, so the same code runs on `*sqlx.DB` (no tx) and `*sqlx.Tx` (inside UoW).
- `UnitOfWork.Do(ctx, fn)` for transactions without a return value; `InTx[T any](ctx, uow, fn)` for transactions that produce a typed result. Both share rollback/commit semantics — only one defines them.
- Failed `Commit` does **not** trigger a follow-up `Rollback` (the tx is already terminated in `database/sql`; redundant Rollback returns `ErrTxDone` and obscures the real error). Don't add it back.

### Outbox / messaging

`internal/shared/messaging/engine.go` is the single implementation of `Publisher` + `Subscriber`. Key semantics:

- `Publish` writes to `public.outbox` + `NOTIFY outbox` in the caller's transaction. NOTIFY is transactional in Postgres — relay sees the event only after the caller commits.
- Relay holds a **dedicated** pgx connection (LISTEN can't run on the sqlx pool), waits via `WaitForNotification` with `cfg.Interval` as safety-net poll.
- `dispatchBatch` is **synchronous** per row: it runs all subscribers (parallel goroutines + result channel) and **waits for all** before deciding ack/retry/dead. Don't go back to fire-and-forget — that broke at-least-once.
- Three outcomes per row inside one tx: `ack` (DELETE), `retry` (UPDATE attempts + next_retry_at + last_error), `dead` (INSERT into `outbox_dead` + DELETE). Backoff is exponential with ±25% jitter.
- Handlers **must be idempotent** — same row may be redelivered (crash between dispatch and commit, or any subscriber error retries all subscribers for that topic).
- Transaction is held open while handlers run; acceptable for single-relay monolith. Multi-relay scale-out needs a lease-based redesign (claim → process → ack in separate tx) — see README.

### Migrations

Two-layer:
- `migrations/00001_init.sql` — schemas + shared `outbox`/`outbox_dead`, embedded via `migrations/migrations.go`.
- Per-module: `internal/modules/<name>/migrations/*.sql`, embedded in `<name>/module.go` via `//go:embed`.

Each module gets its own `goose_db_version` table (per-schema). `internal/app/migrate.go` runs base first, then each module against its schema.

### Logging

`zerolog`; component is set via `.With().Str("component", "...").Logger()`. New shared packages should set their own component label.

## Roadmap (not yet implemented)

README §«Телеметрия и отказоустойчивость (план)» documents what's deliberately missing and the planned order of introduction (OTel SDK, /healthz+/readyz, bulkhead per topic, reconnect backoff with jitter, etc.) — consult before proposing observability/resilience features so the choice of library/integration point stays consistent.
