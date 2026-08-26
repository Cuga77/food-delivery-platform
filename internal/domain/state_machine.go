package domain

import (
	"sort"
	"strings"
)

// OrderStatus — статус заказа в жизненном цикле.
type OrderStatus string

const (
	// StatusNew — заказ создан, остатки списаны, заведение ещё не ответило.
	StatusNew OrderStatus = "NEW"
	// StatusAccepted — заведение приняло заказ в работу.
	StatusAccepted OrderStatus = "ACCEPTED"
	// StatusRejected — заведение отказалось от заказа; остатки возвращаются.
	StatusRejected OrderStatus = "REJECTED"
	// StatusCooking — заказ готовится.
	StatusCooking OrderStatus = "COOKING"
	// StatusReady — заказ готов и передан курьеру.
	StatusReady OrderStatus = "READY"
	// StatusDelivered — заказ доставлен; терминальный успех.
	StatusDelivered OrderStatus = "DELIVERED"
	// StatusCancelled — заказ отменён; остатки возвращаются.
	StatusCancelled OrderStatus = "CANCELLED"
)

// Valid сообщает, входит ли значение в допустимое множество статусов.
func (s OrderStatus) Valid() bool {
	switch s {
	case StatusNew, StatusAccepted, StatusRejected,
		StatusCooking, StatusReady, StatusDelivered, StatusCancelled:
		return true
	default:
		return false
	}
}

// IsTerminal сообщает, что из статуса нет исходящих переходов.
func (s OrderStatus) IsTerminal() bool {
	return len(transitionsFrom[s]) == 0
}

// ReleasesStock сообщает, что переход в этот статус обязан вернуть списанные
// остатки в каталог. Проверяется целевым статусом, а не исходным: любой путь,
// приводящий заказ к отмене или отказу, освобождает резерв.
func (s OrderStatus) ReleasesStock() bool {
	return s == StatusRejected || s == StatusCancelled
}

// transitionsFrom — единственный источник правды по жизненному циклу заказа.
// Ключ верхнего уровня — исходный статус, ключ вложенной карты — целевой,
// значение — множество акторов, которым переход разрешён.
//
// Диаграмма этого графа генерируется отдельно: docs/diagrams/order_state_machine.puml.
var transitionsFrom = map[OrderStatus]map[OrderStatus][]Actor{
	StatusNew: {
		// Заведение отвечает на новый заказ: берёт в работу или отказывается.
		StatusAccepted: {ActorRestaurant},
		StatusRejected: {ActorRestaurant},
		// Пока заказ не принят, пользователь волен передумать. System отменяет
		// заказ, на который заведение не ответило за ORDER_ACCEPT_TIMEOUT.
		StatusCancelled: {ActorUser, ActorSystem},
	},
	StatusAccepted: {
		StatusCooking: {ActorRestaurant},
		// После принятия отмена — прерогатива заведения: продукты уже в работе.
		StatusCancelled: {ActorRestaurant},
	},
	StatusCooking: {
		StatusReady:     {ActorRestaurant},
		StatusCancelled: {ActorRestaurant},
	},
	StatusReady: {
		StatusDelivered: {ActorRestaurant},
	},
	// REJECTED, DELIVERED, CANCELLED — терминальные.
}

// CanTransition сообщает, разрешён ли переход указанному актору.
func CanTransition(from, to OrderStatus, actor Actor) bool {
	actors, ok := transitionsFrom[from][to]
	if !ok {
		return false
	}
	for _, a := range actors {
		if a == actor {
			return true
		}
	}
	return false
}

// AllowedNextStatuses перечисляет достижимые из from статусы (без учёта актора),
// отсортированные для стабильности вывода в текстах ошибок и тестах.
func AllowedNextStatuses(from OrderStatus) []OrderStatus {
	targets := transitionsFrom[from]
	result := make([]OrderStatus, 0, len(targets))
	for to := range targets {
		result = append(result, to)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

// ValidateTransition — то же, что CanTransition, но с внятной доменной ошибкой.
//
// Отдельный код ORDER_CANNOT_BE_CANCELLED выдаётся, когда пользователь пытается
// отменить уже принятый заказ: с точки зрения клиента это не «конфликт
// состояния», а понятное продуктовое правило, и в контракте оно описано отдельно.
func ValidateTransition(from, to OrderStatus, actor Actor) error {
	if !to.Valid() {
		return Errorf(CodeValidationError, "неизвестный статус заказа: %q", to)
	}
	if CanTransition(from, to, actor) {
		return nil
	}

	if to == StatusCancelled && actor == ActorUser {
		return Errorf(CodeOrderCannotBeCancelled,
			"заказ в статусе %s уже нельзя отменить самостоятельно — обратитесь в заведение", from)
	}

	if from.IsTerminal() {
		return Errorf(CodeStateConflict,
			"заказ уже в терминальном статусе %s, переход в %s невозможен", from, to)
	}

	return Errorf(CodeStateConflict,
		"переход %s → %s недоступен для роли %s; допустимые статусы: %s",
		from, to, actor, joinStatuses(AllowedNextStatuses(from)))
}

func joinStatuses(statuses []OrderStatus) string {
	parts := make([]string, len(statuses))
	for i, s := range statuses {
		parts[i] = string(s)
	}
	return strings.Join(parts, ", ")
}
