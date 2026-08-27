// Общие помощники нагрузочных сценариев.
//
// Внешних зависимостей нет намеренно: ни одного импорта с jslib.k6.io, чтобы
// прогон не зависел от доступа в интернет и от чужого CDN.

import http from 'k6/http';
import { check, fail } from 'k6';

// --- Конфигурация -----------------------------------------------------------

export const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
export const PARTNER_TOKEN = __ENV.PARTNER_TOKEN || 'dev-partner-token';
export const RESTAURANT_SLUG = __ENV.RESTAURANT_SLUG || 'pizza-avito';

const JSON_HEADERS = { 'Content-Type': 'application/json' };
const PARTNER_HEADERS = { ...JSON_HEADERS, 'X-Partner-Token': PARTNER_TOKEN };

// Статусы, которые считаются штатным ответом сервиса, а не сбоем.
//
// 409 и 422 — это корректные бизнес-ответы («кончился остаток», «не набрана
// минимальная сумма»), и под нагрузкой их доля как раз и интересна. Без этой
// настройки k6 записал бы их в http_req_failed и метрика ошибок перестала бы
// отличать отказ сервиса от штатного отказа в заказе.
export function expectBusinessStatuses() {
  http.setResponseCallback(http.expectedStatuses(200, 201, 204, 409, 422));
}

// --- Утилиты ----------------------------------------------------------------

// idempotencyKey строит уникальный ключ операции.
//
// Свой генератор вместо uuidv4 из jslib: контракт требует лишь 8..128 символов,
// а внешний импорт добавил бы сетевую зависимость к запуску теста.
export function idempotencyKey(prefix) {
  return `${prefix}-${__VU}-${__ITER}-${Date.now()}-${Math.floor(Math.random() * 1e9)}`;
}

export function jsonOrNull(response) {
  try {
    return response.json();
  } catch (_) {
    return null;
  }
}

// --- Подготовка окружения (вызывается из setup) -----------------------------

export function waitForService(timeoutSeconds = 60) {
  const deadline = Date.now() + timeoutSeconds * 1000;

  while (Date.now() < deadline) {
    const response = http.get(`${BASE_URL}/readyz`, { tags: { name: 'readyz' } });
    if (response.status === 200) {
      return;
    }
  }

  fail(`сервис ${BASE_URL} не поднялся за ${timeoutSeconds}s`);
}

// findRestaurant возвращает заведение из каталога по slug.
export function findRestaurant(slug = RESTAURANT_SLUG) {
  const response = http.get(`${BASE_URL}/api/v1/restaurants?limit=100`, {
    tags: { name: 'restaurants' },
  });

  if (response.status !== 200) {
    fail(`каталог недоступен: HTTP ${response.status}`);
  }

  const found = (jsonOrNull(response)?.items || []).find((r) => r.slug === slug);
  if (!found) {
    fail(`заведение "${slug}" не найдено в каталоге`);
  }

  return found;
}

// publishMenu публикует меню через партнёрское API.
//
// Нагрузочный сценарий сам готовит себе каталог: остатки и цены должны быть
// известны точно, иначе результат зависит от того, что осталось в базе от
// предыдущих прогонов.
export function publishMenu(products) {
  const response = http.post(
    `${BASE_URL}/api/v1/partner/menu/sync`,
    JSON.stringify({ products }),
    { headers: PARTNER_HEADERS, tags: { name: 'menu-sync' } },
  );

  if (response.status !== 200) {
    fail(`не удалось опубликовать меню: HTTP ${response.status} ${response.body}`);
  }

  return jsonOrNull(response);
}

export function setKitchenStatus(status) {
  const response = http.post(
    `${BASE_URL}/api/v1/partner/kitchen-status`,
    JSON.stringify({ status }),
    { headers: PARTNER_HEADERS, tags: { name: 'kitchen-status' } },
  );

  if (response.status !== 200) {
    fail(`не удалось переключить кухню в "${status}": HTTP ${response.status}`);
  }
}

// remainingStock читает текущий остаток позиции с витрины.
export function remainingStock(productKey, slug = RESTAURANT_SLUG) {
  const response = http.get(`${BASE_URL}/api/v1/restaurants/${slug}/menu`, {
    tags: { name: 'menu' },
  });

  if (response.status !== 200) {
    return null;
  }

  for (const category of jsonOrNull(response)?.categories || []) {
    for (const product of category.products) {
      if (product.product_key === productKey) {
        return product.stock_qty;
      }
    }
  }

  return null;
}

// --- Операции под нагрузкой -------------------------------------------------

export function browseCatalog(cursor) {
  const url = cursor
    ? `${BASE_URL}/api/v1/restaurants?limit=20&cursor=${encodeURIComponent(cursor)}`
    : `${BASE_URL}/api/v1/restaurants?limit=20`;

  const response = http.get(url, { tags: { name: 'restaurants' } });

  check(response, {
    'каталог отдан': (r) => r.status === 200,
    'каталог не пуст': (r) => (jsonOrNull(r)?.items || []).length > 0,
  });

  return jsonOrNull(response)?.next_cursor || null;
}

export function browseMenu(slug = RESTAURANT_SLUG) {
  const response = http.get(`${BASE_URL}/api/v1/restaurants/${slug}/menu`, {
    tags: { name: 'menu' },
  });

  check(response, {
    'меню отдано': (r) => r.status === 200,
    'меню содержит категории': (r) => (jsonOrNull(r)?.categories || []).length > 0,
  });

  return response;
}

// createOrder оформляет заказ и возвращает { status, code, order }.
export function createOrder({ restaurantId, items, address = 'ул. Ленина, 10', keyPrefix = 'k6' }) {
  const response = http.post(
    `${BASE_URL}/api/v1/orders`,
    JSON.stringify({
      user_external_id: `k6_user_${__VU}`,
      restaurant_id: restaurantId,
      delivery_address: address,
      items,
    }),
    {
      headers: { ...JSON_HEADERS, 'Idempotency-Key': idempotencyKey(keyPrefix) },
      tags: { name: 'create-order' },
    },
  );

  const body = jsonOrNull(response);

  return {
    status: response.status,
    code: body?.code || null,
    order: response.status === 201 ? body : null,
    response,
  };
}

export function fetchOrder(publicNumber) {
  const response = http.get(`${BASE_URL}/api/v1/orders/${publicNumber}`, {
    tags: { name: 'order' },
  });

  check(response, { 'заказ читается': (r) => r.status === 200 });

  return response;
}
