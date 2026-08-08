BINARY  := kiln
PKG     := github.com/daiwa-zou/kiln
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X '$(PKG)/internal/observability.Version=$(VERSION)'

TEST_DB_URL := postgres://kiln:kiln@localhost:55432/kiln?sslmode=disable

# The local-hosting configuration: fake agent runner, filesystem blobs, and a
# kiln_dev database kept apart from the one the integration tests drop. Defined
# up here because the admin targets below use it too, not just the dev section.
DEV_CONFIG := config.dev.toml

# Pinned to match .github/workflows/ci.yml. Bump both together.
GOLANGCI := github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2

.PHONY: help all build clean fmt tidy lint vulncheck \
        test test-verbose test-integration test-claude cover \
        db-up db-down migrate doctor token status \
        dev dev-cli dev-up dev-seed dev-db migrate-dev dev-build dev-clean \
        image compose-up compose-down manifests helm-lint \
        k8s-up k8s-down k8s-purge k8s-status k8s-logs k8s-token \
        k8s-sync k8s-reauth k8s-shell k8s-doctor k8s-migrate

# Default target. Forty-odd targets across four environments is more than
# anyone remembers, and the answer to "how do I run this locally" should not be
# "read the Makefile". Targets document themselves with a `##` comment; the
# groups below are the four places kiln runs.
.DEFAULT_GOAL := help

help:
	@echo "kiln — make targets"
	@awk 'BEGIN {FS = ":.*?## "} \
		/^# ==/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 6) } \
		/^[a-zA-Z0-9_-]+:.*?## / { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 }' \
		$(MAKEFILE_LIST)
	@echo ""

# == Build and check

all: fmt test build ## format, test, and build

build: ## compile bin/kiln
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/kiln

# Hermetic: no network, no database. Integration tests skip themselves.
test: ## unit tests (no database needed)
	go test ./...

test-verbose: ## unit tests, verbose and race-enabled
	go test -v -race ./...

# Requires db-up. Exercises the schema against a real Postgres.
#
# -p 1 is load-bearing: internal/api and internal/store both DROP SCHEMA public
# on the single shared database, so parallel package binaries race and fail
# with spurious "relation ... does not exist" errors.
test-integration: db-up ## full suite against a real Postgres
	KILN_TEST_DATABASE_URL="$(TEST_DB_URL)" go test ./... -count=1 -p 1

# Coverage profile plus the badge CI commits. Needs the database for the same
# reason CI measures coverage in the integration job: without it internal/store
# reports ~5% and the badge understates the project by a wide margin.
cover: db-up ## coverage profile and badge (matches the CI gate)
	KILN_TEST_DATABASE_URL="$(TEST_DB_URL)" go test -race ./... -count=1 -p 1 -covermode=atomic -coverpkg=./... -coverprofile=coverage.out
	@go tool cover -func=coverage.out | tail -1
	@./scripts/coverage-badge.sh coverage.out .github/badges/coverage.svg

db-up: ## start the test/dev Postgres on :55432
	@docker inspect kiln-test >/dev/null 2>&1 || \
		docker run -d --name kiln-test \
			-e POSTGRES_PASSWORD=kiln -e POSTGRES_USER=kiln -e POSTGRES_DB=kiln \
			-p 55432:5432 postgres:16-alpine >/dev/null
	@docker start kiln-test >/dev/null 2>&1 || true
	@until docker exec kiln-test pg_isready -U kiln >/dev/null 2>&1; do sleep 0.5; done
	@echo "postgres ready on :55432"

db-down: ## remove the test/dev Postgres container
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
test-claude: ## end-to-end against a real claude CLI (spends money)
	KILN_TEST_CLAUDE=1 go test ./internal/jobs/ -count=1 -v -run TestCLIRunnerAgainstRealClaude

# Pinned so local runs match CI exactly. `go run` caches the build, so only
# the first invocation after a version bump is slow.
lint: ## golangci-lint, pinned to the CI version
	go run $(GOLANGCI) run

vulncheck: ## scan dependencies for known vulnerabilities
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

fmt: ## gofmt every package
	go fmt ./...

tidy: ## prune and sync go.mod
	go mod tidy

# == Operating a local instance

# Against the default configuration (config.toml), which is what a real
# deployment on this machine would use. `make migrate-dev` is the dev-database
# equivalent, and `make k8s-migrate` the in-cluster one.
migrate: build ## apply migrations using the default config
	./bin/$(BINARY) admin migrate

# Configuration, database, schema, and -- when the CLI runner is selected --
# whether the claude session is actually usable. Free: no model call is made.
# `make doctor PROBE=1` adds the one check that costs money, a real generation
# round trip, which is the only conclusive answer about the credential.
doctor: build ## check config, database, schema, and CLI credential
	./bin/$(BINARY) admin doctor --config $(DEV_CONFIG) $(if $(PROBE),--probe,)

