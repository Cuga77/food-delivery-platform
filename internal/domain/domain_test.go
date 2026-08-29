package domain_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"avito-kitchen/internal/domain"
)

func TestFormatKopecks(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   int64
		want string
	}{
		{0, "0,00 ₽"},
		{5, "0,05 ₽"},
		{99, "0,99 ₽"},
		{100, "1,00 ₽"},
		{59000, "590,00 ₽"},
		{123456, "1234,56 ₽"},
		{-2550, "-25,50 ₽"},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.in), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, domain.FormatKopecks(tt.in))
		})
	}
}

func TestLineTotal(t *testing.T) {
	t.Parallel()

	assert.Equal(t, int64(118000), domain.LineTotal(59000, 2))
	assert.Equal(t, int64(0), domain.LineTotal(0, 7))
	assert.Equal(t, int64(59000), domain.LineTotal(59000, 1))
}

func TestRestaurantStatus(t *testing.T) {
	t.Parallel()

	assert.True(t, domain.RestaurantOnline.AcceptsOrders())
	assert.False(t, domain.RestaurantPaused.AcceptsOrders())
	assert.False(t, domain.RestaurantClosed.AcceptsOrders())

	assert.True(t, domain.RestaurantOnline.Valid())
	assert.False(t, domain.RestaurantStatus("burning").Valid())
}

func TestRestaurant_EnsureAcceptsOrders(t *testing.T) {
	t.Parallel()

	online := domain.Restaurant{Name: "Пиццерия", Status: domain.RestaurantOnline}
	require.NoError(t, online.EnsureAcceptsOrders())

	paused := domain.Restaurant{Name: "Пиццерия", Status: domain.RestaurantPaused}
	err := paused.EnsureAcceptsOrders()
	require.Error(t, err)
	assert.Equal(t, domain.CodeRestaurantUnavailable, domain.CodeOf(err))
}

func TestRestaurant_EnsureMinOrder(t *testing.T) {
	t.Parallel()

	r := domain.Restaurant{Name: "Пиццерия", MinOrderKopecks: 50000}

	require.NoError(t, r.EnsureMinOrder(50000), "ровно минимум проходит")
	require.NoError(t, r.EnsureMinOrder(50001))

	err := r.EnsureMinOrder(49999)
	require.Error(t, err)
	assert.Equal(t, domain.CodeMinOrderNotMet, domain.CodeOf(err))
	assert.Contains(t, err.Error(), "500,00 ₽")
}

func TestProduct_EnsureOrderable(t *testing.T) {
	t.Parallel()

	available := domain.Product{Name: "Пицца", ProductKey: "pizza", Available: true}
	require.NoError(t, available.EnsureOrderable())

	hidden := domain.Product{Name: "Тирамису", ProductKey: "tiramisu", Available: false}
	err := hidden.EnsureOrderable()
	require.Error(t, err)
	assert.Equal(t, domain.CodeProductUnavailable, domain.CodeOf(err))
}

func TestProduct_HasFiniteStock(t *testing.T) {
	t.Parallel()

	qty := int32(3)
	assert.True(t, domain.Product{StockQty: &qty}.HasFiniteStock())
	assert.False(t, domain.Product{StockQty: nil}.HasFiniteStock())
}

func TestMenuSnapshot_GroupByCategory_PreservesOrder(t *testing.T) {
	t.Parallel()

	snapshot := domain.MenuSnapshot{Products: []domain.Product{
		{ProductKey: "pizza_1", Category: "Пицца"},
		{ProductKey: "drink_1", Category: "Напитки"},
		{ProductKey: "pizza_2", Category: "Пицца"},
		{ProductKey: "salad_1", Category: "Салаты"},
	}}

	categories := snapshot.GroupByCategory()

	require.Len(t, categories, 3)
	assert.Equal(t, "Пицца", categories[0].Name)
	assert.Len(t, categories[0].Products, 2)
	assert.Equal(t, "Напитки", categories[1].Name)
	assert.Equal(t, "Салаты", categories[2].Name)
}

func TestMenuSnapshot_ProductByKey(t *testing.T) {
	t.Parallel()

	snapshot := domain.MenuSnapshot{Products: []domain.Product{
		{ProductKey: "pizza_1", Name: "Маргарита"},
	}}

	p, ok := snapshot.ProductByKey("pizza_1")
	require.True(t, ok)
	assert.Equal(t, "Маргарита", p.Name)

	_, ok = snapshot.ProductByKey("unknown")
	assert.False(t, ok)
}

