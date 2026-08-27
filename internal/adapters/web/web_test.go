package web_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"avito-kitchen/internal/adapters/web"
	"avito-kitchen/internal/app"
	"avito-kitchen/internal/app/apptest"
	"avito-kitchen/internal/domain"
)

// Тесты веб-слоя проверяют транспортные гарантии, а не бизнес-правила:
// доступ к панели заведения, перевод доменных ошибок в понятные страницы с
// верным статусом и идемпотентность формы заказа. Сама логика заказов
// покрыта тестами internal/app и internal/adapters/postgres.

// demoToken — токен заведения pizza-avito в фикстуре.
const demoToken = apptest.PartnerToken

func newTestServer(t *testing.T) (http.Handler, *apptest.Env) {
	t.Helper()

	env := apptest.New()

	handler, err := web.New(web.Config{
		Catalog:        env.Catalog,
		Orders:         env.Orders,
		Partner:        env.Partners,
		Idempotency:    env.Idempotency,
		IdempotencyTTL: time.Hour,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	require.NoError(t, err)

	router := chi.NewRouter()
	handler.Mount(router)

	return router, env
}

// do выполняет запрос без следования редиректам: нас интересует сам ответ.
func do(t *testing.T, h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func postForm(t *testing.T, h http.Handler, path string, form url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range cookies {
		req.AddCookie(c)
	}

	return do(t, h, req)
}

// --- Каталог и меню ---------------------------------------------------------

func TestRestaurantsPage_RendersCatalog(t *testing.T) {
	t.Parallel()
	h, _ := newTestServer(t)

	rec := do(t, h, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/restaurants", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()

	// Пустая страница — тот самый дефект, ради которого написан этот тест:
	// шаблон, вызванный по неверному имени, отдаёт 200 и пустое тело.
	require.Greater(t, len(body), 500, "страница не должна быть пустой")
	assert.Contains(t, body, "Пиццерия Авито")
	assert.Contains(t, body, "Суши Авито")
	assert.Contains(t, body, "<!DOCTYPE html>")
}

func TestRestaurantsPage_HTMXReturnsFragment(t *testing.T) {
	t.Parallel()
	h, _ := newTestServer(t)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/restaurants", nil)
	req.Header.Set("HX-Request", "true")
	rec := do(t, h, req)

	require.Equal(t, http.StatusOK, rec.Code)
	// Фрагмент вставляется внутрь страницы — целый документ там недопустим.
	assert.NotContains(t, rec.Body.String(), "<!DOCTYPE html>")
	assert.Contains(t, rec.Body.String(), "Пиццерия Авито")
}

func TestMenuPage_ShowsUnavailableProducts(t *testing.T) {
	t.Parallel()
	h, _ := newTestServer(t)

	rec := do(t, h, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/restaurants/pizza-avito", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Пицца Маргарита")
	assert.Contains(t, body, "Тирамису", "недоступное блюдо остаётся на витрине")
	assert.Contains(t, body, "Нет в наличии")
	assert.Contains(t, body, `name="qty_pizza_margherita"`)
	assert.Contains(t, body, `name="idempotency_key"`)
}

func TestMenuPage_UnknownRestaurant(t *testing.T) {
	t.Parallel()
	h, _ := newTestServer(t)

	rec := do(t, h, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/restaurants/unknown", nil))

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "RESTAURANT_NOT_FOUND")
}

// --- Оформление заказа ------------------------------------------------------

func orderForm(key string, qty string) url.Values {
	return url.Values{
		"restaurant_id":        {"1"},
		"delivery_address":     {"ул. Ленина, 10"},
		"idempotency_key":      {key},
		"qty_pizza_margherita": {qty},
	}
}

func TestCreateOrder_RedirectsToOrder(t *testing.T) {
	t.Parallel()
	h, env := newTestServer(t)

	rec := postForm(t, h, "/orders", orderForm(uuid.NewString(), "2"))

	require.Equal(t, http.StatusSeeOther, rec.Code, "Post/Redirect/Get")
	location := rec.Header().Get("Location")
	assert.True(t, strings.HasPrefix(location, "/orders/"), "редирект на карточку заказа: %s", location)
	assert.Len(t, env.State().Orders, 1)
}

// Форма отправлена дважды — заказ должен остаться один.
func TestCreateOrder_DoubleSubmitCreatesOneOrder(t *testing.T) {
	t.Parallel()
	h, env := newTestServer(t)

	key := uuid.NewString()

	first := postForm(t, h, "/orders", orderForm(key, "2"))
	require.Equal(t, http.StatusSeeOther, first.Code)

	second := postForm(t, h, "/orders", orderForm(key, "2"))
	require.Equal(t, http.StatusSeeOther, second.Code)

	assert.Equal(t, first.Header().Get("Location"), second.Header().Get("Location"),
		"повтор ведёт на тот же заказ")
	assert.Len(t, env.State().Orders, 1, "второй заказ не создан")
}

// Тот же ключ с другой корзиной — ошибка, а не тихий повтор.
func TestCreateOrder_SameKeyDifferentCart(t *testing.T) {
	t.Parallel()
	h, _ := newTestServer(t)

	key := uuid.NewString()
	require.Equal(t, http.StatusSeeOther, postForm(t, h, "/orders", orderForm(key, "2")).Code)

	rec := postForm(t, h, "/orders", orderForm(key, "3"))

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, rec.Body.String(), "IDEMPOTENCY_PAYLOAD_MISMATCH")
}

// Доменная ошибка должна доходить до экрана понятным текстом и верным статусом,
// а не превращаться в «внутреннюю ошибку сервиса».
func TestCreateOrder_DomainErrorsReachTheUser(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		form       url.Values
		wantStatus int
		wantCode   string
		wantText   string
	}{
		{
			name: "не хватает остатка",
			form: url.Values{
				"restaurant_id": {"1"}, "delivery_address": {"ул. Ленина, 10"},
				"idempotency_key": {uuid.NewString()}, "qty_pizza_margherita": {"99"},
			},
			wantStatus: http.StatusConflict,
			wantCode:   "OUT_OF_STOCK",
			wantText:   "осталось",
		},
		{
			name: "не набрана минимальная сумма",
			form: url.Values{
				"restaurant_id": {"1"}, "delivery_address": {"ул. Ленина, 10"},
				"idempotency_key": {uuid.NewString()}, "qty_drink_cola": {"1"},
			},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   "MIN_ORDER_NOT_MET",
			wantText:   "минимальная сумма",
		},
		{
			name: "блюдо снято с продажи",
			form: url.Values{
				"restaurant_id": {"1"}, "delivery_address": {"ул. Ленина, 10"},
				"idempotency_key": {uuid.NewString()}, "qty_dessert_tiramisu": {"1"},
			},
			wantStatus: http.StatusConflict,
			wantCode:   "PRODUCT_UNAVAILABLE",
			wantText:   "недоступна",
		},
		{
			name: "пустая корзина",
			form: url.Values{
				"restaurant_id": {"1"}, "delivery_address": {"ул. Ленина, 10"},
				"idempotency_key": {uuid.NewString()}, "qty_pizza_margherita": {"0"},
			},
			wantStatus: http.StatusBadRequest,
			wantCode:   "BAD_REQUEST",
			wantText:   "корзина пуста",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, _ := newTestServer(t)

			rec := postForm(t, h, "/orders", tt.form)

			assert.Equal(t, tt.wantStatus, rec.Code)
			body := rec.Body.String()
			assert.Contains(t, body, tt.wantCode)
			assert.Contains(t, strings.ToLower(body), strings.ToLower(tt.wantText))
			assert.NotContains(t, body, "внутренняя ошибка",
				"доменная ошибка не должна выглядеть как сбой сервиса")
		})
	}
}

// Корзина собирается из полей qty_<артикул>, поэтому заказ оформляется и без
// JavaScript. Позиции с нулевым количеством отбрасываются.
func TestCreateOrder_CartFromFormWithoutJS(t *testing.T) {
	t.Parallel()
	h, env := newTestServer(t)

	form := url.Values{
		"restaurant_id":        {"1"},
		"delivery_address":     {"ул. Ленина, 10"},
		"idempotency_key":      {uuid.NewString()},
		"qty_pizza_margherita": {"2"},
		"qty_pasta_carbonara":  {"1"},
		"qty_drink_cola":       {"0"},
	}

	require.Equal(t, http.StatusSeeOther, postForm(t, h, "/orders", form).Code)

	require.Len(t, env.State().Orders, 1)
	for _, order := range env.State().Orders {
		require.Len(t, order.Items, 2, "позиция с нулевым количеством не попадает в заказ")
	}
}

// --- Отмена заказа ----------------------------------------------------------

func TestCancelOrder_TooLateShowsFriendlyPage(t *testing.T) {
	t.Parallel()
	h, env := newTestServer(t)

	order, err := env.Orders.Create(context.Background(), domain.OrderDraft{
		UserExternalID:  "web_guest",
		RestaurantID:    1,
		DeliveryAddress: "ул. Ленина, 10",
		Items:           []domain.DraftItem{{ProductKey: "pizza_margherita", Qty: 2}},
	})
	require.NoError(t, err)

	_, err = env.Orders.ChangeStatus(context.Background(), app.StatusChange{
		PublicNumber: order.PublicNumber,
		To:           domain.StatusAccepted,
		Actor:        domain.ActorRestaurant,
		RestaurantID: 1,
	})
	require.NoError(t, err)

	rec := postForm(t, h, "/orders/"+order.PublicNumber.String()+"/cancel",
		url.Values{"reason": {"Передумал"}})

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "ORDER_CANNOT_BE_CANCELLED")
}

