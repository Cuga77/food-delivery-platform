# Промпт для реализации сервиса · Авито.Кухня MVP (Полная спецификация)

Ты — senior Go-разработчик, проектирующий и реализующий MVP сервиса «Авито.Кухня» в строгом соответствии с требованиями задания[cite: 1]. Решение должно демонстрировать промышленный уровень проектирования: чистые архитектурные границы, строгие гарантии консистентности, детально проработанные краевые сценарии бизнес-логики и прозрачную документацию инженерных решений[cite: 1].

---

## 1. Стек технологий и окружение

1. Язык: Go 1.25 (строго единый `go.mod` в корне репозитория)[cite: 1].
2. База данных: PostgreSQL 17 + официальный драйвер `github.com/jackc/pgx/v5` с использованием `pgxpool.Pool`[cite: 1].
3. Контракт API: Contract-first на базе OpenAPI 3.0 (`api/openapi.yaml`)[cite: 1] с генерацией типизированного `strict-server` и моделей через `github.com/oapi-codegen/oapi-codegen/v2`[cite: 1].
4. HTTP роутер: `github.com/go-chi/chi/v5` со стандартным набором middleware (RequestID, RealIP, Slog-Logger, Recoverer, CORS, Timeout, Idempotency, PartnerAuth).
5. Миграции: `golang-migrate` (`github.com/golang-migrate/migrate/v4`) с обязательным наличием парных файлов `NNNN_name.up.sql` и `NNNN_name.down.sql`[cite: 1].
6. Контейнеризация: единый манифест `docker-compose.yml` (контейнеры `postgres`, `migrate`, `api`, `restaurant-sim`)[cite: 1].
7. Статический анализ: `golangci-lint` с жестким набором линтеров (конфигурационный файл `.golangci.yml` в корне)[cite: 1].
8. Тестирование: table-driven модульные тесты для бизнес-правил, интеграционные тесты с реальным PostgreSQL через транзакционную изоляцию/очистку, сквозной сценарий проверки через Bash-скрипт `scripts/smoke.sh`.

---

## 2. Структура проекта (Clean Architecture / Ports & Adapters)

```text
avito-kitchen/
├── .github/workflows/ci.yml    # CI: lint, test -race, contract diff, docker-compose build
├── .golangci.yml               # Конфигурация статического анализатора
├── .env.example                # Шаблон переменных окружения
├── Makefile                    # Сценарии: up, down, test, lint, gen, smoke, migrate
├── docker-compose.yml          # Оркестрация локального окружения
├── go.mod
├── go.sum
├── api/
│   ├── openapi.yaml            # Единый контракт B2C и B2B API
│   └── codegen.yaml            # Конфиг для генератора oapi-codegen
├── cmd/
│   ├── api/
│   │   └── main.go             # Сборка DI контейнера, запуск сервера и outbox-воркера
│   └── restaurant-sim/
│       └── main.go             # Сервис-эмулятор кухни партнёра
├── internal/
│   ├── config/                 # Парсинг и валидация конфигурации из ENV
│   ├── domain/                 # Доменные сущности, ошибки, State Machine заказа
│   │   ├── restaurant.go
│   │   ├── menu.go
│   │   ├── order.go
│   │   ├── state_machine.go
│   │   └── errors.go
│   ├── app/                    # Use cases (оркестрация сценариев, порты репозиториев)
│   │   ├── catalog_service.go
│   │   ├── order_service.go
│   │   ├── partner_service.go
│   │   ├── outbox_worker.go
│   │   └── ports.go
│   ├── adapters/
│   │   ├── postgres/           # Реализация репозиториев на pgx/v5
│   │   │   ├── pool.go
│   │   │   ├── tx_manager.go
│   │   │   ├── restaurant_repo.go
│   │   │   ├── menu_repo.go
│   │   │   ├── order_repo.go
│   │   │   ├── idempotency_repo.go
│   │   │   └── outbox_repo.go
│   │   ├── http/               # HTTP-контроллеры, адаптеры oapi-codegen strict-server
│   │   │   ├── client_handler.go
│   │   │   ├── partner_handler.go
│   │   │   ├── mappers.go
│   │   │   └── errors.go
│   │   └── simclient/          # HTTP-клиент для отправки вебхуков в restaurant-sim
│   │       └── client.go
│   ├── platform/
│   │   ├── httpserver/         # Конфигурация и запуск chi.Mux
│   │   ├── middleware/         # RequestID, Logger, Idempotency, PartnerAuth
│   │   └── logger/             # Инициализация slog в JSON-формате
│   └── gen/                    # Сгенерированные файлы oapi-codegen (типы, интерфейсы)
├── migrations/
│   ├── 000001_init_schema.up.sql
│   ├── 000001_init_schema.down.sql
│   ├── 000002_seed_demo.up.sql
│   └── 000002_seed_demo.down.sql
├── docs/
│   ├── diagrams/               # Диаграммы PlantUML (*.puml и сгенерированные *.svg)
│   │   ├── cjm_user.puml
│   │   ├── cjm_restaurant.puml
│   │   ├── order_state_machine.puml
│   │   ├── c4_container.puml
│   │   └── erd.puml
│   └── adr/                    # Архитектурные решения
│       ├── 0001_postgresql_as_single_source_of_truth.md
│       ├── 0002_menu_versioning_and_order_snapshots.md
│       ├── 0003_transactional_outbox_for_events.md
│       └── 0004_partner_api_key_auth.md
├── scripts/
│   └── smoke.sh                # Автоматический E2E smoke-прогон
└── README.md                   # Полная проектная документация

```

