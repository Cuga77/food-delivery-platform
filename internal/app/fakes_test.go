package app_test

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"

	"avito-kitchen/internal/app"
	"avito-kitchen/internal/domain"
)

// Фейковые адаптеры для модульных тестов use cases. Они намеренно простые:
// их задача — проверить оркестрацию сценариев, а не поведение PostgreSQL.
// Настоящие гарантии атомарности проверяются интеграционными тестами в
// internal/adapters/postgres.

// fakeState — общее хранилище, разделяемое всеми фейковыми репозиториями,
// чтобы транзакционный откат мог вернуть его целиком.
type fakeState struct {
	restaurants map[int64]domain.Restaurant
	menus       map[int64]domain.MenuSnapshot
	stock       map[int64]*int32
	orders      map[uuid.UUID]domain.Order
	nextOrderID int64
	outbox      []app.OutboxEvent
}

func (s *fakeState) clone() *fakeState {
	dup := &fakeState{
		restaurants: make(map[int64]domain.Restaurant, len(s.restaurants)),
		menus:       make(map[int64]domain.MenuSnapshot, len(s.menus)),
		stock:       make(map[int64]*int32, len(s.stock)),
		orders:      make(map[uuid.UUID]domain.Order, len(s.orders)),
		nextOrderID: s.nextOrderID,
		outbox:      append([]app.OutboxEvent(nil), s.outbox...),
	}
	for k, v := range s.restaurants {
		dup.restaurants[k] = v
	}
	for k, v := range s.menus {
		dup.menus[k] = v
	}
	for k, v := range s.stock {
		if v == nil {
			dup.stock[k] = nil
			continue
		}
		qty := *v
		dup.stock[k] = &qty
	}
	for k, v := range s.orders {
		dup.orders[k] = v
	}
	return dup
}

// fakeTx имитирует главное свойство настоящей транзакции: при ошибке все
// изменения, сделанные внутри, откатываются.
type fakeTx struct {
	state **fakeState
	mu    sync.Mutex
	depth int
}

func (t *fakeTx) WithinTx(ctx context.Context, fn func(context.Context) error) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Вложенные вызовы разделяют внешнюю транзакцию, как и настоящий менеджер.
	if t.depth > 0 {
		t.depth++
		defer func() { t.depth-- }()
		return fn(ctx)
	}

	snapshot := (*t.state).clone()
	t.depth++
	defer func() { t.depth-- }()

	if err := fn(ctx); err != nil {
		*t.state = snapshot
		return err
	}
	return nil
}

type fakeRestaurantRepo struct{ state **fakeState }

func (r *fakeRestaurantRepo) List(_ context.Context, filter app.RestaurantFilter) ([]domain.Restaurant, error) {
	var ids []int64
	for id := range (*r.state).restaurants {
		ids = append(ids, id)
	}
	// Стабильный порядок по id — как в keyset-пагинации репозитория.
	for i := range ids {
		for j := i + 1; j < len(ids); j++ {
			if ids[j] < ids[i] {
				ids[i], ids[j] = ids[j], ids[i]
			}
		}
	}

	out := make([]domain.Restaurant, 0, len(ids))
	for _, id := range ids {
		if id <= filter.AfterID {
			continue
		}
		rest := (*r.state).restaurants[id]
		if filter.Status != nil && rest.Status != *filter.Status {
			continue
		}
		out = append(out, rest)
		if filter.Limit > 0 && int32(len(out)) >= filter.Limit {
			break
		}
	}
	return out, nil
}

func (r *fakeRestaurantRepo) GetByID(_ context.Context, id int64) (domain.Restaurant, error) {
	rest, ok := (*r.state).restaurants[id]
	if !ok {
		return domain.Restaurant{}, domain.Errorf(domain.CodeRestaurantNotFound,
			"заведение %d не найдено", id)
	}
	return rest, nil
}

func (r *fakeRestaurantRepo) GetBySlug(_ context.Context, slug string) (domain.Restaurant, error) {
	for _, rest := range (*r.state).restaurants {
		if rest.Slug == slug {
			return rest, nil
		}
	}
	return domain.Restaurant{}, domain.Errorf(domain.CodeRestaurantNotFound,
		"заведение «%s» не найдено", slug)
}

func (r *fakeRestaurantRepo) GetByAPIKeyHash(_ context.Context, hash string) (domain.Restaurant, error) {
	for _, rest := range (*r.state).restaurants {
		if app.HashToken("token-"+rest.Slug) == hash {
			return rest, nil
		}
	}
	return domain.Restaurant{}, domain.Errorf(domain.CodeRestaurantNotFound, "токен не найден")
}

func (r *fakeRestaurantRepo) UpdateStatus(_ context.Context, id int64, status domain.RestaurantStatus) error {
	rest, ok := (*r.state).restaurants[id]
	if !ok {
		return domain.Errorf(domain.CodeRestaurantNotFound, "заведение %d не найдено", id)
	}
	rest.Status = status
	(*r.state).restaurants[id] = rest
	return nil
}

