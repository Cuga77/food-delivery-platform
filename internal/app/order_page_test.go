package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"avito-kitchen/internal/app"
	"avito-kitchen/internal/domain"
)

// --- Курсор списка заказов --------------------------------------------------

func TestOrderCursor_RoundTrip(t *testing.T) {
	t.Parallel()

	moments := []time.Time{
		time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC),
		// Микросекунды: ровно та точность, которую хранит timestamptz.
		time.Date(2026, 8, 30, 12, 0, 0, 123_456_000, time.UTC),
		// Другой часовой пояс — курсор обязан пережить перевод в UTC.
		time.Date(2026, 8, 30, 12, 0, 0, 0, time.FixedZone("MSK", 3*60*60)),
	}

	for _, moment := range moments {
		want := app.OrderCursor{CreatedAt: moment, ID: 4242}

		got, err := app.DecodeOrderCursor(app.EncodeOrderCursor(want))
		require.NoError(t, err)
		assert.True(t, want.CreatedAt.Equal(got.CreatedAt),
			"время не пережило кодирование: %s → %s", want.CreatedAt, got.CreatedAt)
		assert.Equal(t, want.ID, got.ID)
	}
}

func TestOrderCursor_EmptyMeansFirstPage(t *testing.T) {
	t.Parallel()

	cursor, err := app.DecodeOrderCursor("")
	require.NoError(t, err)
	assert.True(t, cursor.IsZero())
}

// Курсоры двух форматов не взаимозаменяемы: каталог упорядочен по id, заказы —
// по времени. Подставленный не туда курсор должен быть отвергнут, а не разобран
// как попало — иначе граница страницы окажется бессмысленной.
func TestOrderCursor_RejectsForeignFormats(t *testing.T) {
	t.Parallel()

	catalogCursor := app.EncodeCursor(42)

	_, err := app.DecodeOrderCursor(catalogCursor)
	require.Error(t, err, "курсор каталога не годится для списка заказов")
	assert.Equal(t, domain.CodeBadRequest, domain.CodeOf(err))

	orderCursor := app.EncodeOrderCursor(app.OrderCursor{CreatedAt: time.Now(), ID: 1})

	_, err = app.DecodeCursor(orderCursor)
	require.Error(t, err, "курсор заказов не годится для каталога")
	assert.Equal(t, domain.CodeBadRequest, domain.CodeOf(err))
}

func TestOrderCursor_RejectsGarbage(t *testing.T) {
	t.Parallel()

	bad := []string{
		"!!!not-base64!!!",
		"notacursor",
		"MTIz",           // base64 от "123": без префикса версии
		"djI6",           // "v2:" без полезной нагрузки
		"djI6MTIz",       // "v2:123": нет второй составляющей ключа
		"djI6MTIzOmFiYw", // "v2:123:abc": идентификатор не число
		"djI6MTIzOjA",    // "v2:123:0": нулевой id не бывает
	}

	for _, cursor := range bad {
		_, err := app.DecodeOrderCursor(cursor)
		require.Errorf(t, err, "курсор %q должен быть отвергнут", cursor)
		assert.Equal(t, domain.CodeBadRequest, domain.CodeOf(err))
	}
}

// --- История заказов пользователя -------------------------------------------

// placeOrders оформляет n заказов подряд и возвращает их номера в порядке
// оформления.
func placeOrders(t *testing.T, env *testEnv, user string, n int) []string {
	t.Helper()

	numbers := make([]string, 0, n)
	for range n {
		d := draft(domain.DraftItem{ProductKey: "pizza_margherita", Qty: 1})
		d.UserExternalID = user

		order, err := env.Orders.Create(context.Background(), d)
		require.NoError(t, err)
		numbers = append(numbers, order.PublicNumber.String())
	}

	return numbers
}

