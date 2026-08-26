package http

import (
	"context"

	"avito-kitchen/internal/app"
	"avito-kitchen/internal/domain"
	"avito-kitchen/internal/gen"
)

// ClientHandler обслуживает B2C-часть контракта.
//
// Все методы возвращают доменную ошибку как есть: перевод в RFC 7807 делает
// один общий обработчик ошибок strict-сервера, поэтому в хендлерах нет ни
// HTTP-статусов, ни ручной сборки тела ошибки.
type ClientHandler struct {
	catalog *app.CatalogService
	orders  *app.OrderService
}

// NewClientHandler собирает обработчик клиентского API.
func NewClientHandler(catalog *app.CatalogService, orders *app.OrderService) *ClientHandler {
	return &ClientHandler{catalog: catalog, orders: orders}
}

// ListRestaurants — GET /api/v1/restaurants.
func (h *ClientHandler) ListRestaurants(
	ctx context.Context,
	request gen.ListRestaurantsRequestObject,
) (gen.ListRestaurantsResponseObject, error) {
	query := app.ListRestaurantsQuery{}

	if request.Params.Limit != nil {
		query.Limit = *request.Params.Limit
	}
	if request.Params.Cursor != nil {
		query.Cursor = *request.Params.Cursor
	}
	if request.Params.Status != nil {
		status := domain.RestaurantStatus(*request.Params.Status)
		query.Status = &status
	}

	page, err := h.catalog.ListRestaurants(ctx, query)
	if err != nil {
		return nil, err
	}

	return gen.ListRestaurants200JSONResponse(toAPIRestaurantPage(page)), nil
}

// GetRestaurantMenu — GET /api/v1/restaurants/{slug}/menu.
func (h *ClientHandler) GetRestaurantMenu(
	ctx context.Context,
	request gen.GetRestaurantMenuRequestObject,
) (gen.GetRestaurantMenuResponseObject, error) {
	menu, err := h.catalog.GetMenuBySlug(ctx, request.Slug)
	if err != nil {
		return nil, err
	}

	return gen.GetRestaurantMenu200JSONResponse(toAPIMenu(menu)), nil
}

// CreateOrder — POST /api/v1/orders.
//
// Проверка Idempotency-Key выполняется прослойкой до входа сюда: к моменту
// вызова хендлера ключ уже захвачен, и повторы до бизнес-логики не доходят.
func (h *ClientHandler) CreateOrder(
	ctx context.Context,
	request gen.CreateOrderRequestObject,
) (gen.CreateOrderResponseObject, error) {
	if request.Body == nil {
		return nil, domain.Errorf(domain.CodeBadRequest, "пустое тело запроса")
	}

	order, err := h.orders.Create(ctx, toDomainDraft(gen.CreateOrderRequest(*request.Body)))
	if err != nil {
		return nil, err
	}

	return gen.CreateOrder201JSONResponse(toAPIOrder(order)), nil
}

// GetOrder — GET /api/v1/orders/{public_number}.
func (h *ClientHandler) GetOrder(
	ctx context.Context,
	request gen.GetOrderRequestObject,
) (gen.GetOrderResponseObject, error) {
	order, err := h.orders.Get(ctx, request.PublicNumber)
	if err != nil {
		return nil, err
	}

	return gen.GetOrder200JSONResponse(toAPIOrder(order)), nil
}

// CancelOrder — POST /api/v1/orders/{public_number}/cancel.
func (h *ClientHandler) CancelOrder(
	ctx context.Context,
	request gen.CancelOrderRequestObject,
) (gen.CancelOrderResponseObject, error) {
	if request.Body == nil {
		return nil, domain.Errorf(domain.CodeBadRequest, "пустое тело запроса")
	}

	order, err := h.orders.CancelByUser(ctx, request.PublicNumber, request.Body.Reason)
	if err != nil {
		return nil, err
	}

	return gen.CancelOrder200JSONResponse(toAPIOrder(order)), nil
}
