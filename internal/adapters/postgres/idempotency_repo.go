package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"avito-kitchen/internal/app"
)

// IdempotencyRepo хранит результаты операций, защищённых Idempotency-Key.
// Реализует app.IdempotencyRepo.
type IdempotencyRepo struct {
	base
}

// NewIdempotencyRepo создаёт репозиторий ключей идемпотентности.
func NewIdempotencyRepo(pool *pgxpool.Pool) *IdempotencyRepo {
	return &IdempotencyRepo{base{pool: pool}}
}

var _ app.IdempotencyRepo = (*IdempotencyRepo)(nil)

// Reserve захватывает ключ под выполнение операции.
//
// Захват — единственный атомарный INSERT ... ON CONFLICT. Он же обрабатывает
// протухший ключ: если срок жизни прежней записи истёк, DO UPDATE перезапишет
// её и вернёт строку (значит, ключ снова свободен). Если запись жива, WHERE в
// DO UPDATE не сработает, RETURNING ничего не отдаст — и мы дочитываем, что там
// уже лежит: готовый ответ на повтор или отметка «операция выполняется».
//
// Благодаря атомарности два параллельных запроса с одним ключом не могут оба
// оказаться «первыми»: захват достанется ровно одному.
func (r *IdempotencyRepo) Reserve(
	ctx context.Context,
	key, requestHash string,
	expiresAt time.Time,
) (app.IdempotencyRecord, bool, error) {
	const claimQuery = `
		INSERT INTO idempotency_keys (key, request_hash, response_status, response_body, expires_at)
		VALUES ($1, $2, 0, 'null'::jsonb, $3)
		ON CONFLICT (key) DO UPDATE
		SET request_hash    = EXCLUDED.request_hash,
		    response_status = 0,
		    response_body   = 'null'::jsonb,
		    created_at      = NOW(),
		    expires_at      = EXCLUDED.expires_at
		WHERE idempotency_keys.expires_at <= NOW()
		RETURNING key`

	db := r.db(ctx)

	var claimedKey string
	err := db.QueryRow(ctx, claimQuery, key, requestHash, expiresAt).Scan(&claimedKey)
	if err == nil {
		return app.IdempotencyRecord{Key: key, RequestHash: requestHash}, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return app.IdempotencyRecord{}, false, wrapDBError(err, "захват ключа идемпотентности")
	}

	const readQuery = `
		SELECT key, request_hash, response_status, response_body
		FROM idempotency_keys
		WHERE key = $1`

	var record app.IdempotencyRecord
	err = db.QueryRow(ctx, readQuery, key).Scan(
		&record.Key, &record.RequestHash, &record.ResponseStatus, &record.ResponseBody)
	if errors.Is(err, pgx.ErrNoRows) {
		// Запись исчезла между двумя запросами (например, её удалил reaper).
		// Для вызывающего это неотличимо от гонки — пусть повторит.
		return app.IdempotencyRecord{}, false, nil
	}
	if err != nil {
		return app.IdempotencyRecord{}, false, wrapDBError(err, "чтение ключа идемпотентности")
	}

	return record, false, nil
}

// Complete фиксирует ответ за ранее захваченным ключом.
func (r *IdempotencyRepo) Complete(ctx context.Context, key string, status int, body []byte) error {
	const query = `
		UPDATE idempotency_keys
		SET response_status = $2, response_body = $3
		WHERE key = $1`

	if _, err := r.db(ctx).Exec(ctx, query, key, status, body); err != nil {
		return wrapDBError(err, "сохранение ответа идемпотентной операции")
	}

	return nil
}

// Release снимает захват после неудачной операции.
//
// Удаляется только запись без зафиксированного ответа (response_status = 0):
// успешный результат стирать нельзя ни при каких обстоятельствах.
func (r *IdempotencyRepo) Release(ctx context.Context, key string) error {
	const query = `DELETE FROM idempotency_keys WHERE key = $1 AND response_status = 0`

	if _, err := r.db(ctx).Exec(ctx, query, key); err != nil {
		return wrapDBError(err, "освобождение ключа идемпотентности")
	}

	return nil
}

// DeleteExpired убирает протухшие ключи.
func (r *IdempotencyRepo) DeleteExpired(ctx context.Context, now time.Time) (int64, error) {
	const query = `DELETE FROM idempotency_keys WHERE expires_at <= $1`

	tag, err := r.db(ctx).Exec(ctx, query, now)
	if err != nil {
		return 0, wrapDBError(err, "удаление протухших ключей идемпотентности")
	}

	return tag.RowsAffected(), nil
}
