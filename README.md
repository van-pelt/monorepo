# monorepo

Шаблон **модульного монолита** на Go, спроектированный под последующий
распил на микросервисы без переписывания бизнес-кода. Два примера-модуля
(`account`, `payment`), полный платформенный слой (HTTP, outbox, RabbitMQ,
observability, idempotency, crons, feature flags, secrets).

> Зачем читать эту README: чтобы понять **как этот шаблон собран** и
> **почему именно так**. Решения зафиксированы в [`docs/adr/`](docs/adr/) —
> читайте перед тем, как менять архитектуру.

## Стек

| Слой | Выбор |
|---|---|
| HTTP | Fiber v2 |
| БД | PostgreSQL + sqlx + pgx/v5 |
| LISTEN/NOTIFY | нативный pgx/v5 (выделенное соединение) |
| Брокер | RabbitMQ (amqp091-go) |
| Миграции | goose, embed.FS, отдельная команда `cmd/migrate` |
| Конфиг | viper (yaml + `APP_*` env override) |
| Логи | zerolog |
| Метрики | Prometheus (client_golang + promauto) |
| Трейсы | OpenTelemetry SDK (W3C propagator, no-op exporter по умолчанию) |
| Redis | go-redis/v9 (опционально, для idempotency middleware) |
| Cron | robfig/cron/v3 + Postgres advisory lock для leader election |
| Валидация | go-playground/validator |

Go 1.25.

## Структура

```
cmd/
  api/                       composition root + HTTP server
    main.go                  всё wiring руками: модули, relay, consumers, scheduler
    router.go                регистрация handlers на /api/v1
    handlers/                HTTP handlers + DTOs, по одному файлу на сущность
  migrate/                   одноразовый бинарь миграций (k8s Job per release)

internal/
  modules/                   бизнес-модули, вертикальные слайсы
    account/
      api/                   ПУБЛИЧНЫЙ контракт (Service, DTOs, events, errors)
      internal/              недоступно извне модуля (Go internal/-механизм)
        domain/              сущности + Repository интерфейс
        repository/          sqlx реализация Repository
        service/             use cases (Service интерфейс + unexported impl)
      module.go              wiring модуля + api-адаптер
    payment/                 та же структура

  platform/                  cross-module инфраструктура
    apperror/                типизированные ошибки → HTTP status
    config/                  viper-конфиг (yaml + APP_*-env)
    consumers/               RabbitMQ Subscriber: queue per (consumer,topic),
                             DLQ, bulkhead semaphore, queue-depth poller,
                             Dedup helper
    crons/                   Scheduler с PG advisory lock + robfig/cron
    eventbus/                in-process pub/sub (не используется в проде,
                             доступен для тестов / простых сценариев)
    featureflags/            Provider interface + InMemoryProvider
    httpserver/              Fiber server, middleware, error mapping
    idempotency/             HTTP middleware на Idempotency-Key + Redis storage
    messaging/               контракт (Event, Handler, Subscriber)
    observability/
      health/                /healthz + /readyz handlers + Probe тип
      logger/                zerolog setup
      metrics/               Prometheus метрики + Fiber middleware + /metrics
      tracing/               OTel SDK + W3C propagator + middleware
    outbox/                  Publisher (per-schema) + Relay (LISTEN/NOTIFY)
    postgres/                pool, Querier alias, UnitOfWork, InTx[T]
    rabbitmq/                AMQP client + Publisher (outbox.Dispatcher)
    redis/                   go-redis обёртка
    security/                SecretsProvider + EnvSecretsProvider + ResolveSecrets

migrations/                  embed.FS, прокинутый в cmd/migrate
  base/                      платформенный слой: схемы + goose_db_version
  account/                   миграции account.* (включая outbox + processed_events)
  payment/                   миграции payment.*
  migrations.go              //go:embed all:<module>

docs/
  adr/                       Architecture Decision Records (читать перед
                             изменением архитектуры)

deploy/
  docker-compose.yaml        Postgres + RabbitMQ + Redis для локалки

config/
  config.example.yaml        шаблон; config.yaml gitignored
```

## Границы модулей

**Изоляция через Go-компилятор, а не линтер.** Всё под
`internal/modules/<name>/internal/` физически недоступно вне модуля —
импорт отвергается `go build`. См. [ADR-0001](docs/adr/0001-modular-monolith.md).

Контракты:

- Чужой модуль и `cmd/api/handlers/` импортируют **только** `<name>/api`.
- `<name>/api` содержит: `Service`-интерфейс, DTO (`AccountInfo`),
  event-topics + payloads (`TopicPaymentCreated`, `PaymentCreated`),
  sentinel errors (`ErrAccountNotFound`).
- У каждого модуля своя **схема в БД** (`account.*`, `payment.*`). Между
  схемами **нет FK** — связи по ID, валидация через `api`. Готов к распилу
  в отдельную БД. См. [ADR-0003](docs/adr/0003-per-schema-outbox.md).

## Архитектура модуля

```
internal/modules/<name>/
  api/             публичный контракт — единственное окно наружу
  internal/
    domain/        сущности + Repository интерфейс; не знает про БД/HTTP
    repository/    sqlx реализация Repository; принимает postgres.Querier
                   (= sqlx.ExtContext) — работает и на *sqlx.DB, и на *sqlx.Tx
    service/       use cases. Service-интерфейс + unexported impl;
                   New(...) возвращает интерфейс
  module.go        wiring + api-адаптер
```

Service-интерфейс **provider-side** (объявлен рядом с реализацией) — это
сознательный отход от Go-идиомы. Причины и альтернативы — в
[ADR-0002](docs/adr/0002-provider-side-service-interface.md). Не предлагать
«перенести интерфейс к потребителю для testability».

HTTP-handlers живут **вне модулей**, в `cmd/api/handlers/`. Зависят только
от `<name>api.Service`. При выносе модуля в gRPC handler не двигается —
меняется только реализация `api.Service` (in-process adapter → gRPC client).

## Межмодульное взаимодействие

Две оси, осознанный выбор по сценарию:

| Сценарий | Механизм | Когда |
|---|---|---|
| Нужен немедленный ответ для бизнес-решения | синхронный вызов через `<name>/api` | валидация, чтение справочника |
| Side-effect, можно eventual consistency | outbox → broker → consumer | перевод средств, отправка уведомления |

Не использовать порт ради побочного эффекта (теряете гарантии при сбое). Не
использовать outbox когда нужен возврат значения.

## Outbox + broker + consumer

End-to-end async path:

```
business write
  ↓ INSERT INTO <schema>.outbox + NOTIFY outbox   (одна транзакция)
relay (платформенная goroutine)
  ↓ LISTEN + drain → Dispatcher.Dispatch
RabbitMQ
  ↓ topic exchange "events"   (routing key = topic)
consumer queue <consumer>.<topic>
  ↓ Subscriber.handle: bulkhead semaphore → Dedup → user handler
ack | nack→DLQ
```

Два независимых **at-least-once**-участка: relay→broker и broker→handler.
Handler **обязан** быть идемпотентным.

### Producer

`outbox.Publisher` scoped к схеме, пишет в той же tx что и бизнес-write:

```go
s.uow.Do(ctx, func(ctx context.Context, q postgres.Querier) error {
    if err := s.repo.Create(ctx, q, payment); err != nil {
        return err
    }
    return s.publisher.Publish(ctx, q, paymentapi.TopicPaymentCreated, event)
})
```

`Publish` делает в `q`:
1. `INSERT INTO <schema>.outbox (id, topic, payload, trace_context, ...)`.
2. `NOTIFY outbox` — будит relay.

`NOTIFY` в Postgres транзакционен — relay никогда не увидит событие, чья
бизнес-транзакция не закоммитилась.

### Relay

Запускается из `cmd/api/main.go` как goroutine:

1. Открывает **выделенное** pgx-соединение (`LISTEN` не работает на пуле sqlx).
2. `LISTEN outbox` + начальный `drainAll` (вычерпывает backlog).
3. Цикл `WaitForNotification(ctx, cfg.Interval)`:
   - пришло уведомление → `drainAll`;
   - истёк `Interval` → safety-net `drainAll` (на случай отложенных retry);
   - ошибка соединения → reconnect с backoff (1s→30s) + ±25% jitter.
4. `drainAll` цикл по схемам: `dispatchBatch(schema)` — `FOR UPDATE SKIP
   LOCKED`, для каждой строки `Dispatcher.Dispatch`, ack / retry / dead в
   той же транзакции.

`SKIP LOCKED` делает relay безопасным для scale-out (несколько подов забирают
непересекающиеся батчи). Транзакция держится открытой на время дистпатча —
для монолита нормально; для multi-relay scale-out с большим throughput'ом
стоит переключиться на lease-based pattern.

### Брокер

[ADR-0004](docs/adr/0004-rabbitmq-topology.md). Один topic exchange
`events`, DLX `events.dlx`. Очередь `<consumer>.<topic>` для каждой
подписки + одноимённая `.dlq`. На handler-error → `nack(requeue=false)` →
DLX → `.dlq`. Никогда `requeue=true` (poison message в hot loop).

### Consumer

`consumers.Subscriber` — одна на consumer-имя (= имя модуля). Подписка через
`messaging.Subscriber`-контракт; concurrency, timeout, queue-depth poll
interval приходят из `cfg.Consumers`.

**Bulkhead** per подписку: sized semaphore + AMQP prefetch на ту же величину
(дефолт 4). Один медленный handler топика не съест все слоты у соседних
топиков. Per-topic override:

```go
consumers.Config{
    DefaultConcurrency: 4,
    TopicConcurrency: map[string]int{"payment.created": 16},
}
```

**Идемпотентность через `consumers.Dedup`** (ADR-0005):

```go
sub.Subscribe(paymentapi.TopicPaymentCreated, consumers.Dedup(uow, "account",
    func(ctx context.Context, q postgres.Querier, e messaging.Event) error {
        return svc.ApplyTransferTx(ctx, q, e.Payload)
    }))
```

Helper в одной транзакции: `INSERT INTO account.processed_events ... ON
CONFLICT DO NOTHING` → если 0 rows → ack без вызова handler'а, иначе handler
с тем же `q`. Бизнес-write и dedup-mark коммитятся атомарно.

