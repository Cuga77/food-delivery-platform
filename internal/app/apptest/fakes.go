// Package apptest даёт фейковые реализации портов и собранное на них
// окружение для модульных тестов.
//
// Фейки намеренно простые: их задача — проверять оркестрацию сценариев и
// поведение транспорта, а не воспроизводить PostgreSQL. Настоящие гарантии
// атомарности, блокировок и SKIP LOCKED проверяются интеграционными тестами
// в internal/adapters/postgres.
//
// Пакет используется только из тестов (internal/app и internal/adapters/web),
// но вынесен в обычный пакет, чтобы не дублировать фикстуры между ними.
package apptest

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"avito-kitchen/internal/app"
	"avito-kitchen/internal/domain"
)

// PartnerToken — токен демо-заведения pizza-avito в фикстуре.
const PartnerToken = "demo-partner-token"

// Ptr возвращает указатель на значение — сокращение для необязательных полей.
func Ptr[T any](v T) *T { return &v }

// State — общее хранилище всех фейковых репозиториев. Держится в одном месте,
// чтобы транзакционный откат мог вернуть его целиком.
type State struct {
	Restaurants map[int64]domain.Restaurant
	// Tokens сопоставляет заведению его партнёрский токен.
	Tokens      map[int64]string
	Menus       map[int64]domain.MenuSnapshot
	Stock       map[int64]*int32
	Orders      map[uuid.UUID]domain.Order
	Outbox      []app.OutboxEvent
	Idempotency map[string]app.IdempotencyRecord

	nextOrderID int64
}

func (s *State) clone() *State {
	dup := &State{
		Restaurants: make(map[int64]domain.Restaurant, len(s.Restaurants)),
		Tokens:      make(map[int64]string, len(s.Tokens)),
		Menus:       make(map[int64]domain.MenuSnapshot, len(s.Menus)),
		Stock:       make(map[int64]*int32, len(s.Stock)),
		Orders:      make(map[uuid.UUID]domain.Order, len(s.Orders)),
		Outbox:      append([]app.OutboxEvent(nil), s.Outbox...),
		Idempotency: make(map[string]app.IdempotencyRecord, len(s.Idempotency)),
		nextOrderID: s.nextOrderID,
	}

	for k, v := range s.Restaurants {
		dup.Restaurants[k] = v
	}
	for k, v := range s.Tokens {
		dup.Tokens[k] = v
	}
	for k, v := range s.Menus {
		dup.Menus[k] = v
	}
	for k, v := range s.Stock {
		if v == nil {
			dup.Stock[k] = nil
			continue
		}
		qty := *v
		dup.Stock[k] = &qty
	}
	for k, v := range s.Orders {
		dup.Orders[k] = v
	}
	for k, v := range s.Idempotency {
		dup.Idempotency[k] = v
	}

	return dup
}

// Env — набор сервисов, собранный на фейковых адаптерах.
type Env struct {
	Catalog     *app.CatalogService
	Orders      *app.OrderService
	Partners    *app.PartnerService
	Idempotency app.IdempotencyRepo

	state *State
}

// State возвращает текущее состояние. Именно метод, а не поле: транзакционный
// откат подменяет структуру целиком, и сохранённая ссылка устарела бы.
func (e *Env) State() *State { return e.state }

// StockOf возвращает остаток позиции; nil — неограниченный.
func (e *Env) StockOf(productID int64) *int32 { return e.state.Stock[productID] }

// OutboxTypes перечисляет типы накопленных исходящих событий.
func (e *Env) OutboxTypes() []string {
	out := make([]string, 0, len(e.state.Outbox))
	for _, ev := range e.state.Outbox {
		out = append(out, ev.EventType)
	}
	return out
}

// OrderCount — сколько заказов создано.
func (e *Env) OrderCount() int { return len(e.state.Orders) }

