// Package web — серверный рендеринг страниц для веб-клиента.
//
// Пакет ничего не знает о бизнес-правилах: он вызывает те же use cases, что и
// JSON API (CatalogService, OrderService, PartnerService), и переводит их
// результат в HTML. Это второй транспорт над одним ядром, а не вторая
// реализация логики.
//
// HTMX используется как прогрессивное улучшение: все сценарии — просмотр
// каталога, оформление и отмена заказа, управление кухней — работают обычными
// формами и ссылками. Если скрипт не загрузился, страницы остаются
// функциональными, просто без частичных обновлений.
package web

import (
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"avito-kitchen/internal/app"
)

//go:embed templates/*.html
var templatesFS embed.FS

// Handler обслуживает серверные страницы.
type Handler struct {
	catalog     *app.CatalogService
	orders      *app.OrderService
	partner     *app.PartnerService
	idempotency app.IdempotencyRepo
	// idempotencyTTL — сколько живёт запись о выполненной отправке формы заказа.
	idempotencyTTL time.Duration
	// secureCookies помечает cookie сессии флагом Secure. Обязателен за TLS и
	// вреден на локальном HTTP: браузер не сохранит такую cookie.
	secureCookies bool
	tmpl          *template.Template
	log           *slog.Logger
}

// Config — зависимости веб-слоя.
type Config struct {
	Catalog        *app.CatalogService
	Orders         *app.OrderService
	Partner        *app.PartnerService
	Idempotency    app.IdempotencyRepo
	IdempotencyTTL time.Duration
	SecureCookies  bool
	Logger         *slog.Logger
}

// New собирает обработчик и компилирует шаблоны.
//
// Шаблоны разбираются один раз на старте: ошибка в разметке валит сервис при
// запуске, а не первого попавшего на страницу пользователя.
func New(cfg Config) (*Handler, error) {
	tmpl, err := template.New("web").Funcs(templateFuncs()).ParseFS(templatesFS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("разбор шаблонов: %w", err)
	}

	if err := assertTemplatesDefined(tmpl); err != nil {
		return nil, err
	}

	ttl := cfg.IdempotencyTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}

	return &Handler{
		catalog:        cfg.Catalog,
		orders:         cfg.Orders,
		partner:        cfg.Partner,
		idempotency:    cfg.Idempotency,
		idempotencyTTL: ttl,
		secureCookies:  cfg.SecureCookies,
		tmpl:           tmpl,
		log:            cfg.Logger,
	}, nil
}

// Имена шаблонов вынесены в константы, а их наличие проверяется на старте.
//
// Это защита от конкретной ошибки: ParseFS регистрирует каждый файл ещё и под
// его именем, и опечатка вроде "restaurants.html" вместо "page:restaurants"
// не падает, а молча рендерит пустую страницу. Префиксы `page:` и `partial:`
// с именами файлов не совпадают, поэтому промах даёт явную ошибку.
const (
	pageRestaurants  = "page:restaurants"
	pageMyOrders     = "page:my-orders"
	pageMenu         = "page:menu"
	pageOrder        = "page:order"
	pageError        = "page:error"
	pagePartnerLogin = "page:partner-login"
	pagePartnerPanel = "page:partner-panel"

	partialRestaurantCards = "partial:restaurant-cards"
	partialMyOrderRows     = "partial:my-order-rows"
	partialOrderDetail     = "partial:order-detail"
	partialPartnerOrders   = "partial:partner-orders"
)

func assertTemplatesDefined(tmpl *template.Template) error {
	required := []string{
		pageRestaurants, pageMyOrders, pageMenu, pageOrder, pageError,
		pagePartnerLogin, pagePartnerPanel,
		partialRestaurantCards, partialMyOrderRows,
		partialOrderDetail, partialPartnerOrders,
	}

	for _, name := range required {
		if tmpl.Lookup(name) == nil {
			return fmt.Errorf("шаблон %q не определён", name)
		}
	}

	return nil
}

// Mount регистрирует маршруты веб-клиента.
//
// Пути намеренно не пересекаются с /api/v1: JSON-контракт и страницы живут в
// разных пространствах имён и могут развиваться независимо.
func (h *Handler) Mount(r chi.Router) {
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/restaurants", http.StatusFound)
	})

	// --- Клиент ---
	r.Get("/restaurants", h.restaurantsPage)
	r.Get(myOrdersPath, h.myOrdersPage)
	r.Get("/restaurants/{slug}", h.menuPage)
	r.Post("/orders", h.createOrder)
	r.Get("/orders/{public_number}", h.orderPage)
	r.Post("/orders/{public_number}/cancel", h.cancelOrder)

	// --- Заведение ---
	// Вход открыт, всё остальное — только для аутентифицированного заведения.
	r.Get("/partner/login", h.partnerLoginPage)
	r.Post("/partner/login", h.partnerLogin)
	r.Post("/partner/logout", h.partnerLogout)

	r.Group(func(r chi.Router) {
		r.Use(h.requirePartner)
		r.Get("/partner", h.partnerPanel)
		r.Get("/partner/orders", h.partnerOrders)
		r.Post("/partner/kitchen-status", h.partnerKitchenStatus)
		r.Post("/partner/orders/{public_number}/status", h.partnerOrderStatus)
	})
}
