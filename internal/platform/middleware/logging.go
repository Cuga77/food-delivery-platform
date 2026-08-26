// Package middleware содержит HTTP-прослойки платформы: идентификатор запроса,
// логирование, аутентификация заведений и идемпотентность.
package middleware

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"avito-kitchen/internal/platform/logger"
)

// RequestID кладёт идентификатор запроса в контекст логгера и возвращает его
// клиенту заголовком X-Request-Id.
//
// Идентификатор генерирует chi (middleware.RequestID) — здесь он лишь
// прокидывается туда, где его увидят логгер и формат ошибок RFC 7807. По этому
// значению запрос из ответа клиента находится в логах одной командой.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := middleware.GetReqID(r.Context())
		if id == "" {
			next.ServeHTTP(w, r)
			return
		}

		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(logger.WithRequestID(r.Context(), id)))
	})
}

// Logger пишет по одной структурированной записи на запрос.
func Logger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Health-проверки дёргаются раз в секунду и в логах только мешают.
			if isHealthPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			next.ServeHTTP(ww, r)

			log.InfoContext(r.Context(), "запрос обработан",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", ww.Status()),
				slog.Int("bytes", ww.BytesWritten()),
				slog.Duration("duration", time.Since(start)),
				slog.String("remote_addr", r.RemoteAddr),
			)
		})
	}
}

// Recoverer превращает панику в 500 и пишет её в лог со стеком, вместо того
// чтобы уронить весь процесс из-за одного плохого запроса.
func Recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Контекст запроса здесь используется только для логирования: сама
			// обработка паники ничего не отменяет и не запускает.
			defer func() { //nolint:contextcheck // логируем в контексте запроса, новых вызовов не делаем
				rec := recover()
				if rec == nil {
					return
				}
				// http.ErrAbortHandler — штатный способ прервать обработку,
				// его перехватывать не нужно.
				if err, ok := rec.(error); ok && errors.Is(err, http.ErrAbortHandler) {
					panic(rec)
				}

				log.ErrorContext(r.Context(), "паника при обработке запроса",
					slog.Any("panic", rec),
					slog.String("method", r.Method),
					slog.String("path", r.URL.Path),
					slog.String("stack", string(stack())),
				)

				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"type":"https://avito.ru/kitchen/errors/INTERNAL_ERROR",` +
					`"title":"Internal Server Error","status":500,"code":"INTERNAL_ERROR",` +
					`"detail":"внутренняя ошибка сервиса"}`))
			}()

			next.ServeHTTP(w, r)
		})
	}
}

func isHealthPath(path string) bool {
	return path == "/health" || path == "/readyz"
}