Каждый consumer-модуль обязан создать `<schema>.processed_events` миграцией
(см. `migrations/account/20260301000000_processed_events.sql`).

### Retry и backoff

- Outbox-retry: экспоненциальный backoff `BaseBackoff * 2^(attempts-1)` с
  cap `MaxBackoff`, ±25% jitter. После `MaxAttempts` — `INSERT INTO
  <schema>.outbox_dead + DELETE FROM <schema>.outbox`.
- Consumer reconnect: 1s base → 30s cap, ±25% jitter.
- Relay reconnect: те же 1s→30s + jitter.
- Jitter везде из одних соображений: N подов не должны синхронно
  ломиться в только-что-поднявшийся брокер.

### Distributed tracing

Trace продолжается через async-границу без участия модулей:
- `outbox.Publish` сериализует W3C TraceContext в столбец `trace_context jsonb`.
- Relay восстанавливает context перед вызовом `Dispatcher`.
- `rabbitmq.Publisher` инжектит carrier в AMQP headers.
- `consumers.Subscriber.handle` извлекает carrier из headers, восстанавливает
  context для handler'а.

См. CLAUDE.md «Tracing» для деталей.

## Платформенные таблицы

Три таблицы платформы, на которых стоит весь async-flow. Producer и consumer
— **разные модули** (и потенциально разные БД после распила); каждый владеет
своими.

### Краткая карта

| Таблица | Сторона | Когда заполняется | Когда чистится | Зачем |
|---|---|---|---|---|
| `<schema>.outbox` | producer | бизнес-write в той же tx | relay удаляет на успешный dispatch | гарантия доставки события наружу |
| `<schema>.outbox_dead` | producer | retry исчерпан | **никогда** автоматически | архив для разбора оператором |
| `<schema>.processed_events` | consumer | handler успешно обработал | руками / cron (GC по age) | exactly-once-effect под at-least-once delivery |

### `<schema>.outbox` — transactional outbox (producer-side)

```sql
CREATE TABLE payment.outbox (
    id            uuid PRIMARY KEY,
    topic         text NOT NULL,
    payload       jsonb NOT NULL,
    trace_context jsonb,
    created_at    timestamptz NOT NULL DEFAULT now(),
    attempts      integer NOT NULL DEFAULT 0,
    next_retry_at timestamptz NOT NULL DEFAULT now(),
    last_error    text
);

CREATE INDEX idx_payment_outbox_next_retry
    ON payment.outbox (next_retry_at, created_at);
```

**Поля:**

| Поле | Назначение |
|---|---|
| `id` | UUID, генерируется `outbox.Publisher` (`uuid.New()`). End-to-end event_id: летит в `amqp.Publishing.MessageId`, потом в `messaging.Event.ID`, потом в `processed_events.event_id`. |
| `topic` | Routing key (`payment.created`, `account.transferred`). Используется AMQP-exchange'ем для маршрутизации + consumer'ом для выбора handler'а. |
| `payload` | Сериализованный event body (`json.Marshal(eventStruct)`). `jsonb` чтобы можно было руками искать в проде (`WHERE payload->>'payment_id' = '...'`). |
| `trace_context` | W3C TraceContext propagator carrier (через `tracing.MarshalContext`). Relay восстанавливает span перед dispatch'ем → consumer видит продолжение того же trace_id. **Ключ к end-to-end tracing через async-границу.** |
| `created_at` | Время записи. Используется в `ORDER BY` для FIFO-доставки. |
| `attempts` | Счётчик неудачных dispatch'ей. `attempts >= cfg.MaxAttempts` → строка едет в `outbox_dead`. |
| `next_retry_at` | Когда строка снова видна для dispatch'а. На первой записи = `now()`. На retry → `now() + backoff`. Безусловный фильтр `WHERE next_retry_at <= now()`. |
| `last_error` | Текст последней ошибки dispatch'а. Для оператора: `SELECT last_error FROM ... ORDER BY attempts DESC`. |

**Жизненный цикл строки:**

```
Сервис в бизнес-tx:
  UPDATE payment.payments ...
  INSERT INTO payment.outbox (id, topic, payload, trace_context) VALUES (...)
  NOTIFY outbox
COMMIT;
       ↓
Relay просыпается, dispatchBatch():
  BEGIN;
  SELECT ... FROM payment.outbox WHERE next_retry_at <= now()
    ORDER BY created_at LIMIT 100 FOR UPDATE SKIP LOCKED;
  для каждой строки:
    Dispatcher.Dispatch(ctx, id, topic, payload)
      ├ success →  DELETE FROM payment.outbox WHERE id=$1                  (ack)
      ├ error, attempts+1 < MaxAttempts →
      │            UPDATE attempts++, next_retry_at = now()+backoff, ...   (retry)
      └ error, attempts+1 >= MaxAttempts →
                   INSERT INTO payment.outbox_dead ...
                   DELETE FROM payment.outbox WHERE id=$1                  (dead)
  COMMIT;
```

**Почему именно так:**

- **Атомарность с бизнес-write.** Если процесс упадёт между `INSERT INTO payments` и `INSERT INTO outbox` — оба не закоммитятся. Если упадёт после COMMIT но до публикации в брокер — событие останется в outbox и дойдёт на следующем тике. Никогда не бывает «эффект применён, событие потеряно».
- **`NOTIFY` транзакционен.** Postgres доставит NOTIFY **только** при COMMIT. Relay не увидит событие, чья бизнес-транзакция откатилась.
- **`FOR UPDATE SKIP LOCKED`.** Делает relay безопасным для scale-out: N подов делают `SELECT`, каждый видит непересекающийся набор строк. Никаких блокировок и retry на конфликте.
- **DELETE on success, не флаг `dispatched=true`.** Таблица почти всегда пустая → индекс крошечный → запросы тривиальные. Альтернатива (флаг) требует periodic cleanup + partial-индекс.
- **`next_retry_at` в той же таблице.** `WHERE next_retry_at <= now()` фильтрует и новые (DEFAULT now()) и retry — один SELECT.
- **Per-schema, не общая таблица.** См. [ADR-0003](docs/adr/0003-per-schema-outbox.md). При распиле модуля его outbox едет с ним.

**Поведение под нагрузкой:**

