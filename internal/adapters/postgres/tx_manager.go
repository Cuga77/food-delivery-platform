package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// txContextKey — приватный ключ, под которым транзакция живёт в контексте.
// Тип неэкспортируемый, поэтому подменить значение извне пакета невозможно.
type txContextKey struct{}

func withTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, txContextKey{}, tx)
}

func txFromContext(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txContextKey{}).(pgx.Tx)
	return tx, ok
}

// TxManager реализует app.TxManager.
//
// Транзакция прокидывается через context, а не через сигнатуры методов. Это
// даёт два свойства: use case описывает границу транзакции одним вызовом
// WithinTx и не таскает *pgx.Tx через слои, а репозитории остаются
// одинаковыми внутри и снаружи транзакции.
type TxManager struct {
	pool *pgxpool.Pool
}

// NewTxManager создаёт менеджер транзакций поверх пула.
func NewTxManager(pool *pgxpool.Pool) *TxManager {
	return &TxManager{pool: pool}
}

// WithinTx выполняет fn в транзакции: коммитит при успехе, откатывает при
// ошибке или панике.
//
// Вложенный вызов присоединяется к уже открытой транзакции, а не начинает
// новую: use case может вызвать другой use case, не рискуя разорвать
// атомарность сценария.
func (m *TxManager) WithinTx(ctx context.Context, fn func(ctx context.Context) error) (err error) {
	if _, ok := txFromContext(ctx); ok {
		return fn(ctx)
	}

	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("открытие транзакции: %w", err)
	}

	defer func() {
		if p := recover(); p != nil {
			// Откатываем на отдельном контексте: исходный мог быть уже отменён,
			// и тогда откат не успел бы отправиться в БД.
			_ = tx.Rollback(context.WithoutCancel(ctx))
			panic(p)
		}
	}()

	if err := fn(withTx(ctx, tx)); err != nil {
		if rbErr := tx.Rollback(context.WithoutCancel(ctx)); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			return errors.Join(err, fmt.Errorf("откат транзакции: %w", rbErr))
		}
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("фиксация транзакции: %w", err)
	}

	return nil
}