# The dev server disables auth, so this is for exercising the API as a client
# would reach it in a real deployment. Mirrors `make k8s-token`.
#   make token LOGIN=you
token: build migrate-dev ## mint an API token against the dev database
	./bin/$(BINARY) admin token create --config $(DEV_CONFIG) \
		--login $(or $(LOGIN),dev) --scopes read,write,admin --admin

# The local counterpart to `make k8s-status`. A dev server runs in the
# foreground, so "is it up" is a question about the port and the database rather
# than about pods.
status: ## is the local stack up: database, schema, server
	@docker inspect -f 'postgres   {{.State.Status}}' kiln-test 2>/dev/null || echo "postgres   not created (make db-up)"
	@if curl -fsS http://127.0.0.1:8080/readyz >/dev/null 2>&1; then \
		echo "server     ready on http://127.0.0.1:8080"; \
	else \
		echo "server     not responding on :8080 (make dev)"; \
	fi

# == Local development environment
# Everything below runs against config.dev.toml (DEV_CONFIG, defined at the
# top): the fake agent runner (full pipeline, zero LLM cost, no API key) and a
# dedicated kiln_dev database so `make test-integration` (which drops the `kiln`
# database's schema) can never wipe dev state.

dev-db: db-up ## create the kiln_dev database
	@docker exec kiln-test psql -U kiln -tc "SELECT 1 FROM pg_database WHERE datname='kiln_dev'" | grep -q 1 || \
		docker exec kiln-test psql -U kiln -c "CREATE DATABASE kiln_dev"
	@echo "kiln_dev ready"

migrate-dev: build dev-db ## apply migrations to kiln_dev
	./bin/$(BINARY) admin migrate --config $(DEV_CONFIG)

# The one-command entry point: Postgres, schema, a wiki with content already in
# it, then the server. Everything a deployment has except object storage, which
# config.dev.toml points at the filesystem so no MinIO is needed.
#
# `make dev` is the same thing without the seeding, for restarting the server
# against a wiki that already exists.
dev-up: dev-seed dev ## seed a wiki and serve it (start here)

# Builds this repository into the dev wiki until it converges. The loop is not
# belt-and-braces: a run stops at agent.max_pages_per_run and defers the rest,
# so a repository with more units than the cap needs several runs to finish.
# Each one is hash-gated, so the last is free and the loop ends on it.
dev-seed: migrate-dev ## build this repo into the dev wiki until it converges
	@for i in 1 2 3 4 5 6; do \
		out=$$(KILN_WORKER_PERMITTED_SOURCE_ROOTS=$(CURDIR) \
			./bin/$(BINARY) build $(CURDIR) --config $(DEV_CONFIG) 2>&1) || \
			{ echo "$$out" | tail -20; exit 1; }; \
		echo "$$out" | grep -E '^(succeeded|no_changes|partial|over_budget|failed)' || true; \
		case "$$out" in *"nothing changed"*) break ;; esac; \
	done
	@echo "dev wiki seeded — start the server with 'make dev'"

# API + UI + in-process worker. Connectors may read anything under this repo.
dev: migrate-dev ## serve API + UI + worker on :8080
	@echo "kiln dev on http://127.0.0.1:8080 (auth disabled, fake agent runner)"
	KILN_WORKER_PERMITTED_SOURCE_ROOTS=$(CURDIR) \
		./bin/$(BINARY) serve --with-worker --config $(DEV_CONFIG)

# `make dev` with everything a real deployment has, on one machine:
#
#   - the real Claude Code CLI instead of the fake runner, so pages are
#     generated rather than stubbed. Needs a host login (`claude auth login`);
#     `make doctor` says whether kiln can see it.
#   - a master key, so connectors that carry credentials work at all. Generated
#     once into .dev/ (gitignored) and reused, because credentials sealed under
#     one key cannot be opened with another.
#   - budgets sized for real documents. config.dev.toml's $$0.05 analyze ceiling
#     is sized for the fake runner's $$0.01 calls, and a single large PDF blows
#     through it on its first call.
#
# Unlike `make dev`, this spends real money on every build.
MASTER_KEY_FILE := .dev/master.key

$(MASTER_KEY_FILE):
	@mkdir -p $(dir $@)
	@openssl rand -base64 32 > $@ && chmod 600 $@
	@echo "generated $@ (gitignored; keep it -- sealed credentials need it)"

