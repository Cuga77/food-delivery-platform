package app

import (
	"encoding/base64"
	"strconv"
	"strings"
	"time"

	"avito-kitchen/internal/domain"
)

// Курсорная (keyset) пагинация вместо OFFSET: страница всегда выбирается по
// индексу, поэтому стоимость запроса не растёт с номером страницы, а вставка
// новых записей не сдвигает границы уже отданных страниц.
//
// Курсор непрозрачен для клиента: наружу уходит base64url, чтобы никто не
// строил на его внутреннем формате свою логику.
//
// Форматов два, и это не небрежность. Каталог упорядочен по возрастанию id,
// история заказов — по убыванию времени создания. Ключи разные, и один курсор
// для обоих означал бы либо лишнее поле, либо неверную границу страницы.
const (
	// catalogCursorPrefix — курсор по возрастанию id.
	catalogCursorPrefix = "v1:"
	// ordersCursorPrefix — курсор по паре (created_at, id) в порядке убывания.
	ordersCursorPrefix = "v2:"
)

// --- Каталог: ORDER BY id ---------------------------------------------------

// EncodeCursor упаковывает идентификатор последней записи страницы.
func EncodeCursor(lastID int64) string {
	return encode(catalogCursorPrefix + strconv.FormatInt(lastID, 10))
}

// DecodeCursor разбирает курсор каталога. Пустая строка означает первую страницу.
func DecodeCursor(cursor string) (int64, error) {
	if cursor == "" {
		return 0, nil
	}

	payload, err := decode(cursor, catalogCursorPrefix)
	if err != nil {
		return 0, err
	}

	id, err := strconv.ParseInt(payload, 10, 64)
	if err != nil || id < 0 {
		return 0, errBadCursor()
	}

	return id, nil
}

// --- Заказы: ORDER BY created_at DESC, id DESC ------------------------------

// OrderCursor — позиция в списке заказов.
//
// Одного времени недостаточно: два заказа могут быть оформлены в одну и ту же
// микросекунду, и без второго столбца страница на границе либо потеряла бы
// строку, либо показала её дважды. Пара (created_at, id) уникальна всегда.
type OrderCursor struct {
	CreatedAt time.Time
	ID        int64
}

// IsZero сообщает, что курсор не задан — запрошена первая страница.
func (c OrderCursor) IsZero() bool { return c.ID == 0 }

// EncodeOrderCursor упаковывает позицию последней записи страницы.
//
// Время кодируется в наносекундах Unix: это целое число, не зависящее от
// часового пояса и формата записи даты.
func EncodeOrderCursor(c OrderCursor) string {
	return encode(ordersCursorPrefix +
		strconv.FormatInt(c.CreatedAt.UTC().UnixNano(), 10) + ":" +
		strconv.FormatInt(c.ID, 10))
}

// DecodeOrderCursor разбирает курсор списка заказов.
func DecodeOrderCursor(cursor string) (OrderCursor, error) {
	if cursor == "" {
		return OrderCursor{}, nil
	}

	payload, err := decode(cursor, ordersCursorPrefix)
	if err != nil {
		return OrderCursor{}, err
	}

	parts := strings.Split(payload, ":")
	if len(parts) != 2 {
		return OrderCursor{}, errBadCursor()
	}

	nanos, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return OrderCursor{}, errBadCursor()
	}

	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id <= 0 {
		return OrderCursor{}, errBadCursor()
	}

	return OrderCursor{CreatedAt: time.Unix(0, nanos).UTC(), ID: id}, nil
}

// --- Общее ------------------------------------------------------------------

func encode(payload string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}

// decode снимает кодировку и проверяет, что курсор именно того вида, который
// ожидает вызывающий: курсор каталога, подставленный в список заказов, должен
// отвергаться, а не разбираться как попало.
func decode(cursor, prefix string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", errBadCursor()
	}

	decoded := string(raw)
	if !strings.HasPrefix(decoded, prefix) {
		return "", errBadCursor()
	}

	payload := decoded[len(prefix):]
	if payload == "" {
		return "", errBadCursor()
	}

	return payload, nil
}

func errBadCursor() error {
	return domain.Errorf(domain.CodeBadRequest, "некорректный курсор пагинации")
}
