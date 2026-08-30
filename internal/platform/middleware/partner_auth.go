package middleware

import (
	"context"
	"log/slog"
	"net/http"

	"avito-kitchen/internal/domain"
	"avito-kitchen/internal/platform/problem"
)

// PartnerTokenHeader — заголовок с партнёрским токеном.
const PartnerTokenHeader = "X-Partner-Token"

type restaurantContextKey struct{}

// RestaurantFromContext достаёт аутентифицированное заведение.
// Вызывается только внутри партнёрских хендлеров, где middleware гарантирует
// его наличие.
func RestaurantFromContext(ctx context.Context) (domain.Restaurant, bool) {
	restaurant, ok := ctx.Value(restaurantContextKey{}).(domain.Restaurant)
	return restaurant, ok
}

// PartnerAuthenticator — то, что middleware умеет спросить у слоя приложения.
// Интерфейс объявлен здесь, у потребителя, а не в app: прослойке нужен ровно
// один метод, и знать про весь PartnerService ей незачем.
type PartnerAuthenticator interface {
	Authenticate(ctx context.Context, token string) (domain.Restaurant, error)
}

// PartnerAuth аутентифицирует заведение по заголовку X-Partner-Token.
//
// Сам токен нигде не логируется и не попадает в ответ: в логах остаётся только
// slug опознанного заведения.
func PartnerAuth(auth PartnerAuthenticator, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := r.Header.Get(PartnerTokenHeader)

			restaurant, err := auth.Authenticate(r.Context(), token)
			if err != nil {
				log.InfoContext(r.Context(), "партнёрский запрос отклонён",
					slog.String("path", r.URL.Path),
					slog.String("reason", string(domain.CodeOf(err))))
				problem.Write(w, r, err, nil)
				return
			}

			ctx := context.WithValue(r.Context(), restaurantContextKey{}, restaurant)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
