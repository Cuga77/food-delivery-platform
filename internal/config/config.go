// Package config читает конфигурацию из переменных окружения и проверяет её
// на старте. Приложение либо стартует с полностью валидной конфигурацией,
// либо не стартует вовсе — «наполовину настроенного» состояния не бывает.
package config

import (
	"errors"
	"fmt"
	"math"
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
	Web     WebConfig
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

// WebConfig — параметры серверного веб-клиента.
type WebConfig struct {
	// SecureCookies помечает cookie сессии заведения флагом Secure.
	// Включать за TLS; на локальном HTTP браузер такую cookie не сохранит.
	SecureCookies bool
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
			MaxConns:       getInt32("DB_MAX_CONNS", 10, &errs),
			MinConns:       getInt32("DB_MIN_CONNS", 2, &errs),
			ConnectTimeout: getDuration("DB_CONNECT_TIMEOUT", 10*time.Second, &errs),
		},
		Outbox: OutboxConfig{
			PollInterval: getDuration("OUTBOX_POLL_INTERVAL", 500*time.Millisecond, &errs),
			BatchSize:    getInt32("OUTBOX_BATCH_SIZE", 50, &errs),
			MaxAttempts:  getInt32("OUTBOX_MAX_ATTEMPTS", 10, &errs),
			BaseBackoff:  getDuration("OUTBOX_BASE_BACKOFF", time.Second, &errs),
			MaxBackoff:   getDuration("OUTBOX_MAX_BACKOFF", 5*time.Minute, &errs),
			Lease:        getDuration("OUTBOX_LEASE", 30*time.Second, &errs),
			HTTPTimeout:  getDuration("OUTBOX_HTTP_TIMEOUT", 5*time.Second, &errs),
		},
		Reaper: ReaperConfig{
			Interval:           getDuration("REAPER_INTERVAL", 30*time.Second, &errs),
			OrderAcceptTimeout: getDuration("ORDER_ACCEPT_TIMEOUT", 5*time.Minute, &errs),
			BatchSize:          getInt32("REAPER_BATCH_SIZE", 100, &errs),
		},
		Idempot: IdempotencyConfig{
			TTL: getDuration("IDEMPOTENCY_TTL", 24*time.Hour, &errs),
		},
		Web: WebConfig{
			SecureCookies: getBool("WEB_SECURE_COOKIES", false, &errs),
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
	if c.Reaper.BatchSize < 1 {
		errs = append(errs, errors.New("REAPER_BATCH_SIZE должен быть не меньше 1"))
	}

	// Каждая длительность обязана быть положительной, и это не формальность:
	// time.NewTicker паникует на неположительном интервале, а таймаут в ноль
	// означает мгновенную отмену любого запроса. Раньше такие значения
	// проходили проверку, сервис поднимался, подключался к БД, открывал порт —
	// и падал уже в горутине воркера.
	positive := []struct {
		name  string
		value time.Duration
	}{
		{"HTTP_READ_TIMEOUT", c.HTTP.ReadTimeout},
		{"HTTP_WRITE_TIMEOUT", c.HTTP.WriteTimeout},
		{"HTTP_IDLE_TIMEOUT", c.HTTP.IdleTimeout},
		{"HTTP_REQUEST_TIMEOUT", c.HTTP.RequestTimeout},
		{"HTTP_SHUTDOWN_TIMEOUT", c.HTTP.ShutdownTimeout},
		{"DB_CONNECT_TIMEOUT", c.DB.ConnectTimeout},
		{"OUTBOX_POLL_INTERVAL", c.Outbox.PollInterval},
		{"OUTBOX_BASE_BACKOFF", c.Outbox.BaseBackoff},
		{"OUTBOX_MAX_BACKOFF", c.Outbox.MaxBackoff},
		{"OUTBOX_LEASE", c.Outbox.Lease},
		{"OUTBOX_HTTP_TIMEOUT", c.Outbox.HTTPTimeout},
		{"REAPER_INTERVAL", c.Reaper.Interval},
		{"ORDER_ACCEPT_TIMEOUT", c.Reaper.OrderAcceptTimeout},
		{"IDEMPOTENCY_TTL", c.Idempot.TTL},
	}
	for _, d := range positive {
		if d.value <= 0 {
			errs = append(errs, fmt.Errorf("%s должен быть положительным, получено %s", d.name, d.value))
		}
	}

	// Потолок повторов ниже стартовой задержки означает, что рост отключён и
	// backoff вырождается в постоянный интервал — почти наверняка опечатка.
	if c.Outbox.MaxBackoff > 0 && c.Outbox.BaseBackoff > c.Outbox.MaxBackoff {
		errs = append(errs, fmt.Errorf(
			"OUTBOX_BASE_BACKOFF (%s) не может превышать OUTBOX_MAX_BACKOFF (%s)",
			c.Outbox.BaseBackoff, c.Outbox.MaxBackoff))
	}

	// Сервис не успеет корректно погасить соединения, если на остановку
	// отведено меньше, чем на один запрос.
	if c.HTTP.ShutdownTimeout > 0 && c.HTTP.RequestTimeout > c.HTTP.ShutdownTimeout {
		errs = append(errs, fmt.Errorf(
			"HTTP_REQUEST_TIMEOUT (%s) не может превышать HTTP_SHUTDOWN_TIMEOUT (%s)",
			c.HTTP.RequestTimeout, c.HTTP.ShutdownTimeout))
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

// getInt32 читает значение и проверяет, что оно помещается в int32.
// Без проверки конфигурация вроде DB_MAX_CONNS=5000000000 молча превратилась
// бы в отрицательное число.
func getInt32(key string, fallback int32, errs *[]error) int32 {
	value := getInt(key, int(fallback), errs)
	if value < math.MinInt32 || value > math.MaxInt32 {
		*errs = append(*errs, fmt.Errorf("%s: значение %d не помещается в int32", key, value))
		return fallback
	}
	return int32(value)
}

func getBool(key string, fallback bool, errs *[]error) bool {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback
	}

	value, err := strconv.ParseBool(raw)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: ожидалось true/false, получено %q", key, raw))
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
