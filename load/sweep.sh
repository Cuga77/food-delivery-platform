#!/usr/bin/env bash
#
# Развёртка нагрузки: серия прогонов с растущим параметром и сводная таблица.
#
# Один прогон k6 отвечает «выдержал или нет». Чтобы найти потолок и увидеть
# узкое место, нужна серия: где кривая задержки ломается — там и предел.
#
#   ./load/sweep.sh throughput   # ищем максимум пропускной способности записи
#   ./load/sweep.sh contention   # цена конкуренции в зависимости от числа клиентов
#   ./load/sweep.sh spread       # блокировка строки или что-то общее?
#
# Параметры через окружение:
#   BASE_URL  адрес сервиса (по умолчанию http://localhost:8080)
#   RATES     список RPS для throughput
#   VU_LIST   список VU для contention
#   SPREADS   список значений SPREAD

set -euo pipefail

cd "$(dirname "$0")/.."

K6="${K6:-./bin/k6}"
command -v "$K6" >/dev/null 2>&1 || K6=k6
command -v "$K6" >/dev/null 2>&1 || { echo "k6 не найден: поставьте его или задайте K6=<путь>" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "нужен jq" >&2; exit 1; }

export BASE_URL="${BASE_URL:-http://localhost:8080}"

MODE="${1:-throughput}"
OUT="$(mktemp -d)"
trap 'rm -rf "$OUT"' EXIT

# Перцентили, которые k6 положит в сводку. p99 обязателен: под конкуренцией
# именно хвост показывает, во что упирается система, а медиана остаётся ровной.
TREND_STATS="med,p(95),p(99),max"

# run <файл сценария> <имя прогона> <доп. -e аргументы...>
run() {
  local script=$1 label=$2
  shift 2

  # --summary-mode=disabled здесь нельзя: он отключает и запись
  # --summary-export, а именно из неё собирается таблица. Сам вывод глушится
  # перенаправлением в лог прогона.
  "$K6" run --quiet \
    --summary-trend-stats="$TREND_STATS" \
    --summary-export="$OUT/$label.json" \
    "$@" "$script" >"$OUT/$label.log" 2>&1 || true
}

