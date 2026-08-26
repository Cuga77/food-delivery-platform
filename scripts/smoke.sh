#!/usr/bin/env bash
#
# Сквозной E2E-прогон Авито.Кухня по поднятому стеку.
#
#   make up && make smoke
#
# Скрипт проверяет не «отвечает ли сервис», а бизнес-сценарии целиком: что
# заказ нельзя оформить сверх остатка, что повтор с тем же Idempotency-Key не
# создаёт второй заказ, что событие доезжает до заведения и оно само ведёт
# заказ по конвейеру. Любое расхождение — ненулевой код возврата.

set -euo pipefail

API="${API_BASE_URL:-http://localhost:8080}"
SIM="${SIM_BASE_URL:-http://localhost:8081}"
PARTNER_TOKEN="${SIM_PARTNER_TOKEN:-dev-partner-token}"
RESTAURANT_SLUG="${SIM_RESTAURANT_SLUG:-pizza-avito}"

# Сколько ждать готовности сервисов и асинхронных переходов статуса.
STARTUP_TIMEOUT="${SMOKE_STARTUP_TIMEOUT:-60}"
PIPELINE_TIMEOUT="${SMOKE_PIPELINE_TIMEOUT:-30}"

# ---------------------------------------------------------------------------
# Вывод
# ---------------------------------------------------------------------------

if [[ -t 1 ]]; then
  RED=$'\033[31m'; GREEN=$'\033[32m'; YELLOW=$'\033[33m'; BOLD=$'\033[1m'; RESET=$'\033[0m'
else
  RED=''; GREEN=''; YELLOW=''; BOLD=''; RESET=''
fi

STEP=0
FAILURES=0

step()  { STEP=$((STEP + 1)); printf '\n%s[%02d] %s%s\n' "$BOLD" "$STEP" "$1" "$RESET"; }
ok()    { printf '     %s✓%s %s\n' "$GREEN" "$RESET" "$1"; }
fail()  { printf '     %s✗%s %s\n' "$RED" "$RESET" "$1"; FAILURES=$((FAILURES + 1)); }
info()  { printf '     %s·%s %s\n' "$YELLOW" "$RESET" "$1"; }

require_tool() {
  command -v "$1" >/dev/null 2>&1 || {
    printf '%sНе найден %s — он нужен для smoke-прогона%s\n' "$RED" "$1" "$RESET" >&2
    exit 1
  }
}
require_tool curl
require_tool jq

WORKDIR="$(mktemp -d)"
trap 'rm -rf "$WORKDIR"' EXIT

# request METHOD PATH [BODY] [EXTRA_HEADER...]
# Возвращает HTTP-статус в переменной STATUS, тело — в файле $WORKDIR/body.json.
request() {
  local method=$1 path=$2 body=${3:-} ; shift 3 || shift $#
  local -a args=(-sS -o "$WORKDIR/body.json" -w '%{http_code}' -X "$method")

  if [[ -n "$body" ]]; then
    args+=(-H 'Content-Type: application/json' -d "$body")
  fi
  local header
  for header in "$@"; do
    args+=(-H "$header")
  done

  STATUS=$(curl "${args[@]}" "$path" || echo "000")
}

body() { cat "$WORKDIR/body.json"; }

# Проверки намеренно НЕ возвращают ненулевой код: под `set -e` это оборвало бы
# прогон на первом же расхождении, а нам важнее увидеть все сразу. Провалы
# копятся в FAILURES, итог подводится в конце. Там, где продолжать бессмысленно
# (не создался заказ, на котором держатся следующие шаги), используется die.

# expect_status EXPECTED DESCRIPTION
expect_status() {
  local expected=$1 description=$2
  if [[ "$STATUS" == "$expected" ]]; then
    ok "$description → HTTP $STATUS"
    return 0
  fi
  fail "$description → ожидался HTTP $expected, получен $STATUS"
  info "ответ: $(body | head -c 400)"
  return 0
}

# expect_code EXPECTED_ERROR_CODE DESCRIPTION
expect_code() {
  local expected=$1 description=$2
  local actual
  actual=$(body | jq -r '.code // "<нет поля code>"')
  if [[ "$actual" == "$expected" ]]; then
    ok "$description → code = $actual"
    return 0
  fi
  fail "$description → ожидался code = $expected, получен $actual"
  info "ответ: $(body | head -c 400)"
  return 0
}

