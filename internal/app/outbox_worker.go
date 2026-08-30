package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"math/rand"
	"time"

	"avito-kitchen/internal/domain"
)

// OutboxConfig — параметры фонового воркера доставки событий.
type OutboxConfig struct {
	// PollInterval — пауза между опросами таблицы, когда очередь пуста.
	PollInterval time.Duration
	// BatchSize — сколько событий забирать за один проход.
	BatchSize int32
	// MaxAttempts — после скольких неудач событие снимается с доставки.
	MaxAttempts int32
	// BaseBackoff — задержка перед первой повторной попыткой; далее удваивается.
	BaseBackoff time.Duration
	// MaxBackoff — потолок экспоненциального роста задержки.
	MaxBackoff time.Duration
	// Lease — на сколько событие «арендуется» воркером на время доставки.
	// Должен превышать таймаут HTTP-запроса, иначе событие подхватит соседний
	// воркер, пока текущий ещё ждёт ответа.
	Lease time.Duration
}

// DefaultOutboxConfig — разумные значения по умолчанию.
func DefaultOutboxConfig() OutboxConfig {
	return OutboxConfig{
		PollInterval: 500 * time.Millisecond,
		BatchSize:    50,
		MaxAttempts:  10,
		BaseBackoff:  time.Second,
		MaxBackoff:   5 * time.Minute,
		Lease:        30 * time.Second,
	}
}

// OutboxWorker доставляет заведениям события, накопленные в таблице outbox.
//
// Гарантия — at-least-once: событие записано в одной транзакции с изменением
// заказа, поэтому не может потеряться, но при неудачном подтверждении может
// быть доставлено повторно. Дедупликация — задача получателя, для этого
// вместе с событием передаётся его идентификатор (заголовок X-Event-ID).
type OutboxWorker struct {
	outbox      OutboxRepo
	restaurants RestaurantRepo
	deliverer   EventDeliverer
	cfg         OutboxConfig
	log         *slog.Logger
	now         func() time.Time
}

// NewOutboxWorker собирает воркер доставки.
func NewOutboxWorker(
	outbox OutboxRepo,
	restaurants RestaurantRepo,
	deliverer EventDeliverer,
	cfg OutboxConfig,
	log *slog.Logger,
) *OutboxWorker {
	return &OutboxWorker{
		outbox:      outbox,
		restaurants: restaurants,
		deliverer:   deliverer,
		cfg:         cfg,
		log:         log.With(slog.String("worker", "outbox")),
		now:         time.Now,
	}
}

// Run крутит цикл доставки до отмены контекста.
//
// Когда за проход удалось забрать полную пачку, следующий проход начинается
// сразу: очередь разгребается в темпе, а не по одному батчу за тик.
func (w *OutboxWorker) Run(ctx context.Context) error {
	w.log.InfoContext(ctx, "воркер outbox запущен",
		slog.Duration("poll_interval", w.cfg.PollInterval),
		slog.Int("batch_size", int(w.cfg.BatchSize)))

	for {
		processed, err := w.processBatch(ctx)
		switch {
		case errors.Is(err, context.Canceled):
			w.log.InfoContext(ctx, "воркер outbox остановлен")
			return nil
		case err != nil:
			w.log.ErrorContext(ctx, "проход outbox завершился ошибкой", slog.Any("error", err))
		}

		if processed >= int(w.cfg.BatchSize) && ctx.Err() == nil {
			continue
		}

		select {
		case <-ctx.Done():
			w.log.InfoContext(ctx, "воркер outbox остановлен")
			return nil
		case <-time.After(w.cfg.PollInterval):
		}
	}
}

func (w *OutboxWorker) processBatch(ctx context.Context) (int, error) {
	events, err := w.outbox.ClaimBatch(ctx, w.cfg.BatchSize, w.cfg.Lease)
	if err != nil {
		return 0, err
	}

	for i := range events {
		if ctx.Err() != nil {
			return len(events), ctx.Err()
		}
		w.deliver(ctx, events[i])
	}

	return len(events), nil
}

// eventTarget — минимальная часть полезной нагрузки, нужная воркеру, чтобы
// понять, кому доставлять событие. Остальное содержимое его не касается.
type eventTarget struct {
	RestaurantID int64 `json:"restaurant_id"`
}

