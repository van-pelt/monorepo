# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```sh
make config    # one-time: copy config/config.example.yaml → config/config.yaml
make up        # start local Postgres via deploy/docker-compose.yaml
make tidy      # go mod tidy
make migrate   # apply all SQL migrations (run before `make run` on a fresh DB)
make run       # go run ./cmd/api
make build     # build bin/api and bin/migrate
make test      # go test ./...
make lint      # golangci-lint run
```

Run a single test: `go test ./internal/platform/postgres -run TestUnitOfWork_Do_Commit -v`

Config is loaded from `config/config.yaml` (gitignored; bootstrap from `config/config.example.yaml`). Any field is overridable via `APP_*` env vars (key separator `_`), e.g. `APP_DB_DSN`. Migrations are NOT auto-applied — run `make migrate` (calls `cmd/migrate`).

## Architecture overview

Modular monolith in Go (1.25), HTTP via Fiber v2, Postgres via sqlx + pgx/v5. Each module under `internal/modules/<name>/` is a vertical slice. Internal layers `service → domain ← repository` live in `<name>/internal/{...}/`; the public contract is `<name>/api/`. HTTP handlers live in `cmd/api/handlers/` and depend only on `<name>/api.Service` — they never see a module's internals. The composition root is `cmd/api/main.go`.

**README.md** is the canonical architecture doc — read it before making non-trivial changes. Below are conventions that are easy to miss.

### Module boundaries (enforced by the Go compiler)

A module is visible to others **only** through its leaf `api/` package. The Go `internal/` package mechanism guarantees this: anything under `internal/modules/<name>/internal/` is unreachable from outside that module — the compiler refuses such imports. No depguard rules needed for cross-module isolation.

Between schemas there are **no foreign keys** — references are by ID, validated through `<name>/api`. This is a deliberate contract for the microservice-split path; don't add cross-schema FKs.

### Public API package convention

Each module exposes `internal/modules/<name>/api/` (package `api`). Consumers import with an alias: `accountapi "github.com/monorepo/internal/modules/account/api"`. The `api/` package owns the module's DTOs (`AccountInfo`), interfaces (`AccountProvider`), event topics + payloads (`TopicPaymentCreated`, `PaymentCreated`), and sentinel errors (`ErrAccountNotFound`).

### Service layer is provider-side interface (deliberate)

Each module's `internal/service` package declares `type Service interface { ... }` next to an **unexported** `type service struct`; `New(...)` returns the interface. Consumers (HTTP handler, event handler, api adapter) all depend on `service.Service`.

This contradicts the Go-idiomatic "consumer defines a narrow interface" pattern — the user has explicitly chosen this and rejected the alternative. Do **not** propose moving the interface into the consumer package "for testability". See `memory/feedback_service_interface_location.md`.

### HTTP layer (cmd/api/handlers)

Every HTTP endpoint lives in `cmd/api/handlers/<entity>_handler.go` — one file per entity, holding handler + DTOs (request/response structs + mappers). Handlers depend on `<name>api.Service` interfaces only. When a module is extracted to gRPC, only the `api.Service` implementation changes (in-process adapter → gRPC client); handlers don't move.

`cmd/api/router.go` wires every handler to the `/api/v1` group. New endpoints: add to the relevant `<entity>_handler.go` or create a new file, then register in `router.go`.

### Fiber zero-copy strings (gotcha)

Most Fiber `*Ctx` string accessors return **zero-copy** strings backed by the pooled request buffer (`b2s` via `unsafe.Pointer`). The bytes are reused for the next request once the Ctx is recycled — any string handed to a structure that outlives the request silently mutates.

**Real example caught in this repo:** `metrics.Middleware` passed `c.Method()` straight to `prometheus.CounterVec.WithLabelValues(...)`. After a `POST` request, the buffer holding `"POST"` was reused for the next `GET /metrics`, overwriting `P-O-S-T` with `G-E-T` — and the Prometheus label silently became `"GETT"`. Same risk in `tracing.Middleware` (span attributes outlive the request via the batched exporter).

**Unsafe accessors** (consumer must copy if storing beyond request lifetime): `c.Method()`, `c.Path()`, `c.OriginalURL()`, `c.Get(...)`, `c.Params(...)`, `c.Query(...)`, `c.Body()`, `c.Response().Body()`, `c.Response().Header.X()`.

**Forcing a copy:**
- strings → `strings.Clone(s)` (Go 1.18+) or `string([]byte(s))`
- byte slices → `append([]byte(nil), b...)`
- `fmt.Sprintf("...", c.Method(), ...)` and `s1 + s2` already produce new strings — no extra clone needed
- `string(c.Response().Header.ContentType())` — `string([]byte)` allocates, safe

**Synchronous usage is fine:** zerolog `.Str("method", c.Method()).Msg(...)` writes the field immediately in `.Msg()` and stops referencing the string. The risk is async storage: metrics, tracing spans, queued events, goroutines spawned per request.

**Rule of thumb for PRs touching middleware:** any `c.X()` whose result reaches Prometheus / OTel span / channel / goroutine / cache → wrap in `strings.Clone` / explicit copy. If the result dies inside the handler, leave it alone.

### Cross-module communication

- **Synchronous** — through `<name>/api`: caller needs an immediate answer for a decision (e.g. `payment` validates accounts via `accountapi.AccountProvider`).
- **Asynchronous** — through `messaging.Publisher` / `Subscriber`: side effects with at-least-once delivery (e.g. `payment.created` triggers `account.Transfer`).

The decision matrix is in README. Don't use api for side effects, don't use outbox when you need a return value.

### Database / transactions

- `internal/platform/postgres/querier.go` aliases `sqlx.ExtContext` as `Querier`. Repository methods take `Querier`, so the same code runs on `*sqlx.DB` (no tx) and `*sqlx.Tx` (inside UoW).
- `UnitOfWork.Do(ctx, fn)` for transactions without a return value; `InTx[T any](ctx, uow, fn)` for transactions that produce a typed result. Both share rollback/commit semantics — only one defines them.
- Failed `Commit` does **not** trigger a follow-up `Rollback` (the tx is already terminated in `database/sql`; redundant Rollback returns `ErrTxDone` and obscures the real error). Don't add it back.

### Outbox / messaging

End-to-end async path: business write → outbox row → RabbitMQ → consumer queue → module handler. Two independent at-least-once chains (outbox→broker, broker→handler) — handlers must be idempotent. The event id is propagated end-to-end (`outbox.id` → `amqp.Publishing.MessageId` → `messaging.Event.ID`) so consumers can dedup against `<schema>.processed_events` via `consumers.Dedup`.

Packages and contracts:

- `internal/platform/messaging/` — only `Event`, `Handler`, `Subscriber` types. No implementation. The thin contract every other package speaks.
- `internal/platform/outbox/` — `Publisher` writes into `<schema>.outbox` + `NOTIFY outbox` in the caller's transaction. `Relay` reads every configured schema's outbox table, calls a `Dispatcher` per row, ack/retry/dead in the same tx.
- `internal/platform/rabbitmq/` — AMQP client (connection + topic exchange + DLX declared at Connect). `Publisher` implements `outbox.Dispatcher` by publishing to the topic exchange with routing key = event topic.
- `internal/platform/consumers/` — `Subscriber` implements `messaging.Subscriber`. Per registration creates queue `<consumer-name>.<topic>` bound to the exchange; queue is configured with `x-dead-letter-exchange=<DLX>`, plus a `.dlq` queue bound to DLX with the same routing key. On handler error → `nack(requeue=false)` → message lands in `.dlq`. Each subscription is bulkheaded: a sized semaphore caps in-flight handlers per (consumer, topic) and AMQP prefetch matches it, so one slow topic doesn't starve others. Per-handler ctx is wrapped with `HandlerTimeout` (default 30s); panics are recovered → DLQ. A separate poller refreshes `consumer_queue_depth` via passive `QueueDeclare` every `QueueDepthPollInterval`. Settings come from `cfg.Consumers`; per-topic concurrency overrides are wired in code at the composition root via `consumers.Config{TopicConcurrency: ...}`.
- `internal/platform/eventbus/` — in-process `Bus` (also implements `messaging.Subscriber` and satisfies `outbox.Dispatcher`). Not wired into production composition but kept available for use cases that don't need durability or the broker.

Key semantics:

- Per-module outbox: each business module owns its `<schema>.outbox` and `<schema>.outbox_dead`. Module migrations create them; relay iterates the configured schema list at startup.
- `outbox.Publisher` is scoped to a schema — composition root creates one per writing module (e.g. `outbox.NewPublisher("payment")`).
- Relay holds a **dedicated** pgx connection (LISTEN can't run on the sqlx pool); a single `outbox` NOTIFY channel wakes it for any schema. Safety-net poll via `cfg.Interval`. Reconnect on conn loss uses exponential backoff (1s base, 30s cap) + ±25% jitter — across pods the dial attempts are staggered so the just-recovered Postgres isn't dogpiled.
- `dispatchBatch` per schema: `FOR UPDATE SKIP LOCKED`, calls `Dispatcher.Dispatch` synchronously per row, ack/retry/dead inside one tx.
- Three outcomes per row: `ack` (DELETE), `retry` (UPDATE attempts + next_retry_at + last_error, exponential backoff with ±25% jitter), `dead` (INSERT into `<schema>.outbox_dead` + DELETE).
- AMQP consumer: manual ack, prefetch = subscription concurrency. On handler success → ack. On handler error or panic → `nack(requeue=false)` → routed to DLX → lands in `<consumer>.<topic>.dlq`.
- Consumer-side idempotency: `consumers.Dedup(uow, schema, handler)` wraps a `TxHandler` with a per-message tx: `INSERT INTO <schema>.processed_events (event_id, topic) ON CONFLICT DO NOTHING`. If 0 rows → redelivery → skip + ack. Else run handler in the same tx — business writes commit iff the dedup mark commits. Each consumer module owns its own `processed_events` table (migration alongside `outbox`). Messages without an `Event.ID` (legacy publish or non-outbox dispatcher) return `consumers.ErrNoEventID` → DLQ, rather than risk double processing silently.

RabbitMQ topology (declared at Connect / Subscribe):

```
exchange:        events       (topic, durable)
DLX:             events.dlx   (topic, durable)
main queue:      <consumer>.<topic>      (durable, x-dead-letter-exchange=events.dlx)
main binding:    events  ──topic──>  main queue
dlq queue:       <consumer>.<topic>.dlq  (durable)
dlq binding:     events.dlx  ──topic──>  dlq queue
```

`rabbitmq` must be running for `cmd/api` to start (Connect fast-fails). Once the app is up, broker drops are recoverable: `rabbitmq.Client.Channel()` lazy re-dials on the next call after `IsClosed`, and the consumer loop (`consumers.Subscriber.consumeOnce` + outer reconnect with 1s→30s backoff + ±25% jitter) re-declares queues and resumes. `/readyz` returns 503 during the gap, gating traffic until the connection is back. Publishers (used by the outbox relay) propagate the error up; the relay's per-row retry handles delivery.

### Migrations

- Files live at `migrations/<module>/YYYYMMDDHHMMSS_*.sql` with `-- +goose Up` / `-- +goose Down` sections.
- `migrations/base/` is the platform layer (schemas + shared outbox), applied first; tracked in `public.goose_db_version`.
- Each business module gets its own `<module>.goose_db_version` table inside its schema, so modules evolve independently.
- A module that consumes events also gets a `<module>.processed_events` table (see `consumers.Dedup`); produce a migration like `migrations/<module>/YYYYMMDDHHMMSS_processed_events.sql`.
- `migrations/migrations.go` embeds the SQL files. When adding a module, add a `//go:embed all:<name>` line.
- `cmd/migrate` (binary) applies them; in k8s this is a one-shot Job per release. No `auto_migrate` in config.

