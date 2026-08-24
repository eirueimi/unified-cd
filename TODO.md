# Active Backlog

This file contains active work only. Completed history is available in Git.
Last audited: 2026-07-26 against `1e46459`.

## Critical

### 3. Reject invalid conditional expressions and align the documented syntax

- **Status:** Partial
- **Impact:** Invalid or non-boolean `if` expressions currently evaluate as true, so gated steps can run unexpectedly, while the variable reference table documents Go-template syntax that the CEL evaluator does not accept.
- **Evidence:** `docs/user-guide/writing-jobs/steps.md` lists `{{ .Params.NAME }}` and `{{ .Steps.NAME.Outputs.KEY }}` for conditions, while `internal/agent/orchestrator.go` passes `step.If` to `internal/dsl/condition.go` `EvalCondition`; `internal/dsl/condition_test.go` `TestEvalCondition_InvalidExpr` and `TestEvalCondition_NonBoolResult` require the current fail-open result.
- **Done when:** The condition reference consistently documents the accepted CEL syntax, invalid or non-boolean expressions cannot execute the gated step, and regression tests cover parameter references, step-output references, compile errors, evaluation errors, and non-boolean results.

### 13. Verify that the default Kubernetes sidecar image is anonymously pullable

- **Status:** Needs verification
- **Impact:** If the digest-pinned default is unavailable without registry credentials, installations that omit `sidecarImage` cannot start the sidecar and Kubernetes jobs remain blocked.
- **Evidence:** `internal/k8sagent/config.go` `defaultSidecarImage` supplies the digest-pinned default consumed by the rendered `install.yaml` bundle (built from `manifests/install` and published as a release asset); `internal/k8sagent/config_test.go` `TestDefaultSidecarImageIsDigestPinned` checks only its syntax, while `docs/reference/configuration.md`, `docs/operator-manual/kubernetes-integration.md`, and `docs/troubleshooting/artifacts-and-storage.md` still describe a mutable `:latest` default.
- **Done when:** A credential-isolated Docker pull of the image derived from `defaultSidecarImage` exits zero within 120 seconds, and a temporary Kubernetes namespace with a ServiceAccount that has no image pull secrets starts that exact image with pull policy `Always` and reaches `Ready` within 120 seconds with no pull waiting reason and an image ID ending in the configured digest; every external Docker and Kubernetes setup, pull, wait, inspection, and namespace-deletion subprocess is bounded, safe-target checks guard cleanup, any cleanup failure fails the experiment and is combined with rather than hidden by any primary failure, and all three documents state the verified digest-derived default.

### 33c. Prevent AppSource repository credentials from reaching API and UI output

- **Status:** Partial
- **Impact:** Credentials embedded in an AppSource repository URL can be stored and returned by the API, then displayed in the Web UI or other exported output.
- **Evidence:** `internal/controller/appsource_reconciler.go` redacts credentials in reconciliation errors, but `internal/dsl/gitref.go` `ValidateGitRepoURL` accepts URL userinfo, `internal/controller/api_appsources.go` returns `spec.repoURL`, and `web/src/routes/AppSourceList.svelte` renders the returned URL.
- **Done when:** New AppSource apply rejects or removes URL userinfo, list, get, UI, and export output never expose credentials, already-persisted credential-bearing URLs are migrated or safely redacted, `GitCredential` authentication still works, and regression coverage exercises every input and output path.

## High

### 9c. Eliminate Windows cancellation escape paths for child processes

- **Status:** Partial
- **Impact:** A process that spawns before Job Object assignment, or runs after assignment failure, can outlive a cancelled step and continue consuming resources or performing work.
- **Evidence:** `internal/agent/exec_windows.go` `assignJob` enables `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`, and `internal/agent/runner_test.go` `TestRunStep_CancelKillsGrandchild` covers the normal path; `internal/agent/exec_tree.go` `runTreeKilled` assigns only after `cmd.Start` and retains a single-process fallback when assignment fails.
- **Done when:** Cancelling a Windows step leaves no descendant process alive even when the child spawns immediately or Job Object assignment fails, the step reports `Cancelled`, and Windows regression tests deterministically cover both escape paths.