func TestOrderPage_UnknownOrder(t *testing.T) {
	t.Parallel()
	h, _ := newTestServer(t)

	rec := do(t, h, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/orders/"+uuid.NewString(), nil))

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "ORDER_NOT_FOUND")
}

func TestOrderPage_MalformedNumber(t *testing.T) {
	t.Parallel()
	h, _ := newTestServer(t)

	rec := do(t, h, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/orders/not-a-uuid", nil))

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// --- Доступ к панели заведения ---------------------------------------------

// Ключевая проверка безопасности: без входа панель недоступна, а изменяющие
// операции не выполняются.
func TestPartnerPanel_RequiresLogin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{"панель", http.MethodGet, "/partner"},
		{"очередь заказов", http.MethodGet, "/partner/orders"},
		{"режим кухни", http.MethodPost, "/partner/kitchen-status"},
		{"смена статуса заказа", http.MethodPost, "/partner/orders/" + uuid.NewString() + "/status"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, env := newTestServer(t)

			var rec *httptest.ResponseRecorder
			if tt.method == http.MethodGet {
				rec = do(t, h, httptest.NewRequestWithContext(t.Context(), tt.method, tt.path, nil))
			} else {
				rec = postForm(t, h, tt.path, url.Values{"status": {"closed"}})
			}

			assert.Equal(t, http.StatusSeeOther, rec.Code)
			assert.Equal(t, "/partner/login", rec.Header().Get("Location"))

			// И главное — состояние не изменилось.
			assert.Equal(t, domain.RestaurantOnline, env.State().Restaurants[1].Status,
				"неаутентифицированный запрос не должен менять режим кухни")
		})
	}
}