# stat <прогон> <метрика> <поле>
stat() {
  jq -r --arg m "$2" --arg f "$3" '
    (.metrics[$m][$f] // .metrics[$m].values[$f] // empty) as $v
    | if $v == null then "—" else ($v * 100 | round / 100 | tostring) end
  ' "$OUT/$1.json" 2>/dev/null || echo "—"
}

count() {
  jq -r --arg m "$2" '(.metrics[$m].count // .metrics[$m].values.count // 0) | floor' \
    "$OUT/$1.json" 2>/dev/null || echo 0
}

rate_of() {
  jq -r --arg m "$2" '((.metrics[$m].rate // .metrics[$m].values.rate // 0) * 100 | round / 100)' \
    "$OUT/$1.json" 2>/dev/null || echo 0
}

# --- Режим 1: потолок пропускной способности --------------------------------
#
# Позиции с неограниченным остатком: конкуренции за строку нет, меряем
# исключительно способность переваривать транзакции оформления.
sweep_throughput() {
  local rates=(${RATES:-50 100 200 400 800 1200})

  echo "Развёртка по интенсивности — путь записи без конкуренции за остаток"
  echo "Постоянная интенсивность, 20 секунд на шаг, остаток неограничен"
  echo
  printf '| %-10s | %-12s | %-9s | %-9s | %-9s | %-9s | %-10s |\n' \
    'цель, rps' 'факт, зак/с' 'med' 'p95' 'p99' 'max' 'недодано'
  printf '|%s|%s|%s|%s|%s|%s|%s|\n' \
    '------------' '--------------' '-----------' '-----------' '-----------' '-----------' '------------'

  for rate in "${rates[@]}"; do
    run load/order.js "rps-$rate" -e "RATE=$rate" -e DURATION=20s -e PROFILE=constant

    printf '| %-10s | %-12s | %-9s | %-9s | %-9s | %-9s | %-10s |\n' \
      "$rate" \
      "$(jq -r '((.metrics.orders_created.rate // 0) * 100 | round / 100)' "$OUT/rps-$rate.json")" \
      "$(stat "rps-$rate" order_create_duration med)" \
      "$(stat "rps-$rate" order_create_duration 'p(95)')" \
      "$(stat "rps-$rate" order_create_duration 'p(99)')" \
      "$(stat "rps-$rate" order_create_duration max)" \
      "$(jq -r '(.metrics.dropped_iterations.count // 0) | floor' "$OUT/rps-$rate.json")"
  done

  echo
  echo "Насыщение — там, где «факт» отстаёт от «цели» и появляется «недодано»:"
  echo "k6 не успевает выдавать запросы в заданном темпе, потому что сервис не отвечает."
}

# --- Режим 2: цена конкуренции ----------------------------------------------
#
# Остаток фиксирован и лежит на одной строке. Растёт только число клиентов,
# которые за него дерутся. Пропускная способность продаж должна упереться в
# потолок, а задержка успешного заказа — расти вместе с очередью на блокировке.
sweep_contention() {
  local vus=(${VU_LIST:-1 5 10 25 50 100 200})
  local stock="${STOCK:-300}"
  local iterations="${ITERATIONS:-900}"

  echo "Развёртка по конкуренции — $stock порций на ОДНОЙ строке, $iterations попыток"
  echo
  printf '| %-4s | %-8s | %-9s | %-9s | %-9s | %-9s | %-9s |\n' \
    'VU' 'продано' 'усп. med' 'усп. p95' 'усп. p99' 'отказ p95' 'отказ p99'
  printf '|%s|%s|%s|%s|%s|%s|%s|\n' \
    '------' '----------' '-----------' '-----------' '-----------' '-----------' '-----------'

  for vu in "${vus[@]}"; do
    run load/contention.js "vu-$vu" \
      -e "STOCK=$stock" -e "ITERATIONS=$iterations" -e "VUS=$vu" -e SPREAD=1

    printf '| %-4s | %-8s | %-9s | %-9s | %-9s | %-9s | %-9s |\n' \
      "$vu" \
      "$(count "vu-$vu" orders_created)" \
      "$(stat "vu-$vu" order_accepted_duration med)" \
      "$(stat "vu-$vu" order_accepted_duration 'p(95)')" \
      "$(stat "vu-$vu" order_accepted_duration 'p(99)')" \
      "$(stat "vu-$vu" order_rejected_duration 'p(95)')" \
      "$(stat "vu-$vu" order_rejected_duration 'p(99)')"
  done

  echo
  echo "Столбец «продано» обязан всюду равняться $stock — иначе нарушен инвариант."
  echo "Рост «усп. p99» при неизменном «отказ p99» означает очередь на строчной блокировке."
}

# --- Режим 3: изоляция причины ----------------------------------------------
#
# Суммарный остаток и число клиентов постоянны, меняется только число строк,
# по которым дефицит разложен. Если задержка падает вместе с ростом SPREAD,
# узкое место — блокировка строки, а не общий ресурс.
sweep_spread() {
  local spreads=(${SPREADS:-1 2 5 10 25 50})
  local stock="${STOCK:-300}"
  local iterations="${ITERATIONS:-900}"
  local vus="${VUS:-100}"

  echo "Развёртка по разносу — $stock порций, $vus VU, $iterations попыток"
  echo "Меняется только число строк, между которыми поделён тот же остаток"
  echo
  printf '| %-7s | %-10s | %-8s | %-9s | %-9s | %-9s |\n' \
    'строк' 'на строку' 'продано' 'усп. med' 'усп. p95' 'усп. p99'
  printf '|%s|%s|%s|%s|%s|%s|\n' \
    '---------' '------------' '----------' '-----------' '-----------' '-----------'

  for spread in "${spreads[@]}"; do
    run load/contention.js "spread-$spread" \
      -e "STOCK=$stock" -e "ITERATIONS=$iterations" -e "VUS=$vus" -e "SPREAD=$spread"

    printf '| %-7s | %-10s | %-8s | %-9s | %-9s | %-9s |\n' \
      "$spread" \
      "$((stock / spread))" \
      "$(count "spread-$spread" orders_created)" \
      "$(stat "spread-$spread" order_accepted_duration med)" \
      "$(stat "spread-$spread" order_accepted_duration 'p(95)')" \
      "$(stat "spread-$spread" order_accepted_duration 'p(99)')"
  done

  echo
  echo "Падение задержки с ростом числа строк = узкое место в строчной блокировке."
  echo "Если задержка не меняется, упирается что-то общее: пул, CPU, диск."
}

case "$MODE" in
  throughput) sweep_throughput ;;
  contention) sweep_contention ;;
  spread) sweep_spread ;;
  *)
    echo "неизвестный режим: $MODE (throughput | contention | spread)" >&2
    exit 1
    ;;
esac
