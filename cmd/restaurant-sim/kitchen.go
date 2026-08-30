package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Типы событий, которые заведение принимает от платформы.
const (
	eventOrderCreated   = "order_created"
	eventOrderCancelled = "order_cancelled"
)

// Статусы заказа со стороны заведения.
const (
	statusAccepted = "ACCEPTED"
	statusRejected = "REJECTED"
	statusCooking  = "COOKING"
	statusReady    = "READY"
)

// eventEnvelope — формат вебхука, который присылает платформа.
type eventEnvelope struct {
	EventID     int64           `json:"event_id"`
	EventType   string          `json:"event_type"`
	AggregateID string          `json:"aggregate_id"`
	OccurredAt  time.Time       `json:"occurred_at"`
	Payload     json.RawMessage `json:"payload"`
}

// orderCreatedPayload — то, что заведению нужно знать о новом заказе.
type orderCreatedPayload struct {
	PublicNumber    string `json:"public_number"`
	DeliveryAddress string `json:"delivery_address"`
	TotalKopecks    int64  `json:"total_kopecks"`
	Items           []struct {
		ProductKey string `json:"product_key"`
		Name       string `json:"name"`
		Qty        int32  `json:"qty"`
	} `json:"items"`
}

// kitchen — «мозг» заведения: принимает события и ведёт заказы по конвейеру.
type kitchen struct {
	cfg    simConfig
	client *platformClient
	log    *slog.Logger

	dedup *dedupCache

	// Конвейеры выполняются в отдельных горутинах; здесь хранятся их отмены,
	// чтобы событие об отмене заказа могло остановить готовку.
	mu        sync.Mutex
	pipelines map[string]context.CancelFunc
	wg        sync.WaitGroup
}

func newKitchen(cfg simConfig, client *platformClient, log *slog.Logger) *kitchen {
	return &kitchen{
		cfg:       cfg,
		client:    client,
		log:       log,
		dedup:     newDedupCache(dedupCapacity),
		pipelines: make(map[string]context.CancelFunc),
	}
}

// HandleEvent обрабатывает вебхук платформы.
//
// Доставка гарантирует at-least-once, поэтому первое, что делает заведение, —
// проверяет, не видело ли оно это событие раньше. Повтор подтверждается как
// успех, но повторной работы не вызывает: иначе один заказ мог бы дважды уйти
// на кухню.
func (k *kitchen) HandleEvent(ctx context.Context, event eventEnvelope) {
	log := k.log.With(
		slog.Int64("event_id", event.EventID),
		slog.String("event_type", event.EventType),
		slog.String("order", event.AggregateID))

	if !k.dedup.Add(event.EventID) {
		log.InfoContext(ctx, "повторное событие пропущено")
		return
	}

	switch event.EventType {
	case eventOrderCreated:
		k.handleOrderCreated(ctx, event, log)
	case eventOrderCancelled:
		k.handleOrderCancelled(event.AggregateID, log)
	default:
		log.WarnContext(ctx, "неизвестный тип события — проигнорирован")
	}
}

// Конвейер запускается на собственном контексте, а не на контексте запроса:
// платформа не должна ждать, пока кухня доготовит заказ, а обрыв соединения
// не должен обрывать готовку.
//
//nolint:contextcheck // жизненный цикл конвейера намеренно длиннее запроса
func (k *kitchen) handleOrderCreated(ctx context.Context, event eventEnvelope, log *slog.Logger) {
	var payload orderCreatedPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		log.ErrorContext(ctx, "не удалось разобрать payload заказа", slog.Any("error", err))
		return
	}

	log.InfoContext(ctx, "получен новый заказ",
		slog.String("address", payload.DeliveryAddress),
		slog.Int("positions", len(payload.Items)),
		slog.Int64("total_kopecks", payload.TotalKopecks))

	stages := pipelineFor(k.cfg)
	if len(stages) == 0 {
		// Ручной режим: заказ ждёт решения оператора через партнёрское API.
		log.InfoContext(ctx, "автоприёмка выключена — заказ ожидает решения оператора")
		return
	}

	k.startPipeline(payload.PublicNumber, stages)
}

