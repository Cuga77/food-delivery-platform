package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"avito-kitchen/internal/app"
	"avito-kitchen/internal/domain"
)

// orderColumns — единый список полей заказа для всех выборок.
const orderColumns = `
	o.id, o.public_number, o.user_external_id, o.restaurant_id, r.slug, o.status,
	o.delivery_address, o.subtotal_kopecks, o.delivery_fee_kopecks, o.total_kopecks,
	o.cancel_reason, o.version, o.created_at, o.updated_at`

// OrderRepo — доступ к заказам, позициям и истории статусов.
// Реализует app.OrderRepo.
type OrderRepo struct {
	base
}

// NewOrderRepo создаёт репозиторий заказов.
func NewOrderRepo(pool *pgxpool.Pool) *OrderRepo {
	return &OrderRepo{base{pool: pool}}
}

var _ app.OrderRepo = (*OrderRepo)(nil)

// Create вставляет заказ, снапшот позиций и стартовое событие истории.
//
// Вызывается внутри транзакции use case'а: если дальше по сценарию что-то
// упадёт, заказа не останется вовсе.
func (r *OrderRepo) Create(ctx context.Context, order *domain.Order) error {
	const insertOrder = `
		INSERT INTO orders (public_number, user_external_id, restaurant_id, status,
		                    delivery_address, subtotal_kopecks, delivery_fee_kopecks,
		                    total_kopecks, version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 1)
		RETURNING id, created_at, updated_at`

	db := r.db(ctx)

	err := db.QueryRow(ctx, insertOrder,
		order.PublicNumber, order.UserExternalID, order.RestaurantID, string(order.Status),
		order.DeliveryAddress, order.SubtotalKopecks, order.DeliveryFeeKopecks,
		order.TotalKopecks,
	).Scan(&order.ID, &order.CreatedAt, &order.UpdatedAt)
	if err != nil {
		return wrapDBError(err, "создание заказа")
	}
	order.Version = 1

	if err := r.insertItems(ctx, order); err != nil {
		return err
	}

	// Первое событие истории: from_status пуст, инициатор — пользователь.
	event := domain.StatusEvent{
		FromStatus: "",
		ToStatus:   order.Status,
		Actor:      domain.ActorUser,
		Comment:    "заказ оформлен",
	}
	if err := r.appendStatusEvent(ctx, order.ID, &event); err != nil {
		return err
	}
	order.Timeline = []domain.StatusEvent{event}

	return nil
}

// insertItems вставляет позиции заказа одним запросом через unnest:
// количество round-trip'ов не зависит от размера корзины.
func (r *OrderRepo) insertItems(ctx context.Context, order *domain.Order) error {
	if len(order.Items) == 0 {
		return nil
	}

	const query = `
		INSERT INTO order_items (order_id, product_id, product_key,
		                         product_name_snapshot, unit_price_kopecks,
		                         qty, line_total_kopecks)
		SELECT $1, t.product_id, t.product_key, t.name, t.unit_price, t.qty, t.line_total
		FROM unnest($2::bigint[], $3::text[], $4::text[],
		            $5::bigint[], $6::int[], $7::bigint[])
		     AS t(product_id, product_key, name, unit_price, qty, line_total)`

	n := len(order.Items)
	var (
		productIDs = make([]*int64, n)
		keys       = make([]string, n)
		names      = make([]string, n)
		prices     = make([]int64, n)
		quantities = make([]int32, n)
		lineTotals = make([]int64, n)
	)
	for i, item := range order.Items {
		productIDs[i] = item.ProductID
		keys[i] = item.ProductKey
		names[i] = item.ProductNameSnapshot
		prices[i] = item.UnitPriceKopecks
		quantities[i] = item.Qty
		lineTotals[i] = item.LineTotalKopecks
	}

	if _, err := r.db(ctx).Exec(ctx, query, order.ID,
		productIDs, keys, names, prices, quantities, lineTotals,
	); err != nil {
		return wrapDBError(err, "вставка позиций заказа")
	}

	return nil
}

