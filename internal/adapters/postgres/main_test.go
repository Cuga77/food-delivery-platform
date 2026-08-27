//go:build integration

// Интеграционные тесты слоя PostgreSQL. Запускаются только с тегом
// `integration` и требуют живую базу:
//
//	make up && make test-integration
//
// Модульные тесты бизнес-правил живут в internal/app и базы не требуют.
// Здесь проверяется ровно то, что нельзя проверить на фейках: атомарность
// списаний, оптимистичные блокировки, SKIP LOCKED и обратимость миграций.
package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"avito-kitchen/internal/app"
	"avito-kitchen/internal/domain"
)

var (
	testPool *pgxpool.Pool
	testDSN  string
	uniqueN  atomic.Int64
)

func TestMain(m *testing.M) {
	testDSN = os.Getenv("TEST_DATABASE_URL")
	if testDSN == "" {
		fmt.Fprintln(os.Stderr,
			"TEST_DATABASE_URL не задан — интеграционные тесты пропущены (см. make test-integration)")
		os.Exit(0)
	}

	ctx := context.Background()

	if err := migrateUp(); err != nil {
		fmt.Fprintf(os.Stderr, "не удалось применить миграции: %v\n", err)
		os.Exit(1)
	}

	pool, err := NewPool(ctx, PoolConfig{DSN: testDSN, MaxConns: 16, ConnectTimeout: 10 * time.Second})
	if err != nil {
		fmt.Fprintf(os.Stderr, "не удалось подключиться к БД: %v\n", err)
		os.Exit(1)
	}
	testPool = pool

	code := m.Run()

	pool.Close()
	os.Exit(code)
}

func newMigrator() (*migrate.Migrate, error) {
	abs, err := filepath.Abs(filepath.Join("..", "..", "..", "migrations"))
	if err != nil {
		return nil, err
	}
	return migrate.New("file://"+abs, testDSN)
}

func migrateUp() error {
	m, err := newMigrator()
	if err != nil {
		return err
	}
	defer m.Close()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}

// unique даёт уникальный суффикс, чтобы тесты не мешали друг другу и не
// требовали очистки базы между собой.
func unique(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, time.Now().UnixNano(), uniqueN.Add(1))
}

// testEnv — репозитории и сервисы, собранные на настоящей БД.
type testEnv struct {
	pool        *pgxpool.Pool
	tx          *TxManager
	restaurants *RestaurantRepo
	menus       *MenuRepo
	orders      *OrderRepo
	idempotency *IdempotencyRepo
	outbox      *OutboxRepo

	orderService   *app.OrderService
	partnerService *app.PartnerService
	catalogService *app.CatalogService
}

func newTestEnv(t testing.TB) *testEnv {
	t.Helper()

	env := &testEnv{
		pool:        testPool,
		tx:          NewTxManager(testPool),
		restaurants: NewRestaurantRepo(testPool),
		menus:       NewMenuRepo(testPool),
		orders:      NewOrderRepo(testPool),
		idempotency: NewIdempotencyRepo(testPool),
		outbox:      NewOutboxRepo(testPool),
	}

	env.orderService = app.NewOrderService(env.tx, env.restaurants, env.menus, env.orders, env.outbox)
	env.partnerService = app.NewPartnerService(env.tx, env.restaurants, env.menus, env.orders)
	env.catalogService = app.NewCatalogService(env.restaurants, env.menus)

	return env
}

// seedRestaurant создаёт заведение со случайным slug и токеном.
func (e *testEnv) seedRestaurant(
	t testing.TB,
	status domain.RestaurantStatus,
	minOrder, deliveryFee int64,
) (restaurant domain.Restaurant, partnerToken string) {
	t.Helper()

	slug := unique("test-rest")
	token := unique("token")

	const query = `
		INSERT INTO restaurants (slug, name, status, provider_base_url, api_key_hash,
		                         min_order_kopecks, delivery_fee_kopecks)
		VALUES ($1, $2, $3, 'http://localhost:9999', $4, $5, $6)
		RETURNING id`

	var id int64
	err := e.pool.QueryRow(context.Background(), query,
		slug, "Тестовое заведение "+slug, string(status), app.HashToken(token),
		minOrder, deliveryFee).Scan(&id)
	require.NoError(t, err)

	restaurant, err = e.restaurants.GetByID(context.Background(), id)
	require.NoError(t, err)

	return restaurant, token
}

// seedMenu публикует меню заведения через боевой путь (PublishVersion).
func (e *testEnv) seedMenu(t testing.TB, restaurantID int64, products []domain.Product) domain.MenuSnapshot {
	t.Helper()

	_, err := e.partnerService.SyncMenu(context.Background(), restaurantID, products)
	require.NoError(t, err)

	snapshot, err := e.menus.GetPublished(context.Background(), restaurantID)
	require.NoError(t, err)

	return snapshot
}

func (e *testEnv) productID(t testing.TB, snapshot domain.MenuSnapshot, key string) int64 {
	t.Helper()

	product, ok := snapshot.ProductByKey(key)
	require.Truef(t, ok, "в меню нет позиции %q", key)

	return product.ID
}

func (e *testEnv) stockOf(t testing.TB, productID int64) *int32 {
	t.Helper()

	var stock *int32
	err := e.pool.QueryRow(context.Background(),
		`SELECT stock_qty FROM products WHERE id = $1`, productID).Scan(&stock)
	require.NoError(t, err)

	return stock
}

func ptrInt32(v int32) *int32 { return &v }

// testHash возвращает хэш той же формы, что и боевой код: ровно 64 hex-символа.
// Короткие строки сюда подставлять нельзя — request_hash имеет тип CHAR(64) и
// дополняет значение пробелами до фиксированной длины.
func testHash(payload string) string {
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}