func (w *OutboxWorker) deliver(ctx context.Context, event OutboxEvent) {
	log := w.log.With(
		slog.Int64("event_id", event.ID),
		slog.String("event_type", event.EventType),
		slog.String("aggregate_id", event.AggregateID),
		slog.Int("attempts", int(event.Attempts)))

	var target eventTarget
	if err := json.Unmarshal(event.Payload, &target); err != nil || target.RestaurantID == 0 {
		// Такое событие не станет доставляемым ни при какой попытке.
		log.ErrorContext(ctx, "в событии не определён получатель, снимаем с доставки")
		w.markDead(ctx, event, "в payload отсутствует restaurant_id")
		return
	}

	restaurant, err := w.restaurants.GetByID(ctx, target.RestaurantID)
	if err != nil {
		if domain.HasCode(err, domain.CodeRestaurantNotFound) {
			log.ErrorContext(ctx, "заведение-получатель не найдено, снимаем с доставки")
			w.markDead(ctx, event, "заведение-получатель не найдено")
			return
		}
		w.retry(ctx, event, err)
		return
	}

	deliveryErr := w.deliverer.Deliver(ctx, restaurant.ProviderBaseURL, DeliveryEvent{
		EventID:     event.ID,
		EventType:   event.EventType,
		AggregateID: event.AggregateID,
		Payload:     event.Payload,
		CreatedAt:   event.CreatedAt,
	})
	if deliveryErr != nil {
		// Окончательный отказ не имеет смысла повторять: ответ не изменится, а
		// попытки только оттянут разбор инцидента.
		if errors.Is(deliveryErr, ErrDeliveryRejected) {
			log.ErrorContext(ctx, "заведение отвергло событие, повторять не будем",
				slog.Any("error", deliveryErr))
			w.markDead(ctx, event, deliveryErr.Error())
			return
		}

		w.retry(ctx, event, deliveryErr)
		return
	}

	if err := w.outbox.MarkSent(ctx, event.ID); err != nil {
		// Событие доставлено, но отметка не прошла: после истечения аренды оно
		// уйдёт повторно. Именно поэтому получатель обязан дедуплицировать.
		log.ErrorContext(ctx, "не удалось отметить событие доставленным",
			slog.Any("error", err))
		return
	}

	log.DebugContext(ctx, "событие доставлено",
		slog.String("restaurant", restaurant.Slug))
}

func (w *OutboxWorker) retry(ctx context.Context, event OutboxEvent, cause error) {
	attempts := event.Attempts + 1
	log := w.log.With(
		slog.Int64("event_id", event.ID),
		slog.Int("attempts", int(attempts)),
		slog.Any("error", cause))

	if attempts >= w.cfg.MaxAttempts {
		log.ErrorContext(ctx, "попытки доставки исчерпаны, событие снято с доставки")
		w.markDead(ctx, event, cause.Error())
		return
	}

	next := w.now().Add(w.backoff(attempts))
	if err := w.outbox.MarkFailed(ctx, event.ID, next, cause.Error()); err != nil {
		log.ErrorContext(ctx, "не удалось запланировать повтор", slog.Any("mark_error", err))
		return
	}

	log.WarnContext(ctx, "доставка не удалась, запланирован повтор",
		slog.Time("next_retry_at", next))
}

func (w *OutboxWorker) markDead(ctx context.Context, event OutboxEvent, reason string) {
	if err := w.outbox.MarkDead(ctx, event.ID, reason); err != nil {
		w.log.ErrorContext(ctx, "не удалось снять событие с доставки",
			slog.Int64("event_id", event.ID), slog.Any("error", err))
	}
}

// backoff считает экспоненциальную задержку с полным джиттером.
//
// Джиттер обязателен: без него пачка событий, отвалившаяся из-за недоступности
// заведения, будет повторяться синхронно и «выстреливать» в него залпами.
func (w *OutboxWorker) backoff(attempts int32) time.Duration {
	if attempts < 1 {
		attempts = 1
	}

	// Ограничиваем показатель, чтобы сдвиг не переполнил int64.
	const maxShift = 32
	shift := math.Min(float64(attempts-1), maxShift)

	delay := time.Duration(float64(w.cfg.BaseBackoff) * math.Pow(2, shift))
	if delay <= 0 || delay > w.cfg.MaxBackoff {
		delay = w.cfg.MaxBackoff
	}

	// Full jitter: равномерно размазываем повтор по всему окну [0, delay].
	return time.Duration(rand.Int63n(int64(delay)) + 1) //nolint:gosec // джиттер не требует криптостойкости
}