### 12. Define and align full database-loss agent recovery behavior

- **Status:** Partial
- **Impact:** After complete controller database loss, existing agents cannot authenticate to claim work until they are re-enrolled, despite operations guidance also promising automatic inventory recovery.
- **Evidence:** `internal/controller/api_agent.go` `handleAgentClaim` calls `internal/store/postgres.go` `UpsertAgentOnClaim`, but `internal/controller/agent_auth.go` authenticates first through `GetAgentCredentialForAuth`; `internal/controller/api_agent_test.go` `TestAgentAPI_Claim_UpsertsUnregisteredAgent` covers only a missing inventory row, and `docs/operator-manual/operations.md` states conflicting full-loss recovery expectations.
- **Done when:** The supported full-database-loss workflow has one explicit runtime and documentation contract, and an integration test starting from absent inventory and credential records proves either bounded automatic recovery or the documented re-enrollment requirement before the agent reappears and can claim work.

### 33d. Prevent late step reports after run finalization

- **Status:** Partial
- **Impact:** A step report racing with run finalization can write stale step state beneath an already-terminal run, making persisted and displayed execution state inconsistent.
- **Evidence:** Finish writes use compare-and-set finalization, but `internal/controller/api_agent.go` `handleAgentStepReport` checks run status separately from `internal/store/postgres.go` `UpsertStepReport`, leaving finalization between the read and write unguarded.
- **Done when:** Once a run becomes terminal, no concurrent or later step report can change its step state, and a deterministic store or controller concurrency test forces finalization between validation and write and proves the late report is rejected without mutation.

### 33e. Verify that the published quickstart controller image passes its health check

- **Status:** Needs verification
- **Impact:** If the published default controller image lacks the `wget` used by the quickstart health check, the controller remains unhealthy and dependent quickstart services do not start.
- **Evidence:** `deployments/docker/docker-compose.yaml` defines the controller image and `wget` health check, while `docker/controller.Dockerfile` uses an Alpine runtime that supplies BusyBox `wget`; source inspection cannot establish the contents of the published default image.
- **Done when:** A credential-isolated pull of the image derived from the Compose controller image expression completes within 120 seconds, then a uniquely named Compose project starts PostgreSQL, Garage, and the controller without building and reports the controller `healthy` within 120 seconds; every Docker subprocess is bounded, prior version environment state is restored, project containers, volumes, and orphans plus the isolated Docker configuration are always cleaned up, and any failure is corrected and the experiment repeated.

## Medium

### 8. Verify that artifact downloads preserve configured step metadata

- **Status:** Needs verification
- **Impact:** Download-artifact progress can be attributed to a fallback step name or wrong stage in run status, making the UI and CLI misleading even if the transfer succeeds.
- **Evidence:** `internal/agent/orchestrator.go` `executeDownloadArtifact` reports `step.DisplayName()` and `step.StageIndex`, but `internal/agent/agent_test.go` `TestAgent_UploadArtifact_RelativePath` verifies those fields only for uploads.
- **Done when:** An exact `executeDownloadArtifact` test captures its Running and terminal reports and proves both use the configured download display name and configured stage index.

### 10. Verify that pre-cancellation logs remain visible in the Web UI

- **Status:** Needs verification
- **Impact:** Users investigating a cancelled run may lose the historical log lines needed to understand why it was cancelled or what completed first.
- **Evidence:** `web/src/routes/RunDetail.svelte` opens one event stream and `internal/controller/sse.go` `handleRunEvents` replays stored logs before terminal status, but `web/src/routes/RunDetail.test.js` uses a Running run with an empty stream and `internal/controller/api_runs_test.go` `TestAPI_RunEvents_SSE_ReceivesExistingLogs` does not exercise Web rendering.
- **Done when:** A component or browser test loads a pre-cancelled run whose event stream backfills known historical lines and then closes with terminal status, and proves those exact lines remain rendered after closure with only one event connection.

