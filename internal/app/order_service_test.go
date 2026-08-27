package app_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"avito-kitchen/internal/app"
	"avito-kitchen/internal/domain"
)

func TestOrderService_Create_HappyPath(t *testing.T) {
	t.Parallel()

	env := newTestEnv()

	order, err := env.Orders.Create(context.Background(), draft(
		domain.DraftItem{ProductKey: "pizza_margherita", Qty: 2},
	))
	require.NoError(t, err)

	assert.Equal(t, domain.StatusNew, order.Status)
	assert.Equal(t, int32(1), order.Version)
	assert.NotEqual(t, uuid.Nil, order.PublicNumber)

	// Сумма считается по ценам из меню: 2 × 590 ₽ + 150 ₽ доставки.
	assert.Equal(t, int64(118000), order.SubtotalKopecks)
	assert.Equal(t, int64(15000), order.DeliveryFeeKopecks)
	assert.Equal(t, int64(133000), order.TotalKopecks)

	require.Len(t, order.Items, 1)
	assert.Equal(t, "Пицца Маргарита", order.Items[0].ProductNameSnapshot)
	assert.Equal(t, int64(59000), order.Items[0].UnitPriceKopecks)
	assert.Equal(t, int64(118000), order.Items[0].LineTotalKopecks)

	assert.Equal(t, int32(8), *env.StockOf(11), "остаток списан")
	assert.Equal(t, []string{app.EventOrderCreated}, env.OutboxTypes())

	require.Len(t, order.Timeline, 1)
	assert.Equal(t, domain.StatusNew, order.Timeline[0].ToStatus)
}

func TestOrderService_Create_UnlimitedStockIsNotDecremented(t *testing.T) {
	t.Parallel()

	env := newTestEnv()

	_, err := env.Orders.Create(context.Background(), draft(
		domain.DraftItem{ProductKey: "pasta_carbonara", Qty: 3},
	))
	require.NoError(t, err)

	assert.Nil(t, env.StockOf(12), "позиция без ограничения остатка не трогается")
}

func TestOrderService_Create_OutOfStock(t *testing.T) {
	t.Parallel()

	env := newTestEnv()

	_, err := env.Orders.Create(context.Background(), draft(
		domain.DraftItem{ProductKey: "pizza_margherita", Qty: 11},
	))
	require.Error(t, err)
	assert.Equal(t, domain.CodeOutOfStock, domain.CodeOf(err))

	assert.Equal(t, int32(10), *env.StockOf(11), "остаток не изменился")
	assert.Empty(t, env.OutboxTypes(), "событие не отправлено")
	assert.Empty(t, env.State().Orders, "заказ не создан")
}

// Ключевой тест на транзакционность: первая позиция списывается успешно,
// вторая — падает. Откат обязан вернуть остаток первой.
func TestOrderService_Create_RollsBackPartialStockDecrement(t *testing.T) {
	t.Parallel()

	env := newTestEnv()

	_, err := env.Orders.Create(context.Background(), draft(
		domain.DraftItem{ProductKey: "pizza_margherita", Qty: 1},
		domain.DraftItem{ProductKey: "drink_cola", Qty: 5}, // в наличии только 2
	))
	require.Error(t, err)
	assert.Equal(t, domain.CodeOutOfStock, domain.CodeOf(err))

	assert.Equal(t, int32(10), *env.StockOf(11), "списание первой позиции откатилось")
	assert.Equal(t, int32(2), *env.StockOf(14))
	assert.Empty(t, env.State().Orders)
}

func TestOrderService_Create_ProductUnavailable(t *testing.T) {
	t.Parallel()

	env := newTestEnv()

	_, err := env.Orders.Create(context.Background(), draft(
		domain.DraftItem{ProductKey: "dessert_tiramisu", Qty: 1},
	))
	require.Error(t, err)
	assert.Equal(t, domain.CodeProductUnavailable, domain.CodeOf(err))
	assert.Equal(t, int32(5), *env.StockOf(13), "у недоступной позиции остаток не трогается")
}

func TestOrderService_Create_UnknownProduct(t *testing.T) {
	t.Parallel()

	env := newTestEnv()

	_, err := env.Orders.Create(context.Background(), draft(
		domain.DraftItem{ProductKey: "pizza_with_pineapple", Qty: 1},
	))
	require.Error(t, err)
	assert.Equal(t, domain.CodeProductUnavailable, domain.CodeOf(err))
}

func TestOrderService_Create_MinOrderNotMet(t *testing.T) {
	t.Parallel()

	env := newTestEnv()

	// 120 ₽ при минимуме 500 ₽.
	_, err := env.Orders.Create(context.Background(), draft(
		domain.DraftItem{ProductKey: "drink_cola", Qty: 1},
	))
	require.Error(t, err)
	assert.Equal(t, domain.CodeMinOrderNotMet, domain.CodeOf(err))

	assert.Equal(t, int32(2), *env.StockOf(14), "остаток вернулся при откате")
}

