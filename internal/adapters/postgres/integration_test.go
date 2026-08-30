//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"avito-kitchen/internal/app"
	"avito-kitchen/internal/domain"
)

func pizzaMenu() []domain.Product {
	return []domain.Product{
		{ProductKey: "pizza_margherita", Category: "Пицца", Name: "Пицца Маргарита",
			Description: "Томаты, моцарелла", PriceKopecks: 59000, Available: true, StockQty: ptrInt32(10)},
		{ProductKey: "pasta_carbonara", Category: "Паста", Name: "Паста Карбонара",
			PriceKopecks: 49000, Available: true},
		{ProductKey: "dessert_tiramisu", Category: "Десерты", Name: "Тирамису",
			PriceKopecks: 29000, Available: false, StockQty: ptrInt32(5)},
	}
}

func newOrderDraft(restaurantID int64, items ...domain.DraftItem) domain.OrderDraft {
	return domain.OrderDraft{
		UserExternalID:  "usr_integration",
		RestaurantID:    restaurantID,
		DeliveryAddress: "ул. Ленина, 10",
		Items:           items,
	}
}

// ---------------------------------------------------------------------------
// Миграции
// ---------------------------------------------------------------------------

// Схема должна разворачиваться и сворачиваться без ручного вмешательства:
// down-миграция без up-миграции — это невозможность откатить релиз.
func TestMigrations_AreReversible(t *testing.T) {
	m, err := newMigrator()
	require.NoError(t, err)
	defer m.Close()

	require.NoError(t, m.Down(), "откат всех миграций")

	err = m.Up()
	require.True(t, err == nil || errors.Is(err, migrate.ErrNoChange),
		"повторное применение миграций: %v", err)

	// После восстановления схемы демо-данные снова на месте.
	var count int
	require.NoError(t, testPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM restaurants WHERE slug IN ('pizza-avito', 'sushi-avito')`).Scan(&count))
	assert.Equal(t, 2, count, "seed-миграция применилась заново")
}

// ---------------------------------------------------------------------------
// Каталог и меню
// ---------------------------------------------------------------------------

func TestMenuRepo_PublishVersion_ArchivesPrevious(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	restaurant, _ := env.seedRestaurant(t, domain.RestaurantOnline, 50000, 15000)

	first := env.seedMenu(t, restaurant.ID, pizzaMenu())
	assert.Equal(t, int32(1), first.Menu.Version)

	second := env.seedMenu(t, restaurant.ID, []domain.Product{
		{ProductKey: "pizza_new", Category: "Пицца", Name: "Новинка",
			PriceKopecks: 71000, Available: true},
	})
	assert.Equal(t, int32(2), second.Menu.Version)
	assert.Len(t, second.Products, 1, "витрина показывает только новую версию")

	// Опубликованная версия ровно одна — это гарантирует частичный уникальный
	// индекс idx_menus_restaurant_active.
	var published int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM menus WHERE restaurant_id = $1 AND status = 'published'`,
		restaurant.ID).Scan(&published))
	assert.Equal(t, 1, published)

	var archived int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM menus WHERE restaurant_id = $1 AND status = 'archived'`,
		restaurant.ID).Scan(&archived))
	assert.Equal(t, 1, archived, "старая версия архивирована, а не удалена")
}

// Две одновременные синхронизации не должны падать на уникальном индексе
// (restaurant_id, version): блокировка строки заведения выстраивает их в очередь.
func TestMenuRepo_PublishVersion_ConcurrentSyncsSerialize(t *testing.T) {
	env := newTestEnv(t)
	restaurant, _ := env.seedRestaurant(t, domain.RestaurantOnline, 50000, 15000)
	env.seedMenu(t, restaurant.ID, pizzaMenu())

	const syncs = 6
	var wg sync.WaitGroup
	errs := make([]error, syncs)

	for i := 0; i < syncs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = env.partnerService.SyncMenu(context.Background(), restaurant.ID, pizzaMenu())
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		assert.NoErrorf(t, err, "синхронизация #%d", i)
	}

	snapshot, err := env.menus.GetPublished(context.Background(), restaurant.ID)
	require.NoError(t, err)
	assert.Equal(t, int32(1+syncs), snapshot.Menu.Version, "версии не перескакивают и не дублируются")
}

func TestRestaurantRepo_KeysetPagination(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)

	// Собираем всю выдачу постранично и сверяем с выдачей одной страницей.
	var paged []int64
	cursor := ""
	for {
		page, err := env.catalogService.ListRestaurants(ctx, app.ListRestaurantsQuery{
			Limit: 1, Cursor: cursor,
		})
		require.NoError(t, err)
		for _, r := range page.Items {
			paged = append(paged, r.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
		require.Less(t, len(paged), 10_000, "пагинация зациклилась")
	}

	whole, err := env.catalogService.ListRestaurants(ctx, app.ListRestaurantsQuery{Limit: 100})
	require.NoError(t, err)

	var expected []int64
	for _, r := range whole.Items {
		expected = append(expected, r.ID)
	}

	require.GreaterOrEqual(t, len(expected), 2)
	assert.Equal(t, expected, paged[:len(expected)], "постраничный обход даёт тот же порядок")
}

func TestRestaurantRepo_GetByAPIKeyHash(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	restaurant, token := env.seedRestaurant(t, domain.RestaurantOnline, 50000, 15000)

	found, err := env.partnerService.Authenticate(ctx, token)
	require.NoError(t, err)
	assert.Equal(t, restaurant.ID, found.ID)

	_, err = env.partnerService.Authenticate(ctx, token+"-wrong")
	require.Error(t, err)
	assert.Equal(t, domain.CodeUnauthorized, domain.CodeOf(err))
}

// ---------------------------------------------------------------------------
// Оформление заказа
// ---------------------------------------------------------------------------

func TestOrderService_Create_PersistsEverythingAtomically(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	restaurant, _ := env.seedRestaurant(t, domain.RestaurantOnline, 50000, 15000)
	snapshot := env.seedMenu(t, restaurant.ID, pizzaMenu())
	pizzaID := env.productID(t, snapshot, "pizza_margherita")

	order, err := env.orderService.Create(ctx, newOrderDraft(restaurant.ID,
		domain.DraftItem{ProductKey: "pizza_margherita", Qty: 2},
	))
	require.NoError(t, err)

	assert.Equal(t, domain.StatusNew, order.Status)
	assert.Equal(t, int64(118000), order.SubtotalKopecks)
	assert.Equal(t, int64(133000), order.TotalKopecks)
	assert.Equal(t, int32(8), *env.stockOf(t, pizzaID))

	// Заказ читается обратно со всеми связями.
	stored, err := env.orders.GetByPublicNumber(ctx, order.PublicNumber)
	require.NoError(t, err)
	assert.Equal(t, order.PublicNumber, stored.PublicNumber)
	assert.Equal(t, restaurant.Slug, stored.RestaurantSlug)
	require.Len(t, stored.Items, 1)
	assert.Equal(t, "Пицца Маргарита", stored.Items[0].ProductNameSnapshot)
	require.Len(t, stored.Timeline, 1)
	assert.Equal(t, domain.StatusNew, stored.Timeline[0].ToStatus)
	assert.Equal(t, domain.OrderStatus(""), stored.Timeline[0].FromStatus)

	// Событие для заведения записано в той же транзакции.
	var outboxCount int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = $2`,
		order.PublicNumber.String(), app.EventOrderCreated).Scan(&outboxCount))
	assert.Equal(t, 1, outboxCount)
}