type fakeMenuRepo struct{ state **fakeState }

func (m *fakeMenuRepo) GetPublished(_ context.Context, restaurantID int64) (domain.MenuSnapshot, error) {
	snapshot, ok := (*m.state).menus[restaurantID]
	if !ok {
		return domain.MenuSnapshot{}, domain.Errorf(domain.CodeMenuNotFound,
			"у заведения %d нет опубликованного меню", restaurantID)
	}

	// Остатки живут отдельно, чтобы списание было видно всем читателям.
	products := make([]domain.Product, len(snapshot.Products))
	copy(products, snapshot.Products)
	for i := range products {
		products[i].StockQty = (*m.state).stock[products[i].ID]
	}
	snapshot.Products = products
	return snapshot, nil
}

func (m *fakeMenuRepo) PublishVersion(_ context.Context, restaurantID int64, products []domain.Product) (domain.Menu, error) {
	prev := (*m.state).menus[restaurantID]
	version := prev.Menu.Version + 1

	stored := make([]domain.Product, len(products))
	nextID := 1000*restaurantID + int64(version)*100
	for i, p := range products {
		p.ID = nextID + int64(i)
		p.MenuID = int64(version)
		stored[i] = p
		(*m.state).stock[p.ID] = p.StockQty
	}

	menu := domain.Menu{
		ID:           int64(version),
		RestaurantID: restaurantID,
		Version:      version,
		Status:       domain.MenuPublished,
	}
	(*m.state).menus[restaurantID] = domain.MenuSnapshot{Menu: menu, Products: stored}
	return menu, nil
}

func (m *fakeMenuRepo) DecreaseStock(_ context.Context, productID int64, qty int32) error {
	current, ok := (*m.state).stock[productID]
	if !ok || current == nil {
		// Неограниченный остаток списывать не нужно.
		return nil
	}
	if *current < qty {
		return domain.Errorf(domain.CodeOutOfStock,
			"недостаточно остатка позиции %d: запрошено %d, доступно %d", productID, qty, *current)
	}
	left := *current - qty
	(*m.state).stock[productID] = &left
	return nil
}

func (m *fakeMenuRepo) RestoreStock(_ context.Context, productID int64, qty int32) error {
	current, ok := (*m.state).stock[productID]
	if !ok || current == nil {
		return nil
	}
	restored := *current + qty
	(*m.state).stock[productID] = &restored
	return nil
}

type fakeOrderRepo struct{ state **fakeState }

func (o *fakeOrderRepo) Create(_ context.Context, order *domain.Order) error {
	(*o.state).nextOrderID++
	order.ID = (*o.state).nextOrderID
	now := time.Now()
	order.CreatedAt = now
	order.UpdatedAt = now
	order.Timeline = []domain.StatusEvent{{
		FromStatus: "",
		ToStatus:   order.Status,
		Actor:      domain.ActorUser,
		CreatedAt:  now,
	}}
	(*o.state).orders[order.PublicNumber] = *order
	return nil
}

func (o *fakeOrderRepo) GetByPublicNumber(_ context.Context, number uuid.UUID) (domain.Order, error) {
	order, ok := (*o.state).orders[number]
	if !ok {
		return domain.Order{}, domain.Errorf(domain.CodeOrderNotFound, "заказ %s не найден", number)
	}
	return order, nil
}

func (o *fakeOrderRepo) ListByRestaurant(_ context.Context, filter app.OrderFilter) ([]domain.Order, error) {
	out := make([]domain.Order, 0)
	for _, order := range (*o.state).orders {
		if order.RestaurantID != filter.RestaurantID {
			continue
		}
		if filter.Status != nil && order.Status != *filter.Status {
			continue
		}
		out = append(out, order)
		if filter.Limit > 0 && int32(len(out)) >= filter.Limit {
			break
		}
	}
	return out, nil
}

func (o *fakeOrderRepo) ApplyStatus(_ context.Context, upd app.StatusUpdate) error {
	for number, order := range (*o.state).orders {
		if order.ID != upd.OrderID {
			continue
		}
		if order.Version != upd.ExpectedVersion {
			return domain.Errorf(domain.CodeStateConflict,
				"заказ изменился параллельно: ожидалась версия %d, актуальна %d",
				upd.ExpectedVersion, order.Version)
		}

		order.Status = upd.ToStatus
		order.Version++
		order.UpdatedAt = time.Now()
		if upd.CancelReason != nil {
			order.CancelReason = upd.CancelReason
		}
		order.Timeline = append(order.Timeline, domain.StatusEvent{
			FromStatus: upd.FromStatus,
			ToStatus:   upd.ToStatus,
			Actor:      upd.Actor,
			Comment:    upd.Comment,
			CreatedAt:  time.Now(),
		})
		(*o.state).orders[number] = order
		return nil
	}
	return domain.Errorf(domain.CodeOrderNotFound, "заказ %d не найден", upd.OrderID)
}

