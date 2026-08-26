-- ---------------------------------------------------------------------------
-- Демонстрационные данные для локального запуска и E2E-сценария.
--
-- Токены заведений (в БД лежат только SHA-256 хэши):
--   pizza-avito → dev-partner-token
--   sushi-avito → sushi-partner-token
--
-- Токены намеренно предсказуемы: это демо-окружение, поднимаемое через
-- docker compose. В README это отмечено как осознанное упрощение MVP.
-- ---------------------------------------------------------------------------

BEGIN;

-- --- Заведения --------------------------------------------------------------
INSERT INTO restaurants (slug, name, status, provider_base_url, api_key_hash,
                         min_order_kopecks, delivery_fee_kopecks)
VALUES
    ('pizza-avito',
     'Пиццерия Авито',
     'online',
     'http://restaurant-sim:8081',
     '296e205c464e4d1a0ff6657b598a1a3f37b4e191ad3c5d3be599d68cb766504e',
     50000,
     15000),
    ('sushi-avito',
     'Суши Авито',
     'paused',
     'http://restaurant-sim:8081',
     '0477c240db77d444125a331cfa51fa7dcb02d54dce17dba40b41658493bb374a',
     80000,
     20000);

-- --- Стартовое меню пиццерии (версия 1) -------------------------------------
INSERT INTO menus (restaurant_id, version, status)
SELECT id, 1, 'published' FROM restaurants WHERE slug = 'pizza-avito';

INSERT INTO products (menu_id, product_key, category, name, description,
                      price_kopecks, available, stock_qty)
SELECT m.id, v.product_key, v.category, v.name, v.description,
       v.price_kopecks, v.available, v.stock_qty
FROM menus m
         JOIN restaurants r ON r.id = m.restaurant_id AND r.slug = 'pizza-avito'
         CROSS JOIN (VALUES
    ('pizza_margherita', 'Пицца',   'Пицца Маргарита',  'Томаты, моцарелла, свежий базилик',        59000, TRUE,  10),
    ('pizza_pepperoni',  'Пицца',   'Пицца Пепперони',  'Пепперони, моцарелла, томатный соус',      69000, TRUE,  5),
    ('pizza_four_cheese','Пицца',   'Пицца 4 сыра',     'Моцарелла, горгонзола, пармезан, чеддер',  75000, TRUE,  NULL),
    ('pasta_carbonara',  'Паста',   'Паста Карбонара',  'Спагетти, гуанчале, пармезан, желток',     49000, TRUE,  NULL),
    ('salad_caesar',     'Салаты',  'Цезарь с курицей', 'Романо, курица гриль, пармезан, гренки',   39000, TRUE,  3),
    ('drink_cola',       'Напитки', 'Кола 0,5 л',       'Охлаждённая',                              12000, TRUE,  NULL),
    ('dessert_tiramisu', 'Десерты', 'Тирамису',         'Классический, с маскарпоне',               29000, FALSE, 0)
) AS v(product_key, category, name, description, price_kopecks, available, stock_qty)
WHERE m.version = 1;

-- --- Стартовое меню суши (версия 1) -----------------------------------------
INSERT INTO menus (restaurant_id, version, status)
SELECT id, 1, 'published' FROM restaurants WHERE slug = 'sushi-avito';

INSERT INTO products (menu_id, product_key, category, name, description,
                      price_kopecks, available, stock_qty)
SELECT m.id, v.product_key, v.category, v.name, v.description,
       v.price_kopecks, v.available, v.stock_qty
FROM menus m
         JOIN restaurants r ON r.id = m.restaurant_id AND r.slug = 'sushi-avito'
         CROSS JOIN (VALUES
    ('roll_philadelphia', 'Роллы',  'Филадельфия',   'Лосось, сливочный сыр, огурец', 64000, TRUE, 8),
    ('roll_california',   'Роллы',  'Калифорния',    'Краб, авокадо, икра тобико',    54000, TRUE, 8),
    ('set_family',        'Сеты',   'Сет «Семейный»','40 кусочков, 4 вида роллов',   189000, TRUE, 2)
) AS v(product_key, category, name, description, price_kopecks, available, stock_qty)
WHERE m.version = 1;

COMMIT;
