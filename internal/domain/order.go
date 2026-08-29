package domain

import (
	"time"

	"github.com/google/uuid"
)

// Actor — кто инициировал изменение статуса заказа.
type Actor string

const (
	// ActorUser — действие пользователя из веб-клиента.
	ActorUser Actor = "user"
	// ActorRestaurant — действие заведения через B2B API.
	ActorRestaurant Actor = "restaurant"
	// ActorSystem — автоматическое действие платформы (например, отмена по таймауту).
	ActorSystem Actor = "system"
)

// Valid сообщает, входит ли значение в допустимое множество акторов.
func (a Actor) Valid() bool {
	switch a {
	case ActorUser, ActorRestaurant, ActorSystem:
		return true
	default:
		return false
	}
}

// Order — заказ пользователя. Суммы зафиксированы на момент оформления и
// пересчёту не подлежат.
type Order struct {
	ID                 int64
	PublicNumber       uuid.UUID
	UserExternalID     string
	RestaurantID       int64
	RestaurantSlug     string
	Status             OrderStatus
	DeliveryAddress    string
	SubtotalKopecks    int64
	DeliveryFeeKopecks int64
	TotalKopecks       int64
	CancelReason       *string
	// Version — счётчик оптимистичной блокировки; каждое обновление статуса
	// увеличивает его на единицу.
	Version   int32
	CreatedAt time.Time
	UpdatedAt time.Time

	Items    []OrderItem
	Timeline []StatusEvent
}

// OrderItem — снапшот позиции на момент оформления заказа. Название и цена
// скопированы из меню, чтобы последующая синхронизация каталога не переписала
// историю (ADR-0002).
type OrderItem struct {
	ID int64
	// ProductID может быть nil, если позиция была удалена из каталога.
	ProductID           *int64
	ProductKey          string
	ProductNameSnapshot string
	UnitPriceKopecks    int64
	Qty                 int32
	LineTotalKopecks    int64
}

// StatusEvent — запись в истории переходов заказа.
type StatusEvent struct {
	ID int64
	// FromStatus пуст у самого первого события (создание заказа).
	FromStatus OrderStatus
	ToStatus   OrderStatus
	Actor      Actor
	Comment    string
	CreatedAt  time.Time
}

// DraftItem — позиция корзины из запроса клиента. Цену клиент не передаёт:
// сервер считает её сам по опубликованному меню.
type DraftItem struct {
	ProductKey string
	Qty        int32
}

// OrderDraft — намерение оформить заказ, ещё не прошедшее валидацию.
type OrderDraft struct {
	UserExternalID  string
	RestaurantID    int64
	DeliveryAddress string
	Items           []DraftItem
}

// Validate проверяет структурные инварианты корзины: пустой заказ, нулевые
// количества и дубли артикулов. Контракт ловит часть этого сам, но use case не
// должен зависеть от того, кто его вызвал.
func (d OrderDraft) Validate() error {
	if d.UserExternalID == "" {
		return Errorf(CodeValidationError, "не указан user_external_id")
	}
	if d.DeliveryAddress == "" {
		return Errorf(CodeValidationError, "не указан адрес доставки")
	}
	if len(d.Items) == 0 {
		return Errorf(CodeValidationError, "корзина пуста")
	}

	seen := make(map[string]struct{}, len(d.Items))
	for _, item := range d.Items {
		if item.ProductKey == "" {
			return Errorf(CodeValidationError, "у позиции не указан product_key")
		}
		if item.Qty <= 0 {
			return Errorf(CodeValidationError,
				"количество для «%s» должно быть больше нуля", item.ProductKey)
		}
		if item.Qty > MaxItemQty {
			return Errorf(CodeValidationError,
				"количество для «%s» не может превышать %d", item.ProductKey, MaxItemQty)
		}
		if _, dup := seen[item.ProductKey]; dup {
			return Errorf(CodeValidationError,
				"позиция «%s» указана в корзине дважды — объедините количества",
				item.ProductKey)
		}
		seen[item.ProductKey] = struct{}{}
	}

	return nil
}
