-- Откат: возвращаем прежний индекс и убираем новый.

BEGIN;

CREATE INDEX idx_orders_user_created ON orders (user_external_id, created_at DESC);

DROP INDEX idx_orders_user_recent;

COMMIT;
