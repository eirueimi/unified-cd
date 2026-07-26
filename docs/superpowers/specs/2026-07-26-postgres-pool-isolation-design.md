# PostgreSQL Pool Isolation Design

## Problem

The controller currently shares one `pgxpool.Pool` between ordinary queries,
session-level advisory locks, and SSE `LISTEN` sessions. The pool defaults to
`runtime.NumCPU()` connections. A controller with twelve visible CPUs can
therefore deadlock when long-lived advisory locks and SSE listeners occupy all
twelve connections while lock-owning background workers wait for another
connection to perform their guarded work. `/healthz` remains healthy because it
does not access PostgreSQL, while `/readyz`, APIs, agent claims, and heartbeats
stall.

## Design

Create four independently bounded PostgreSQL pools:

| Pool | Default maximum | Consumers |
| --- | ---: | --- |
| API | 128 | HTTP handlers, authentication, agents, metrics, bootstrap |
| Background | 32 | Scheduler, reapers, archivers, retention, reconcilers |
| Lock | 16 | Session-level advisory locks only |
| Listen | 128 | SSE PostgreSQL `LISTEN` sessions only |

`Postgres` remains the concrete store implementation. Its primary query pool is
the API pool. `BackgroundStore()` returns a non-owning view whose primary query
pool is the background pool while sharing the lock and listen pools. The
controller server and startup/bootstrap paths use the API store; every
background worker receives the background view. Advisory-lock acquisition uses
only the lock pool, and notification listening uses only the listen pool.

The owning `Postgres` closes all four pools exactly once. A background view does
not own or close pools.

## Configuration

The controller accepts positive integer environment variables:

- `UNIFIED_DB_API_MAX_CONNS` (default `128`)
- `UNIFIED_DB_BACKGROUND_MAX_CONNS` (default `32`)
- `UNIFIED_DB_LOCK_MAX_CONNS` (default `16`)
- `UNIFIED_DB_LISTEN_MAX_CONNS` (default `128`)

Invalid or non-positive values fall back to the documented defaults and emit a
startup warning. The development PostgreSQL service sets
`max_connections=1000`, leaving headroom above one controller's maximum of 304
connections. Operators must budget all controller replicas plus migrations and
administrative access under the server limit.

## Availability

The Docker controller healthcheck uses `/readyz`, not `/healthz`, so a
controller that cannot obtain an API database connection is marked unhealthy.
`/healthz` remains the liveness endpoint for process monitoring.

## Verification

PostgreSQL integration tests prove that:

1. Exhausting the lock pool does not prevent an API query.
2. Exhausting the listen pool does not prevent an API query.
3. Exhausting the background query pool does not prevent an API query.
4. Background store queries use the configured background pool.

Controller unit tests prove environment defaults, overrides, and invalid-value
fallbacks. Existing store, controller, and command tests remain green. The
development stack is rebuilt and `/readyz` is checked after rollout.
