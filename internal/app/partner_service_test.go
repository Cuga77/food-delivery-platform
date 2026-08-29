package app_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"avito-kitchen/internal/app"
	"avito-kitchen/internal/app/apptest"
	"avito-kitchen/internal/domain"
)

func TestHashToken(t *testing.T) {
	t.Parallel()

	// Значение зафиксировано: тот же хэш лежит в migrations/000002_seed_demo.up.sql,
	// и расхождение сломало бы аутентификацию демо-заведения.
	assert.Equal(t,
		"296e205c464e4d1a0ff6657b598a1a3f37b4e191ad3c5d3be599d68cb766504e",
		app.HashToken("dev-partner-token"))

	assert.NotEqual(t, app.HashToken("a"), app.HashToken("b"))
	assert.Len(t, app.HashToken(""), 64)
}

func TestPartnerService_Authenticate(t *testing.T) {
	t.Parallel()

	env := newTestEnv()
	ctx := context.Background()

	restaurant, err := env.Partners.Authenticate(ctx, apptest.PartnerToken)
	require.NoError(t, err)
	assert.Equal(t, int64(1), restaurant.ID)

	_, err = env.Partners.Authenticate(ctx, "wrong-token")
	require.Error(t, err)
	assert.Equal(t, domain.CodeUnauthorized, domain.CodeOf(err))

	_, err = env.Partners.Authenticate(ctx, "")
	require.Error(t, err)
	assert.Equal(t, domain.CodeUnauthorized, domain.CodeOf(err))
}

func TestPartnerService_SyncMenu_PublishesNextVersion(t *testing.T) {
	t.Parallel()

	env := newTestEnv()
	ctx := context.Background()

	result, err := env.Partners.SyncMenu(ctx, 1, []domain.Product{
		{ProductKey: "pizza_margherita", Category: "Пицца", Name: "Пицца Маргарита",
			PriceKopecks: 62000, Available: true, StockQty: ptrInt32(20)},
		{ProductKey: "pizza_new", Category: "Пицца", Name: "Новинка",
			PriceKopecks: 71000, Available: true},
	})
	require.NoError(t, err)

	assert.Equal(t, int32(2), result.Version, "версия увеличилась на единицу")
	assert.Equal(t, int32(2), result.ItemsSynced)

	menu, err := env.Catalog.GetMenuBySlug(ctx, "pizza-avito")
	require.NoError(t, err)
	assert.Equal(t, int32(2), menu.MenuVersion)

	// Позиции, отсутствующие в новом меню, исчезают с витрины.
	require.Len(t, menu.Categories, 1)
	assert.Len(t, menu.Categories[0].Products, 2)
}

func TestPartnerService_SyncMenu_Validation(t *testing.T) {
	t.Parallel()

	valid := domain.Product{
		ProductKey: "p1", Category: "Пицца", Name: "Пицца", PriceKopecks: 1000,
	}

	tests := []struct {
		name     string
		products []domain.Product
	}{
		{"пустое меню", nil},
		{"без ключа", []domain.Product{{Category: "Пицца", Name: "Пицца"}}},
		{"без названия", []domain.Product{{ProductKey: "p1", Category: "Пицца"}}},
		{"без категории", []domain.Product{{ProductKey: "p1", Name: "Пицца"}}},
		{"отрицательная цена", []domain.Product{
			{ProductKey: "p1", Category: "Пицца", Name: "Пицца", PriceKopecks: -1},
		}},
		{"отрицательный остаток", []domain.Product{
			{ProductKey: "p1", Category: "Пицца", Name: "Пицца", StockQty: ptrInt32(-1)},
		}},
		{"дубль ключа", []domain.Product{valid, valid}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := newTestEnv()

			_, err := env.Partners.SyncMenu(context.Background(), 1, tt.products)
			require.Error(t, err)
			assert.Equal(t, domain.CodeValidationError, domain.CodeOf(err))
		})
	}
}

func TestPartnerService_SetKitchenStatus(t *testing.T) {
	t.Parallel()

	env := newTestEnv()
	ctx := context.Background()

	status, err := env.Partners.SetKitchenStatus(ctx, 1, domain.RestaurantClosed)
	require.NoError(t, err)
	assert.Equal(t, domain.RestaurantClosed, status)

	// Закрытая кухня перестаёт принимать заказы.
	_, err = env.Orders.Create(ctx, draft(domain.DraftItem{ProductKey: "pizza_margherita", Qty: 2}))
	require.Error(t, err)
	assert.Equal(t, domain.CodeRestaurantUnavailable, domain.CodeOf(err))

	// И снова начинает после возврата в online.
	_, err = env.Partners.SetKitchenStatus(ctx, 1, domain.RestaurantOnline)
	require.NoError(t, err)
	_, err = env.Orders.Create(ctx, draft(domain.DraftItem{ProductKey: "pizza_margherita", Qty: 2}))
	require.NoError(t, err)
}

func TestPartnerService_SetKitchenStatus_RejectsUnknown(t *testing.T) {
	t.Parallel()

	env := newTestEnv()

	_, err := env.Partners.SetKitchenStatus(context.Background(), 1, domain.RestaurantStatus("vacation"))
	require.Error(t, err)
	assert.Equal(t, domain.CodeValidationError, domain.CodeOf(err))
}

func TestPartnerService_ListOrders(t *testing.T) {
	t.Parallel()

	env := newTestEnv()
	ctx := context.Background()

	order, err := env.Orders.Create(ctx, draft(domain.DraftItem{ProductKey: "pizza_margherita", Qty: 2}))
	require.NoError(t, err)

	all, err := env.Partners.ListOrders(ctx, 1, nil, 0)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, order.PublicNumber, all[0].PublicNumber)

	newStatus := domain.StatusNew
	filtered, err := env.Partners.ListOrders(ctx, 1, &newStatus, 0)
	require.NoError(t, err)
	assert.Len(t, filtered, 1)

	ready := domain.StatusReady
	empty, err := env.Partners.ListOrders(ctx, 1, &ready, 0)
	require.NoError(t, err)
	assert.Empty(t, empty)

	// Чужие заказы в очередь заведения не попадают.
	foreign, err := env.Partners.ListOrders(ctx, 2, nil, 0)
	require.NoError(t, err)
	assert.Empty(t, foreign)
}

func TestPartnerService_ListOrders_RejectsUnknownStatus(t *testing.T) {
	t.Parallel()

	env := newTestEnv()
	bogus := domain.OrderStatus("FLYING")

	_, err := env.Partners.ListOrders(context.Background(), 1, &bogus, 0)
	require.Error(t, err)
	assert.Equal(t, domain.CodeBadRequest, domain.CodeOf(err))
}

// Цена сверх допустимой отклоняется на валидации, а не превращается в
// переполнение при расчёте суммы заказа.
func TestPartnerService_SyncMenu_RejectsHugePrice(t *testing.T) {
	t.Parallel()

	env := newTestEnv()

	_, err := env.Partners.SyncMenu(context.Background(), 1, []domain.Product{
		{ProductKey: "p1", Category: "Пицца", Name: "Золотая пицца",
			PriceKopecks: domain.MaxPriceKopecks + 1, Available: true},
	})

	require.Error(t, err)
	assert.Equal(t, domain.CodeValidationError, domain.CodeOf(err))
	assert.Contains(t, err.Error(), "превышает допустимую")
}
