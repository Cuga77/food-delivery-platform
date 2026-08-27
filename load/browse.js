// Сценарий «витрина»: чтение каталога и меню.
//
// Что проверяем: держит ли read-path заданную интенсивность и не растёт ли
// задержка при обходе каталога курсором. Keyset-пагинация должна давать
// одинаковое время на первой и на последней странице — в отличие от OFFSET,
// который дорожает с глубиной.
//
//   k6 run load/browse.js
//   k6 run -e RATE=200 -e DURATION=1m load/browse.js

import { sleep } from 'k6';
import {
  BASE_URL,
  RESTAURANT_SLUG,
  browseCatalog,
  browseMenu,
  expectBusinessStatuses,
  publishMenu,
  setKitchenStatus,
  waitForService,
} from './lib.js';

expectBusinessStatuses();

const RATE = Number(__ENV.RATE || 100);
const DURATION = __ENV.DURATION || '30s';

export const options = {
  scenarios: {
    browse: {
      // Постоянная интенсивность, а не постоянное число VU: измеряем поведение
      // сервиса при заданном RPS, и очередь копится на стороне k6, а не
      // маскируется тем, что виртуальные пользователи ждут ответа.
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: Math.max(10, Math.ceil(RATE / 4)),
      maxVUs: Math.max(50, RATE * 2),
    },
  },
  thresholds: {
    // Чтение идёт по индексам и не открывает транзакций — четверть секунды
    // на девяносто пятом перцентиле уже сигнал, что что-то не так.
    'http_req_duration{name:restaurants}': ['p(95)<250'],
    'http_req_duration{name:menu}': ['p(95)<250'],
    http_req_failed: ['rate<0.01'],
    checks: ['rate>0.99'],
  },
};

export function setup() {
  waitForService();
  setKitchenStatus('online');

  // Меню фиксируем сами: результат не должен зависеть от того, что осталось
  // в базе от предыдущих прогонов.
  publishMenu([
    { product_key: 'load_pizza', category: 'Пицца', name: 'Пицца нагрузочная',
      description: 'Позиция для нагрузочного прогона', price_kopecks: 60000, available: true, stock_qty: null },
    { product_key: 'load_pasta', category: 'Паста', name: 'Паста нагрузочная',
      price_kopecks: 55000, available: true, stock_qty: null },
    { product_key: 'load_drink', category: 'Напитки', name: 'Напиток',
      price_kopecks: 12000, available: true, stock_qty: null },
    { product_key: 'load_hidden', category: 'Десерты', name: 'Снятая с продажи позиция',
      price_kopecks: 30000, available: false, stock_qty: 0 },
  ]);

  return { baseURL: BASE_URL, slug: RESTAURANT_SLUG };
}

export default function () {
  // Три четверти запросов — открыть меню конкретного заведения, остальное —
  // листание каталога: так выглядит поведение человека, а не равномерный обход.
  if (Math.random() < 0.75) {
    browseMenu();
  } else {
    const cursor = browseCatalog(null);
    if (cursor) {
      browseCatalog(cursor);
    }
  }

  sleep(0.1);
}
