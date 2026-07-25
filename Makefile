BINARY  := kiln
PKG     := github.com/daiwa-zou/kiln
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X '$(PKG)/internal/observability.Version=$(VERSION)'

TEST_DB_URL := postgres://kiln:kiln@localhost:55432/kiln?sslmode=disable

.PHONY: all build test test-verbose test-integration db-up db-down lint fmt tidy migrate dev clean

all: fmt test build

build:
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/kiln

# Hermetic: no network, no database. Integration tests skip themselves.
test:
	go test ./...

test-verbose:
	go test -v -race ./...

# Requires db-up. Exercises the schema against a real Postgres.
test-integration: db-up
	KILN_TEST_DATABASE_URL="$(TEST_DB_URL)" go test ./... -count=1

db-up:
	@docker inspect kiln-test >/dev/null 2>&1 || \
		docker run -d --name kiln-test \
			-e POSTGRES_PASSWORD=kiln -e POSTGRES_USER=kiln -e POSTGRES_DB=kiln \
			-p 55432:5432 postgres:16-alpine >/dev/null
	@docker start kiln-test >/dev/null 2>&1 || true
	@until docker exec kiln-test pg_isready -U kiln >/dev/null 2>&1; do sleep 0.5; done
	@echo "postgres ready on :55432"

db-down:
	@docker rm -f kiln-test >/dev/null 2>&1 || true
	@echo "postgres removed"

lint:
	go vet ./...

fmt:
	go fmt ./...

tidy:
	go mod tidy

migrate: build
	./bin/$(BINARY) admin migrate

dev: build
	./bin/$(BINARY) serve --with-worker

clean:
	rm -rf bin dist