// GetByPublicNumber отдаёт заказ вместе с позициями и полной историей статусов.
func (r *OrderRepo) GetByPublicNumber(ctx context.Context, publicNumber uuid.UUID) (domain.Order, error) {
	const query = `
		SELECT ` + orderColumns + `
		FROM orders o
		JOIN restaurants r ON r.id = o.restaurant_id
		WHERE o.public_number = $1`

	order, err := scanOrder(r.db(ctx).QueryRow(ctx, query, publicNumber))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Order{}, domain.Errorf(domain.CodeOrderNotFound,
			"заказ %s не найден", publicNumber)
	}
	if err != nil {
		return domain.Order{}, wrapDBError(err, "чтение заказа")
	}

	items, err := r.itemsByOrders(ctx, []int64{order.ID})
	if err != nil {
		return domain.Order{}, err
	}
	order.Items = items[order.ID]

	timeline, err := r.timelineByOrders(ctx, []int64{order.ID})
	if err != nil {
		return domain.Order{}, err
	}
	order.Timeline = timeline[order.ID]

	return order, nil
}

// keysetPredicate — граница страницы при keyset-пагинации по (created_at, id).
//
// Предикат подставляется в запрос только когда курсор задан, а не остаётся в
// нём всегда под `($n::timestamptz IS NULL OR ...)`, как это сделано для
// фильтра по статусу. Причина измерена на 20 000 заказов одного пользователя
// (PostgreSQL 17.5):
//
//   - пока планировщик строит индивидуальный план, значения параметров ему
//     известны, `$n IS NULL` сворачивается в false, и обе формы дают
//     одинаковый Index Cond — разницы нет;
//   - но pgx по умолчанию кэширует подготовленные выражения, а PostgreSQL
//     после нескольких выполнений вправе перейти на обобщённый план, где
//     значения параметров неизвестны и свернуть условие уже нельзя. Тогда
//     форма с OR опускается в Filter: 4 буфера и 0,18 мс превращаются в 711
//     буферов и 8,0 мс, из которых 18 999 строк отбрасываются фильтром.
//
// То есть обёртка не ломает план гарантированно — она делает его зависимым от
// решения, которое принимается за пределами кода. Стоимость страницы при этом
// снова растёт с её глубиной, то есть keyset вырождается в OFFSET. Отдельная
// ветка запроса убирает эту зависимость: условие остаётся границей индексного
// поиска при любом режиме планирования.
const keysetPredicate = ` AND (o.created_at, o.id) < ($%d::timestamptz, $%d::bigint)`

// ListByRestaurant отдаёт очередь заказов заведения, свежие сверху.
//
// Позиции и таймлайн догружаются двумя запросами на всю страницу, а не по
// запросу на заказ: количество round-trip'ов остаётся постоянным (проблема N+1).
func (r *OrderRepo) ListByRestaurant(ctx context.Context, filter app.OrderFilter) ([]domain.Order, error) {
	const head = `
		SELECT ` + orderColumns + `
		FROM orders o
		JOIN restaurants r ON r.id = o.restaurant_id
		WHERE o.restaurant_id = $1
		  AND ($2::text IS NULL OR o.status = $2::text)`

	var status *string
	if filter.Status != nil {
		s := string(*filter.Status)
		status = &s
	}

	query := head
	args := []any{filter.RestaurantID, status}
	if !filter.After.IsZero() {
		query += fmt.Sprintf(keysetPredicate, len(args)+1, len(args)+2)
		args = append(args, filter.After.CreatedAt, filter.After.ID)
	}
	query += fmt.Sprintf(`
		ORDER BY o.created_at DESC, o.id DESC
		LIMIT $%d`, len(args)+1)
	args = append(args, filter.Limit)

	return r.listOrders(ctx, query, args, filter.Limit, "очереди заказов")
}

