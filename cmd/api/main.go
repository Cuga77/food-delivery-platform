// Команда api — основной сервис платформы Авито.Кухня: HTTP API для клиента
// и заведений плюс фоновые воркеры доставки событий и уборки.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sync/errgroup"

	"avito-kitchen/internal/adapters/http"
	"avito-kitchen/internal/adapters/postgres"
	"avito-kitchen/internal/adapters/simclient"
	"avito-kitchen/internal/adapters/web"
	"avito-kitchen/internal/app"
	"avito-kitchen/internal/config"
	"avito-kitchen/internal/platform/httpserver"
	"avito-kitchen/internal/platform/logger"
)

// version подставляется линкером при сборке образа (-ldflags "-X main.version=...").
var version = "dev"

func main() {
	if err := run(); err != nil {
		// Логгер к этому моменту может быть ещё не собран — пишем в stderr.
		fmt.Fprintf(os.Stderr, "сервис остановлен с ошибкой: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("конфигурация: %w", err)
	}

	log := logger.New(os.Stdout, cfg.LogLevel)
	slog.SetDefault(log)

	// Контекст отменяется по SIGINT/SIGTERM — это сигнал всем компонентам
	// начать корректную остановку.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.InfoContext(ctx, "запуск сервиса",
		slog.String("version", version),
		slog.String("env", cfg.Env))

	pool, err := postgres.NewPool(ctx, postgres.PoolConfig{
		DSN:            cfg.DB.DSN,
		MaxConns:       cfg.DB.MaxConns,
		MinConns:       cfg.DB.MinConns,
		ConnectTimeout: cfg.DB.ConnectTimeout,
	})
	if err != nil {
		return fmt.Errorf("подключение к БД: %w", err)
	}
	defer pool.Close()

	log.InfoContext(ctx, "соединение с БД установлено",
		slog.Int("max_conns", int(cfg.DB.MaxConns)))

	// --- Адаптеры ----------------------------------------------------------
	txManager := postgres.NewTxManager(pool)
	restaurantRepo := postgres.NewRestaurantRepo(pool)
	menuRepo := postgres.NewMenuRepo(pool)
	orderRepo := postgres.NewOrderRepo(pool)
	idempotencyRepo := postgres.NewIdempotencyRepo(pool)
	outboxRepo := postgres.NewOutboxRepo(pool)
	deliverer := simclient.New(cfg.Outbox.HTTPTimeout)

	// --- Use cases ---------------------------------------------------------
	catalogService := app.NewCatalogService(restaurantRepo, menuRepo)
	orderService := app.NewOrderService(txManager, restaurantRepo, menuRepo, orderRepo, outboxRepo)
	partnerService := app.NewPartnerService(txManager, restaurantRepo, menuRepo, orderRepo)

	// --- Транспорт ---------------------------------------------------------
	server := http.NewServer(
		http.NewClientHandler(catalogService, orderService),
		http.NewPartnerHandler(partnerService, orderService),
		http.NewHealthHandler(pool, version),
	)

	webHandler, err := web.New(web.Config{
		Catalog:        catalogService,
		Orders:         orderService,
		Partner:        partnerService,
		Idempotency:    idempotencyRepo,
		IdempotencyTTL: cfg.Idempot.TTL,
		SecureCookies:  cfg.Web.SecureCookies,
		Logger:         log,
	})
	if err != nil {
		return fmt.Errorf("сборка веб-слоя: %w", err)
	}

	router := http.NewRouter(http.RouterConfig{
		Server:         server,
		WebHandler:     webHandler,
		PartnerAuth:    partnerService,
		Idempotency:    idempotencyRepo,
		IdempotencyTTL: cfg.Idempot.TTL,
		RequestTimeout: cfg.HTTP.RequestTimeout,
		CORSOrigins:    cfg.HTTP.CORSOrigins,
		Logger:         log,
	})

	httpSrv := httpserver.New(router, httpserver.Config{
		Port:            cfg.HTTP.Port,
		ReadTimeout:     cfg.HTTP.ReadTimeout,
		WriteTimeout:    cfg.HTTP.WriteTimeout,
		IdleTimeout:     cfg.HTTP.IdleTimeout,
		ShutdownTimeout: cfg.HTTP.ShutdownTimeout,
	}, log)

	// --- Фоновые воркеры ---------------------------------------------------
	outboxWorker := app.NewOutboxWorker(outboxRepo, restaurantRepo, deliverer, app.OutboxConfig{
		PollInterval: cfg.Outbox.PollInterval,
		BatchSize:    cfg.Outbox.BatchSize,
		MaxAttempts:  cfg.Outbox.MaxAttempts,
		BaseBackoff:  cfg.Outbox.BaseBackoff,
		MaxBackoff:   cfg.Outbox.MaxBackoff,
		Lease:        cfg.Outbox.Lease,
	}, log)

	reaperWorker := app.NewReaperWorker(orderRepo, orderService, idempotencyRepo, app.ReaperConfig{
		Interval:           cfg.Reaper.Interval,
		OrderAcceptTimeout: cfg.Reaper.OrderAcceptTimeout,
		BatchSize:          cfg.Reaper.BatchSize,
	}, log)

	// Все три компонента живут в одной группе: падение любого из них
	// останавливает остальные, и процесс не остаётся «наполовину рабочим».
	group, groupCtx := errgroup.WithContext(ctx)
	group.Go(func() error { return httpSrv.Run(groupCtx) })
	group.Go(func() error { return outboxWorker.Run(groupCtx) })
	group.Go(func() error { return reaperWorker.Run(groupCtx) })

	if err := group.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}

	log.InfoContext(context.WithoutCancel(ctx), "сервис остановлен")
	return nil
}
