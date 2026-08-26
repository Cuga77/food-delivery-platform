package app

import (
	"encoding/json"
	"time"

	"avito-kitchen/internal/domain"
)

// Полезная нагрузка событий, уходящих заведению. Это часть внешнего контракта
// интеграции, поэтому структуры описаны явно, а не сериализуются из доменных
// сущностей: домен может меняться независимо от того, что видит партнёр.

// EventItem — позиция заказа в событии.
type EventItem struct {
	ProductKey       string `json:"product_key"`
	Name             string `json:"name"`
	UnitPriceKopecks int64  `json:"unit_price_kopecks"`
	Qty              int32  `json:"qty"`
	LineTotalKopecks int64  `json:"line_total_kopecks"`
}

// OrderCreatedPayload — событие о новом заказе.
type OrderCreatedPayload struct {
	PublicNumber       string      `json:"public_number"`
	RestaurantID       int64       `json:"restaurant_id"`
	RestaurantSlug     string      `json:"restaurant_slug"`
	UserExternalID     string      `json:"user_external_id"`
	Status             string      `json:"status"`
	DeliveryAddress    string      `json:"delivery_address"`
	Items              []EventItem `json:"items"`
	SubtotalKopecks    int64       `json:"subtotal_kopecks"`
	DeliveryFeeKopecks int64       `json:"delivery_fee_kopecks"`
	TotalKopecks       int64       `json:"total_kopecks"`
	CreatedAt          time.Time   `json:"created_at"`
}

// OrderCancelledPayload — событие об отмене или отказе. Заведению важно знать,
// кто инициировал отмену: пользователь, оно само или платформа по таймауту.
type OrderCancelledPayload struct {
	PublicNumber string    `json:"public_number"`
	RestaurantID int64     `json:"restaurant_id"`
	Status       string    `json:"status"`
	Actor        string    `json:"actor"`
	Reason       string    `json:"reason"`
	CancelledAt  time.Time `json:"cancelled_at"`
}

func newOrderCreatedEvent(order domain.Order) (OutboxEvent, error) {
	items := make([]EventItem, 0, len(order.Items))
	for _, it := range order.Items {
		items = append(items, EventItem{
			ProductKey:       it.ProductKey,
			Name:             it.ProductNameSnapshot,
			UnitPriceKopecks: it.UnitPriceKopecks,
			Qty:              it.Qty,
			LineTotalKopecks: it.LineTotalKopecks,
		})
	}

	payload, err := json.Marshal(OrderCreatedPayload{
		PublicNumber:       order.PublicNumber.String(),
		RestaurantID:       order.RestaurantID,
		RestaurantSlug:     order.RestaurantSlug,
		UserExternalID:     order.UserExternalID,
		Status:             string(order.Status),
		DeliveryAddress:    order.DeliveryAddress,
		Items:              items,
		SubtotalKopecks:    order.SubtotalKopecks,
		DeliveryFeeKopecks: order.DeliveryFeeKopecks,
		TotalKopecks:       order.TotalKopecks,
		CreatedAt:          order.CreatedAt,
	})
	if err != nil {
		return OutboxEvent{}, domain.WrapErrorf(err, domain.CodeInternalError,
			"не удалось сериализовать событие о создании заказа")
	}

	return OutboxEvent{
		AggregateType: AggregateOrder,
		AggregateID:   order.PublicNumber.String(),
		EventType:     EventOrderCreated,
		Payload:       payload,
	}, nil
}

func newOrderCancelledEvent(order domain.Order, actor domain.Actor, reason string, at time.Time) (OutboxEvent, error) {
	payload, err := json.Marshal(OrderCancelledPayload{
		PublicNumber: order.PublicNumber.String(),
		RestaurantID: order.RestaurantID,
		Status:       string(order.Status),
		Actor:        string(actor),
		Reason:       reason,
		CancelledAt:  at,
	})
	if err != nil {
		return OutboxEvent{}, domain.WrapErrorf(err, domain.CodeInternalError,
			"не удалось сериализовать событие об отмене заказа")
	}

	return OutboxEvent{
		AggregateType: AggregateOrder,
		AggregateID:   order.PublicNumber.String(),
		EventType:     EventOrderCancelled,
		Payload:       payload,
	}, nil
}