func TestOrderService_ListByUser_OnlyOwnOrders(t *testing.T) {
	t.Parallel()

	env := newTestEnv()
	mine := placeOrders(t, env, "usr_mine", 2)
	placeOrders(t, env, "usr_other", 3)

	page, err := env.Orders.ListByUser(context.Background(),
		app.ListUserOrdersQuery{UserExternalID: "usr_mine"})
	require.NoError(t, err)

	require.Len(t, page.Items, 2, "чужие заказы в историю не попадают")
	for _, order := range page.Items {
		assert.Equal(t, "usr_mine", order.UserExternalID)
		assert.Contains(t, mine, order.PublicNumber.String())
	}
	assert.Empty(t, page.NextCursor, "данные закончились — курсора быть не должно")
}

// Страница отдаётся свежими сверху и обходится курсором без пропусков и
// повторов. Проверяется весь обход целиком: ошибка в границе страницы обычно
// проявляется именно на стыке.
func TestOrderService_ListByUser_CursorWalksWholeHistory(t *testing.T) {
	t.Parallel()

	env := newTestEnv()
	const total = 7
	placed := placeOrders(t, env, "usr_mine", total)

	var (
		seen   []string
		cursor string
		pages  int
	)
	for {
		page, err := env.Orders.ListByUser(context.Background(), app.ListUserOrdersQuery{
			UserExternalID: "usr_mine",
			Limit:          2,
			Cursor:         cursor,
		})
		require.NoError(t, err)
		require.LessOrEqual(t, len(page.Items), 2, "страница не больше лимита")

		for _, order := range page.Items {
			seen = append(seen, order.PublicNumber.String())
		}

		pages++
		require.Less(t, pages, total+2, "обход не сходится")

		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	// Свежие первыми — порядок обратен порядку оформления.
	want := make([]string, 0, total)
	for i := len(placed) - 1; i >= 0; i-- {
		want = append(want, placed[i])
	}

	assert.Equal(t, want, seen, "обход по курсору вернул историю ровно один раз")
	assert.Equal(t, 4, pages, "7 заказов по 2 на страницу")
}

func TestOrderService_ListByUser_Validation(t *testing.T) {
	t.Parallel()

	env := newTestEnv()
	ctx := context.Background()

	_, err := env.Orders.ListByUser(ctx, app.ListUserOrdersQuery{})
	require.Error(t, err, "без идентификатора история не отдаётся")
	assert.Equal(t, domain.CodeValidationError, domain.CodeOf(err))

	_, err = env.Orders.ListByUser(ctx, app.ListUserOrdersQuery{
		UserExternalID: string(make([]byte, 129)),
	})
	require.Error(t, err, "длиннее колонки в БД")
	assert.Equal(t, domain.CodeValidationError, domain.CodeOf(err))

	_, err = env.Orders.ListByUser(ctx, app.ListUserOrdersQuery{
		UserExternalID: "usr_mine",
		Cursor:         "мусор",
	})
	require.Error(t, err)
	assert.Equal(t, domain.CodeBadRequest, domain.CodeOf(err))
}

// --- Очередь заведения ------------------------------------------------------

func TestPartnerService_ListOrders_CursorWalksWholeQueue(t *testing.T) {
	t.Parallel()

	env := newTestEnv()
	const total = 5
	placeOrders(t, env, "usr_mine", total)

	var (
		seen   []string
		cursor string
	)
	for range total + 2 {
		page, err := env.Partners.ListOrders(context.Background(), app.ListOrdersQuery{
			RestaurantID: 1,
			Limit:        2,
			Cursor:       cursor,
		})
		require.NoError(t, err)

		for _, order := range page.Items {
			seen = append(seen, order.PublicNumber.String())
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	assert.Len(t, seen, total, "очередь обойдена целиком и без повторов")
	assert.Len(t, unique(seen), total)
}

func TestPartnerService_ListOrders_RejectsBadCursor(t *testing.T) {
	t.Parallel()

	env := newTestEnv()

	_, err := env.Partners.ListOrders(context.Background(), app.ListOrdersQuery{
		RestaurantID: 1,
		Cursor:       "не курсор",
	})
	require.Error(t, err)
	assert.Equal(t, domain.CodeBadRequest, domain.CodeOf(err))
}

func unique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
