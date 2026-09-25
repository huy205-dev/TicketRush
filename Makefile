SHELL := /bin/bash
.DEFAULT_GOAL := help

COMPOSE  := docker compose -f deploy/compose.yaml
# Export every variable in .env to the recipe's shell.
LOAD_ENV := set -a && . ./.env && set +a &&

# Placeholder for targets whose milestone has not been built yet. Exits
# non-zero so scripts never mistake a stub for a pass.
todo = @echo "make $@: chưa triển khai, dự kiến có ở $(1) (SPEC.md mục 12)." >&2; exit 1

.PHONY: help up down ps logs migrate db-reset seed sqlc load-hold bench-hold bench-relay \
	run-booking run-waitingroom run-relay run-expiry run-ticket run-refunder run-notifier run-fakepay \
	test test-integration lint fmt bench invariants chaos-redis k8s-up

help: ## Liệt kê các target
	@grep -hE '^[a-zA-Z0-9_.-]+:.*## ' $(MAKEFILE_LIST) | awk -F ':.*## ' '{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

# Created only when missing, so it never overwrites local edits.
.env: ## Tạo .env từ .env.example với secret ngẫu nhiên
	@awk '/^[A-Z_]+_SECRET=$$/ { cmd = "openssl rand -hex 32"; cmd | getline s; close(cmd); print $$0 s; next } { print }' .env.example > $@
	@echo "Đã tạo .env (secret sinh ngẫu nhiên)."

## ---- Hạ tầng -------------------------------------------------------------

up: ## Chạy postgres, redis, redpanda, console và chờ healthy
	$(COMPOSE) up -d --wait

down: ## Dừng hạ tầng (giữ volume dữ liệu)
	$(COMPOSE) down

ps: ## Trạng thái container
	$(COMPOSE) ps

logs: ## Theo dõi log hạ tầng
	$(COMPOSE) logs -f

GOOSE := goose -dir migrations postgres "$$DATABASE_URL"

migrate: .env ## goose up
	$(LOAD_ENV) $(GOOSE) up

db-reset: .env ## XOÁ SẠCH dữ liệu (goose reset rồi up), chỉ dùng cho dev
	$(LOAD_ENV) $(GOOSE) reset && $(GOOSE) up

seed: .env ## Tạo sự kiện mẫu 5.000 ghế (sau db-reset thì event_id = 1)
	$(LOAD_ENV) go run ./cmd/seed

sqlc: ## Sinh code từ internal/*/queries.sql
	sqlc generate

## ---- Chạy service ---------------------------------------------------------

run-booking: .env ## Booking API trên :8080
	$(LOAD_ENV) go run ./cmd/booking

run-waitingroom: ## Waiting room trên :8081
	$(call todo,M5)

run-relay: .env ## Outbox relay: PostgreSQL outbox → Kafka (tạo topic nếu thiếu)
	$(LOAD_ENV) go run ./cmd/relay

run-expiry: .env ## Expiry worker: đơn HELD quá hạn → EXPIRED, nhả ghế
	$(LOAD_ENV) go run ./cmd/expiry

run-ticket: ## Ticket consumer
	$(call todo,M4)

run-refunder: ## Refund consumer
	$(call todo,M4)

run-notifier: ## Notifier consumer
	$(call todo,M4)

run-fakepay: ## Cổng thanh toán giả lập trên :8090
	$(call todo,M4)

## ---- Kiểm thử -------------------------------------------------------------

test: ## Unit test (race detector bật)
	go test -race ./...

test-integration: ## Test tích hợp với PostgreSQL thật (testcontainers, cần Docker)
	go test -race -count=1 -tags=integration ./...

lint: ## gofmt, go vet, staticcheck, code sqlc đã sinh khớp query
	@unformatted="$$(gofmt -l .)"; if [ -n "$$unformatted" ]; then echo "Cần chạy gofmt (make fmt):"; echo "$$unformatted"; exit 1; fi
	go vet -tags=integration ./...
	go tool staticcheck -tags=integration ./...
	sqlc diff

fmt: ## gofmt -w
	gofmt -w .

LOADTEST_OUT := loadtest/out

load-hold: ## k6 kịch bản mở bán; cần booking đang chạy và sự kiện đã seed
	@mkdir -p $(LOADTEST_OUT)
	k6 run --summary-export=$(LOADTEST_OUT)/hold_contention-summary.json \
		--out csv=$(LOADTEST_OUT)/hold_contention.csv.gz loadtest/hold_contention.js
	./loadtest/peak_rps.sh $(LOADTEST_OUT)/hold_contention.csv.gz hold

BACKEND ?= pg
RUNS ?= 3

bench-hold: .env ## Đo chuẩn kịch bản mở bán: RUNS lần (mặc định 3), reset DB + seed mỗi lần; BACKEND=pg|redis
	./loadtest/bench_hold.sh $(BACKEND) $(RUNS)

bench-relay: .env ## Đo throughput outbox relay: xả 200.000 dòng lên Kafka, 3 lần
	./loadtest/bench_relay.sh

bench: ## seed → k6 → invariants → tóm tắt
	$(call todo,M7)

invariants: ## Kiểm tra bất biến
	$(call todo,M7)

chaos-redis: ## Restart Redis giữa lúc k6 chạy
	$(call todo,M8)

k8s-up: ## Dựng toàn bộ hệ thống trên k3d
	$(call todo,M9)
