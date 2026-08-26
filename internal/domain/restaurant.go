package domain

import "time"

// RestaurantStatus — режим работы заведения.
type RestaurantStatus string

const (
	// RestaurantOnline — заведение принимает заказы.
	RestaurantOnline RestaurantStatus = "online"
	// RestaurantPaused — временная остановка приёма (аврал на кухне).
	RestaurantPaused RestaurantStatus = "paused"
	// RestaurantClosed — заведение закрыто.
	RestaurantClosed RestaurantStatus = "closed"
)

// Valid сообщает, входит ли значение в допустимое множество. Дублирует
// CHECK-ограничение в БД, но позволяет отсечь мусор до похода в базу.
func (s RestaurantStatus) Valid() bool {
	switch s {
	case RestaurantOnline, RestaurantPaused, RestaurantClosed:
		return true
	default:
		return false
	}
}

// AcceptsOrders — единственное место, где решается, можно ли оформить заказ.
func (s RestaurantStatus) AcceptsOrders() bool {
	return s == RestaurantOnline
}

// Restaurant — заведение, подключённое к площадке.
type Restaurant struct {
	ID                 int64
	Slug               string
	Name               string
	Status             RestaurantStatus
	ProviderBaseURL    string
	MinOrderKopecks    int64
	DeliveryFeeKopecks int64
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// EnsureAcceptsOrders возвращает доменную ошибку, если заведение сейчас не
// принимает заказы.
func (r Restaurant) EnsureAcceptsOrders() error {
	if r.Status.AcceptsOrders() {
		return nil
	}
	return Errorf(CodeRestaurantUnavailable,
		"заведение «%s» сейчас не принимает заказы (режим: %s)", r.Name, r.Status)
}

// EnsureMinOrder проверяет, что сумма позиций дотягивает до минимального заказа.
// Стоимость доставки в проверку не входит — минимум считается по товарам.
func (r Restaurant) EnsureMinOrder(subtotalKopecks int64) error {
	if subtotalKopecks >= r.MinOrderKopecks {
		return nil
	}
	return Errorf(CodeMinOrderNotMet,
		"минимальная сумма заказа в «%s» — %s, в корзине %s",
		r.Name, FormatKopecks(r.MinOrderKopecks), FormatKopecks(subtotalKopecks))
}
