// Package config читает конфигурацию из переменных окружения и проверяет её
// на старте. Приложение либо стартует с полностью валидной конфигурацией,
// либо не стартует вовсе — «наполовину настроенного» состояния не бывает.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config — конфигурация сервиса api.
type Config struct {
	Env      string
	LogLevel string

	HTTP    HTTPConfig
	DB      DBConfig
	Outbox  OutboxConfig
	Reaper  ReaperConfig
	Idempot IdempotencyConfig
}

// HTTPConfig — параметры HTTP-сервера.
type HTTPConfig struct {
	Port            int
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	RequestTimeout  time.Duration
	ShutdownTimeout time.Duration
	CORSOrigins     []string
}

// DBConfig — параметры подключения к PostgreSQL.
type DBConfig struct {
	DSN            string
	MaxConns       int32
	MinConns       int32
	ConnectTimeout time.Duration
}

// OutboxConfig — параметры воркера доставки событий.
type OutboxConfig struct {
	PollInterval time.Duration
	BatchSize    int32
	MaxAttempts  int32
	BaseBackoff  time.Duration
	MaxBackoff   time.Duration
	Lease        time.Duration
	HTTPTimeout  time.Duration
}

// ReaperConfig — параметры фоновой уборки.
type ReaperConfig struct {
	Interval           time.Duration
	OrderAcceptTimeout time.Duration
	BatchSize          int32
}

// IdempotencyConfig — параметры хранения ключей идемпотентности.
type IdempotencyConfig struct {
	TTL time.Duration
}

// Load собирает конфигурацию из окружения.
//
// Ошибки не возвращаются по одной: собираются все сразу, чтобы за один запуск
// стало видно всё, что нужно поправить.
func Load() (Config, error) {
	var errs []error

	cfg := Config{
		Env:      getString("APP_ENV", "local"),
		LogLevel: getString("LOG_LEVEL", "info"),
		HTTP: HTTPConfig{
			Port:            getInt("API_PORT", 8080, &errs),
			ReadTimeout:     getDuration("HTTP_READ_TIMEOUT", 10*time.Second, &errs),
			WriteTimeout:    getDuration("HTTP_WRITE_TIMEOUT", 15*time.Second, &errs),
			IdleTimeout:     getDuration("HTTP_IDLE_TIMEOUT", 60*time.Second, &errs),
			RequestTimeout:  getDuration("HTTP_REQUEST_TIMEOUT", 10*time.Second, &errs),
			ShutdownTimeout: getDuration("HTTP_SHUTDOWN_TIMEOUT", 10*time.Second, &errs),
			CORSOrigins:     getList("CORS_ALLOWED_ORIGINS", []string{"*"}),
		},
		DB: DBConfig{
			DSN:            getString("DATABASE_URL", ""),
			MaxConns:       int32(getInt("DB_MAX_CONNS", 10, &errs)),
			MinConns:       int32(getInt("DB_MIN_CONNS", 2, &errs)),
			ConnectTimeout: getDuration("DB_CONNECT_TIMEOUT", 10*time.Second, &errs),
		},
		Outbox: OutboxConfig{
			PollInterval: getDuration("OUTBOX_POLL_INTERVAL", 500*time.Millisecond, &errs),
			BatchSize:    int32(getInt("OUTBOX_BATCH_SIZE", 50, &errs)),
			MaxAttempts:  int32(getInt("OUTBOX_MAX_ATTEMPTS", 10, &errs)),
			BaseBackoff:  getDuration("OUTBOX_BASE_BACKOFF", time.Second, &errs),
			MaxBackoff:   getDuration("OUTBOX_MAX_BACKOFF", 5*time.Minute, &errs),
			Lease:        getDuration("OUTBOX_LEASE", 30*time.Second, &errs),
			HTTPTimeout:  getDuration("OUTBOX_HTTP_TIMEOUT", 5*time.Second, &errs),
		},
		Reaper: ReaperConfig{
			Interval:           getDuration("REAPER_INTERVAL", 30*time.Second, &errs),
			OrderAcceptTimeout: getDuration("ORDER_ACCEPT_TIMEOUT", 5*time.Minute, &errs),
			BatchSize:          int32(getInt("REAPER_BATCH_SIZE", 100, &errs)),
		},
		Idempot: IdempotencyConfig{
			TTL: getDuration("IDEMPOTENCY_TTL", 24*time.Hour, &errs),
		},
	}

	errs = append(errs, cfg.validate()...)

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}

	return cfg, nil
}

func (c Config) validate() []error {
	var errs []error

	if c.DB.DSN == "" {
		errs = append(errs, errors.New("DATABASE_URL обязателен"))
	}
	if c.HTTP.Port < 1 || c.HTTP.Port > 65535 {
		errs = append(errs, fmt.Errorf("API_PORT вне диапазона 1..65535: %d", c.HTTP.Port))
	}
	if c.DB.MaxConns < 1 {
		errs = append(errs, errors.New("DB_MAX_CONNS должен быть не меньше 1"))
	}
	if c.DB.MinConns > c.DB.MaxConns {
		errs = append(errs, fmt.Errorf(
			"DB_MIN_CONNS (%d) не может превышать DB_MAX_CONNS (%d)", c.DB.MinConns, c.DB.MaxConns))
	}
	if c.Outbox.BatchSize < 1 {
		errs = append(errs, errors.New("OUTBOX_BATCH_SIZE должен быть не меньше 1"))
	}
	if c.Outbox.MaxAttempts < 1 {
		errs = append(errs, errors.New("OUTBOX_MAX_ATTEMPTS должен быть не меньше 1"))
	}
	// Аренда должна пережить HTTP-запрос: иначе событие подхватит соседний
	// воркер, пока текущий ещё ждёт ответа от заведения.
	if c.Outbox.Lease <= c.Outbox.HTTPTimeout {
		errs = append(errs, fmt.Errorf(
			"OUTBOX_LEASE (%s) должен быть больше OUTBOX_HTTP_TIMEOUT (%s)",
			c.Outbox.Lease, c.Outbox.HTTPTimeout))
	}
	if c.Reaper.OrderAcceptTimeout <= 0 {
		errs = append(errs, errors.New("ORDER_ACCEPT_TIMEOUT должен быть положительным"))
	}
	if c.Idempot.TTL <= 0 {
		errs = append(errs, errors.New("IDEMPOTENCY_TTL должен быть положительным"))
	}

	return errs
}

func getString(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func getInt(key string, fallback int, errs *[]error) int {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback
	}

	value, err := strconv.Atoi(raw)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: ожидалось целое число, получено %q", key, raw))
		return fallback
	}
	return value
}

func getDuration(key string, fallback time.Duration, errs *[]error) time.Duration {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback
	}

	value, err := time.ParseDuration(raw)
	if err != nil {
		*errs = append(*errs, fmt.Errorf(
			"%s: ожидалась длительность вида 500ms/10s/5m, получено %q", key, raw))
		return fallback
	}
	return value
}

func getList(key string, fallback []string) []string {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback
	}

	parts := strings.Split(raw, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	if len(result) == 0 {
		return fallback
	}
	return result
}
