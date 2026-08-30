package web

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"avito-kitchen/internal/app"
	"avito-kitchen/internal/domain"
)

const (
	partnerOrdersLimit = 50

	// partnerPanelPath — корень кабинета заведения.
	partnerPanelPath = "/partner"
	partnerLoginPath = "/partner/login"
)

func urlQueryEscape(s string) string { return url.QueryEscape(s) }

// --- Вход -------------------------------------------------------------------

type partnerLoginData struct {
	Title  string
	Notice string
	Error  string
}

func (h *Handler) partnerLoginPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, http.StatusOK, pagePartnerLogin, partnerLoginData{
		Title:  "Вход для заведений",
		Notice: r.URL.Query().Get("notice"),
	})
}

// partnerLogin проверяет токен и открывает сессию.
//
// Токен проверяется тем же PartnerService.Authenticate, что и заголовок
// X-Partner-Token в JSON API: у панели и у интеграции одна модель доступа.
func (h *Handler) partnerLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.badRequest(w, r, "не удалось разобрать форму входа", backToRestaurants)
		return
	}

	token := strings.TrimSpace(r.PostFormValue("token"))

	if _, err := h.partner.Authenticate(r.Context(), token); err != nil {
		// Причину не уточняем: «нет такого токена» и «пустой токен» снаружи
		// выглядят одинаково, чтобы перебор не давал подсказок.
		h.render(w, r, http.StatusUnauthorized, pagePartnerLogin, partnerLoginData{
			Title: "Вход для заведений",
			Error: "Токен не распознан. Проверьте значение и попробуйте снова.",
		})
		return
	}

	setPartnerCookie(w, token, h.secureCookies)
	http.Redirect(w, r, partnerPanelPath, http.StatusSeeOther)
}

func (h *Handler) partnerLogout(w http.ResponseWriter, r *http.Request) {
	clearPartnerCookie(w)
	http.Redirect(w, r, partnerLoginPath, http.StatusSeeOther)
}

// --- Панель -----------------------------------------------------------------

type partnerPanelData struct {
	Title      string
	Restaurant domain.Restaurant
	Orders     []domain.Order
	Status     string
	// Cursor — курсор текущей страницы; нужен, чтобы автообновление опрашивало
	// ту страницу, которую заведение сейчас смотрит, а не возвращало его в
	// начало очереди каждые пять секунд.
	Cursor     string
	NextCursor string
}

type partnerOrdersData struct {
	Orders     []domain.Order
	Status     string
	Cursor     string
	NextCursor string
}

// partnerPanel — рабочее место заведения.
//
// Селектора заведений здесь нет и быть не может: панель показывает ровно то
// заведение, чьим токеном выполнен вход.
func (h *Handler) partnerPanel(w http.ResponseWriter, r *http.Request) {
	restaurant, ok := partnerFromContext(r.Context())
	if !ok {
		h.redirectToLogin(w, r, "")
		return
	}

	statusFilter := r.URL.Query().Get("status")

	page, err := h.listPartnerOrders(r, restaurant.ID, statusFilter)
	if err != nil {
		h.renderError(w, r, err, backLink{URL: partnerPanelPath, Label: "Обновить панель"})
		return
	}

	h.render(w, r, http.StatusOK, pagePartnerPanel, partnerPanelData{
		Title:      restaurant.Name + " — панель",
		Restaurant: restaurant,
		Orders:     page.Items,
		Status:     statusFilter,
		Cursor:     r.URL.Query().Get("cursor"),
		NextCursor: page.NextCursor,
	})
}

// partnerOrders отдаёт только таблицу очереди — для частичного обновления.
func (h *Handler) partnerOrders(w http.ResponseWriter, r *http.Request) {
	restaurant, ok := partnerFromContext(r.Context())
	if !ok {
		h.redirectToLogin(w, r, "")
		return
	}

	statusFilter := r.URL.Query().Get("status")

	page, err := h.listPartnerOrders(r, restaurant.ID, statusFilter)
	if err != nil {
		h.renderError(w, r, err, backLink{URL: partnerPanelPath, Label: "Обновить панель"})
		return
	}

	h.render(w, r, http.StatusOK, partialPartnerOrders, partnerOrdersData{
		Orders:     page.Items,
		Status:     statusFilter,
		Cursor:     r.URL.Query().Get("cursor"),
		NextCursor: page.NextCursor,
	})
}

func (h *Handler) listPartnerOrders(
	r *http.Request,
	restaurantID int64,
	statusFilter string,
) (app.OrderPage, error) {
	query := app.ListOrdersQuery{
		RestaurantID: restaurantID,
		Limit:        partnerOrdersLimit,
		Cursor:       r.URL.Query().Get("cursor"),
	}

	if statusFilter != "" {
		status := domain.OrderStatus(statusFilter)
		query.Status = &status
	}

	return h.partner.ListOrders(r.Context(), query)
}

// partnerKitchenStatus переключает режим работы кухни.
func (h *Handler) partnerKitchenStatus(w http.ResponseWriter, r *http.Request) {
	restaurant, ok := partnerFromContext(r.Context())
	if !ok {
		h.redirectToLogin(w, r, "")
		return
	}

	if err := r.ParseForm(); err != nil {
		h.badRequest(w, r, "не удалось разобрать форму", backLink{URL: partnerPanelPath, Label: "К панели"})
		return
	}

	status := domain.RestaurantStatus(r.PostFormValue("status"))

	// Идентификатор заведения — из сессии, а не из формы.
	if _, err := h.partner.SetKitchenStatus(r.Context(), restaurant.ID, status); err != nil {
		h.renderError(w, r, err, backLink{URL: partnerPanelPath, Label: "К панели"})
		return
	}

	http.Redirect(w, r, partnerPanelPath, http.StatusSeeOther)
}

// partnerOrderStatus двигает заказ по конвейеру.
func (h *Handler) partnerOrderStatus(w http.ResponseWriter, r *http.Request) {
	restaurant, ok := partnerFromContext(r.Context())
	if !ok {
		h.redirectToLogin(w, r, "")
		return
	}

	back := backLink{URL: partnerPanelPath, Label: "К панели"}

	publicNumber, err := uuid.Parse(chi.URLParam(r, "public_number"))
	if err != nil {
		h.badRequest(w, r, "некорректный номер заказа", back)
		return
	}

	if err := r.ParseForm(); err != nil {
		h.badRequest(w, r, "не удалось разобрать форму", back)
		return
	}

	// RestaurantID из сессии включает в use case проверку владения заказом:
	// чужой заказ вернёт FORBIDDEN и не сдвинется.
	_, err = h.orders.ChangeStatus(r.Context(), app.StatusChange{
		PublicNumber: publicNumber,
		To:           domain.OrderStatus(r.PostFormValue("status")),
		Actor:        domain.ActorRestaurant,
		Comment:      strings.TrimSpace(r.PostFormValue("comment")),
		Reason:       strings.TrimSpace(r.PostFormValue("comment")),
		RestaurantID: restaurant.ID,
	})
	if err != nil {
		h.renderError(w, r, err, back)
		return
	}

	redirect := partnerPanelPath
	if status := r.PostFormValue("filter_status"); status != "" {
		redirect += "?status=" + url.QueryEscape(status)
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}