### Logging

`zerolog`; component is set via `.With().Str("component", "...").Logger()`. Located at `internal/platform/observability/logger`. New platform packages should set their own component label.

### Ops endpoints (mounted on the unversioned root)

- `GET /healthz` — liveness probe, always 200. Process-alive only.
- `GET /readyz` — readiness probe. Runs DB ping + `rabbitmq.Client.HealthCheck` with a 3s shared timeout. 503 on any failure.
- `GET /metrics` — Prometheus scrape endpoint (`promhttp.Handler` wrapped via fiber adaptor).

In production this should sit behind cluster-only access (separate port or auth) — currently exposed on the main HTTP port for simplicity.

### Idempotency

`platform/idempotency` is the HTTP middleware that gives clients safe retries on mutating endpoints via the `Idempotency-Key` request header. Backed by `platform/redis` (`Storage` interface; `RedisStorage` is the only impl today).

Flow per request:

1. GET / HEAD pass through. POST/PUT/PATCH/DELETE without an `Idempotency-Key` also pass through (sending one is opt-in for the client).
2. With a key present: `storage.Claim` does `SETNX <cache-key> "__in_flight__" EX 60`. Three outcomes:
   - **Claim succeeded** → process the request; after the handler, 2xx-4xx is `Store`d for 24h (overwriting the sentinel), 5xx → `Release` deletes the key so the client can retry immediately.
   - **Cached response** → middleware returns it (status + Content-Type + body) without invoking the handler.
   - **In flight** (sentinel still present) → 409 Conflict.
