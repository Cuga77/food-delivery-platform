// Package app содержит use cases — оркестрацию бизнес-сценариев поверх
// доменной модели. Здесь же объявлены порты (интерфейсы), которые реализуют
// адаптеры: пакет app ничего не знает ни о PostgreSQL, ни о HTTP.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"avito-kitchen/internal/domain"
)

// TxManager выполняет функцию в транзакции. Транзакция прокидывается через
// context — благодаря этому репозитории не различают транзакционный и обычный
// вызов, а use case не тащит *pgx.Tx через свою сигнатуру.
type TxManager interface {
	WithinTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// RestaurantFilter — параметры выборки каталога (keyset-пагинация).
type RestaurantFilter struct {
	// AfterID — идентификатор последней записи предыдущей страницы.
	AfterID int64
	Limit   int32
	// Status == nil означает «без фильтра по режиму работы».
	Status *domain.RestaurantStatus
}

// RestaurantRepo — доступ к заведениям.
type RestaurantRepo interface {
	List(ctx context.Context, filter RestaurantFilter) ([]domain.Restaurant, error)
	GetByID(ctx context.Context, id int64) (domain.Restaurant, error)
	GetBySlug(ctx context.Context, slug string) (domain.Restaurant, error)
	// GetByAPIKeyHash ищет заведение по SHA-256 партнёрского токена.
	GetByAPIKeyHash(ctx context.Context, apiKeyHash string) (domain.Restaurant, error)
	UpdateStatus(ctx context.Context, id int64, status domain.RestaurantStatus) error
}

// MenuRepo — доступ к версиям меню, позициям и остаткам.
type MenuRepo interface {
	// GetPublished возвращает актуальную опубликованную версию меню.
	GetPublished(ctx context.Context, restaurantID int64) (domain.MenuSnapshot, error)
	// PublishVersion архивирует текущую версию и публикует новую (N+1) с
	// переданным набором позиций. Выполняется целиком в одной транзакции.
	PublishVersion(ctx context.Context, restaurantID int64, products []domain.Product) (domain.Menu, error)
	// DecreaseStock атомарно списывает остаток. Возвращает доменную ошибку
	// OUT_OF_STOCK, если позиция недоступна или остатка не хватает.
	DecreaseStock(ctx context.Context, productID int64, qty int32) error
	// RestoreStock возвращает остаток при отмене или отказе. Если позиция уже
	// вытеснена новой версией меню, операция — no-op (см. ADR-0002).
	RestoreStock(ctx context.Context, productID int64, qty int32) error
}

// OrderFilter — параметры выборки очереди заказов заведения.
type OrderFilter struct {
	RestaurantID int64
	// Status == nil означает «все статусы».
	Status *domain.OrderStatus
	Limit  int32
}

// StatusUpdate — параметры перевода заказа в новый статус под оптимистичной
// блокировкой.
type StatusUpdate struct {
	OrderID int64
	// ExpectedVersion — значение orders.version, прочитанное перед переходом.
	ExpectedVersion int32
	FromStatus      domain.OrderStatus
	ToStatus        domain.OrderStatus
	Actor           domain.Actor
	Comment         string
	CancelReason    *string
}

// OrderRepo — доступ к заказам, их позициям и истории статусов.
type OrderRepo interface {
	// Create вставляет заказ, его позиции и стартовое событие истории.
	// Заполняет ID и CreatedAt/UpdatedAt переданного заказа.
	Create(ctx context.Context, order *domain.Order) error
	// GetByPublicNumber отдаёт заказ вместе с позициями и таймлайном.
	GetByPublicNumber(ctx context.Context, publicNumber uuid.UUID) (domain.Order, error)
	// ListByRestaurant отдаёт очередь заказов заведения (без таймлайна).
	ListByRestaurant(ctx context.Context, filter OrderFilter) ([]domain.Order, error)
	// ApplyStatus обновляет статус под оптимистичной блокировкой и пишет
	// событие в историю. При расхождении версий возвращает STATE_CONFLICT.
	ApplyStatus(ctx context.Context, upd StatusUpdate) error
	// ListStaleNew находит заказы в статусе NEW, созданные раньше указанного
	// момента: кандидаты на автоматическую отмену.
	ListStaleNew(ctx context.Context, createdBefore time.Time, limit int32) ([]domain.Order, error)
}

// IdempotencyRecord — сохранённый результат идемпотентной операции.
type IdempotencyRecord struct {
	Key            string
	RequestHash    string
	ResponseStatus int
	ResponseBody   []byte
}

// InProgress сообщает, что ключ захвачен, но ответ ещё не зафиксирован.
func (r IdempotencyRecord) InProgress() bool { return r.ResponseStatus == 0 }

// IdempotencyRepo хранит результаты операций, защищённых Idempotency-Key.
type IdempotencyRepo interface {
	// Reserve пытается захватить ключ. claimed == true означает, что запрос
	// выполняется впервые и его нужно обработать; при claimed == false в
	// existing лежит уже известная запись (завершённая или ещё выполняющаяся).
	Reserve(ctx context.Context, key, requestHash string, expiresAt time.Time) (existing IdempotencyRecord, claimed bool, err error)
	// Complete фиксирует ответ за ранее захваченным ключом.
	Complete(ctx context.Context, key string, status int, body []byte) error
	// Release снимает захват, если операция завершилась ошибкой: клиент должен
	// иметь возможность повторить запрос с тем же ключом.
	Release(ctx context.Context, key string) error
	// DeleteExpired убирает протухшие ключи; вызывается фоновым reaper-ом.
	DeleteExpired(ctx context.Context, now time.Time) (int64, error)
}

// Типы агрегатов и событий, попадающих в outbox.
const (
	AggregateOrder = "order"

	EventOrderCreated   = "order_created"
	EventOrderCancelled = "order_cancelled"
)

// OutboxEvent — исходящее событие, записанное в одной транзакции с изменением
// агрегата.
type OutboxEvent struct {
	ID            int64
	AggregateType string
	AggregateID   string
	EventType     string
	Payload       json.RawMessage
	Attempts      int32
	NextRetryAt   time.Time
	CreatedAt     time.Time
}

// OutboxRepo — очередь исходящих событий на базе таблицы PostgreSQL.
type OutboxRepo interface {
	// Enqueue добавляет событие. Вызывается внутри бизнес-транзакции.
	Enqueue(ctx context.Context, event OutboxEvent) error
	// ClaimBatch забирает пачку готовых к отправке событий под
	// FOR UPDATE SKIP LOCKED и сразу выдаёт на них аренду: next_retry_at
	// сдвигается на lease вперёд, поэтому другой воркер не подхватит то же
	// событие, пока текущий его доставляет. Благодаря аренде транзакция не
	// держится открытой на время HTTP-запроса (см. ADR-0003).
	ClaimBatch(ctx context.Context, limit int32, lease time.Duration) ([]OutboxEvent, error)
	MarkSent(ctx context.Context, id int64) error
	// MarkFailed переносит попытку на next и сохраняет причину.
	MarkFailed(ctx context.Context, id int64, next time.Time, reason string) error
	// MarkDead окончательно снимает событие с доставки после исчерпания попыток.
	MarkDead(ctx context.Context, id int64, reason string) error
}

// DeliveryEvent — то, что уходит заведению по HTTP.
type DeliveryEvent struct {
	EventID     int64
	EventType   string
	AggregateID string
	Payload     json.RawMessage
	CreatedAt   time.Time
}

// ErrDeliveryRejected означает, что заведение отвергло событие по существу, а
// не из-за временного сбоя: неверный формат, неизвестный тип, отозванный
// доступ. Повторять такое бессмысленно — ответ не изменится ни через секунду,
// ни через час, а десять попыток с нарастающим backoff лишь оттянут разбор
// инцидента на часы.
//
// Реализация EventDeliverer оборачивает этой ошибкой те ответы, которые
// считает окончательными; воркер снимает такое событие с доставки сразу.
var ErrDeliveryRejected = errors.New("заведение отвергло событие")

// EventDeliverer доставляет событие в сервис заведения. Реализация —
// internal/adapters/simclient.
type EventDeliverer interface {
	Deliver(ctx context.Context, baseURL string, event DeliveryEvent) error
}
