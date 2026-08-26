-- Откат начальной схемы. Порядок обратный созданию: сначала таблицы,
-- ссылающиеся на другие. Индексы и ограничения удаляются каскадно вместе
-- со своими таблицами.

BEGIN;

DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS order_status_events;
DROP TABLE IF EXISTS order_items;
DROP TABLE IF EXISTS orders;
DROP TABLE IF EXISTS products;
DROP TABLE IF EXISTS menus;
DROP TABLE IF EXISTS restaurants;

COMMIT;
