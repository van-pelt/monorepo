# monorepo

Шаблон модульного монолита (с парой модулей для примера) на Go с чистой архитектурой, готовый к
последующему распилу на микросервисы.

## Стек

- **Go 1.25**, HTTP — **Fiber v2** (заменить потом на v3 когда стабильна будет)
- **PostgreSQL**, доступ через **sqlx** + драйвер **pgx/v5**
- LISTEN/NOTIFY в outbox-relay — нативный **pgx/v5**
- Миграции — **goose**, по папке на модуль
- Конфиг — **viper** (yaml + env-override), логгер — **zerolog**
- Валидация — **go-playground/validator**

## Структура

```
cmd/app                           точка входа
internal/
  app/                            composition root: сборка и жизненный цикл
  modules/
    account/                      модуль счетов
      accountport/                публичный контракт модуля для других модулей
      domain/                     сущности + бизнес-правила + Repository-интерфейс
      repository/                 реализация Repository через sqlx
      service/                    use cases (Service-интерфейс + unexported impl)
      transport/                  HTTP-handlers + request/response DTO
      migrations/                 goose-миграции в схему account
      module.go                   wiring + реализация module.Module + подписки
    payment/                      модуль платежей (та же структура)
  shared/
    apperror/                     типизированные ошибки (Kind → HTTP status)
    config/                       viper-конфиг (yaml + APP_*-env override)
    httpserver/                   Fiber-сервер, middleware, error mapping
    logger/                       zerolog setup
    messaging/                    Publisher + Subscriber + Engine (outbox + bus + relay)
    module/                       Module-интерфейс (Name, RegisterRoutes, Migrations)
    postgres/                     pool, Querier-alias, UnitOfWork, InTx[T]
migrations/                       базовые миграции: схемы + таблица outbox
```

## Архитектура модуля

Каждый модуль — самодостаточный вертикальный срез со слоями
`transport → service → domain ← repository`. Зависимости направлены к
`domain`; `domain` не зависит ни от чего, кроме общих утилит.

### Слои

- **domain** — сущности (`Account`, `Payment`), бизнес-правила, доменные
  ошибки, интерфейс `Repository`. Не знает про БД, HTTP и фреймворки.
- **repository** — sqlx-реализация `Repository`. Принимает
  `postgres.Querier`, чтобы один и тот же код работал и на `*sqlx.DB`, и на
  `*sqlx.Tx` (см. Unit of Work).
- **service** — use cases. Объявлен как **интерфейс** `Service` рядом с
  unexported реализацией; `New(...)` возвращает интерфейс. Это позволяет
  потребителям (handler, event-handler, port-adapter) подменять сервис
  моком в тестах.
- **transport** — HTTP-handlers. `handler.go` — только маршруты и парсинг,
  `dto.go` — request/response-структуры и мапперы. Fiber никогда не утекает
  в service/domain.
- **module.go** — wiring модуля: собирает слои, реализует `module.Module`
  (`Name`, `RegisterRoutes`, `Migrations`), подписывается на события через
  `messaging.Subscriber`, экспонирует свой публичный порт.
- **`<name>port/`** — leaf-пакет без внутренних зависимостей. Содержит
  интерфейсы (например, `accountport.AccountProvider`), DTO (`AccountInfo`),
  топики (`paymentport.TopicPaymentCreated`) и payload-структуры событий
  (`PaymentCreated`). Это единственное, что чужой модуль имеет право
  импортировать.

### Границы модулей

- У каждого модуля своя **схема** в одной БД (`account.*`, `payment.*`).
- Между схемами **нет внешних ключей** — ссылки по ID, валидация через
  port. Это контракт распила: при выносе в отдельную БД ничего не сломается.
- Импорт `domain`/`service`/`repository` чужого модуля **запрещён**
  линтером (`depguard` в `.golangci.yml`). Разрешён только `<name>port/`.

## Межмодульное взаимодействие

Два механизма, осознанный выбор по сценарию.

### Когда использовать порт (синхронно)

- Нужен **немедленный ответ** для принятия решения здесь и сейчас (валидация,
  чтение справочника).
- Вызывающий **готов ждать** и считать сбой вызываемого своим сбоем.
- Не нужны транзакционные гарантии «или оба, или ни одного».