// Главный тест на консистентность: N параллельных заказов на товар, которого
// хватает только на часть из них. Успешных должно быть ровно столько, сколько
// позволяет остаток, — ни больше, ни меньше.
func TestOrderService_Create_ConcurrentStockRace(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	restaurant, _ := env.seedRestaurant(t, domain.RestaurantOnline, 0, 0)

	const (
		stock    = 10
		attempts = 25
		qty      = 1
	)

	snapshot := env.seedMenu(t, restaurant.ID, []domain.Product{
		{ProductKey: "scarce", Category: "Дефицит", Name: "Дефицитная позиция",
			PriceKopecks: 10000, Available: true, StockQty: ptrInt32(stock)},
	})
	productID := env.productID(t, snapshot, "scarce")

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		success int
		outOfSt int
		other   []error
	)

	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // стартуем одновременно, чтобы гонка была настоящей

			_, err := env.orderService.Create(ctx, newOrderDraft(restaurant.ID,
				domain.DraftItem{ProductKey: "scarce", Qty: qty},
			))

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				success++
			case domain.HasCode(err, domain.CodeOutOfStock):
				outOfSt++
			default:
				other = append(other, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	assert.Empty(t, other, "неожиданные ошибки при гонке")
	assert.Equal(t, stock/qty, success, "успешных заказов ровно столько, сколько было остатка")
	assert.Equal(t, attempts-stock/qty, outOfSt, "остальные получили OUT_OF_STOCK")

	assert.Equal(t, int32(0), *env.stockOf(t, productID), "остаток ушёл в ноль и не в минус")

	// Сумма списаний по заказам совпадает с исчезнувшим остатком: ни одна
	// единица не потерялась и не задвоилась.
	var ordered int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT COALESCE(SUM(oi.qty), 0) FROM order_items oi
		 JOIN orders o ON o.id = oi.order_id
		 WHERE o.restaurant_id = $1`, restaurant.ID).Scan(&ordered))
	assert.Equal(t, stock, ordered)
}

// Откат обязан вернуть уже списанные остатки предыдущих позиций корзины.
func TestOrderService_Create_RollbackRestoresPartialDecrements(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	restaurant, _ := env.seedRestaurant(t, domain.RestaurantOnline, 0, 0)

	snapshot := env.seedMenu(t, restaurant.ID, []domain.Product{
		{ProductKey: "plenty", Category: "Еда", Name: "Много", PriceKopecks: 10000,
			Available: true, StockQty: ptrInt32(50)},
		{ProductKey: "scarce", Category: "Еда", Name: "Мало", PriceKopecks: 10000,
			Available: true, StockQty: ptrInt32(1)},
	})
	plentyID := env.productID(t, snapshot, "plenty")
	scarceID := env.productID(t, snapshot, "scarce")

	_, err := env.orderService.Create(ctx, newOrderDraft(restaurant.ID,
		domain.DraftItem{ProductKey: "plenty", Qty: 5},
		domain.DraftItem{ProductKey: "scarce", Qty: 3},
	))
	require.Error(t, err)
	assert.Equal(t, domain.CodeOutOfStock, domain.CodeOf(err))

	assert.Equal(t, int32(50), *env.stockOf(t, plentyID), "списание откатилось")
	assert.Equal(t, int32(1), *env.stockOf(t, scarceID))

	var orders int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM orders WHERE restaurant_id = $1`, restaurant.ID).Scan(&orders))
	assert.Zero(t, orders, "заказ не создан")

	var events int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM outbox_events WHERE payload->>'restaurant_id' = $1::text`,
		strconv.FormatInt(restaurant.ID, 10)).Scan(&events))
	assert.Zero(t, events, "событие не отправлено")
}

func TestOrderService_Create_MinOrderRollsBackStock(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	restaurant, _ := env.seedRestaurant(t, domain.RestaurantOnline, 500000, 15000)
	snapshot := env.seedMenu(t, restaurant.ID, pizzaMenu())
	pizzaID := env.productID(t, snapshot, "pizza_margherita")

	_, err := env.orderService.Create(ctx, newOrderDraft(restaurant.ID,
		domain.DraftItem{ProductKey: "pizza_margherita", Qty: 1},
	))
	require.Error(t, err)
	assert.Equal(t, domain.CodeMinOrderNotMet, domain.CodeOf(err))
	assert.Equal(t, int32(10), *env.stockOf(t, pizzaID))
}

func TestOrderService_Create_RestaurantPaused(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	restaurant, _ := env.seedRestaurant(t, domain.RestaurantPaused, 0, 0)
	env.seedMenu(t, restaurant.ID, pizzaMenu())

	_, err := env.orderService.Create(ctx, newOrderDraft(restaurant.ID,
		domain.DraftItem{ProductKey: "pizza_margherita", Qty: 1},
	))
	require.Error(t, err)
	assert.Equal(t, domain.CodeRestaurantUnavailable, domain.CodeOf(err))
}

func TestOrderService_Create_UnavailableProduct(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	restaurant, _ := env.seedRestaurant(t, domain.RestaurantOnline, 0, 0)
	env.seedMenu(t, restaurant.ID, pizzaMenu())

	_, err := env.orderService.Create(ctx, newOrderDraft(restaurant.ID,
		domain.DraftItem{ProductKey: "dessert_tiramisu", Qty: 1},
	))
	require.Error(t, err)
	assert.Equal(t, domain.CodeProductUnavailable, domain.CodeOf(err))
}

// ---------------------------------------------------------------------------
// Переходы статусов
// ---------------------------------------------------------------------------

func TestOrderRepo_ApplyStatus_OptimisticLock(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	restaurant, _ := env.seedRestaurant(t, domain.RestaurantOnline, 0, 0)
	env.seedMenu(t, restaurant.ID, pizzaMenu())

	order, err := env.orderService.Create(ctx, newOrderDraft(restaurant.ID,
		domain.DraftItem{ProductKey: "pizza_margherita", Qty: 1},
	))
	require.NoError(t, err)

	upd := app.StatusUpdate{
		OrderID:         order.ID,
		ExpectedVersion: order.Version,
		FromStatus:      domain.StatusNew,
		ToStatus:        domain.StatusAccepted,
		Actor:           domain.ActorRestaurant,
	}

	require.NoError(t, env.orders.ApplyStatus(ctx, upd), "первое обновление проходит")

	err = env.orders.ApplyStatus(ctx, upd)
	require.Error(t, err, "повтор с устаревшей версией должен упасть")
	assert.Equal(t, domain.CodeStateConflict, domain.CodeOf(err))
}

// Параллельные попытки перевести заказ дальше по конвейеру: пройти должна
// ровно одна, остальные получают STATE_CONFLICT вместо затирания чужой записи.
func TestOrderService_ChangeStatus_ConcurrentTransitions(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	restaurant, _ := env.seedRestaurant(t, domain.RestaurantOnline, 0, 0)
	env.seedMenu(t, restaurant.ID, pizzaMenu())

	order, err := env.orderService.Create(ctx, newOrderDraft(restaurant.ID,
		domain.DraftItem{ProductKey: "pizza_margherita", Qty: 1},
	))
	require.NoError(t, err)

	const racers = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted int
		rejected int
		conflict int
	)

	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		to := domain.StatusAccepted
		if i%2 == 1 {
			to = domain.StatusRejected
		}

		wg.Add(1)
		go func(to domain.OrderStatus) {
			defer wg.Done()
			<-start

			_, err := env.orderService.ChangeStatus(ctx, app.StatusChange{
				PublicNumber: order.PublicNumber,
				To:           to,
				Actor:        domain.ActorRestaurant,
				RestaurantID: restaurant.ID,
				Reason:       "гонка",
			})

			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil && to == domain.StatusAccepted:
				accepted++
			case err == nil && to == domain.StatusRejected:
				rejected++
			case domain.HasCode(err, domain.CodeStateConflict):
				conflict++
			default:
				t.Errorf("неожиданная ошибка: %v", err)
			}
		}(to)
	}
	close(start)
	wg.Wait()

	assert.Equal(t, 1, accepted+rejected, "заказ покинул статус NEW ровно один раз")
	assert.Equal(t, racers-1, conflict)

	final, err := env.orders.GetByPublicNumber(ctx, order.PublicNumber)
	require.NoError(t, err)
	assert.Equal(t, int32(2), final.Version, "версия увеличилась ровно на один переход")
	assert.Len(t, final.Timeline, 2, "в истории ровно одно событие перехода")
}

func TestOrderService_ChangeStatus_CancelRestoresStock(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	restaurant, _ := env.seedRestaurant(t, domain.RestaurantOnline, 0, 0)
	snapshot := env.seedMenu(t, restaurant.ID, pizzaMenu())
	pizzaID := env.productID(t, snapshot, "pizza_margherita")

	order, err := env.orderService.Create(ctx, newOrderDraft(restaurant.ID,
		domain.DraftItem{ProductKey: "pizza_margherita", Qty: 3},
	))
	require.NoError(t, err)
	require.Equal(t, int32(7), *env.stockOf(t, pizzaID))

	cancelled, err := env.orderService.CancelByUser(ctx, order.PublicNumber, "Передумал")
	require.NoError(t, err)
	assert.Equal(t, domain.StatusCancelled, cancelled.Status)
	assert.Equal(t, int32(10), *env.stockOf(t, pizzaID), "остаток вернулся")

	require.NotNil(t, cancelled.CancelReason)
	assert.Equal(t, "Передумал", *cancelled.CancelReason)

	var cancelEvents int
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = $2`,
		order.PublicNumber.String(), app.EventOrderCancelled).Scan(&cancelEvents))
	assert.Equal(t, 1, cancelEvents)
}

