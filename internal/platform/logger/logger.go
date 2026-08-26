// Package logger настраивает структурированное логирование.
package logger

import (
	"context"
	"io"
	"log/slog"
	"strings"
)

// requestIDKey — ключ, под которым идентификатор запроса кладётся в контекст.
type requestIDKey struct{}

// WithRequestID кладёт идентификатор запроса в контекст.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFromContext достаёт идентификатор запроса; пустая строка, если его нет.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// New создаёт JSON-логгер.
//
// JSON, а не текст: логи сервиса читает не человек в терминале, а система
// сбора, и структурированные поля там ищутся и агрегируются, а строка — нет.
func New(out io.Writer, level string) *slog.Logger {
	handler := slog.NewJSONHandler(out, &slog.HandlerOptions{
		Level: parseLevel(level),
	})

	return slog.New(&contextHandler{Handler: handler})
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// contextHandler подмешивает request_id в каждую запись, у которой есть
// контекст. Благодаря этому не нужно вручную протаскивать логгер с полем через
// все слои: достаточно вызывать методы вида InfoContext.
type contextHandler struct {
	slog.Handler
}

func (h *contextHandler) Handle(ctx context.Context, record slog.Record) error {
	if id := RequestIDFromContext(ctx); id != "" {
		record.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, record)
}

func (h *contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h *contextHandler) WithGroup(name string) slog.Handler {
	return &contextHandler{Handler: h.Handler.WithGroup(name)}
}
