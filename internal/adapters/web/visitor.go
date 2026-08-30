package web

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Личность анонимного посетителя.
//
// Аутентификации пользователей в MVP нет — задание её не требует, а
// интеграция с экосистемой Авито подставила бы сюда идентификатор из своей
// сессии. Но «нет аутентификации» не должно означать «нет личности вовсе»:
// пока веб-клиент оформлял все заказы от лица одного захардкоженного
// `web_guest`, поле user_external_id не значило ничего, а история заказов была
// бы общей на всех посетителей сразу.
//
// Поэтому браузер получает собственный случайный идентификатор в cookie. Это
// не аутентификация, а capability: знание значения даёт доступ к истории, ровно
// как знание public_number даёт доступ к карточке заказа. Значение
// неугадываемо, поэтому перебрать чужую историю нельзя, но подтверждать
// личность оно не может, и права на его основе выдавать нельзя. Ограничение
// зафиксировано в README и ADR-0004.
const (
	// visitorCookieName — имя cookie с идентификатором посетителя.
	visitorCookieName = "kitchen_visitor"
	// visitorTTL — год: корзина и история переживают закрытие вкладки.
	visitorTTL = 365 * 24 * time.Hour
	// visitorPrefix отделяет идентификаторы веб-посетителей от идентификаторов,
	// которые пришли бы из экосистемы Авито. Без префикса подставленная cookie
	// могла бы совпасть с чужим идентификатором из другого пространства имён.
	visitorPrefix = "web_"
	// visitorEntropyBytes — 128 бит случайности: перебор невозможен.
	visitorEntropyBytes = 16
	// visitorIDLen — длина корректного значения: префикс плюс base64url без
	// выравнивания от 16 байт (22 символа).
	visitorIDLen = len(visitorPrefix) + 22
)

// visitorID возвращает идентификатор посетителя, выдавая новый при первом
// заходе.
//
// Cookie выставляется в w, поэтому вызывать функцию нужно до записи тела
// ответа. Значение из cookie проверяется по формату: принять произвольную
// строку значило бы позволить клиенту назначить себе любой user_external_id,
// в том числе чужой.
func (h *Handler) visitorID(w http.ResponseWriter, r *http.Request) (string, error) {
	if cookie, err := r.Cookie(visitorCookieName); err == nil && validVisitorID(cookie.Value) {
		return cookie.Value, nil
	}

	id, err := newVisitorID()
	if err != nil {
		return "", err
	}

	setVisitorCookie(w, id, h.secureCookies)

	return id, nil
}

// currentVisitorID читает идентификатор, не выдавая новый.
//
// Нужен там, где ответ уже мог начать писаться или где отсутствие личности —
// нормальная ситуация с осмысленным пустым ответом (история заказов у
// посетителя, который ещё ничего не заказывал).
func currentVisitorID(r *http.Request) string {
	cookie, err := r.Cookie(visitorCookieName)
	if err != nil || !validVisitorID(cookie.Value) {
		return ""
	}

	return cookie.Value
}

// newVisitorID генерирует новый идентификатор.
//
// Ошибка генератора случайных чисел возвращается наверх, а не подменяется
// предсказуемым значением: тихий откат на счётчик или на время создал бы
// угадываемые идентификаторы, то есть ровно ту дыру, от которой защищает
// случайность.
func newVisitorID() (string, error) {
	buf := make([]byte, visitorEntropyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("генерация идентификатора посетителя: %w", err)
	}

	return visitorPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

func validVisitorID(value string) bool {
	if len(value) != visitorIDLen || !strings.HasPrefix(value, visitorPrefix) {
		return false
	}

	// Декодирование заодно проверяет алфавит: посторонние символы до БД не
	// доходят, а длина совпадает с ожидаемой энтропией.
	raw, err := base64.RawURLEncoding.DecodeString(value[len(visitorPrefix):])

	return err == nil && len(raw) == visitorEntropyBytes
}

// setVisitorCookie сохраняет идентификатор в браузере.
//
// HttpOnly — значение недоступно из JavaScript; SameSite=Lax — межсайтовый
// POST не сможет оформить заказ от лица посетителя; Path=/ — в отличие от
// партнёрской cookie, эта нужна на всех клиентских страницах. Флаг Secure
// управляется той же настройкой WEB_SECURE_COOKIES: на локальном HTTP браузер
// Secure-cookie просто не сохранит.
func setVisitorCookie(w http.ResponseWriter, id string, secure bool) {
	//nolint:gosec // G124: HttpOnly и SameSite заданы; Secure — из конфигурации
	http.SetCookie(w, &http.Cookie{
		Name:     visitorCookieName,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   int(visitorTTL.Seconds()),
	})
}
