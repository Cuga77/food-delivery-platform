// Сценарий «гонка за последней порцией».
//
// Отвечает на два разных вопроса, и их важно не путать.
//
// 1. КОРРЕКТНОСТЬ. Нельзя продать больше порций, чем есть. Пороги ниже требуют
//    точного совпадения числа проданных порций с исходным остатком, поэтому
//    расхождение роняет прогон само.
//
// 2. ЦЕНА КОНКУРЕНЦИИ. Сколько стоит сериализация на строке дефицитного товара.
//    Задержка измеряется отдельно по исходам: успешный заказ ждёт строчную
//    блокировку, а отказ после обнуления остатка отвечает сразу. Смешивать их
//    в одну метрику бессмысленно — это две разные популяции, и общий p95
//    получается средней температурой по больнице.
//
// Ручка SPREAD разносит тот же суммарный остаток по нескольким позициям и
// показывает, действительно ли узкое место — блокировка одной строки:
//
//   SPREAD=1  — весь дефицит на одной строке, максимальная конкуренция
//   SPREAD=20 — тот же остаток на 20 строках, конкуренция за строку в 20 раз ниже
//
// Если при равной нагрузке SPREAD=20 даёт кратно меньшую задержку, узкое место
// именно в строчной блокировке, а не в пуле соединений, сети или CPU.
//
//   k6 run load/contention.js
//   k6 run -e STOCK=500 -e ITERATIONS=1500 -e VUS=100 load/contention.js
//   k6 run -e SPREAD=20 load/contention.js

import { check } from 'k6';
import { Counter, Trend } from 'k6/metrics';
import {
  createOrder,
  expectBusinessStatuses,
  findRestaurant,
  publishMenu,
  remainingStock,
  setKitchenStatus,
  waitForService,
} from './lib.js';

expectBusinessStatuses();

const STOCK = Number(__ENV.STOCK || 200);
const ITERATIONS = Number(__ENV.ITERATIONS || 600);
const VUS = Number(__ENV.VUS || 60);
// На сколько позиций разнести суммарный остаток.
const SPREAD = Math.max(1, Number(__ENV.SPREAD || 1));

const keyOf = (index) => `load_scarce_${index}`;

const ordersCreated = new Counter('orders_created');
const ordersOutOfStock = new Counter('orders_out_of_stock');
const ordersUnexpected = new Counter('orders_unexpected');

// Задержка по исходам — главное улучшение метрик этого сценария.
const acceptedDuration = new Trend('order_accepted_duration', true);
const rejectedDuration = new Trend('order_rejected_duration', true);

// Полный остаток продаётся целиком только когда он лежит на одной строке:
// при SPREAD > 1 попытки распределяются между позициями случайно, и часть
// позиций может остаться нераспроданной. Поэтому при разносе утверждается
// только безопасность (не продали лишнего), но не полная распродажа.
const sellOutThresholds =
  SPREAD === 1 ? [`count<=${STOCK}`, `count>=${STOCK}`] : [`count<=${STOCK}`];

export const options = {
  scenarios: {
    contention: {
      // Фиксированное число попыток, поделённое между VU: результат
      // детерминирован и его можно сравнить с исходным остатком.
      executor: 'shared-iterations',
      vus: VUS,
      iterations: ITERATIONS,
      maxDuration: '5m',
    },
  },
  thresholds: {
    // --- Корректность ---
    // Верхняя граница ловит перепродажу, нижняя — потерю остатка.
    orders_created: sellOutThresholds,
    // Отказы обязаны быть: иначе попыток было меньше остатка и гонки не случилось.
    orders_out_of_stock: ['count>0'],
    // Ни одного ответа, кроме 201 и 409 OUT_OF_STOCK.
    orders_unexpected: ['count==0'],
    http_req_failed: ['rate<0.01'],
    checks: ['rate>0.99'],

    // --- Цена конкуренции ---
    // Пороги намеренно широкие: это защита от катастрофы (лавина ожиданий,
    // исчерпание пула, дедлок), а не тонкая настройка. Точные значения для
    // сравнения между прогонами снимает load/sweep.sh.
    order_accepted_duration: ['p(95)<3000', 'p(99)<5000'],
    // Отказ не открывает транзакции и обязан быть быстрым при любой нагрузке.
    // Если и он тормозит, упёрлись не в блокировку строки, а во что-то общее.
    order_rejected_duration: ['p(95)<1000', 'p(99)<2000'],
  },
};

