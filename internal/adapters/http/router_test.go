package http_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	kitchenhttp "avito-kitchen/internal/adapters/http"
	"avito-kitchen/internal/app/apptest"
)

// Тесты маршрутизации: коды ответов на транспортные ситуации и поведение проб.
//
// Поводом послужил реальный инцидент: healthcheck контейнера ходил
// `wget --spider`, то есть методом HEAD; chi отвечал 405, контейнер api
// оставался unhealthy, и restaurant-sim, зависящий от service_healthy,
// не стартовал вовсе. Снаружи при этом всё выглядело рабочим.

// stubPinger изображает доступную или упавшую БД.
type stubPinger struct{ err error }

func (p stubPinger) Ping(context.Context) error { return p.err }

func newRouter(t *testing.T, pinger kitchenhttp.Pinger) http.Handler {
	t.Helper()

	env := apptest.New()

	server := kitchenhttp.NewServer(
		kitchenhttp.NewClientHandler(env.Catalog, env.Orders),
		kitchenhttp.NewPartnerHandler(env.Partners, env.Orders),
		kitchenhttp.NewHealthHandler(pinger, "test"),
	)

	return kitchenhttp.NewRouter(kitchenhttp.RouterConfig{
		Server:         server,
		PartnerAuth:    env.Partners,
		Idempotency:    env.Idempotency,
		IdempotencyTTL: time.Hour,
		RequestTimeout: 5 * time.Second,
		CORSOrigins:    []string{"*"},
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func request(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, path, nil))

	return rec
}

// Пробы обязаны отвечать и на HEAD: так ходят `wget --spider` и часть
// балансировщиков, а RFC 9110 требует HEAD везде, где работает GET.
func TestProbes_AnswerGetAndHead(t *testing.T) {
	t.Parallel()

	router := newRouter(t, stubPinger{})

	for _, path := range []string{"/health", "/readyz"} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(method+" "+path, func(t *testing.T) {
				t.Parallel()

				rec := request(t, router, method, path)
				assert.Equal(t, http.StatusOK, rec.Code)
			})
		}
	}
}

func TestProbes_GetReturnsBody(t *testing.T) {
	t.Parallel()

	rec := request(t, newRouter(t, stubPinger{}), http.MethodGet, "/readyz")
	require.Equal(t, http.StatusOK, rec.Code)

	var body struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "ready", body.Status)
	assert.Equal(t, "test", body.Version)
}

// Готовность обязана падать вместе с БД, иначе оркестратор будет слать трафик
// в экземпляр, который не может обслужить ни один запрос.
func TestReadiness_FailsWhenDatabaseIsDown(t *testing.T) {
	t.Parallel()

	router := newRouter(t, stubPinger{err: assert.AnError})

	rec := request(t, router, http.MethodGet, "/readyz")

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), "SERVICE_UNAVAILABLE")
}

// А живость — не должна: перезапускать рабочий процесс из-за недоступной базы
// бессмысленно, он поднимется в том же состоянии.
func TestHealth_StaysUpWhenDatabaseIsDown(t *testing.T) {
	t.Parallel()

	router := newRouter(t, stubPinger{err: assert.AnError})

	rec := request(t, router, http.MethodGet, "/health")

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestRouter_UnknownRouteIsNotFound(t *testing.T) {
	t.Parallel()

	rec := request(t, newRouter(t, stubPinger{}), http.MethodGet, "/api/v1/nope")

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "ROUTE_NOT_FOUND")
	assert.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))
}

func TestRouter_WrongMethodIsMethodNotAllowed(t *testing.T) {
	t.Parallel()

	// Каталог существует, но только на чтение.
	rec := request(t, newRouter(t, stubPinger{}), http.MethodDelete, "/api/v1/restaurants")

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
	assert.Contains(t, rec.Body.String(), "METHOD_NOT_ALLOWED")
}

// Партнёрский контур закрыт прослойкой, а клиентский — открыт.
func TestRouter_PartnerRoutesRequireToken(t *testing.T) {
	t.Parallel()

	router := newRouter(t, stubPinger{})

	rec := request(t, router, http.MethodGet, "/api/v1/partner/orders")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "UNAUTHORIZED")

	open := request(t, router, http.MethodGet, "/api/v1/restaurants")
	assert.Equal(t, http.StatusOK, open.Code)
}

// Каждый ответ несёт идентификатор запроса — по нему ошибка ищется в логах.
func TestRouter_SetsRequestID(t *testing.T) {
	t.Parallel()

	rec := request(t, newRouter(t, stubPinger{}), http.MethodGet, "/api/v1/restaurants")

	assert.NotEmpty(t, rec.Header().Get("X-Request-Id"))
}