// Если заведение успело опубликовать новую версию меню, возврат остатка в
// позицию архивной версии — осознанный no-op, а не ошибка (ADR-0002).
func TestOrderService_ChangeStatus_CancelAfterMenuResyncIsNoop(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	restaurant, _ := env.seedRestaurant(t, domain.RestaurantOnline, 0, 0)
	oldSnapshot := env.seedMenu(t, restaurant.ID, pizzaMenu())
	oldPizzaID := env.productID(t, oldSnapshot, "pizza_margherita")

	order, err := env.orderService.Create(ctx, newOrderDraft(restaurant.ID,
		domain.DraftItem{ProductKey: "pizza_margherita", Qty: 3},
	))
	require.NoError(t, err)
	require.Equal(t, int32(7), *env.stockOf(t, oldPizzaID))

	// Заведение перевыложило меню — старая версия ушла в архив.
	newSnapshot := env.seedMenu(t, restaurant.ID, pizzaMenu())
	newPizzaID := env.productID(t, newSnapshot, "pizza_margherita")
	require.NotEqual(t, oldPizzaID, newPizzaID)

	_, err = env.orderService.CancelByUser(ctx, order.PublicNumber, "Передумал")
	require.NoError(t, err)

	assert.Equal(t, int32(7), *env.stockOf(t, oldPizzaID), "архивная позиция не трогается")
	assert.Equal(t, int32(10), *env.stockOf(t, newPizzaID), "актуальные остатки задаёт заведение")
}

