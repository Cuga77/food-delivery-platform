-- ---------------------------------------------------------------------------
-- Авито.Кухня: начальная схема.
--
-- Ключевые решения:
--   * все деньги — BIGINT в копейках, никаких NUMERIC/FLOAT;
--   * меню версионируется, заказы хранят снапшот позиций (см. ADR-0002);
--   * доставка событий заведению — через таблицу outbox (ADR-0003);
--   * инварианты продублированы CHECK-ограничениями: БД остаётся корректной,
--     даже если приложение ошибётся.
-- ---------------------------------------------------------------------------

BEGIN;

-- --- Заведения --------------------------------------------------------------
CREATE TABLE restaurants (
    id                   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    slug                 VARCHAR(64)  NOT NULL UNIQUE,
    name                 VARCHAR(255) NOT NULL,
    status               VARCHAR(32)  NOT NULL
                             CHECK (status IN ('online', 'paused', 'closed')),
    provider_base_url    VARCHAR(512) NOT NULL,
    api_key_hash         CHAR(64)     NOT NULL,
    min_order_kopecks    BIGINT       NOT NULL CHECK (min_order_kopecks >= 0),
    delivery_fee_kopecks BIGINT       NOT NULL CHECK (delivery_fee_kopecks >= 0),
    created_at           TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

COMMENT ON COLUMN restaurants.provider_base_url IS 'Базовый URL сервиса заведения для доставки вебхуков';
COMMENT ON COLUMN restaurants.api_key_hash IS 'SHA-256 партнёрского токена в hex; сам токен не хранится';

-- Аутентификация партнёра — поиск строго по хэшу токена.
CREATE UNIQUE INDEX idx_restaurants_api_key_hash ON restaurants (api_key_hash);

-- --- Меню -------------------------------------------------------------------
CREATE TABLE menus (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    restaurant_id BIGINT      NOT NULL REFERENCES restaurants (id) ON DELETE CASCADE,
    version       INT         NOT NULL CHECK (version > 0),
    status        VARCHAR(32) NOT NULL
                      CHECK (status IN ('draft', 'published', 'archived')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (restaurant_id, version)
);

-- У заведения не может быть двух опубликованных версий меню одновременно.
CREATE UNIQUE INDEX idx_menus_restaurant_active
    ON menus (restaurant_id)
    WHERE status = 'published';

-- --- Позиции меню -----------------------------------------------------------
CREATE TABLE products (
    id            BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    menu_id       BIGINT       NOT NULL REFERENCES menus (id) ON DELETE CASCADE,
    product_key   VARCHAR(64)  NOT NULL,
    category      VARCHAR(128) NOT NULL,
    name          VARCHAR(255) NOT NULL,
    description   TEXT         NOT NULL DEFAULT '',
    price_kopecks BIGINT       NOT NULL CHECK (price_kopecks >= 0),
    available     BOOLEAN      NOT NULL DEFAULT TRUE,
    stock_qty     INT          NULL CHECK (stock_qty IS NULL OR stock_qty >= 0),
    UNIQUE (menu_id, product_key)
);

COMMENT ON COLUMN products.stock_qty IS 'NULL означает неограниченный остаток';

CREATE INDEX idx_products_menu ON products (menu_id);

-- --- Заказы -----------------------------------------------------------------
CREATE TABLE orders (
    id                   BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    public_number        UUID         NOT NULL UNIQUE,
    user_external_id     VARCHAR(128) NOT NULL,
    restaurant_id        BIGINT       NOT NULL REFERENCES restaurants (id) ON DELETE RESTRICT,
    status               VARCHAR(32)  NOT NULL
                             CHECK (status IN ('NEW', 'ACCEPTED', 'REJECTED',
                                               'COOKING', 'READY', 'DELIVERED',
                                               'CANCELLED')),
    delivery_address     TEXT         NOT NULL,
    subtotal_kopecks     BIGINT       NOT NULL CHECK (subtotal_kopecks >= 0),
    delivery_fee_kopecks BIGINT       NOT NULL CHECK (delivery_fee_kopecks >= 0),
    total_kopecks        BIGINT       NOT NULL CHECK (total_kopecks >= 0),
    cancel_reason        TEXT         NULL,
    version              INT          NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at           TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    CONSTRAINT orders_total_matches_parts
        CHECK (total_kopecks = subtotal_kopecks + delivery_fee_kopecks)
);

COMMENT ON COLUMN orders.public_number IS 'UUIDv7: публичный идентификатор, сортируемый по времени';
COMMENT ON COLUMN orders.version IS 'Счётчик для оптимистичной блокировки';

CREATE INDEX idx_orders_user_created ON orders (user_external_id, created_at DESC);
CREATE INDEX idx_orders_restaurant_status ON orders (restaurant_id, status);
-- Reaper ищет «зависшие» заказы в статусе NEW.
CREATE INDEX idx_orders_stale_new ON orders (created_at) WHERE status = 'NEW';

-- --- Позиции заказа (снапшот) -----------------------------------------------
CREATE TABLE order_items (
    id                    BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id              BIGINT       NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    product_id            BIGINT       NULL REFERENCES products (id) ON DELETE SET NULL,
    product_key           VARCHAR(64)  NOT NULL,
    product_name_snapshot VARCHAR(255) NOT NULL,
    unit_price_kopecks    BIGINT       NOT NULL CHECK (unit_price_kopecks >= 0),
    qty                   INT          NOT NULL CHECK (qty > 0),
    line_total_kopecks    BIGINT       NOT NULL CHECK (line_total_kopecks >= 0),
    CONSTRAINT order_items_line_total_matches
        CHECK (line_total_kopecks = unit_price_kopecks * qty)
);

COMMENT ON TABLE order_items IS
    'Снапшот позиций: цена и название фиксируются на момент заказа и не меняются при обновлении меню';

CREATE INDEX idx_order_items_order ON order_items (order_id);
CREATE INDEX idx_order_items_product ON order_items (product_id);

-- --- История статусов -------------------------------------------------------
CREATE TABLE order_status_events (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    order_id    BIGINT      NOT NULL REFERENCES orders (id) ON DELETE CASCADE,
    from_status VARCHAR(32) NOT NULL,
    to_status   VARCHAR(32) NOT NULL,
    actor       VARCHAR(32) NOT NULL
                    CHECK (actor IN ('user', 'restaurant', 'system')),
    comment     TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON COLUMN order_status_events.from_status IS 'Пустая строка для первого события (создание заказа)';

CREATE INDEX idx_order_status_events_order ON order_status_events (order_id, id);

-- --- Идемпотентность --------------------------------------------------------
CREATE TABLE idempotency_keys (
    key             VARCHAR(128) PRIMARY KEY,
    request_hash    CHAR(64)     NOT NULL,
    response_status INT          NOT NULL DEFAULT 0 CHECK (response_status >= 0),
    response_body   JSONB        NOT NULL DEFAULT 'null'::jsonb,
    created_at      TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    expires_at      TIMESTAMPTZ  NOT NULL
);

COMMENT ON COLUMN idempotency_keys.response_status IS
    '0 — операция выполняется прямо сейчас; ответ ещё не зафиксирован';
COMMENT ON COLUMN idempotency_keys.request_hash IS 'SHA-256 канонизированного тела запроса';

-- Уборка протухших ключей фоновым reaper-ом.
CREATE INDEX idx_idempotency_expires ON idempotency_keys (expires_at);

-- --- Transactional outbox ---------------------------------------------------
CREATE TABLE outbox_events (
    id             BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    aggregate_type VARCHAR(64) NOT NULL,
    aggregate_id   VARCHAR(64) NOT NULL,
    event_type     VARCHAR(64) NOT NULL,
    payload        JSONB       NOT NULL,
    attempts       INT         NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_retry_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sent_at        TIMESTAMPTZ NULL,
    dead_at        TIMESTAMPTZ NULL,
    last_error     TEXT        NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE outbox_events IS
    'Событие пишется в одной транзакции с изменением агрегата; воркер доставляет его заведению';
COMMENT ON COLUMN outbox_events.sent_at IS 'Заполняется после успешной доставки';
COMMENT ON COLUMN outbox_events.dead_at IS
    'Событие снято с доставки после исчерпания попыток; строка остаётся для разбора инцидента';

-- Частичный индекс: воркер сканирует только события, ожидающие доставки.
CREATE INDEX idx_outbox_pending ON outbox_events (next_retry_at)
    WHERE sent_at IS NULL AND dead_at IS NULL;
CREATE INDEX idx_outbox_aggregate ON outbox_events (aggregate_type, aggregate_id);

COMMIT;