// ListByUser отдаёт историю заказов пользователя, свежие сверху.
//
// Обслуживается индексом idx_orders_user_recent: порядок индекса совпадает с
// ORDER BY, поэтому сортировки в плане нет, а курсор задаёт точку входа.
func (r *OrderRepo) ListByUser(ctx context.Context, filter app.UserOrderFilter) ([]domain.Order, error) {
	const head = `
		SELECT ` + orderColumns + `
		FROM orders o
		JOIN restaurants r ON r.id = o.restaurant_id
		WHERE o.user_external_id = $1`

	query := head
	args := []any{filter.UserExternalID}
	if !filter.After.IsZero() {
		query += fmt.Sprintf(keysetPredicate, len(args)+1, len(args)+2)
		args = append(args, filter.After.CreatedAt, filter.After.ID)
	}
	query += fmt.Sprintf(`
		ORDER BY o.created_at DESC, o.id DESC
		LIMIT $%d`, len(args)+1)
	args = append(args, filter.Limit)

	return r.listOrders(ctx, query, args, filter.Limit, "истории заказов пользователя")
}

// listOrders выполняет постраничную выборку заказов и догружает позиции с
// таймлайном на всю страницу разом. subject попадает в текст ошибок, чтобы по
// логу было видно, какая именно выборка не удалась.
func (r *OrderRepo) listOrders(
	ctx context.Context,
	query string,
	args []any,
	limit int32,
	subject string,
) ([]domain.Order, error) {
	rows, err := r.db(ctx).Query(ctx, query, args...)
	if err != nil {
		return nil, wrapDBError(err, "выборка "+subject)
	}
	defer rows.Close()

	orders := make([]domain.Order, 0, limit)
	ids := make([]int64, 0, limit)
	for rows.Next() {
		order, err := scanOrder(rows)
		if err != nil {
			return nil, wrapDBError(err, "чтение строки "+subject)
		}
		orders = append(orders, order)
		ids = append(ids, order.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDBError(err, "обход "+subject)
	}

	if len(orders) == 0 {
		return orders, nil
	}

	items, err := r.itemsByOrders(ctx, ids)
	if err != nil {
		return nil, err
	}
	timeline, err := r.timelineByOrders(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range orders {
		orders[i].Items = items[orders[i].ID]
		orders[i].Timeline = timeline[orders[i].ID]
	}

	return orders, nil
}

// ApplyStatus меняет статус под оптимистичной блокировкой и пишет событие истории.
//
// Условие `version = $expected` — суть оптимистичной блокировки: если между
// чтением заказа и записью его успел изменить кто-то другой, UPDATE не заденет
// ни одной строки, и мы честно сообщаем о конфликте вместо того, чтобы
// затереть чужое изменение.
func (r *OrderRepo) ApplyStatus(ctx context.Context, upd app.StatusUpdate) error {
	const query = `
		UPDATE orders
		SET status        = $1,
		    version       = version + 1,
		    updated_at    = NOW(),
		    cancel_reason = COALESCE($2, cancel_reason)
		WHERE id = $3 AND version = $4`

	tag, err := r.db(ctx).Exec(ctx, query,
		string(upd.ToStatus), upd.CancelReason, upd.OrderID, upd.ExpectedVersion)
	if err != nil {
		return wrapDBError(err, "смена статуса заказа")
	}
	if tag.RowsAffected() == 0 {
		return domain.Errorf(domain.CodeStateConflict,
			"заказ был изменён параллельно (ожидалась версия %d) — повторите запрос",
			upd.ExpectedVersion)
	}

	event := domain.StatusEvent{
		FromStatus: upd.FromStatus,
		ToStatus:   upd.ToStatus,
		Actor:      upd.Actor,
		Comment:    upd.Comment,
	}
	return r.appendStatusEvent(ctx, upd.OrderID, &event)
}

func (r *OrderRepo) appendStatusEvent(ctx context.Context, orderID int64, event *domain.StatusEvent) error {
	const query = `
		INSERT INTO order_status_events (order_id, from_status, to_status, actor, comment)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, created_at`

	err := r.db(ctx).QueryRow(ctx, query,
		orderID, string(event.FromStatus), string(event.ToStatus),
		string(event.Actor), event.Comment,
	).Scan(&event.ID, &event.CreatedAt)
	if err != nil {
		return wrapDBError(err, "запись события истории заказа")
	}

	return nil
}

// ListStaleNew находит заказы, на которые заведение не ответило вовремя.
//
// Позиции не догружаются: reaper всё равно перечитает заказ целиком перед
// отменой, чтобы вернуть остатки по актуальным данным.
func (r *OrderRepo) ListStaleNew(ctx context.Context, createdBefore time.Time, limit int32) ([]domain.Order, error) {
	const query = `
		SELECT ` + orderColumns + `
		FROM orders o
		JOIN restaurants r ON r.id = o.restaurant_id
		WHERE o.status = 'NEW' AND o.created_at < $1
		ORDER BY o.created_at
		LIMIT $2`

	rows, err := r.db(ctx).Query(ctx, query, createdBefore, limit)
	if err != nil {
		return nil, wrapDBError(err, "выборка зависших заказов")
	}
	defer rows.Close()

	orders := make([]domain.Order, 0, limit)
	for rows.Next() {
		order, err := scanOrder(rows)
		if err != nil {
			return nil, wrapDBError(err, "чтение строки зависшего заказа")
		}
		orders = append(orders, order)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDBError(err, "обход зависших заказов")
	}

	return orders, nil
}

func (r *OrderRepo) itemsByOrders(ctx context.Context, orderIDs []int64) (map[int64][]domain.OrderItem, error) {
	const query = `
		SELECT order_id, id, product_id, product_key, product_name_snapshot,
		       unit_price_kopecks, qty, line_total_kopecks
		FROM order_items
		WHERE order_id = ANY($1::bigint[])
		ORDER BY order_id, id`

	rows, err := r.db(ctx).Query(ctx, query, orderIDs)
	if err != nil {
		return nil, wrapDBError(err, "чтение позиций заказов")
	}
	defer rows.Close()

	result := make(map[int64][]domain.OrderItem, len(orderIDs))
	for rows.Next() {
		var (
			orderID int64
			item    domain.OrderItem
		)
		if err := rows.Scan(&orderID, &item.ID, &item.ProductID, &item.ProductKey,
			&item.ProductNameSnapshot, &item.UnitPriceKopecks, &item.Qty,
			&item.LineTotalKopecks,
		); err != nil {
			return nil, wrapDBError(err, "чтение строки позиции заказа")
		}
		result[orderID] = append(result[orderID], item)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDBError(err, "обход позиций заказов")
	}

	return result, nil
}

func (r *OrderRepo) timelineByOrders(ctx context.Context, orderIDs []int64) (map[int64][]domain.StatusEvent, error) {
	const query = `
		SELECT order_id, id, from_status, to_status, actor, comment, created_at
		FROM order_status_events
		WHERE order_id = ANY($1::bigint[])
		ORDER BY order_id, id`

	rows, err := r.db(ctx).Query(ctx, query, orderIDs)
	if err != nil {
		return nil, wrapDBError(err, "чтение истории статусов")
	}
	defer rows.Close()

	result := make(map[int64][]domain.StatusEvent, len(orderIDs))
	for rows.Next() {
		var (
			orderID    int64
			event      domain.StatusEvent
			fromStatus string
			toStatus   string
			actor      string
		)
		if err := rows.Scan(&orderID, &event.ID, &fromStatus, &toStatus,
			&actor, &event.Comment, &event.CreatedAt,
		); err != nil {
			return nil, wrapDBError(err, "чтение строки истории статусов")
		}
		event.FromStatus = domain.OrderStatus(fromStatus)
		event.ToStatus = domain.OrderStatus(toStatus)
		event.Actor = domain.Actor(actor)
		result[orderID] = append(result[orderID], event)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDBError(err, "обход истории статусов")
	}

	return result, nil
}

func scanOrder(row rowScanner) (domain.Order, error) {
	var (
		order  domain.Order
		status string
	)

	err := row.Scan(
		&order.ID,
		&order.PublicNumber,
		&order.UserExternalID,
		&order.RestaurantID,
		&order.RestaurantSlug,
		&status,
		&order.DeliveryAddress,
		&order.SubtotalKopecks,
		&order.DeliveryFeeKopecks,
		&order.TotalKopecks,
		&order.CancelReason,
		&order.Version,
		&order.CreatedAt,
		&order.UpdatedAt,
	)
	if err != nil {
		return domain.Order{}, err
	}

	order.Status = domain.OrderStatus(status)
	return order, nil
}
