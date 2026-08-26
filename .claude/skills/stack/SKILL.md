---
name: stack
description: Поднять, погасить или проверить локальный стек Авито.Кухня (postgres, migrate, api, restaurant-sim), прогнать сквозной сценарий, посмотреть логи, сбросить БД. Использовать при запросах «запусти приложение», «подними стек», «проверь, что работает», «покажи логи», «сбрось базу», а также перед любой проверкой изменения в живом сервисе.
---

# Запуск стека Авито.Кухня

Стек поднимается только через Docker Compose. Бинарники руками не запускаются:
`api` бесполезен без применённых миграций, а `restaurant-sim` — без `api`.

## Поднять и проверить

```bash
make up          # postgres → migrate → api + restaurant-sim, с ожиданием готовности
make smoke       # сквозной сценарий: 12 шагов, ненулевой exit при расхождении
```

`make up` возвращает управление, когда контейнеры запущены, но **готовность
проверяется отдельно**. Перед любыми запросами дождитесь:

```bash
curl -sf http://localhost:8080/readyz   # api готов (БД доступна)
curl -sf http://localhost:8081/health   # restaurant-sim жив
```

Порты: `api` — 8080, `restaurant-sim` — 8081, `postgres` — 5432.

## Чистый прогон с нуля

Это единственный надёжный способ проверить, что стек поднимается «с нуля», —
и именно он воспроизводит поведение у проверяющего:

```bash
make e2e         # = down-v + up + smoke
```

`down-v` удаляет том с данными, поэтому миграции и seed применяются заново.

## Диагностика

```bash
make ps                              # статус и health контейнеров
make logs                            # хвост логов api и restaurant-sim
docker compose logs api | tail -50   # только api
docker compose logs migrate          # если api не стартует — смотреть сюда первым
```

Логи — JSON. Полезные поля: `request_id` (сквозной идентификатор запроса),
`worker` (`outbox` / `reaper`), `code` (доменный код ошибки).

Найти всё по одному запросу:

```bash
docker compose logs api | grep '<request_id из заголовка X-Request-Id>'
```

## Ручные запросы

Партнёрский токен демо-заведения: `dev-partner-token` (slug `pizza-avito`).

```bash
# Каталог
curl -s localhost:8080/api/v1/restaurants | jq

# Меню
curl -s localhost:8080/api/v1/restaurants/pizza-avito/menu | jq

# Заказ (Idempotency-Key обязателен)
curl -s -X POST localhost:8080/api/v1/orders \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"user_external_id":"usr_1","restaurant_id":1,
       "delivery_address":"ул. Ленина, 10",
       "items":[{"product_key":"pizza_margherita","qty":2}]}' | jq

# Очередь заказов заведения
curl -s localhost:8080/api/v1/partner/orders \
  -H 'X-Partner-Token: dev-partner-token' | jq
```

## Заглянуть в БД

```bash
docker compose exec postgres psql -U kitchen -d kitchen

# Частые вопросы к базе:
#   что не доставлено заведению
SELECT id, event_type, attempts, next_retry_at, last_error
FROM outbox_events WHERE sent_at IS NULL AND dead_at IS NULL;

#   история конкретного заказа
SELECT from_status, to_status, actor, created_at
FROM order_status_events e
JOIN orders o ON o.id = e.order_id
WHERE o.public_number = '<uuid>' ORDER BY e.id;
```

## Проверка сценариев заведения

Режимы `restaurant-sim` переключаются переменными окружения в
`docker-compose.yml`; после правки нужен перезапуск контейнера:

| Переменная | Что проверяет |
|---|---|
| `SIM_AUTO_ACCEPT=false` | заказ остаётся в `NEW`, срабатывает reaper по таймауту |
| `SIM_REJECT_ALL=true` | отказ заведения и возврат остатков |
| `SIM_COOKING_TIME_MS` | скорость конвейера (для ускорения smoke) |

```bash
docker compose up -d --force-recreate restaurant-sim
```

## Когда стек не поднимается

1. `docker compose logs migrate` — миграции падают чаще всего.
2. `docker compose ps` — если `api` в `unhealthy`, смотреть `logs api`: обычно
   это невалидный `DATABASE_URL` или ошибка валидации конфигурации на старте
   (сервис намеренно не стартует с неполной конфигурацией).
3. Порт занят — поменять в `.env` (`API_PORT`, `SIM_PORT`, `POSTGRES_PORT`).