---

## 3. Детальная схема базы данных и гарантии консистентности

Все схемы реализуются в `migrations/000001_init_schema.up.sql`.

### Таблицы

1. **`restaurants`**:
* `id` (BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY)
* `slug` (VARCHAR(64) NOT NULL UNIQUE)
* `name` (VARCHAR(255) NOT NULL)
* `status` (VARCHAR(32) NOT NULL CHECK (status IN ('online', 'paused', 'closed')))
* `provider_base_url` (VARCHAR(512) NOT NULL) — базовый URL сервиса заведения
* `api_key_hash` (CHAR(64) NOT NULL) — SHA-256 хэш партнерского токена
* `min_order_kopecks` (BIGINT NOT NULL CHECK (min_order_kopecks >= 0))
* `delivery_fee_kopecks` (BIGINT NOT NULL CHECK (delivery_fee_kopecks >= 0))
* `created_at` (TIMESTAMPTZ NOT NULL DEFAULT NOW())
* `updated_at` (TIMESTAMPTZ NOT NULL DEFAULT NOW())


2. **`menus`**:
* `id` (BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY)
* `restaurant_id` (BIGINT NOT NULL REFERENCES restaurants(id) ON DELETE CASCADE)
* `version` (INT NOT NULL)
* `status` (VARCHAR(32) NOT NULL CHECK (status IN ('draft', 'published', 'archived')))
* `created_at` (TIMESTAMPTZ NOT NULL DEFAULT NOW())
* `UNIQUE(restaurant_id, version)`


3. **`products`**:
* `id` (BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY)
* `menu_id` (BIGINT NOT NULL REFERENCES menus(id) ON DELETE CASCADE)
* `product_key` (VARCHAR(64) NOT NULL) — артикул/идентификатор в системе заведения
* `category` (VARCHAR(128) NOT NULL) — категория блюда
* `name` (VARCHAR(255) NOT NULL)
* `description` (TEXT NOT NULL DEFAULT '')
* `price_kopecks` (BIGINT NOT NULL CHECK (price_kopecks >= 0))
* `available` (BOOLEAN NOT NULL DEFAULT TRUE)
* `stock_qty` (INT NULL CHECK (stock_qty IS NULL OR stock_qty >= 0)) — NULL означает бесконечный остаток
* `UNIQUE(menu_id, product_key)`


4. **`orders`**:
* `id` (BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY)
* `public_number` (UUID NOT NULL UNIQUE) — публичный UUIDv7 заказа для выдачи клиенту
* `user_external_id` (VARCHAR(128) NOT NULL) — внешний ID пользователя в экосистеме Авито
* `restaurant_id` (BIGINT NOT NULL REFERENCES restaurants(id) ON DELETE RESTRICT)
* `status` (VARCHAR(32) NOT NULL)
* `delivery_address` (TEXT NOT NULL)
* `subtotal_kopecks` (BIGINT NOT NULL CHECK (subtotal_kopecks >= 0))
* `delivery_fee_kopecks` (BIGINT NOT NULL CHECK (delivery_fee_kopecks >= 0))
* `total_kopecks` (BIGINT NOT NULL CHECK (total_kopecks >= 0))
* `cancel_reason` (TEXT NULL)
* `version` (INT NOT NULL DEFAULT 1) — для оптимистичной блокировки
* `created_at` (TIMESTAMPTZ NOT NULL DEFAULT NOW())
* `updated_at` (TIMESTAMPTZ NOT NULL DEFAULT NOW())


