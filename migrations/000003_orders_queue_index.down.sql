-- Откат: возвращаем прежний индекс и убираем новый.

BEGIN;

CREATE INDEX idx_orders_restaurant_status ON orders (restaurant_id, status);

DROP INDEX idx_orders_restaurant_recent;

COMMIT;
