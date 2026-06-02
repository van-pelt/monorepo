# ADR-0004: RabbitMQ топология: topic exchange + DLX + queue per (consumer, topic)

**Статус:** принято
**Дата:** 2026-02

## Контекст

Outbox-relay должен куда-то публиковать события, consumer'ы должны их
получать. Брокер выбран — RabbitMQ (см. §«почему не Kafka» ниже). Нужно
зафиксировать топологию exchange/queue/binding, поведение при handler-error,
и где живут poison messages.

## Решение

```
exchange:        events           (topic, durable)
DLX:             events.dlx       (topic, durable)
main queue:      <consumer>.<topic>      (durable, x-dead-letter-exchange=events.dlx)
main binding:    events ──topic──>  main queue
dlq queue:       <consumer>.<topic>.dlq  (durable)
dlq binding:     events.dlx ──topic──>  dlq queue
```

Ключевые свойства:

- **Один `events` exchange** для всего приложения, тип `topic`. Producer (=
  `rabbitmq.Publisher` от relay) шлёт с `routing key = event topic`.
- **Очередь на каждую пару `(consumer-name, topic)`**: `account.payment.created`,
  `account.user.deleted`. Несколько consumer'ов одного топика → каждый со
  своей очередью → fan-out + независимый retry/lag.
- **DLX `events.dlx`**: каждая main-queue настроена с
  `x-dead-letter-exchange=events.dlx`. На `nack(requeue=false)` сообщение
  ре-публикуется в DLX с тем же routing key и оседает в одноимённой
  `.dlq`-очереди.
- **`nack(requeue=false)` всегда, не `requeue=true`**: requeue загнал бы
  poison-сообщение в hot loop в одной очереди, заняв слот семафора.
- **`prefetch = subscription concurrency`**: см. [ADR](README.md#bulkhead)
  про bulkhead (в этом же файле ниже).

## Почему не Kafka

- **Операционная сложность**: ZooKeeper/KRaft + брокеры — отдельный
  кластер, требует SRE. RabbitMQ — один процесс с понятной memory-моделью.
- **Семантика очередей**: для нашего use case (job-queue с DLQ) Rabbit
  моделирует это нативно. В Kafka pattern «consumer-group + offset commit»
  требует дополнительной аккуратности с partitions и rebalances.
- **Throughput**: проект не upper-bound по throughput'у, где Kafka обходит
  Rabbit на порядки. При смене требований — обмен брокеров локализован в
  `platform/rabbitmq` + `platform/consumers`.

## Bulkhead per (consumer, topic)

Каждая подписка получает sized semaphore + AMQP prefetch на ту же величину:

- Один медленный handler топика «X» не съест все горутины — лимит N
  in-flight per (consumer, topic).
- Broker перестаёт push'ить когда bulkhead полный → backpressure на
  producer-уровне через `consumer_queue_depth`-метрику.
- Дефолт `consumers.default_concurrency = 4`, override per-topic через код в
  composition root: `consumers.Config{TopicConcurrency: map[string]int{...}}`.

## Reconnect и health

- `rabbitmq.Client.Channel()` — **lazy-dial**: проверяет cached connection,
  re-dial под mutex'ом если `IsClosed`. Caller'ы не управляют состоянием
  reconnect'а.
- Consumer-loop (`consumeOnce` → outer reconnect) пере-объявляет очереди при
  reconnect'е — declare идемпотентен.
- Reconnect-backoff: 1s base → 30s cap, ±25% jitter, чтобы N подов не
  ломились синхронно в только что поднявшийся брокер.
- `/readyz` отдаёт 503, пока connection лежит — k8s роутит трафик мимо.

## Последствия

**+** DLQ-сообщения видны как отдельные очереди в UI — operator копается
там же, где и в main queue.

**+** Bulkhead per (consumer, topic) — изоляция отказов на уровне
подписки, не на уровне процесса.

**+** Distributed tracing работает через AMQP headers (`traceparent` etc) —
producer injects, consumer extracts. См. CLAUDE.md «Tracing».

**−** RabbitMQ становится hard dependency для старта приложения (`Connect`
fast-fails). Это решено сознательно: приложение без брокера всё равно
бесполезно, лучше упасть на старте, чем работать с тихим failure outbox →
DLQ. Recovery — k8s рестартит pod, кикбэкоф предотвращает дребезг.

**−** Очередей становится много: `N consumers × M topics`. Для текущего
шаблона (1-2 consumer'а × горстка топиков) — некритично.

## Ссылки

- [ADR-0003](0003-per-schema-outbox.md) — producer-сторона
- [ADR-0005](0005-consumer-side-idempotency.md) — что делает handler
  получив сообщение
