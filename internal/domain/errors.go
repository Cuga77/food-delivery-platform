package domain

import (
	"errors"
	"fmt"
)

// ErrorCode — машиночитаемый код ошибки. Значения совпадают с перечислением
// ErrorCode в api/openapi.yaml: транспорт отдаёт их клиенту как есть, поэтому
// список здесь и в контракте обязан не расходиться.
type ErrorCode string

const (
	CodeBadRequest      ErrorCode = "BAD_REQUEST"
	CodeValidationError ErrorCode = "VALIDATION_ERROR"
	CodeUnauthorized    ErrorCode = "UNAUTHORIZED"
	CodeForbidden       ErrorCode = "FORBIDDEN"

	CodeRestaurantNotFound ErrorCode = "RESTAURANT_NOT_FOUND"
	CodeMenuNotFound       ErrorCode = "MENU_NOT_FOUND"
	CodeOrderNotFound      ErrorCode = "ORDER_NOT_FOUND"

	CodeProductUnavailable    ErrorCode = "PRODUCT_UNAVAILABLE"
	CodeOutOfStock            ErrorCode = "OUT_OF_STOCK"
	CodeMinOrderNotMet        ErrorCode = "MIN_ORDER_NOT_MET"
	CodeRestaurantUnavailable ErrorCode = "RESTAURANT_UNAVAILABLE"

	CodeOrderCannotBeCancelled ErrorCode = "ORDER_CANNOT_BE_CANCELLED"
	CodeStateConflict          ErrorCode = "STATE_CONFLICT"

	CodeIdempotencyKeyRequired     ErrorCode = "IDEMPOTENCY_KEY_REQUIRED"
	CodeIdempotencyPayloadMismatch ErrorCode = "IDEMPOTENCY_PAYLOAD_MISMATCH"
	CodeIdempotencyInProgress      ErrorCode = "IDEMPOTENCY_IN_PROGRESS"

	CodeServiceUnavailable ErrorCode = "SERVICE_UNAVAILABLE"
	CodeInternalError      ErrorCode = "INTERNAL_ERROR"
)

// Error — доменная ошибка с кодом. Транспортный слой разбирает её через
// errors.As и превращает в RFC 7807 problem+json.
type Error struct {
	Code   ErrorCode
	Detail string
	cause  error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Detail, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Detail)
}

func (e *Error) Unwrap() error { return e.cause }

// Is позволяет сравнивать ошибки по коду: errors.Is(err, &Error{Code: ...}).
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	return t.Code == e.Code
}

// Errorf конструирует доменную ошибку с форматированным описанием.
func Errorf(code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...)}
}

// WrapErrorf оборачивает низкоуровневую ошибку доменным кодом, сохраняя цепочку
// для логов; наружу клиенту уходит только Detail.
func WrapErrorf(cause error, code ErrorCode, format string, args ...any) *Error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...), cause: cause}
}

// CodeOf извлекает код из любой ошибки в цепочке. Для не-доменных ошибок
// возвращает CodeInternalError — наружу такие детали не просачиваются.
func CodeOf(err error) ErrorCode {
	var de *Error
	if errors.As(err, &de) {
		return de.Code
	}
	return CodeInternalError
}

// HasCode сообщает, есть ли в цепочке ошибок доменная ошибка с данным кодом.
func HasCode(err error, code ErrorCode) bool {
	var de *Error
	if errors.As(err, &de) {
		return de.Code == code
	}
	return false
}