// Стоимость доставки не участвует в проверке минимальной суммы: 490 ₽ товаров
// плюс 150 ₽ доставки дают 640 ₽, но минимум считается по товарам и не пройден.
func TestOrderService_Create_DeliveryFeeDoesNotCountTowardMinimum(t *testing.T) {
	t.Parallel()

	env := newTestEnv()

	_, err := env.Orders.Create(context.Background(), draft(
		domain.DraftItem{ProductKey: "pasta_carbonara", Qty: 1},
	))
	require.Error(t, err)
	assert.Equal(t, domain.CodeMinOrderNotMet, domain.CodeOf(err))
}

func TestOrderService_Create_RestaurantUnavailable(t *testing.T) {
	t.Parallel()

	env := newTestEnv()

	d := draft(domain.DraftItem{ProductKey: "roll_philadelphia", Qty: 2})
	d.RestaurantID = 2 // заведение в статусе paused

	_, err := env.Orders.Create(context.Background(), d)
	require.Error(t, err)
	assert.Equal(t, domain.CodeRestaurantUnavailable, domain.CodeOf(err))
	assert.Equal(t, int32(8), *env.StockOf(21))
}

func TestOrderService_Create_RestaurantNotFound(t *testing.T) {
	t.Parallel()

	env := newTestEnv()

	d := draft(domain.DraftItem{ProductKey: "pizza_margherita", Qty: 1})
	d.RestaurantID = 999

	_, err := env.Orders.Create(context.Background(), d)
	require.Error(t, err)
	assert.Equal(t, domain.CodeRestaurantNotFound, domain.CodeOf(err))
}

func TestOrderService_Create_ValidationErrors(t *testing.T) {
	t.Parallel()

	env := newTestEnv()

	_, err := env.Orders.Create(context.Background(), draft())
	require.Error(t, err)
	assert.Equal(t, domain.CodeValidationError, domain.CodeOf(err))
}

func TestOrderService_CancelByUser(t *testing.T) {
	t.Parallel()

	env := newTestEnv()
	ctx := context.Background()

	order, err := env.Orders.Create(ctx, draft(
		domain.DraftItem{ProductKey: "pizza_margherita", Qty: 2},
	))
	require.NoError(t, err)
	require.Equal(t, int32(8), *env.StockOf(11))

	cancelled, err := env.Orders.CancelByUser(ctx, order.PublicNumber, "Передумал")
	require.NoError(t, err)

	assert.Equal(t, domain.StatusCancelled, cancelled.Status)
	assert.Equal(t, int32(2), cancelled.Version, "версия увеличилась")
	require.NotNil(t, cancelled.CancelReason)
	assert.Equal(t, "Передумал", *cancelled.CancelReason)

	assert.Equal(t, int32(10), *env.StockOf(11), "остатки вернулись в каталог")
	assert.Equal(t, []string{app.EventOrderCreated, app.EventOrderCancelled}, env.OutboxTypes())

	require.Len(t, cancelled.Timeline, 2)
	assert.Equal(t, domain.ActorUser, cancelled.Timeline[1].Actor)
}

func TestOrderService_CancelByUser_TooLate(t *testing.T) {
	t.Parallel()

	env := newTestEnv()
	ctx := context.Background()

	order, err := env.Orders.Create(ctx, draft(
		domain.DraftItem{ProductKey: "pizza_margherita", Qty: 2},
	))
	require.NoError(t, err)

	_, err = env.Orders.ChangeStatus(ctx, app.StatusChange{
		PublicNumber: order.PublicNumber,
		To:           domain.StatusAccepted,
		Actor:        domain.ActorRestaurant,
		RestaurantID: 1,
	})
	require.NoError(t, err)

	_, err = env.Orders.CancelByUser(ctx, order.PublicNumber, "Передумал")
	require.Error(t, err)
	assert.Equal(t, domain.CodeOrderCannotBeCancelled, domain.CodeOf(err))

	assert.Equal(t, int32(8), *env.StockOf(11), "остатки остались списанными")
}

func TestOrderService_ChangeStatus_FullPipeline(t *testing.T) {
	t.Parallel()

	env := newTestEnv()
	ctx := context.Background()

	order, err := env.Orders.Create(ctx, draft(
		domain.DraftItem{ProductKey: "pizza_margherita", Qty: 2},
	))
	require.NoError(t, err)

	pipeline := []domain.OrderStatus{
		domain.StatusAccepted, domain.StatusCooking,
		domain.StatusReady, domain.StatusDelivered,
	}

	var current domain.Order
	for _, status := range pipeline {
		current, err = env.Orders.ChangeStatus(ctx, app.StatusChange{
			PublicNumber: order.PublicNumber,
			To:           status,
			Actor:        domain.ActorRestaurant,
			RestaurantID: 1,
		})
		require.NoErrorf(t, err, "переход в %s", status)
		assert.Equal(t, status, current.Status)
	}

	assert.Equal(t, int32(5), current.Version, "версия росла на каждом переходе")
	assert.Len(t, current.Timeline, 5, "создание + четыре перехода")
	assert.Equal(t, int32(8), *env.StockOf(11), "доставленный заказ остатки не возвращает")
	assert.Equal(t, []string{app.EventOrderCreated}, env.OutboxTypes())
}