5. **`order_items`**:
* `id` (BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY)
* `order_id` (BIGINT NOT NULL REFERENCES orders(id) ON DELETE CASCADE)
* `product_id` (BIGINT NULL REFERENCES products(id) ON DELETE SET NULL)
* `product_key` (VARCHAR(64) NOT NULL)
* `product_name_snapshot` (VARCHAR(255) NOT NULL)
* `unit_price_kopecks` (BIGINT NOT NULL CHECK (unit_price_kopecks >= 0))
* `qty` (INT NOT NULL CHECK (qty > 0))
* `line_total_kopecks` (BIGINT NOT NULL CHECK (line_total_kopecks >= 0))


6. **`order_status_events`**:
* `id` (BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY)
* `order_id` (BIGINT NOT NULL REFERENCES orders(id) ON DELETE CASCADE)
* `from_status` (VARCHAR(32) NOT NULL)
* `to_status` (VARCHAR(32) NOT NULL)
* `actor` (VARCHAR(32) NOT NULL CHECK (actor IN ('user', 'restaurant', 'system')))
* `comment` (TEXT NOT NULL DEFAULT '')
* `created_at` (TIMESTAMPTZ NOT NULL DEFAULT NOW())


7. **`idempotency_keys`**:
* `key` (VARCHAR(128) PRIMARY KEY)
* `request_hash` (CHAR(64) NOT NULL) — SHA-256 от тела запроса
* `response_status` (INT NOT NULL)
* `response_body` (JSONB NOT NULL)
* `created_at` (TIMESTAMPTZ NOT NULL DEFAULT NOW())
* `expires_at` (TIMESTAMPTZ NOT NULL)


8. **`outbox_events`**:
* `id` (BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY)
* `aggregate_type` (VARCHAR(64) NOT NULL)
* `aggregate_id` (VARCHAR(64) NOT NULL)
* `event_type` (VARCHAR(64) NOT NULL)
* `payload` (JSONB NOT NULL)
* `attempts` (INT NOT NULL DEFAULT 0)
* `next_retry_at` (TIMESTAMPTZ NOT NULL DEFAULT NOW())
* `sent_at` (TIMESTAMPTZ NULL)
* `created_at` (TIMESTAMPTZ NOT NULL DEFAULT NOW())



### Обязательные индексы

* `CREATE INDEX idx_orders_user_created ON orders (user_external_id, created_at DESC);`
* `CREATE INDEX idx_orders_restaurant_status ON orders (restaurant_id, status);`
* `CREATE INDEX idx_outbox_pending ON outbox_events (next_retry_at) WHERE sent_at IS NULL;`
* `CREATE INDEX idx_products_menu ON products (menu_id);`
* `CREATE INDEX idx_menus_restaurant_active ON menus (restaurant_id) WHERE status = 'published';`

---

## 4. Консистентность и транзакционные сценарии

### 1. Атомарное создание заказа

Создание заказа выполняется в единой транзакции PostgreSQL:

1. Проверка статуса заведения (`status == 'online'`). Если `paused`/`closed` — откат с ошибкой `RESTAURANT_UNAVAILABLE`.
2. Загрузка активного опубликованного меню заведения (`status == 'published'`).
3. Валидация позиций: существование в опубликованном меню, активность (`available == true`). При несовпадении — откат с `PRODUCT_UNAVAILABLE`.
4. Атомарное списание остатков для товаров с конечным запасом (`stock_qty IS NOT NULL`):
```sql
UPDATE products
SET stock_qty = stock_qty - $1
WHERE id = $2 AND available = TRUE AND (stock_qty IS NULL OR stock_qty >= $1)
RETURNING stock_qty;

```