// New поднимает окружение с двумя заведениями:
//
//	pizza-avito — принимает заказы, минимум 500 ₽, доставка 150 ₽;
//	sushi-avito — на паузе.
//
// Меню пиццерии содержит позицию с конечным остатком, с бесконечным,
// недоступную и дешёвую — этого набора хватает, чтобы воспроизвести
// OUT_OF_STOCK, PRODUCT_UNAVAILABLE и MIN_ORDER_NOT_MET.
func New() *Env {
	state := &State{
		Restaurants: map[int64]domain.Restaurant{
			1: {
				ID: 1, Slug: "pizza-avito", Name: "Пиццерия Авито",
				Status: domain.RestaurantOnline, ProviderBaseURL: "http://sim:8081",
				MinOrderKopecks: 50000, DeliveryFeeKopecks: 15000,
			},
			2: {
				ID: 2, Slug: "sushi-avito", Name: "Суши Авито",
				Status: domain.RestaurantPaused, ProviderBaseURL: "http://sim:8081",
				MinOrderKopecks: 80000, DeliveryFeeKopecks: 20000,
			},
		},
		Tokens: map[int64]string{
			1: PartnerToken,
			2: "sushi-partner-token",
		},
		Menus: map[int64]domain.MenuSnapshot{
			1: {
				Menu: domain.Menu{ID: 1, RestaurantID: 1, Version: 1, Status: domain.MenuPublished},
				Products: []domain.Product{
					{ID: 11, MenuID: 1, ProductKey: "pizza_margherita", Category: "Пицца",
						Name: "Пицца Маргарита", Description: "Томаты, моцарелла, базилик",
						PriceKopecks: 59000, Available: true, StockQty: Ptr(int32(10))},
					{ID: 12, MenuID: 1, ProductKey: "pasta_carbonara", Category: "Паста",
						Name: "Паста Карбонара", PriceKopecks: 49000, Available: true},
					{ID: 13, MenuID: 1, ProductKey: "dessert_tiramisu", Category: "Десерты",
						Name: "Тирамису", PriceKopecks: 29000, Available: false,
						StockQty: Ptr(int32(5))},
					{ID: 14, MenuID: 1, ProductKey: "drink_cola", Category: "Напитки",
						Name: "Кола", PriceKopecks: 12000, Available: true,
						StockQty: Ptr(int32(2))},
				},
			},
			2: {
				Menu: domain.Menu{ID: 2, RestaurantID: 2, Version: 1, Status: domain.MenuPublished},
				Products: []domain.Product{
					{ID: 21, MenuID: 2, ProductKey: "roll_philadelphia", Category: "Роллы",
						Name: "Филадельфия", PriceKopecks: 64000, Available: true,
						StockQty: Ptr(int32(8))},
				},
			},
		},
		Stock: map[int64]*int32{
			11: Ptr(int32(10)),
			12: nil,
			13: Ptr(int32(5)),
			14: Ptr(int32(2)),
			21: Ptr(int32(8)),
		},
		Orders:      map[uuid.UUID]domain.Order{},
		Idempotency: map[string]app.IdempotencyRecord{},
	}

	env := &Env{state: state}

	tx := &fakeTx{state: &env.state}
	restaurants := &fakeRestaurantRepo{state: &env.state}
	menus := &fakeMenuRepo{state: &env.state}
	orders := &fakeOrderRepo{state: &env.state}
	outbox := &fakeOutboxRepo{state: &env.state}
	idempotency := &fakeIdempotencyRepo{state: &env.state}

	env.Catalog = app.NewCatalogService(restaurants, menus)
	env.Orders = app.NewOrderService(tx, restaurants, menus, orders, outbox)
	env.Partners = app.NewPartnerService(tx, restaurants, menus, orders)
	env.Idempotency = idempotency

	return env
}

// fakeTx воспроизводит главное свойство настоящей транзакции: при ошибке все
// изменения, сделанные внутри, откатываются.
type fakeTx struct {
	state **State
	mu    sync.Mutex
	depth int
}

func (t *fakeTx) WithinTx(ctx context.Context, fn func(context.Context) error) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Вложенный вызов присоединяется к внешней транзакции, как и настоящий
	// менеджер: свой снимок он не делает.
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

type fakeRestaurantRepo struct{ state **State }

