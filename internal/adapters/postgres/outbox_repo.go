package postgres

import (
	"context"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"

	"avito-kitchen/internal/app"
)

// OutboxRepo — очередь исходящих событий поверх таблицы outbox_events.
// Реализует app.OutboxRepo.
type OutboxRepo struct {
	base
}

// NewOutboxRepo создаёт репозиторий outbox.
func NewOutboxRepo(pool *pgxpool.Pool) *OutboxRepo {
	return &OutboxRepo{base{pool: pool}}
}

var _ app.OutboxRepo = (*OutboxRepo)(nil)

// Enqueue добавляет событие. Вызывается внутри бизнес-транзакции, поэтому
// событие фиксируется ровно тогда же, когда и изменение заказа: либо есть и
// то, и другое, либо ничего.
func (r *OutboxRepo) Enqueue(ctx context.Context, event app.OutboxEvent) error {
	const query = `
		INSERT INTO outbox_events (aggregate_type, aggregate_id, event_type, payload)
		VALUES ($1, $2, $3, $4)`

	if _, err := r.db(ctx).Exec(ctx, query,
		event.AggregateType, event.AggregateID, event.EventType, []byte(event.Payload),
	); err != nil {
		return wrapDBError(err, "постановка события в outbox")
	}

	return nil
}

// ClaimBatch забирает пачку событий, готовых к доставке, и сразу выдаёт на них
// аренду.
//
// Один запрос делает три вещи:
//   - SKIP LOCKED пропускает строки, уже захваченные соседним воркером, поэтому
//     несколько экземпляров api могут разгребать очередь параллельно и не
//     блокировать друг друга;
//   - сдвиг next_retry_at на lease вперёд — это аренда: событие не будет
//     выдано повторно, пока текущий воркер его доставляет;
//   - RETURNING сразу отдаёт содержимое, так что второй запрос не нужен.
//
// Аренда позволяет не держать транзакцию открытой на время HTTP-запроса к
// заведению: если воркер умрёт в процессе, событие само вернётся в очередь по
// истечении аренды (ADR-0003).
func (r *OutboxRepo) ClaimBatch(ctx context.Context, limit int32, lease time.Duration) ([]app.OutboxEvent, error) {
	const query = `
		WITH claimed AS (
			SELECT id
			FROM outbox_events
			WHERE sent_at IS NULL
			  AND dead_at IS NULL
			  AND next_retry_at <= NOW()
			ORDER BY id
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE outbox_events o
		SET next_retry_at = NOW() + make_interval(secs => $2::double precision)
		FROM claimed c
		WHERE o.id = c.id
		RETURNING o.id, o.aggregate_type, o.aggregate_id, o.event_type,
		          o.payload, o.attempts, o.next_retry_at, o.created_at`

	rows, err := r.db(ctx).Query(ctx, query, limit, lease.Seconds())
	if err != nil {
		return nil, wrapDBError(err, "захват событий outbox")
	}
	defer rows.Close()

	events := make([]app.OutboxEvent, 0, limit)
	for rows.Next() {
		var (
			event   app.OutboxEvent
			payload []byte
		)
		if err := rows.Scan(&event.ID, &event.AggregateType, &event.AggregateID,
			&event.EventType, &payload, &event.Attempts, &event.NextRetryAt,
			&event.CreatedAt,
		); err != nil {
			return nil, wrapDBError(err, "чтение строки события outbox")
		}
		event.Payload = payload
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDBError(err, "обход событий outbox")
	}

	return events, nil
}

// MarkSent помечает событие доставленным.
func (r *OutboxRepo) MarkSent(ctx context.Context, id int64) error {
	const query = `
		UPDATE outbox_events
		SET sent_at = NOW(), last_error = NULL
		WHERE id = $1 AND sent_at IS NULL`

	if _, err := r.db(ctx).Exec(ctx, query, id); err != nil {
		return wrapDBError(err, "отметка события доставленным")
	}

	return nil
}

// MarkFailed переносит следующую попытку и сохраняет причину неудачи.
func (r *OutboxRepo) MarkFailed(ctx context.Context, id int64, next time.Time, reason string) error {
	const query = `
		UPDATE outbox_events
		SET attempts = attempts + 1, next_retry_at = $2, last_error = $3
		WHERE id = $1 AND sent_at IS NULL AND dead_at IS NULL`

	if _, err := r.db(ctx).Exec(ctx, query, id, next, truncateReason(reason)); err != nil {
		return wrapDBError(err, "планирование повторной доставки события")
	}

	return nil
}

// MarkDead снимает событие с доставки после исчерпания попыток.
//
// Строка не удаляется: она остаётся свидетельством инцидента и точкой, с
// которой начнётся ручной разбор.
func (r *OutboxRepo) MarkDead(ctx context.Context, id int64, reason string) error {
	const query = `
		UPDATE outbox_events
		SET attempts = attempts + 1, dead_at = NOW(), last_error = $2
		WHERE id = $1 AND sent_at IS NULL AND dead_at IS NULL`

	if _, err := r.db(ctx).Exec(ctx, query, id, truncateReason(reason)); err != nil {
		return wrapDBError(err, "снятие события с доставки")
	}

	return nil
}

// maxReasonLength ограничивает текст причины: в last_error нужен повод для
// разбора, а не мегабайтный дамп чужого ответа.
const maxReasonLength = 1000

// truncateReason обрезает причину по границе символа.
//
// Срез по байтам разрубил бы многобайтную руну пополам: колонка TEXT такое
// примет, но в last_error осталась бы битая последовательность — а это
// единственный след инцидента, по которому его потом разбирают. Сообщения
// сервиса на русском, поэтому почти каждый обрыв пришёлся бы на середину.
func truncateReason(reason string) string {
	if len(reason) <= maxReasonLength {
		return reason
	}

	cut := maxReasonLength
	for cut > 0 && !utf8.RuneStart(reason[cut]) {
		cut--
	}

	return reason[:cut] + "…"
}
