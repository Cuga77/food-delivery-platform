package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"

	"avito-kitchen/internal/domain"
)

// Коды ошибок PostgreSQL, которые интересны бизнес-логике.
const (
	pgUniqueViolation     = "23505"
	pgForeignKeyViolation = "23503"
	pgCheckViolation      = "23514"
	pgSerializationFail   = "40001"
	pgDeadlockDetected    = "40P01"
)

// wrapDBError приводит ошибку драйвера к доменной.
//
// Задача — не дать деталям устройства БД просочиться в HTTP-ответ и при этом
// сохранить исходную ошибку в цепочке для логов.
func wrapDBError(err error, operation string) error {
	if err == nil {
		return nil
	}

	// Отмена контекста — не ошибка базы: пробрасываем как есть, чтобы вызывающий
	// мог отличить остановку сервиса от сбоя.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case pgSerializationFail, pgDeadlockDetected:
			// Транзакция не смогла упорядочиться с параллельной — повтор запроса
			// имеет шанс пройти, поэтому отдаём конфликт состояния, а не 500.
			return domain.WrapErrorf(err, domain.CodeStateConflict,
				"параллельное изменение данных, повторите запрос")
		case pgUniqueViolation:
			return domain.WrapErrorf(err, domain.CodeStateConflict,
				"нарушено ограничение уникальности при выполнении операции «%s»", operation)
		case pgForeignKeyViolation, pgCheckViolation:
			return domain.WrapErrorf(err, domain.CodeValidationError,
				"данные не прошли проверку целостности при выполнении операции «%s»", operation)
		}
	}

	return domain.WrapErrorf(err, domain.CodeInternalError, "ошибка БД при выполнении операции «%s»", operation)
}