func (r *fakeRestaurantRepo) List(_ context.Context, filter app.RestaurantFilter) ([]domain.Restaurant, error) {
	ids := make([]int64, 0, len((*r.state).Restaurants))
	for id := range (*r.state).Restaurants {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	out := make([]domain.Restaurant, 0, len(ids))
	for _, id := range ids {
		if id <= filter.AfterID {
			continue
		}
		rest := (*r.state).Restaurants[id]
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
	rest, ok := (*r.state).Restaurants[id]
	if !ok {
		return domain.Restaurant{}, domain.Errorf(domain.CodeRestaurantNotFound,
			"заведение %d не найдено", id)
	}
	return rest, nil
}

func (r *fakeRestaurantRepo) GetBySlug(_ context.Context, slug string) (domain.Restaurant, error) {
	for _, rest := range (*r.state).Restaurants {
		if rest.Slug == slug {
			return rest, nil
		}
	}
	return domain.Restaurant{}, domain.Errorf(domain.CodeRestaurantNotFound,
		"заведение «%s» не найдено", slug)
}

func (r *fakeRestaurantRepo) GetByAPIKeyHash(_ context.Context, hash string) (domain.Restaurant, error) {
	for id, token := range (*r.state).Tokens {
		if app.HashToken(token) == hash {
			return (*r.state).Restaurants[id], nil
		}
	}
	return domain.Restaurant{}, domain.Errorf(domain.CodeRestaurantNotFound, "токен не найден")
}

func (r *fakeRestaurantRepo) UpdateStatus(_ context.Context, id int64, status domain.RestaurantStatus) error {
	rest, ok := (*r.state).Restaurants[id]
	if !ok {
		return domain.Errorf(domain.CodeRestaurantNotFound, "заведение %d не найдено", id)
	}
	rest.Status = status
	(*r.state).Restaurants[id] = rest
	return nil
}

type fakeMenuRepo struct{ state **State }

func (m *fakeMenuRepo) GetPublished(_ context.Context, restaurantID int64) (domain.MenuSnapshot, error) {
	snapshot, ok := (*m.state).Menus[restaurantID]
	if !ok {
		return domain.MenuSnapshot{}, domain.Errorf(domain.CodeMenuNotFound,
			"у заведения %d нет опубликованного меню", restaurantID)
	}

	// Остатки живут отдельно, чтобы списание было видно всем читателям.
	products := make([]domain.Product, len(snapshot.Products))
	copy(products, snapshot.Products)
	for i := range products {
		products[i].StockQty = (*m.state).Stock[products[i].ID]
	}
	snapshot.Products = products

	return snapshot, nil
}

func (m *fakeMenuRepo) PublishVersion(_ context.Context, restaurantID int64, products []domain.Product) (domain.Menu, error) {
	prev := (*m.state).Menus[restaurantID]
	version := prev.Menu.Version + 1

	stored := make([]domain.Product, len(products))
	nextID := 1000*restaurantID + int64(version)*100
	for i, p := range products {
		p.ID = nextID + int64(i)
		p.MenuID = int64(version)
		stored[i] = p
		(*m.state).Stock[p.ID] = p.StockQty
	}

	menu := domain.Menu{
		ID: int64(version), RestaurantID: restaurantID,
		Version: version, Status: domain.MenuPublished,
	}
	(*m.state).Menus[restaurantID] = domain.MenuSnapshot{Menu: menu, Products: stored}

	return menu, nil
}

func (m *fakeMenuRepo) DecreaseStock(_ context.Context, productID int64, qty int32) error {
	current, ok := (*m.state).Stock[productID]
	if !ok || current == nil {
		return nil // неограниченный остаток списывать не нужно
	}
	if *current < qty {
		return domain.Errorf(domain.CodeOutOfStock,
			"позиции осталось %d, а запрошено %d", *current, qty)
	}
	left := *current - qty
	(*m.state).Stock[productID] = &left
	return nil
}

func (m *fakeMenuRepo) RestoreStock(_ context.Context, productID int64, qty int32) error {
	current, ok := (*m.state).Stock[productID]
	if !ok || current == nil {
		return nil
	}
	restored := *current + qty
	(*m.state).Stock[productID] = &restored
	return nil
}

type fakeOrderRepo struct{ state **State }

func (o *fakeOrderRepo) Create(_ context.Context, order *domain.Order) error {
	(*o.state).nextOrderID++
	order.ID = (*o.state).nextOrderID

	now := time.Now()
	order.CreatedAt = now
	order.UpdatedAt = now
	order.Timeline = []domain.StatusEvent{{
		ToStatus: order.Status, Actor: domain.ActorUser, CreatedAt: now,
	}}
	(*o.state).Orders[order.PublicNumber] = *order

	return nil
}

func (o *fakeOrderRepo) GetByPublicNumber(_ context.Context, number uuid.UUID) (domain.Order, error) {
	order, ok := (*o.state).Orders[number]
	if !ok {
		return domain.Order{}, domain.Errorf(domain.CodeOrderNotFound, "заказ %s не найден", number)
	}
	return order, nil
}

func (o *fakeOrderRepo) ListByRestaurant(_ context.Context, filter app.OrderFilter) ([]domain.Order, error) {
	out := make([]domain.Order, 0)
	for _, order := range (*o.state).Orders {
		if order.RestaurantID != filter.RestaurantID {
			continue
		}
		if filter.Status != nil && order.Status != *filter.Status {
			continue
		}
		out = append(out, order)
	}

	// Стабильный порядок: свежие сверху, как в настоящем репозитории.
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })

	if filter.Limit > 0 && int32(len(out)) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

func (o *fakeOrderRepo) ApplyStatus(_ context.Context, upd app.StatusUpdate) error {
	for number, order := range (*o.state).Orders {
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
			FromStatus: upd.FromStatus, ToStatus: upd.ToStatus,
			Actor: upd.Actor, Comment: upd.Comment, CreatedAt: time.Now(),
		})
		(*o.state).Orders[number] = order

		return nil
	}

	return domain.Errorf(domain.CodeOrderNotFound, "заказ %d не найден", upd.OrderID)
}

