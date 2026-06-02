# ADR-0006: Явный composition root, без `Module` интерфейса и DI-фреймворка

**Статус:** принято
**Дата:** 2026-02

## Контекст

Каждый модуль возвращает свой `api.Service`, нуждается в зависимостях (БД,
event subscriber, чужие `api.Service`'ы, publisher), регистрирует HTTP-роуты
и подписки. Где это всё соединяется?

Варианты:

1. **`Module` интерфейс + registry**: каждый модуль реализует
   `Name() / RegisterRoutes(r) / Migrations() / New(deps) error`. Цикл по
   массиву модулей в `main.go`. Было в первой версии шаблона.
2. **DI-фреймворк** (uber-fx, google/wire, dig). Графы зависимостей
   собираются автоматически.
3. **Явный wiring**: composition root руками вызывает `New` каждого модуля в
   правильном порядке, руками передаёт `api.Service` зависимым модулям.

## Решение

Вариант 3 — **явный wiring в `cmd/api/main.go`**. Никаких интерфейсов
`Module`, никаких DI-фреймворков.

```go
func run(ctx context.Context, cfg *config.Config, log zerolog.Logger) error {
    db, _ := postgres.Connect(...)
    rmq, _ := rabbitmq.Connect(...)
    accountSubscriber := consumers.New(rmq, "account", consumersCfg, log)
    account := accountmod.New(db, accountSubscriber, log)
    payment := paymentmod.New(db, log, account.API(), paymentPublisher)
    registerRoutes(server.API(), idemStorage, log, account.API(), payment.API())
    ...
}
```

Чтение сверху вниз = граф зависимостей. `account` строится первым, потому
что `payment.New` принимает `account.API()`.

## Почему не Module-интерфейс

Был. Убран в Phase 1. Причины:

- **Прячет ordering**. С интерфейсом `New(deps)` deps — это map / struct, в
  ней лежит уже собранный `account.API()`. Логика «account нужно построить
  раньше payment, потому что payment его потребляет» оказывается размазана:
  либо реализована вручную внутри registry, либо переложена на DI-фреймворк.
  При явном wiring это просто Go-код — порядок вызовов.
- **HTTP-регистрация чужеродна модулю**. `Module.RegisterRoutes(r)` означает,
  что модуль знает про fiber, про роутинг, про middleware. Это противоречит
  плану распила: после выноса модуль раздаёт только `api.Service`, без HTTP.
  Лучше HTTP жил в `cmd/api/handlers/` и зависел от `<name>api.Service` —
  при распиле handler не двигается.
- **Migrations внутри Module зависели от запуска приложения** (auto_migrate).
  Это плохо для production: миграции должны быть отдельной фазой деплоя
  (k8s Job per release), а не быть запущены при первом старте новой версии
  приложения. См. `cmd/migrate`.

## Почему не DI-фреймворк

- **wire (compile-time)** генерирует код, но требует своих директив и
  билд-шага. Для горстки зависимостей выигрыш отрицательный — больше
  ceremony, чем кода.
- **fx (runtime)** — реальный DI-граф с lifecycle. Имеет смысл когда
  модулей десятки, lifecycle-зависимости между ними сложные. У нас 2 модуля.
  Преждевременно.
- **Главное**: явный wiring **сам по себе документация**. Новый разработчик
  читает `run()` сверху вниз и видит весь граф приложения. С fx граф нужно
  собирать в голове, читая `fx.Provide`/`fx.Invoke`.

## Последствия

**+** `cmd/api/main.go` — единое место правды о приложении. Поиск «кто
зависит от чего» = чтение функции.

**+** Тесты композиции (если они нужны) — обычный Go-код, никаких
test-helper'ов фреймворка.

**+** Удаление модуля из приложения = удаление его блока из `run()` и
импорта. Никаких registry-побочек.

**−** Каждый новый модуль = +5-10 строк ручного wiring'а. Это явно, и это
читается. При росте до десятка модулей пересмотрим.

**−** Нет автоматической проверки циклов зависимостей — но Go компилятор её
делает за нас (cyclic import).

## Связанное

- **Cron-jobs**: `crons.Scheduler` инстанциируется тут же, модули могут
  `Register` своих джобов из их `New(...)`.
- **Feature flags / Secrets**: provider'ы создаются в composition root и
  передаются в модули как параметры — те же правила.
- **Outbox publisher**: `outbox.NewPublisher("payment")` — один на пишущий
  модуль, scoped к схеме.

## Ссылки

- [ADR-0001](0001-modular-monolith.md) — модули в принципе
- [ADR-0002](0002-provider-side-service-interface.md) — что возвращает `New`
