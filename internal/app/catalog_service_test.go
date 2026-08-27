package app_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"avito-kitchen/internal/app"
	"avito-kitchen/internal/domain"
)

func TestCursor_RoundTrip(t *testing.T) {
	t.Parallel()

	for _, id := range []int64{0, 1, 42, 9_223_372_036_854_775_807} {
		decoded, err := app.DecodeCursor(app.EncodeCursor(id))
		require.NoError(t, err)
		assert.Equal(t, id, decoded)
	}
}

func TestCursor_EmptyMeansFirstPage(t *testing.T) {
	t.Parallel()

	id, err := app.DecodeCursor("")
	require.NoError(t, err)
	assert.Equal(t, int64(0), id)
}

func TestCursor_RejectsGarbage(t *testing.T) {
	t.Parallel()

	// Курсор непрозрачен, поэтому сырое число тоже не принимается: клиент не
	// должен угадывать внутренний формат.
	for _, bad := range []string{"!!!not-base64!!!", "MTIz", "dj I6MQ", "42"} {
		_, err := app.DecodeCursor(bad)
		require.Errorf(t, err, "курсор %q должен быть отвергнут", bad)
		assert.Equal(t, domain.CodeBadRequest, domain.CodeOf(err))
	}
}

func TestCatalogService_ListRestaurants_Pagination(t *testing.T) {
	t.Parallel()

	env := newTestEnv()
	ctx := context.Background()

	first, err := env.Catalog.ListRestaurants(ctx, app.ListRestaurantsQuery{Limit: 1})
	require.NoError(t, err)
	require.Len(t, first.Items, 1)
	assert.Equal(t, "pizza-avito", first.Items[0].Slug)
	require.NotEmpty(t, first.NextCursor, "есть ещё данные — курсор обязателен")

	second, err := env.Catalog.ListRestaurants(ctx, app.ListRestaurantsQuery{
		Limit:  1,
		Cursor: first.NextCursor,
	})
	require.NoError(t, err)
	require.Len(t, second.Items, 1)
	assert.Equal(t, "sushi-avito", second.Items[0].Slug)
	assert.Empty(t, second.NextCursor, "данные закончились — курсора нет")
}

func TestCatalogService_ListRestaurants_DefaultLimit(t *testing.T) {
	t.Parallel()

	env := newTestEnv()

	page, err := env.Catalog.ListRestaurants(context.Background(), app.ListRestaurantsQuery{})
	require.NoError(t, err)
	assert.Len(t, page.Items, 2)
	assert.Empty(t, page.NextCursor)
}

func TestCatalogService_ListRestaurants_StatusFilter(t *testing.T) {
	t.Parallel()

	env := newTestEnv()
	online := domain.RestaurantOnline

	page, err := env.Catalog.ListRestaurants(context.Background(), app.ListRestaurantsQuery{
		Status: &online,
	})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	assert.Equal(t, "pizza-avito", page.Items[0].Slug)
}

func TestCatalogService_ListRestaurants_RejectsUnknownStatus(t *testing.T) {
	t.Parallel()

	env := newTestEnv()
	bogus := domain.RestaurantStatus("burning")

	_, err := env.Catalog.ListRestaurants(context.Background(), app.ListRestaurantsQuery{
		Status: &bogus,
	})
	require.Error(t, err)
	assert.Equal(t, domain.CodeBadRequest, domain.CodeOf(err))
}

func TestCatalogService_GetMenuBySlug(t *testing.T) {
	t.Parallel()

	env := newTestEnv()

	menu, err := env.Catalog.GetMenuBySlug(context.Background(), "pizza-avito")
	require.NoError(t, err)

	assert.Equal(t, "Пиццерия Авито", menu.Restaurant.Name)
	assert.Equal(t, int32(1), menu.MenuVersion)
	require.Len(t, menu.Categories, 4)
	assert.Equal(t, "Пицца", menu.Categories[0].Name)

	// Недоступная позиция остаётся на витрине с признаком available = false.
	var found bool
	for _, cat := range menu.Categories {
		for _, p := range cat.Products {
			if p.ProductKey == "dessert_tiramisu" {
				found = true
				assert.False(t, p.Available)
			}
		}
	}
	assert.True(t, found, "недоступная позиция не должна пропадать из меню")
}

func TestCatalogService_GetMenuBySlug_NotFound(t *testing.T) {
	t.Parallel()

	env := newTestEnv()

	_, err := env.Catalog.GetMenuBySlug(context.Background(), "nonexistent")
	require.Error(t, err)
	assert.Equal(t, domain.CodeRestaurantNotFound, domain.CodeOf(err))
}
