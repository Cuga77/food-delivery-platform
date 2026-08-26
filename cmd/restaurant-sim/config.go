package main

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

// simConfig — конфигурация эмулятора кухни.
type simConfig struct {
	Port     int
	LogLevel string

	// APIBaseURL — адрес платформы, куда сим ходит по B2B API.
	APIBaseURL string
	// PartnerToken — токен заведения; хэш этого значения лежит в БД платформы.
	PartnerToken string
	// RestaurantSlug используется только в логах для наглядности.
	RestaurantSlug string

	// AutoAccept включает автоматический конвейер ACCEPTED → COOKING → READY.
	AutoAccept bool
	// RejectAll — аварийный режим: отклонять все входящие заказы.
	// Имеет приоритет над AutoAccept.
	RejectAll bool
	// SyncMenuOnStart — синхронизировать эталонное меню при запуске.
	SyncMenuOnStart bool

	AcceptDelay time.Duration
	CookingTime time.Duration
	ReadyDelay  time.Duration

	HTTPTimeout    time.Duration
	StartupTimeout time.Duration
}

func loadConfig() (simConfig, error) {
	var errs []error

	cfg := simConfig{
		Port:            envInt("SIM_PORT", 8081, &errs),
		LogLevel:        envString("LOG_LEVEL", "info"),
		APIBaseURL:      envString("SIM_API_BASE_URL", "http://api:8080"),
		PartnerToken:    envString("SIM_PARTNER_TOKEN", ""),
		RestaurantSlug:  envString("SIM_RESTAURANT_SLUG", "pizza-avito"),
		AutoAccept:      envBool("SIM_AUTO_ACCEPT", true, &errs),
		RejectAll:       envBool("SIM_REJECT_ALL", false, &errs),
		SyncMenuOnStart: envBool("SIM_SYNC_MENU_ON_START", true, &errs),
		AcceptDelay:     envMillis("SIM_ACCEPT_DELAY_MS", time.Second, &errs),
		CookingTime:     envMillis("SIM_COOKING_TIME_MS", 3*time.Second, &errs),
		ReadyDelay:      envMillis("SIM_READY_DELAY_MS", 2*time.Second, &errs),
		HTTPTimeout:     envDuration("SIM_HTTP_TIMEOUT", 5*time.Second, &errs),
		StartupTimeout:  envDuration("SIM_STARTUP_TIMEOUT", 60*time.Second, &errs),
	}

	if cfg.PartnerToken == "" {
		errs = append(errs, errors.New("SIM_PARTNER_TOKEN обязателен"))
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		errs = append(errs, fmt.Errorf("SIM_PORT вне диапазона 1..65535: %d", cfg.Port))
	}

	if len(errs) > 0 {
		return simConfig{}, errors.Join(errs...)
	}
	return cfg, nil
}

func envString(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int, errs *[]error) int {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: ожидалось целое число, получено %q", key, raw))
		return fallback
	}
	return v
}

func envBool(key string, fallback bool, errs *[]error) bool {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: ожидалось true/false, получено %q", key, raw))
		return fallback
	}
	return v
}

// envMillis читает значение в миллисекундах: так параметры таймингов заданы в
// задании (SIM_ACCEPT_DELAY_MS и подобные).
func envMillis(key string, fallback time.Duration, errs *[]error) time.Duration {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms < 0 {
		*errs = append(*errs, fmt.Errorf("%s: ожидалось неотрицательное число миллисекунд, получено %q", key, raw))
		return fallback
	}
	return time.Duration(ms) * time.Millisecond
}

func envDuration(key string, fallback time.Duration, errs *[]error) time.Duration {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return fallback
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s: ожидалась длительность вида 5s, получено %q", key, raw))
		return fallback
	}
	return v
}