# die STATUS MESSAGE — останавливает прогон, если дальше проверять нечего.
die() {
  local expected=$1 message=$2
  if [[ "$STATUS" != "$expected" ]]; then
    fail "$message (HTTP $STATUS)"
    info "ответ: $(body | head -c 400)"
    printf '\n%sДальнейшие шаги зависят от этого результата — прогон остановлен%s\n' "$RED" "$RESET"
    exit 1
  fi
}

# ---------------------------------------------------------------------------
# 1. Готовность сервисов
# ---------------------------------------------------------------------------

step "Ожидание готовности api и restaurant-sim"

wait_ready() {
  local name=$1 url=$2 deadline=$((SECONDS + STARTUP_TIMEOUT))
  while (( SECONDS < deadline )); do
    if curl -sSf -o /dev/null "$url" 2>/dev/null; then
      ok "$name готов ($url)"
      return 0
    fi
    sleep 1
  done
  fail "$name не поднялся за ${STARTUP_TIMEOUT}s ($url)"
  return 1
}

wait_ready "api" "$API/readyz" || exit 1
wait_ready "restaurant-sim" "$SIM/health" || exit 1

# ---------------------------------------------------------------------------
# 2. Каталог заведений
# ---------------------------------------------------------------------------

step "Каталог заведений"

request GET "$API/api/v1/restaurants?limit=20"
expect_status 200 "GET /api/v1/restaurants"

RESTAURANT_ID=$(body | jq -r --arg slug "$RESTAURANT_SLUG" \
  '.items[] | select(.slug == $slug) | .id')

if [[ -z "$RESTAURANT_ID" || "$RESTAURANT_ID" == "null" ]]; then
  fail "заведение «$RESTAURANT_SLUG» не найдено в каталоге"
  exit 1
fi
ok "заведение «$RESTAURANT_SLUG» найдено, id = $RESTAURANT_ID"

MIN_ORDER=$(body | jq -r --arg slug "$RESTAURANT_SLUG" \
  '.items[] | select(.slug == $slug) | .min_order_kopecks')
info "минимальная сумма заказа: $((MIN_ORDER / 100)) ₽"

# ---------------------------------------------------------------------------
# 3. Синхронизация меню через Partner API
# ---------------------------------------------------------------------------

step "Синхронизация меню заведением (Partner API)"

# Остатки задаются здесь явно, чтобы сценарий не зависел от того, что успел
# синхронизировать restaurant-sim при старте.
SYNC_BODY=$(cat <<'JSON'
{
  "products": [
    {"product_key": "pizza_margherita", "category": "Пицца", "name": "Пицца Маргарита",
     "description": "Томаты, моцарелла, базилик", "price_kopecks": 59000,
     "available": true, "stock_qty": 5},
    {"product_key": "pasta_carbonara", "category": "Паста", "name": "Паста Карбонара",
     "description": "Спагетти, гуанчале, пармезан", "price_kopecks": 49000,
     "available": true, "stock_qty": null},
    {"product_key": "drink_cola", "category": "Напитки", "name": "Кола 0,5 л",
     "description": "Охлаждённая", "price_kopecks": 12000,
     "available": true, "stock_qty": 20},
    {"product_key": "dessert_tiramisu", "category": "Десерты", "name": "Тирамису",
     "description": "Классический", "price_kopecks": 29000,
     "available": false, "stock_qty": 0}
  ]
}
JSON
)

request POST "$API/api/v1/partner/menu/sync" "$SYNC_BODY" "X-Partner-Token: $PARTNER_TOKEN"
expect_status 200 "POST /api/v1/partner/menu/sync"

MENU_VERSION=$(body | jq -r '.version')
ITEMS_SYNCED=$(body | jq -r '.items_synced')
ok "опубликована версия меню $MENU_VERSION, позиций: $ITEMS_SYNCED"

request GET "$API/api/v1/restaurants/$RESTAURANT_SLUG/menu"
expect_status 200 "GET /api/v1/restaurants/{slug}/menu"
SHOWN_VERSION=$(body | jq -r '.menu_version')
if [[ "$SHOWN_VERSION" == "$MENU_VERSION" ]]; then
  ok "витрина показывает актуальную версию меню ($SHOWN_VERSION)"
else
  fail "витрина показывает версию $SHOWN_VERSION, а опубликована $MENU_VERSION"
fi

# Партнёрское API закрыто: без токена доступа нет.
request POST "$API/api/v1/partner/menu/sync" "$SYNC_BODY"
expect_status 401 "запрос к Partner API без токена"
expect_code "UNAUTHORIZED" "  отказ в доступе"