3. Any Storage error → fail open (log + passthrough). Redis being down must not block business traffic.

Cache key: `idempotency:<method>:<route>:<header>` (route is the Fiber pattern, not the literal path). No user-scoping until auth lands.

Limitations: only Status / Content-Type / Body are preserved across replay — other response headers set by the handler (Location, custom) are dropped. TTL constants (60s in-flight, 24h cached) live in the package.

Redis is optional. When `cfg.Redis.DSN` is empty, the composition root skips Redis connect and passes `nil` storage to `registerRoutes` — the middleware is simply not mounted.

### Cron jobs

`platform/crons` runs recurring jobs and serializes execution across pods via Postgres advisory locks. No third infrastructure needed — Postgres is already there, locks auto-release on connection close (no stale-lock cleanup).

Registration:
```go
scheduler.Register("payment-cleanup", "0 3 * * *", func(ctx context.Context) error {
    return paymentSvc.CleanupOldFailed(ctx)
})
```

Standard 5-field cron. Job key is FNV-64a of the name → int64 for `pg_try_advisory_lock`. Every fire:
1. Take a dedicated conn from the pool.
2. `SELECT pg_try_advisory_lock($key)`. If `false` — another pod has it, skip this tick.
3. Run job (Job uses the main pool or anything via closure — the locked conn is *only* the lease).
4. `SELECT pg_advisory_unlock($key)` + return conn.
5. On pod crash mid-run: conn closes, lock auto-releases, next tick takes over.