func TestOrderService_ChangeStatus_ForeignRestaurantForbidden(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)

	owner, _ := env.seedRestaurant(t, domain.RestaurantOnline, 0, 0)
	stranger, _ := env.seedRestaurant(t, domain.RestaurantOnline, 0, 0)
	env.seedMenu(t, owner.ID, pizzaMenu())

	order, err := env.orderService.Create(ctx, newOrderDraft(owner.ID,
		domain.DraftItem{ProductKey: "pizza_margherita", Qty: 1},
	))
	require.NoError(t, err)

	_, err = env.orderService.ChangeStatus(ctx, app.StatusChange{
		PublicNumber: order.PublicNumber,
		To:           domain.StatusAccepted,
		Actor:        domain.ActorRestaurant,
		RestaurantID: stranger.ID,
	})
	require.Error(t, err)
	assert.Equal(t, domain.CodeForbidden, domain.CodeOf(err))
}

func TestOrderRepo_ListStaleNew(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	restaurant, _ := env.seedRestaurant(t, domain.RestaurantOnline, 0, 0)
	env.seedMenu(t, restaurant.ID, pizzaMenu())

	order, err := env.orderService.Create(ctx, newOrderDraft(restaurant.ID,
		domain.DraftItem{ProductKey: "pizza_margherita", Qty: 1},
	))
	require.NoError(t, err)

	// Только что созданный заказ «зависшим» не считается.
	fresh, err := env.orders.ListStaleNew(ctx, time.Now().Add(-time.Hour), 100)
	require.NoError(t, err)
	for _, o := range fresh {
		assert.NotEqual(t, order.PublicNumber, o.PublicNumber)
	}

	// Сдвигаем время создания в прошлое.
	_, err = testPool.Exec(ctx,
		`UPDATE orders SET created_at = NOW() - INTERVAL '10 minutes' WHERE id = $1`, order.ID)
	require.NoError(t, err)

	stale, err := env.orders.ListStaleNew(ctx, time.Now().Add(-5*time.Minute), 100)
	require.NoError(t, err)

	var found bool
	for _, o := range stale {
		if o.PublicNumber == order.PublicNumber {
			found = true
		}
	}
	assert.True(t, found, "просроченный заказ попал в выборку")
}

func TestPartnerService_ListOrders_FiltersByRestaurantAndStatus(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)

	mine, _ := env.seedRestaurant(t, domain.RestaurantOnline, 0, 0)
	foreign, _ := env.seedRestaurant(t, domain.RestaurantOnline, 0, 0)
	env.seedMenu(t, mine.ID, pizzaMenu())
	env.seedMenu(t, foreign.ID, pizzaMenu())

	own, err := env.orderService.Create(ctx, newOrderDraft(mine.ID,
		domain.DraftItem{ProductKey: "pizza_margherita", Qty: 1}))
	require.NoError(t, err)
	_, err = env.orderService.Create(ctx, newOrderDraft(foreign.ID,
		domain.DraftItem{ProductKey: "pizza_margherita", Qty: 1}))
	require.NoError(t, err)

	queue, err := env.partnerService.ListOrders(ctx, mine.ID, nil, 50)
	require.NoError(t, err)
	require.Len(t, queue, 1)
	assert.Equal(t, own.PublicNumber, queue[0].PublicNumber)
	assert.NotEmpty(t, queue[0].Items, "в очереди видны позиции заказа")
	assert.NotEmpty(t, queue[0].Timeline)

	ready := domain.StatusReady
	empty, err := env.partnerService.ListOrders(ctx, mine.ID, &ready, 50)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// ---------------------------------------------------------------------------
// Идемпотентность
// ---------------------------------------------------------------------------

func TestIdempotencyRepo_ReserveCompleteReplay(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	key := unique("idem")
	expires := time.Now().Add(time.Hour)

	_, claimed, err := env.idempotency.Reserve(ctx, key, testHash("payload-1"), expires)
	require.NoError(t, err)
	assert.True(t, claimed, "первый запрос захватывает ключ")

	existing, claimed, err := env.idempotency.Reserve(ctx, key, testHash("payload-1"), expires)
	require.NoError(t, err)
	assert.False(t, claimed)
	assert.True(t, existing.InProgress(), "операция ещё выполняется")

	require.NoError(t, env.idempotency.Complete(ctx, key, 201, []byte(`{"ok":true}`)))

	replay, claimed, err := env.idempotency.Reserve(ctx, key, testHash("payload-1"), expires)
	require.NoError(t, err)
	assert.False(t, claimed)
	assert.False(t, replay.InProgress())
	assert.Equal(t, 201, replay.ResponseStatus)
	assert.JSONEq(t, `{"ok":true}`, string(replay.ResponseBody))
	assert.Equal(t, testHash("payload-1"), replay.RequestHash)
}