export function setup() {
  waitForService();
  setKitchenStatus('online');

  // Суммарный остаток делится между SPREAD позициями. Цена каждой выше
  // минимальной суммы заказа, чтобы отказы могли быть только из-за остатка.
  const perProduct = Math.floor(STOCK / SPREAD);
  const remainder = STOCK - perProduct * SPREAD;

  const products = [];
  for (let i = 0; i < SPREAD; i++) {
    products.push({
      product_key: keyOf(i),
      category: 'Дефицит',
      name: `Дефицитная позиция ${i + 1}`,
      price_kopecks: 60000,
      available: true,
      // Остаток от деления кладём в первую позицию, чтобы сумма сошлась точно.
      stock_qty: perProduct + (i === 0 ? remainder : 0),
    });
  }

  publishMenu(products);

  const restaurant = findRestaurant();

  const total = products.reduce((sum, p) => sum + p.stock_qty, 0);
  if (total !== STOCK) {
    throw new Error(`суммарный остаток ${total}, ожидалось ${STOCK}`);
  }

  const first = remainingStock(keyOf(0));
  if (first !== products[0].stock_qty) {
    throw new Error(`меню опубликовано с остатком ${first}, ожидалось ${products[0].stock_qty}`);
  }

  console.log(
    `гонка: ${ITERATIONS} попыток от ${VUS} VU за ${STOCK} порций на ${SPREAD} ` +
      `позици${SPREAD === 1 ? 'и' : 'ях'}`,
  );

  return { restaurantId: restaurant.id, stock: STOCK, spread: SPREAD };
}

export default function (data) {
  const productKey = keyOf(data.spread === 1 ? 0 : Math.floor(Math.random() * data.spread));

  const result = createOrder({
    restaurantId: data.restaurantId,
    items: [{ product_key: productKey, qty: 1 }],
    keyPrefix: 'k6-race',
  });

  const duration = result.response.timings.duration;

  if (result.status === 201) {
    ordersCreated.add(1);
    acceptedDuration.add(duration);
  } else if (result.status === 409 && result.code === 'OUT_OF_STOCK') {
    ordersOutOfStock.add(1);
    rejectedDuration.add(duration);
  } else {
    ordersUnexpected.add(1);
    console.error(`неожиданный ответ: HTTP ${result.status} ${result.code} ${result.response.body}`);
  }

  check(result, {
    'ответ — либо созданный заказ, либо честный отказ по остатку': (r) =>
      r.status === 201 || (r.status === 409 && r.code === 'OUT_OF_STOCK'),
  });
}

export function teardown(data) {
  let left = 0;
  for (let i = 0; i < data.spread; i++) {
    left += remainingStock(keyOf(i)) || 0;
  }

  console.log('');
  console.log('──────────────────────────────────────────────');
  console.log(`  было порций:   ${data.stock} на ${data.spread} позици${data.spread === 1 ? 'и' : 'ях'}`);
  console.log(`  осталось:      ${left}`);
  console.log('──────────────────────────────────────────────');

  if (data.spread === 1 && left !== 0) {
    // Остаток не в нуле означает, что попыток не хватило разобрать товар:
    // гонки не было и проверять нечего.
    throw new Error(`остаток не обнулился (${left}) — увеличьте ITERATIONS относительно STOCK`);
  }

  console.log('  остаток не ушёл в минус');
  console.log('  число проданных порций проверяется порогом orders_created');
  console.log('');
}
