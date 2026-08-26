package http

import (
	"context"

	"avito-kitchen/internal/domain"
	"avito-kitchen/internal/gen"
)

// Pinger — минимальная зависимость проверки готовности: умение достучаться до БД.
type Pinger interface {
	Ping(ctx context.Context) error
}

// HealthHandler отвечает на проверки живости и готовности.
//
// Проверки намеренно разные: /health говорит «процесс жив» и нужен, чтобы
// оркестратор не перезапускал рабочий контейнер из-за недоступной БД;
// /readyz говорит «могу обслуживать запросы» и снимает под с балансировки,
// пока зависимости не поднялись.
type HealthHandler struct {
	db      Pinger
	version string
}

// NewHealthHandler собирает обработчик проверок.
func NewHealthHandler(db Pinger, version string) *HealthHandler {
	return &HealthHandler{db: db, version: version}
}

// GetHealth — GET /health.
func (h *HealthHandler) GetHealth(
	_ context.Context,
	_ gen.GetHealthRequestObject,
) (gen.GetHealthResponseObject, error) {
	version := h.version
	return gen.GetHealth200JSONResponse{Status: "ok", Version: &version}, nil
}

// GetReadiness — GET /readyz.
func (h *HealthHandler) GetReadiness(
	ctx context.Context,
	_ gen.GetReadinessRequestObject,
) (gen.GetReadinessResponseObject, error) {
	if err := h.db.Ping(ctx); err != nil {
		return nil, domain.WrapErrorf(err, domain.CodeServiceUnavailable,
			"база данных недоступна")
	}

	version := h.version
	return gen.GetReadiness200JSONResponse{Status: "ready", Version: &version}, nil
}