// Захватить ключ должен ровно один из параллельных запросов.
func TestIdempotencyRepo_Reserve_ConcurrentClaimIsExclusive(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	key := unique("idem-race")
	expires := time.Now().Add(time.Hour)

	const racers = 16
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		claims  int
		replays int
	)

	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			_, claimed, err := env.idempotency.Reserve(ctx, key, testHash("payload"), expires)
			require.NoError(t, err)

			mu.Lock()
			defer mu.Unlock()
			if claimed {
				claims++
			} else {
				replays++
			}
		}()
	}
	close(start)
	wg.Wait()

	assert.Equal(t, 1, claims, "ключ захвачен ровно один раз")
	assert.Equal(t, racers-1, replays)
}

func TestIdempotencyRepo_ReleaseAllowsRetry(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	key := unique("idem-release")
	expires := time.Now().Add(time.Hour)

	_, claimed, err := env.idempotency.Reserve(ctx, key, testHash("payload"), expires)
	require.NoError(t, err)
	require.True(t, claimed)

	require.NoError(t, env.idempotency.Release(ctx, key))

	_, claimed, err = env.idempotency.Reserve(ctx, key, testHash("payload"), expires)
	require.NoError(t, err)
	assert.True(t, claimed, "после освобождения ключ снова доступен")

	// Зафиксированный ответ Release стирать не должен.
	require.NoError(t, env.idempotency.Complete(ctx, key, 201, []byte(`{}`)))
	require.NoError(t, env.idempotency.Release(ctx, key))

	record, claimed, err := env.idempotency.Reserve(ctx, key, testHash("payload"), expires)
	require.NoError(t, err)
	assert.False(t, claimed)
	assert.Equal(t, 201, record.ResponseStatus)
}

func TestIdempotencyRepo_ExpiredKeyIsReclaimable(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	key := unique("idem-expired")

	_, claimed, err := env.idempotency.Reserve(ctx, key, testHash("payload-1"), time.Now().Add(-time.Minute))
	require.NoError(t, err)
	require.True(t, claimed)
	require.NoError(t, env.idempotency.Complete(ctx, key, 201, []byte(`{}`)))

	_, claimed, err = env.idempotency.Reserve(ctx, key, testHash("payload-2"), time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.True(t, claimed, "протухший ключ можно захватить заново")

	deleted, err := env.idempotency.DeleteExpired(ctx, time.Now())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, deleted, int64(0))
}

// ---------------------------------------------------------------------------
// Outbox
// ---------------------------------------------------------------------------

func TestOutboxRepo_ClaimBatch_SkipsLockedAndLeases(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	restaurant, _ := env.seedRestaurant(t, domain.RestaurantOnline, 0, 0)
	env.seedMenu(t, restaurant.ID, pizzaMenu())

	order, err := env.orderService.Create(ctx, newOrderDraft(restaurant.ID,
		domain.DraftItem{ProductKey: "pizza_margherita", Qty: 1},
	))
	require.NoError(t, err)

	claim := func() []app.OutboxEvent {
		events, err := env.outbox.ClaimBatch(ctx, 100, time.Minute)
		require.NoError(t, err)

		var mine []app.OutboxEvent
		for _, e := range events {
			if e.AggregateID == order.PublicNumber.String() {
				mine = append(mine, e)
			}
		}
		return mine
	}

	first := claim()
	require.Len(t, first, 1, "событие захвачено первым воркером")
	assert.Equal(t, app.EventOrderCreated, first[0].EventType)

	var payload app.OrderCreatedPayload
	require.NoError(t, json.Unmarshal(first[0].Payload, &payload))
	assert.Equal(t, restaurant.ID, payload.RestaurantID)
	assert.Equal(t, int64(59000), payload.SubtotalKopecks)

	assert.Empty(t, claim(), "во время аренды событие повторно не выдаётся")

	require.NoError(t, env.outbox.MarkSent(ctx, first[0].ID))
	assert.Empty(t, claim(), "доставленное событие больше не выдаётся")

	var sentAt *time.Time
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT sent_at FROM outbox_events WHERE id = $1`, first[0].ID).Scan(&sentAt))
	assert.NotNil(t, sentAt)
}

func TestOutboxRepo_MarkFailedAndDead(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	restaurant, _ := env.seedRestaurant(t, domain.RestaurantOnline, 0, 0)
	env.seedMenu(t, restaurant.ID, pizzaMenu())

	order, err := env.orderService.Create(ctx, newOrderDraft(restaurant.ID,
		domain.DraftItem{ProductKey: "pizza_margherita", Qty: 1},
	))
	require.NoError(t, err)

	var eventID int64
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT id FROM outbox_events WHERE aggregate_id = $1`,
		order.PublicNumber.String()).Scan(&eventID))

	require.NoError(t, env.outbox.MarkFailed(ctx, eventID, time.Now().Add(time.Hour), "соединение отвергнуто"))

	var (
		attempts  int32
		lastError *string
	)
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT attempts, last_error FROM outbox_events WHERE id = $1`, eventID).Scan(&attempts, &lastError))
	assert.Equal(t, int32(1), attempts)
	require.NotNil(t, lastError)
	assert.Contains(t, *lastError, "соединение отвергнуто")

	// Отложенное событие не выдаётся до наступления next_retry_at.
	events, err := env.outbox.ClaimBatch(ctx, 100, time.Minute)
	require.NoError(t, err)
	for _, e := range events {
		assert.NotEqual(t, eventID, e.ID)
	}

	require.NoError(t, env.outbox.MarkDead(ctx, eventID, "попытки исчерпаны"))

	var deadAt *time.Time
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT dead_at FROM outbox_events WHERE id = $1`, eventID).Scan(&deadAt))
	assert.NotNil(t, deadAt, "событие снято с доставки, но строка сохранена для разбора")
}

