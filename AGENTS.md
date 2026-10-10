# Working on kiln

Notes for coding agents (and people) changing this repository. What the
system is and why it is shaped this way: [docs/architecture.md](docs/architecture.md).
What to build next, with a verification line per milestone:
[ROADMAP.md](ROADMAP.md). Pick work from there, and update a milestone's
status line in the same PR that ships it.

## Before you push

CI runs four checks. Each one below fails PRs that were green locally.

| Check | Run locally | Trap |
| --- | --- | --- |
| Lint | `make lint` | Runs golangci-lint **v2.12.2** with the local toolchain. A globally installed `golangci-lint` is often v1 and cannot read `.golangci.yml`, so don't use it. gofmt and goimports are linters here: adding a comment inside a struct splits its alignment group, so re-run `make fmt`. |
| Tidy | `go mod tidy && git diff --exit-code go.mod go.sum` | CI fails on any diff. |
| Hermetic tests | `make test` | No network, no database, no cost. |
| Integration + coverage | `make cover` | The **78.0%** floor is measured with `-coverpkg=./...`, so a plain `go test` passes while coverage is under it. A new package, or a cobra wrapper under `cmd/kiln` that no test calls, drags the total down. |

Lint rules that bite most often: `errorlint` (use `errors.Is`/`errors.As`,
never `==` against `pgx.ErrNoRows` or a sentinel), `errcheck` (write
`_ = x.Close()` for best-effort cleanup), and staticcheck SA5001
(`defer x.Close()` placed before the error check on the call that returned `x`).

## Tests and the database

- `make db-up` starts Postgres 16 as the `kiln-test` container on
  `localhost:55432`. Integration tests read
  `KILN_TEST_DATABASE_URL=postgres://kiln:kiln@localhost:55432/kiln?sslmode=disable`
  and skip themselves when it is unset.
- **Always pass `-p 1` when testing more than one package against the
  database.** `internal/api` and `internal/store` each drop and recreate the
  schema on the shared database, so parallel package runs cause failures
  that are not real.
- Dev state lives in a separate `kiln_dev` database on the same container
  (`make dev-db`), so test runs cannot wipe it.

## Running it

- `make dev-up` goes from nothing to a served wiki on :8080 with the fake
  runner. It costs nothing and runs every pipeline stage.
- After adding a migration: `make migrate-dev`.
- `make dev-cli`, `make k8s-up`, and `make test-claude` **spend real
  money**. Don't run them unless the person you are working for asked.

## Conventions

- Deletion and destruction are review-queue questions for a human, never
  side effects (ROADMAP principle 3). That includes anything an agent or
  an MCP tool can trigger.
- The content-hash gate is the correctness guarantee. An optimization that
  can lose a change must fall back to a wider rebuild, never to a stale page.
- Any text that enters a prompt and wasn't written by the program goes
  inside a fence (`fenceAs` in `internal/jobs/prompts.go`), never as bare
  markdown. That covers source material, model output, and anything a
  workspace member wrote.
- Comments explain why, at the density of the surrounding code. When code
  changes, fix any comment, README line, or doc that now contradicts it in
  the same PR.
