package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"avito-kitchen/internal/domain"
)

const (
	defaultPartnerOrdersLimit = 50
	maxPartnerOrdersLimit     = 200
)

// PartnerService — сценарии B2B-контура: аутентификация заведения,
// синхронизация меню, очередь заказов и переключение режима кухни.
type PartnerService struct {
	tx          TxManager
	restaurants RestaurantRepo
	menus       MenuRepo
	orders      OrderRepo
}

// NewPartnerService собирает сервис партнёрского API.
func NewPartnerService(
	tx TxManager,
	restaurants RestaurantRepo,
	menus MenuRepo,
	orders OrderRepo,
) *PartnerService {
	return &PartnerService{tx: tx, restaurants: restaurants, menus: menus, orders: orders}
}

// HashToken считает SHA-256 партнёрского токена в hex-виде. В БД хранится
// только хэш: утечка дампа не даёт доступа к API (ADR-0004).
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Authenticate находит заведение по партнёрскому токену.
func (s *PartnerService) Authenticate(ctx context.Context, token string) (domain.Restaurant, error) {
	if token == "" {
		return domain.Restaurant{}, domain.Errorf(domain.CodeUnauthorized,
			"не передан заголовок X-Partner-Token")
	}

	restaurant, err := s.restaurants.GetByAPIKeyHash(ctx, HashToken(token))
	if err != nil {
		// Не различаем «нет такого токена» и «ошибка БД» в тексте наружу:
		// это ответ на неаутентифицированный запрос.
		if domain.HasCode(err, domain.CodeRestaurantNotFound) {
			return domain.Restaurant{}, domain.Errorf(domain.CodeUnauthorized,
				"партнёрский токен не распознан")
		}
		return domain.Restaurant{}, err
	}

	return restaurant, nil
}

// SyncMenuResult — итог синхронизации меню.
type SyncMenuResult struct {
	Version     int32
	ItemsSynced int32
}

// SyncMenu публикует новую версию меню заведения.
//
// Полная замена, а не частичное обновление: заведение — источник правды по
// своему ассортименту, и «отсутствие позиции в присланном меню» однозначно
// означает, что позиции больше нет. Предыдущая версия архивируется, а не
// удаляется, поэтому исторические заказы сохраняют ссылки на свои позиции.
func (s *PartnerService) SyncMenu(
	ctx context.Context,
	restaurantID int64,
	products []domain.Product,
) (SyncMenuResult, error) {
	if len(products) == 0 {
		return SyncMenuResult{}, domain.Errorf(domain.CodeValidationError,
			"меню не может быть пустым")
	}
	if err := validateSyncProducts(products); err != nil {
		return SyncMenuResult{}, err
	}

	var menu domain.Menu
	err := s.tx.WithinTx(ctx, func(ctx context.Context) error {
		var err error
		menu, err = s.menus.PublishVersion(ctx, restaurantID, products)
		return err
	})
	if err != nil {
		return SyncMenuResult{}, err
	}

	return SyncMenuResult{Version: menu.Version, ItemsSynced: int32(len(products))}, nil
}

func validateSyncProducts(products []domain.Product) error {
	seen := make(map[string]struct{}, len(products))

	for _, p := range products {
		switch {
		case p.ProductKey == "":
			return domain.Errorf(domain.CodeValidationError, "у позиции не указан product_key")
		case p.Name == "":
			return domain.Errorf(domain.CodeValidationError,
				"у позиции «%s» не указано название", p.ProductKey)
		case p.Category == "":
			return domain.Errorf(domain.CodeValidationError,
				"у позиции «%s» не указана категория", p.ProductKey)
		case p.PriceKopecks < 0:
			return domain.Errorf(domain.CodeValidationError,
				"отрицательная цена у позиции «%s»", p.ProductKey)
		case p.StockQty != nil && *p.StockQty < 0:
			return domain.Errorf(domain.CodeValidationError,
				"отрицательный остаток у позиции «%s»", p.ProductKey)
		}

		if _, dup := seen[p.ProductKey]; dup {
			return domain.Errorf(domain.CodeValidationError,
				"позиция «%s» встречается в меню дважды", p.ProductKey)
		}
		seen[p.ProductKey] = struct{}{}
	}

	return nil
}

// ListOrders отдаёт очередь заказов заведения.
func (s *PartnerService) ListOrders(
	ctx context.Context,
	restaurantID int64,
	status *domain.OrderStatus,
	limit int32,
) ([]domain.Order, error) {
	if status != nil && !status.Valid() {
		return nil, domain.Errorf(domain.CodeBadRequest, "неизвестный статус заказа: %q", *status)
	}

	return s.orders.ListByRestaurant(ctx, OrderFilter{
		RestaurantID: restaurantID,
		Status:       status,
		Limit:        normalizeLimit(limit, defaultPartnerOrdersLimit, maxPartnerOrdersLimit),
	})
}

// SetKitchenStatus переключает режим работы кухни.
func (s *PartnerService) SetKitchenStatus(
	ctx context.Context,
	restaurantID int64,
	status domain.RestaurantStatus,
) (domain.RestaurantStatus, error) {
	if !status.Valid() {
		return "", domain.Errorf(domain.CodeValidationError,
			"неизвестный режим работы: %q (допустимо: online, paused, closed)", status)
	}

	if err := s.restaurants.UpdateStatus(ctx, restaurantID, status); err != nil {
		return "", err
	}

	return status, nil
}