// Несколько воркеров, разгребающих очередь одновременно, не должны выдать одно
// событие дважды: это и есть смысл FOR UPDATE SKIP LOCKED.
func TestOutboxRepo_ClaimBatch_ConcurrentWorkersDoNotOverlap(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	restaurant, _ := env.seedRestaurant(t, domain.RestaurantOnline, 0, 0)
	env.seedMenu(t, restaurant.ID, pizzaMenu())

	const orders = 12
	wanted := make(map[string]struct{}, orders)
	for i := 0; i < orders; i++ {
		order, err := env.orderService.Create(ctx, newOrderDraft(restaurant.ID,
			domain.DraftItem{ProductKey: "pasta_carbonara", Qty: 1},
		))
		require.NoError(t, err)
		wanted[order.PublicNumber.String()] = struct{}{}
	}

	const workers = 4
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		claims = make(map[int64]int)
	)

	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			events, err := env.outbox.ClaimBatch(ctx, 100, time.Minute)
			require.NoError(t, err)

			mu.Lock()
			defer mu.Unlock()
			for _, e := range events {
				if _, ours := wanted[e.AggregateID]; ours {
					claims[e.ID]++
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	assert.Len(t, claims, orders, "каждое событие захвачено")
	for id, count := range claims {
		assert.Equalf(t, 1, count, "событие %d выдано более одного раза", id)
	}
}

// ---------------------------------------------------------------------------
// Транзакции
// ---------------------------------------------------------------------------

func TestTxManager_RollbackOnError(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	restaurant, _ := env.seedRestaurant(t, domain.RestaurantOnline, 0, 0)

	sentinel := domain.Errorf(domain.CodeInternalError, "запланированный сбой")

	err := env.tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := env.restaurants.UpdateStatus(ctx, restaurant.ID, domain.RestaurantClosed); err != nil {
			return err
		}
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)

	after, err := env.restaurants.GetByID(ctx, restaurant.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.RestaurantOnline, after.Status, "изменение откатилось")
}

func TestTxManager_NestedTxJoinsOuter(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	restaurant, _ := env.seedRestaurant(t, domain.RestaurantOnline, 0, 0)

	sentinel := domain.Errorf(domain.CodeInternalError, "сбой во внешней транзакции")

	err := env.tx.WithinTx(ctx, func(ctx context.Context) error {
		// Вложенный вызов присоединяется к внешней транзакции, а не коммитит
		// сам по себе — иначе изменение пережило бы откат.
		if err := env.tx.WithinTx(ctx, func(ctx context.Context) error {
			return env.restaurants.UpdateStatus(ctx, restaurant.ID, domain.RestaurantPaused)
		}); err != nil {
			return err
		}
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)

	after, err := env.restaurants.GetByID(ctx, restaurant.ID)
	require.NoError(t, err)
	assert.Equal(t, domain.RestaurantOnline, after.Status)
}

// ---------------------------------------------------------------------------
// Воркеры поверх настоящей БД
// ---------------------------------------------------------------------------

// stubDeliverer подменяет HTTP-клиент заведения: позволяет проверить поведение
// воркера при отказах, не поднимая сервер.
type stubDeliverer struct {
	mu       sync.Mutex
	failures int
	calls    []app.DeliveryEvent
	baseURLs []string
}

func (d *stubDeliverer) Deliver(_ context.Context, baseURL string, event app.DeliveryEvent) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.calls = append(d.calls, event)
	d.baseURLs = append(d.baseURLs, baseURL)

	if d.failures > 0 {
		d.failures--
		return domain.Errorf(domain.CodeServiceUnavailable, "заведение недоступно")
	}
	return nil
}

func (d *stubDeliverer) delivered() []app.DeliveryEvent {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]app.DeliveryEvent(nil), d.calls...)
}

func TestOutboxWorker_DeliversAndMarksSent(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	restaurant, _ := env.seedRestaurant(t, domain.RestaurantOnline, 0, 0)
	env.seedMenu(t, restaurant.ID, pizzaMenu())

	order, err := env.orderService.Create(ctx, newOrderDraft(restaurant.ID,
		domain.DraftItem{ProductKey: "pizza_margherita", Qty: 1},
	))
	require.NoError(t, err)

	deliverer := &stubDeliverer{}
	cfg := app.DefaultOutboxConfig()
	cfg.PollInterval = 10 * time.Millisecond
	worker := app.NewOutboxWorker(env.outbox, env.restaurants, deliverer, cfg, discardLogger())

	runCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		require.NoError(t, worker.Run(runCtx))
	}()

	require.Eventually(t, func() bool {
		var sentAt *time.Time
		err := testPool.QueryRow(ctx,
			`SELECT sent_at FROM outbox_events WHERE aggregate_id = $1`,
			order.PublicNumber.String()).Scan(&sentAt)
		return err == nil && sentAt != nil
	}, 3*time.Second, 20*time.Millisecond, "событие должно быть доставлено и отмечено")

	cancel()
	<-done

	var found bool
	for _, call := range deliverer.delivered() {
		if call.AggregateID == order.PublicNumber.String() {
			found = true
			assert.Equal(t, app.EventOrderCreated, call.EventType)
			assert.NotZero(t, call.EventID, "получателю передаётся идентификатор для дедупликации")
		}
	}
	assert.True(t, found, "воркер доставил наше событие")
	assert.Contains(t, deliverer.baseURLs, restaurant.ProviderBaseURL)
}

