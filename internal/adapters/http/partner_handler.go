package http

import (
	"context"

	"avito-kitchen/internal/app"
	"avito-kitchen/internal/domain"
	"avito-kitchen/internal/gen"
	"avito-kitchen/internal/platform/middleware"
)

// PartnerHandler обслуживает B2B-часть контракта.
//
// Заведение берётся из контекста, куда его положила прослойка PartnerAuth:
// хендлер не принимает идентификатор заведения из тела или пути, поэтому
// подделать чужой restaurant_id в запросе невозможно.
type PartnerHandler struct {
	partners *app.PartnerService
	orders   *app.OrderService
}

// NewPartnerHandler собирает обработчик партнёрского API.
func NewPartnerHandler(partners *app.PartnerService, orders *app.OrderService) *PartnerHandler {
	return &PartnerHandler{partners: partners, orders: orders}
}

// SyncMenu — POST /api/v1/partner/menu/sync.
func (h *PartnerHandler) SyncMenu(
	ctx context.Context,
	request gen.SyncMenuRequestObject,
) (gen.SyncMenuResponseObject, error) {
	restaurant, err := currentRestaurant(ctx)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, domain.Errorf(domain.CodeBadRequest, "пустое тело запроса")
	}

	result, err := h.partners.SyncMenu(ctx, restaurant.ID,
		toDomainSyncProducts(gen.MenuSyncRequest(*request.Body)))
	if err != nil {
		return nil, err
	}

	return gen.SyncMenu200JSONResponse{
		Version:     result.Version,
		ItemsSynced: result.ItemsSynced,
	}, nil
}

// ListPartnerOrders — GET /api/v1/partner/orders.
func (h *PartnerHandler) ListPartnerOrders(
	ctx context.Context,
	request gen.ListPartnerOrdersRequestObject,
) (gen.ListPartnerOrdersResponseObject, error) {
	restaurant, err := currentRestaurant(ctx)
	if err != nil {
		return nil, err
	}

	var status *domain.OrderStatus
	if request.Params.Status != nil {
		s := domain.OrderStatus(*request.Params.Status)
		status = &s
	}

	var limit int32
	if request.Params.Limit != nil {
		limit = *request.Params.Limit
	}

	orders, err := h.partners.ListOrders(ctx, restaurant.ID, status, limit)
	if err != nil {
		return nil, err
	}

	return gen.ListPartnerOrders200JSONResponse{Items: toAPIOrders(orders)}, nil
}

// UpdateOrderStatus — POST /api/v1/partner/orders/{public_number}/status.
func (h *PartnerHandler) UpdateOrderStatus(
	ctx context.Context,
	request gen.UpdateOrderStatusRequestObject,
) (gen.UpdateOrderStatusResponseObject, error) {
	restaurant, err := currentRestaurant(ctx)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, domain.Errorf(domain.CodeBadRequest, "пустое тело запроса")
	}

	comment := ""
	if request.Body.Comment != nil {
		comment = *request.Body.Comment
	}

	order, err := h.orders.ChangeStatus(ctx, app.StatusChange{
		PublicNumber: request.PublicNumber,
		To:           domain.OrderStatus(request.Body.Status),
		Actor:        domain.ActorRestaurant,
		Comment:      comment,
		Reason:       comment,
		RestaurantID: restaurant.ID,
	})
	if err != nil {
		return nil, err
	}

	return gen.UpdateOrderStatus200JSONResponse(toAPIOrder(order)), nil
}

// SetKitchenStatus — POST /api/v1/partner/kitchen-status.
func (h *PartnerHandler) SetKitchenStatus(
	ctx context.Context,
	request gen.SetKitchenStatusRequestObject,
) (gen.SetKitchenStatusResponseObject, error) {
	restaurant, err := currentRestaurant(ctx)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, domain.Errorf(domain.CodeBadRequest, "пустое тело запроса")
	}

	status, err := h.partners.SetKitchenStatus(ctx, restaurant.ID,
		domain.RestaurantStatus(request.Body.Status))
	if err != nil {
		return nil, err
	}

	return gen.SetKitchenStatus200JSONResponse{Status: gen.RestaurantStatus(status)}, nil
}

// currentRestaurant достаёт заведение, аутентифицированное прослойкой.
//
// Отсутствие значения означает ошибку сборки роутера (партнёрский маршрут без
// PartnerAuth), а не ошибку клиента, — но отдаём 401, чтобы не раскрывать
// внутреннее устройство.
func currentRestaurant(ctx context.Context) (domain.Restaurant, error) {
	restaurant, ok := middleware.RestaurantFromContext(ctx)
	if !ok {
		return domain.Restaurant{}, domain.Errorf(domain.CodeUnauthorized,
			"заведение не аутентифицировано")
	}
	return restaurant, nil
}
