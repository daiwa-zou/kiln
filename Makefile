BINARY  := kiln
PKG     := github.com/daiwa-zou/kiln
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X '$(PKG)/internal/observability.Version=$(VERSION)'

TEST_DB_URL := postgres://kiln:kiln@localhost:55432/kiln?sslmode=disable

# Pinned to match .github/workflows/ci.yml. Bump both together.
GOLANGCI := github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2

.PHONY: all build test test-verbose test-integration cover db-up db-down lint vulncheck fmt tidy migrate dev dev-db migrate-dev dev-build dev-clean clean image compose-up compose-down manifests helm-lint

all: fmt test build

build:
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/kiln

# Hermetic: no network, no database. Integration tests skip themselves.
test:
	go test ./...

test-verbose:
	go test -v -race ./...

# Requires db-up. Exercises the schema against a real Postgres.
#
# -p 1 is load-bearing: internal/api and internal/store both DROP SCHEMA public
# on the single shared database, so parallel package binaries race and fail
# with spurious "relation ... does not exist" errors.
test-integration: db-up
	KILN_TEST_DATABASE_URL="$(TEST_DB_URL)" go test ./... -count=1 -p 1

# Coverage profile plus the badge CI commits. Needs the database for the same
# reason CI measures coverage in the integration job: without it internal/store
# reports ~5% and the badge understates the project by a wide margin.
cover: db-up
	KILN_TEST_DATABASE_URL="$(TEST_DB_URL)" go test -race ./... -count=1 -p 1 -covermode=atomic -coverpkg=./... -coverprofile=coverage.out
	@go tool cover -func=coverage.out | tail -1
	@./scripts/coverage-badge.sh coverage.out .github/badges/coverage.svg

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

# Pinned so local runs match CI exactly. `go run` caches the build, so only
# the first invocation after a version bump is slow.
lint:
	go run $(GOLANGCI) run

vulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

fmt:
	go fmt ./...

tidy:
	go mod tidy

migrate: build
	./bin/$(BINARY) admin migrate

# --- Local development environment ------------------------------------------
# Everything below runs against config.dev.toml: the fake agent runner (full
# pipeline, zero LLM cost, no API key) and a dedicated kiln_dev database so
# `make test-integration` (which drops the `kiln` database's schema) can never
# wipe dev state.

DEV_CONFIG := config.dev.toml

dev-db: db-up
	@docker exec kiln-test psql -U kiln -tc "SELECT 1 FROM pg_database WHERE datname='kiln_dev'" | grep -q 1 || \
		docker exec kiln-test psql -U kiln -c "CREATE DATABASE kiln_dev"
	@echo "kiln_dev ready"

migrate-dev: build dev-db
	./bin/$(BINARY) admin migrate --config $(DEV_CONFIG)

# API + UI + in-process worker. Connectors may read anything under this repo.
dev: migrate-dev
	KILN_WORKER_PERMITTED_SOURCE_ROOTS=$(CURDIR) \
		./bin/$(BINARY) serve --with-worker --config $(DEV_CONFIG)

# One-shot build of kiln itself into the dev wiki through the full pipeline.
# Bootstraps the org/workspace/wiki chain on first run; repeating it is free
# (the hash gate skips unchanged sources).
dev-build: migrate-dev
	./bin/$(BINARY) build $(CURDIR) --config $(DEV_CONFIG)

dev-clean:
	@docker exec kiln-test psql -U kiln -c "DROP DATABASE IF EXISTS kiln_dev" 2>/dev/null || true
	@rm -rf .dev
	@echo "dev state removed"

# --- deployment -------------------------------------------------------------

# The same image CI publishes, built locally for a smoke test or an air-gapped
# registry push.
image:
	docker build -t kiln:$(VERSION) --build-arg VERSION=$(VERSION) .

compose-up:
	docker compose up -d --build
	@echo "kiln at http://localhost:8080 — mint a token:"
	@echo "  docker compose exec api kiln admin token create --login you --scopes read,write,admin --admin"

compose-down:
	docker compose down -v

helm-lint:
	helm lint deploy/helm/kiln --set secrets.existingSecret=kiln-secrets

# Renders the chart to plain YAML for GitOps repositories and Kustomize
# overlays. Deliberately gitignored: a generated manifest committed beside its
# generator drifts, and a stale one still applies cleanly.
manifests:
	@mkdir -p deploy/kubernetes
	helm template kiln deploy/helm/kiln \
		--namespace kiln \
		--set secrets.existingSecret=kiln-secrets \
		--set config.database.host=postgres.kiln.svc.cluster.local \
		> deploy/kubernetes/kiln.yaml
	@echo "wrote deploy/kubernetes/kiln.yaml"

clean:
	rm -rf bin dist
