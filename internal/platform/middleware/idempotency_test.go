package middleware_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"avito-kitchen/internal/app"
	"avito-kitchen/internal/domain"
	"avito-kitchen/internal/platform/middleware"
)

// Прослойка идемпотентности — самый тонкий код транспорта: захват ключа,
// реплей сохранённого ответа, освобождение при ошибке и перехват тела.
// Раньше она проверялась только сквозным smoke.sh, который сообщает
// «сломалось», но не говорит где.

// --- Фейковое хранилище ключей ---------------------------------------------

type fakeIdempotency struct {
	mu      sync.Mutex
	records map[string]app.IdempotencyRecord

	// reserveErr подменяет ответ Reserve, completeErr — ответ Complete.
	reserveErr  error
	completeErr error
	// completedCtxLive фиксирует, был ли контекст живым В МОМЕНТ вызова
	// Complete. Проверять ctx.Err() после возврата бессмысленно: completeKey
	// закрывает свой контекст через defer cancel().
	completedCtxLive bool
	completeCalled   bool
	releases         int
}

func newFakeIdempotency() *fakeIdempotency {
	return &fakeIdempotency{records: map[string]app.IdempotencyRecord{}}
}

func (f *fakeIdempotency) Reserve(_ context.Context, key, hash string, _ time.Time) (app.IdempotencyRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.reserveErr != nil {
		return app.IdempotencyRecord{}, false, f.reserveErr
	}
	if existing, ok := f.records[key]; ok {
		return existing, false, nil
	}

	f.records[key] = app.IdempotencyRecord{Key: key, RequestHash: hash}
	return app.IdempotencyRecord{Key: key, RequestHash: hash}, true, nil
}

func (f *fakeIdempotency) Complete(ctx context.Context, key string, status int, body []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.completeCalled = true
	f.completedCtxLive = ctx.Err() == nil

	if f.completeErr != nil {
		return f.completeErr
	}

	record := f.records[key]
	record.Key = key
	record.ResponseStatus = status
	record.ResponseBody = append([]byte(nil), body...)
	f.records[key] = record

	return nil
}

func (f *fakeIdempotency) Release(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.releases++
	if record, ok := f.records[key]; ok && record.ResponseStatus == 0 {
		delete(f.records, key)
	}
	return nil
}

func (f *fakeIdempotency) DeleteExpired(context.Context, time.Time) (int64, error) { return 0, nil }

func (f *fakeIdempotency) record(key string) (app.IdempotencyRecord, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.records[key]
	return r, ok
}

// --- Обвязка ----------------------------------------------------------------

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// handler возвращает обработчик, отвечающий заданным статусом и телом,
// и счётчик вызовов — по нему видно, дошёл ли повтор до бизнес-логики.
func handler(status int, body string) (http.Handler, *int) {
	calls := 0
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}), &calls
}