dev-cli: migrate-dev $(MASTER_KEY_FILE) ## like `make dev` but with the real CLI runner (spends money)
	@echo "kiln on http://127.0.0.1:8080 (auth disabled, real claude CLI, real spend)"
	KILN_MASTER_KEY="$$(cat $(MASTER_KEY_FILE))" \
	KILN_WORKER_PERMITTED_SOURCE_ROOTS=$(CURDIR) \
	KILN_AGENT_RUNNER=cli \
	KILN_AGENT_ANALYZE_BUDGET_USD=$(or $(ANALYZE_USD),2.00) \
	KILN_AGENT_PAGE_BUDGET_USD=$(or $(PAGE_USD),2.00) \
	KILN_AGENT_RUN_BUDGET_USD=$(or $(RUN_USD),10.00) \
		./bin/$(BINARY) serve --with-worker --config $(DEV_CONFIG)

# One-shot build of kiln itself into the dev wiki through the full pipeline.
# Bootstraps the org/workspace/wiki chain on first run; repeating it is free
# (the hash gate skips unchanged sources).
dev-build: migrate-dev ## one-shot build of this repo into the dev wiki
	./bin/$(BINARY) build $(CURDIR) --config $(DEV_CONFIG)

dev-clean: ## drop kiln_dev and the local blob store
	@docker exec kiln-test psql -U kiln -c "DROP DATABASE IF EXISTS kiln_dev" 2>/dev/null || true
	@rm -rf .dev
	@echo "dev state removed"

# == Deployment

# The same image CI publishes, built locally for a smoke test or an air-gapped
# registry push.
image: ## build the container image
	docker build -t kiln:$(VERSION) --build-arg VERSION=$(VERSION) .

compose-up: ## bring up the docker compose stack
	docker compose up -d --build
	@echo "kiln at http://localhost:8080 — mint a token:"
	@echo "  docker compose exec api kiln admin token create --login you --scopes read,write,admin --admin"

compose-down: ## tear down the compose stack and its volumes
	docker compose down -v

# == Local Kubernetes
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

k8s-up: ## deploy to Docker Desktop Kubernetes (spends money)
	@$(K8S_LOCAL) up

# Stops the workloads and keeps the volumes: the wiki is still there on the
# next `k8s-up`. `k8s-purge` is the one that deletes data.
k8s-down: ## stop the workloads, keep the data
	@$(K8S_LOCAL) down

k8s-purge: ## delete the namespace and every volume in it
	@$(K8S_LOCAL) purge

k8s-status: ## pods, services, and volumes
	@$(K8S_LOCAL) status

# make k8s-logs            -> the worker, where generation happens
# make k8s-logs C=api      -> the API
k8s-logs: ## follow logs (C=api|worker|postgres)
	@$(K8S_LOCAL) logs $(or $(C),worker)

k8s-token: ## mint an API token in the cluster
	@$(K8S_LOCAL) token

# The in-cluster counterparts of `make doctor` and `make migrate`. Both run
# inside a pod: the configuration is in the pod's environment and the database
# is only reachable from inside the namespace.
k8s-doctor: ## check config, database, schema, and CLI credential in-cluster
	@$(K8S_LOCAL) doctor $(if $(PROBE),--probe,)

k8s-migrate: ## apply migrations to a cluster already running
	@$(K8S_LOCAL) migrate

# The cluster's nodes cannot see this filesystem, so a local repository is
# copied into the sources volume rather than mounted:
#   make k8s-sync SRC=/path/to/repo
k8s-sync: ## copy a local directory into the sources volume (SRC=path)
	@$(K8S_LOCAL) sync $(SRC)

# Discards the credential the worker refreshed for itself and re-seeds from the
# secret, for when a re-exported Claude Code session replaces a stale one.
k8s-reauth: ## re-seed the worker's Claude credential from the secret
	@$(K8S_LOCAL) reauth

k8s-shell: ## a shell in the worker pod
	@$(K8S_LOCAL) shell

# == Charts and housekeeping

helm-lint: ## lint the Helm chart
	helm lint deploy/helm/kiln --set secrets.existingSecret=kiln-secrets

# Renders the chart to plain YAML for GitOps repositories and Kustomize
# overlays. Deliberately gitignored: a generated manifest committed beside its
# generator drifts, and a stale one still applies cleanly.
manifests: ## render the chart to plain YAML for GitOps
	@mkdir -p deploy/kubernetes
	helm template kiln deploy/helm/kiln \
		--namespace kiln \
		--set secrets.existingSecret=kiln-secrets \
		--set config.database.host=postgres.kiln.svc.cluster.local \
		> deploy/kubernetes/kiln.yaml
	@echo "wrote deploy/kubernetes/kiln.yaml"

clean: ## remove build artifacts
	rm -rf bin dist
