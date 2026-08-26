// Package simclient — HTTP-клиент для доставки событий в сервис заведения.
package simclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"avito-kitchen/internal/app"
	"avito-kitchen/internal/domain"
)

// EventIDHeader — идентификатор события для дедупликации на стороне заведения.
//
// Доставка гарантирует at-least-once, поэтому одно и то же событие может
// прийти дважды. Заведение обязано отличать повтор от нового события — по
// этому заголовку.
const EventIDHeader = "X-Event-ID"

// Client отправляет события заведению по HTTP.
type Client struct {
	httpClient *http.Client
}

// New создаёт клиент с ограничением на время запроса.
func New(timeout time.Duration) *Client {
	return &Client{
		httpClient: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

var _ app.EventDeliverer = (*Client)(nil)

// envelope — формат вебхука, который принимает заведение.
type envelope struct {
	EventID     int64           `json:"event_id"`
	EventType   string          `json:"event_type"`
	AggregateID string          `json:"aggregate_id"`
	OccurredAt  time.Time       `json:"occurred_at"`
	Payload     json.RawMessage `json:"payload"`
}

// Deliver отправляет событие на POST {baseURL}/kitchen/events.
//
// Успехом считается любой 2xx. Ответ 4xx означает, что заведение считает
// событие некорректным — повторять его бессмысленно, поэтому ошибка помечается
// так, чтобы воркер не крутил бесконечные попытки.
func (c *Client) Deliver(ctx context.Context, baseURL string, event app.DeliveryEvent) error {
	body, err := json.Marshal(envelope{
		EventID:     event.EventID,
		EventType:   event.EventType,
		AggregateID: event.AggregateID,
		OccurredAt:  event.CreatedAt,
		Payload:     event.Payload,
	})
	if err != nil {
		return domain.WrapErrorf(err, domain.CodeInternalError, "сериализация события")
	}

	endpoint := strings.TrimRight(baseURL, "/") + "/kitchen/events"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return domain.WrapErrorf(err, domain.CodeInternalError, "сборка запроса к заведению")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(EventIDHeader, strconv.FormatInt(event.EventID, 10))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return domain.WrapErrorf(err, domain.CodeServiceUnavailable,
			"заведение недоступно по адресу %s", endpoint)
	}
	defer func() {
		// Тело нужно дочитать и закрыть, иначе соединение не вернётся в пул.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	return fmt.Errorf("заведение ответило %d на %s", resp.StatusCode, endpoint)
}
