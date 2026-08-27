// Package problem формирует ответы об ошибках в формате RFC 7807
// (application/problem+json).
//
// Формат выбран вместо самодельного {"error": "..."} по двум причинам: он
// стандартизирован (клиенты и прокси умеют его разбирать) и разделяет
// человекочитаемое описание и машиночитаемый код, по которому веб-клиент
// принимает решения.
package problem

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"avito-kitchen/internal/domain"
	"avito-kitchen/internal/platform/logger"
)

// ContentType — медиа-тип ответов об ошибках.
const ContentType = "application/problem+json"

// typePrefix — база для поля type: ссылка на описание конкретного кода ошибки.
const typePrefix = "https://avito.ru/kitchen/errors/"

// Problem — тело ответа об ошибке. Поля совпадают со схемой Problem в
// api/openapi.yaml.
type Problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Code      string `json:"code"`
	Detail    string `json:"detail,omitempty"`
	Instance  string `json:"instance,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// statusByCode — единственная точка сопоставления доменных кодов с HTTP.
//
// Маппинг живёт в транспортном слое: домен не должен знать про HTTP, а вот
// транспорт обязан отвечать на «недостаточно остатка» именно 409, а не 500.
var statusByCode = map[domain.ErrorCode]int{
	domain.CodeBadRequest:       http.StatusBadRequest,
	domain.CodeValidationError:  http.StatusUnprocessableEntity,
	domain.CodeRouteNotFound:    http.StatusNotFound,
	domain.CodeMethodNotAllowed: http.StatusMethodNotAllowed,
	domain.CodeUnauthorized:     http.StatusUnauthorized,
	domain.CodeForbidden:        http.StatusForbidden,

	domain.CodeRestaurantNotFound: http.StatusNotFound,
	domain.CodeMenuNotFound:       http.StatusNotFound,
	domain.CodeOrderNotFound:      http.StatusNotFound,

	// 409 — состояние каталога изменилось; повтор с другой корзиной имеет смысл.
	domain.CodeProductUnavailable: http.StatusConflict,
	domain.CodeOutOfStock:         http.StatusConflict,

	// 422 — запрос корректен синтаксически, но нарушает бизнес-правило.
	domain.CodeMinOrderNotMet:        http.StatusUnprocessableEntity,
	domain.CodeRestaurantUnavailable: http.StatusUnprocessableEntity,

	domain.CodeOrderCannotBeCancelled: http.StatusConflict,
	domain.CodeStateConflict:          http.StatusConflict,

	domain.CodeIdempotencyKeyRequired:     http.StatusBadRequest,
	domain.CodeIdempotencyPayloadMismatch: http.StatusUnprocessableEntity,
	domain.CodeIdempotencyInProgress:      http.StatusConflict,

	domain.CodeServiceUnavailable: http.StatusServiceUnavailable,
	domain.CodeInternalError:      http.StatusInternalServerError,
}

// StatusFor возвращает HTTP-статус для доменного кода ошибки.
func StatusFor(code domain.ErrorCode) int {
	if status, ok := statusByCode[code]; ok {
		return status
	}
	return http.StatusInternalServerError
}

// FromError строит Problem по ошибке.
//
// Детали не-доменных ошибок наружу не выносятся: клиенту достаточно знать, что
// это внутренний сбой, а подробности остаются в логах.
func FromError(ctx context.Context, err error, instance string) Problem {
	code := domain.CodeOf(err)
	status := StatusFor(code)

	detail := "внутренняя ошибка сервиса"
	var domainErr *domain.Error
	if errors.As(err, &domainErr) {
		detail = domainErr.Detail
	}

	return Problem{
		Type:      typePrefix + string(code),
		Title:     http.StatusText(status),
		Status:    status,
		Code:      string(code),
		Detail:    detail,
		Instance:  instance,
		RequestID: logger.RequestIDFromContext(ctx),
	}
}

// Write отправляет ответ об ошибке клиенту.
func Write(w http.ResponseWriter, r *http.Request, err error, log *slog.Logger) {
	ctx := r.Context()
	p := FromError(ctx, err, r.URL.Path)

	if log != nil {
		attrs := []any{
			slog.String("code", p.Code),
			slog.Int("status", p.Status),
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Any("error", err),
		}
		// Ошибки клиента — часть штатной работы API и не должны шуметь на
		// уровне error; серверные — наоборот, требуют внимания.
		if p.Status >= http.StatusInternalServerError {
			log.ErrorContext(ctx, "запрос завершился ошибкой сервиса", attrs...)
		} else {
			log.InfoContext(ctx, "запрос отклонён", attrs...)
		}
	}

	WriteProblem(w, p)
}

// WriteProblem отправляет уже собранный Problem.
func WriteProblem(w http.ResponseWriter, p Problem) {
	w.Header().Set("Content-Type", ContentType)
	w.WriteHeader(p.Status)

	// Ошибку кодирования логировать некуда: заголовки уже отправлены.
	_ = json.NewEncoder(w).Encode(p)
}

// WriteCode отправляет ответ по доменному коду и описанию.
func WriteCode(w http.ResponseWriter, r *http.Request, code domain.ErrorCode, detail string) {
	WriteProblem(w, Problem{
		Type:      typePrefix + string(code),
		Title:     http.StatusText(StatusFor(code)),
		Status:    StatusFor(code),
		Code:      string(code),
		Detail:    detail,
		Instance:  r.URL.Path,
		RequestID: logger.RequestIDFromContext(r.Context()),
	})
}
