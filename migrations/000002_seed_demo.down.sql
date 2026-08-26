-- Откат демо-данных. Заказы, созданные поверх этих заведений, удаляются
-- каскадом вместе с позициями и историей статусов; исходящие события чистим
-- отдельно, так как outbox не связан внешним ключом.

BEGIN;

DELETE FROM outbox_events
WHERE aggregate_type = 'order'
  AND aggregate_id IN (
      SELECT o.public_number::text
      FROM orders o
               JOIN restaurants r ON r.id = o.restaurant_id
      WHERE r.slug IN ('pizza-avito', 'sushi-avito')
  );

DELETE FROM orders
WHERE restaurant_id IN (
    SELECT id FROM restaurants WHERE slug IN ('pizza-avito', 'sushi-avito')
);

DELETE FROM restaurants WHERE slug IN ('pizza-avito', 'sushi-avito');

COMMIT;