Пример: `payment.CreatePayment` перед записью платежа спрашивает у
`accountport.AccountProvider`, существуют ли счета и совпадают ли валюты.
Без этой информации платёж создавать нельзя — поэтому синхронно.

### Когда использовать outbox (асинхронно)

- Это **побочный эффект**, который может произойти позже (минуты — норма).
- Вызывающий **не должен** падать, если получатель временно недоступен.
- Нужна **гарантия доставки**: событие не должно пропасть, даже если процесс
  упадёт сразу после коммита.
- Получатель — другой модуль, его реакция допускает eventual consistency.

Пример: создание платежа должно привести к переводу средств между счетами.
Это можно сделать асинхронно — `payment` пишет `payment.created` в outbox в
той же транзакции, что и сам платёж; `account` подпиской выполняет перевод.
Если процесс упадёт между commit-ом платежа и публикацией события — событие
останется в outbox и доедет на следующем запуске.

### Что НЕ нужно делать

- Не вызывать другой модуль через порт ради побочного эффекта (получите
  тесное сцепление + потеряете гарантии при сбое).
- Не публиковать событие, когда нужен ответ для бизнес-решения (вызывающий
  не дождётся — eventual consistency).

## Outbox-механизм

### Таблица

```sql
CREATE TABLE public.outbox (
    id            uuid PRIMARY KEY,
    topic         text NOT NULL,
    payload       jsonb NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    attempts      integer NOT NULL DEFAULT 0,
    next_retry_at timestamptz NOT NULL DEFAULT now(),
    last_error    text
);
CREATE INDEX idx_outbox_next_retry ON public.outbox (next_retry_at, created_at);

CREATE TABLE public.outbox_dead (
    id          uuid PRIMARY KEY,
    topic       text NOT NULL,
    payload     jsonb NOT NULL,
    created_at  timestamptz NOT NULL,
    attempts    integer NOT NULL,
    last_error  text,
    failed_at   timestamptz NOT NULL DEFAULT now()
);
```

Одна общая таблица, не разрезанная по модулям — у неё нет домена. После
успешной доставки relay **удаляет** строку, поэтому в `outbox` лежит только
backlog (новые + ретраимые). `outbox_dead` хранит события, выбившие
`max_attempts`, для ручного разбора.

### Producer: `messaging.Publisher.Publish`

```go
s.uow.Do(ctx, func(ctx context.Context, q postgres.Querier) error {
    if err := s.repo.Create(ctx, q, payment); err != nil {
        return err
    }
    return s.publisher.Publish(ctx, q, paymentport.TopicPaymentCreated, event)
})
```

`Publish` делает в той же транзакции (через `q`):

1. `INSERT INTO public.outbox(...)`.
2. `NOTIFY outbox` — будит relay, чтобы не ждать следующий poll.

`NOTIFY` в Postgres **транзакционен**: уведомление доставляется только при
commit транзакции. Значит relay никогда не увидит событие, чьи бизнес-данные
ещё не закоммичены.

### Relay: `messaging.Engine.Run`

Запускается из композиционного корня как goroutine; работает до отмены
context-а.

1. Открывает **выделенное** соединение через `pgx.Connect(dsn)` (пул `sqlx`
   не подходит — `LISTEN/NOTIFY` требует свою сессию на всё время жизни
   подписки).
2. `LISTEN outbox`.
3. `drain()` — на старте вычерпывает backlog, который мог накопиться, пока
   relay был выключен.
4. В цикле `WaitForNotification(ctx, interval)`:
   - пришло уведомление → `drain()`;
   - истёк `interval` (safety-net polling на случай пропуска NOTIFY при
     реконнекте) → тоже `drain()`;
   - другая ошибка → выход, реконнект через `reconnectDelay`.

### Доставка батча: `dispatchBatch`

```sql
BEGIN;
SELECT id, topic, payload, attempts, created_at FROM public.outbox
WHERE next_retry_at <= now()
ORDER BY created_at LIMIT $1 FOR UPDATE SKIP LOCKED;
-- для каждой строки: dispatch (синхронно, ждём всех подписчиков) → ack | retry | dead
COMMIT;
```

