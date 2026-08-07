BINARY      := aigcd
BIN_DIR     := bin
PKG         := ./...
MIGRATE_DIR := ./migrations

.PHONY: build run vet test migrate-up migrate-down

build:
	go build -o $(BIN_DIR)/$(BINARY) ./cmd/aigcd

run:
	go run ./cmd/aigcd

vet:
	go vet $(PKG)

test:
	go test $(PKG)

# migrate-up / migrate-down shell out to the `migrate` CLI
# (https://github.com/golang-migrate/migrate) against $AIGC_MYSQL_DSN.
# The DSN is read from the environment on purpose: it carries credentials and
# must never be baked into the Makefile or committed.
migrate-up:
	@test -n "$(AIGC_MYSQL_DSN)" || (echo "AIGC_MYSQL_DSN is not set" && exit 1)
	migrate -path $(MIGRATE_DIR) -database "mysql://$(AIGC_MYSQL_DSN)" up

migrate-down:
	@test -n "$(AIGC_MYSQL_DSN)" || (echo "AIGC_MYSQL_DSN is not set" && exit 1)
	migrate -path $(MIGRATE_DIR) -database "mysql://$(AIGC_MYSQL_DSN)" down 1