# ---------------------------------------------------------------------------
# 4. Заказ сверх остатка
# ---------------------------------------------------------------------------

step "Заказ сверх доступного остатка → 409 OUT_OF_STOCK"

OVER_STOCK_BODY=$(cat <<JSON
{
  "user_external_id": "usr_smoke",
  "restaurant_id": $RESTAURANT_ID,
  "delivery_address": "ул. Ленина, 10",
  "items": [{"product_key": "pizza_margherita", "qty": 99}]
}
JSON
)

request POST "$API/api/v1/orders" "$OVER_STOCK_BODY" \
  "Idempotency-Key: $(uuidgen 2>/dev/null || echo "smoke-overstock-$RANDOM$RANDOM")"
expect_status 409 "POST /api/v1/orders (99 пицц при остатке 5)"
expect_code "OUT_OF_STOCK" "  причина отказа"

# ---------------------------------------------------------------------------
# 5. Заказ ниже минимальной суммы
# ---------------------------------------------------------------------------

step "Заказ ниже минимальной суммы → 422 MIN_ORDER_NOT_MET"

BELOW_MIN_BODY=$(cat <<JSON
{
  "user_external_id": "usr_smoke",
  "restaurant_id": $RESTAURANT_ID,
  "delivery_address": "ул. Ленина, 10",
  "items": [{"product_key": "drink_cola", "qty": 1}]
}
JSON
)

request POST "$API/api/v1/orders" "$BELOW_MIN_BODY" \
  "Idempotency-Key: $(uuidgen 2>/dev/null || echo "smoke-belowmin-$RANDOM$RANDOM")"
expect_status 422 "POST /api/v1/orders (120 ₽ при минимуме $((MIN_ORDER / 100)) ₽)"
expect_code "MIN_ORDER_NOT_MET" "  причина отказа"

# Недоступная позиция тоже не проходит.
UNAVAILABLE_BODY=$(cat <<JSON
{
  "user_external_id": "usr_smoke",
  "restaurant_id": $RESTAURANT_ID,
  "delivery_address": "ул. Ленина, 10",
  "items": [{"product_key": "dessert_tiramisu", "qty": 1}]
}
JSON
)
request POST "$API/api/v1/orders" "$UNAVAILABLE_BODY" \
  "Idempotency-Key: $(uuidgen 2>/dev/null || echo "smoke-unavail-$RANDOM$RANDOM")"
expect_status 409 "POST /api/v1/orders (позиция снята с продажи)"
expect_code "PRODUCT_UNAVAILABLE" "  причина отказа"

# ---------------------------------------------------------------------------
# 6. Успешное оформление
# ---------------------------------------------------------------------------

step "Успешное оформление заказа → 201"

IDEMPOTENCY_KEY=$(uuidgen 2>/dev/null || echo "smoke-order-$RANDOM$RANDOM$RANDOM")

ORDER_BODY=$(cat <<JSON
{
  "user_external_id": "usr_smoke",
  "restaurant_id": $RESTAURANT_ID,
  "delivery_address": "ул. Ленина, 10",
  "items": [
    {"product_key": "pizza_margherita", "qty": 2},
    {"product_key": "drink_cola", "qty": 1}
  ]
}
JSON
)

request POST "$API/api/v1/orders" "$ORDER_BODY" "Idempotency-Key: $IDEMPOTENCY_KEY"
expect_status 201 "POST /api/v1/orders"
die 201 "заказ не создан — нечего проверять дальше"

ORDER_NUMBER=$(body | jq -r '.public_number')
ORDER_TOTAL=$(body | jq -r '.total_kopecks')
ORDER_SUBTOTAL=$(body | jq -r '.subtotal_kopecks')
ORDER_STATUS=$(body | jq -r '.status')

ok "заказ $ORDER_NUMBER создан в статусе $ORDER_STATUS"
info "сумма позиций $((ORDER_SUBTOTAL / 100)) ₽, итого с доставкой $((ORDER_TOTAL / 100)) ₽"

# Сумма считается сервером по ценам из БД: 2 × 590 + 1 × 120 = 1300 ₽.
if [[ "$ORDER_SUBTOTAL" == "130000" ]]; then
  ok "сумма посчитана по ценам из БД, а не из запроса"
else
  fail "ожидалась сумма позиций 130000 копеек, получена $ORDER_SUBTOTAL"
fi

# ---------------------------------------------------------------------------
# 7. Идемпотентный повтор
# ---------------------------------------------------------------------------

step "Повтор с тем же Idempotency-Key → тот же заказ"