- В стабильном состоянии таблица почти пустая (DELETE срабатывает сразу после dispatch).
- Backlog растёт только при долгом offline'е брокера или медленном relay. Метрика `outbox_backlog{schema}` — алертить на sustained growth.
- Узкое место — autovacuum / dead tuples от DELETE. При QPS в тысячи стоит тюнить `autovacuum_vacuum_scale_factor`. При десятках тысяч — partitioning по `created_at` + `TRUNCATE` старых партиций (см. [Operational TODOs](#operational-todos)).

**Что нельзя делать:**

- **Удалять строки руками** в проде — это ровно потеря события. Если событие «застряло», правильный путь — посмотреть `last_error` и починить downstream или relay.
- **Изменять `id` или `payload` после INSERT.** event_id уже мог быть отправлен (retry); consumer'ы могут наблюдать inconsistency.
- **Использовать как очередь job'ов.** Outbox строго про события (что произошло), не про команды (что сделать). Job-queue — отдельная таблица с другим контрактом (claim/heartbeat/visibility timeout).

### `<schema>.outbox_dead` — dead-letter (producer-side)

```sql
CREATE TABLE payment.outbox_dead (
    id            uuid PRIMARY KEY,
    topic         text NOT NULL,
    payload       jsonb NOT NULL,
    trace_context jsonb,
    created_at    timestamptz NOT NULL,
    attempts      integer NOT NULL,
    last_error    text,
    failed_at     timestamptz NOT NULL DEFAULT now()
);
```

Почти зеркало `outbox`, но **без** `next_retry_at` (retry больше не будет) и
**с** `failed_at` (когда мы сдались).

**Когда заполняется.** Ровно один путь: `outbox.Relay.moveToDead` после
исчерпания `MaxAttempts`. В одной tx с `DELETE FROM outbox`:

```sql
INSERT INTO payment.outbox_dead (id, topic, payload, trace_context, created_at, attempts, last_error)
SELECT id, topic, payload, trace_context, created_at, attempts+1, $error
FROM payment.outbox WHERE id = $id;

DELETE FROM payment.outbox WHERE id = $id;
```

Одновременно инкрементится `outbox_dead_total{schema, topic}` — на это
**обязательный** алерт. Норма — 0 за день.

**Когда чистится.** Никогда автоматически. Это намеренный архив для
расследования. Чистить руками после решения проблемы либо периодической job'ой
с большим TTL (90+ дней).

**Зачем отдельная таблица, а не флаг `dead=true` в `outbox`:**

- Hot path SELECT в relay сканирует `outbox` каждый тик. Если dead-строки лежали бы там же — нужно `WHERE NOT dead` или partial-индекс. Усложняет план.
- `outbox` концептуально «inbox того что нужно отправить» — пустая в стабильном состоянии. Dead — «не отправилось, разбирайся руками» — растёт монотонно, читается редко.
- Разное поведение по retention: outbox чистится автоматически (DELETE), outbox_dead — нет.

**Use-case'ы оператора:**

```sql
-- Сколько событий упало за сегодня по топикам
SELECT topic, count(*) FROM payment.outbox_dead
WHERE failed_at > now() - interval '24 hours' GROUP BY topic;

-- Что именно с конкретным платежом
SELECT id, payload, last_error, attempts, failed_at
FROM payment.outbox_dead WHERE payload->>'payment_id' = '...';

-- После починки downstream — вернуть событие в outbox для replay
BEGIN;
INSERT INTO payment.outbox (id, topic, payload, trace_context, created_at)
  SELECT id, topic, payload, trace_context, now()
  FROM payment.outbox_dead WHERE id = $1;
DELETE FROM payment.outbox_dead WHERE id = $1;
NOTIFY outbox;
COMMIT;
```

Это сознательно **ручная** операция. Автоматический replay из dead — путь к
infinite loop'ам когда сломан handler.

### `<schema>.processed_events` — consumer-side dedup

```sql
CREATE TABLE account.processed_events (
    event_id     uuid PRIMARY KEY,
    topic        text NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_account_processed_events_processed_at
    ON account.processed_events (processed_at);
```

**Поля:**

| Поле | Назначение |
|---|---|
| `event_id` | Тот же UUID, что и `<producer>.outbox.id`. End-to-end ID: outbox.id → `amqp.Publishing.MessageId` → `messaging.Event.ID` → сюда. **PRIMARY KEY** — основа дедупа. |
| `topic` | Дублируется из event'а. Для SQL-запросов и метрик («сколько событий какого топика обработали за час»). |
| `processed_at` | Когда handler успешно завершился. Используется для GC по возрасту. Индекс по нему — чтобы `DELETE WHERE processed_at < ...` был дешёвым. |

**Кто пишет/читает.** Только `consumers.Dedup` wrapper:

```go
func Dedup(uow, schema, h TxHandler) messaging.Handler {
    return func(ctx, e) error {
        return uow.Do(ctx, func(ctx, q) error {
            res, _ := q.ExecContext(ctx,
                "INSERT INTO account.processed_events (event_id, topic) "+
                "VALUES ($1, $2) ON CONFLICT DO NOTHING",
                e.ID, e.Topic)

            n, _ := res.RowsAffected()
            if n == 0 {
                return nil          // redelivery: ack, handler не вызван
            }
            return h(ctx, q, e)     // первая доставка: бизнес-write в той же tx
        })
    }
}
```

**Три исхода per delivery:**

| Сценарий | INSERT result | Tx-фейт | Видимый эффект |
|---|---|---|---|
| Первая доставка, handler ok | 1 row | commit | mark + бизнес-write коммитятся вместе → ack |
| Первая доставка, handler error | 1 row (потом rollback) | rollback | mark **тоже** откатывается → message redelivered |
| Redelivery (тот же event_id) | 0 rows (conflict) | commit | handler не вызван → ack |

Это и есть **exactly-once-effect** относительно этой БД: невозможно
состояние «эффект применён, mark не сохранился» или наоборот.

**Per-module, не общая.** Таблица — в схеме **consumer'а**, не producer'а.
Два разных consumer'а одного события (например `account` и `notifications`
оба слушают `payment.created`) дедупятся **независимо**: общая таблица
сломала бы это (один заmark'нул → второй пропустит).

**Что не годится для дедупа через эту таблицу.** `consumers.Dedup` гарантирует
exactly-once только для **DB-side эффектов** в той же транзакции. Не годится
когда handler:

- Делает HTTP-запрос наружу (например к платёжному провайдеру). HTTP-call не часть Postgres tx → на rollback запрос уже улетел.
- Публикует в другую очередь / шлёт email / SMS. То же самое.

Для таких handler'ов нужна **дополнительная** idempotency-стратегия —
idempotency-key на стороне callee или local outbox с ретраями. Это типовая
граница transactional outbox: только то, что атомарно с DB.

**Если event_id потерян.** `Event.ID == uuid.Nil` → `Dedup` возвращает
`ErrNoEventID` → consumer nack'ает → DLQ. Это сознательно: лучше нагрузить
DLQ и расследовать «откуда пришёл event без id», чем молча задвоить эффект.

**Рост и GC.** Таблица растёт **монотонно**. Под нагрузкой 100 events/s —
~3M строк в год; PRIMARY KEY на UUID, по 36 байт ключа + ~30 байт
метаданных → 3M × 100B = 300MB. Не катастрофа, но и не маленькая.

В [Operational TODOs](#operational-todos) зафиксирован cron-job:
```sql
DELETE FROM account.processed_events WHERE processed_at < now() - interval '30 days';
```

30 дней — компромисс: достаточно длинная окно, чтобы любой разумный
retry-storm не вылез за горизонт, и достаточно короткая, чтобы таблица не
была проблемой. Не реализован — нет реальной нагрузки.

**Не путать с idempotency middleware:**

| | `idempotency` middleware | `consumers.Dedup` |
|---|---|---|
| Уровень | HTTP request | event delivery |
| Storage | Redis (TTL 24h) | Postgres table |
| Ключ | client-supplied `Idempotency-Key` header | producer-supplied event_id (UUID) |
| Возвращает | cached response при replay | пропуск handler'а при redelivery |
| Гарантия | exactly-once response для retry клиента | exactly-once-effect для at-least-once delivery |

Они **не заменяют** друг друга и часто работают на одном flow: клиент шлёт
POST с `Idempotency-Key` → middleware дедупит retries клиента → handler
пишет outbox-row → relay диспатчит → `consumers.Dedup` дедупит broker-side
redelivery.

### Связи end-to-end

```
producer (payment) tx:
  ┌─────────────────────────┐
  │ payment.payments        │  (бизнес-write)
  │ payment.outbox          │  (id = UUID-X)
  └─────────────────────────┘
            ↓ NOTIFY outbox
relay:
  Dispatcher.Dispatch(ctx, UUID-X, "payment.created", payload)
            ↓
RabbitMQ:
  publish to exchange "events", routing_key "payment.created"
  amqp.Publishing.MessageId = UUID-X
            ↓
consumer (account):
  delivery.MessageId = UUID-X → messaging.Event.ID = UUID-X
            ↓
consumers.Dedup tx:
  ┌─────────────────────────┐
  │ account.processed_events│  INSERT event_id=UUID-X
  │ account.accounts        │  UPDATE balance ...
  └─────────────────────────┘
            ↓ commit
ack to broker → DELETE payment.outbox WHERE id=UUID-X
```

Один и тот же UUID-X светится в 3 местах и связывает три таблицы между
собой. Если что-то пошло не так — этот UUID и есть ключ, по которому можно
проследить судьбу события.

## Транзакции

```go
err := uow.Do(ctx, func(ctx context.Context, q postgres.Querier) error {
    return repo.Save(ctx, q, entity)
})

// типизированный результат:
res, err := postgres.InTx(ctx, uow, func(ctx, q) (Result, error) {
    return svc.Compute(ctx, q)
})
```

Репозитории принимают `postgres.Querier` (= `sqlx.ExtContext`) — один код
работает и на `*sqlx.DB`, и на `*sqlx.Tx`. Сценарий «UoW решает, обернуть ли в
tx» работает без проброса tx-флагов.

Семантика: на failed `Commit` follow-up `Rollback` **не делается** —
`database/sql` уже завершил tx, повторный rollback вернёт `ErrTxDone` и
зашумит реальную ошибку. Не возвращать.

## Платформенные пакеты

Реестр всего что живёт под `internal/platform/`, по слоям. Каждый пакет —
**что делает**, **публичная поверхность**, **ключевые механики**, **что
важно не упустить**.

### Карта

| Слой | Пакеты |
|---|---|
| Фундамент (типы и контракты) | [`apperror`](#apperror), [`messaging`](#messaging), [`postgres`](#postgres), [`config`](#config) |
| Транспорт (вход/выход процесса) | [`httpserver`](#httpserver), [`rabbitmq`](#rabbitmq), [`redis`](#redis) |
| Доставка событий | [`outbox`](#outbox), [`consumers`](#consumers), [`eventbus`](#eventbus) |
| Платформенные сервисы | [`idempotency`](#idempotency), [`crons`](#crons), [`featureflags`](#featureflags), [`security`](#security) |
| Observability | [`observability/logger`](#observabilitylogger), [`observability/metrics`](#observabilitymetrics), [`observability/tracing`](#observabilitytracing), [`observability/health`](#observabilityhealth) |

---

### apperror

**Зачем.** Типизированные доменные/прикладные ошибки, которые HTTP-слой
умеет переводить в HTTP-статусы без `if errors.Is(...)` лестницы в каждом
handler'е.

**Внутри.** Тип `Error` с полем `Kind` (enum: `Invalid`, `NotFound`,
`Conflict`, `Internal` и т.п.). Конструкторы `apperror.Invalid(msg)`,
`apperror.NotFound(msg)`, `Conflict`, `Internal`. Метод `Error() string` +
`Unwrap()`.

**Использование.**
- Handler возвращает `apperror.Invalid("bad uuid")` вместо
  `return c.Status(400)...`.
- `httpserver.ErrorHandler` смотрит на `Kind` и мапит на HTTP-код + JSON-тело
  `{"error": "..."}`.
- Sentinel-ошибки модулей (например `accountapi.ErrAccountNotFound`) могут
  оборачиваться в `apperror.NotFound(...)` в api-adapter'е.

**Зачем не просто `errors.New`.** Чтобы маппинг «ошибка → HTTP-код» жил
**в одном месте** (HTTP error handler) и не дублировался по handler'ам.

### messaging

**Зачем.** Тончайший контракт «событие + подписка», на который завязаны все
остальные пакеты доставки. **Только типы**, никаких реализаций.

```go
type Event struct {
    ID      uuid.UUID         // producer outbox row id, propagated end-to-end
    Topic   string
    Payload json.RawMessage
}

type Handler func(ctx context.Context, e Event) error

type Subscriber interface {
    Subscribe(topic string, h Handler)
}
```

**Why thin.** `Subscriber` — это интерфейс, который видят модули. Конкретный
имплементатор подменяется в composition root'е: `consumers.Subscriber`
(RabbitMQ) или `eventbus.Bus` (in-process). Модулю в `New()` приходит
`messaging.Subscriber` — ему всё равно, кто реально доставит сообщение.

**Что вынесено наружу намеренно.** `Event.ID` — для `consumers.Dedup`.
Producer (outbox) кладёт UUID в outbox-row, дальше: `outbox.id` →
`rabbitmq.Publisher.Dispatch` → `amqp.Publishing.MessageId` →
`consumers.Subscriber.handle` → `Event.ID`. См. [ADR-0005](docs/adr/0005-consumer-side-idempotency.md).

### postgres

**Зачем.** Пул соединений + транзакционная обёртка + универсальный
`Querier`-тип, чтобы один и тот же код работал и с пулом, и с tx.

**Файлы:**

- **`pool.go`** — `Connect(ctx, Config)` открывает `*sqlx.DB` (pgx driver),
  ставит `MaxOpenConns/MaxIdleConns/ConnMaxLifetime`. **Добавляет в DSN
  `statement_timeout=Nms`** если не задан вручную — серверная гарантия что
  зависший запрос будет убит Postgres'ом, даже если клиент игнорирует ctx.
  `cmd/migrate` использует raw DSN (миграции не должны убиваться).

- **`querier.go`** — однострочник:
  ```go
  type Querier = sqlx.ExtContext
  ```
  Алиас, не отдельный тип. `*sqlx.DB` и `*sqlx.Tx` оба удовлетворяют.
  Репозитории принимают `Querier` и работают одинаково в обоих режимах.

- **`uow.go`** — `UnitOfWork.Do(ctx, fn)` для tx без возврата значения,
  `InTx[T](ctx, uow, fn)` для типизированного возврата (Go не разрешает
  type-parameters на методах, поэтому `InTx` — функция). Семантика:
  - Бизнес-ошибка → `Rollback` + возврат ошибки;
  - Panic → `Rollback` + re-panic;
  - Failed `Commit` → **не** делаем follow-up `Rollback` (это вернёт
    `ErrTxDone` и зашумит реальную ошибку — `database/sql` уже завершил tx).

**Gotcha.** `Querier = sqlx.ExtContext` НЕ покрывает `Begin/Commit` — это
специально. Репозиторий не должен открывать свои tx; tx-граница принадлежит
сервису через `uow.Do`.

### config

**Зачем.** Загрузить YAML, наложить `APP_*` env-override, отдать
типизированную структуру `*Config`.

**Внутри.** viper: `SetConfigFile`, `AutomaticEnv` с префиксом `APP` и
replacer'ом `.` → `_`. `setDefaults` ставит безопасные дефолты — на проде
минимум полей в YAML.

**Структуры.** Per-domain: `HTTPConfig`, `DBConfig`, `OutboxConfig`,
`RabbitMQConfig`, `RedisConfig`, `ConsumersConfig`, `LogConfig` +
`FeatureFlags map[string]any`. Все теги `mapstructure:"..."`.

**Совместная работа с `security`.** Поля вида `secret:NAME` загружаются как
литералы; после `config.Load` вызывается
`security.ResolveSecrets(ctx, cfg, provider)` который проходит по
перечисленным полям и резолвит через `SecretsProvider`.

---

### httpserver

**Зачем.** Готовый Fiber-сервер с одинаковым middleware-стеком для всех
endpoint'ов и единым error-mapping.

**Что делает `New(Config, log)`:**
1. `fiber.New(fiber.Config{ErrorHandler: errorHandler, ...})` — `errorHandler`
   мапит `apperror.Error` → HTTP status + JSON; всё прочее → 500.
2. Middleware (порядок важен — сверху вниз):
   - **recover** — ловит panic, пишет `panics_total` + 500;
   - **tracing.Middleware** — стартует span, кладёт ctx через
     `c.SetUserContext`;
   - **timeoutMiddleware** — оборачивает `c.UserContext()` в
     `WithTimeout(RequestTimeout)`;
   - **requestLogger** — структурированный лог per-request с `trace_id`,
     `request_id`, `latency`, `method`, `path`, `status`;
   - **metrics.Middleware** — `http_requests_total` +
     `http_request_duration_seconds`, лейбл `route` берётся как Fiber pattern
     (`/api/v1/accounts/:id`), не литеральный path.
3. Возвращает `*Server` с методами `API() fiber.Router` (для `/api/v1`
   group), `Root() fiber.Router` (для `/healthz` и т.п.), `Start()`,
   `Shutdown(ctx)`.

**Ops endpoints** монтируются на `Root()` в composition root: `/healthz`,
`/readyz`, `/metrics`. В продакшене должны висеть за cluster-only доступом
(отдельный порт / auth) — сейчас на основном порту для простоты.

### rabbitmq

**Зачем.** AMQP-client + Publisher (= `outbox.Dispatcher`). Никакого
consumer-кода здесь — это в `consumers`.

**Внутри.**

- **`client.go`** — `Client` владеет одним AMQP connection. `Connect(cfg, log)`
  диалит и декларирует exchanges (`events` topic + `events.dlx` topic, оба
  durable). `Channel()` — **lazy-dial**: под mutex'ом проверяет cached
  connection, если `IsClosed` — re-dial + redeclare exchanges. Caller'ы не
  управляют состоянием reconnect'а, просто вызывают `Channel()` и обрабатывают
  ошибку через свой retry.
  - `HealthCheck(ctx)` — для `/readyz`; возвращает error если connection
    closed → 503.
  - `Close()` — graceful shutdown.

- **`publisher.go`** — `Publisher{client}` реализует `outbox.Dispatcher`.
  На каждый `Dispatch(ctx, eventID, topic, payload)`:
  1. Открывает channel из client'а (relay single-threaded → нет contention
     за переиспользование).
  2. Инжектит W3C TraceContext propagator в `amqp.Table` headers
     (`traceparent`, `tracestate`).
  3. `PublishWithContext` с `MessageId=eventID.String()`,
     `ContentType=application/json`, `DeliveryMode=Persistent`.

Channel создаётся per-publish и тут же закрывается — транзиентный failure
scoped'ится к одной строке outbox, retry-логика на стороне `outbox.Relay`.

### redis

**Зачем.** Тонкая обёртка над `go-redis/v9` для одного use case
(`idempotency.Storage`) и для `/readyz`.

**Внутри.** `Client{*redis.Client}` с методами `Connect(ctx, Config)`,
`HealthCheck(ctx)` (`PING`), `Close()`. Опциональный — если
`cfg.Redis.DSN == ""`, composition root его не строит и
`idempotency.Storage` остаётся `nil` → middleware не монтируется.

Не лезет в логику клиента — `idempotency.RedisStorage` оборачивает напрямую
методами `SetNX/Get/Del`.

---

### outbox

**Зачем.** Transactional outbox — два компонента: Publisher (пишет в outbox
в той же tx, что бизнес-write) и Relay (читает + диспатчит).

**Файлы.**

- **`publisher.go`** — `Publisher` интерфейс scoped к схеме:
  ```go
  outbox.NewPublisher("payment")  // payment.outbox
  ```
  В одной транзакции (через `q Querier`):
  1. `INSERT INTO <schema>.outbox (id=uuid.New(), topic, payload, trace_context)`.
  2. `NOTIFY outbox` — будит relay.

  Trace context сериализуется через `tracing.MarshalContext` и сохраняется
  в `trace_context jsonb` столбец.

- **`relay.go`** — единый relay на весь app, конфигурируется со списком
  схем. Запускается goroutine'ой из composition root.

  Жизненный цикл:
  1. Открывает **выделенное** pgx-соединение (sqlx-пул не годится для `LISTEN`).
  2. `LISTEN outbox` + начальный `drainAll` (вычерпывает накопленный backlog).
  3. Цикл `WaitForNotification(ctx, cfg.Interval)`:
     - NOTIFY → `drainAll`;
     - deadline expired → safety-net `drainAll` (для отложенных retry,
       которым NOTIFY не пришёл);
     - другая ошибка → reconnect с экспоненциальным backoff (1s→30s) +
       ±25% jitter.
  4. `drainAll` цикл по схемам: `dispatchBatch(schema)` — `FOR UPDATE SKIP
     LOCKED`, для каждой строки `Dispatcher.Dispatch(ctx, row.ID, row.Topic,
     row.Payload)`, и в той же tx один из трёх исходов:
     - **ack** → `DELETE`;
     - **retry** (`attempts+1 < MaxAttempts`) → `UPDATE attempts++,
       next_retry_at = now()+backoff, last_error`;
     - **dead** → `INSERT INTO <schema>.outbox_dead + DELETE`.
  5. Отдельная goroutine `refreshBacklog` каждые 30s гонит
     `SELECT count(*) FROM <schema>.outbox` и пишет в `outbox_backlog{schema}`
     gauge.

  Trace context **восстанавливается** перед каждым dispatch:
  `dispatchCtx := tracing.UnmarshalContext(ctx, row.TraceContext)`. Dispatcher
  и downstream consumer видят продолжение исходного span'а.

**Почему scale-out безопасен.** `FOR UPDATE SKIP LOCKED` гарантирует что
несколько подов забирают непересекающиеся батчи без блокировок. tx держится
открытой на время dispatch'а — для монолита с одним relay ok; для multi-relay
с большим throughput'ом нужен lease-based redesign.

### consumers

**Зачем.** RabbitMQ Subscriber + bulkhead + per-message timeout +
queue-depth poller + idempotency helper. Самый «толстый» платформенный
пакет.

**Файлы.**

- **`subscriber.go`** — `Subscriber` per consumer-имя (= имя модуля).
  Регистрация через `Subscribe(topic, handler)` wiring-time. На `Run(ctx)`:
  1. Спавнит goroutine per subscription → `consume(ctx, sub)`.
  2. Спавнит **один** poller goroutine → `pollQueueDepths(ctx)`.
  3. `WaitGroup.Wait` на shutdown.

  Per subscription:
  - **`consume`** loop: внешний reconnect с backoff+jitter; вызывает
    `consumeOnce` пока ctx жив.
  - **`consumeOnce`** — один здоровый session:
    1. Открывает channel.
    2. **Declare queue + DLQ + bindings** идемпотентно (повторный declare с
       теми же args = no-op).
    3. `ch.Qos(concurrency, 0, false)` — prefetch = bulkhead size.
    4. `ch.Consume` → `<-chan amqp.Delivery`.
    5. Цикл: на каждый delivery — берёт слот семафора (`chan struct{}`,
       buffered=concurrency), спавнит goroutine `s.handle(...)` которая
       возвращает слот.
  - **`handle`** per-message:
    1. Парсит `delivery.MessageId` (UUID) → `Event.ID`. Невалидный → warn +
       Nil.
    2. Восстанавливает trace context из AMQP headers через propagator.
    3. `context.WithTimeout(HandlerTimeout)`.
    4. `defer` метрики (`consumer_messages_total{status}`,
       `consumer_handle_duration_seconds`).
    5. `defer recover()` — panic → `status="panic"` + `nack(requeue=false)` →
       DLQ.
    6. Вызывает handler. `nil` → `ack`; error → `status="nack"` +
       `nack(requeue=false)`.

  **Queue-depth poller**:
  - Каждые `QueueDepthPollInterval` (default 30s) проходит по всем
    subscriptions.
  - На каждую queue делает **отдельный** AMQP channel
    (`QueueDeclarePassive` закрывает channel при ошибке missing-queue —
    общий channel убил бы остаток тика).
  - `QueueDeclarePassive` → `q.Messages` →
    `consumer_queue_depth{consumer, topic, kind=main|dlq}`.

- **`dedup.go`** — `Dedup(uow, schema, TxHandler) messaging.Handler`.
  Контракт в [ADR-0005](docs/adr/0005-consumer-side-idempotency.md):
  ```go
  type TxHandler func(ctx, q postgres.Querier, e Event) error

  func Dedup(uow *postgres.UnitOfWork, schema string, h TxHandler) messaging.Handler {
      return func(ctx, e) error {
          if e.ID == uuid.Nil { return ErrNoEventID }    // → nack → DLQ
          return uow.Do(ctx, func(ctx, q) error {
              res, _ := q.ExecContext(ctx,
                  "INSERT INTO <schema>.processed_events (event_id, topic) "+
                  "VALUES ($1,$2) ON CONFLICT DO NOTHING",
                  e.ID, e.Topic)
              if n, _ := res.RowsAffected(); n == 0 {
                  return nil           // redelivery → ack без вызова handler'а
              }
              return h(ctx, q, e)     // ошибка handler'а → rollback и mark, и business
          })
      }
  }
  ```
  Exactly-once-effect относительно той же БД: бизнес-write коммитится тогда
  и только тогда, когда коммитится dedup-mark.

**Конфигурация.** `Config{DefaultConcurrency, TopicConcurrency map[string]int,
HandlerTimeout, QueueDepthPollInterval}` приходит из `cfg.Consumers` +
per-topic overrides в composition root.

### eventbus

**Зачем.** In-process pub/sub. **Не используется в продакшен-composition**
(мы там через RabbitMQ), но оставлен в платформе.

**Use cases.**
- Тестирование handler'ов без RabbitMQ (отдать модулю `eventbus.Bus` вместо
  `consumers.Subscriber`).
- Простые случаи когда не нужна durable доставка (in-proc cross-module
  wiring).

**API.** Реализует и `messaging.Subscriber`, и `outbox.Dispatcher`.
`Dispatch` — запускает всех handler'ов топика в параллельных goroutine'ах,
**ждёт всех** и возвращает первую ненулевую ошибку. Wait — критично:
fire-and-forget сломал бы at-least-once.

---

### idempotency

**Зачем.** Безопасные retry для клиентов на mutating endpoints через
header `Idempotency-Key`.

**Файлы.**

- **`idempotency.go`** — типы. `Storage` интерфейс:
  ```go
  type Storage interface {
      Claim(ctx, key, ttl) (bool, error)   // SETNX sentinel
      Store(ctx, key, response, ttl) error // overwrite sentinel with real response
      Get(ctx, key) (*Response, bool, error)
      Release(ctx, key) error              // 5xx → delete sentinel for immediate retry
  }
  type Response struct { Status int; ContentType string; Body []byte }
  ```
  Константы: `inFlightTTL=60s`, `cachedTTL=24h`, sentinel-значение
  `"__in_flight__"`.

- **`redis_storage.go`** — единственная реализация поверх `platform/redis`.
  `Claim` → `SETNX <key> "__in_flight__" EX 60`. `Store` →
  `SET <key> <serialized> EX 86400`. `Get` десериализует и возвращает
  sentinel-aware bool.

- **`middleware.go`** — Fiber middleware:
  1. GET / HEAD → passthrough.
  2. POST/PUT/PATCH/DELETE без `Idempotency-Key` → passthrough (использование
     opt-in для клиента).
  3. С key:
     - `Claim` succeeded → process; после handler'а 2xx-4xx → `Store`,
       5xx → `Release`.
     - Sentinel still present (in-flight) → 409.
     - Cached → отдать `Response.Status/ContentType/Body`.
  4. Любая ошибка Storage → **fail open** (log + passthrough). Redis down
     не должен блокировать бизнес-трафик.

**Cache key.** `idempotency:<method>:<route>:<header>` — `route` это Fiber
pattern (не литеральный path), per-user пока нет (нет auth).

**Limitation.** Только Status/Content-Type/Body переживают replay. Custom
headers (`Location` и т.п.) теряются.

### crons

**Зачем.** Recurring jobs с leader election через Postgres advisory lock —
без отдельного scheduler-сервиса.

**Внутри.** `Scheduler` использует `robfig/cron/v3` для парсинга расписаний
(5-field standard cron). На `Register(name, schedule, job)` оборачивает job
в `runWithLock(name, job)`:

1. `db.Connx(ctx)` — выделенное соединение из пула.
2. `pg_try_advisory_lock($key)` где `$key = FNV-64a(name)`.
3. `false` → другой pod держит → skip + debug-log.
4. `true` → запуск job с `context.WithTimeout(jobTimeout=5min)`.
5. `pg_advisory_unlock($key)` + `conn.Close()`.

**Кризис-устойчивость.** При краше pod'а в середине job'ы соединение
закрывается → Postgres авто-релизит lock → следующий тик подбирает другой
pod. Никакой cleanup stale-lock'ов не нужен.

**Middleware-обёртки** на уровне robfig/cron:
- `cron.Recover(...)` — panic в job не убивает scheduler-goroutine.
- `cron.SkipIfStillRunning(...)` — overlapping ticks одной job'ы дропаются
  (а не queue'ются).

**Cost.** Long-running job держит **одно** соединение из пула на всю свою
длительность — keep jobs bounded или split.

```go
scheduler.Register("payment-cleanup", "0 3 * * *", func(ctx) error { ... })
```

### featureflags

**Зачем.** Абстракция над runtime-toggles. Сегодня — статический snapshot
из конфига; завтра — LaunchDarkly/Unleash/internal drop-in.

**Внутри.** Интерфейс `Provider`:
```go
type Provider interface {
    Bool(ctx, name string, defaultVal bool) bool
    String(ctx, name string, defaultVal string) string
    Int(ctx, name string, defaultVal int) int
    Count() int
}
```

Единственная реализация — `InMemoryProvider`. Конструктор
`NewInMemoryProvider(cfg.FeatureFlags)` копирует map'у через `maps.Copy`
(immutable snapshot). Test-only `Set(name, value)` для unit-тестов.

**ctx в сигнатуре** — для будущей remote-реализации, которая может решать
per-user/tenant из claims. `InMemoryProvider` его игнорит.

**Текущее.** Никакой модуль не потребляет — нужно явно принять
`featureflags.Provider` в `New()` модуля.

### security

**Зачем.** Резолв секретов из конфига через подменяемый provider. Сегодня —
env vars; завтра — Vault.

**Файлы.**

- **`security.go`** — интерфейс:
  ```go
  type SecretsProvider interface {
      Resolve(ctx context.Context, name string) (string, error)
  }
  ```
  + `EnvSecretsProvider{}` который делает `os.Getenv(name)`.

- **`resolve.go`** — `ResolveSecrets(ctx, cfg, provider)`:
  Проходит по **явному списку** полей (`cfg.DB.DSN`, `cfg.RabbitMQ.DSN`,
  `cfg.Redis.DSN`), для каждого: если начинается с `secret:`, отрезает
  префикс, зовёт `provider.Resolve(name)`, подставляет.

**Зачем явный список а не auto-walk.** Walk через reflection (`reflect.Value`
→ проверка всех string полей) хрупкий и наводнил бы конфиг рисками — поле
пользователя `Comment` с двоеточием случайно резолвилось бы. Явный список —
компромисс между удобством и предсказуемостью.

**Пример.**
```yaml
db:
  dsn: "secret:DATABASE_URL"
```
На старте `ResolveSecrets` → `os.Getenv("DATABASE_URL")` → подставляет в
`cfg.DB.DSN`.

---

### observability/logger

**Zerolog setup.** `New(level, env)` возвращает `zerolog.Logger`:
- `env=local` → human-friendly console-writer с цветами.
- иначе → JSON в stdout.
- Лейбл `component` ставится сабпакетами через
  `.With().Str("component", "rabbitmq").Logger()`.

Глобально не выставляется — все обращения через переданный `log` (functional
injection, не singleton).

### observability/metrics

**Внутри.** Все метрики — package-level `var` через `promauto.New*` против
default registry. Импорт пакета достаточен для регистрации.

| Имя | Тип | Источник записи |
|---|---|---|
| `http_request_duration_seconds{method,route,status}` | histogram | `metrics.Middleware` в httpserver |
| `http_requests_total{method,route,status}` | counter | то же |
| `outbox_backlog{schema}` | gauge | `outbox.Relay.refreshBacklog` каждые 30s |
| `outbox_dead_total{schema,topic}` | counter | `outbox.Relay` при `moveToDead` |
| `outbox_dispatch_duration_seconds{schema,topic}` | histogram | `outbox.Relay.dispatchBatch` per-row |
| `consumer_queue_depth{consumer,topic,kind}` | gauge | `consumers.Subscriber.pollQueueDepths` через `QueueDeclarePassive` |
| `consumer_messages_total{consumer,topic,status}` | counter | `consumers.Subscriber.handle`, status=`ack`/`nack`/`panic` |
| `consumer_handle_duration_seconds{consumer,topic}` | histogram | то же |
| `panics_total` | counter | recover middleware (placeholder) |

**`Middleware`** для Fiber — обёртка вокруг `c.Next()` с замером, читает
`c.Route().Path` для лейбла `route` (Fiber pattern, не литерал).

**`Handler() http.Handler`** — `promhttp.Handler()` для `/metrics`.
Монтируется через fiber `adaptor.HTTPHandler(...)`.

### observability/tracing

**Что wired.**

- **`Init() func(ctx) error`** — создаёт
  `sdktrace.NewTracerProvider(WithSampler(AlwaysSample))` **без exporter'а**:
  spans сэмплируются и дропаются. Чтобы начать слать в Tempo/Jaeger/etc —
  добавить `sdktrace.WithBatcher(otlpExporter)`. Возвращает shutdown-func
  для graceful flush.
- Глобальный propagator: `propagation.NewCompositeTextMapPropagator(TraceContext{},
  Baggage{})` — W3C-стандарт.

- **`Middleware(...)`** для Fiber — на каждый запрос:
  1. Извлекает `traceparent` из входящих headers через propagator.
  2. Стартует span `<METHOD> <route>` (route = Fiber pattern).
  3. `c.SetUserContext(spanCtx)` — следующие middleware/handler видят span
     через `c.UserContext()`.

- **`MarshalContext(ctx) ([]byte, error)`** и
  **`UnmarshalContext(ctx, []byte) ctx`** — сериализация/восстановление W3C
  propagator carrier для пересечения async-границы (outbox `trace_context
  jsonb` столбец).

**Цепочка propagation:**
```
HTTP request    tracing.Middleware extracts traceparent header
     ↓
service tx      uses ctx with span (zerolog пишет trace_id из этого ctx)
     ↓
outbox.Publish  MarshalContext → trace_context jsonb
     ↓
relay           UnmarshalContext → passes restored ctx to Dispatcher
     ↓
rabbitmq.Pub    injects carrier into amqp.Publishing.Headers
     ↓
consumer        Subscriber.handle extracts carrier from delivery.Headers
     ↓
TxHandler       runs with restored ctx — span тот же
```

В демо (см. конец README): trace_id `5e02489...` был и в HTTP-логе POST, и в
`traceparent` DLQ-сообщения.

### observability/health

**Зачем.** Готовые handler'ы для k8s probes.

**Внутри.**
- `Probe func(ctx) error` тип.
- `Liveness(c *fiber.Ctx) error` — всегда 200, `OK`. Только «процесс жив».
- `Readiness(probes ...Probe) fiber.Handler` — запускает все probes
  **последовательно** с общим 3s timeout; на первой ошибке → 503 с именем
  неуспешной.

В composition root:
```go
probes := []health.Probe{
    func(ctx) error { return db.PingContext(ctx) },
    rmq.HealthCheck,
}
if rdb != nil { probes = append(probes, rdb.HealthCheck) }
```

`/readyz` отдаёт 503 → k8s роутит трафик мимо pod'а.

---

## Timeouts

Три уровня, намеренно избыточные:

- **HTTP request timeout** (`cfg.http.request_timeout`, default 10s):
  middleware оборачивает `c.UserContext()` в `context.WithTimeout`. Handlers /
  services / repos, уважающие ctx, unwind'ятся при истечении.
- **DB statement timeout** (`cfg.db.statement_timeout`, default 5s):
  `postgres.Connect` добавляет `statement_timeout=Nms` в DSN; сервер убивает
  запросы независимо от того, уважает ли клиент ctx. `cmd/migrate` берёт raw
  DSN — миграции не убиваются.
- **HTTP shutdown timeout** (`cfg.http.shutdown_timeout`, default 15s, >
  request_timeout): graceful drain — request_timeout срабатывает раньше,
  shutdown_timeout даёт finally-логике дойти.
- **Consumer handler timeout** (`cfg.consumers.handler_timeout`, default
  30s): per-message ctx с deadline. Если handler не уважает ctx — он
  держит bulkhead slot до завершения, но не более этого времени.

## Распил на микросервисы

Цель шаблона — распил **без переписывания бизнес-кода**. Меняется только
composition root и реализация портов.

### Что НЕ меняется

- `domain/`, `repository/`, `service/`, `module.go` — код модуля.
- `api/` — становится gRPC-контрактом.
- Сигнатуры `messaging.Subscriber` / `messaging.Handler` — потребители не
  видят разницы между in-proc и broker'ом.

### Что меняется

1. **Composition root**: каждый модуль уезжает в свой процесс. В монолите
   на его месте остаётся клиент.
2. **Реализация `<name>api.Service`**: in-process adapter заменяется на
   gRPC-клиент с тем же интерфейсом. На стороне сервиса — gRPC-сервер,
   обёрнутый вокруг локального `service.Service`.
3. **Outbox publishing**: остаётся как есть. Producer-сторона
   приложения-сервиса пишет в свой outbox в своей БД, свой relay шлёт в
   тот же RabbitMQ. Альтернатива — Debezium CDC, если outbox-таблицу
   читать самим не хочется.
4. **Consumer side**: не меняется — `platform/consumers` уже про
   broker-based delivery. Просто запускается в другом процессе.
5. **БД**: вынос модуля в свой инстанс — миграции уже изолированы
   (`migrations/<module>/`), переносим папку.

### Пошаговый план

1. Зафиксировать `.proto` для каждого `<name>/api` (Service + DTO + event
   payloads + sentinel-ошибки → gRPC коды). Общий контрактный репо.
2. По одному модулю:
   - Поднять gRPC-сервер модуля, обёрнутый вокруг `service.Service`.
   - В монолите заменить in-process adapter на gRPC-клиент того же
     `<name>api.Service`-интерфейса. Поведение модулей не меняется.
3. Вынести БД модуля в свой инстанс.
4. Удалить модуль из монолита, оставив только gRPC-клиент и контрактный
   пакет.

На каждом шаге монолит работоспособен — инкрементальная миграция, не big bang.

## Запуск

```sh
make config    # один раз: config/config.yaml ← config/config.example.yaml
make up        # docker compose: Postgres + RabbitMQ + Redis
make tidy      # go mod tidy
make migrate   # применить миграции (cmd/migrate)
make run       # запустить cmd/api
make test      # go test ./...
make lint      # golangci-lint run
```

Конфиг — `config/config.yaml` (gitignored). Любое поле перекрывается env-vars
с префиксом `APP_`, разделитель `_`. Например: `APP_DB_DSN`, `APP_HTTP_PORT`,
`APP_CONSUMERS_DEFAULT_CONCURRENCY`. Поля вида `secret:NAME` резолвятся
через `SecretsProvider` (сегодня — `os.Getenv`).

Миграции **не запускаются автоматически**. На локалке — `make migrate`. В
k8s — отдельный Job per release.

## API

```sh
# создать счёт
curl -X POST localhost:8080/api/v1/accounts \
  -H 'Content-Type: application/json' \
  -d '{"owner_id":"<uuid>","currency":"USD"}'

# создать платёж — синхронно проверит существование счетов,
# асинхронно (через outbox + RabbitMQ) переведёт средства
curl -X POST localhost:8080/api/v1/payments \
  -H 'Content-Type: application/json' \
  -d '{"from_account_id":"<uuid>","to_account_id":"<uuid>","amount":1000}'
```

С опциональным idempotency-key:
```sh
curl -X POST localhost:8080/api/v1/payments \
  -H 'Idempotency-Key: <client-generated-uuid>' \
  -H 'Content-Type: application/json' \
  -d '{"from_account_id":"<uuid>","to_account_id":"<uuid>","amount":1000}'
```

## Поток выполнения API

Полный трейс каждого endpoint'а от HTTP-запроса до состояния в БД и брокере,
со всеми SQL-запросами и middleware.

### Общий middleware-стек

Любой запрос проходит снизу вверх по этим middleware (порядок установлен в
`httpserver.New`):

```
HTTP request (TCP) → Fiber acceptor
   │
   ↓ 1. recover middleware (Fiber built-in)
   │     panic → log + 500 + panics_total++
   │
   ↓ 2. tracing.Middleware
   │     extract `traceparent` header (W3C) via OTel propagator
   │     start span "<METHOD> <route-pattern>"
   │     c.SetUserContext(spanCtx)
   │     → дальше всё видит span через c.UserContext()
   │
   ↓ 3. timeoutMiddleware
   │     ctx, cancel := context.WithTimeout(c.UserContext(), 10s)
   │     c.SetUserContext(ctx); defer cancel()
   │     → handler/service/repo, уважающие ctx, unwind'нутся через 10s
   │
   ↓ 4. requestLogger
   │     start := time.Now()
   │     defer log: trace_id, request_id, method, path, status, latency
   │
   ↓ 5. metrics.Middleware
   │     defer:
   │       http_request_duration_seconds{method,route,status}.Observe(latency)
   │       http_requests_total{method,route,status}.Inc()
   │
   ↓ 6. idempotency.Middleware  (только если Redis включён, только mutating methods)
   │     if Idempotency-Key header:
   │       key := "idempotency:<method>:<route>:<header>"
   │       claim, _ := redis.SetNX(key, "__in_flight__", 60s)
   │       if cached    → return cached response, skip handler
   │       if in-flight → return 409
   │       if claimed   → process, потом:
   │         2xx-4xx → redis.Set(key, serialized_response, 24h)
   │         5xx     → redis.Del(key)
   │
   ↓ 7. handler
   │
   ↓ 8. errorHandler (Fiber config)
         apperror.Error → status + JSON {error: ...}
         любая другая   → 500
```

При **shutdown** (`SIGTERM`): `signal.NotifyContext` отменяет ctx →
`server.Shutdown(15s)` → Fiber перестаёт принимать новые соединения, ждёт
in-flight до 15s.

### `POST /api/v1/accounts` — создание счёта

```http
POST /api/v1/accounts
Content-Type: application/json
{ "owner_id": "<uuid>", "currency": "USD" }
```

```
fiber dispatch → AccountHandler.create(c)                       [cmd/api/handlers/account_handler.go]
   │
   │ 1. c.BodyParser(&req)                                       JSON → createAccountRequest
   │    err? → apperror.Invalid("invalid request body") → 400
   │
   │ 2. validator.Struct(req)                                    `validate:"required,uuid"` и `len=3`
   │    err? → apperror.Invalid(err.Error()) → 400
   │
   │ 3. uuid.Parse(req.OwnerID)                                  (уже валиден после validator)
   │
   │ 4. svc.CreateAccount(ctx, CreateAccountInput{...})          ← accountapi.Service
   │      ↓ через apiAdapter в account/module.go
   │      ↓
   │ 5. service.CreateAccount(ctx, ownerID, "USD")               [account/internal/service/service.go]
   │    │
   │    │ a. domain.NewAccount(ownerID, currency)                [account/internal/domain/account.go]
   │    │    проверяет ownerID != Nil, len(currency) == 3
   │    │    создаёт *Account: id=uuid.New(), balance=0, created_at=now(), updated_at=now()
   │    │
   │    │ b. uow.Do(ctx, func(ctx, q) error { ... })             [platform/postgres/uow.go]
   │    │    │
   │    │    │ BEGIN TRANSACTION
   │    │    │
   │    │    │ repo.Create(ctx, q, acc)                          [account/internal/repository/account_repository.go]
   │    │    │   INSERT INTO account.accounts
   │    │    │     (id, owner_id, currency, balance, created_at, updated_at)
   │    │    │   VALUES ($1, $2, $3, 0, $4, $4)
   │    │    │
   │    │    │ err? → ROLLBACK + return err
   │    │    │ ok?  → COMMIT
   │    │    └─
   │    │
   │    └─ return *domain.Account
   │
   │ 6. apiAdapter.toAPI(acc)                                    *domain.Account → accountapi.Account
   │
   ↓ 7. c.Status(201).JSON(accountToResponse(acc))               accountapi.Account → accountResponse JSON
```

**Что записано в БД:** `account.accounts` — одна новая строка, `balance=0`.
**Side effects:** никаких. Это чистый write без событий.
**Метрики:** `http_requests_total{method="POST", route="/api/v1/accounts/", status="201"}` +1.

### `GET /api/v1/accounts/:id` — чтение счёта

```http
GET /api/v1/accounts/ab1ab15f-58f8-4f78-8616-79c7dd1e180d
```

```
fiber → AccountHandler.getByID(c)                                [account_handler.go]
   │
   │ 1. uuid.Parse(c.Params("id"))
   │    err? → apperror.Invalid("invalid account id") → 400
   │
   │ 2. svc.GetByID(ctx, id)                                     ← accountapi.Service
   │      ↓ apiAdapter.GetByID
   │      ↓
   │ 3. service.GetAccount(ctx, id)                              [account/internal/service/service.go]
   │    │
   │    │ repo.GetByID(ctx, s.db, id)                            ← вне tx, прямо на пуле!
   │    │   SELECT id, owner_id, currency, balance, created_at, updated_at
   │    │   FROM account.accounts WHERE id = $1
   │    │
   │    │   sql.ErrNoRows → domain.ErrAccountNotFound (apperror.NotFound)
   │    │   apiAdapter подменяет на accountapi.ErrAccountNotFound
   │    │
   │ 4. toAPI(acc)
   │
   ↓ 5. c.JSON(accountToResponse(acc))                           → 200, JSON
```

**Read вне tx:** `s.repo.GetByID(ctx, s.db, id)` передаёт **пул** напрямую
(не открывает tx). Чтения не требуют UoW.
**Auto-mapping ошибок:** `domain.ErrAccountNotFound` уже типа
`apperror.NotFound(...)` → `errorHandler` мапит в 404.

### `POST /api/v1/payments` — создание платежа (полный async-flow)

Самый интересный endpoint — триггерит цепочку из **3 транзакций** + RabbitMQ.

```http
POST /api/v1/payments
Content-Type: application/json
{ "from_account_id": "<uuid>", "to_account_id": "<uuid>", "amount": 1000 }
```

#### Фаза 1: HTTP handler + sync проверки

```
fiber → PaymentHandler.create(c)                                 [cmd/api/handlers/payment_handler.go]
   │
   │ 1. BodyParser + validator (UUID, amount>0)
   │
   │ 2. svc.CreatePayment(ctx, CreatePaymentInput{...})          ← paymentapi.Service
   │      ↓ apiAdapter (payment/module.go)
   │      ↓
   │ 3. service.CreatePayment(ctx, in)                           [payment/internal/service/service.go]
   │    │
   │    │ a. ── Sync cross-module через api ──
   │    │    s.accounts.GetByID(ctx, in.FromAccountID)           ← accountapi.Service
   │    │      ↓ apiAdapter.GetByID
   │    │    accountSvc.GetAccount → SELECT FROM account.accounts WHERE id=$1
   │    │      err? → return ErrAccountNotFound
   │    │
   │    │    s.accounts.GetByID(ctx, in.ToAccountID)             ← второй SELECT
   │    │
   │    │ b. if from.Currency != to.Currency:
   │    │       return domain.ErrCurrencyMismatch                ← apperror.Conflict → 409
   │    │
   │    │ c. domain.NewPayment(from, to, amount, currency)       [payment/internal/domain/payment.go]
   │    │    проверяет from!=to, amount>0
   │    │    создаёт *Payment: id=uuid.New(), status="pending", created_at=now()
```

На этом этапе ничего ещё не записано — только два SELECT'а.

#### Фаза 2: transactional outbox

```
   │    │ d. ── Бизнес-write + событие в ОДНОЙ tx ──
   │    │    uow.Do(ctx, func(ctx, q) error {
   │    │        BEGIN
   │    │
   │    │        // d.1 — бизнес-write
   │    │        repo.Create(ctx, q, payment)                    [payment/internal/repository/payment_repository.go]
   │    │            INSERT INTO payment.payments
   │    │                (id, from_account_id, to_account_id, amount, currency, status, created_at)
   │    │            VALUES ($1, $2, $3, $4, $5, 'pending', $6)
   │    │
   │    │        // d.2 — событие в outbox
   │    │        publisher.Publish(ctx, q, "payment.created", PaymentCreated{...})
   │    │            ↓                                            [platform/outbox/publisher.go]
   │    │            json.Marshal(payload)
   │    │            tracing.MarshalContext(ctx) → []byte         ← W3C TraceContext в jsonb
   │    │
   │    │            INSERT INTO payment.outbox
   │    │                (id, topic, payload, trace_context)
   │    │            VALUES (uuid.New(), 'payment.created', $payload_json, $trace_jsonb)
   │    │
   │    │            EXEC "NOTIFY outbox"                         ← будит relay
   │    │
   │    │        COMMIT                                           ← оба INSERT'а атомарны;
   │    │                                                          NOTIFY доставится только при коммите
   │    │    })
   │    │
   │    └─ return *Payment
   │
   │ 4. apiAdapter.toAPI(payment) → paymentapi.Payment           status="pending"
   │
   ↓ 5. c.Status(201).JSON(paymentToResponse(p))                 ← клиент получает ответ
                                                                  HTTP-цикл закончен.
                                                                  Settlement происходит асинхронно ниже.
```

**Состояние БД сразу после COMMIT:**

| Таблица | Изменение |
|---|---|
| `payment.payments` | +1 строка, status='pending' |
| `payment.outbox` | +1 строка, id=UUID-X, attempts=0 |
| `account.accounts` | без изменений |

#### Фаза 3: outbox relay → AMQP

`outbox.Relay.Run` живёт в отдельной goroutine'е (`go relay.Run(ctx)` в
`cmd/api/main.go`):

```
relay listening on dedicated pgx conn:                           [platform/outbox/relay.go]
    LISTEN outbox

NOTIFY от COMMIT'а будит relay → drainAll:
    для каждой схемы в []string{"account", "payment"}:
        dispatchBatch("payment"):
            BEGIN                                                ← новая tx, отдельная от той что писала
            SELECT id, topic, payload, trace_context, attempts, created_at
            FROM payment.outbox
            WHERE next_retry_at <= now()
            ORDER BY created_at LIMIT 100
            FOR UPDATE SKIP LOCKED                               ← scale-out safe

            для каждой строки:
                dispatchCtx := tracing.UnmarshalContext(ctx, row.TraceContext)
                                                                 ← восстанавливает span
                rabbitmq.Publisher.Dispatch(dispatchCtx, row.ID, row.Topic, row.Payload):
                                                                 [platform/rabbitmq/publisher.go]
                    ch := client.Channel()                       ← lazy-dial если connection упал
                    carrier := MapCarrier{}
                    otel.GetTextMapPropagator().Inject(dispatchCtx, carrier)
                                                                 ← traceparent, tracestate в map
                    headers := amqp.Table{"traceparent": "...", "tracestate": "..."}

                    ch.PublishWithContext(events_exchange, "payment.created",
                        amqp.Publishing{
                            MessageId:    row.ID.String(),       ← end-to-end event_id
                            ContentType:  "application/json",
                            Body:         row.Payload,
                            DeliveryMode: amqp.Persistent,
                            Headers:      headers,
                        })

                    ch.Close()
                    return nil

                metrics.OutboxDispatchDuration{schema,topic}.Observe(elapsed)

                success:
                    DELETE FROM payment.outbox WHERE id = $1     (ack)

                ─ или ─

                error, attempts+1 < MaxAttempts (5):
                    UPDATE payment.outbox SET
                        attempts = attempts+1,
                        next_retry_at = now() + backoff(attempts),  ← exp + jitter, cap 60s
                        last_error = $err_text
                    WHERE id = $1

                ─ или ─

                error, attempts+1 >= MaxAttempts:
                    INSERT INTO payment.outbox_dead (...)
                    SELECT ... FROM payment.outbox WHERE id = $1
                    DELETE FROM payment.outbox WHERE id = $1
                    metrics.OutboxDeadTotal{schema,topic}++

            COMMIT
```

**Состояние после успешного dispatch:**

| Таблица / Очередь | Изменение |
|---|---|
| `payment.outbox` | строка с id=UUID-X удалена |
| RabbitMQ exchange `events` | сообщение опубликовано с routing_key=`payment.created`, MessageId=UUID-X |

#### Фаза 4: RabbitMQ routing

```
exchange "events" (topic)
    routing_key = "payment.created"
        ↓ binding (declared в consumeOnce)
    queue "account.payment.created" (durable, x-dead-letter-exchange=events.dlx)
        ↑ один consumer держит её через ch.Consume
```

Сообщение появляется в `account.payment.created` очереди.

#### Фаза 5: consumer обрабатывает

```
consumers.Subscriber.consumeOnce loop                            [platform/consumers/subscriber.go]
    delivery приходит из <-chan amqp.Delivery

    sem <- struct{}{}                                            ← взять слот семафора (concurrency=4)
    go func(d) {
        defer { <-sem }
        s.handle(ctx, sub, d):

            // 5.1 — propagate event_id
            eventID, _ := uuid.Parse(d.MessageId)                ← UUID-X
            // 5.2 — restore trace context
            carrier := MapCarrier(d.Headers as strings)
            ctx = otel.GetTextMapPropagator().Extract(ctx, carrier)
                                                                 ← span continues from producer's

            ev := messaging.Event{
                ID:      eventID,                                ← UUID-X
                Topic:   "payment.created",
                Payload: d.Body,
            }

            // 5.3 — per-message timeout
            ctx, cancel := context.WithTimeout(ctx, 30s)
            defer cancel()

            // 5.4 — observability
            defer record(consumer_handle_duration_seconds)
            defer record(consumer_messages_total{status})
            defer recover() → status="panic" + nack→DLQ

            // 5.5 — call handler (= Dedup wrapper из account/module.go)
            err := sub.handler(ctx, ev)
                ↓ это consumers.Dedup(uow, "account", m.onPaymentCreatedTx)
                ↓
                Dedup func:                                      [platform/consumers/dedup.go]
                    if ev.ID == uuid.Nil → return ErrNoEventID

                    uow.Do(ctx, func(ctx, q) error {
                        BEGIN                                    ← новая tx у consumer'а

                        // 5.5.1 — dedup mark
                        res := q.Exec("INSERT INTO account.processed_events
                            (event_id, topic) VALUES ($1, $2)
                            ON CONFLICT DO NOTHING", UUID-X, "payment.created")

                        if res.RowsAffected() == 0:
                            return nil                           ← redelivery → COMMIT empty tx → ack

                        // 5.5.2 — handler (бизнес-write в той же tx)
                        return m.onPaymentCreatedTx(ctx, q, ev)
                            ↓                                    [account/module.go]
                            json.Unmarshal(ev.Payload, &evt)
                            return svc.TransferTx(ctx, q, evt.From, evt.To, evt.Amount)
                                ↓                                [account/internal/service/service.go]
                                from := repo.GetByID(ctx, q, fromID)
                                       SELECT FROM account.accounts WHERE id=$1
                                to   := repo.GetByID(ctx, q, toID)
                                       SELECT FROM account.accounts WHERE id=$1

                                if !from.CanDebit(amount):
                                    return domain.ErrInsufficient
                                                                 ← tx rollback в Dedup → mark тоже откатывается
                                                                 → nack → DLQ

                                repo.UpdateBalance(ctx, q, from.ID, from.Balance-amount)
                                    UPDATE account.accounts
                                    SET balance=$2, updated_at=now()
                                    WHERE id=$1
                                repo.UpdateBalance(ctx, q, to.ID, to.Balance+amount)
                                    UPDATE account.accounts
                                    SET balance=$2, updated_at=now()
                                    WHERE id=$1

                                return nil

                        COMMIT                                   ← processed_events + 2× UPDATE atomically
                    })

            if err != nil:
                nack(requeue=false) → DLX → "account.payment.created.dlq"
                metrics.ConsumerMessagesTotal{status="nack"}++

            else:
                ack
                metrics.ConsumerMessagesTotal{status="ack"}++
    }(d)
```

**Финальное состояние БД (успешный сценарий):**

| Таблица | Изменение |
|---|---|
| `payment.payments` | status='pending' (не меняется — это намеренно: status мог бы обновляться через ещё одно событие `account.transferred`, но в шаблоне такого нет) |
| `payment.outbox` | пусто (удалено в фазе 3) |
| `account.processed_events` | +1 строка, event_id=UUID-X |
| `account.accounts` | balance from −1000, to +1000 |
| RabbitMQ `account.payment.created` | пусто (ack'нуто) |

**Если transfer упал** (например `insufficient funds`):

| Таблица | Изменение |
|---|---|
| `payment.payments` | status='pending' (не тронут) |
| `payment.outbox` | пусто (publish успешный, удалено) |
| `account.processed_events` | **пусто** (tx откатилась → mark тоже) |
| `account.accounts` | balances не тронуты (UPDATE откатился) |
| RabbitMQ `account.payment.created.dlq` | +1 сообщение с `MessageId=UUID-X`, `traceparent=<тот же span>`, `x-death.reason=rejected` |

Это и есть **exactly-once-effect**: либо все 3 INSERT/UPDATE (mark + 2
баланса) случились вместе, либо никто.

#### Trace continuity (наблюдается извне)

В логах:
```
3:41PM INF http request method=POST path=/api/v1/payments trace_id=70432e89313a2b4cb69bcdc38500b13f
3:41PM INF settling payment event_id=a477bec8-... trace_id=70432e89313a2b4cb69bcdc38500b13f   ← consumer
```

Один `trace_id` от HTTP request до consumer handler.

### `GET /api/v1/payments/:id` — чтение платежа

Симметрично `GET /accounts/:id`:

```
fiber → PaymentHandler.getByID(c)                                [payment_handler.go]
   │
   │ 1. uuid.Parse(c.Params("id"))
   │ 2. svc.GetByID(ctx, id) ← apiAdapter ← service.GetPayment
   │
   │ 3. repo.GetByID(ctx, s.db, id)                              ← вне tx, прямо на пуле
   │       SELECT id, from_account_id, to_account_id, amount, currency, status, created_at
   │       FROM payment.payments WHERE id = $1
   │       sql.ErrNoRows → domain.ErrPaymentNotFound (apperror.NotFound)
   │
   ↓ 4. c.JSON(paymentToResponse(p))
```

### Сводка: что куда уезжает

**Хранилища данных:**

| Хранилище | Что лежит |
|---|---|
| `account.accounts` | owner_id, currency, balance |
| `account.processed_events` | дедуп-метки события (event_id PK) |
| `account.outbox` / `outbox_dead` | сейчас пусто (account ничего не публикует) |
| `account.goose_db_version` | состояние миграций account-модуля |
| `payment.payments` | from/to, amount, status |
| `payment.outbox` / `outbox_dead` | producer outbox payment'а |
| `payment.processed_events` | сейчас пусто (payment ничего не consume'ит) |
| `payment.goose_db_version` | миграции payment-модуля |
| `public.goose_db_version` | миграции base-слоя (схемы) |
| Redis `idempotency:*` | HTTP idempotency-keys + cached responses |
| RabbitMQ `events`/`events.dlx` | exchanges (no storage, routing only) |
| RabbitMQ `account.payment.created` | durable queue консьюмера account |
| RabbitMQ `account.payment.created.dlq` | сообщения, упавшие handler-side |

**Транзакции на один POST /payments (успешный сценарий):**

| # | Tx | Что внутри |
|---|---|---|
| Tx-1 | `payment.CreatePayment` | INSERT payment.payments + INSERT payment.outbox + NOTIFY |
| Tx-2 | `outbox.Relay.dispatchBatch` | SELECT FOR UPDATE SKIP LOCKED + AMQP publish + DELETE payment.outbox |
| Tx-3 | `consumers.Dedup` в account | INSERT account.processed_events + 2× SELECT account.accounts + 2× UPDATE account.accounts |

Три независимых tx в трёх goroutine'ах (HTTP handler / relay / consumer
worker), связанных одним `event_id = UUID-X`.

**Ошибки на каком уровне:**

| Уровень | Сценарий | Что происходит |
|---|---|---|
| HTTP | bad JSON / invalid uuid / amount<=0 | `apperror.Invalid` → 400 |
| HTTP idempotency | sentinel still in-flight | 409 Conflict |
| Account check (sync) | `from.Currency != to.Currency` | `apperror.Conflict` → 409 |
| Account check | account не найден | `apperror.NotFound` → 404 |
| Tx-1 (writing payment) | DB down / unique violation | `apperror.Internal` → 500, `idempotency.Release` |
| Tx-2 (relay) | RabbitMQ down | `attempts++`, retry с backoff; после 5 попыток → `outbox_dead` |
| Tx-3 (settlement) | `insufficient funds` | tx rollback → DLQ, payment остаётся `pending` навсегда (бизнес-логика "что делать с pending → DLQ" — TODO для шаблона) |

## Как добавить модуль

1. Создать `internal/modules/<name>/` со структурой выше.
2. В `<name>/api/` объявить `Service`-интерфейс + DTO + event-topics +
   sentinel-ошибки.
3. В `<name>/internal/{domain,repository,service}/` реализовать слои.
4. Создать `<name>/module.go` с `New(...) *Module` и `API() *api.Service`.
5. Создать `migrations/<name>/`:
   - `*_init.sql` — схема + бизнес-таблицы;
   - `*_outbox.sql` — если модуль публикует события;
   - `*_processed_events.sql` — если модуль их потребляет.
6. В `migrations/migrations.go` добавить `//go:embed all:<name>`.
7. В `cmd/api/main.go` (`run()`):
   - сконструировать модуль: `<name> := <name>mod.New(deps...)`;
   - если потребляет — добавить subscriber с подписками внутри `module.go`;
   - если публикует — добавить схему в `outbox.NewRelay(... schemas ...)`;
   - передать `<name>.API()` в `registerRoutes(...)`.

## Как добавить cron job

В `New(...)` модуля:
```go
scheduler.Register("account-cleanup-stale", "0 4 * * *", m.cleanupStale)
```

Multi-pod safe из коробки (PG advisory lock). `Scheduler` живёт в
composition root.

## Тесты

- Доменный слой — обычные unit-тесты (`account/internal/domain/account_test.go`).
- Репозитории — интеграционные тесты на реальном Postgres (testcontainers-go;
  не включены в шаблон).
- Сервисы — с моками `domain.Repository` и `<name>api.Service` зависимых модулей.
- HTTP handlers — с моком `<name>api.Service` и `fiber.App.Test(...)`.
- `internal/platform/postgres` — sqlmock-тесты для `UnitOfWork`/`InTx`.

## Operational TODOs

Что осознанно осталось без реализации в шаблоне — добавлять при появлении
реальной нагрузки:

- **GC `processed_events`**: `DELETE WHERE processed_at < now() - interval '30 days'`
  как cron-job. Сейчас таблица растёт без верхнего предела.
- **VaultSecretsProvider** + ротация секретов: интерфейс есть, реализация —
  при появлении Vault в кластере.
- **Remote feature flags provider**: интерфейс есть, реализация — при выборе
  vendor'а (LaunchDarkly, Unleash, internal).
- **OTLP exporter**: tracing wired, exporter no-op. Добавить
  `sdktrace.WithBatcher(otlpExporter)` в `tracing.Init` при появлении коллектора.
- **Outbox партиционирование** при очень высоких QPS (DELETE-only нагрузка
  упирается в autovacuum). Сейчас не нужно — DELETE сразу освобождает место
  и `idx_*_outbox_next_retry` обслуживает основной запрос.

## Документация

- [`CLAUDE.md`](CLAUDE.md) — конвенции и нюансы для агентов / новых разработчиков.
- [`docs/adr/`](docs/adr/) — Architecture Decision Records.
- Этот файл — обзорно и как пользоваться.
