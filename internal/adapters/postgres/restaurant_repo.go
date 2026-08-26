package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"avito-kitchen/internal/app"
	"avito-kitchen/internal/domain"
)

// restaurantColumns перечисляется один раз, чтобы порядок полей в SELECT и в
// scanRestaurant не разъезжался при правках.
const restaurantColumns = `
	id, slug, name, status, provider_base_url, api_key_hash,
	min_order_kopecks, delivery_fee_kopecks, created_at, updated_at`

// RestaurantRepo — доступ к заведениям. Реализует app.RestaurantRepo.
type RestaurantRepo struct {
	base
}

// NewRestaurantRepo создаёт репозиторий заведений.
func NewRestaurantRepo(pool *pgxpool.Pool) *RestaurantRepo {
	return &RestaurantRepo{base{pool: pool}}
}

var _ app.RestaurantRepo = (*RestaurantRepo)(nil)

// List отдаёт страницу каталога по keyset-пагинации.
//
// Условие `id > $1` вместо OFFSET: выборка идёт по первичному ключу, поэтому
// стоимость запроса не зависит от глубины страницы.
func (r *RestaurantRepo) List(ctx context.Context, filter app.RestaurantFilter) ([]domain.Restaurant, error) {
	const query = `
		SELECT ` + restaurantColumns + `
		FROM restaurants
		WHERE id > $1
		  AND ($2::text IS NULL OR status = $2::text)
		ORDER BY id
		LIMIT $3`

	var status *string
	if filter.Status != nil {
		s := string(*filter.Status)
		status = &s
	}

	rows, err := r.db(ctx).Query(ctx, query, filter.AfterID, status, filter.Limit)
	if err != nil {
		return nil, wrapDBError(err, "выборка каталога заведений")
	}
	defer rows.Close()

	restaurants := make([]domain.Restaurant, 0, filter.Limit)
	for rows.Next() {
		restaurant, err := scanRestaurant(rows)
		if err != nil {
			return nil, wrapDBError(err, "чтение строки каталога")
		}
		restaurants = append(restaurants, restaurant)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDBError(err, "обход каталога заведений")
	}

	return restaurants, nil
}

// GetByID находит заведение по идентификатору.
func (r *RestaurantRepo) GetByID(ctx context.Context, id int64) (domain.Restaurant, error) {
	const query = `SELECT ` + restaurantColumns + ` FROM restaurants WHERE id = $1`

	restaurant, err := scanRestaurant(r.db(ctx).QueryRow(ctx, query, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Restaurant{}, domain.Errorf(domain.CodeRestaurantNotFound,
			"заведение с id=%d не найдено", id)
	}
	if err != nil {
		return domain.Restaurant{}, wrapDBError(err, "чтение заведения по id")
	}

	return restaurant, nil
}

// GetBySlug находит заведение по человекочитаемому идентификатору.
func (r *RestaurantRepo) GetBySlug(ctx context.Context, slug string) (domain.Restaurant, error) {
	const query = `SELECT ` + restaurantColumns + ` FROM restaurants WHERE slug = $1`

	restaurant, err := scanRestaurant(r.db(ctx).QueryRow(ctx, query, slug))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Restaurant{}, domain.Errorf(domain.CodeRestaurantNotFound,
			"заведение «%s» не найдено", slug)
	}
	if err != nil {
		return domain.Restaurant{}, wrapDBError(err, "чтение заведения по slug")
	}

	return restaurant, nil
}

// GetByAPIKeyHash находит заведение по SHA-256 партнёрского токена.
//
// Поиск идёт по уникальному индексу на api_key_hash, а не перебором с
// посимвольным сравнением: время ответа не зависит от того, какой токен
// прислан, и не даёт подбирать его по таймингам.
func (r *RestaurantRepo) GetByAPIKeyHash(ctx context.Context, apiKeyHash string) (domain.Restaurant, error) {
	const query = `SELECT ` + restaurantColumns + ` FROM restaurants WHERE api_key_hash = $1`

	restaurant, err := scanRestaurant(r.db(ctx).QueryRow(ctx, query, apiKeyHash))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Restaurant{}, domain.Errorf(domain.CodeRestaurantNotFound,
			"заведение с таким токеном не найдено")
	}
	if err != nil {
		return domain.Restaurant{}, wrapDBError(err, "аутентификация заведения")
	}

	return restaurant, nil
}

// UpdateStatus переключает режим работы кухни.
func (r *RestaurantRepo) UpdateStatus(ctx context.Context, id int64, status domain.RestaurantStatus) error {
	const query = `
		UPDATE restaurants
		SET status = $2, updated_at = NOW()
		WHERE id = $1`

	tag, err := r.db(ctx).Exec(ctx, query, id, string(status))
	if err != nil {
		return wrapDBError(err, "смена режима работы заведения")
	}
	if tag.RowsAffected() == 0 {
		return domain.Errorf(domain.CodeRestaurantNotFound, "заведение с id=%d не найдено", id)
	}

	return nil
}

// rowScanner объединяет pgx.Row и pgx.Rows: у обоих есть Scan.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanRestaurant(row rowScanner) (domain.Restaurant, error) {
	var (
		restaurant domain.Restaurant
		status     string
	)

	err := row.Scan(
		&restaurant.ID,
		&restaurant.Slug,
		&restaurant.Name,
		&status,
		&restaurant.ProviderBaseURL,
		new(string), // api_key_hash наружу не выносим
		&restaurant.MinOrderKopecks,
		&restaurant.DeliveryFeeKopecks,
		&restaurant.CreatedAt,
		&restaurant.UpdatedAt,
	)
	if err != nil {
		return domain.Restaurant{}, err
	}

	restaurant.Status = domain.RestaurantStatus(status)
	return restaurant, nil
}