request POST "$API/api/v1/orders" "$ORDER_BODY" "Idempotency-Key: $IDEMPOTENCY_KEY"
expect_status 201 "повторный POST /api/v1/orders"

REPLAY_NUMBER=$(body | jq -r '.public_number')
if [[ "$REPLAY_NUMBER" == "$ORDER_NUMBER" ]]; then
  ok "вернулся тот же public_number — второй заказ не создан"
else
  fail "повтор создал другой заказ: $REPLAY_NUMBER вместо $ORDER_NUMBER"
fi

# ---------------------------------------------------------------------------
# 8. Тот же ключ с другим телом
# ---------------------------------------------------------------------------

step "Тот же Idempotency-Key с изменённым телом → 422"

CHANGED_BODY=$(cat <<JSON
{
  "user_external_id": "usr_smoke",
  "restaurant_id": $RESTAURANT_ID,
  "delivery_address": "ул. Ленина, 11",
  "items": [{"product_key": "pizza_margherita", "qty": 1}]
}
JSON
)

request POST "$API/api/v1/orders" "$CHANGED_BODY" "Idempotency-Key: $IDEMPOTENCY_KEY"
expect_status 422 "POST /api/v1/orders с чужим телом под тем же ключом"
expect_code "IDEMPOTENCY_PAYLOAD_MISMATCH" "  причина отказа"

# Заголовок обязателен.
request POST "$API/api/v1/orders" "$ORDER_BODY"
expect_status 400 "POST /api/v1/orders без Idempotency-Key"
expect_code "IDEMPOTENCY_KEY_REQUIRED" "  причина отказа"

# ---------------------------------------------------------------------------
# 9. Доставка события и автоматический конвейер заведения
# ---------------------------------------------------------------------------

step "Событие доезжает до заведения, оно ведёт заказ по конвейеру"

info "ждём переход в READY (до ${PIPELINE_TIMEOUT}s)"

deadline=$((SECONDS + PIPELINE_TIMEOUT))
seen_statuses=""
final_status=""

while (( SECONDS < deadline )); do
  request GET "$API/api/v1/orders/$ORDER_NUMBER"
  if [[ "$STATUS" != "200" ]]; then
    sleep 1
    continue
  fi

  current=$(body | jq -r '.status')
  if [[ "$seen_statuses" != *"$current"* ]]; then
    seen_statuses="$seen_statuses $current"
    info "статус: $current"
  fi

  if [[ "$current" == "READY" ]]; then
    final_status="READY"
    break
  fi
  if [[ "$current" == "REJECTED" || "$current" == "CANCELLED" ]]; then
    final_status="$current"
    break
  fi
  sleep 1
done

if [[ "$final_status" == "READY" ]]; then
  ok "заведение приняло заказ и довело его до READY без ручного вмешательства"
else
  fail "заказ не дошёл до READY за ${PIPELINE_TIMEOUT}s (последний статус: ${final_status:-неизвестен})"
fi

# Заказ виден в очереди заведения.
request GET "$API/api/v1/partner/orders?limit=50" "" "X-Partner-Token: $PARTNER_TOKEN"
expect_status 200 "GET /api/v1/partner/orders"
IN_QUEUE=$(body | jq -r --arg n "$ORDER_NUMBER" '[.items[] | select(.public_number == $n)] | length')
if [[ "$IN_QUEUE" == "1" ]]; then
  ok "заказ виден в очереди заведения"
else
  fail "заказ не найден в очереди заведения"
fi

# ---------------------------------------------------------------------------
# 10. Карточка заказа и таймлайн
# ---------------------------------------------------------------------------

step "Карточка заказа: позиции, снапшот цен и таймлайн"

request GET "$API/api/v1/orders/$ORDER_NUMBER"
expect_status 200 "GET /api/v1/orders/{public_number}"

TIMELINE_LEN=$(body | jq -r '.timeline | length')
ITEMS_LEN=$(body | jq -r '.items | length')

if [[ "$ITEMS_LEN" == "2" ]]; then
  ok "в заказе 2 позиции со снапшотом названий и цен"
else
  fail "ожидалось 2 позиции, получено $ITEMS_LEN"
fi

# NEW + ACCEPTED + COOKING + READY = 4 события.
if (( TIMELINE_LEN >= 4 )); then
  ok "таймлайн содержит $TIMELINE_LEN событий: $(body | jq -r '[.timeline[].to_status] | join(" → ")')"
