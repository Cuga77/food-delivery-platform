// Package httpserver запускает HTTP-сервер с корректной остановкой.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"
)

// Config — параметры сервера.
type Config struct {
	Port            int
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownTimeout time.Duration
}

// Server — HTTP-сервер с graceful shutdown.
type Server struct {
	srv             *http.Server
	shutdownTimeout time.Duration
	log             *slog.Logger
}

// New создаёт сервер поверх переданного обработчика.
func New(handler http.Handler, cfg Config, log *slog.Logger) *Server {
	return &Server{
		srv: &http.Server{
			Addr:              net.JoinHostPort("", strconv.Itoa(cfg.Port)),
			Handler:           handler,
			ReadTimeout:       cfg.ReadTimeout,
			ReadHeaderTimeout: cfg.ReadTimeout,
			WriteTimeout:      cfg.WriteTimeout,
			IdleTimeout:       cfg.IdleTimeout,
		},
		shutdownTimeout: cfg.ShutdownTimeout,
		log:             log,
	}
}

// Run слушает порт до отмены контекста, после чего корректно останавливается.
//
// «Корректно» означает: перестать принимать новые соединения, дать уже
// начатым запросам доесть свои транзакции и только потом закрыться. Без этого
// каждый деплой обрывал бы заказы в момент оформления.
func (s *Server) Run(ctx context.Context) error {
	errCh := make(chan error, 1)

	go func() {
		s.log.InfoContext(ctx, "HTTP-сервер запущен", slog.String("addr", s.srv.Addr))
		if err := s.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("работа HTTP-сервера: %w", err)
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	s.log.InfoContext(ctx, "останавливаем HTTP-сервер",
		slog.Duration("timeout", s.shutdownTimeout))

	// Контекст остановки не наследует отменённый ctx: иначе Shutdown завершился
	// бы мгновенно и оборвал текущие запросы.
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.shutdownTimeout)
	defer cancel()

	if err := s.srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("остановка HTTP-сервера: %w", err)
	}

	s.log.InfoContext(ctx, "HTTP-сервер остановлен")
	return <-errCh
}
