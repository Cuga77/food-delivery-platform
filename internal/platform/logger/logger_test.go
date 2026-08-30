package logger_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"avito-kitchen/internal/platform/logger"
)

// Смысл этого пакета — сквозной request_id: клиент получает его в заголовке
// ответа, и по этому же значению запрос находится в логах одной командой.
// Если идентификатор не подмешивается, связь ответа с логом теряется, а
// вместе с ней и вся отладка продакшена.

// parse разбирает последнюю строку JSON-лога.
func parse(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.NotEmpty(t, lines[len(lines)-1], "лог пуст")

	var record map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[len(lines)-1]), &record))

	return record
}

func TestRequestID_RoundTrip(t *testing.T) {
	t.Parallel()

	ctx := logger.WithRequestID(context.Background(), "req-123")

	assert.Equal(t, "req-123", logger.RequestIDFromContext(ctx))
	assert.Empty(t, logger.RequestIDFromContext(context.Background()),
		"без идентификатора — пустая строка, а не паника")
}

func TestNew_AddsRequestIDFromContext(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log := logger.New(&buf, "info")

	log.InfoContext(logger.WithRequestID(context.Background(), "req-abc"), "заказ создан")

	record := parse(t, &buf)
	assert.Equal(t, "req-abc", record["request_id"])
	assert.Equal(t, "заказ создан", record["msg"])
}

func TestNew_OmitsRequestIDWhenAbsent(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log := logger.New(&buf, "info")

	log.InfoContext(context.Background(), "фоновая задача")

	record := parse(t, &buf)
	_, present := record["request_id"]
	assert.False(t, present, "пустое поле в каждой строке фонового воркера — мусор")
}

// Подмешивание обязано переживать With и WithGroup: воркеры навешивают на
// логгер свои постоянные поля, и после этого request_id теряться не должен.
func TestNew_SurvivesWithAndGroup(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	ctx := logger.WithRequestID(context.Background(), "req-xyz")

	withAttrs := logger.New(&buf, "info").With(slog.String("worker", "outbox"))
	withAttrs.InfoContext(ctx, "событие доставлено")

	record := parse(t, &buf)
	assert.Equal(t, "req-xyz", record["request_id"])
	assert.Equal(t, "outbox", record["worker"])
}

// Известное ограничение, зафиксированное намеренно: при открытой группе
// идентификатор попадает внутрь неё. Группы применяет вложенный обработчик, а
// поле добавляется к записи; вынести его наружу можно только заново
// реализовав семантику групп slog. Проект WithGroup не использует, поэтому
// поведение зафиксировано как есть — тест упадёт, если оно молча изменится.
func TestNew_RequestIDLandsInsideOpenGroup(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	ctx := logger.WithRequestID(context.Background(), "req-xyz")

	logger.New(&buf, "info").WithGroup("delivery").InfoContext(ctx, "попытка")

	record := parse(t, &buf)

	_, atTopLevel := record["request_id"]
	assert.False(t, atTopLevel, "на верхнем уровне его нет — это и есть ограничение")

	group, ok := record["delivery"].(map[string]any)
	require.True(t, ok, "группа присутствует в записи")
	assert.Equal(t, "req-xyz", group["request_id"], "идентификатор не потерян, но вложен")
}

func TestNew_ParsesLevels(t *testing.T) {
	t.Parallel()

	tests := []struct {
		level        string
		debugVisible bool
		infoVisible  bool
	}{
		{"debug", true, true},
		{"info", false, true},
		{"warn", false, false},
		{"warning", false, false},
		{"error", false, false},
		{"ERROR", false, false},
		{"  Info  ", false, true},
		{"чепуха", false, true}, // непонятное значение не должно глушить сервис
		{"", false, true},
	}

	for _, tt := range tests {
		t.Run("уровень="+tt.level, func(t *testing.T) {
			t.Parallel()

			var buf bytes.Buffer
			log := logger.New(&buf, tt.level)

			log.DebugContext(context.Background(), "отладка")
			assert.Equalf(t, tt.debugVisible, strings.Contains(buf.String(), "отладка"),
				"видимость debug при уровне %q", tt.level)

			buf.Reset()
			log.InfoContext(context.Background(), "информация")
			assert.Equalf(t, tt.infoVisible, strings.Contains(buf.String(), "информация"),
				"видимость info при уровне %q", tt.level)

			// Ошибки видны всегда: уровень не может их спрятать.
			buf.Reset()
			log.ErrorContext(context.Background(), "сбой")
			assert.Contains(t, buf.String(), "сбой")
		})
	}
}

// Формат — JSON: логи читает система сбора, а не человек в терминале.
func TestNew_WritesJSON(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger.New(&buf, "info").InfoContext(context.Background(), "проверка",
		slog.Int("orders", 3))

	record := parse(t, &buf)
	// JSON разбирается в float64; сравниваем через EqualValues, чтобы не
	// сравнивать вещественные числа на точное равенство.
	assert.EqualValues(t, 3, record["orders"])
	assert.Contains(t, record, "time")
	assert.Equal(t, "INFO", record["level"])
}
