# ADR-0005: Consumer-side идемпотентность через `<schema>.processed_events`

**Статус:** принято
**Дата:** 2026-03

## Контекст

Доставка событий — **at-least-once** на двух независимых участках:

1. **Outbox → broker**: relay упал между `Dispatcher.Dispatch` и `COMMIT` →
   на старте увидит ту же строку и опубликует повторно.
2. **Broker → handler**: handler упал между обработкой и `ack` → broker
   доставит ещё раз другому consumer'у/после reconnect'а.

Значит handler **обязан** быть идемпотентным. Шаблон не давал ничего, кроме
комментария «handlers must be idempotent». Это **известный источник багов**:
на production самописная дедупликация в каждом handler'е выглядит по-разному
и часто протекает (race conditions, забыли учесть retry).

## Решение

**Платформенный helper + per-module dedup-таблица.**

Per-module миграция создаёт `<schema>.processed_events`:
```sql
CREATE TABLE account.processed_events (
    event_id     uuid PRIMARY KEY,
    topic        text NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now()
);
```

Producer-сторона уже даёт event_id: outbox `id` → `amqp.Publishing.MessageId`
→ `messaging.Event.ID`. Helper:

```go
type TxHandler func(ctx context.Context, q postgres.Querier, e messaging.Event) error

func Dedup(uow *postgres.UnitOfWork, schema string, h TxHandler) messaging.Handler
```

Логика per delivery, в одной транзакции:

1. `INSERT INTO <schema>.processed_events (event_id, topic) VALUES ($1, $2)
   ON CONFLICT DO NOTHING`
2. `RowsAffected == 0` → redelivery → commit пустой tx → ack, handler не
   вызывается.
3. `RowsAffected == 1` → handler с тем же tx; ошибка → rollback (и dedup
   mark, и бизнес-write) → message redelivered.

## Ключевые свойства

- **Exactly-once-effect относительно этой БД**. Бизнес-write коммитится
  тогда и только тогда, когда коммитится dedup-mark. Невозможно «эффект
  применён, mark не сохранился».
- **Не годится для не-DB эффектов**. HTTP-вызовы наружу, публикация в другую
  очередь — это не часть tx. Для таких handler'ов нужна своя idempotency
  (idempotency-key на стороне callee). В коде helper'а явно отражено: handler
  принимает `Querier`, не `context.Context` only.
- **`Event.ID == uuid.Nil` → `ErrNoEventID` → DLQ.** Никакого silent fallback:
  если event-id не propagate'нулся, лучше нагрузить DLQ для расследования,
  чем молча задвоить эффект.

## Per-module schema (не общая)

Dedup-таблица в схеме **consumer'а**, не producer'а. Причина: разные
consumer'ы одного события дедупятся независимо. Если consumer A обработал, а
B упал — на retry B должен запуститься, а A — пропустить. Общая таблица
сломала бы это.

## Альтернативы

**Application-level dedup map** (in-memory LRU). Не работает — состояние
теряется при рестарте, не распределённый.

**Брокер-level idempotent producer** (Kafka has it, Rabbit нет). Решает
только Producer→broker дублирование, не broker→consumer.

**`SET TRANSACTION ISOLATION SERIALIZABLE` + бизнес-инвариант** (полагаться
на «перевод средств с тем же payment_id не пройдёт, потому что balance уже
изменился»). Хрупко: handler должен знать инвариант и явно его проверять. Не
универсально (создание сущности — нет такого инварианта).

## Последствия

**+** Один паттерн на все handler'ы, проверяемый на этапе ревью: «handler
обёрнут в `consumers.Dedup`? Если нет — почему?».

**+** Метрика мониторинга через rowcount в `processed_events` и delta с
`consumer_messages_total{status="ack"}`.

**−** Таблица растёт без верхнего предела. GC: cron-job `DELETE WHERE
processed_at < now() - interval '30 days'`. Не реализован — нет реальной
нагрузки, формально нужен. Зафиксировано в README раздел «Operational
todos».

**−** Каждый INSERT в `processed_events` — extra round-trip к БД. Для
throughput'а в десятки тысяч RPS на consumer'е стоит подумать о batch dedup
(read-mostly cache + periodic flush). Сейчас не нужно.

## Ссылки

- [ADR-0003](0003-per-schema-outbox.md) — откуда event_id берётся
- [ADR-0004](0004-rabbitmq-topology.md) — где живёт DLQ