func TestOrderService_ChangeStatus_RejectRestoresStock(t *testing.T) {
	t.Parallel()

	env := newTestEnv()
	ctx := context.Background()

	order, err := env.Orders.Create(ctx, draft(
		domain.DraftItem{ProductKey: "pizza_margherita", Qty: 3},
	))
	require.NoError(t, err)
	require.Equal(t, int32(7), *env.StockOf(11))

	rejected, err := env.Orders.ChangeStatus(ctx, app.StatusChange{
		PublicNumber: order.PublicNumber,
		To:           domain.StatusRejected,
		Actor:        domain.ActorRestaurant,
		RestaurantID: 1,
		Reason:       "нет продуктов",
	})
	require.NoError(t, err)

	assert.Equal(t, domain.StatusRejected, rejected.Status)
	assert.Equal(t, int32(10), *env.StockOf(11))
	assert.Equal(t, []string{app.EventOrderCreated, app.EventOrderCancelled}, env.OutboxTypes())
}

// Отмена после принятия — прерогатива заведения; остатки всё равно
// возвращаются в каталог.
func TestOrderService_ChangeStatus_RestaurantCancelsAfterAccept(t *testing.T) {
	t.Parallel()

	env := newTestEnv()
	ctx := context.Background()

	order, err := env.Orders.Create(ctx, draft(
		domain.DraftItem{ProductKey: "pizza_margherita", Qty: 4},
	))
	require.NoError(t, err)

	_, err = env.Orders.ChangeStatus(ctx, app.StatusChange{
		PublicNumber: order.PublicNumber,
		To:           domain.StatusCooking,
		Actor:        domain.ActorRestaurant,
		RestaurantID: 1,
	})
	require.Error(t, err, "нельзя перепрыгнуть ACCEPTED")

	_, err = env.Orders.ChangeStatus(ctx, app.StatusChange{
		PublicNumber: order.PublicNumber,
		To:           domain.StatusAccepted,
		Actor:        domain.ActorRestaurant,
		RestaurantID: 1,
	})
	require.NoError(t, err)

	cancelled, err := env.Orders.ChangeStatus(ctx, app.StatusChange{
		PublicNumber: order.PublicNumber,
		To:           domain.StatusCancelled,
		Actor:        domain.ActorRestaurant,
		RestaurantID: 1,
		Reason:       "сломалась печь",
	})
	require.NoError(t, err)

	assert.Equal(t, domain.StatusCancelled, cancelled.Status)
	assert.Equal(t, int32(10), *env.StockOf(11))
}

func TestOrderService_ChangeStatus_ForeignOrderIsForbidden(t *testing.T) {
	t.Parallel()

	env := newTestEnv()
	ctx := context.Background()

	order, err := env.Orders.Create(ctx, draft(
		domain.DraftItem{ProductKey: "pizza_margherita", Qty: 2},
	))
	require.NoError(t, err)

	_, err = env.Orders.ChangeStatus(ctx, app.StatusChange{
		PublicNumber: order.PublicNumber,
		To:           domain.StatusAccepted,
		Actor:        domain.ActorRestaurant,
		RestaurantID: 2, // чужое заведение
	})
	require.Error(t, err)
	assert.Equal(t, domain.CodeForbidden, domain.CodeOf(err))
}

func TestOrderService_ChangeStatus_OrderNotFound(t *testing.T) {
	t.Parallel()

	env := newTestEnv()

	_, err := env.Orders.Get(context.Background(), uuid.New())
	require.Error(t, err)
	assert.Equal(t, domain.CodeOrderNotFound, domain.CodeOf(err))
}

func TestOrderService_ChangeStatus_SystemCancelsStaleOrder(t *testing.T) {
	t.Parallel()

	env := newTestEnv()
	ctx := context.Background()

	order, err := env.Orders.Create(ctx, draft(
		domain.DraftItem{ProductKey: "pizza_margherita", Qty: 2},
	))
	require.NoError(t, err)

	cancelled, err := env.Orders.ChangeStatus(ctx, app.StatusChange{
		PublicNumber: order.PublicNumber,
		To:           domain.StatusCancelled,
		Actor:        domain.ActorSystem,
		Reason:       "заведение не подтвердило заказ вовремя",
	})
	require.NoError(t, err)

	assert.Equal(t, domain.StatusCancelled, cancelled.Status)
	assert.Equal(t, domain.ActorSystem, cancelled.Timeline[1].Actor)
	assert.Equal(t, int32(10), *env.StockOf(11))
}