Если запрос возвращает 0 затронутых строк — откат с ошибкой `OUT_OF_STOCK`.
5. Расчет `subtotal_kopecks` на основе актуальных цен в БД (клиентским ценам сервис не доверяет).
6. Проверка условия `subtotal_kopecks >= min_order_kopecks`. При нарушении — откат с `MIN_ORDER_NOT_MET`.
7. Вставка записи в `orders` со статусом `NEW` и `version = 1`.
8. Массовая вставка снапшотов позиций в `order_items`.
9. Фиксация события в `order_status_events` (`from_status = ''`, `to_status = 'NEW'`, `actor = 'user'`).
10. Запись исходящего события в `outbox_events` (`aggregate_type = 'order'`, `event_type = 'order_created'`).

### 2. State Machine и оптимистичные блокировки

Допустимые переходы статусов:

* `NEW` -> `ACCEPTED` (actor: `restaurant`)
* `NEW` -> `REJECTED` (actor: `restaurant`, освобождение списанных остатков)
* `NEW` -> `CANCELLED` (actor: `user` или `system` по таймауту, освобождение остатков)
* `ACCEPTED` -> `COOKING` (actor: `restaurant`)
* `COOKING` -> `READY` (actor: `restaurant`)
* `READY` -> `DELIVERED` (actor: `restaurant` / `courier_mock`)
* `ACCEPTED` / `COOKING` -> `CANCELLED` разрешено только со стороны ресторана.

Каждое изменение статуса проверяется через `WHERE id = $id AND version = $current_version` с инкрементом `version = version + 1`. При конфликте версий возвращается ошибка `STATE_CONFLICT` (HTTP 409).

### 3. Гарантии доставки (Transactional Outbox)

Фоновый воркер `outbox_worker.go`:

* Выбирает пачку неотправленных событий: `SELECT ... FROM outbox_events WHERE sent_at IS NULL AND next_retry_at <= NOW() ORDER BY id ASC LIMIT 50 FOR UPDATE SKIP LOCKED`.
* Отправляет HTTP POST запрос на `provider_base_url/kitchen/events` заведения.
* При успешном ответе (HTTP 200/204) проставляет `sent_at = NOW()`.
* При сетевой ошибке или HTTP 5xx увеличивает `attempts = attempts + 1` и выставляет `next_retry_at = NOW() + backoff` (экспоненциальный рост: 1s, 2s, 4s, 8s...).

---

## 5. Спецификация API (Contract-First OpenAPI 3.0)

Файл `api/openapi.yaml` содержит полную спецификацию с разделением по тегам:

### 1. Client Endpoints (B2C)

* `GET /api/v1/restaurants`: получение каталога заведений.
* Query: `limit` (default 20), `cursor` (string), `status` (optional).
* Response 200: массив заведений, пагинация `next_cursor`.


* `GET /api/v1/restaurants/{slug}/menu`: просмотр витрины меню.
* Response 200: метаданные заведения, меню (версия, сгруппированные по категориям продукты с признаками наличия и остатков).
* Response 404: заведение не найдено (`RESTAURANT_NOT_FOUND`).


* `POST /api/v1/orders`: оформление заказа.
* Headers: `Idempotency-Key: <UUID>` (обязателен).
* Body: `{ "user_external_id": "usr_123", "restaurant_id": 1, "delivery_address": "ул. Ленина, 10", "items": [{"product_key": "pizza_margherita", "qty": 2}] }`.
* Response 201: карточка созданного заказа (`public_number`, статус `NEW`, итоговая сумма).
* Response 409: `OUT_OF_STOCK`, `PRODUCT_UNAVAILABLE`.
* Response 422: `MIN_ORDER_NOT_MET`, `RESTAURANT_UNAVAILABLE`, `IDEMPOTENCY_PAYLOAD_MISMATCH`.


* `GET /api/v1/orders/{public_number}`: получение карточки заказа и истории статусов.
* Response 200: информация о заказе, текущий статус, список позиций, снапшот цен, таймлайн событий (`order_status_events`).
* Response 404: заказ не найден (`ORDER_NOT_FOUND`).


* `POST /api/v1/orders/{public_number}/cancel`: отмена заказа пользователем.
* Body: `{ "reason": "Передумал" }`.
* Response 200: обновленный заказ в статусе `CANCELLED`.
* Response 409: `ORDER_CANNOT_BE_CANCELLED` (заказ уже перешел в статус `ACCEPTED` или выше).