### 24. Record run replay operations in the audit log

- **Status:** Partial
- **Impact:** A human replay of a run mutates system state without recording who performed it or which source run was replayed, leaving the documented audit trail incomplete.
- **Evidence:** `internal/controller/server.go` registers `POST /api/v1/runs/{id}/replay`, while `internal/controller/audit.go` `auditActionTable` has no replay classification; `docs/operator-manual/audit.md` claims coverage for state-changing human operations.
- **Done when:** Replay produces a `run.replay` audit record containing the actor, source run ID, method, path, and result status without request or response secret material; an integration test proves the record, `docs/operator-manual/audit.md` lists the action and route coverage, and the stale generic-fallback comment in `internal/controller/audit.go` matches the explicit allow-list behavior.

### 35b. Exercise AppSource migration 003 through first synchronization

- **Status:** Partial
- **Impact:** Upgrade regressions in bare-name backfill, first-sync qualification, history preservation, idempotency, or prune safety can pass despite current reconciler coverage.
- **Evidence:** `internal/controller/appsource_reconciler_test.go` seeds post-migration bare-name state directly instead of running `internal/store/migrations/003_appsource_managed_resources.up.sql` through the production migration runner; store tests cover `RenameJob` separately.
- **Done when:** An integration test creates a genuine pre-003 schema and state, runs migration 003 through the production migration runner, verifies the deliberate bare-name backfill, performs the first sync, and proves qualification, history preservation, repeat-sync idempotency, and prune safety end to end.

### 35c. Verify exact matrix-variant output aggregation

- **Status:** Partial
- **Impact:** Variant outputs can collapse or include the wrong combinations while current tests still pass, producing incorrect promoted job outputs.
- **Evidence:** `test/e2e/matrix_smoke_test.go` `TestMatrixSmoke` substring-checks only one included and one excluded output, while `internal/store/postgres_outputs_test.go` `TestStepOutputs_VariantKeyed` accepts either value returned by variant-unaware `GetStepOutputs`.
- **Done when:** Coverage JSON-decodes and exactly compares all three expected promoted key/value pairs, rejects extras and the excluded combination, and directly verifies the count and identity of variant-keyed rows through storage inspection or a variant-aware getter.

### 35d. Complete human and cross-agent artifact-download router coverage

- **Status:** Partial
- **Impact:** Owner-agent downloads already traverse the real router, but successful human-viewer access and cross-run access by a distinct enrolled agent remain unproven, so principal or router regressions can still violate the documented download contract.
- **Evidence:** `internal/controller/api_artifacts_test.go` `TestArtifact_UploadDownload_RoundTrip` sends an owner enrolled-agent download through `s.Router().ServeHTTP`, requires HTTP 200, and compares the exact artifact bytes; `TestArtifactDownload_RejectsNoAuth` sends an unauthenticated request through `s.r.ServeHTTP` and requires HTTP 401.
- **Done when:** Router-level tests seed exact artifact bytes and prove byte-for-byte successful downloads for a human viewer and a distinct non-owner enrolled agent under the documented cross-run contract.

### 35e. Assert HTTP methods for every CLI request constructor

- **Status:** Partial
- **Impact:** CLI tests can miss GET and POST regressions when the path, body, and output remain plausible, causing commands to call the right endpoint with the wrong method.
- **Evidence:** `internal/cli/apply_test.go` `captureTransport` records methods and selected mutation tests assert them, but most tests under `internal/cli/` still verify only paths, bodies, or output.
- **Done when:** A table-driven CLI contract covers every current request constructor and exact method-path pair, with command-specific tests retaining body, authorization, and response assertions where applicable.

## Low

No active items.