- `FOR UPDATE SKIP LOCKED` делает relay безопасным для **scale-out**:
  несколько инстансов забирают непересекающиеся батчи без блокировок.
- `WHERE next_retry_at <= now()` фильтрует события, у которых ещё не подошло
  время повторной попытки.
- `dispatch` **синхронно** перебирает подписчиков (параллельно через
  goroutines + WaitGroup) и **дожидается всех результатов** перед тем, как
  решить судьбу строки. Это критично: fire-and-forget потерял бы ошибки и
  сломал at-least-once.
- Три исхода для каждой строки внутри одной транзакции:
  - **ack** — все обработчики вернули `nil` → `DELETE FROM outbox`.
  - **retry** — хоть один вернул error и `attempts+1 < max_attempts` →
    `UPDATE outbox SET attempts++, next_retry_at = now() + backoff,
    last_error = ...`.
  - **dead** — `attempts+1 >= max_attempts` → `INSERT INTO outbox_dead +
    DELETE FROM outbox`.

Транзакция держится открытой на время работы обработчиков — допустимо для
монолита с одним relay. Для multi-relay scale-out стоит перейти на
lease-based pattern: claim (SELECT FOR UPDATE + UPDATE next_retry_at = now()
+ visibility_timeout, COMMIT) → process → ack/fail в отдельных транзакциях.

### Backoff

Экспоненциальный с jitter: `base_backoff * 2^(attempts-1)`, ограниченный
`max_backoff`, ±25% случайности (избегаем синхронных retry-штормов, когда
много событий упало одновременно). По умолчанию: `1s, 2s, 4s, 8s, 16s →
cap 60s`. Параметры — в `outbox.*` секции конфига.

### At-least-once и идемпотентность

Доставка **at-least-once**: событие может прийти подписчику повторно (краш
relay между dispatch и commit; явный retry после ошибки). Это **обязывает**
обработчики быть идемпотентными.

Способы:
- Завести у получателя таблицу `processed_events(event_id PRIMARY KEY)` и
  игнорировать дубликаты в той же транзакции, что и применяет изменение.
- Дать самой операции естественную идемпотентность (например, перевод
  средств с `payment_id`-флагом в таблице).
- На крайний случай — `INSERT ... ON CONFLICT DO NOTHING` на бизнес-данных.

Если **один из нескольких** подписчиков упадёт, на ретрае будут вызваны
**все** подписчики этого топика — поэтому идемпотентность нужна не только
от падающего, но и от соседних.

### Поведение под нагрузкой

- Backlog растёт только при долгом offline-е consumer-а. Под нормальной
  нагрузкой таблица почти пустая — DELETE сразу освобождает место.
- Узкое место — auto-vacuum / dead tuples от DELETE. Если QPS большой,
  стоит увеличить `autovacuum_vacuum_scale_factor` для таблицы или
  `VACUUM` по расписанию.
- При очень высоких QPS partitioning по `created_at` + `TRUNCATE` старых
  партиций дешевле, чем построчный DELETE — но это уже оптимизация под
  конкретный профиль нагрузки, по умолчанию не нужна.
- `idx_outbox_created` обслуживает `ORDER BY created_at`; больше индексов
  не нужно, всё остальное — full scan по нескольким строкам.

## Распил на микросервисы

Шаблон спроектирован так, чтобы распил не требовал переписывания
**ничего внутри модуля** — меняются только реализации портов и адаптеры
messaging в композиционном корне.

### Что НЕ меняется

- `domain/`, `repository/`, `service/`, `transport/`, `module.go` — код
  модуля остаётся как есть.
- `<name>port/` — публичный контракт; становится контрактом сервиса.
- Сигнатуры `messaging.Publisher` / `messaging.Subscriber` —
  потребители их не различают.

### Что меняется

1. **Композиционный корень** — каждый модуль теперь живёт в своём процессе.
2. **Реализация порта** — `providerAdapter` (in-process вызов сервиса)
   заменяется на **gRPC-клиент**, реализующий тот же интерфейс
   `accountport.AccountProvider`. На стороне-владельце поднимается
   gRPC-сервер, обёрнутый вокруг сервиса.
