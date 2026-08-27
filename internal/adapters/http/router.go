package http

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"

	"avito-kitchen/internal/adapters/web"
	"avito-kitchen/internal/app"
	"avito-kitchen/internal/domain"
	"avito-kitchen/internal/gen"
	"avito-kitchen/internal/platform/middleware"
	"avito-kitchen/internal/platform/problem"
)

// Server реализует gen.StrictServerInterface, собирая её из трёх обработчиков.
// Встраивание даёт продвинутые методы, поэтому отдельного «фасада» с
// делегирующими вызовами не нужно.
type Server struct {
	*ClientHandler
	*PartnerHandler
	*HealthHandler
}

var _ gen.StrictServerInterface = (*Server)(nil)

// NewServer собирает реализацию контракта.
func NewServer(client *ClientHandler, partner *PartnerHandler, health *HealthHandler) *Server {
	return &Server{ClientHandler: client, PartnerHandler: partner, HealthHandler: health}
}

// RouterConfig — зависимости и параметры сборки маршрутизатора.
type RouterConfig struct {
	Server         *Server
	WebHandler     *web.Handler
	PartnerAuth    middleware.PartnerAuthenticator
	Idempotency    app.IdempotencyRepo
	IdempotencyTTL time.Duration
	RequestTimeout time.Duration
	CORSOrigins    []string
	Logger         *slog.Logger
}

// NewRouter собирает chi.Mux со всеми прослойками и маршрутами контракта.
//
// Порядок глобальных прослоек существенен: RequestID должен отработать до
// логгера (иначе в записях не будет идентификатора), Recoverer — до всего
// прикладного (иначе паника не превратится в 500), Timeout — после логгера
// (иначе отменённые запросы исчезнут из логов).
func NewRouter(cfg RouterConfig) http.Handler {
	r := chi.NewRouter()

	r.Use(chimw.RequestID)
	// chimw.RealIP намеренно не используется: он доверяет заголовкам
	// X-Forwarded-For / X-Real-IP, которые клиент может подделать, если перед
	// сервисом нет доверенного прокси. В MVP такого прокси нет, поэтому
	// remote_addr в логах остаётся адресом реального соединения.
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer(cfg.Logger))
	r.Use(middleware.Logger(cfg.Logger))
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins: cfg.CORSOrigins,
		AllowedMethods: []string{http.MethodGet, http.MethodPost, http.MethodOptions},
		AllowedHeaders: []string{
			"Accept", "Content-Type", "Authorization",
			middleware.IdempotencyKeyHeader, middleware.PartnerTokenHeader,
		},
		ExposedHeaders: []string{"X-Request-Id", "Idempotent-Replay"},
		MaxAge:         300,
	}))
	r.Use(chimw.Timeout(cfg.RequestTimeout))

	// Прослойки, действующие не на весь API, а на конкретные маршруты.
	// Генератор умеет добавлять middleware только глобально, поэтому область
	// действия задаётся явным предикатом по пути.
	r.Use(scoped(isPartnerRequest,
		middleware.PartnerAuth(cfg.PartnerAuth, cfg.Logger)))
	r.Use(scoped(isCreateOrderRequest,
		middleware.Idempotency(cfg.Idempotency, cfg.IdempotencyTTL, cfg.Logger)))

	strictHandler := gen.NewStrictHandlerWithOptions(cfg.Server, nil, gen.StrictHTTPServerOptions{
		// Тело запроса не разобралось: невалидный JSON или не тот тип поля.
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			problem.WriteCode(w, r, domain.CodeBadRequest,
				"не удалось разобрать тело запроса: "+err.Error())
		},
		// Хендлер вернул ошибку — единственная точка перевода доменных ошибок
		// в HTTP-ответ.
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			problem.Write(w, r, err, cfg.Logger)
		},
	})

	gen.HandlerWithOptions(strictHandler, gen.ChiServerOptions{
		BaseRouter: r,
		// Не разобрались параметры пути или query (например, кривой UUID).
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			problem.WriteCode(w, r, domain.CodeBadRequest,
				"некорректные параметры запроса: "+err.Error())
		},
	})

	// Серверные страницы (HTMX + Go templates).
	if cfg.WebHandler != nil {
		cfg.WebHandler.Mount(r)
	}

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		problem.WriteCode(w, r, domain.CodeBadRequest, "маршрут не найден")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		problem.WriteCode(w, r, domain.CodeBadRequest, "метод не поддерживается для этого маршрута")
	})

	return r
}

// scoped применяет прослойку только к запросам, для которых предикат истинен.
func scoped(matches func(*http.Request) bool, mw func(http.Handler) http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		wrapped := mw(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if matches(r) {
				wrapped.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

const (
	partnerPathPrefix = "/api/v1/partner/"
	createOrderPath   = "/api/v1/orders"
)

func isPartnerRequest(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, partnerPathPrefix)
}

func isCreateOrderRequest(r *http.Request) bool {
	return r.Method == http.MethodPost && r.URL.Path == createOrderPath
}
