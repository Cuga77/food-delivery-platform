package app

import (
	"encoding/base64"
	"strconv"

	"avito-kitchen/internal/domain"
)

// Курсорная (keyset) пагинация вместо OFFSET: страница всегда выбирается по
// индексу первичного ключа, поэтому стоимость запроса не растёт с номером
// страницы, а вставка новых заведений не сдвигает границы уже отданных страниц.
//
// Курсор непрозрачен для клиента: наружу уходит base64url, чтобы никто не
// строил на его внутреннем формате свою логику.

const cursorPrefix = "v1:"

// EncodeCursor упаковывает идентификатор последней записи страницы.
func EncodeCursor(lastID int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(cursorPrefix + strconv.FormatInt(lastID, 10)))
}

// DecodeCursor разбирает курсор. Пустая строка означает первую страницу.
func DecodeCursor(cursor string) (int64, error) {
	if cursor == "" {
		return 0, nil
	}

	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, domain.Errorf(domain.CodeBadRequest, "некорректный курсор пагинации")
	}

	decoded := string(raw)
	if len(decoded) <= len(cursorPrefix) || decoded[:len(cursorPrefix)] != cursorPrefix {
		return 0, domain.Errorf(domain.CodeBadRequest, "неподдерживаемая версия курсора пагинации")
	}

	id, err := strconv.ParseInt(decoded[len(cursorPrefix):], 10, 64)
	if err != nil || id < 0 {
		return 0, domain.Errorf(domain.CodeBadRequest, "некорректный курсор пагинации")
	}

	return id, nil
}
