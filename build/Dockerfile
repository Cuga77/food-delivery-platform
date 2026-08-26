# Общий multi-stage Dockerfile для обоих бинарников репозитория.
# Целевой сервис выбирается аргументом SERVICE: api или restaurant-sim.
#
# Стадии разделены так, чтобы слой с зависимостями переиспользовался между
# сборками: go.mod/go.sum копируются отдельно от исходников, поэтому правка
# кода не тянет за собой повторное скачивание модулей.

# --- Сборка -----------------------------------------------------------------
FROM golang:1.26-alpine AS builder

ARG SERVICE
ARG VERSION=dev

WORKDIR /src

# Слой зависимостей: инвалидируется только при изменении go.mod/go.sum.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO выключен — получаем полностью статический бинарник, которому не нужен
# ни libc, ни что-либо ещё в рантайм-образе.
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/service ./cmd/${SERVICE}

# --- Запуск -----------------------------------------------------------------
FROM alpine:3.24 AS runtime

# ca-certificates нужны для исходящих HTTPS-запросов, tzdata — чтобы время в
# логах и таймстампах не было привязано к UTC-по-умолчанию.
RUN apk add --no-cache ca-certificates tzdata wget \
    && adduser -D -u 10001 app

COPY --from=builder /out/service /usr/local/bin/service

USER app
ENTRYPOINT ["/usr/local/bin/service"]
