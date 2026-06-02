Модули
9 bounded contexts + platform packages. Все модули — в internal/modules/<name>/. Каждый экспортирует пакет api/ с публичным интерфейсом и DTO; реализация — в internal/.

Platform packages¶

Не модули, а инфраструктура — могут использоваться всеми модулями. Не имеют domain-логики. Здесь же живут background pipelines, которые в первой итерации были отдельным cmd/worker бинарём:
азовые правила¶

#	ПРИНЦИП	РЕАЛИЗАЦИЯ
1	Один процесс, много модулей	Один бинарь. HTTP сервер + background pipelines (outbox publisher, postcheck, RabbitMQ consumers, crons) в одном процессе
2	Schema-per-module в PostgreSQL	Каждый модуль владеет своей схемой; cross-schema FK/JOIN запрещены
3	Контракты на уровне Go interfaces	Публичный API модуля = пакет api/; имплементация в internal/
4	События через transactional outbox	Запись в outbox в той же tx, что и бизнес-операция; outbox publisher (goroutine) шлёт в RabbitMQ
5	Idempotency на API Gateway уровне	Middleware читает Idempotency-Key, кеширует ответы в Redis
6	Внешние вызовы — только через provider/	Каждый внешний интегратор — отдельный пакет с circuit breaker, backoff, contract test
7	Все секреты — в Vault	Никаких секретов в env vars или конфигах
8	Observability с дня 1	Structured JSON logs, Prometheus-совместимые метрики, OpenTelemetry traces
Три слоя модульного монолита¶

Слой 1 — Bounded contexts как модули¶

Каждый bounded context — отдельный модуль с публичной поверхностью (Go interface) и приватной реализацией. Другие модули могут вызывать только публичный API. Прямой доступ к внутренним структурам, репозиториям или таблицам другого модуля запрещён компилятором (Go internal/ mechanism).

Слой 2 — Изолированное владение данными¶

Каждый модуль владеет своими таблицами. В PostgreSQL это реализуется через отдельную схему на модуль: user.*, wallet.*, ledger.*, cards.*, movement.*, и т. д. Запреты:

Никаких foreign keys между схемами
Никаких JOIN-запросов между схемами в продакшн-коде
Доступ к чужой таблице — только через публичный API модуля
Read-only views для отчётов допустимы, но должны быть в <module>.public_view_* и поддерживаться как контракт
Слой 3 — Явные контракты между модулями¶

Sync — Go interfaces в internal/modules/<name>/api. Имплементация в internal/modules/<name>/internal. Go internal/ package mechanism автоматически блокирует кросс-модульный импорт реализации.

Async — внутренняя шина событий с transactional outbox. Запись в <module>.outbox происходит в той же транзакции, что и бизнес-операция. Outbox publisher (background goroutine) читает outbox и публикует в RabbitMQ. At-least-once без распределённых транзакций.
Структура репозитория сервиса¶

Это структура production-репозитория проекта. Здесь зафиксирован контракт.


zinda-wallet/
├── cmd/
│   ├── api/                      # ЕДИНСТВЕННЫЙ runtime бинарь
│   │   ├── main.go               # композиция всех модулей + старт background pipelines
│   │   ├── handlers/             # тонкие обёртки над module APIs
│   │   │   ├── user_handler.go
│   │   │   ├── movement_handler.go
│   │   │   ├── cards_handler.go
│   │   │   ├── webhooks_handler.go
│   │   │   └── admin/            # бэк-офис endpoints (mTLS + VPN)
│   │   ├── router.go
│   │   └── middleware.go
│   └── migrate/                  # one-shot DB migration tool (k8s Job per release)
│
├── internal/
│   ├── modules/
│   │   ├── user/
│   │   │   ├── api/              # PUBLIC — interfaces + DTOs
│   │   │   └── internal/         # PRIVATE — implementation
│   │   ├── wallet/
│   │   ├── ledger/
│   │   ├── cards/
│   │   ├── money-movement/
│   │   ├── provider/             # все внешние адаптеры
│   │   │   └── internal/{visa,abs,ocrlive,sms,hsm,utility,mobile,push}
│   │   ├── notification/
│   │   ├── reconciliation/
│   │   └── admin/
│   └── platform/                 # cross-module инфраструктура
│       ├── eventbus/             # in-process pub/sub
│       ├── outbox/               # transactional outbox + publisher goroutine
│       ├── postcheck/            # background worker для movement.postcheck_queue
│       ├── consumers/            # RabbitMQ consumers
│       ├── crons/                # scheduled jobs (recon, cleanup, hold expiry)
│       ├── idempotency/          # HTTP middleware
│       ├── observability/        # logger, metrics, tracing
│       ├── security/             # Vault, JWT, encryption
│       ├── audit/                # writer в admin.audit_events
│       ├── postgres/             # pool, tx helpers
│       ├── redis/
│       ├── rabbitmq/
│       ├── featureflags/
│       └── httpserver/
│
├── migrations/                   # версионированные SQL миграции
│   ├── user/
│   │   └── 20260101120000_init.sql   # single-file up/down
│   ├── wallet/
│   ├── ledger/
│   └── ...
│
├── config/
│   ├── config.local.yaml
│   ├── config.staging.yaml
│   └── config.production.yaml
│
├── deployments/
│   ├── docker/
│   │   ├── Dockerfile
│   │   └── docker-compose.local.yaml
│   └── kubernetes/
│       ├── api-deployment.yaml   # ОДИН Deployment
│       ├── migrate-job.yaml
│       ├── configmap.yaml
│       └── ingress.yaml
│
├── docs/
│   ├── adr/                      # Architecture Decision Records
│   ├── runbooks/
│   └── api/openapi.yaml
│
├── scripts/
├── .github/workflows/
├── go.mod
└── README.md
Что критично¶

