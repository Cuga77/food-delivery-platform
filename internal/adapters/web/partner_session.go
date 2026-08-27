package web

import (
	"context"
	"net/http"
	"time"

	"avito-kitchen/internal/domain"
)

// partnerCookieName — имя cookie с партнёрским токеном.
const partnerCookieName = "kitchen_partner"

// partnerSessionTTL — сколько живёт сессия панели.
const partnerSessionTTL = 12 * time.Hour

type partnerContextKey struct{}

// partnerFromContext достаёт заведение, аутентифицированное прослойкой.
func partnerFromContext(ctx context.Context) (domain.Restaurant, bool) {
	restaurant, ok := ctx.Value(partnerContextKey{}).(domain.Restaurant)
	return restaurant, ok
}

// setPartnerCookie сохраняет партнёрский токен в браузере.
//
// Свойства cookie выбраны осознанно:
//   - HttpOnly — токен недоступен из JavaScript, поэтому XSS на странице не
//     превращается в кражу партнёрского доступа;
//   - SameSite=Lax — браузер не приложит cookie к межсайтовому POST, что
//     закрывает CSRF на изменяющих операциях панели (смена статуса заказа,
//     закрытие кухни) без отдельных токенов в формах;
//   - Path=/partner — cookie не уходит с запросами клиентской части и JSON API.
//
// Флаг Secure включается переменной WEB_SECURE_COOKIES: локальный стек
// поднимается по HTTP, где Secure-cookie браузер просто не сохранит, а за TLS
// он обязателен.
func setPartnerCookie(w http.ResponseWriter, token string, secure bool) {
	//nolint:gosec // G124: HttpOnly и SameSite заданы; Secure управляется
	// конфигурацией, потому что на локальном HTTP браузер такую cookie не сохранит
	http.SetCookie(w, &http.Cookie{
		Name:     partnerCookieName,
		Value:    token,
		Path:     partnerPanelPath,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   int(partnerSessionTTL.Seconds()),
	})
}

func clearPartnerCookie(w http.ResponseWriter) {
	//nolint:gosec // G124: cookie удаляется (MaxAge < 0), значение пустое
	http.SetCookie(w, &http.Cookie{
		Name:     partnerCookieName,
		Value:    "",
		Path:     partnerPanelPath,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// requirePartner пропускает дальше только аутентифицированное заведение.
//
// Заведение кладётся в контекст, и хендлеры панели берут restaurant_id
// исключительно оттуда. Из формы или пути он не принимается никогда — иначе
// подстановка чужого идентификатора давала бы управление чужой кухней.
func (h *Handler) requirePartner(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(partnerCookieName)
		if err != nil || cookie.Value == "" {
			h.redirectToLogin(w, r, "")
			return
		}

		restaurant, err := h.partner.Authenticate(r.Context(), cookie.Value)
		if err != nil {
			// Токен отозван или заведение удалено — сессия больше не годится.
			clearPartnerCookie(w)
			h.redirectToLogin(w, r, "Сессия истекла — войдите заново")
			return
		}

		ctx := context.WithValue(r.Context(), partnerContextKey{}, restaurant)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// redirectToLogin отправляет на страницу входа.
//
// htmx-запросы редиректом не разворачиваются (фрагмент подставился бы внутрь
// страницы), поэтому для них используется заголовок HX-Redirect — браузер
// перейдёт на страницу входа целиком.
func (h *Handler) redirectToLogin(w http.ResponseWriter, r *http.Request, notice string) {
	target := partnerLoginPath
	if notice != "" {
		target += "?notice=" + urlQueryEscape(notice)
	}

	if isHTMX(r) {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	http.Redirect(w, r, target, http.StatusSeeOther)
}
