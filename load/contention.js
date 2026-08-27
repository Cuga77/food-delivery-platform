// Сценарий «гонка за последней порцией».
//
// Это не столько замер производительности, сколько проверка главного
// инварианта системы под настоящей конкуренцией: **нельзя продать больше
// порций, чем есть**. Десятки виртуальных пользователей одновременно заказывают
// одну и ту же дефицитную позицию, которой заведомо меньше, чем попыток.
//
// Ожидаемый результат: успешных заказов ровно столько, каков был остаток,
// остальные получают 409 OUT_OF_STOCK, а остаток на витрине уходит в ноль и
// никогда — в минус. Проверяется не глазами: пороги ниже требуют точного
// совпадения, поэтому расхождение роняет прогон.
//
// Интеграционный тест проверяет то же самое на 25 горутинах внутри процесса;
// здесь та же гарантия проверяется снаружи, через HTTP, с сетью и пулом
// соединений в цепочке.
//
//   k6 run load/contention.js
//   k6 run -e STOCK=500 -e ITERATIONS=1500 -e VUS=100 load/contention.js

import { check } from 'k6';
import { Counter } from 'k6/metrics';
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

const SCARCE_KEY = 'load_scarce';

const ordersCreated = new Counter('orders_created');
const ordersOutOfStock = new Counter('orders_out_of_stock');
const ordersUnexpected = new Counter('orders_unexpected');

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
    // Ядро проверки. Верхняя граница ловит перепродажу, нижняя — потерю
    // остатка: если успешных меньше, чем было порций, часть товара «испарилась».
    orders_created: [`count<=${STOCK}`, `count>=${STOCK}`],
    // Отказы обязаны быть: иначе попыток было меньше остатка и гонки не случилось.
    orders_out_of_stock: ['count>0'],
    // Ни одного ответа, кроме 201 и 409 OUT_OF_STOCK.
    orders_unexpected: ['count==0'],
    http_req_failed: ['rate<0.01'],
    checks: ['rate>0.99'],
  },
};

export function setup() {
  waitForService();
  setKitchenStatus('online');

  // Публикуем новую версию меню: остаток должен быть известен точно.
  // Цена выше минимальной суммы заказа, чтобы отказы могли быть только
  // из-за остатка, а не из-за недобора корзины.
  publishMenu([
    {
      product_key: SCARCE_KEY,
      category: 'Дефицит',
      name: 'Дефицитная позиция',
      description: 'Ровно ограниченное количество порций',
      price_kopecks: 60000,
      available: true,
      stock_qty: STOCK,
    },
  ]);

  const restaurant = findRestaurant();

  const before = remainingStock(SCARCE_KEY);
  if (before !== STOCK) {
    throw new Error(`меню опубликовано с остатком ${before}, ожидалось ${STOCK}`);
  }

  console.log(`гонка: ${ITERATIONS} попыток от ${VUS} VU за ${STOCK} порций`);

  return { restaurantId: restaurant.id, stock: STOCK };
}

export default function (data) {
  const result = createOrder({
    restaurantId: data.restaurantId,
    items: [{ product_key: SCARCE_KEY, qty: 1 }],
    keyPrefix: 'k6-race',
  });

  if (result.status === 201) {
    ordersCreated.add(1);
  } else if (result.status === 409 && result.code === 'OUT_OF_STOCK') {
    ordersOutOfStock.add(1);
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
  const left = remainingStock(SCARCE_KEY);

  console.log('');
  console.log('──────────────────────────────────────────────');
  console.log(`  было порций:      ${data.stock}`);
  console.log(`  осталось:         ${left}`);
  console.log('──────────────────────────────────────────────');

  if (left !== 0) {
    // Остаток не в нуле означает, что попыток не хватило, чтобы разобрать
    // товар: гонки не было и проверять нечего.
    throw new Error(`остаток не обнулился (${left}) — увеличьте ITERATIONS относительно STOCK`);
  }

  console.log('  остаток обнулён и не ушёл в минус');
  console.log('  точное число проданных порций проверяется порогом orders_created');
  console.log('');
}
