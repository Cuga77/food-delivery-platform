package app

import (
	"context"

	"avito-kitchen/internal/domain"
)

const (
	defaultRestaurantPageSize = 20
	maxRestaurantPageSize     = 100
)

// CatalogService обслуживает витрину: список заведений и меню конкретного
// заведения. Сценарии здесь read-only, транзакции не нужны.
type CatalogService struct {
	restaurants RestaurantRepo
	menus       MenuRepo
}

// NewCatalogService собирает сервис витрины.
func NewCatalogService(restaurants RestaurantRepo, menus MenuRepo) *CatalogService {
	return &CatalogService{restaurants: restaurants, menus: menus}
}

// RestaurantPage — страница каталога с курсором на следующую.
type RestaurantPage struct {
	Items []domain.Restaurant
	// NextCursor пуст, когда данные закончились.
	NextCursor string
}

// ListRestaurantsQuery — входные параметры выборки каталога.
type ListRestaurantsQuery struct {
	Limit  int32
	Cursor string
	Status *domain.RestaurantStatus
}

// ListRestaurants отдаёт страницу каталога.
//
// Чтобы понять, есть ли следующая страница, запрашиваем на одну запись больше
// лимита: лишняя запись не отдаётся клиенту, но её наличие означает, что курсор
// нужно вернуть.
func (s *CatalogService) ListRestaurants(ctx context.Context, q ListRestaurantsQuery) (RestaurantPage, error) {
	limit := normalizeLimit(q.Limit, defaultRestaurantPageSize, maxRestaurantPageSize)

	if q.Status != nil && !q.Status.Valid() {
		return RestaurantPage{}, domain.Errorf(domain.CodeBadRequest,
			"неизвестный режим работы заведения: %q", *q.Status)
	}

	afterID, err := DecodeCursor(q.Cursor)
	if err != nil {
		return RestaurantPage{}, err
	}

	items, err := s.restaurants.List(ctx, RestaurantFilter{
		AfterID: afterID,
		Limit:   limit + 1,
		Status:  q.Status,
	})
	if err != nil {
		return RestaurantPage{}, err
	}

	page := RestaurantPage{Items: items}
	if int32(len(items)) > limit {
		page.Items = items[:limit]
		page.NextCursor = EncodeCursor(page.Items[len(page.Items)-1].ID)
	}

	return page, nil
}

// RestaurantMenu — витрина меню: заведение, версия меню и позиции по категориям.
type RestaurantMenu struct {
	Restaurant  domain.Restaurant
	MenuVersion int32
	Categories  []domain.MenuCategory
}

// GetMenuBySlug отдаёт опубликованное меню заведения.
//
// Позиции с available == false остаются в выдаче: клиент должен видеть, что
// блюдо существует, но сейчас недоступно, — иначе оно просто «исчезает» из
// меню, и пользователь не понимает, что произошло.
func (s *CatalogService) GetMenuBySlug(ctx context.Context, slug string) (RestaurantMenu, error) {
	restaurant, err := s.restaurants.GetBySlug(ctx, slug)
	if err != nil {
		return RestaurantMenu{}, err
	}

	snapshot, err := s.menus.GetPublished(ctx, restaurant.ID)
	if err != nil {
		return RestaurantMenu{}, err
	}

	return RestaurantMenu{
		Restaurant:  restaurant,
		MenuVersion: snapshot.Menu.Version,
		Categories:  snapshot.GroupByCategory(),
	}, nil
}

func normalizeLimit(limit, fallback, maximum int32) int32 {
	switch {
	case limit <= 0:
		return fallback
	case limit > maximum:
		return maximum
	default:
		return limit
	}
}
