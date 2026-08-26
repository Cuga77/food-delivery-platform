package middleware

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"time"

	"avito-kitchen/internal/app"
	"avito-kitchen/internal/domain"
	"avito-kitchen/internal/platform/problem"
)

// IdempotencyKeyHeader — заголовок с ключом идемпотентности.
const IdempotencyKeyHeader = "Idempotency-Key"

const (
	// maxIdempotentBodySize ограничивает объём, который прослойка готова
	// прочитать в память ради подсчёта хэша.
	maxIdempotentBodySize = 1 << 20 // 1 МБ
	minKeyLength          = 8
	maxKeyLength          = 128
)

// Idempotency защищает небезопасные операции от повторного выполнения.
//
// Схема работы:
//  1. тело запроса читается целиком и хэшируется — хэш привязывается к ключу;
//  2. ключ атомарно захватывается в БД; захват достаётся ровно одному из
//     параллельных запросов;
//  3. захвативший выполняет операцию, а её успешный ответ сохраняется;
//  4. повтор с тем же ключом и тем же телом получает сохранённый ответ, не
//     доходя до бизнес-логики;
//  5. тот же ключ с другим телом — ошибка: это программная ошибка клиента,
//     а не повтор.
//
// Ответы с ошибкой не кэшируются, а захват освобождается: заказ, отклонённый
// из-за нехватки остатка, клиент вправе повторить тем же ключом, когда остаток
// пополнится.
func Idempotency(repo app.IdempotencyRepo, ttl time.Duration, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()

			key := r.Header.Get(IdempotencyKeyHeader)
			if len(key) < minKeyLength || len(key) > maxKeyLength {
				problem.WriteCode(w, r, domain.CodeIdempotencyKeyRequired,
					"требуется заголовок Idempotency-Key длиной от 8 до 128 символов (рекомендуется UUID)")
				return
			}

			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxIdempotentBodySize))
			if err != nil {
				problem.WriteCode(w, r, domain.CodeBadRequest,
					"не удалось прочитать тело запроса (возможно, оно превышает 1 МБ)")
				return
			}
			// Хендлер ниже по цепочке должен получить тело нетронутым.
			r.Body = io.NopCloser(bytes.NewReader(body))

			hash := hashBody(body)

			existing, claimed, err := repo.Reserve(ctx, key, hash, time.Now().Add(ttl))
			if err != nil {
				problem.Write(w, r, err, log)
				return
			}

			if !claimed {
				replayOrReject(w, r, existing, hash)
				return
			}

			recorder := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
			completed := false

			defer func() {
				if completed {
					return
				}
				// Сюда попадаем при панике: захват нужно снять, иначе ключ
				// останется «выполняющимся» до истечения TTL.
				releaseKey(ctx, repo, key, log)
			}()

			next.ServeHTTP(recorder, r)
			completed = true

			if recorder.status >= http.StatusOK && recorder.status < http.StatusMultipleChoices {
				if err := repo.Complete(ctx, key, recorder.status, recorder.body.Bytes()); err != nil {
					// Ответ клиенту уже ушёл; повтор с тем же ключом просто
					// выполнится заново. Логируем и живём дальше.
					log.ErrorContext(ctx, "не удалось сохранить ответ идемпотентной операции",
						slog.String("idempotency_key", key), slog.Any("error", err))
				}
				return
			}

			releaseKey(ctx, repo, key, log)
		})
	}
}

func replayOrReject(w http.ResponseWriter, r *http.Request, existing app.IdempotencyRecord, hash string) {
	switch {
	case existing.Key == "":
		// Запись исчезла между захватом и чтением — редкая гонка с уборкой.
		problem.WriteCode(w, r, domain.CodeIdempotencyInProgress,
			"запрос с этим Idempotency-Key сейчас обрабатывается, повторите позже")

	case existing.RequestHash != hash:
		problem.WriteCode(w, r, domain.CodeIdempotencyPayloadMismatch,
			"этот Idempotency-Key уже использован с другим телом запроса — используйте новый ключ")

	case existing.InProgress():
		problem.WriteCode(w, r, domain.CodeIdempotencyInProgress,
			"запрос с этим Idempotency-Key сейчас обрабатывается, повторите позже")

	default:
		// Честный повтор: отдаём ровно тот ответ, что получил первый запрос.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Idempotent-Replay", "true")
		w.WriteHeader(existing.ResponseStatus)
		_, _ = w.Write(existing.ResponseBody)
	}
}

func releaseKey(ctx context.Context, repo app.IdempotencyRepo, key string, log *slog.Logger) {
	// Контекст запроса к этому моменту может быть уже отменён, а снять захват
	// нужно обязательно — иначе клиент не сможет повторить операцию.
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()

	if err := repo.Release(releaseCtx, key); err != nil {
		log.ErrorContext(releaseCtx, "не удалось освободить ключ идемпотентности",
			slog.String("idempotency_key", key), slog.Any("error", err))
	}
}

func hashBody(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// responseRecorder запоминает статус и тело ответа, попутно отдавая их клиенту.
type responseRecorder struct {
	http.ResponseWriter
	status      int
	body        bytes.Buffer
	wroteHeader bool
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	r.body.Write(p)
	return r.ResponseWriter.Write(p)
}