// pipelineFor строит последовательность переходов по режиму работы кухни.
//
// Вынесено отдельной функцией, потому что порядок статусов обязан совпадать с
// разрешёнными переходами конечного автомата платформы: NEW → ACCEPTED →
// COOKING → READY. Ошибка здесь означала бы, что сим получает STATE_CONFLICT
// на каждом заказе. Пустой результат — ручной режим.
func pipelineFor(cfg simConfig) []stage {
	switch {
	case cfg.RejectAll:
		// Аварийный режим: отказываемся немедленно, чтобы платформа вернула
		// остатки и не держала заказ.
		return []stage{
			{delay: 0, status: statusRejected, comment: "кухня не принимает заказы"},
		}

	case cfg.AutoAccept:
		return []stage{
			{delay: cfg.AcceptDelay, status: statusAccepted, comment: "заказ принят"},
			{delay: cfg.CookingTime, status: statusCooking, comment: "начали готовить"},
			{delay: cfg.ReadyDelay, status: statusReady, comment: "заказ готов"},
		}

	default:
		return nil
	}
}

func (k *kitchen) handleOrderCancelled(publicNumber string, log *slog.Logger) {
	k.mu.Lock()
	cancel, running := k.pipelines[publicNumber]
	k.mu.Unlock()

	if !running {
		return
	}

	log.Info("заказ отменён — прекращаем готовку")
	cancel()
}

// stage — шаг конвейера: подождать delay и перевести заказ в status.
type stage struct {
	delay   time.Duration
	status  string
	comment string
}

func (k *kitchen) startPipeline(publicNumber string, stages []stage) {
	// Отдельный контекст с отменой: событие об отмене заказа остановит конвейер.
	ctx, cancel := context.WithCancel(context.Background())

	k.mu.Lock()
	if _, exists := k.pipelines[publicNumber]; exists {
		// Конвейер уже идёт — второй запускать нельзя.
		k.mu.Unlock()
		cancel()
		return
	}
	k.pipelines[publicNumber] = cancel
	k.mu.Unlock()

	k.wg.Add(1)
	go func() {
		defer k.wg.Done()
		defer cancel()
		defer func() {
			k.mu.Lock()
			delete(k.pipelines, publicNumber)
			k.mu.Unlock()
		}()

		k.runPipeline(ctx, publicNumber, stages)
	}()
}

func (k *kitchen) runPipeline(ctx context.Context, publicNumber string, stages []stage) {
	log := k.log.With(slog.String("order", publicNumber))

	for _, s := range stages {
		if s.delay > 0 {
			select {
			case <-ctx.Done():
				log.InfoContext(ctx, "конвейер остановлен", slog.String("на_шаге", s.status))
				return
			case <-time.After(s.delay):
			}
		}
		if ctx.Err() != nil {
			return
		}

		if err := k.client.SetOrderStatus(ctx, publicNumber, s.status, s.comment); err != nil {
			var apiErr *apiError
			if errors.As(err, &apiErr) && apiErr.Status == 409 {
				// Заказ уже отменён пользователем или платформой — это штатный
				// исход гонки, а не сбой интеграции.
				log.InfoContext(ctx, "заказ уже сменил статус — конвейер прекращён",
					slog.String("попытка", s.status),
					slog.String("code", apiErr.Code))
				return
			}
			log.ErrorContext(ctx, "не удалось перевести заказ в новый статус",
				slog.String("статус", s.status), slog.Any("error", err))
			return
		}

		log.InfoContext(ctx, "статус заказа обновлён", slog.String("статус", s.status))
	}
}

// Shutdown дожидается завершения активных конвейеров.
func (k *kitchen) Shutdown(ctx context.Context) {
	k.mu.Lock()
	for _, cancel := range k.pipelines {
		cancel()
	}
	k.mu.Unlock()

	done := make(chan struct{})
	go func() {
		k.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
		k.log.WarnContext(ctx, "часть конвейеров не завершилась до истечения таймаута остановки")
	}
}

// dedupCapacity — сколько идентификаторов событий заведение помнит.
const dedupCapacity = 1000

// dedupCache — ограниченный по размеру набор уже виденных событий.
//
// Ограничение обязательно: без него сим накапливал бы идентификаторы вечно.
// Достаточно помнить недавние — повторы приходят вскоре после оригинала.
type dedupCache struct {
	mu       sync.Mutex
	seen     map[int64]struct{}
	order    []int64
	capacity int
}

func newDedupCache(capacity int) *dedupCache {
	return &dedupCache{
		seen:     make(map[int64]struct{}, capacity),
		order:    make([]int64, 0, capacity),
		capacity: capacity,
	}
}

// Add регистрирует событие и сообщает, видим ли мы его впервые.
func (c *dedupCache) Add(id int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.seen[id]; ok {
		return false
	}

	if len(c.order) >= c.capacity {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.seen, oldest)
	}

	c.seen[id] = struct{}{}
	c.order = append(c.order, id)
	return true
}
