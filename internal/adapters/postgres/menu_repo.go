package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"avito-kitchen/internal/app"
	"avito-kitchen/internal/domain"
)

// MenuRepo — доступ к версиям меню, позициям и остаткам.
// Реализует app.MenuRepo.
type MenuRepo struct {
	base
}

// NewMenuRepo создаёт репозиторий меню.
func NewMenuRepo(pool *pgxpool.Pool) *MenuRepo {
	return &MenuRepo{base{pool: pool}}
}

var _ app.MenuRepo = (*MenuRepo)(nil)

// GetPublished возвращает опубликованную версию меню вместе с позициями.
//
// Позиции отдаются в порядке вставки (по id): именно он определяет порядок
// категорий и блюд на витрине, а его задаёт заведение при синхронизации.
func (r *MenuRepo) GetPublished(ctx context.Context, restaurantID int64) (domain.MenuSnapshot, error) {
	const menuQuery = `
		SELECT id, restaurant_id, version, status, created_at
		FROM menus
		WHERE restaurant_id = $1 AND status = 'published'`

	var (
		menu   domain.Menu
		status string
	)
	err := r.db(ctx).QueryRow(ctx, menuQuery, restaurantID).Scan(
		&menu.ID, &menu.RestaurantID, &menu.Version, &status, &menu.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.MenuSnapshot{}, domain.Errorf(domain.CodeMenuNotFound,
			"у заведения нет опубликованного меню")
	}
	if err != nil {
		return domain.MenuSnapshot{}, wrapDBError(err, "чтение опубликованного меню")
	}
	menu.Status = domain.MenuStatus(status)

	products, err := r.productsByMenu(ctx, menu.ID)
	if err != nil {
		return domain.MenuSnapshot{}, err
	}

	return domain.MenuSnapshot{Menu: menu, Products: products}, nil
}

func (r *MenuRepo) productsByMenu(ctx context.Context, menuID int64) ([]domain.Product, error) {
	const query = `
		SELECT id, menu_id, product_key, category, name, description,
		       price_kopecks, available, stock_qty
		FROM products
		WHERE menu_id = $1
		ORDER BY id`

	rows, err := r.db(ctx).Query(ctx, query, menuID)
	if err != nil {
		return nil, wrapDBError(err, "чтение позиций меню")
	}
	defer rows.Close()

	products := make([]domain.Product, 0, 32)
	for rows.Next() {
		var p domain.Product
		if err := rows.Scan(
			&p.ID, &p.MenuID, &p.ProductKey, &p.Category, &p.Name, &p.Description,
			&p.PriceKopecks, &p.Available, &p.StockQty,
		); err != nil {
			return nil, wrapDBError(err, "чтение строки позиции меню")
		}
		products = append(products, p)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDBError(err, "обход позиций меню")
	}

	return products, nil
}

// PublishVersion архивирует текущее меню и публикует следующую версию.
//
// Первым делом берётся блокировка строки заведения. Без неё две одновременные
// синхронизации вычислили бы одинаковый номер версии и одна из них упала бы на
// уникальном индексе (restaurant_id, version); блокировка выстраивает их в
// очередь, и обе проходят.
func (r *MenuRepo) PublishVersion(
	ctx context.Context,
	restaurantID int64,
	products []domain.Product,
) (domain.Menu, error) {
	db := r.db(ctx)

	const lockQuery = `SELECT id FROM restaurants WHERE id = $1 FOR UPDATE`
	var lockedID int64
	if err := db.QueryRow(ctx, lockQuery, restaurantID).Scan(&lockedID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Menu{}, domain.Errorf(domain.CodeRestaurantNotFound,
				"заведение с id=%d не найдено", restaurantID)
		}
		return domain.Menu{}, wrapDBError(err, "блокировка заведения перед публикацией меню")
	}

	const archiveQuery = `
		UPDATE menus
		SET status = 'archived'
		WHERE restaurant_id = $1 AND status = 'published'`
	if _, err := db.Exec(ctx, archiveQuery, restaurantID); err != nil {
		return domain.Menu{}, wrapDBError(err, "архивация предыдущей версии меню")
	}

	const insertMenuQuery = `
		INSERT INTO menus (restaurant_id, version, status)
		VALUES ($1,
		        (SELECT COALESCE(MAX(version), 0) + 1 FROM menus WHERE restaurant_id = $1),
		        'published')
		RETURNING id, restaurant_id, version, status, created_at`

	var (
		menu   domain.Menu
		status string
	)
	if err := db.QueryRow(ctx, insertMenuQuery, restaurantID).Scan(
		&menu.ID, &menu.RestaurantID, &menu.Version, &status, &menu.CreatedAt,
	); err != nil {
		return domain.Menu{}, wrapDBError(err, "создание новой версии меню")
	}
	menu.Status = domain.MenuStatus(status)

	if err := r.insertProducts(ctx, menu.ID, products); err != nil {
		return domain.Menu{}, err
	}

	return menu, nil
}