3. **Реализация messaging** — `messaging.Engine` (outbox → in-proc bus)
   разделяется на две стороны:
   - **Producer**: Publisher остаётся outbox-based (важно для
     транзакционности). От outbox до брокера — два варианта:
     - Свой relay-процесс пишет из outbox в Kafka/NATS (тот же
       `dispatchBatch`, но `fanout` → broker.Produce).
     - **CDC (Debezium)** читает WAL Postgres и кладёт изменения outbox в
       Kafka. Плюс: outbox-таблица не требует чтения сервисом. Минус:
       отдельная инфраструктура.
   - **Consumer**: `messaging.Subscriber` реализуется как Kafka-consumer,
     обрабатывающий те же топики. Подписка `account` на
     `paymentport.TopicPaymentCreated` остаётся такой же.
4. **БД** — каждый сервис получает свою. Миграции уже разделены по модулям
   (`internal/modules/<name>/migrations/`), смена `goose_db_version`-таблицы
   на свою схему не требуется — она уже per-module.

### Контракты (общие артефакты)

После распила контракты — единственная общая зависимость:

- `<name>port/` уезжает в общий репо (или генерируется из `.proto`):
  - port-интерфейсы → `.proto` сервисы (`account.proto`),
  - топики и payload-структуры событий → схема событий (JSON Schema или
    `.proto`-сообщения для Kafka),
  - доменные ошибки порта (`accountport.ErrAccountNotFound`) — отдельные
    gRPC-коды.

### Пошаговый план

1. Зафиксировать контракты: для каждого port-пакета сгенерировать `.proto`,
   для топиков — отдельные `.proto`-сообщения. Сложить в общий контрактный
   репо.
2. По одному модулю за раз:
   - Поднять gRPC-сервер модуля, обёртывающий `service.Service`.
   - В монолите заменить `providerAdapter` на gRPC-клиент-реализацию того же
     интерфейса. Поведение модулей не меняется.
3. Поставить Kafka (или другой брокер). Включить CDC или собственный
   broker-relay для outbox.
4. Подменить `messaging.Subscriber` на Kafka-consumer в нужных модулях.
5. Вынести БД модуля в отдельный инстанс (миграции уже изолированы).
6. Вынести модуль из монолита в свой репо/деплой; в монолите удалить его
   код, оставив только gRPC-клиент + контрактный пакет.

На каждом шаге монолит остаётся работоспособным — это инкрементальная
миграция, а не big bang.

## Телеметрия и отказоустойчивость (план)

В шаблоне намеренно отсутствуют — добавляются по мере роста. Здесь зафиксирован
порядок внедрения, чтобы выборы (библиотеки, точки интеграции) не делались
дважды.

### Сейчас (в монолите)

**Телеметрия:**

- **OpenTelemetry SDK** с OTLP-экспортёром — единый источник metrics +
  traces, vendor-neutral. Не Prometheus-клиент отдельно: при распиле
  пришлось бы переделывать ради distributed-tracing.
- `trace_id` прокидывается middleware-ом в `zerolog`-контекст (`log.With().
  Str("trace_id", ...)`) и **в payload outbox-события** (отдельные поля
  рядом с `topic`/`payload`). После распила трасса бесшовно продолжится
  через брокер.
- HTTP-эндпоинты `/healthz` (always-200, для liveness) и `/readyz` (DB ping
  + проверка LISTEN-соединения relay, для readiness).
- Метрики: `http_request_duration` (histogram, labels: route/status),
  `outbox_backlog` (gauge, `SELECT count(*) FROM outbox`),
  `outbox_dead_total` (counter), длительность handler-ов подписчиков,
  длительность SQL-запросов, `panics_total`.

**Отказоустойчивость:**

- Context-таймауты на границе handler↔service (handler делает
  `c.UserContext()` + `context.WithTimeout`; сейчас отсутствует — handler
  держит дефолтный контекст Fiber-а).
- `statement_timeout` в DSN — серверная гарантия, что запрос не повиснет
  навсегда даже если клиент забыл таймаут.
- **Exponential backoff + jitter на reconnect relay** — сейчас фиксированные
  2 секунды (`reconnectDelay` в `engine.go`); при перезапуске Postgres все
  инстансы реконнектятся синхронно (thundering herd).