func TestPartnerLogin_RejectsWrongToken(t *testing.T) {
	t.Parallel()
	h, _ := newTestServer(t)

	rec := postForm(t, h, "/partner/login", url.Values{"token": {"не-тот-токен"}})

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "Токен не распознан")
	assert.Empty(t, rec.Result().Cookies(), "сессия не открывается")
}

func TestPartnerLogin_SetsHardenedCookie(t *testing.T) {
	t.Parallel()
	h, _ := newTestServer(t)

	rec := postForm(t, h, "/partner/login", url.Values{"token": {demoToken}})

	require.Equal(t, http.StatusSeeOther, rec.Code)
	require.Equal(t, "/partner", rec.Header().Get("Location"))

	cookies := rec.Result().Cookies()
	require.Len(t, cookies, 1)

	c := cookies[0]
	assert.True(t, c.HttpOnly, "cookie недоступна из JavaScript")
	assert.Equal(t, http.SameSiteLaxMode, c.SameSite, "SameSite=Lax закрывает межсайтовый POST")
	assert.Equal(t, "/partner", c.Path, "cookie не уходит с запросами клиентской части")
}

func partnerCookie(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()

	rec := postForm(t, h, "/partner/login", url.Values{"token": {demoToken}})
	require.Equal(t, http.StatusSeeOther, rec.Code)
	require.Len(t, rec.Result().Cookies(), 1)

	return rec.Result().Cookies()[0]
}

