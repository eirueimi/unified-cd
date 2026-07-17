# Task 5 Report: One-Time Exchange, VM Refresh, Limiter, and Telemetry

## Commit

`feat(auth): exchange and rotate short-lived agent tokens`

## Implemented

- Added public VM credential endpoints outside human `ServerAuth`:
  - `POST /api/v1/agents/enroll` exchanges only a parsed `uce_` bearer token.
  - `POST /api/v1/agents/token/refresh` accepts only a parsed `ucr_` bearer token.
- Successful exchange returns a one-hour access token and a 30-day refresh
  token. The initial credentials share a new family UUID at generation 1;
  refresh rotation preserves the family and advances both credentials.
- Kept the five-minute refresh overlap and replay-family revocation in the
  existing transactional PostgreSQL rotation path. Access credentials cannot
  call the refresh endpoint and no access-renewal route was added.
- Added a per-controller LRU enrollment limiter (4096 entries, burst 5,
  one token per six seconds) keyed by provider, normalized remote IP, and
  optional policy. The key is never exported as a metric label.
- Added bounded agent-auth and legacy-auth Prometheus metrics, direct
  lifecycle audit calls using non-secret resource IDs, bounded policy events
  for principal/path, principal/body, and principal/run mismatches, and a
  4096-entry five-minute LRU throttle for `TouchAgentCredential`.

## Files

- `internal/api/agent_auth.go`
- `internal/controller/agent_auth.go`
- `internal/controller/agent_guard.go`
- `internal/controller/api_agent.go`
- `internal/controller/api_agent_enrollment.go`
- `internal/controller/api_agent_enrollment_test.go`
- `internal/controller/credential_touch_limiter.go`
- `internal/controller/enrollment_limiter.go`
- `internal/controller/enrollment_limiter_test.go`
- `internal/controller/server.go`
- `internal/metrics/metrics.go`
- `internal/metrics/metrics_test.go`
- `internal/store/postgres_agent_auth.go`

## Test Evidence

- RED observed with:

  `go test ./internal/controller ./internal/metrics -run 'TestAgentEnroll|TestAgentRefresh|TestAgentEnrollmentHA|TestEnrollmentLimiter|TestAgentAuthEvents' -count=1`

  It failed as expected before implementation because the new response DTO,
  limiter, and metrics recorder were undefined.
- After implementation, the metrics package passed the same focused command.
  The controller package initially failed on a missing receiver conversion;
  that conversion was then implemented and formatting/diff checks completed.
- The final elevated controller test attempt exceeded the parent-requested
  wait budget and was interrupted, so no final controller or PostgreSQL green
  claim is made in this report.
- `gofmt` was run on all changed Go files and `git diff --check` completed
  with exit code 0.

## Remaining Verification / Concerns

- Parent should run the brief's final command in an environment where the
  shared Go build cache and PostgreSQL test service are available:

  `go test -race ./internal/controller ./internal/metrics -run 'AgentEnroll|AgentRefresh|EnrollmentLimiter|AgentAuth' -count=1`

- The new controller test coverage exercises successful exchange, access-token
  rejection by refresh, rotation, repeated enrollment rejection, limiter
  refill/bounds, and metrics label folding. The pre-existing PostgreSQL
  credential tests cover overlap retry, replay-family revocation, HA sharing,
  and concurrent single-use consumption; the parent verification above is
  needed to re-confirm these integration cases after endpoint wiring.