- **Bulkhead для подписчиков** — семафор/worker-pool на каждый топик, чтобы
  один медленный handler не сожрал все горутины и не задушил остальные
  топики.

### При переходе на микросервисы

**gRPC (реализация портов):**

- **Deadline propagation** — встроено в gRPC, нужно лишь убедиться что
  context с deadline доходит от HTTP-handler-а до gRPC-клиента.
- **Retry interceptor** с backoff+jitter — только для идемпотентных
  методов; для unsafe-вызовов клиент должен передавать idempotency-key, а
  сервер дедупить.
- **Circuit breaker** на client-side (`sony/gobreaker`) — чтобы при
  падении вызываемого сервиса не выгребать таймауты на каждый запрос.
  Открытый CB должен возвращать ошибку, маппящуюся в `accountport.
  ErrAccountNotFound`-стиль кодов.
- **Bulkhead** — отдельные клиентские пулы соединений / семафоры на разные
  сервисы, чтобы насыщение одного не блокировало вызовы к другим.

**Брокер (Kafka/NATS):**

- Consumer-group с **manual commit после успешной обработки** — иначе
  at-least-once не соблюдается (auto-commit подтверждает offset до того
  как handler закончил).
- **DLQ-топик** для poison messages — аналог `outbox_dead` в монолите.
- Метрика **consumer lag** + алерт; лаг растёт → consumer не успевает
  или зациклился.
- **Idempotent producer** (Kafka-конфиг) + дедуп на стороне consumer-а
  через таблицу `processed_events(event_id PRIMARY KEY)` — те же подходы,
  что и для outbox.

**Distributed tracing:**

- **OTel propagator** через HTTP/gRPC headers и Kafka headers
  (W3C TraceContext). Если `trace_id`/`span_id` уже лежат в outbox payload
  (см. план «сейчас»), то на producer-стороне дополнительно ничего не
  нужно — relay просто прокладывает их в Kafka headers.

## Запуск

```sh
make config    # создать config/config.yaml из шаблона (один раз)
make tidy      # скачать зависимости (нужен интернет)
make up        # поднять Postgres в docker
make run       # запустить приложение
```

Конфиг — `config/config.yaml` (в git не коммитится; шаблон —
`config/config.example.yaml`). Любой параметр переопределяется env-переменной
с префиксом `APP_`, например `APP_DB_DSN`, `APP_HTTP_PORT`. На проде секреты
(DSN, пароли) **обязаны** приходить из env / секрет-хранилища, а не из
`config.yaml`.

## API

```sh
# создать счёт
curl -X POST localhost:8080/api/v1/accounts \
  -d '{"owner_id":"<uuid>","currency":"USD"}'

# создать платёж (синхронно проверит счета, асинхронно переведёт средства)
curl -X POST localhost:8080/api/v1/payments \
  -d '{"from_account_id":"<uuid>","to_account_id":"<uuid>","amount":1000}'
```

## Как добавить модуль

1. Создать `internal/modules/<name>/` со слоями и `<name>port/`.
2. Реализовать `module.Module` в `<name>/module.go`.
3. Добавить одну строку в срез `modules` в `internal/app/app.go`.
4. Положить миграции в `internal/modules/<name>/migrations/` (схема `<name>`).

## Тесты

- Доменный слой — обычные unit-тесты (см. `account/domain/account_test.go`).
- Репозитории — интеграционные тесты на реальном Postgres через
  `testcontainers-go` (рекомендуемый подход, в шаблон не включён).
- Сервисы — с моками `domain.Repository` и `accountport.AccountProvider`.
- HTTP-handlers — с моком `service.Service` и `fiber.App.Test(...)`.
- `internal/shared/postgres` — sqlmock-тесты для `UnitOfWork`/`InTx`.

## Замечание по чистоте слоёв

`domain.Repository` принимает `postgres.Querier` — это сознательный
прагматичный компромисс: доменный слой получает зависимость на тип
`sqlx.ExtContext` ради поддержки Unit of Work. Если нужна абсолютная чистота
домена — вынесите интерфейс `Repository` в слой `service`.