Один бинарь = один deploy unit¶

cmd/api/main.go стартует:

HTTP/gRPC сервер
Outbox publisher goroutine
Postcheck worker goroutine
RabbitMQ consumers (notification dispatch, recon trigger, webhook async)
Cron jobs (daily recon, cleanup) под distributed lock
Все в одном процессе. Когда конкретный pipeline начнёт мешать API SLO (метрики покажут) — извлечь в cmd/worker или ввести --mode флаг. До этого момента: проще, дешевле, быстрее в деплое.

Enforced правила (через CI lint)¶

Импорт internal/modules/X/internal/ из любого пакета вне internal/modules/X/ → fail (Go-уровень, проверяется компилятором).
SQL files в migrations/<module>/ в формате YYYYMMDDHHMMSS_name.sql с up/down секциями.
Нет import циклов (Go-уровень + custom check на cross-module dependencies).
Никаких упоминаний имён конкретных провайдеров (visa, kortimilli) за пределами internal/modules/provider/internal/.
Контракт модуля¶

Каждый модуль экспортирует один пакет api/ с интерфейсом и DTO. Пример для user:


// internal/modules/user/api/api.go
package api

import "context"

type User struct { /* ... */ }

type Service interface {
Register(ctx context.Context, phone string) (string, error)
GetByID(ctx context.Context, id string) (*User, error)
// ...
}
Имплементация в internal/modules/user/internal/service.go — не экспортируется. Композиция в cmd/api/main.go:


userSvc     := userinternal.NewService(db, providerSvc, eventBus)
walletSvc   := walletinternal.NewService(db)
ledgerSvc   := ledgerinternal.NewService(db)
movementSvc := movementinternal.NewService(db, walletSvc, ledgerSvc, providerSvc, eventBus)
Все зависимости передаются явно. Граф зависимостей виден в одном файле. Подмена на mock в тестах — тривиальная.
---

# Оценка готовности шаблона (2026-06-02)

После выполнения всех 4 фаз (структурный рефакторинг → HTTP-централизация → outbox + RabbitMQ → платформенное обвязывание + ADR/README) — текущая оценка.

## Итоговая оценка: 7/10 как стартер для нового проекта, 4-5/10 как production-ready

**Зачем эта оценка вообще:** зафиксировать честный baseline. Шаблон — не «hello world», но и не готовый к публичному SaaS продукт. Понимание границ позволяет приоритизировать дальнейшие работы.

## Что реализовано и работает (сильные стороны)

| Слой | Оценка | Состояние |
|---|---|---|
| Архитектура модулей | 9/10 | Compiler-enforced boundaries (Go `internal/`), вертикальные срезы, per-schema isolation, ADR-0001 |
| Transactional outbox | 9/10 | Per-schema, NOTIFY + safety-poll, `FOR UPDATE SKIP LOCKED` scale-out safe, dead-letter, trace_context jsonb. ADR-0003 |
| Consumer-side idempotency | 9/10 | `processed_events` + tx-bound `consumers.Dedup`, end-to-end event_id через `MessageId`. ADR-0005 |
| RabbitMQ topology | 8/10 | Topic exchange + DLX, queue-per-(consumer,topic), bulkhead с semaphore + prefetch, lazy reconnect, queue depth poller. ADR-0004 |
| Observability foundation | 7/10 | Prometheus с разумным набором метрик, OTel propagation (W3C через async), health probes, structured logging с `trace_id` |
| HTTP middleware stack | 8/10 | Recover, tracing, timeout, logger, metrics, idempotency в правильном порядке; Fiber zero-copy gotcha исправлен (см. CLAUDE.md) |
| Composition root | 8/10 | Явный wiring без DI-фреймворка. ADR-0006 |
| Документация | 9/10 | README ~1800 строк с end-to-end flow, 6 ADR в `docs/adr/`, CLAUDE.md с конвенциями + gotchas |
| End-to-end verification | 8/10 | Прогнан live: POST → outbox → AMQP → consumer.Dedup → balance/DLQ; processed_events работает |

## Критические пробелы (блокируют production)

| Слой | Оценка | Что отсутствует |
|---|---|---|
| **Auth / authz** | **0/10** | Ни JWT, ни session, ни RBAC, ни middleware извлечения user_id. `Idempotency-Key` даже не user-scoped |
| **Тесты** | **2/10** | Только `postgres.UoW` unit + один domain тест в account. Нет: integration на testcontainers, handler-тестов через `app.Test()`, e2e на outbox→AMQP flow. Шаблон не показывает «как у нас тестируется» |
| **CI/CD** | **0/10** | Нет `.github/workflows`, нет Dockerfile для prod-сборки, нет k8s манифестов, нет Helm chart. Только Makefile для локалки |

