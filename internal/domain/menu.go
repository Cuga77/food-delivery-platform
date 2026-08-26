package domain

import "time"

// MenuStatus — состояние версии меню.
type MenuStatus string

const (
	// MenuDraft — версия готовится, витрине не показывается.
	MenuDraft MenuStatus = "draft"
	// MenuPublished — актуальная версия, по ней оформляются заказы.
	MenuPublished MenuStatus = "published"
	// MenuArchived — вытесненная версия; на неё продолжают ссылаться старые заказы.
	MenuArchived MenuStatus = "archived"
)

// Menu — версия меню заведения. Версии не перезаписываются: синхронизация
// создаёт N+1 и архивирует предыдущую (см. ADR-0002).
type Menu struct {
	ID           int64
	RestaurantID int64
	Version      int32
	Status       MenuStatus
	CreatedAt    time.Time
}

// Product — позиция конкретной версии меню.
type Product struct {
	ID           int64
	MenuID       int64
	ProductKey   string
	Category     string
	Name         string
	Description  string
	PriceKopecks int64
	Available    bool
	// StockQty == nil означает неограниченный остаток; такие позиции не
	// участвуют в списании и возврате остатков.
	StockQty *int32
}

// HasFiniteStock сообщает, нужно ли для этой позиции списывать остаток.
func (p Product) HasFiniteStock() bool { return p.StockQty != nil }

// EnsureOrderable проверяет, что позицию вообще можно положить в заказ.
// Проверка остатка здесь не делается — она атомарна и живёт в репозитории.
func (p Product) EnsureOrderable() error {
	if !p.Available {
		return Errorf(CodeProductUnavailable,
			"позиция «%s» (ключ: %s) сейчас недоступна", p.Name, p.ProductKey)
	}
	return nil
}

// MenuCategory — группа позиций для витрины.
type MenuCategory struct {
	Name     string
	Products []Product
}

// MenuSnapshot — опубликованное меню целиком: то, что видит клиент.
type MenuSnapshot struct {
	Menu     Menu
	Products []Product
}

// ProductByKey ищет позицию по артикулу заведения.
func (s MenuSnapshot) ProductByKey(key string) (Product, bool) {
	for _, p := range s.Products {
		if p.ProductKey == key {
			return p, true
		}
	}
	return Product{}, false
}

// GroupByCategory раскладывает позиции по категориям, сохраняя порядок их
// первого появления. Порядок задаётся заведением при синхронизации меню —
// это его способ управлять выкладкой на витрине.
func (s MenuSnapshot) GroupByCategory() []MenuCategory {
	indexByName := make(map[string]int, len(s.Products))
	categories := make([]MenuCategory, 0, len(s.Products))

	for _, p := range s.Products {
		idx, ok := indexByName[p.Category]
		if !ok {
			idx = len(categories)
			indexByName[p.Category] = idx
			categories = append(categories, MenuCategory{Name: p.Category})
		}
		categories[idx].Products = append(categories[idx].Products, p)
	}

	return categories
}