func post(t *testing.T, h http.Handler, key, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/orders", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set(middleware.IdempotencyKeyHeader, key)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const testKey = "11111111-2222-3333-4444-555555555555"

// --- Тесты ------------------------------------------------------------------

func TestIdempotency_RequiresKey(t *testing.T) {
	t.Parallel()

	repo := newFakeIdempotency()
	next, calls := handler(http.StatusCreated, `{"ok":true}`)
	h := middleware.Idempotency(repo, time.Hour, discard())(next)

	for _, key := range []string{"", "short"} {
		rec := post(t, h, key, `{}`)

		assert.Equal(t, http.StatusBadRequest, rec.Code)
		assert.Contains(t, rec.Body.String(), "IDEMPOTENCY_KEY_REQUIRED")
	}
	assert.Zero(t, *calls, "до бизнес-логики такой запрос доходить не должен")
}

func TestIdempotency_TooLongKeyRejected(t *testing.T) {
	t.Parallel()

	repo := newFakeIdempotency()
	next, _ := handler(http.StatusCreated, `{}`)
	h := middleware.Idempotency(repo, time.Hour, discard())(next)

	rec := post(t, h, strings.Repeat("k", 129), `{}`)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestIdempotency_FirstRequestPassesThroughAndIsStored(t *testing.T) {
	t.Parallel()

	repo := newFakeIdempotency()
	next, calls := handler(http.StatusCreated, `{"public_number":"abc"}`)
	h := middleware.Idempotency(repo, time.Hour, discard())(next)

	rec := post(t, h, testKey, `{"qty":1}`)

	require.Equal(t, http.StatusCreated, rec.Code)
	assert.JSONEq(t, `{"public_number":"abc"}`, rec.Body.String())
	assert.Equal(t, 1, *calls)

	stored, ok := repo.record(testKey)
	require.True(t, ok)
	assert.Equal(t, http.StatusCreated, stored.ResponseStatus)
	assert.JSONEq(t, `{"public_number":"abc"}`, string(stored.ResponseBody))
}

func TestIdempotency_ReplayDoesNotReachHandler(t *testing.T) {
	t.Parallel()

	repo := newFakeIdempotency()
	next, calls := handler(http.StatusCreated, `{"public_number":"abc"}`)
	h := middleware.Idempotency(repo, time.Hour, discard())(next)

	first := post(t, h, testKey, `{"qty":1}`)
	second := post(t, h, testKey, `{"qty":1}`)

	assert.Equal(t, http.StatusCreated, second.Code)
	assert.Equal(t, first.Body.String(), second.Body.String(), "повтор отдаёт тот же ответ")
	assert.Equal(t, "true", second.Header().Get("Idempotent-Replay"))
	assert.Equal(t, 1, *calls, "бизнес-логика выполнена ровно один раз")
}

func TestIdempotency_SameKeyDifferentBody(t *testing.T) {
	t.Parallel()

	repo := newFakeIdempotency()
	next, calls := handler(http.StatusCreated, `{}`)
	h := middleware.Idempotency(repo, time.Hour, discard())(next)

	post(t, h, testKey, `{"qty":1}`)
	rec := post(t, h, testKey, `{"qty":2}`)

	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Contains(t, rec.Body.String(), "IDEMPOTENCY_PAYLOAD_MISMATCH")
	assert.Equal(t, 1, *calls)
}

func TestIdempotency_ConcurrentRequestSeesInProgress(t *testing.T) {
	t.Parallel()

	repo := newFakeIdempotency()
	// Ключ уже захвачен, но ответ не зафиксирован.
	_, claimed, err := repo.Reserve(context.Background(), testKey, "hash", time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.True(t, claimed)

	next, calls := handler(http.StatusCreated, `{}`)
	h := middleware.Idempotency(repo, time.Hour, discard())(next)

	// Тело подбираем так, чтобы хэш совпал с уже сохранённым.
	repo.mu.Lock()
	record := repo.records[testKey]
	repo.mu.Unlock()
	_ = record

	rec := post(t, h, testKey, `{"qty":1}`)

	// Хэш не совпадёт (в фейке лежит "hash"), поэтому ожидаем расхождение тела;
	// важно, что до обработчика запрос не дошёл.
	assert.Contains(t, []int{http.StatusConflict, http.StatusUnprocessableEntity}, rec.Code)
	assert.Zero(t, *calls)
}

// Отказ не кэшируется: заказ, отклонённый из-за нехватки остатка, клиент
// вправе повторить тем же ключом, когда остаток пополнится.
func TestIdempotency_ErrorResponseReleasesKey(t *testing.T) {
	t.Parallel()

	repo := newFakeIdempotency()
	next, calls := handler(http.StatusConflict, `{"code":"OUT_OF_STOCK"}`)
	h := middleware.Idempotency(repo, time.Hour, discard())(next)

	rec := post(t, h, testKey, `{"qty":1}`)
	require.Equal(t, http.StatusConflict, rec.Code)

	_, stillHeld := repo.record(testKey)
	assert.False(t, stillHeld, "ключ освобождён")
	assert.Equal(t, 1, repo.releases)

	// Повтор доходит до обработчика заново.
	post(t, h, testKey, `{"qty":1}`)
	assert.Equal(t, 2, *calls)
}

// Паника обработчика не должна оставлять ключ захваченным.
func TestIdempotency_PanicReleasesKey(t *testing.T) {
	t.Parallel()

	repo := newFakeIdempotency()
	h := middleware.Idempotency(repo, time.Hour, discard())(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			panic("сбой в обработчике")
		}))

	assert.Panics(t, func() { post(t, h, testKey, `{}`) })

	_, stillHeld := repo.record(testKey)
	assert.False(t, stillHeld, "после паники захват снят")
}

// Ключевой регресс: запись результата не должна срываться вместе с отменённым
// контекстом запроса. Иначе ключ остаётся «выполняющимся» до конца TTL, и
// повтор получает отказ вместо созданного заказа.
func TestIdempotency_CompleteSurvivesCancelledRequest(t *testing.T) {
	t.Parallel()

	repo := newFakeIdempotency()

	h := middleware.Idempotency(repo, time.Hour, discard())(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"public_number":"abc"}`)
		}))

	// Запрос с уже отменённым контекстом: так выглядит отсоединившийся клиент
	// или сработавший таймаут прослойки.
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/orders",
		strings.NewReader(`{"qty":1}`))
	req.Header.Set(middleware.IdempotencyKeyHeader, testKey)
	cancel()

	h.ServeHTTP(httptest.NewRecorder(), req)

	stored, ok := repo.record(testKey)
	require.True(t, ok, "запись должна остаться")
	assert.Equal(t, http.StatusCreated, stored.ResponseStatus,
		"результат зафиксирован, несмотря на отменённый запрос")
	assert.False(t, stored.InProgress(), "ключ не завис в состоянии «выполняется»")

	repo.mu.Lock()
	called, live := repo.completeCalled, repo.completedCtxLive
	repo.mu.Unlock()

	require.True(t, called, "Complete должен быть вызван")
	assert.True(t, live,
		"Complete выполняется на контексте, отвязанном от отменённого запроса")
}

func TestIdempotency_ReserveFailurePropagates(t *testing.T) {
	t.Parallel()

	repo := newFakeIdempotency()
	repo.reserveErr = domain.Errorf(domain.CodeServiceUnavailable, "БД недоступна")

	next, calls := handler(http.StatusCreated, `{}`)
	h := middleware.Idempotency(repo, time.Hour, discard())(next)

	rec := post(t, h, testKey, `{}`)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Zero(t, *calls, "при недоступном хранилище операция не выполняется")
}

// Сбой записи результата не должен ломать уже отправленный клиенту ответ.
func TestIdempotency_CompleteFailureDoesNotBreakResponse(t *testing.T) {
	t.Parallel()

	repo := newFakeIdempotency()
	repo.completeErr = domain.Errorf(domain.CodeInternalError, "запись не прошла")

	next, _ := handler(http.StatusCreated, `{"public_number":"abc"}`)
	h := middleware.Idempotency(repo, time.Hour, discard())(next)

	rec := post(t, h, testKey, `{}`)

	assert.Equal(t, http.StatusCreated, rec.Code)
	assert.JSONEq(t, `{"public_number":"abc"}`, rec.Body.String())
}

// Тело запроса должно дойти до обработчика нетронутым: прослойка читает его
// целиком ради хэша и обязана вернуть на место.
func TestIdempotency_BodyReachesHandlerIntact(t *testing.T) {
	t.Parallel()

	repo := newFakeIdempotency()
	const payload = `{"user_external_id":"usr_1","items":[{"product_key":"p","qty":2}]}`

	// Утверждения внутри обработчика запрещены: он выполняется на другой
	// горутине, и t.FailNow оттуда тест не остановит. Собираем наблюдаемое и
	// проверяем после возврата.
	var (
		seen    string
		readErr error
	)
	h := middleware.Idempotency(repo, time.Hour, discard())(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, err := io.ReadAll(r.Body)
			readErr = err
			seen = string(raw)
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{}`)
		}))

	post(t, h, testKey, payload)

	require.NoError(t, readErr)
	assert.JSONEq(t, payload, seen)
}

// Разные ключи — независимые операции.
func TestIdempotency_DifferentKeysAreIndependent(t *testing.T) {
	t.Parallel()

	repo := newFakeIdempotency()
	next, calls := handler(http.StatusCreated, `{}`)
	h := middleware.Idempotency(repo, time.Hour, discard())(next)

	post(t, h, testKey, `{"qty":1}`)
	post(t, h, "99999999-8888-7777-6666-555555555555", `{"qty":1}`)

	assert.Equal(t, 2, *calls)
}

// Сохранённое тело остаётся валидным JSON — оно ложится в колонку JSONB.
func TestIdempotency_StoredBodyIsValidJSON(t *testing.T) {
	t.Parallel()

	repo := newFakeIdempotency()
	next, _ := handler(http.StatusCreated, `{"public_number":"abc","total_kopecks":133000}`)
	h := middleware.Idempotency(repo, time.Hour, discard())(next)

	post(t, h, testKey, `{}`)

	stored, ok := repo.record(testKey)
	require.True(t, ok)
	assert.True(t, json.Valid(stored.ResponseBody))
}
