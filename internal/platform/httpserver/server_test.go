package httpserver_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"avito-kitchen/internal/platform/httpserver"
)

// Корректная остановка — не украшение: без неё каждый деплой обрывал бы заказы
// в момент оформления, посреди открытой транзакции.

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// freePort занимает и сразу отпускает порт, чтобы получить заведомо свободный.
func freePort(t *testing.T) int {
	t.Helper()

	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	addr, ok := l.Addr().(*net.TCPAddr)
	require.True(t, ok)
	port := addr.Port
	require.NoError(t, l.Close())

	return port
}

// get выполняет запрос с контекстом теста.
//
// Утверждений внутри намеренно нет: помощник вызывается в том числе из
// горутин, а t.FailNow из чужой горутины тест не останавливает — он лишь
// помечает его, и дальнейшее поведение непредсказуемо. Ошибку возвращаем
// вызывающему, который проверит её на своей горутине.
func get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}

	return http.DefaultClient.Do(req)
}

func config(port int) httpserver.Config {
	return httpserver.Config{
		Port:            port,
		ReadTimeout:     2 * time.Second,
		WriteTimeout:    5 * time.Second,
		IdleTimeout:     10 * time.Second,
		ShutdownTimeout: 3 * time.Second,
	}
}

// start поднимает сервер и возвращает его адрес и канал с результатом Run.
func start(t *testing.T, h http.Handler, cfg httpserver.Config) (*httpserver.Server, <-chan error, context.CancelFunc) {
	t.Helper()

	srv := httpserver.New(h, cfg, discard())
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()

	require.Eventually(t, func() bool { return srv.Addr() != "" },
		3*time.Second, 10*time.Millisecond, "сервер должен встать на порт")

	return srv, done, cancel
}

func TestRun_ServesAndStopsCleanly(t *testing.T) {
	t.Parallel()

	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "готово")
	})

	srv, done, cancel := start(t, handler, config(0))

	resp, err := get(t.Context(), "http://"+srv.Addr())
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "готово", string(body))

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "штатная остановка не ошибка")
	case <-time.After(5 * time.Second):
		t.Fatal("сервер не остановился")
	}
}

// Главное свойство: запрос, начавшийся до сигнала остановки, обязан
// доработать. Иначе деплой рвёт транзакции на середине.
func TestRun_WaitsForInFlightRequest(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		// Имитируем работу, которая идёт дольше, чем сигнал остановки.
		time.Sleep(400 * time.Millisecond)
		_, _ = io.WriteString(w, "доделал")
	})

	srv, done, cancel := start(t, handler, config(0))

	type result struct {
		body string
		err  error
	}
	responses := make(chan result, 1)

	go func() {
		resp, err := get(t.Context(), "http://"+srv.Addr())
		if err != nil {
			responses <- result{err: err}
			return
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		responses <- result{body: string(body)}
	}()

	<-started
	cancel() // остановка ровно посреди обработки

	select {
	case r := <-responses:
		require.NoError(t, r.err, "запрос не должен быть оборван")
		assert.Equal(t, "доделал", r.body, "начатый запрос доработал до конца")
	case <-time.After(5 * time.Second):
		t.Fatal("ответ так и не пришёл")
	}

	require.NoError(t, <-done)
}

// Если начатый запрос не укладывается в отведённое на остановку время,
// Run обязан сообщить об этом, а не притвориться, что всё прошло гладко.
func TestRun_ReportsShutdownTimeout(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
	})

	cfg := config(0)
	cfg.ShutdownTimeout = 200 * time.Millisecond

	srv, done, cancel := start(t, handler, cfg)

	go func() {
		resp, err := get(t.Context(), "http://"+srv.Addr())
		if err == nil {
			_ = resp.Body.Close()
		}
	}()

	<-started
	cancel()

	select {
	case err := <-done:
		require.Error(t, err, "истёкший таймаут остановки должен быть виден")
		assert.Contains(t, err.Error(), "остановка HTTP-сервера")
	case <-time.After(5 * time.Second):
		t.Fatal("Run не завершился")
	}

	close(release)
}

// Занятый порт обязан обнаруживаться сразу, а не после строки «сервер запущен».
func TestRun_ReportsBusyPort(t *testing.T) {
	t.Parallel()

	port := freePort(t)

	var lc net.ListenConfig
	occupied, err := lc.Listen(t.Context(), "tcp", net.JoinHostPort("", strconv.Itoa(port)))
	require.NoError(t, err)
	defer func() { _ = occupied.Close() }()

	srv := httpserver.New(http.NotFoundHandler(), config(port), discard())

	runErr := srv.Run(context.Background())

	require.Error(t, runErr)
	assert.Contains(t, runErr.Error(), "привязка")
	assert.Empty(t, srv.Addr(), "адрес не выставляется, раз привязка не удалась")
}
