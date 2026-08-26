.DEFAULT_GOAL := help
SHELL := /bin/bash

# Версии инструментов, которые не пинуются через `tool` в go.mod.
GOLANGCI_LINT_VERSION := v2.13.1
PLANTUML_VERSION      := 1.2025.4

COMPOSE     := docker compose
LOCAL_BIN   := $(CURDIR)/bin
PLANTUML_JAR := $(CURDIR)/build/plantuml.jar

# DSN для операций с хоста (make test-integration, migrate-*).
DATABASE_URL_HOST ?= postgres://kitchen:kitchen@localhost:5432/kitchen?sslmode=disable

export PATH := $(LOCAL_BIN):$(PATH)

## help: список доступных команд
.PHONY: help
help:
	@echo "Авито.Кухня — доступные команды:"
	@grep -hE '^## ' $(MAKEFILE_LIST) | sed 's/^## /  make /' | sort

# ---------------------------------------------------------------------------
# Окружение
# ---------------------------------------------------------------------------

## up: поднять весь стек (postgres → migrate → api + restaurant-sim)
.PHONY: up
up:
	$(COMPOSE) up -d --build
	@echo "api → http://localhost:8080   restaurant-sim → http://localhost:8081"

## down: остановить стек, сохранив данные
.PHONY: down
down:
	$(COMPOSE) down

## down-v: остановить стек и удалить том с данными БД
.PHONY: down-v
down-v:
	$(COMPOSE) down -v

## ps: статус контейнеров
.PHONY: ps
ps:
	$(COMPOSE) ps

## logs: хвост логов api и restaurant-sim
.PHONY: logs
logs:
	$(COMPOSE) logs -f api restaurant-sim

## rebuild: пересобрать образы с нуля и поднять стек
.PHONY: rebuild
rebuild:
	$(COMPOSE) build --no-cache
	$(COMPOSE) up -d

# ---------------------------------------------------------------------------
# Миграции (выполняются образом migrate/migrate внутри сети compose)
# ---------------------------------------------------------------------------

## migrate-up: применить все миграции
.PHONY: migrate-up
migrate-up:
	$(COMPOSE) run --rm migrate up

## migrate-down: откатить одну миграцию
.PHONY: migrate-down
migrate-down:
	$(COMPOSE) run --rm migrate down 1

## migrate-drop: снести схему целиком (осторожно)
.PHONY: migrate-drop
migrate-drop:
	$(COMPOSE) run --rm migrate drop -f

## migrate-new: создать пару файлов миграции (make migrate-new name=add_promo)
.PHONY: migrate-new
migrate-new:
	@test -n "$(name)" || (echo "укажите name=<имя_миграции>" && exit 1)
	$(COMPOSE) run --rm migrate create -ext sql -dir /migrations -seq $(name)

# ---------------------------------------------------------------------------
# Кодогенерация
# ---------------------------------------------------------------------------

## gen: сгенерировать типы и strict-сервер из api/openapi.yaml
.PHONY: gen
gen:
	go tool oapi-codegen -config api/codegen.yaml api/openapi.yaml

## gen-check: убедиться, что сгенерированный код совпадает с контрактом
.PHONY: gen-check
gen-check: gen
	@git diff --exit-code -- internal/gen \
		|| (echo "internal/gen расходится с api/openapi.yaml — выполните make gen и закоммитьте результат" && exit 1)

# ---------------------------------------------------------------------------
# Качество кода
# ---------------------------------------------------------------------------

$(LOCAL_BIN)/golangci-lint:
	@mkdir -p $(LOCAL_BIN)
	GOBIN=$(LOCAL_BIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

## lint: статический анализ (golangci-lint)
.PHONY: lint
lint: $(LOCAL_BIN)/golangci-lint
	$(LOCAL_BIN)/golangci-lint run ./...

## fmt: автоформатирование
.PHONY: fmt
fmt: $(LOCAL_BIN)/golangci-lint
	$(LOCAL_BIN)/golangci-lint fmt ./...

## tidy: привести go.mod/go.sum в порядок
.PHONY: tidy
tidy:
	go mod tidy

## build: собрать оба бинарника в ./bin
.PHONY: build
build:
	go build -o $(LOCAL_BIN)/api ./cmd/api
	go build -o $(LOCAL_BIN)/restaurant-sim ./cmd/restaurant-sim

# ---------------------------------------------------------------------------
# Тесты
# ---------------------------------------------------------------------------

## test: модульные тесты с детектором гонок
.PHONY: test
test:
	go test -race -count=1 ./...

## test-integration: интеграционные тесты против живого PostgreSQL
.PHONY: test-integration
test-integration:
	TEST_DATABASE_URL="$(DATABASE_URL_HOST)" go test -race -count=1 -tags=integration ./internal/adapters/postgres/...

## cover: покрытие модульными тестами
.PHONY: cover
cover:
	go test -count=1 -coverprofile=coverage.txt ./...
	go tool cover -func=coverage.txt | tail -1

## smoke: сквозной E2E-прогон по поднятому стеку
.PHONY: smoke
smoke:
	./scripts/smoke.sh

# ---------------------------------------------------------------------------
# Диаграммы
# ---------------------------------------------------------------------------

$(PLANTUML_JAR):
	@mkdir -p $(dir $(PLANTUML_JAR))
	curl -sSfL -o $(PLANTUML_JAR) \
		https://github.com/plantuml/plantuml/releases/download/v$(PLANTUML_VERSION)/plantuml-$(PLANTUML_VERSION).jar

## diagrams: отрендерить docs/diagrams/*.puml в SVG
# -Playout=smetana включает встроенный в PlantUML движок раскладки, поэтому
# внешний graphviz (dot) не нужен — диаграммы собираются и в CI, и на пустой
# машине без дополнительных пакетов.
.PHONY: diagrams
diagrams: $(PLANTUML_JAR)
	java -jar $(PLANTUML_JAR) -tsvg -nometadata -Playout=smetana docs/diagrams/*.puml
	@echo "готово: $$(ls docs/diagrams/*.svg | wc -l) SVG"

# ---------------------------------------------------------------------------
# Композитные цели
# ---------------------------------------------------------------------------

## check: полный локальный прогон качества (gen-check + lint + test)
.PHONY: check
check: gen-check lint test

## e2e: чистый стек с нуля + сквозной сценарий
.PHONY: e2e
e2e: down-v up smoke
