package web

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"

	"avito-kitchen/internal/domain"
	"avito-kitchen/internal/platform/problem"
)

// render отдаёт страницу целиком.
//
// Шаблон исполняется в буфер, а не прямо в ResponseWriter: если разметка
// упадёт на середине, пользователь получит страницу ошибки, а не обрубок
// HTML с уже отправленным статусом 200.
func (h *Handler) render(w http.ResponseWriter, r *http.Request, status int, name string, data any) {
	var buf bytes.Buffer

	if err := h.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		h.log.ErrorContext(r.Context(), "не удалось отрендерить шаблон",
			slog.String("template", name), slog.Any("error", err))
		http.Error(w, "внутренняя ошибка сервиса", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// isHTMX сообщает, что запрос пришёл от htmx и ждёт фрагмент, а не страницу.
func isHTMX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}

// errorPageData — данные страницы ошибки.
type errorPageData struct {
	Title      string
	Status     int
	Code       string
	Message    string
	BackURL    string
	BackLabel  string
	IsInternal bool
}

// renderError показывает ошибку человеку.
//
// Доменные ошибки уже содержат текст, написанный для пользователя («позиции
// осталось 5, а запрошено 12»), и несут код, по которому определяется
// HTTP-статус. Соответствие кода и статуса берётся из того же места, что и для
// JSON API, — иначе два транспорта начали бы отвечать по-разному на одну и ту
// же ситуацию.
//
// Всё, что не является доменной ошибкой, наружу не раскрывается: пользователь
// видит нейтральный текст, подробности уходят в лог.
func (h *Handler) renderError(w http.ResponseWriter, r *http.Request, err error, back backLink) {
	code := domain.CodeOf(err)
	status := problem.StatusFor(code)

	var domainErr *domain.Error
	isDomain := errors.As(err, &domainErr)

	message := "Что-то пошло не так на нашей стороне. Попробуйте позже."
	if isDomain {
		message = domainErr.Detail
	}

	if status >= http.StatusInternalServerError {
		h.log.ErrorContext(r.Context(), "страница завершилась ошибкой сервиса",
			slog.String("path", r.URL.Path), slog.Any("error", err))
	} else {
		h.log.InfoContext(r.Context(), "страница отклонила запрос",
			slog.String("path", r.URL.Path),
			slog.String("code", string(code)))
	}

	h.render(w, r, status, pageError, errorPageData{
		Title:      errorTitle(status),
		Status:     status,
		Code:       string(code),
		Message:    message,
		BackURL:    back.URL,
		BackLabel:  back.Label,
		IsInternal: status >= http.StatusInternalServerError,
	})
}

// backLink — куда предложить вернуться со страницы ошибки. Тупик без выхода
// хуже самой ошибки.
type backLink struct {
	URL   string
	Label string
}

var backToRestaurants = backLink{URL: "/restaurants", Label: "К списку заведений"}

func errorTitle(status int) string {
	switch status {
	case http.StatusNotFound:
		return "Не найдено"
	case http.StatusUnauthorized, http.StatusForbidden:
		return "Нет доступа"
	case http.StatusConflict:
		return "Не получилось"
	case http.StatusUnprocessableEntity:
		return "Проверьте заказ"
	case http.StatusBadRequest:
		return "Некорректный запрос"
	default:
		return "Ошибка"
	}
}

// badRequest — короткий путь для ошибок разбора запроса.
func (h *Handler) badRequest(w http.ResponseWriter, r *http.Request, detail string, back backLink) {
	h.renderError(w, r, domain.Errorf(domain.CodeBadRequest, "%s", detail), back)
}
