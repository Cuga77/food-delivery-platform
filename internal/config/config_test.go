package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"avito-kitchen/internal/config"
)

// Конфигурация — единственное место, где значения приходят снаружи без всякой
// типизации. Проверка её ветвей дешева, а цена пропуска высока: значение
// вроде REAPER_INTERVAL=0s раньше проходило валидацию, сервис поднимался,
// подключался к БД, открывал порт — и падал паникой в горутине воркера.

const validDSN = "postgres://u:p@localhost:5432/db?sslmode=disable"

// withEnv выставляет переменные на время теста; t.Setenv сам вернёт прежние.
func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	t.Setenv("DATABASE_URL", validDSN)
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestLoad_Defaults(t *testing.T) {
	withEnv(t, nil)

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, 8080, cfg.HTTP.Port)
	assert.Equal(t, validDSN, cfg.DB.DSN)
	assert.Equal(t, int32(10), cfg.DB.MaxConns)
	assert.Equal(t, 500*time.Millisecond, cfg.Outbox.PollInterval)
	assert.Equal(t, 5*time.Minute, cfg.Reaper.OrderAcceptTimeout)
	assert.Equal(t, 24*time.Hour, cfg.Idempot.TTL)
	assert.False(t, cfg.Web.SecureCookies)
	assert.Equal(t, []string{"*"}, cfg.HTTP.CORSOrigins)
}

func TestLoad_RequiresDSN(t *testing.T) {
	t.Setenv("DATABASE_URL", "")

	_, err := config.Load()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "DATABASE_URL")
}

// Ноль и отрицательные длительности обязаны отсекаться на старте: time.NewTicker
// паникует на неположительном интервале, а нулевой таймаут отменяет любой запрос.
func TestLoad_RejectsNonPositiveDurations(t *testing.T) {
	durations := []string{
		"HTTP_READ_TIMEOUT",
		"HTTP_WRITE_TIMEOUT",
		"HTTP_IDLE_TIMEOUT",
		"HTTP_REQUEST_TIMEOUT",
		"HTTP_SHUTDOWN_TIMEOUT",
		"DB_CONNECT_TIMEOUT",
		"OUTBOX_POLL_INTERVAL",
		"OUTBOX_BASE_BACKOFF",
		"OUTBOX_MAX_BACKOFF",
		"OUTBOX_LEASE",
		"OUTBOX_HTTP_TIMEOUT",
		"REAPER_INTERVAL",
		"ORDER_ACCEPT_TIMEOUT",
		"IDEMPOTENCY_TTL",
	}

	for _, name := range durations {
		for _, bad := range []string{"0s", "-1s"} {
			t.Run(name+"="+bad, func(t *testing.T) {
				withEnv(t, map[string]string{name: bad})

				_, err := config.Load()

				require.Error(t, err, "значение %s=%s должно быть отвергнуто", name, bad)
				assert.Contains(t, err.Error(), name)
			})
		}
	}
}

func TestLoad_RejectsMalformedValues(t *testing.T) {
	tests := []struct {
		name  string
		env   map[string]string
		wants string
	}{
		{"порт не число", map[string]string{"API_PORT": "восемь"}, "API_PORT"},
		{"порт вне диапазона", map[string]string{"API_PORT": "70000"}, "API_PORT"},
		{"длительность не разбирается", map[string]string{"REAPER_INTERVAL": "быстро"}, "REAPER_INTERVAL"},
		{"булево не разбирается", map[string]string{"WEB_SECURE_COOKIES": "ага"}, "WEB_SECURE_COOKIES"},
		{"пул меньше единицы", map[string]string{"DB_MAX_CONNS": "0"}, "DB_MAX_CONNS"},
		{"минимум больше максимума", map[string]string{"DB_MIN_CONNS": "50", "DB_MAX_CONNS": "10"}, "DB_MIN_CONNS"},
		{"размер пачки outbox", map[string]string{"OUTBOX_BATCH_SIZE": "0"}, "OUTBOX_BATCH_SIZE"},
		{"попытки outbox", map[string]string{"OUTBOX_MAX_ATTEMPTS": "0"}, "OUTBOX_MAX_ATTEMPTS"},
		{"пачка reaper", map[string]string{"REAPER_BATCH_SIZE": "0"}, "REAPER_BATCH_SIZE"},
		{"переполнение int32", map[string]string{"DB_MAX_CONNS": "5000000000"}, "DB_MAX_CONNS"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withEnv(t, tt.env)

			_, err := config.Load()

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wants)
		})
	}
}

// Аренда события обязана переживать HTTP-запрос к заведению: иначе соседний
// воркер заберёт событие, пока текущий ещё ждёт ответа, и оно уйдёт дважды.
func TestLoad_LeaseMustOutliveHTTPTimeout(t *testing.T) {
	withEnv(t, map[string]string{"OUTBOX_LEASE": "5s", "OUTBOX_HTTP_TIMEOUT": "5s"})

	_, err := config.Load()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "OUTBOX_LEASE")
}

func TestLoad_BaseBackoffMustNotExceedMax(t *testing.T) {
	withEnv(t, map[string]string{"OUTBOX_BASE_BACKOFF": "10m", "OUTBOX_MAX_BACKOFF": "1m"})

	_, err := config.Load()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "OUTBOX_BASE_BACKOFF")
}

func TestLoad_RequestTimeoutMustFitShutdown(t *testing.T) {
	withEnv(t, map[string]string{"HTTP_REQUEST_TIMEOUT": "60s", "HTTP_SHUTDOWN_TIMEOUT": "10s"})

	_, err := config.Load()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP_REQUEST_TIMEOUT")
}

// Все ошибки собираются разом: за один запуск видно всё, что нужно поправить,
// а не первую попавшуюся строчку.
func TestLoad_ReportsAllProblemsAtOnce(t *testing.T) {
	withEnv(t, map[string]string{
		"REAPER_INTERVAL":      "0s",
		"OUTBOX_POLL_INTERVAL": "-1s",
		"DB_MAX_CONNS":         "0",
	})

	_, err := config.Load()

	require.Error(t, err)
	for _, want := range []string{"REAPER_INTERVAL", "OUTBOX_POLL_INTERVAL", "DB_MAX_CONNS"} {
		assert.Contains(t, err.Error(), want)
	}
	assert.GreaterOrEqual(t, strings.Count(err.Error(), "\n"), 2, "ошибки перечислены построчно")
}

func TestLoad_CORSOriginsList(t *testing.T) {
	withEnv(t, map[string]string{"CORS_ALLOWED_ORIGINS": " https://a.example , https://b.example ,, "})

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, []string{"https://a.example", "https://b.example"}, cfg.HTTP.CORSOrigins)
}
