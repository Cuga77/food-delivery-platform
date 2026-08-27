package web

import (
	"errors"
	"fmt"
	"html/template"

	"avito-kitchen/internal/domain"
)

// templateFuncs — функции, доступные шаблонам.
//
// Форматирование денег берётся из домена, а не пересчитывается через float:
// в системе действует одно правило представления суммы, и витрина не имеет
// права округлять иначе, чем чек.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"money": domain.FormatKopecks,

		"orderStatusLabel": orderStatusLabel,
		"orderStatusColor": orderStatusColor,
		"isOrderStatus":    func(s domain.OrderStatus, want string) bool { return string(s) == want },

		"restaurantStatusLabel": restaurantStatusLabel,
		"restaurantStatusColor": restaurantStatusColor,

		"actorLabel": actorLabel,
		"shortID":    shortID,
		"hasStock":   func(qty *int32) bool { return qty != nil },

		"dict": dict,
	}
}

// dict собирает map для передачи нескольких значений во вложенный шаблон:
// у {{template}} в Go-шаблонах только один аргумент.
func dict(pairs ...any) (map[string]any, error) {
	if len(pairs)%2 != 0 {
		return nil, errors.New("dict: ожидается чётное число аргументов (ключ-значение)")
	}

	result := make(map[string]any, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		key, ok := pairs[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict: ключ %v не строка", pairs[i])
		}
		result[key] = pairs[i+1]
	}

	return result, nil
}

func orderStatusLabel(s domain.OrderStatus) string {
	switch s {
	case domain.StatusNew:
		return "Новый"
	case domain.StatusAccepted:
		return "Принят"
	case domain.StatusRejected:
		return "Отклонён"
	case domain.StatusCooking:
		return "Готовится"
	case domain.StatusReady:
		return "Готов"
	case domain.StatusDelivered:
		return "Доставлен"
	case domain.StatusCancelled:
		return "Отменён"
	default:
		return string(s)
	}
}

func orderStatusColor(s domain.OrderStatus) string {
	switch s {
	case domain.StatusNew:
		return "blue"
	case domain.StatusAccepted, domain.StatusCooking:
		return "orange"
	case domain.StatusReady:
		return "green"
	case domain.StatusDelivered:
		return "gray"
	case domain.StatusRejected, domain.StatusCancelled:
		return "red"
	default:
		return "gray"
	}
}

func restaurantStatusLabel(s domain.RestaurantStatus) string {
	switch s {
	case domain.RestaurantOnline:
		return "Открыто"
	case domain.RestaurantPaused:
		return "Пауза"
	case domain.RestaurantClosed:
		return "Закрыто"
	default:
		return string(s)
	}
}

func restaurantStatusColor(s domain.RestaurantStatus) string {
	switch s {
	case domain.RestaurantOnline:
		return "green"
	case domain.RestaurantPaused:
		return "orange"
	case domain.RestaurantClosed:
		return "red"
	default:
		return "gray"
	}
}

func actorLabel(a domain.Actor) string {
	switch a {
	case domain.ActorUser:
		return "Пользователь"
	case domain.ActorRestaurant:
		return "Заведение"
	case domain.ActorSystem:
		return "Платформа"
	default:
		return string(a)
	}
}

// shortID укорачивает UUID до первой группы — этого хватает, чтобы отличить
// заказ на экране, а целиком он остаётся в ссылке.
func shortID(id string) string {
	const shown = 8
	if len(id) <= shown {
		return id
	}
	return id[:shown]
}