### 2. Partner Endpoints (B2B)

* Аутентификация: заголовок `X-Partner-Token: <token>`.
* `POST /api/v1/partner/menu/sync`: полная синхронизация меню заведения.
* Body: `{ "products": [{ "product_key": "p1", "category": "Пицца", "name": "Пепперони", "description": "...", "price_kopecks": 59000, "available": true, "stock_qty": 10 }] }`.
* Поведение: создается меню с версией $N+1$, заполняются `products`, статус старого меню меняется на `archived`, нового — на `published`.
* Response 200: `{ "version": 2, "items_synced": 15 }`.


* `GET /api/v1/partner/orders`: получение очереди заказов заведения.
* Query: `status` (optional), `limit` (default 50).
* Response 200: список заказов.


* `POST /api/v1/partner/orders/{public_number}/status`: перевод статуса заказа.
* Body: `{ "status": "COOKING", "comment": "Начали готовить" }`.
* Response 200: обновленный статус заказа.
* Response 409: `STATE_CONFLICT` (недопустимый переход статуса).


* `POST /api/v1/partner/kitchen-status`: переключение режима работы кухни.
* Body: `{ "status": "paused" }`.
* Response 200: `{ "status": "paused" }`.



### 3. Формат ошибок (RFC 7807)

Content-Type: `application/problem+json`

```json
{
  "type": "[https://avito.ru/kitchen/errors/OUT_OF_STOCK](https://avito.ru/kitchen/errors/OUT_OF_STOCK)",
  "title": "Conflict",
  "status": 409,
  "code": "OUT_OF_STOCK",
  "detail": "Товар 'Пицца Маргарита' (ключ: pizza_margherita) закончился на складе",
  "instance": "/api/v1/orders",
  "request_id": "c1f7a8e2-8921-4f3b-87b4-192a6c1a8e10"
}

```

---

## 6. Сервис заведения (`cmd/restaurant-sim`)

Реализуется отдельным бинарником в рамках общего `go.mod`:

1. Предоставляет собственный HTTP-сервер:
* `GET /kitchen/menu`: возвращает эталонное JSON-меню ресторана.
* `POST /kitchen/events`: вебхук от `api`. Принимает события `order_created`.
* Проверяет заголовок `X-Event-ID` для дедупликации (хранит последние 1000 ID в LRU/in-memory sync.Map).
* Возвращает HTTP 200 OK.




2. Фоновый цикл обработки принятых заказов:
* Если переменная окружения `SIM_AUTO_ACCEPT=true`:
* Через `SIM_ACCEPT_DELAY_MS` (например, 1000 мс) дергает B2B API платформы `POST /api/v1/partner/orders/{number}/status` со статусом `ACCEPTED`.
* Через `SIM_COOKING_TIME_MS` (например, 3000 мс) дергает перевод в `COOKING`, затем в `READY`.




3. Поддерживает аварийный режим: если `SIM_REJECT_ALL=true`, автоматически отклоняет заказ вызовом со статусом `REJECTED`.

---

## 7. Диаграммы и документация (Code-Generated PlantUML)

В папке `docs/diagrams/` формируются исходники `.puml`:

1. `cjm_user.puml`: Sequence/Activity диаграмма CJM клиента (Выбор заведения -> Просмотр меню -> Формирование заказа -> Ошибка OUT_OF_STOCK -> Корректировка корзины -> Успешное создание -> Отслеживание статуса -> Попытка поздней отмены с ошибкой 409).


2. `cjm_restaurant.puml`: Диаграмма процесса заведения (Синхронизация меню -> Открытие кухни -> Получение вебхука о новом заказе -> Авто-приемка -> Этап готовки -> Смена статуса на READY).


3. `order_state_machine.puml`: Полный граф переходов жизненного цикла заказа с указанием допустимых ролей (User, Restaurant, System).
4. `c4_container.puml`: C4 Level 2 диаграмма архитектуры контейнеров (Web Client, API Monolith, PostgreSQL, Restaurant Simulator, Outbox Worker).


5. `erd.puml`: ER-диаграмма связей таблиц базы данных с указанием ключей, ограничений и индексов.

В папке `docs/adr/` формируются записи:

