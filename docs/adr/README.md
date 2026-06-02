# Architecture Decision Records

Решения, формирующие архитектуру проекта. Цель — зафиксировать **почему**
сделано так, а не **что** сделано (это видно из кода). Перед тем как
менять что-то из перечисленного — прочитайте соответствующий ADR; если
аргументы устарели, добавьте новый ADR со статусом «supersedes ADR-NNNN».

| # | Решение |
|---|---|
| [0001](0001-modular-monolith.md) | Модульный монолит с вертикальными слайсами, изоляция через Go `internal/` |
| [0002](0002-provider-side-service-interface.md) | `Service` — интерфейс provider-side, не consumer-side |
| [0003](0003-per-schema-outbox.md) | Transactional outbox per-schema, не общий |
| [0004](0004-rabbitmq-topology.md) | RabbitMQ: topic exchange + DLX + queue per (consumer, topic) |
| [0005](0005-consumer-side-idempotency.md) | Consumer-side dedup через `<schema>.processed_events` |
| [0006](0006-explicit-composition-root.md) | Явный composition root, без `Module` интерфейса и DI-фреймворка |

## Формат

Lightweight MADR: **Контекст** → **Решение** → **Альтернативы и почему
отказались** → **Последствия** → **Ссылки**. Не больше 1-2 экранов на ADR;
если нужна длинная справочная информация — в README.md или CLAUDE.md.