## Важные пробелы (production-grade, но не блокеры)

| Слой | Оценка | Состояние |
|---|---|---|
| Vault | 2/10 | Интерфейс есть, реализация только `EnvSecretsProvider`. Цель «no secrets in env» зафиксирована, не реализована |
| OTel exporter | 3/10 | Wired propagation, но spans дропаются (`AlwaysSample` без батчера). Tempo уже работает у разработчика на 4317 — две строки кода до полной трассировки |
| GC `processed_events` | 0/10 | Растёт монотонно. Cron-job в Operational TODOs, не написан |
| Реалистичный бизнес-flow | 5/10 | Payment остаётся `pending` навсегда после settle — нет обратного события `account.transferred → payment.completed`. Для демо OK, для шаблона — плохой пример |
| Admin endpoints для ops | 0/10 | Drain DLQ, replay из `outbox_dead`, list stuck payments — всё руками через psql/rabbitmqctl |
| Rate limiting | 0/10 | Нет middleware. Брутфорсу не противопоставлено ничего |
| CORS | 0/10 | Не настроен. Browser-фронтенду на другом домене не работать |
| OpenAPI / Swagger | 0/10 | Нет. Контракт API только в коде |
| Outbound HTTP client | 0/10 | Нет обёртки с timeouts/retries/circuit-breaker для исходящих вызовов |
| API versioning стратегия | 3/10 | Только `/api/v1` group, без плана «как переезжаем на v2» |
| Multi-tenancy | 0/10 | Не предусмотрено |
| Feature flags remote | 2/10 | Только `InMemoryProvider` |
| Apperror vocabulary | 5/10 | Generic `Kind` enum (`Invalid`, `NotFound`, `Conflict`, `Internal`). Production обычно хочет error codes + локализацию |
| Request body size limit | — | Fiber default 4MB, не настроен явно |

## Оценка по сценариям использования

| Сценарий | Оценка | Комментарий |
|---|---|---|
| Стартую новый проект на этом шаблоне | **7/10** | Очень крепкая основа. Архитектура продумана глубже большинства open-source стартеров. Outbox + dedup + bulkhead — то, на чём команды обычно ломаются на 6-м месяце. Минус 3 балла: нет auth, нет test-инфры (образца), нет CI/CD |
| Деплой в прод как MVP внутреннего инструмента | **5/10** | Сработает для маленького внутреннего сервиса без внешних пользователей. Минус 5: нет auth, rate limiting, admin endpoints, prod-grade exporter'ов, CI/CD |
| Деплой в прод для публичного SaaS | **3/10** | Нет. Не для production-grade. Требуется минимум: auth, integration tests, CI/CD, OTel exporter, rate limiting, OpenAPI, GC, admin endpoints |

## План действий чтобы поднять стартер-оценку до 8/10

Порядок — убывание пользы за час работы:

1. **OTel OTLP exporter** (~30 мин). Tempo уже работает у разработчика. Две строки в `tracing.Init` — и весь distributed trace виден в одном клике
2. **GitHub Actions с `go test ./... && go vet ./... && golangci-lint`** (~45 мин). Без CI шаблон деградирует с каждым PR'ом
3. **Dockerfile multi-stage для prod-сборки** (~30 мин). Сейчас даже непонятно как развернуть
4. **Один полноценный integration test** на testcontainers (Postgres + RabbitMQ) для e2e flow `POST /payments → settle` (~2 часа). Не для покрытия — для **образца**: «вот как у нас принято тестировать»
5. **Auth-skeleton**: JWT middleware + извлечение user_id в ctx + user-scoping для `Idempotency-Key` (~3 часа). Даже простой HMAC-JWT снимает «нет auth»
6. **Доделать payment lifecycle**: ввести `account.transferred` событие + consumer в payment, который двигает status на `completed`/`failed` (~2 часа). Закроет главное смысловое замечание «pending навсегда»
7. **Один admin endpoint** `POST /admin/outbox/replay` (~1 час). Хотя бы для демонстрации паттерна

После этих 7 пунктов (~10 часов работы) шаблон будет **8/10 для нового проекта** и **6-7/10 для прода**. Дальше — auth RBAC, multi-tenancy, OpenAPI — уже под конкретный продукт.

## Резюме

То, что обычно занимает у команды месяцы дискуссий («как делаем outbox? а DLQ? а idempotency? а distributed tracing?»), здесь уже зафиксировано в коде + ADR. Это **редкость** для стартер-шаблона.

Но **готовности к production-as-is нет**, и это нормально — стартер не должен быть готов к проду, он должен быть **готов к началу проекта без боли**. С этим он справляется на 7/10. Чтобы стать «8», нужны три вещи: тесты-образцы, CI/CD, и auth-скелет. Всё остальное — продуктовая специфика.