func TestOrderDraft_Validate(t *testing.T) {
	t.Parallel()

	valid := domain.OrderDraft{
		UserExternalID:  "usr_1",
		RestaurantID:    1,
		DeliveryAddress: "ул. Ленина, 10",
		Items:           []domain.DraftItem{{ProductKey: "pizza", Qty: 1}},
	}
	require.NoError(t, valid.Validate())

	tests := []struct {
		name  string
		mut   func(d *domain.OrderDraft)
		field string
	}{
		{"без пользователя", func(d *domain.OrderDraft) { d.UserExternalID = "" }, "user_external_id"},
		{"без адреса", func(d *domain.OrderDraft) { d.DeliveryAddress = "" }, "адрес"},
		{"пустая корзина", func(d *domain.OrderDraft) { d.Items = nil }, "пуста"},
		{"позиция без ключа", func(d *domain.OrderDraft) {
			d.Items = []domain.DraftItem{{ProductKey: "", Qty: 1}}
		}, "product_key"},
		{"нулевое количество", func(d *domain.OrderDraft) {
			d.Items = []domain.DraftItem{{ProductKey: "pizza", Qty: 0}}
		}, "больше нуля"},
		{"дубль артикула", func(d *domain.OrderDraft) {
			d.Items = []domain.DraftItem{
				{ProductKey: "pizza", Qty: 1},
				{ProductKey: "pizza", Qty: 2},
			}
		}, "дважды"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			draft := valid
			draft.Items = append([]domain.DraftItem(nil), valid.Items...)
			tt.mut(&draft)

			err := draft.Validate()
			require.Error(t, err)
			assert.Equal(t, domain.CodeValidationError, domain.CodeOf(err))
			assert.Contains(t, err.Error(), tt.field)
		})
	}
}

func TestError_WrappingAndCodes(t *testing.T) {
	t.Parallel()

	cause := errors.New("connection refused")
	err := domain.WrapErrorf(cause, domain.CodeServiceUnavailable, "БД недоступна")

	assert.Equal(t, domain.CodeServiceUnavailable, domain.CodeOf(err))
	assert.True(t, errors.Is(err, cause), "исходная ошибка сохраняется в цепочке")
	assert.True(t, domain.HasCode(err, domain.CodeServiceUnavailable))
	assert.False(t, domain.HasCode(err, domain.CodeOutOfStock))

	wrapped := fmt.Errorf("слой выше: %w", err)
	assert.Equal(t, domain.CodeServiceUnavailable, domain.CodeOf(wrapped),
		"код достаётся из любой глубины цепочки")

	assert.Equal(t, domain.CodeInternalError, domain.CodeOf(errors.New("что-то пошло не так")),
		"не-доменная ошибка не раскрывает деталей наружу")
}

func TestActor_Valid(t *testing.T) {
	t.Parallel()

	assert.True(t, domain.ActorUser.Valid())
	assert.True(t, domain.ActorRestaurant.Valid())
	assert.True(t, domain.ActorSystem.Valid())
	assert.False(t, domain.Actor("courier").Valid())
}

// Границы денег и количества существуют, чтобы произведение заведомо не
// переполнило int64. Без них заведение могло бы выставить цену в 9·10¹⁸, и
// заказ из двух позиций дал бы отрицательную сумму.
func TestOrderDraft_RejectsHugeQuantity(t *testing.T) {
	t.Parallel()

	draft := domain.OrderDraft{
		UserExternalID:  "usr_1",
		RestaurantID:    1,
		DeliveryAddress: "ул. Ленина, 10",
		Items:           []domain.DraftItem{{ProductKey: "pizza", Qty: domain.MaxItemQty + 1}},
	}

	err := draft.Validate()

	require.Error(t, err)
	assert.Equal(t, domain.CodeValidationError, domain.CodeOf(err))
	assert.Contains(t, err.Error(), "не может превышать")
}

func TestOrderDraft_AcceptsBoundaryQuantity(t *testing.T) {
	t.Parallel()

	draft := domain.OrderDraft{
		UserExternalID:  "usr_1",
		RestaurantID:    1,
		DeliveryAddress: "ул. Ленина, 10",
		Items:           []domain.DraftItem{{ProductKey: "pizza", Qty: domain.MaxItemQty}},
	}

	assert.NoError(t, draft.Validate())
}

// При заявленных границах произведение и сумма по сотне позиций остаются
// далеко от потолка int64 — это и есть обоснование, почему LineTotal не
// проверяет переполнение сам.
func TestLineTotal_CannotOverflowWithinBounds(t *testing.T) {
	t.Parallel()

	maxLine := domain.LineTotal(domain.MaxPriceKopecks, domain.MaxItemQty)

	assert.Positive(t, maxLine, "произведение границ не переполняется")
	assert.Equal(t, int64(100_000_000)*1_000, maxLine)

	// Сотня таких позиций — предел корзины по контракту.
	subtotal := maxLine * 100
	assert.Positive(t, subtotal)
	assert.Less(t, subtotal, int64(1)<<62, "запас до потолка int64 не меньше двух порядков")
}
