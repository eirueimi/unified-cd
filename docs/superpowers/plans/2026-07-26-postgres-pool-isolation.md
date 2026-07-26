# PostgreSQL Pool Isolation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Isolate API, background, advisory-lock, and SSE listener PostgreSQL connections so long-lived sessions cannot starve controller APIs.

**Architecture:** `store.Postgres` owns four `pgxpool.Pool` instances. Ordinary methods use the receiver's query pool, `BackgroundStore()` swaps that query pool for the background pool, advisory locks use the lock pool, and notification listeners use the listen pool. The command configures explicit limits and routes all background workers through the background store.

**Tech Stack:** Go, pgx v5/pgxpool, PostgreSQL 16, Docker Compose, testify.

## Global Constraints

- Default pool maxima are API `128`, background `32`, lock `16`, and listen `128`.
- Development PostgreSQL uses `max_connections=1000`.
- Invalid or non-positive pool environment values fall back to defaults.
- `/readyz` is the Docker controller healthcheck; `/healthz` remains liveness.
- Repository code, comments, logs, configuration, and documentation are English.

---

### Task 1: Isolate PostgreSQL pools

**Files:**
- Modify: `internal/store/postgres.go`
- Create: `internal/store/postgres_pool_test.go`

**Interfaces:**
- Produces: `type PostgresPoolConfig struct { APIMaxConns, BackgroundMaxConns, LockMaxConns, ListenMaxConns int32 }`
- Produces: `func DefaultPostgresPoolConfig() PostgresPoolConfig`
- Produces: `func NewPostgresWithPoolConfig(context.Context, string, PostgresPoolConfig) (*Postgres, error)`
- Produces: `func (*Postgres) BackgroundStore() Store`

- [ ] **Step 1: Write failing integration tests**

Create a real PostgreSQL test that constructs pools with limits `1/1/1/1`,
holds the single lock or listen connection, then verifies `Ping` and a simple
API-store operation complete before a 500 ms deadline. Add a test that holds
the background query pool and proves the API pool still responds.

- [ ] **Step 2: Run the tests and verify RED**

Run: `go test ./internal/store -run 'TestPostgresPoolIsolation|TestPostgresBackgroundStore' -count=1 -v`

Expected: compilation failure because `PostgresPoolConfig`,
`NewPostgresWithPoolConfig`, and `BackgroundStore` do not exist.

- [ ] **Step 3: Implement the four-pool owner and view**

Parse the DSN separately for each pool, assign its explicit `MaxConns`, close
already-created pools on constructor failure, and ping only the API pool.
Retain `pool` as the receiver's query pool to avoid rewriting all store methods.
Route `AcquireAdvisoryLock` to `lockPool` and `ListenForNotify` to
`listenPool`. Make `Close` close all owned pools once.

- [ ] **Step 4: Run focused and store tests**

Run:

```powershell
go test ./internal/store -run 'TestPostgresPoolIsolation|TestPostgresBackgroundStore' -count=1 -v
go test ./internal/store -count=1
```

Expected: PASS.

### Task 2: Configure limits and route background workers

**Files:**
- Modify: `cmd/controller/main.go`
- Modify: `cmd/controller/main_test.go`

**Interfaces:**
- Consumes: `store.DefaultPostgresPoolConfig`
- Consumes: `store.NewPostgresWithPoolConfig`
- Consumes: `(*store.Postgres).BackgroundStore`
- Produces: `func postgresPoolConfigFromEnv() (store.PostgresPoolConfig, []string)`

- [ ] **Step 1: Write failing environment tests**

Add table-driven tests proving the four defaults, positive overrides, and
fallback with one warning per invalid or non-positive value.

- [ ] **Step 2: Run the tests and verify RED**

Run: `go test ./cmd/controller -run TestPostgresPoolConfigFromEnv -count=1 -v`

Expected: compilation failure because `postgresPoolConfigFromEnv` is absent.

- [ ] **Step 3: Implement parsing and worker routing**

Build the pool config before store initialization, log collected warnings after
logger setup, construct with `NewPostgresWithPoolConfig`, and create one
instrumented API store plus one background store view. Pass the background view
to scheduler, cleanup, archiver, reaper, retention, Git resolver, and AppSource
reconciler goroutines. Keep HTTP, bootstrap, metrics, and token operations on
the API store.

- [ ] **Step 4: Run controller command tests**

Run: `go test ./cmd/controller ./internal/controller -count=1`

Expected: PASS.

### Task 3: Configure and document operations

**Files:**
- Modify: `docker-compose.yaml`
- Modify: `.env.example`
- Modify: `deployments/docker/docker-compose.yaml`
- Modify: `docs/configuration.md`
- Modify: `docs/operations.md`
- Modify: `docs/troubleshooting.md`

**Interfaces:**
- Consumes: the four environment names and defaults from Task 2.

- [ ] **Step 1: Update Compose configuration**

Set PostgreSQL `max_connections=1000`, expose the four pool variables on the
controller, and change the controller Docker healthcheck to `/readyz`.
Apply equivalent production Compose defaults without forcing an operator's
managed PostgreSQL server configuration.

- [ ] **Step 2: Update operator documentation**

Document pool purposes, defaults, multi-replica connection budgeting, the
`max_connections=1000` development setting, and the diagnostic distinction
between `/healthz` and `/readyz`. Add a troubleshooting entry for
`db unavailable` with pool-stat and `pg_stat_activity` checks.

- [ ] **Step 3: Validate configuration references**

Run:

```powershell
rg -n 'UNIFIED_DB_(API|BACKGROUND|LOCK|LISTEN)_MAX_CONNS|max_connections|readyz' .env.example docker-compose.yaml deployments/docker/docker-compose.yaml docs
```

Expected: all four variables are documented and both Compose files are
consistent with their intended deployment roles.

### Task 4: Verify and commit

**Files:**
- Modify only files listed in Tasks 1-3.

- [ ] **Step 1: Format**

Run: `gofmt -w internal/store/postgres.go internal/store/postgres_pool_test.go cmd/controller/main.go cmd/controller/main_test.go`

- [ ] **Step 2: Run focused regression tests**

Run:

```powershell
go test ./internal/store ./internal/controller ./cmd/controller -count=1
```

Expected: PASS.

- [ ] **Step 3: Run repository verification**

Run:

```powershell
go test ./... -count=1
go build ./...
git diff --check
```

Expected: all commands exit zero.

- [ ] **Step 4: Review and commit**

Review `git status --short` and `git diff`, verify no unrelated changes or
personal paths, then commit:

```powershell
git add internal/store/postgres.go internal/store/postgres_pool_test.go cmd/controller/main.go cmd/controller/main_test.go docker-compose.yaml .env.example deployments/docker/docker-compose.yaml docs/configuration.md docs/operations.md docs/troubleshooting.md docs/superpowers/specs/2026-07-26-postgres-pool-isolation-design.md docs/superpowers/plans/2026-07-26-postgres-pool-isolation.md
git commit -m "fix: isolate controller database pools"
```