* `0001_postgresql_as_single_source_of_truth.md` (Отказ от Redis в пользу транзакций PostgreSQL 17 для обеспечения бескомпромиссной атомарности списаний и резервов в MVP).
* `0002_menu_versioning_and_order_snapshots.md` (Обоснование версионирования меню $N+1$ и фиксации цен в `order_items` для изоляции исторических заказов от изменений в каталоге).
* `0003_transactional_outbox_for_events.md` (Выбор паттерна Outbox на базе таблицы PG и `SKIP LOCKED` вместо прямой отправки вебхуков из хендлера для исключения потери событий).
* `0004_partner_api_key_auth.md` (Реализация аутентификации заведений по статическому токену с хранением SHA-256 хэша в БД).

---

## 8. E2E Сценарий (`scripts/smoke.sh`)

Скрипт на Bash с использованием `curl` и `jq`, автоматизирующий сквозную валидацию:

1. Ожидание готовности сервисов (`GET /health` на `8080` и `8081`).
2. Запрос списка заведений (`GET /api/v1/restaurants`).
3. Синхронизация меню через Partner API (`POST /api/v1/partner/menu/sync`).
4. Попытка создания заказа с превышением доступного остатка -> Проверка ответа HTTP 409 `OUT_OF_STOCK`.
5. Попытка создания заказа ниже минимальной суммы -> Проверка ответа HTTP 422 `MIN_ORDER_NOT_MET`.
6. Успешное создание заказа с валидным телом и `Idempotency-Key` -> Проверка статуса HTTP 201.
7. Повторная отправка того же запроса с тем же `Idempotency-Key` -> Проверка идемпотентного ответа 201 с совпадающим `public_number`.
8. Повторная отправка с тем же `Idempotency-Key`, но измененным телом -> Проверка ошибки HTTP 422 `IDEMPOTENCY_PAYLOAD_MISMATCH`.
9. Ожидание доставки события в `restaurant-sim` и автоматического перевода статуса в `ACCEPTED` -> `COOKING` -> `READY`.
10. Запрос карточки заказа (`GET /api/v1/orders/{public_number}`) -> Валидация таймлайна из 4 событий в `order_status_events`.
11. Попытка отмены клиентом заказа в статусе `READY` -> Проверка ошибки HTTP 409 `ORDER_CANNOT_BE_CANCELLED`.
12. Переключение кухни заведения в режим `closed` (`POST /api/v1/partner/kitchen-status`) -> Попытка создания нового заказа -> Проверка ошибки HTTP 422 `RESTAURANT_UNAVAILABLE`.

---

## 9. Пошаговый план генерации кодовой базы

Реализация выполняется строго последовательно, без пропусков и заглушек:

* **Этап 1: Инфраструктура и Контракты**
* Создание `go.mod`, `.golangci.yml`, `Makefile`, `docker-compose.yml`.


* Написание спецификации `api/openapi.yaml` и конфигурации кодогенерации `api/codegen.yaml`.


* Написание файлов миграций `migrations/000001_init_schema.*` и `migrations/000002_seed_demo.*`.




* **Этап 2: Доменный слой и Ядро (`internal/domain`, `internal/app`)**
* Реализация структур сущностей, правил State Machine и доменных ошибок.
* Объявление интерфейсов репозиториев (портов) и реализация сервисов Use Cases.


* **Этап 3: Адаптеры базы данных (`internal/adapters/postgres`)**
* Инициализация пула `pgxpool`, реализация менеджера транзакций.
* Написание SQL-запросов и методов репозиториев с обработкой гонок и атомарными операциями.


* **Этап 4: Транспортный слой и Сервисы (`internal/adapters/http`, `cmd/`)**
* Реализация интерфейсов strict-сервера oapi-codegen для Client API и Partner API.
* Написание middleware (включая проверку идемпотентности и авторизацию партнеров).
* Реализация Outbox Worker с периодическим опросом БД и механизмом backoff.
* Реализация сервиса `cmd/restaurant-sim`.


* Сборка точки входа `cmd/api/main.go` с graceful shutdown.


* **Этап 5: Документация, Диаграммы и Тесты**
* Написание PlantUML-диаграмм в `docs/diagrams/`.


* Оформление архитектурных записей ADR в `docs/adr/`.
* Создание скрипта `scripts/smoke.sh` и модульных тестов.
* Написание итогового `README.md` с описанием архитектуры C4, схемы БД, упрощений и инструкций.
