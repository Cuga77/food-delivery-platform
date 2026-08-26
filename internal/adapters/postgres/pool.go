// Package postgres содержит реализацию портов из internal/app поверх
// PostgreSQL и драйвера pgx/v5.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolConfig — параметры пула соединений.
type PoolConfig struct {
	DSN             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	ConnectTimeout  time.Duration
}

// NewPool поднимает пул и проверяет связь с базой.
func NewPool(ctx context.Context, cfg PoolConfig) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("разбор DATABASE_URL: %w", err)
	}

	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		poolCfg.MinConns = cfg.MinConns
	}
	if cfg.MaxConnLifetime > 0 {
		poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	}
	if cfg.MaxConnIdleTime > 0 {
		poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime
	}

	// Запросы описаны параметризованными плейсхолдерами и выполняются часто —
	// кэш подготовленных выражений на соединении экономит парсинг и планирование.
	poolCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheStatement

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("создание пула соединений: %w", err)
	}

	pingCtx := ctx
	if cfg.ConnectTimeout > 0 {
		var cancel context.CancelFunc
		pingCtx, cancel = context.WithTimeout(ctx, cfg.ConnectTimeout)
		defer cancel()
	}

	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("проверка соединения с БД: %w", err)
	}

	return pool, nil
}

// Querier — общий знаменатель *pgxpool.Pool и pgx.Tx. Репозитории работают
// через него и не различают транзакционный и обычный вызов.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// base — общая часть всех репозиториев: доступ к пулу и выбор источника
// запросов с учётом текущей транзакции.
type base struct {
	pool *pgxpool.Pool
}

// db возвращает транзакцию из контекста, если она открыта, иначе — пул.
func (b base) db(ctx context.Context) Querier {
	if tx, ok := txFromContext(ctx); ok {
		return tx
	}
	return b.pool
}