func TestOutboxWorker_RetriesWithBackoff(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	restaurant, _ := env.seedRestaurant(t, domain.RestaurantOnline, 0, 0)
	env.seedMenu(t, restaurant.ID, pizzaMenu())

	order, err := env.orderService.Create(ctx, newOrderDraft(restaurant.ID,
		domain.DraftItem{ProductKey: "pizza_margherita", Qty: 1},
	))
	require.NoError(t, err)

	var eventID int64
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT id FROM outbox_events WHERE aggregate_id = $1`,
		order.PublicNumber.String()).Scan(&eventID))

	deliverer := &stubDeliverer{failures: 1}
	cfg := app.DefaultOutboxConfig()
	cfg.PollInterval = 10 * time.Millisecond
	cfg.BaseBackoff = 20 * time.Millisecond
	cfg.MaxBackoff = 50 * time.Millisecond
	worker := app.NewOutboxWorker(env.outbox, env.restaurants, deliverer, cfg, discardLogger())

	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		require.NoError(t, worker.Run(runCtx))
	}()

	require.Eventually(t, func() bool {
		var sentAt *time.Time
		err := testPool.QueryRow(ctx,
			`SELECT sent_at FROM outbox_events WHERE id = $1`, eventID).Scan(&sentAt)
		return err == nil && sentAt != nil
	}, 5*time.Second, 20*time.Millisecond, "после неудачи событие доставляется повторно")

	cancel()
	<-done

	var attempts int32
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT attempts FROM outbox_events WHERE id = $1`, eventID).Scan(&attempts))
	assert.Equal(t, int32(1), attempts, "зафиксирована ровно одна неудачная попытка")
}

func TestReaperWorker_CancelsStaleOrdersAndPurgesKeys(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	restaurant, _ := env.seedRestaurant(t, domain.RestaurantOnline, 0, 0)
	snapshot := env.seedMenu(t, restaurant.ID, pizzaMenu())
	pizzaID := env.productID(t, snapshot, "pizza_margherita")

	order, err := env.orderService.Create(ctx, newOrderDraft(restaurant.ID,
		domain.DraftItem{ProductKey: "pizza_margherita", Qty: 2},
	))
	require.NoError(t, err)
	require.Equal(t, int32(8), *env.stockOf(t, pizzaID))

	// Заказ «провисел» дольше допустимого.
	_, err = testPool.Exec(ctx,
		`UPDATE orders SET created_at = NOW() - INTERVAL '10 minutes' WHERE id = $1`, order.ID)
	require.NoError(t, err)

	// И заодно протухший ключ идемпотентности.
	expiredKey := unique("idem-stale")
	_, claimed, err := env.idempotency.Reserve(ctx, expiredKey, testHash("payload"), time.Now().Add(-time.Hour))
	require.NoError(t, err)
	require.True(t, claimed)

	cfg := app.DefaultReaperConfig()
	cfg.Interval = 20 * time.Millisecond
	cfg.OrderAcceptTimeout = 5 * time.Minute
	worker := app.NewReaperWorker(env.orders, env.orderService, env.idempotency, cfg, discardLogger())

	runCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		require.NoError(t, worker.Run(runCtx))
	}()

	require.Eventually(t, func() bool {
		current, err := env.orders.GetByPublicNumber(ctx, order.PublicNumber)
		return err == nil && current.Status == domain.StatusCancelled
	}, 3*time.Second, 20*time.Millisecond, "зависший заказ должен быть отменён системой")

	require.Eventually(t, func() bool {
		var count int
		err := testPool.QueryRow(ctx,
			`SELECT COUNT(*) FROM idempotency_keys WHERE key = $1`, expiredKey).Scan(&count)
		return err == nil && count == 0
	}, 3*time.Second, 20*time.Millisecond, "протухший ключ должен быть удалён")

	cancel()
	<-done

	assert.Equal(t, int32(10), *env.stockOf(t, pizzaID), "остатки вернулись в каталог")

	cancelled, err := env.orders.GetByPublicNumber(ctx, order.PublicNumber)
	require.NoError(t, err)
	require.Len(t, cancelled.Timeline, 2)
	assert.Equal(t, domain.ActorSystem, cancelled.Timeline[1].Actor,
		"инициатором отмены записана платформа")
}

// ---------------------------------------------------------------------------
// Симметрия операций с остатком
// ---------------------------------------------------------------------------

// Списание обязано отказать, если позиция уехала в архивную версию меню.
//
// Сценарий: заказ уже прочитал опубликованное меню и держит идентификаторы
// позиций, а заведение в этот момент публикует новую версию. Без проверки
// статуса меню списание ушло бы в архивную строку — продажа из меню, которого
// больше нет на витрине, по ценам, которых заведение не заявляет, при этом
// остатки новой версии остались бы нетронутыми.
func TestMenuRepo_DecreaseStock_RefusesArchivedMenu(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	restaurant, _ := env.seedRestaurant(t, domain.RestaurantOnline, 0, 0)

	oldSnapshot := env.seedMenu(t, restaurant.ID, pizzaMenu())
	oldPizzaID := env.productID(t, oldSnapshot, "pizza_margherita")

	// Списание из опубликованной версии проходит.
	require.NoError(t, env.menus.DecreaseStock(ctx, oldPizzaID, 1))
	assert.Equal(t, int32(9), *env.stockOf(t, oldPizzaID))

	// Заведение публикует новую версию — прежняя уходит в архив.
	newSnapshot := env.seedMenu(t, restaurant.ID, pizzaMenu())
	newPizzaID := env.productID(t, newSnapshot, "pizza_margherita")
	require.NotEqual(t, oldPizzaID, newPizzaID)

	// Списание из архивной версии отклоняется с внятной причиной.
	err := env.menus.DecreaseStock(ctx, oldPizzaID, 1)
	require.Error(t, err)
	assert.Equal(t, domain.CodeProductUnavailable, domain.CodeOf(err))
	assert.Contains(t, err.Error(), "обновило меню")

	assert.Equal(t, int32(9), *env.stockOf(t, oldPizzaID), "архивный остаток не тронут")
	assert.Equal(t, int32(10), *env.stockOf(t, newPizzaID), "актуальный остаток не тронут")
}

