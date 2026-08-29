// Сценарий «оформление заказа»: путь записи.
//
// Каждая итерация открывает транзакцию, списывает остатки, вставляет заказ,
// снапшот позиций, событие истории и запись в outbox. Это самая дорогая
// операция сервиса, и именно её пропускную способность имеет смысл мерить.
//
// Позиции намеренно с неограниченным остатком (stock_qty: null): здесь нужна
// пропускная способность, а не борьба за дефицит. Конкуренция за строку —
// отдельный сценарий, load/contention.js.
//
//   k6 run load/order.js
//   k6 run -e RATE=50 -e DURATION=1m load/order.js

import { check } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';
import {
  createOrder,
  expectWriteStatuses,
  fetchOrder,
  findRestaurant,
  publishMenu,
  setKitchenStatus,
  waitForService,
} from './lib.js';

expectWriteStatuses();

const RATE = Number(__ENV.RATE || 30);
const DURATION = __ENV.DURATION || '30s';

const ordersCreated = new Counter('orders_created');
const orderCreateDuration = new Trend('order_create_duration', true);
const orderRejected = new Rate('orders_rejected');

// PROFILE=constant включает постоянную интенсивность вместо разгона ступенями.
//
// Для одиночного прогона удобнее ступени: видно, как сервис входит в нагрузку.
// Для развёртки (load/sweep.sh) нужна ровная полка — иначе средняя за прогон
// оказывается ниже цели просто из-за формы ступеней, и «недобор» невозможно
// отличить от насыщения.
const CONSTANT = (__ENV.PROFILE || '') === 'constant';

const rampingScenario = {
  executor: 'ramping-arrival-rate',
  startRate: Math.max(1, Math.floor(RATE / 5)),
  timeUnit: '1s',
  preAllocatedVUs: Math.max(10, RATE),
  maxVUs: Math.max(50, RATE * 4),
  stages: [
    { target: RATE, duration: '10s' }, // разгон
    { target: RATE, duration: DURATION }, // плато
    { target: 0, duration: '5s' }, // остановка
  ],
};

const constantScenario = {
  executor: 'constant-arrival-rate',
  rate: RATE,
  timeUnit: '1s',
  duration: DURATION,
  preAllocatedVUs: Math.max(20, RATE),
  maxVUs: Math.max(100, RATE * 4),
};

export const options = {
  scenarios: {
    order: CONSTANT ? constantScenario : rampingScenario,
  },
  thresholds: {
    // Оформление — транзакция на пять вставок; полсекунды на p95 — потолок,
    // за которым имеет смысл смотреть в план запросов и пул соединений.
    order_create_duration: ['p(95)<500', 'p(99)<1000'],
    // Отказ здесь означал бы реальную проблему: остатки не ограничены,
    // сумма выше минимума, кухня открыта.
    orders_rejected: ['rate<0.01'],
    http_req_failed: ['rate<0.01'],
    checks: ['rate>0.99'],
    orders_created: ['count>0'],
  },
};

export function setup() {
  waitForService();
  setKitchenStatus('online');

  publishMenu([
    { product_key: 'load_pizza', category: 'Пицца', name: 'Пицца нагрузочная',
      price_kopecks: 60000, available: true, stock_qty: null },
    { product_key: 'load_pasta', category: 'Паста', name: 'Паста нагрузочная',
      price_kopecks: 55000, available: true, stock_qty: null },
    { product_key: 'load_drink', category: 'Напитки', name: 'Напиток',
      price_kopecks: 12000, available: true, stock_qty: null },
  ]);

  const restaurant = findRestaurant();

  return { restaurantId: restaurant.id, minOrder: restaurant.min_order_kopecks };
}

export default function (data) {
  // Корзина из одной-двух позиций; любая из них дороже минимальной суммы
  // заказа, поэтому MIN_ORDER_NOT_MET здесь возникать не должен.
  const items = [{ product_key: 'load_pizza', qty: 1 + Math.floor(Math.random() * 2) }];
  if (Math.random() < 0.5) {
    items.push({ product_key: 'load_drink', qty: 1 });
  }

  const result = createOrder({ restaurantId: data.restaurantId, items, keyPrefix: 'k6-order' });

  orderCreateDuration.add(result.response.timings.duration);
  orderRejected.add(result.status !== 201);

  const ok = check(result, {
    'заказ создан': (r) => r.status === 201,
    'номер заказа выдан': (r) => Boolean(r.order?.public_number),
    'сумма посчитана сервером': (r) =>
      r.status !== 201 || r.order.total_kopecks === r.order.subtotal_kopecks + r.order.delivery_fee_kopecks,
  });

  if (!ok || !result.order) {
    return;
  }

  ordersCreated.add(1);

  // Каждый десятый заказ перечитываем: путь чтения карточки собирает заказ,
  // позиции и таймлайн тремя запросами, и его стоимость тоже важна.
  if (Math.random() < 0.1) {
    fetchOrder(result.order.public_number);
  }
}