Per-job constants (in package): `jobTimeout = 5min`, `shutdownTimeout = 30s`. `SkipIfStillRunning` is wired so overlapping ticks of the same job drop rather than queue. `Recover` wraps each job so a panic doesn't kill the cron goroutine.

Long-running jobs hold one Postgres connection for the entire duration — keep jobs bounded (or split work).

The scheduler is instantiated in `cmd/api/main.go` and started via `go scheduler.Run(ctx)`. No concrete jobs registered yet — modules will Register from their `New(...)` when they grow recurring work.

### Feature flags

`platform/featureflags` defines `Provider` — the abstraction for runtime toggles. Today only `InMemoryProvider` is implemented (snapshot from `cfg.FeatureFlags` at startup, immutable except via test-only `Set`). A future remote-config Provider (LaunchDarkly, Unleash, …) plugs into the same interface.

Usage: a module accepts `featureflags.Provider` in its `New()` and reads flags by name with a typed default:
```go
if m.flags.Bool(ctx, "enable_new_payment_flow", false) { ... }
```
`ctx` is passed so future implementations can resolve per-user/tenant from claims. The in-memory provider ignores it.

Provider is instantiated in `cmd/api/main.go` from `cfg.FeatureFlags` (a `map[string]any` in YAML); no module consumes it yet. Add the parameter to a module's `New()` when it actually needs a flag.

### Secrets

`platform/security` defines `SecretsProvider` — the abstraction for fetching secrets. Today only `EnvSecretsProvider` (reads `os.Getenv`) is implemented; a future `VaultSecretsProvider` will plug into the same interface without callers changing.

Pattern: any config string starting with `secret:NAME` is resolved at startup by `security.ResolveSecrets(ctx, cfg, provider)` (called from both `cmd/api/main.go` and `cmd/migrate/main.go` right after `config.Load`). Currently `db.dsn`, `rabbitmq.dsn`, `redis.dsn` are eligible; add new secret-bearing fields to the explicit list in `platform/security/resolve.go`.

Example:
```yaml
db:
  dsn: "secret:DATABASE_URL"   # → os.Getenv("DATABASE_URL")
```

This is opt-in per field — literal values still work for local dev. Viper's existing `APP_DB_DSN` ENV override remains; `secret:` is for explicit references with arbitrary names and a future Vault path.

Long-term goal (per UNRELISED) is "no secrets in env" — that's when `VaultSecretsProvider` replaces the env provider at the composition root. The interface stays.

### Timeouts

Two layers, intentionally redundant:

- `http.request_timeout` (default 10s) — `httpserver.timeoutMiddleware` wraps `c.UserContext()` with `context.WithTimeout`. Handlers / services / repos that respect ctx unwind when the deadline hits. Mounted after tracing, before requestLogger.
- `db.statement_timeout` (default 5s) — `postgres.Connect` appends `statement_timeout=<ms>` to the DSN unless the DSN already specifies it. Postgres applies it per-session, killing any query that runs longer. Even ctx-ignoring code paths eventually unblock.
- `http.shutdown_timeout` (default 15s, > request_timeout) — `server.Shutdown` blocks this long for in-flight requests to finish. Set larger than request_timeout so the shutdown phase always has slack to let the timeout middleware fire naturally.

`cmd/migrate` uses `cfg.DB.DSN` raw — no statement_timeout — so long migrations are not killed mid-run.

### Tracing