func (o *fakeOrderRepo) ListStaleNew(_ context.Context, before time.Time, limit int32) ([]domain.Order, error) {
	out := make([]domain.Order, 0)
	for _, order := range (*o.state).Orders {
		if order.Status == domain.StatusNew && order.CreatedAt.Before(before) {
			out = append(out, order)
			if limit > 0 && int32(len(out)) >= limit {
				break
			}
		}
	}
	return out, nil
}

type fakeOutboxRepo struct{ state **State }

func (b *fakeOutboxRepo) Enqueue(_ context.Context, event app.OutboxEvent) error {
	event.ID = int64(len((*b.state).Outbox) + 1)
	event.CreatedAt = time.Now()
	(*b.state).Outbox = append((*b.state).Outbox, event)
	return nil
}

func (b *fakeOutboxRepo) ClaimBatch(context.Context, int32, time.Duration) ([]app.OutboxEvent, error) {
	return append([]app.OutboxEvent(nil), (*b.state).Outbox...), nil
}

func (b *fakeOutboxRepo) MarkSent(context.Context, int64) error { return nil }

func (b *fakeOutboxRepo) MarkFailed(context.Context, int64, time.Time, string) error { return nil }

func (b *fakeOutboxRepo) MarkDead(context.Context, int64, string) error { return nil }

// fakeIdempotencyRepo воспроизводит контракт захвата ключа: захват достаётся
// ровно одному, повтор видит сохранённый результат.
type fakeIdempotencyRepo struct {
	state **State
	mu    sync.Mutex
}

func (i *fakeIdempotencyRepo) Reserve(
	_ context.Context,
	key, requestHash string,
	_ time.Time,
) (app.IdempotencyRecord, bool, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	if existing, ok := (*i.state).Idempotency[key]; ok {
		return existing, false, nil
	}

	(*i.state).Idempotency[key] = app.IdempotencyRecord{
		Key: key, RequestHash: requestHash, ResponseStatus: 0,
	}
	return app.IdempotencyRecord{Key: key, RequestHash: requestHash}, true, nil
}

func (i *fakeIdempotencyRepo) Complete(_ context.Context, key string, status int, body []byte) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	record := (*i.state).Idempotency[key]
	record.Key = key
	record.ResponseStatus = status
	record.ResponseBody = json.RawMessage(append([]byte(nil), body...))
	(*i.state).Idempotency[key] = record

	return nil
}

func (i *fakeIdempotencyRepo) Release(_ context.Context, key string) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	if record, ok := (*i.state).Idempotency[key]; ok && record.ResponseStatus == 0 {
		delete((*i.state).Idempotency, key)
	}
	return nil
}

func (i *fakeIdempotencyRepo) DeleteExpired(context.Context, time.Time) (int64, error) {
	return 0, nil
}
