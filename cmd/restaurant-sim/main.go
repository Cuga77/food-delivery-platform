// Команда restaurant-sim — сервис заведения, подключённого к Авито.Кухня.
//
// Он играет роль реального партнёра: держит собственное меню, принимает от
// платформы вебхуки о заказах и управляет их статусами через партнёрское API.
// Именно на нём проверяется, что интеграция двусторонняя и работает целиком.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"

	"avito-kitchen/internal/platform/httpserver"
	"avito-kitchen/internal/platform/logger"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "restaurant-sim остановлен с ошибкой: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("конфигурация: %w", err)
	}

	log := logger.New(os.Stdout, cfg.LogLevel)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.InfoContext(ctx, "запуск эмулятора кухни",
		slog.String("restaurant", cfg.RestaurantSlug),
		slog.String("api", cfg.APIBaseURL),
		slog.Bool("auto_accept", cfg.AutoAccept),
		slog.Bool("reject_all", cfg.RejectAll))

	client := newPlatformClient(cfg.APIBaseURL, cfg.PartnerToken, cfg.HTTPTimeout)
	kitchen := newKitchen(cfg, client, log)

	srv := httpserver.New(newRouter(kitchen, log), httpserver.Config{
		Port:            cfg.Port,
		ReadTimeout:     10 * time.Second,
		WriteTimeout:    15 * time.Second,
		IdleTimeout:     60 * time.Second,
		ShutdownTimeout: 10 * time.Second,
	}, log)

	// Синхронизация меню выполняется параллельно с подъёмом сервера: платформа
	// может подниматься дольше, и это не повод задерживать приём вебхуков.
	if cfg.SyncMenuOnStart {
		go syncMenuWhenReady(ctx, cfg, client, log)
	}

	err = srv.Run(ctx)

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	kitchen.Shutdown(shutdownCtx)

	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}

	log.InfoContext(context.WithoutCancel(ctx), "эмулятор кухни остановлен")
	return nil
}

// newRouter собирает HTTP-API заведения.
func newRouter(kitchen *kitchen, log *slog.Logger) http.Handler {
	r := chi.NewRouter()
	r.Use(chimw.RequestID, chimw.RealIP, chimw.Recoverer)

	r.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Готовность заведения не зависит от платформы: сим обязан принимать
	// вебхуки, даже если сам сейчас не может достучаться до API.
	r.Get("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})

	// Эталонное меню заведения: то, что оно синхронизирует в платформу.
	r.Get("/kitchen/menu", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"products": referenceMenu()})
	})

	r.Post("/kitchen/events", func(w http.ResponseWriter, r *http.Request) {
		var event eventEnvelope
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&event); err != nil {
			log.WarnContext(r.Context(), "некорректный вебхук", slog.Any("error", err))
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "некорректное тело события"})
			return
		}

		// Идентификатор события дублируется заголовком: заголовок надёжнее,
		// потому что не зависит от формата тела.
		if header := r.Header.Get("X-Event-ID"); header != "" {
			if id, err := strconv.ParseInt(header, 10, 64); err == nil {
				event.EventID = id
			}
		}

		// Событие принимается сразу, а обрабатывается асинхронно: платформа
		// не должна ждать, пока кухня «подумает». Долгий ответ здесь означал бы
		// исчерпание аренды в outbox и повторную доставку.
		kitchen.HandleEvent(r.Context(), event)

		w.WriteHeader(http.StatusNoContent)
	})

	return r
}

// syncMenuWhenReady дожидается готовности платформы и публикует меню.
//
// Повторы нужны потому, что порядок старта контейнеров не гарантирует, что
// api уже принимает запросы, когда sim поднялся.
func syncMenuWhenReady(ctx context.Context, cfg simConfig, client *platformClient, log *slog.Logger) {
	deadline := time.Now().Add(cfg.StartupTimeout)
	attempt := 0

	for {
		if ctx.Err() != nil {
			return
		}

		attempt++
		if err := client.Health(ctx); err == nil {
			result, err := client.SyncMenu(ctx, referenceMenu())
			if err == nil {
				log.InfoContext(ctx, "меню синхронизировано с платформой",
					slog.Int("version", int(result.Version)),
					slog.Int("items", int(result.ItemsSynced)))
				return
			}
			log.WarnContext(ctx, "не удалось синхронизировать меню",
				slog.Int("attempt", attempt), slog.Any("error", err))
		}

		if time.Now().After(deadline) {
			log.ErrorContext(ctx, "платформа не поднялась за отведённое время — меню не синхронизировано",
				slog.Duration("timeout", cfg.StartupTimeout))
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