// insertProducts вставляет позиции одним запросом через unnest: количество
// round-trip'ов не зависит от размера меню.
func (r *MenuRepo) insertProducts(ctx context.Context, menuID int64, products []domain.Product) error {
	const query = `
		INSERT INTO products (menu_id, product_key, category, name, description,
		                      price_kopecks, available, stock_qty)
		SELECT $1, k, c, n, d, p, a, s
		FROM unnest($2::text[], $3::text[], $4::text[], $5::text[],
		            $6::bigint[], $7::boolean[], $8::int[])
		     AS t(k, c, n, d, p, a, s)`

	var (
		keys         = make([]string, len(products))
		categories   = make([]string, len(products))
		names        = make([]string, len(products))
		descriptions = make([]string, len(products))
		prices       = make([]int64, len(products))
		available    = make([]bool, len(products))
		stock        = make([]*int32, len(products))
	)

	for i, p := range products {
		keys[i] = p.ProductKey
		categories[i] = p.Category
		names[i] = p.Name
		descriptions[i] = p.Description
		prices[i] = p.PriceKopecks
		available[i] = p.Available
		stock[i] = p.StockQty
	}

	if _, err := r.db(ctx).Exec(ctx, query, menuID,
		keys, categories, names, descriptions, prices, available, stock,
	); err != nil {
		return wrapDBError(err, "вставка позиций меню")
	}

	return nil
}

// DecreaseStock атомарно списывает остаток позиции.
//
// Проверка доступности и достаточности остатка встроена в WHERE самого
// UPDATE — между проверкой и списанием нет окна, в которое могла бы вклиниться
// параллельная транзакция. Строка блокируется на время транзакции, поэтому
// два одновременных заказа на последнюю единицу выстраиваются в очередь и
// второй получает OUT_OF_STOCK.
func (r *MenuRepo) DecreaseStock(ctx context.Context, productID int64, qty int32) error {
	const query = `
		UPDATE products
		SET stock_qty = stock_qty - $2
		WHERE id = $1
		  AND available = TRUE
		  AND (stock_qty IS NULL OR stock_qty >= $2)
		RETURNING stock_qty`

	var remaining *int32
	err := r.db(ctx).QueryRow(ctx, query, productID, qty).Scan(&remaining)
	if err == nil {
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return wrapDBError(err, "списание остатка позиции")
	}

	// Списание не прошло. Причину выясняем отдельным запросом — только ради
	// внятного текста ошибки для клиента.
	return r.explainStockFailure(ctx, productID, qty)
}

func (r *MenuRepo) explainStockFailure(ctx context.Context, productID int64, qty int32) error {
	const query = `
		SELECT product_key, name, available, stock_qty
		FROM products
		WHERE id = $1`

	var (
		productKey string
		name       string
		available  bool
		stockQty   *int32
	)
	err := r.db(ctx).QueryRow(ctx, query, productID).Scan(&productKey, &name, &available, &stockQty)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Errorf(domain.CodeProductUnavailable,
			"позиция была удалена из меню, пока формировался заказ")
	}
	if err != nil {
		return wrapDBError(err, "уточнение причины отказа в списании остатка")
	}

	if !available {
		return domain.Errorf(domain.CodeProductUnavailable,
			"позиция «%s» (ключ: %s) стала недоступна", name, productKey)
	}

	availableQty := int32(0)
	if stockQty != nil {
		availableQty = *stockQty
	}
	return domain.Errorf(domain.CodeOutOfStock,
		"позиции «%s» (ключ: %s) осталось %d, а запрошено %d",
		name, productKey, availableQty, qty)
}

// RestoreStock возвращает остаток при отмене или отказе.
//
// Возврат применяется только к позициям опубликованного меню. Если заведение
// успело синхронизировать новую версию, позиция заказа принадлежит архивной
// версии, и возвращать остаток некуда: актуальные остатки заведение прислало
// заново. Такой случай — штатный no-op, а не ошибка (ADR-0002).
func (r *MenuRepo) RestoreStock(ctx context.Context, productID int64, qty int32) error {
	const query = `
		UPDATE products p
		SET stock_qty = p.stock_qty + $2
		FROM menus m
		WHERE p.id = $1
		  AND m.id = p.menu_id
		  AND m.status = 'published'
		  AND p.stock_qty IS NOT NULL`

	if _, err := r.db(ctx).Exec(ctx, query, productID, qty); err != nil {
		return wrapDBError(err, "возврат остатка позиции")
	}

	return nil
}
