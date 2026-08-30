package problem_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"avito-kitchen/internal/domain"
	"avito-kitchen/internal/gen"
	"avito-kitchen/internal/platform/problem"
)

// domainCodes — все коды ошибок домена. Список ведётся вручную и намеренно:
// забыть добавить сюда новый код невозможно, потому что тест ниже сверяет его
// с перечислением из контракта, а оно генерируется из api/openapi.yaml.
func domainCodes() []domain.ErrorCode {
	return []domain.ErrorCode{
		domain.CodeBadRequest,
		domain.CodeValidationError,
		domain.CodeRouteNotFound,
		domain.CodeMethodNotAllowed,
		domain.CodeUnauthorized,
		domain.CodeForbidden,
		domain.CodeRestaurantNotFound,
		domain.CodeMenuNotFound,
		domain.CodeOrderNotFound,
		domain.CodeProductUnavailable,
		domain.CodeOutOfStock,
		domain.CodeMinOrderNotMet,
		domain.CodeRestaurantUnavailable,
		domain.CodeOrderCannotBeCancelled,
		domain.CodeStateConflict,
		domain.CodeIdempotencyKeyRequired,
		domain.CodeIdempotencyPayloadMismatch,
		domain.CodeIdempotencyInProgress,
		domain.CodeServiceUnavailable,
		domain.CodeInternalError,
	}
}

// contractCodes — перечисление ErrorCode, сгенерированное из api/openapi.yaml.
func contractCodes() []gen.ErrorCode {
	return []gen.ErrorCode{
		gen.BADREQUEST,
		gen.VALIDATIONERROR,
		gen.ROUTENOTFOUND,
		gen.METHODNOTALLOWED,
		gen.UNAUTHORIZED,
		gen.FORBIDDEN,
		gen.RESTAURANTNOTFOUND,
		gen.MENUNOTFOUND,
		gen.ORDERNOTFOUND,
		gen.PRODUCTUNAVAILABLE,
		gen.OUTOFSTOCK,
		gen.MINORDERNOTMET,
		gen.RESTAURANTUNAVAILABLE,
		gen.ORDERCANNOTBECANCELLED,
		gen.STATECONFLICT,
		gen.IDEMPOTENCYKEYREQUIRED,
		gen.IDEMPOTENCYPAYLOADMISMATCH,
		gen.IDEMPOTENCYINPROGRESS,
		gen.SERVICEUNAVAILABLE,
		gen.INTERNALERROR,
	}
}

// Домен и контракт обязаны описывать одно и то же множество кодов. Раньше это
// было только обещанием в комментарии; теперь расхождение роняет сборку.
func TestErrorCodes_DomainMatchesContract(t *testing.T) {
	t.Parallel()

	inDomain := make(map[string]struct{}, len(domainCodes()))
	for _, c := range domainCodes() {
		inDomain[string(c)] = struct{}{}
	}

	inContract := make(map[string]struct{}, len(contractCodes()))
	for _, c := range contractCodes() {
		inContract[string(c)] = struct{}{}
	}

	require.Len(t, domainCodes(), len(inDomain), "в списке домена есть дубли")
	require.Len(t, contractCodes(), len(inContract), "в списке контракта есть дубли")

	for code := range inDomain {
		assert.Containsf(t, inContract, code,
			"код %s есть в домене, но отсутствует в api/openapi.yaml", code)
	}
	for code := range inContract {
		assert.Containsf(t, inDomain, code,
			"код %s есть в api/openapi.yaml, но отсутствует в домене", code)
	}
}

// Каждый доменный код обязан иметь осмысленный HTTP-статус. Пропущенный код
// молча превратился бы в 500 — клиент увидел бы «внутреннюю ошибку» вместо
// понятного отказа.
func TestStatusFor_EveryCodeIsMapped(t *testing.T) {
	t.Parallel()

	for _, code := range domainCodes() {
		if code == domain.CodeInternalError {
			continue // единственный код, для которого 500 — правильный ответ
		}

		status := problem.StatusFor(code)

		assert.NotEqualf(t, http.StatusInternalServerError, status,
			"код %s не описан в карте статусов и падает в 500 по умолчанию", code)
		assert.GreaterOrEqualf(t, status, 400, "код %s: статус должен быть ошибочным", code)
		assert.Lessf(t, status, 600, "код %s: недопустимый статус", code)
	}

	// Единственное исключение — сам INTERNAL_ERROR.
	assert.Equal(t, http.StatusInternalServerError, problem.StatusFor(domain.CodeInternalError))
	// Неизвестный код тоже обязан давать 500, а не ноль.
	assert.Equal(t, http.StatusInternalServerError, problem.StatusFor(domain.ErrorCode("НЕТ_ТАКОГО")))
}

// Разделение 409 и 422 несёт смысл: 409 — состояние каталога изменилось,
// повтор с другой корзиной осмыслен; 422 — запрос корректен, но нарушает
// бизнес-правило.
func TestStatusFor_SemanticGrouping(t *testing.T) {
	t.Parallel()

	conflict := []domain.ErrorCode{
		domain.CodeOutOfStock,
		domain.CodeProductUnavailable,
		domain.CodeStateConflict,
		domain.CodeOrderCannotBeCancelled,
		domain.CodeIdempotencyInProgress,
	}
	for _, code := range conflict {
		assert.Equalf(t, http.StatusConflict, problem.StatusFor(code), "код %s", code)
	}

	unprocessable := []domain.ErrorCode{
		domain.CodeMinOrderNotMet,
		domain.CodeRestaurantUnavailable,
		domain.CodeValidationError,
		domain.CodeIdempotencyPayloadMismatch,
	}
	for _, code := range unprocessable {
		assert.Equalf(t, http.StatusUnprocessableEntity, problem.StatusFor(code), "код %s", code)
	}

	notFound := []domain.ErrorCode{
		domain.CodeOrderNotFound,
		domain.CodeRestaurantNotFound,
		domain.CodeMenuNotFound,
		domain.CodeRouteNotFound,
	}
	for _, code := range notFound {
		assert.Equalf(t, http.StatusNotFound, problem.StatusFor(code), "код %s", code)
	}
}

// Детали не-доменных ошибок наружу не выносятся: клиент видит нейтральный
// текст, подробности остаются в логах.
func TestFromError_HidesInternalDetails(t *testing.T) {
	t.Parallel()

	leaky := assertAnError{}
	p := problem.FromError(t.Context(), leaky, "/api/v1/orders")

	assert.Equal(t, http.StatusInternalServerError, p.Status)
	assert.Equal(t, string(domain.CodeInternalError), p.Code)
	assert.NotContains(t, p.Detail, "pq: relation")
	assert.Equal(t, "внутренняя ошибка сервиса", p.Detail)
	assert.Equal(t, "/api/v1/orders", p.Instance)
}

func TestFromError_KeepsDomainDetail(t *testing.T) {
	t.Parallel()

	err := domain.Errorf(domain.CodeOutOfStock, "осталось %d, а запрошено %d", 5, 12)
	p := problem.FromError(t.Context(), err, "/api/v1/orders")

	assert.Equal(t, http.StatusConflict, p.Status)
	assert.Equal(t, "OUT_OF_STOCK", p.Code)
	assert.Contains(t, p.Detail, "осталось 5, а запрошено 12")
	assert.Contains(t, p.Type, "OUT_OF_STOCK")
}

type assertAnError struct{}

func (assertAnError) Error() string { return "pq: relation \"orders\" does not exist" }