func (o *fakeOrderRepo) ListStaleNew(_ context.Context, before time.Time, limit int32) ([]domain.Order, error) {
	out := make([]domain.Order, 0)
	for _, order := range (*o.state).orders {
		if order.Status == domain.StatusNew && order.CreatedAt.Before(before) {
			out = append(out, order)
			if limit > 0 && int32(len(out)) >= limit {
				break
			}
		}
	}
	return out, nil
}

type fakeOutboxRepo struct{ state **fakeState }

func (b *fakeOutboxRepo) Enqueue(_ context.Context, event app.OutboxEvent) error {
	event.ID = int64(len((*b.state).outbox) + 1)
	event.CreatedAt = time.Now()
	(*b.state).outbox = append((*b.state).outbox, event)
	return nil
}

func (b *fakeOutboxRepo) ClaimBatch(context.Context, int32, time.Duration) ([]app.OutboxEvent, error) {
	return append([]app.OutboxEvent(nil), (*b.state).outbox...), nil
}

func (b *fakeOutboxRepo) MarkSent(context.Context, int64) error { return nil }

func (b *fakeOutboxRepo) MarkFailed(context.Context, int64, time.Time, string) error { return nil }

func (b *fakeOutboxRepo) MarkDead(context.Context, int64, string) error { return nil }

// testEnv — собранный на фейках набор сервисов для одного теста.
type testEnv struct {
	state    *fakeState
	orders   *app.OrderService
	catalog  *app.CatalogService
	partners *app.PartnerService
}

func ptrInt32(v int32) *int32 { return &v }

// newTestEnv поднимает окружение с одним онлайн-заведением и меню из трёх
// позиций: с конечным остатком, с бесконечным и недоступной.
func newTestEnv() *testEnv {
	state := &fakeState{
		restaurants: map[int64]domain.Restaurant{
			1: {
				ID:                 1,
				Slug:               "pizza-avito",
				Name:               "Пиццерия Авито",
				Status:             domain.RestaurantOnline,
				ProviderBaseURL:    "http://sim:8081",
				MinOrderKopecks:    50000,
				DeliveryFeeKopecks: 15000,
			},
			2: {
				ID:                 2,
				Slug:               "sushi-avito",
				Name:               "Суши Авито",
				Status:             domain.RestaurantPaused,
				MinOrderKopecks:    80000,
				DeliveryFeeKopecks: 20000,
			},
		},
		menus: map[int64]domain.MenuSnapshot{
			1: {
				Menu: domain.Menu{ID: 1, RestaurantID: 1, Version: 1, Status: domain.MenuPublished},
				Products: []domain.Product{
					{ID: 11, MenuID: 1, ProductKey: "pizza_margherita", Category: "Пицца",
						Name: "Пицца Маргарита", PriceKopecks: 59000, Available: true, StockQty: ptrInt32(10)},
					{ID: 12, MenuID: 1, ProductKey: "pasta_carbonara", Category: "Паста",
						Name: "Паста Карбонара", PriceKopecks: 49000, Available: true, StockQty: nil},
					{ID: 13, MenuID: 1, ProductKey: "dessert_tiramisu", Category: "Десерты",
						Name: "Тирамису", PriceKopecks: 29000, Available: false, StockQty: ptrInt32(5)},
					{ID: 14, MenuID: 1, ProductKey: "drink_cola", Category: "Напитки",
						Name: "Кола", PriceKopecks: 12000, Available: true, StockQty: ptrInt32(2)},
				},
			},
			2: {
				Menu: domain.Menu{ID: 2, RestaurantID: 2, Version: 1, Status: domain.MenuPublished},
				Products: []domain.Product{
					{ID: 21, MenuID: 2, ProductKey: "roll_philadelphia", Category: "Роллы",
						Name: "Филадельфия", PriceKopecks: 64000, Available: true, StockQty: ptrInt32(8)},
				},
			},
		},
		stock: map[int64]*int32{
			11: ptrInt32(10),
			12: nil,
			13: ptrInt32(5),
			14: ptrInt32(2),
			21: ptrInt32(8),
		},
		orders: map[uuid.UUID]domain.Order{},
	}

	env := &testEnv{state: state}
	tx := &fakeTx{state: &env.state}
	restaurants := &fakeRestaurantRepo{state: &env.state}
	menus := &fakeMenuRepo{state: &env.state}
	orders := &fakeOrderRepo{state: &env.state}
	outbox := &fakeOutboxRepo{state: &env.state}

	env.orders = app.NewOrderService(tx, restaurants, menus, orders, outbox)
	env.catalog = app.NewCatalogService(restaurants, menus)
	env.partners = app.NewPartnerService(tx, restaurants, menus, orders)

	return env
}

// stockOf возвращает текущий остаток позиции; nil — неограниченный.
func (e *testEnv) stockOf(productID int64) *int32 { return e.state.stock[productID] }

// outboxTypes перечисляет типы накопленных исходящих событий.
func (e *testEnv) outboxTypes() []string {
	out := make([]string, 0, len(e.state.outbox))
	for _, ev := range e.state.outbox {
		out = append(out, ev.EventType)
	}
	return out
}
