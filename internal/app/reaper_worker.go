package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"avito-kitchen/internal/domain"
)

// ReaperConfig — параметры фоновой уборки.
type ReaperConfig struct {
	// Interval — как часто запускается проход.
	Interval time.Duration
	// OrderAcceptTimeout — сколько заказ может провисеть в статусе NEW, прежде
	// чем платформа отменит его за заведение.
	OrderAcceptTimeout time.Duration
	// BatchSize — максимум заказов, отменяемых за один проход.
	BatchSize int32
}

// DefaultReaperConfig — разумные значения по умолчанию.
func DefaultReaperConfig() ReaperConfig {
	return ReaperConfig{
		Interval:           30 * time.Second,
		OrderAcceptTimeout: 5 * time.Minute,
		BatchSize:          100,
	}
}

// ReaperWorker закрывает два «долга» системы, которые иначе накапливаются молча:
//
//  1. Заказ, на который заведение не ответило, вечно висит в NEW и держит
//     списанные остатки. Через OrderAcceptTimeout платформа отменяет его от
//     лица system и возвращает остатки в каталог.
//  2. Таблица idempotency_keys растёт без ограничений — протухшие ключи
//     удаляются по expires_at.
type ReaperWorker struct {
	orders       OrderRepo
	orderService *OrderService
	idempotency  IdempotencyRepo
	cfg          ReaperConfig
	log          *slog.Logger
	now          func() time.Time
}

// NewReaperWorker собирает воркер фоновой уборки.
func NewReaperWorker(
	orders OrderRepo,
	orderService *OrderService,
	idempotency IdempotencyRepo,
	cfg ReaperConfig,
	log *slog.Logger,
) *ReaperWorker {
	return &ReaperWorker{
		orders:       orders,
		orderService: orderService,
		idempotency:  idempotency,
		cfg:          cfg,
		log:          log.With(slog.String("worker", "reaper")),
		now:          time.Now,
	}
}

// Run крутит цикл уборки до отмены контекста.
func (w *ReaperWorker) Run(ctx context.Context) error {
	w.log.InfoContext(ctx, "воркер уборки запущен",
		slog.Duration("interval", w.cfg.Interval),
		slog.Duration("order_accept_timeout", w.cfg.OrderAcceptTimeout))

	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.log.InfoContext(ctx, "воркер уборки остановлен")
			return nil
		case <-ticker.C:
			w.cancelStaleOrders(ctx)
			w.purgeIdempotencyKeys(ctx)
		}
	}
}

func (w *ReaperWorker) cancelStaleOrders(ctx context.Context) {
	deadline := w.now().Add(-w.cfg.OrderAcceptTimeout)

	stale, err := w.orders.ListStaleNew(ctx, deadline, w.cfg.BatchSize)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			w.log.ErrorContext(ctx, "не удалось выбрать зависшие заказы", slog.Any("error", err))
		}
		return
	}

	for i := range stale {
		if ctx.Err() != nil {
			return
		}
		order := stale[i]

		_, err := w.orderService.ChangeStatus(ctx, StatusChange{
			PublicNumber: order.PublicNumber,
			To:           domain.StatusCancelled,
			Actor:        domain.ActorSystem,
			Comment:      "заведение не подтвердило заказ вовремя",
			Reason:       "заведение не подтвердило заказ вовремя",
		})
		switch {
		case err == nil:
			w.log.InfoContext(ctx, "заказ отменён по таймауту подтверждения",
				slog.String("public_number", order.PublicNumber.String()))
		case domain.HasCode(err, domain.CodeStateConflict):
			// Заведение успело ответить между выборкой и обновлением — штатная
			// гонка, оптимистичная блокировка отработала как задумано.
			w.log.DebugContext(ctx, "заказ уже сменил статус, отмена не требуется",
				slog.String("public_number", order.PublicNumber.String()))
		default:
			w.log.ErrorContext(ctx, "не удалось отменить зависший заказ",
				slog.String("public_number", order.PublicNumber.String()),
				slog.Any("error", err))
		}
	}
}

func (w *ReaperWorker) purgeIdempotencyKeys(ctx context.Context) {
	deleted, err := w.idempotency.DeleteExpired(ctx, w.now())
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			w.log.ErrorContext(ctx, "не удалось удалить протухшие ключи идемпотентности",
				slog.Any("error", err))
		}
		return
	}

	if deleted > 0 {
		w.log.InfoContext(ctx, "удалены протухшие ключи идемпотентности",
			slog.Int64("count", deleted))
	}
}
