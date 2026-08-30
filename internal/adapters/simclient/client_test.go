package simclient_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"avito-kitchen/internal/adapters/simclient"
	"avito-kitchen/internal/app"
	"avito-kitchen/internal/domain"
)

// Клиент доставки решает, стоит ли повторять неудачу. Ошибка в этой
// классификации стоит дорого в обе стороны: временный сбой, принятый за
// окончательный, теряет событие; окончательный отказ, принятый за временный,
// десять раз бьётся в стену и оттягивает разбор инцидента на часы.

func testEvent() app.DeliveryEvent {
	return app.DeliveryEvent{
		EventID:     42,
		EventType:   app.EventOrderCreated,
		AggregateID: "01a0443a-edcf-7be1-a022-c718b102293f",
		Payload:     json.RawMessage(`{"restaurant_id":1}`),
		CreatedAt:   time.Now(),
	}
}

func TestDeliver_Success(t *testing.T) {
	t.Parallel()

	var (
		gotPath    string
		gotEventID string
		gotBody    []byte
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotEventID = r.Header.Get(simclient.EventIDHeader)
		gotBody, _ = readAll(r)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	err := simclient.New(2*time.Second).Deliver(t.Context(), srv.URL, testEvent())

	require.NoError(t, err)
	assert.Equal(t, "/kitchen/events", gotPath)
	assert.Equal(t, "42", gotEventID, "идентификатор события нужен получателю для дедупликации")

	var envelope struct {
		EventID     int64           `json:"event_id"`
		EventType   string          `json:"event_type"`
		AggregateID string          `json:"aggregate_id"`
		Payload     json.RawMessage `json:"payload"`
	}
	require.NoError(t, json.Unmarshal(gotBody, &envelope))
	assert.Equal(t, int64(42), envelope.EventID)
	assert.Equal(t, app.EventOrderCreated, envelope.EventType)
	assert.JSONEq(t, `{"restaurant_id":1}`, string(envelope.Payload))
}

func TestDeliver_TrimsTrailingSlashInBaseURL(t *testing.T) {
	t.Parallel()

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	require.NoError(t, simclient.New(2*time.Second).Deliver(t.Context(), srv.URL+"/", testEvent()))
	assert.Equal(t, "/kitchen/events", gotPath, "двойного слэша в пути быть не должно")
}

// Классификация ответов — суть этого клиента.
func TestDeliver_ClassifiesResponses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		status    int
		wantErr   bool
		permanent bool
	}{
		{"200 — доставлено", http.StatusOK, false, false},
		{"201 — доставлено", http.StatusCreated, false, false},
		{"204 — доставлено", http.StatusNoContent, false, false},

		{"400 — окончательный отказ", http.StatusBadRequest, true, true},
		{"401 — доступ отозван, повтор не поможет", http.StatusUnauthorized, true, true},
		{"404 — заведение не знает такого эндпоинта", http.StatusNotFound, true, true},
		{"422 — событие не понято", http.StatusUnprocessableEntity, true, true},

		{"408 — заведение не успело, но готово принять", http.StatusRequestTimeout, true, false},
		{"429 — просит сбавить темп", http.StatusTooManyRequests, true, false},
		{"500 — временный сбой", http.StatusInternalServerError, true, false},
		{"503 — временно недоступно", http.StatusServiceUnavailable, true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			err := simclient.New(2*time.Second).Deliver(t.Context(), srv.URL, testEvent())

			if !tt.wantErr {
				require.NoError(t, err)
				return
			}

			require.Error(t, err)
			assert.Equalf(t, tt.permanent, errors.Is(err, app.ErrDeliveryRejected),
				"HTTP %d: окончательность отказа определена неверно", tt.status)
		})
	}
}

// Недоступное заведение — временный сбой: сервис могли перезапускать.
func TestDeliver_UnreachableIsTransient(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // адрес занят никем

	err := simclient.New(time.Second).Deliver(t.Context(), srv.URL, testEvent())

	require.Error(t, err)
	require.NotErrorIs(t, err, app.ErrDeliveryRejected, "повторить стоит")
	assert.Equal(t, domain.CodeServiceUnavailable, domain.CodeOf(err))
}

// Таймаут тоже временный: заведение живо, но медлит.
func TestDeliver_TimeoutIsTransient(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	err := simclient.New(100*time.Millisecond).Deliver(t.Context(), srv.URL, testEvent())

	require.Error(t, err)
	assert.NotErrorIs(t, err, app.ErrDeliveryRejected)
}

// Отмена контекста не должна выглядеть как отказ заведения.
func TestDeliver_ContextCancellation(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release
		w.WriteHeader(http.StatusNoContent)
	}))
	defer func() {
		close(release)
		srv.Close()
	}()

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := simclient.New(5*time.Second).Deliver(ctx, srv.URL, testEvent())

	require.Error(t, err)
	assert.NotErrorIs(t, err, app.ErrDeliveryRejected)
}

func readAll(r *http.Request) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()

	buf := make([]byte, 0, 512)
	tmp := make([]byte, 512)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return buf, nil
		}
	}
}
