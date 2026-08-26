package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// platformClient — клиент B2B API платформы со стороны заведения.
//
// Это вторая половина интеграции: платформа шлёт заведению вебхуки, заведение
// ходит в платформу за управлением заказами и меню.
type platformClient struct {
	baseURL string
	token   string
	http    *http.Client
}

func newPlatformClient(baseURL, token string, timeout time.Duration) *platformClient {
	return &platformClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: timeout},
	}
}

// apiError — ошибка платформы с сохранённым HTTP-статусом и доменным кодом:
// по ним сим решает, стоит ли повторять операцию.
type apiError struct {
	Status int
	Code   string
	Detail string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("платформа ответила %d (%s): %s", e.Status, e.Code, e.Detail)
}

// syncMenuResponse — ответ на синхронизацию меню.
type syncMenuResponse struct {
	Version     int32 `json:"version"`
	ItemsSynced int32 `json:"items_synced"`
}

// SyncMenu публикует эталонное меню заведения на платформе.
func (c *platformClient) SyncMenu(ctx context.Context, products []menuProduct) (syncMenuResponse, error) {
	var result syncMenuResponse
	err := c.do(ctx, http.MethodPost, "/api/v1/partner/menu/sync",
		map[string]any{"products": products}, &result)
	return result, err
}

// SetOrderStatus переводит заказ в новый статус.
func (c *platformClient) SetOrderStatus(ctx context.Context, publicNumber, status, comment string) error {
	return c.do(ctx, http.MethodPost,
		"/api/v1/partner/orders/"+publicNumber+"/status",
		map[string]any{statusField: status, "comment": comment}, nil)
}

// SetKitchenStatus переключает режим работы кухни.
func (c *platformClient) SetKitchenStatus(ctx context.Context, status string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/partner/kitchen-status",
		map[string]any{statusField: status}, nil)
}

// Health проверяет, что платформа поднялась. Используется при старте, пока
// контейнер api ещё инициализируется.
func (c *platformClient) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/readyz", http.NoBody)
	if err != nil {
		return err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer drainAndClose(resp)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("платформа не готова: %d", resp.StatusCode)
	}
	return nil
}

func (c *platformClient) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("сериализация тела запроса: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("сборка запроса: %w", err)
	}
	req.Header.Set("X-Partner-Token", c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("запрос к платформе %s: %w", path, err)
	}
	defer drainAndClose(resp)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if out == nil {
			return nil
		}
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("разбор ответа платформы: %w", err)
		}
		return nil
	}

	return parseAPIError(resp)
}

func parseAPIError(resp *http.Response) error {
	var problem struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	}

	// Тело может не быть problem+json (например, ответил прокси) — тогда
	// довольствуемся статусом.
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	_ = json.Unmarshal(raw, &problem)

	return &apiError{Status: resp.StatusCode, Code: problem.Code, Detail: problem.Detail}
}

func drainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
	_ = resp.Body.Close()
}
