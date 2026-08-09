BINARY  := aigcd
BIN_DIR := bin
PKG     := ./...

.PHONY: build run vet test smoke health db-up db-down migrate-up migrate-down seed dev clean

build:
	go build -o $(BIN_DIR)/$(BINARY) ./cmd/aigcd

run:
	go run ./cmd/aigcd serve

vet:
	go vet $(PKG)

test:
	go test $(PKG)

# smoke 打的是另一台已经在跑的服务，因此不依赖任何本地目标：
# 先在一个终端 make run，再在另一个终端 make smoke。
# 目标地址取 AIGC_SMOKE_BASE_URL，默认 http://localhost:18080。
smoke:
	go run ./scripts/smoke

# health 只盯上线前会要命、且代码没改也会漂移的四件事：用户端目录里有没有
# mock/假上游模型、前端调的端点后端有没有、图片与视频主流程是不是真跑得通。
# 与 smoke 一样打的是另一台已经在跑的服务。
health:
	go run ./scripts/healthcheck

# ── 本地依赖 ─────────────────────────────────────────────────────────
# db-up 拉起本地 MySQL 8.4。docker-compose.yml 里的口令是明摆着的开发占位符，
# 任何共享环境都不该用它。
db-up:
	docker compose up -d
	@echo "waiting for mysql to become healthy..."
	@until [ "$$(docker inspect -f '{{.State.Health.Status}}' aigc-pool-mysql 2>/dev/null)" = "healthy" ]; do sleep 1; done
	@echo "mysql is ready"

db-down:
	docker compose down

# ── 迁移与种子 ───────────────────────────────────────────────────────
# 迁移由 aigcd 自己跑，不依赖外部 golang-migrate CLI：
# 「照着 README 走能从零到可用」不该卡在让人先装一个 Go 工具上。
# DSN 仍只从环境读（AIGC_MYSQL_DSN），它带凭证，不能写进 Makefile。
migrate-up:
	go run ./cmd/aigcd migrate up

migrate-down:
	go run ./cmd/aigcd migrate down 1

seed:
	go run ./cmd/aigcd seed

# dev 是从零到可用的一条龙：起库 → 建表 → 灌种子 → 起服务。
dev: db-up migrate-up seed run

clean:
	rm -rf $(BIN_DIR)