func TestPartnerPanel_ShowsOwnRestaurantOnly(t *testing.T) {
	t.Parallel()
	h, _ := newTestServer(t)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/partner", nil)
	req.AddCookie(partnerCookie(t, h))
	rec := do(t, h, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "Пиццерия Авито")
	assert.NotContains(t, body, "Суши Авито", "чужое заведение в панели не показывается")
	assert.NotContains(t, body, `name="restaurant_id"`,
		"идентификатор заведения не передаётся формой — он берётся из сессии")
}

// Даже с валидной сессией нельзя тронуть заказ чужого заведения: проверка
// владения выполняется в use case по restaurant_id из сессии.
func TestPartnerOrderStatus_ForeignOrderForbidden(t *testing.T) {
	t.Parallel()
	h, env := newTestServer(t)

	// «Суши Авито» в фикстуре на паузе — открываем, чтобы оформить там заказ.
	_, err := env.Partners.SetKitchenStatus(context.Background(), 2, domain.RestaurantOnline)
	require.NoError(t, err)

	foreign, err := env.Orders.Create(context.Background(), domain.OrderDraft{
		UserExternalID:  "web_guest",
		RestaurantID:    2, // заведение «Суши Авито»
		DeliveryAddress: "ул. Ленина, 10",
		Items:           []domain.DraftItem{{ProductKey: "roll_philadelphia", Qty: 2}},
	})
	require.NoError(t, err)

	rec := postForm(t, h, "/partner/orders/"+foreign.PublicNumber.String()+"/status",
		url.Values{"status": {"ACCEPTED"}}, partnerCookie(t, h))

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "FORBIDDEN")

	unchanged := env.State().Orders[foreign.PublicNumber]
	assert.Equal(t, domain.StatusNew, unchanged.Status, "чужой заказ не сдвинулся")
}

func TestPartnerKitchenStatus_UsesSessionRestaurant(t *testing.T) {
	t.Parallel()
	h, env := newTestServer(t)

	// В форме намеренно передан чужой restaurant_id — он должен быть проигнорирован.
	rec := postForm(t, h, "/partner/kitchen-status",
		url.Values{"status": {"closed"}, "restaurant_id": {"2"}}, partnerCookie(t, h))

	require.Equal(t, http.StatusSeeOther, rec.Code)
	assert.Equal(t, domain.RestaurantClosed, env.State().Restaurants[1].Status,
		"закрылось заведение из сессии")
	assert.Equal(t, domain.RestaurantPaused, env.State().Restaurants[2].Status,
		"чужое заведение не тронуто, хотя его id был в форме")
}

func TestPartnerOrderStatus_InvalidTransition(t *testing.T) {
	t.Parallel()
	h, env := newTestServer(t)

	order, err := env.Orders.Create(context.Background(), domain.OrderDraft{
		UserExternalID:  "web_guest",
		RestaurantID:    1,
		DeliveryAddress: "ул. Ленина, 10",
		Items:           []domain.DraftItem{{ProductKey: "pizza_margherita", Qty: 2}},
	})
	require.NoError(t, err)

	// NEW → READY через голову конвейера.
	rec := postForm(t, h, "/partner/orders/"+order.PublicNumber.String()+"/status",
		url.Values{"status": {"READY"}}, partnerCookie(t, h))

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "STATE_CONFLICT")
}

func TestPartnerLogout_ClearsSession(t *testing.T) {
	t.Parallel()
	h, _ := newTestServer(t)

	cookie := partnerCookie(t, h)

	rec := postForm(t, h, "/partner/logout", url.Values{}, cookie)
	require.Equal(t, http.StatusSeeOther, rec.Code)

	cleared := rec.Result().Cookies()
	require.Len(t, cleared, 1)
	assert.Negative(t, cleared[0].MaxAge, "cookie удаляется")
}

// Панель под htmx не разворачивается редиректом: фрагмент подставился бы
// внутрь страницы вместо перехода на вход.
func TestPartnerPanel_HTMXRedirectsViaHeader(t *testing.T) {
	t.Parallel()
	h, _ := newTestServer(t)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/partner/orders", nil)
	req.Header.Set("HX-Request", "true")
	rec := do(t, h, req)

	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "/partner/login", rec.Header().Get("HX-Redirect"))
}
