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
	"sync"
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

	// addr заполняется после успешной привязки к порту. Нужен тестам, где
	// сервер поднимается на порту 0 и адрес известен только операционной
	// системе; заодно в лог попадает реально занятый порт, а не «:0».
	mu   sync.Mutex
	addr string
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
	// Привязка выполняется до запуска горутины, а не внутри ListenAndServe.
	// Иначе занятый порт обнаруживался бы уже после строки «сервер запущен» —
	// лог сообщал бы об успехе, которого не было.
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp", s.srv.Addr)
	if err != nil {
		return fmt.Errorf("привязка к %s: %w", s.srv.Addr, err)
	}

	s.setAddr(listener.Addr().String())
	s.log.InfoContext(ctx, "HTTP-сервер запущен", slog.String("addr", s.Addr()))

	errCh := make(chan error, 1)

	go func() {
		if err := s.srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
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

// Addr возвращает адрес, на который сервер реально встал. До вызова Run пуст.
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.addr
}

func (s *Server) setAddr(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addr = addr
}
