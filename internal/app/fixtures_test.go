package app_test

import (
	"avito-kitchen/internal/app/apptest"
	"avito-kitchen/internal/domain"
)

// Фикстуры вынесены в internal/app/apptest, чтобы их могли переиспользовать
// тесты веб-слоя. Здесь остаются только короткие псевдонимы для читаемости
// самих тестов.

type testEnv = apptest.Env

func newTestEnv() *testEnv { return apptest.New() }

func ptrInt32(v int32) *int32 { return apptest.Ptr(v) }

// draft собирает корзину для заведения pizza-avito.
func draft(items ...domain.DraftItem) domain.OrderDraft {
	return domain.OrderDraft{
		UserExternalID:  "usr_123",
		RestaurantID:    1,
		DeliveryAddress: "ул. Ленина, 10",
		Items:           items,
	}
}