`internal/platform/observability/tracing` wires the OpenTelemetry SDK at `tracing.Init()`: TracerProvider without an exporter (spans get sampled and dropped), W3C TraceContext + Baggage as the global propagator. To emit spans somewhere, add `sdktrace.WithBatcher(otlpExporter)` to `Init` — the rest of the propagation is already wired.

Propagation chain:
- HTTP in: `tracing.Middleware` extracts `traceparent` from incoming headers, starts a `METHOD route` span, calls `c.SetUserContext`. Subsequent middleware/handlers see the trace via `c.UserContext()`.
- `requestLogger` reads `trace.SpanContextFromContext(ctx)` and writes `trace_id` into the structured log line — one tail filter shows every log for a given request.
- Outbox: `tracing.MarshalContext(ctx)` serializes the propagator carrier into the `trace_context jsonb` column on Publish. The relay calls `tracing.UnmarshalContext(ctx, row.TraceContext)` and passes the restored ctx to the Dispatcher.
- AMQP: `rabbitmq.Publisher.Dispatch` injects the carrier into `amqp.Publishing.Headers`. `consumers.Subscriber.handle` extracts the carrier from `delivery.Headers` and passes the restored ctx to the handler.

Result: a single trace can span `HTTP request → service tx → outbox row → relay dispatch → AMQP → consumer handler → service tx` without the modules knowing anything about trace plumbing.

### Metrics

Defined in `internal/platform/observability/metrics` and registered against the default Prometheus registry via promauto. Adding a metric: declare a package-level var with `promauto.New*`, reference it from the update site. Keep label cardinality bounded.

Current metrics:
- `http_request_duration_seconds{method,route,status}` — histogram, recorded by `metrics.Middleware` in `httpserver.New`. `route` is Fiber's pattern, not the literal path.
- `http_requests_total{method,route,status}` — counter, same source.
- `outbox_backlog{schema}` — gauge, refreshed every 30s by `outbox.Relay.refreshBacklog`. Alert on sustained growth.
- `outbox_dead_total{schema,topic}` — counter, incremented when relay moves an event to `<schema>.outbox_dead`.
- `outbox_dispatch_duration_seconds{schema,topic}` — histogram per relay dispatch.
- `consumer_queue_depth{consumer,topic,kind}` — gauge, refreshed by `consumers.Subscriber.pollQueueDepths` via passive `QueueDeclare`. `kind` is `main`|`dlq` — alert on `dlq>0` and on sustained `main` growth.
- `consumer_messages_total{consumer,topic,status}` — counter, status is `ack`|`nack`|`panic`. Recorded per delivery.
- `consumer_handle_duration_seconds{consumer,topic}` — histogram, per-handler latency (excludes time spent waiting for a bulkhead slot).
- `panics_total` — counter (placeholder for the recover middleware to fill in later).

### Composition root

`cmd/api/main.go`'s `run(...)` wires every dependency by hand — no `Module` interface, no module registry. Each module's `New(...)` returns its concrete `*Module`; the composition root calls `module.API()` to get its `<name>api.Service`, then passes the services to `registerRoutes(...)` and (where needed) to other modules' `New(...)`. When adding a module, add construction + a call in `registerRoutes` there.

## Architecture Decision Records

`docs/adr/` documents the load-bearing decisions and their alternatives.
Before changing anything listed there, read the relevant ADR — if the
reasoning is stale, add a new ADR that supersedes it.

| # | Decision |
|---|---|
| [0001](docs/adr/0001-modular-monolith.md) | Modular monolith with vertical slices; Go `internal/` for enforcement |
| [0002](docs/adr/0002-provider-side-service-interface.md) | `Service` interface lives provider-side |
| [0003](docs/adr/0003-per-schema-outbox.md) | Transactional outbox per-schema, not shared |
| [0004](docs/adr/0004-rabbitmq-topology.md) | RabbitMQ topology: topic exchange + DLX + queue per (consumer, topic) |
| [0005](docs/adr/0005-consumer-side-idempotency.md) | Consumer-side dedup via `<schema>.processed_events` |
| [0006](docs/adr/0006-explicit-composition-root.md) | Explicit wiring in `cmd/api/main.go`; no `Module` interface, no DI framework |

## Operational TODOs

Deliberately left out of the template — wire when real load appears:

- GC for `<schema>.processed_events` (cron `DELETE WHERE processed_at < now() - interval '30 days'`).
- `VaultSecretsProvider` implementation (interface in place).
- Remote feature flags provider (interface in place).
- OTLP exporter wired into `tracing.Init` (currently no-op; propagation works).
- Outbox partitioning if QPS rises enough to strain autovacuum on the DELETE-heavy table.