// Индекс под очередь заказов должен существовать после миграций, а прежний —
// быть удалён: он не обслуживал ни один запрос и только дорожал вставки.
func TestMigrations_OrdersQueueIndex(t *testing.T) {
	ctx := context.Background()

	var hasNew, hasOld bool
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_indexes
		                WHERE tablename='orders' AND indexname='idx_orders_restaurant_recent')`).Scan(&hasNew))
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_indexes
		                WHERE tablename='orders' AND indexname='idx_orders_restaurant_status')`).Scan(&hasOld))

	assert.True(t, hasNew, "индекс очереди заказов создан")
	assert.False(t, hasOld, "прежний индекс (restaurant_id, status) удалён")
}

// Очередь заказов обязана читаться по индексу, а не полным сканированием.
// Это защита от возврата к плану, который на 120 000 заказов занимал 829 мс.
func TestOrderRepo_ListByRestaurant_UsesIndex(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	restaurant, _ := env.seedRestaurant(t, domain.RestaurantOnline, 0, 0)

	// Достаточно строк, чтобы планировщику было что выбирать.
	_, err := testPool.Exec(ctx, `
		INSERT INTO orders (public_number, user_external_id, restaurant_id, status,
		                    delivery_address, subtotal_kopecks, delivery_fee_kopecks,
		                    total_kopecks, created_at)
		SELECT gen_random_uuid(), 'u'||g, $1, 'NEW', 'ул. Ленина, 10',
		       60000, 0, 60000, NOW() - (g || ' seconds')::interval
		FROM generate_series(1, 5000) g`, restaurant.ID)
	require.NoError(t, err)
	_, err = testPool.Exec(ctx, `ANALYZE orders`)
	require.NoError(t, err)

	// EXPLAIN отдаёт план построчно, поэтому собираем его целиком: QueryRow
	// вернул бы только верхний узел («Limit») и проверка стала бы бессмысленной.
	rows, err := testPool.Query(ctx, `
		EXPLAIN (FORMAT TEXT)
		SELECT o.id FROM orders o
		JOIN restaurants r ON r.id = o.restaurant_id
		WHERE o.restaurant_id = $1 AND (NULL::text IS NULL OR o.status = NULL::text)
		ORDER BY o.created_at DESC, o.id DESC LIMIT 50`, restaurant.ID)
	require.NoError(t, err)
	defer rows.Close()

	var lines []string
	for rows.Next() {
		var line string
		require.NoError(t, rows.Scan(&line))
		lines = append(lines, line)
	}
	require.NoError(t, rows.Err())
	plan := strings.Join(lines, "\n")

	assert.Contains(t, plan, "idx_orders_restaurant_recent",
		"выборка очереди должна опираться на индекс, план: %s", plan)
	assert.NotContains(t, plan, "Seq Scan on orders",
		"полное сканирование заказов недопустимо, план: %s", plan)
}

// Окончательный отказ заведения снимается с доставки сразу, без десяти
// попыток с нарастающим backoff: ответ 400 не изменится ни через секунду,
// ни через час, а попытки лишь оттянут разбор инцидента на часы.
func TestOutboxWorker_PermanentRejectionIsNotRetried(t *testing.T) {
	ctx := context.Background()
	env := newTestEnv(t)
	restaurant, _ := env.seedRestaurant(t, domain.RestaurantOnline, 0, 0)
	env.seedMenu(t, restaurant.ID, pizzaMenu())

	order, err := env.orderService.Create(ctx, newOrderDraft(restaurant.ID,
		domain.DraftItem{ProductKey: "pizza_margherita", Qty: 1},
	))
	require.NoError(t, err)

	var eventID int64
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT id FROM outbox_events WHERE aggregate_id = $1`,
		order.PublicNumber.String()).Scan(&eventID))

	deliverer := &rejectingDeliverer{}
	cfg := app.DefaultOutboxConfig()
	cfg.PollInterval = 10 * time.Millisecond
	worker := app.NewOutboxWorker(env.outbox, env.restaurants, deliverer, cfg, discardLogger())

	runCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		require.NoError(t, worker.Run(runCtx))
	}()

	require.Eventually(t, func() bool {
		var deadAt *time.Time
		err := testPool.QueryRow(ctx,
			`SELECT dead_at FROM outbox_events WHERE id = $1`, eventID).Scan(&deadAt)
		return err == nil && deadAt != nil
	}, 3*time.Second, 20*time.Millisecond, "событие должно быть снято с доставки")

	cancel()
	<-done

	var attempts int32
	require.NoError(t, testPool.QueryRow(ctx,
		`SELECT attempts FROM outbox_events WHERE id = $1`, eventID).Scan(&attempts))

	assert.Equal(t, int32(1), attempts,
		"ровно одна попытка: повторять окончательный отказ бессмысленно")
	assert.Equal(t, 1, deliverer.callsFor(order.PublicNumber.String()),
		"воркер не стучался повторно по нашему заказу")
}

// rejectingDeliverer изображает заведение, отвергающее событие по существу.
//
// Счётчик ведётся по агрегатам, а не общий: в базе остаются недоставленные
// события соседних тестов, и воркер честно берёт в работу их тоже.
type rejectingDeliverer struct {
	mu    sync.Mutex
	calls map[string]int
}

func (d *rejectingDeliverer) Deliver(_ context.Context, _ string, event app.DeliveryEvent) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.calls == nil {
		d.calls = map[string]int{}
	}
	d.calls[event.AggregateID]++

	return fmt.Errorf("%w: HTTP 400", app.ErrDeliveryRejected)
}

func (d *rejectingDeliverer) callsFor(aggregateID string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls[aggregateID]
}