else
  fail "ожидалось не менее 4 событий в таймлайне, получено $TIMELINE_LEN"
  info "таймлайн: $(body | jq -c '[.timeline[] | {to_status, actor}]')"
fi

ACTORS=$(body | jq -r '[.timeline[].actor] | unique | join(", ")')
info "инициаторы переходов: $ACTORS"

# Несуществующий заказ.
request GET "$API/api/v1/orders/00000000-0000-7000-8000-000000000000"
expect_status 404 "GET несуществующего заказа"
expect_code "ORDER_NOT_FOUND" "  причина отказа"

# ---------------------------------------------------------------------------
# 11. Поздняя отмена
# ---------------------------------------------------------------------------

step "Отмена заказа в статусе READY → 409 ORDER_CANNOT_BE_CANCELLED"

request POST "$API/api/v1/orders/$ORDER_NUMBER/cancel" '{"reason": "Передумал"}'
expect_status 409 "POST /api/v1/orders/{n}/cancel для готового заказа"
expect_code "ORDER_CANNOT_BE_CANCELLED" "  причина отказа"

# А вот свежий заказ пользователь отменить вправе — и остатки вернутся.
step "Отмена свежего заказа пользователем → 200 и возврат остатков"

request GET "$API/api/v1/restaurants/$RESTAURANT_SLUG/menu"
STOCK_BEFORE=$(body | jq -r '[.categories[].products[] | select(.product_key == "pizza_margherita") | .stock_qty][0]')

CANCELLABLE_KEY=$(uuidgen 2>/dev/null || echo "smoke-cancel-$RANDOM$RANDOM")
request POST "$API/api/v1/orders" "$ORDER_BODY" "Idempotency-Key: $CANCELLABLE_KEY"
expect_status 201 "создание заказа под отмену"
CANCEL_NUMBER=$(body | jq -r '.public_number')

request POST "$API/api/v1/orders/$CANCEL_NUMBER/cancel" '{"reason": "Ошибся адресом"}'
if [[ "$STATUS" == "200" ]]; then
  CANCEL_STATUS=$(body | jq -r '.status')
  ok "заказ отменён пользователем, статус $CANCEL_STATUS"

  request GET "$API/api/v1/restaurants/$RESTAURANT_SLUG/menu"
  STOCK_AFTER=$(body | jq -r '[.categories[].products[] | select(.product_key == "pizza_margherita") | .stock_qty][0]')
  if [[ "$STOCK_AFTER" == "$STOCK_BEFORE" ]]; then
    ok "остатки вернулись в каталог: было $STOCK_BEFORE, стало $STOCK_AFTER"
  else
    info "остаток до отмены $STOCK_BEFORE, после $STOCK_AFTER (заведение могло принять заказ раньше отмены)"
  fi
else
  # Заведение с SIM_AUTO_ACCEPT принимает заказ примерно через секунду —
  # проигранная гонка здесь штатна, а не ошибка.
  info "заведение успело принять заказ раньше отмены (HTTP $STATUS) — штатная гонка"
fi

# ---------------------------------------------------------------------------
# 12. Закрытие кухни
# ---------------------------------------------------------------------------

step "Закрытие кухни → новые заказы отклоняются"

request POST "$API/api/v1/partner/kitchen-status" '{"status": "closed"}' \
  "X-Partner-Token: $PARTNER_TOKEN"
expect_status 200 "POST /api/v1/partner/kitchen-status { closed }"

request POST "$API/api/v1/orders" "$ORDER_BODY" \
  "Idempotency-Key: $(uuidgen 2>/dev/null || echo "smoke-closed-$RANDOM$RANDOM")"
expect_status 422 "POST /api/v1/orders при закрытой кухне"
expect_code "RESTAURANT_UNAVAILABLE" "  причина отказа"

# Возвращаем кухню в рабочий режим, чтобы повторный прогон начинался с чистого
# состояния.
request POST "$API/api/v1/partner/kitchen-status" '{"status": "online"}' \
  "X-Partner-Token: $PARTNER_TOKEN"
expect_status 200 "возврат кухни в режим online"

# ---------------------------------------------------------------------------
# Итог
# ---------------------------------------------------------------------------

printf '\n%s' "$BOLD"
if (( FAILURES == 0 )); then
  printf '%sВсе проверки пройдены (%d шагов)%s\n' "$GREEN" "$STEP" "$RESET"
  exit 0
fi

printf '%sПровалено проверок: %d (шагов: %d)%s\n' "$RED" "$FAILURES" "$STEP" "$RESET"
exit 1
