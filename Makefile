BINARY  := kiln
PKG     := github.com/daiwa-zou/kiln
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X '$(PKG)/internal/observability.Version=$(VERSION)'

TEST_DB_URL := postgres://kiln:kiln@localhost:55432/kiln?sslmode=disable

# Pinned to match .github/workflows/ci.yml. Bump both together.
GOLANGCI := github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2

.PHONY: all build test test-verbose test-integration test-claude cover db-up db-down lint vulncheck fmt tidy migrate dev dev-up dev-seed dev-db migrate-dev dev-build dev-clean clean image compose-up compose-down manifests helm-lint k8s-up k8s-down k8s-purge k8s-status k8s-logs k8s-token k8s-sync k8s-reauth k8s-shell

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

# The one check the rest of the suite cannot make: a real claude, a real model
# call, real prose imported as a page. Everything else about the CLI runner is
# covered against a fixture, which proves the pipeline drives it correctly but
# not that it then produces a wiki.
#
# Kept out of `test` and out of CI because it spends money and needs a logged-in
# session. Costs a few cents.
#
#   claude auth login     # or export ANTHROPIC_API_KEY
#   make test-claude
test-claude:
	KILN_TEST_CLAUDE=1 go test ./internal/jobs/ -count=1 -v -run TestCLIRunnerAgainstRealClaude

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

# The one-command entry point: Postgres, schema, a wiki with content already in
# it, then the server. Everything a deployment has except object storage, which
# config.dev.toml points at the filesystem so no MinIO is needed.
#
# `make dev` is the same thing without the seeding, for restarting the server
# against a wiki that already exists.
dev-up: dev-seed dev

# Builds this repository into the dev wiki until it converges. The loop is not
# belt-and-braces: a run stops at agent.max_pages_per_run and defers the rest,
# so a repository with more units than the cap needs several runs to finish.
# Each one is hash-gated, so the last is free and the loop ends on it.
dev-seed: migrate-dev
	@for i in 1 2 3 4 5 6; do \
		out=$$(KILN_WORKER_PERMITTED_SOURCE_ROOTS=$(CURDIR) \
			./bin/$(BINARY) build $(CURDIR) --config $(DEV_CONFIG) 2>&1) || \
			{ echo "$$out" | tail -20; exit 1; }; \
		echo "$$out" | grep -E '^(succeeded|no_changes|partial|over_budget|failed)' || true; \
		case "$$out" in *"nothing changed"*) break ;; esac; \
	done
	@echo "dev wiki seeded — start the server with 'make dev'"

# API + UI + in-process worker. Connectors may read anything under this repo.
dev: migrate-dev
	@echo "kiln dev on http://127.0.0.1:8080 (auth disabled, fake agent runner)"
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

# --- local Kubernetes -------------------------------------------------------
# A persistent single-machine instance on Docker Desktop's Kubernetes, with
# generation running through the Claude Code CLI rather than the Messages API.
# Unlike `make dev` this spends real money on every build; the run budget and
# page cap are what bound it.
#
#   ANTHROPIC_API_KEY=sk-ant-... make k8s-up
#
# See deploy/local-k8s/README.md for what it creates and how to point it at a
# source.

K8S_LOCAL := ./scripts/k8s-local.sh

k8s-up:
	@$(K8S_LOCAL) up

# Stops the workloads and keeps the volumes: the wiki is still there on the
# next `k8s-up`. `k8s-purge` is the one that deletes data.
k8s-down:
	@$(K8S_LOCAL) down

k8s-purge:
	@$(K8S_LOCAL) purge

k8s-status:
	@$(K8S_LOCAL) status

# make k8s-logs            -> the worker, where generation happens
# make k8s-logs C=api      -> the API
k8s-logs:
	@$(K8S_LOCAL) logs $(or $(C),worker)

k8s-token:
	@$(K8S_LOCAL) token

# The cluster's nodes cannot see this filesystem, so a local repository is
# copied into the sources volume rather than mounted:
#   make k8s-sync SRC=/path/to/repo
k8s-sync:
	@$(K8S_LOCAL) sync $(SRC)

# Discards the credential the worker refreshed for itself and re-seeds from the
# secret, for when a re-exported Claude Code session replaces a stale one.
k8s-reauth:
	@$(K8S_LOCAL) reauth

k8s-shell:
	@$(K8S_LOCAL) shell

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
