# Active Backlog Audit and Reorganization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the mixed historical `TODO.md` with an evidence-backed, English-only active backlog that contains only Open, Partial, or Needs verification work.

**Architecture:** Use a temporary, tracked audit matrix to account for every legacy heading, embedded follow-up, unnumbered Low item, and contextual section. Audit the matrix in reviewable waves against current code, tests, documentation, and history; then generate the final active backlog from retained rows, verify one-to-one accounting and formatting, and remove the temporary matrix from the final tree.

**Tech Stack:** Markdown, Git, ripgrep, PowerShell, Go tests, Vitest, Vite, GitHub CI

## Global Constraints

- The audit baseline is `main` commit `1e46459` from 2026-07-26.
- This is documentation-only work; do not implement any retained backlog item or change production behavior.
- All repository text, documentation, commit messages, and review artifacts must be English-only.
- Do not include personally identifiable information or machine-specific home-directory paths.
- Verify every legacy claim against the current implementation, tests, documentation, and relevant history; legacy completion annotations are not evidence by themselves.
- Use exactly these outcomes: `Resolved`, `Accepted behavior`, `Environment-only`, `Open`, `Partial`, or `Needs verification`.
- Only `Open`, `Partial`, and `Needs verification` entries may appear in the final active backlog.
- Preserve legacy numeric and lettered IDs; assign `L1`, `L2`, and so on to retained unnumbered Low items.
- Consolidate duplicate root causes under the clearest existing ID and record every merged ID in a `Legacy IDs` line.
- Every retained entry must contain `Status`, `Impact`, `Evidence`, and `Done when`.
- Completed history remains in Git; do not add a completed-work archive.
- Remove the temporary audit matrix before the final branch is offered for integration.
- Do not edit application code, examples, templates, or user documentation unless a live reference to the reorganized backlog would otherwise break.

## File Map

- Create temporarily, then delete: `.todo-audit-matrix.md` — complete candidate inventory, evidence, outcome, and final-action ledger used for review gates.
- Modify: `TODO.md` — final English-only active backlog.
- Modify only if a live link is found: the exact referring Markdown file under `README.md`, `docs/`, `examples/`, `templates/`, `manifests/`, or `.github/`.
- Preserve: `docs/superpowers/specs/2026-07-26-active-backlog-audit-design.md` — approved design authority.
- Preserve: `docs/superpowers/plans/2026-07-26-active-backlog-audit.md` — this execution plan.

## Known Baseline Verification State

The exact base commit passed all five GitHub CI jobs. A local Windows run of
`go test -buildvcs=false ./...` on the unchanged base had these environment or
platform-specific failures:

1. `cmd/ucd-sh TestCLI_EndToEnd`: nested Go build rejected the worktree as an unsafe VCS directory.
2. `internal/agent TestRunStepWithShell_CredentialsNotInheritedByChild`: Windows `bash.exe` selection did not propagate the test environment.
3. `internal/cli TestLogsFollow_StopsPromptlyOnContextCancel`: local timing/context deadline.
4. `internal/shim` corpus test using `cat`: executable unavailable in `PATH`.
5. `internal/shim` corpus test using `ls`: executable unavailable in `PATH`.

These known failures may be recorded as unchanged local baseline evidence, but
they do not replace the requirement for a green GitHub CI run before
integration. Any additional failure is a regression and must be investigated.

---

### Task 1: Establish the Complete Audit Ledger

**Files:**
- Create: `.todo-audit-matrix.md`
- Read: `TODO.md`
- Read: `docs/superpowers/specs/2026-07-26-active-backlog-audit-design.md`

**Interfaces:**
- Consumes: the 47 legacy numbered or lettered headings, four unnumbered Low bullets, embedded follow-ups, and three contextual sections in `TODO.md`.
- Produces: a matrix in which every candidate has one stable audit key, one assigned audit wave, and explicit `Audit required` values ready to be replaced.

- [ ] **Step 1: Confirm the legacy heading inventory**

Run:

```powershell
Get-Content -Encoding utf8 TODO.md |
  Select-String -Pattern '^## |^### ' |
  ForEach-Object { '{0}:{1}' -f $_.LineNumber, $_.Line }
```

Expected: 47 numbered or lettered issue headings are present:

```text
1 2 3 4 5 6 7 8 9 9c 10 11 12 A
13 14 15 16 21 17 18 19 20b 20
22 23 25 26 27 28 29 30 31 32 33 34 35 36
37 38 39 40 41 42 43 44 24
```

Also confirm the Low section contains exactly four unnumbered bullets and that
the file contains the Kubernetes validation note, verified-healthy section,
and general validation-environment section.

- [ ] **Step 2: Create the temporary matrix with the exact schema**

Create `.todo-audit-matrix.md` with this header and decision rules:

```markdown
# Active Backlog Audit Matrix

Audit baseline: `1e46459` (2026-07-26)

This is a temporary review artifact. Every row must be decided before the
active backlog is rewritten, and this file must be removed from the final
tree.

## Decision Rules

- Resolved: current implementation and tests cover the legacy problem.
- Accepted behavior: current behavior is intentional and documented.
- Environment-only: the observation belongs to a historical validation setup.
- Open: the problematic behavior is still present.
- Partial: a concrete part remains after verified fixes.
- Needs verification: static evidence is insufficient; the retained task must
  state an exact experiment and success condition.

## Candidate Ledger

| Key | Legacy scope | Wave | Outcome | Evidence | Final action |
|---|---|---:|---|---|---|
```

Append one row for every key below. Set `Outcome` to `Audit required`,
`Evidence` to `Current-tree inspection required`, and `Final action` to
`Decision required` when the row is first created.

```text
1 Step failure or skip can wedge a run
2 Required/default parameter enforcement
3 Conditional syntax mismatch and fail-open behavior
4 Standard-agent artifact upload relative paths
5 Approval visibility in run show
6 parallel-steps example apply validity
7 CLI flag spelling in examples and docs
8 Artifact step display name
9 Workspace reuse between runs
9c Windows cancellation and orphaned child processes
10 Cancelled-run log visibility in the Web UI
11 CLI run cancel command
12 Agent inventory recovery after controller database loss
A Orphaned-run reaper
13 Default Kubernetes sidecar image pullability
14 Distroless sidecar versus shell transfer
15 Kubernetes working directory
16 Kubernetes single-file artifact upload and failure reporting
21 Sidecar idle mode without S3
17 Schedule apply support
18 Artifact download default destination
19 call step self-deadlock
20b CLI configuration precedence
20 SSO direct hash-route rendering
22 Windows Job Object handle lifecycle
23 Agent label deletion on re-registration
25 AppSource first-sync prune after upgrade
26 Artifact-name path traversal
27 Heartbeat lifecycle while draining
28 Pod GC handling of controller errors
29 Database migration coverage for pre-squash installations
30 Template shell argument quoting
31 Matrix cardinality limit before Cartesian allocation
32 Cache-path expansion parity
32a Empty parameters bypassing defaults
32b Standard-agent cache paths relative to process cwd
32c Kubernetes cache path traversal
33a SSO round-trip deep-link preservation
33b AppSource sync status recovery
33c Credential-bearing AppSource repository URL disclosure
33d Late finish and step reports after terminal state
33e Published quickstart image health-check compatibility
34 Stale failing automated tests
35a Heartbeat cancellation assertion
35b AppSource migration-path coverage
35c Matrix variant output aggregation coverage
35d Human artifact-download authorization coverage
35e CLI HTTP method assertions
35f CLI request-construction error handling
36 Parent-to-child call-run cancellation propagation
37 Kubernetes secret interpolation
38 Kubernetes secret log masking
39 Kubernetes call step execution
40 Kubernetes step environment propagation
41 Kubernetes timeout enforcement
42 Kubernetes post-hook execution
43 Kubernetes stderr streaming
44 Shared host/Kubernetes orchestration and parity coverage
24 Audit logging
L1 Missing favicon
L2 Mixed UI language and HTML language metadata
L3 login command server configuration parity
L4 AppSource mixed-kind documentation and sync-status visibility
CTX-K8S Kubernetes validation-environment notes
CTX-HEALTHY Verified-healthy feature list
CTX-ENV General validation-environment notes
```

Assign waves as follows:

```text
Wave 1: 1-5
Wave 2: 6-12, 9c, A
Wave 3: 13-21 and 20b
Wave 4: 22-36, including every suffixed child key
Wave 5: 37-44, 24, L1-L4, and CTX-K8S/CTX-HEALTHY/CTX-ENV
```

- [ ] **Step 3: Validate inventory completeness**

Run:

```powershell
$matrix = Get-Content -Raw -Encoding utf8 .todo-audit-matrix.md
$required = @(
  '1','2','3','4','5','6','7','8','9','9c','10','11','12','A',
  '13','14','15','16','21','17','18','19','20b','20','22','23',
  '25','26','27','28','29','30','31','32','32a','32b','32c',
  '33a','33b','33c','33d','33e','34','35a','35b','35c','35d',
  '35e','35f','36','37','38','39','40','41','42','43','44','24',
  'L1','L2','L3','L4','CTX-K8S','CTX-HEALTHY','CTX-ENV'
)
$missing = $required | Where-Object {
  $matrix -notmatch "(?m)^\| $([regex]::Escape($_)) \|"
}
if ($missing.Count -ne 0) { throw "Missing matrix keys: $($missing -join ', ')" }
if ((Select-String -Path .todo-audit-matrix.md -Pattern '^\| (?!Key |---)' -Encoding utf8).Count -ne $required.Count) {
  throw 'The matrix contains a duplicate or unexpected candidate row'
}
```

Expected: exit code 0 with no output.

- [ ] **Step 4: Commit the inventory**

```powershell
git add .todo-audit-matrix.md
git commit -m "docs: inventory legacy backlog candidates"
```

---

### Task 2: Audit Critical and High Candidates

**Files:**
- Modify: `.todo-audit-matrix.md`
- Read: `internal/store/migrations/*.sql`
- Read: `internal/store/postgres.go`
- Read: `internal/agent/`
- Read: `internal/controller/`
- Read: `internal/dsl/`
- Read: `internal/cli/`
- Read: `docs/jobs.md`
- Read: `README.md`

**Interfaces:**
- Consumes: matrix rows `1`, `2`, `3`, `4`, and `5`.
- Produces: five decided rows with one allowed outcome, current evidence paths, and a final remove/retain/consolidate action.

- [ ] **Step 1: Inspect current source and tests for rows 1-5**

Run these focused searches before reading the matching implementations and
tests:

```powershell
rg -n "Skipped|WaitingApproval|step_reports|retryUntilSuccess" internal test
rg -n "Required|Default|resolveParams|handleTriggerRun|Params" internal/controller internal/dsl test docs/jobs.md
rg -n "EvaluateCondition|condition|fail.open|fail.closed|filters:" internal README.md docs examples templates
rg -n "uploadArtifact|UploadArtifact|filepath.Abs|workspace" internal/agent internal/artifact test
rg -n "WaitingApproval|approval|run show|StepReport" internal/cli internal/controller internal/store test
```

For each row, inspect at least one implementation path and one test path. Also
inspect current user documentation when the claim is a documentation/runtime
contract mismatch.

- [ ] **Step 2: Use history only to explain current evidence**

For paths identified in Step 1, run:

```powershell
git log --oneline --all -- TODO.md internal/store internal/agent internal/controller internal/dsl internal/cli docs/jobs.md README.md
git blame -w -- TODO.md
```

Do not decide a row from a commit subject or the legacy annotation. Use history
to locate the current implementation and regression tests, then cite those
current paths in the matrix.

- [ ] **Step 3: Decide and review every Wave 1 row**

Replace `Audit required` for rows `1` through `5` with exactly one allowed
outcome. Evidence must name current paths and symbols; source line numbers are
optional and must not be the sole locator. Final action must be one of:

```text
Remove from active backlog
Retain as <ID>
Consolidate into <ID>; preserve Legacy IDs
```

Run:

```powershell
$wave = Select-String -Path .todo-audit-matrix.md -Pattern '^\| (1|2|3|4|5) \|' -Encoding utf8
if ($wave.Count -ne 5) { throw 'Wave 1 row count is not five' }
if ($wave.Line -match 'Audit required|Current-tree inspection required|Decision required') {
  throw 'Wave 1 contains an undecided row'
}
```

Expected: exit code 0 with no output.

- [ ] **Step 4: Commit the Wave 1 audit**

```powershell
git add .todo-audit-matrix.md
git commit -m "docs: audit critical and high backlog"
```

---

### Task 3: Audit Medium and Availability Candidates

**Files:**
- Modify: `.todo-audit-matrix.md`
- Read: `examples/jobs/`
- Read: `docs/`
- Read: `internal/agent/`
- Read: `internal/cli/`
- Read: `internal/controller/`
- Read: `internal/config/`
- Read: `internal/store/`
- Read: `web/src/`
- Read: `test/e2e/`

**Interfaces:**
- Consumes: matrix rows `6`, `7`, `8`, `9`, `9c`, `10`, `11`, `12`, and `A`.
- Produces: nine decided rows with current evidence and final actions.

- [ ] **Step 1: Inspect examples, CLI, agent, controller, and Web UI behavior**

Run:

```powershell
rg -n "parallel-steps|parallel:" examples internal/dsl test
rg -n -- "--file|--server|--follow|--output|unified-cli|unified-cd " README.md docs examples templates
rg -n "step\\[|DisplayName|displayName|artifact" internal web/src test
rg -n "workspace|workingDir|workspace-dir|RemoveAll|cleanup" cmd internal docs test
rg -n "Job Object|jobObject|TerminateJobObject|cancel" internal/agent cmd test
rg -n "Cancelled|logs|archiv|trim" web/src internal/cli internal/controller internal/store test
rg -n "run cancel|cancelRun|CancelRun" cmd internal docs test
rg -n "register|UpsertAgent|heartbeat|inventory|reconcile" cmd internal/controller internal/agent internal/store test
rg -n "stuck|orphan|reaper|heartbeat" internal/controller internal/store docs test
```

Parse every referenced example through the existing DSL tests rather than
assuming that a YAML file is valid because it exists:

```powershell
go test -buildvcs=false ./internal/dsl -run 'TestExamplesParse|TestTemplatesParse' -count=1
```

Expected: PASS.

- [ ] **Step 2: Check user-visible Web and CLI coverage**

Run:

```powershell
go test -buildvcs=false ./internal/cli ./internal/config -count=1
Push-Location web
npm test
Pop-Location
```

Expected: both commands PASS. If a command reproduces one of the documented
base failures, record the exact unchanged failure separately from the backlog
decision; any new failure must be investigated before proceeding.

- [ ] **Step 3: Decide and review every Wave 2 row**

Fill all nine rows using the same outcome, evidence, and final-action rules
from Task 2.

Run:

```powershell
$keys = @('6','7','8','9','9c','10','11','12','A')
$rows = foreach ($key in $keys) {
  Select-String -Path .todo-audit-matrix.md -Pattern "^\| $([regex]::Escape($key)) \|" -Encoding utf8
}
if ($rows.Count -ne $keys.Count) { throw 'Wave 2 row count mismatch' }
if ($rows.Line -match 'Audit required|Current-tree inspection required|Decision required') {
  throw 'Wave 2 contains an undecided row'
}
```

Expected: exit code 0 with no output.

- [ ] **Step 4: Commit the Wave 2 audit**

```powershell
git add .todo-audit-matrix.md
git commit -m "docs: audit medium and availability backlog"
```

---

### Task 4: Audit Kubernetes and Adjacent Candidates

**Files:**
- Modify: `.todo-audit-matrix.md`
- Read: `internal/k8sagent/`
- Read: `internal/agent/` — shared orchestration is implemented in this package and imported by the Kubernetes agent as `agentlib`.
- Read: `internal/artifact/`
- Read: `cmd/unified-sidecar/`
- Read: `internal/runtime/`
- Read: `internal/cli/`
- Read: `internal/config/`
- Read: `internal/controller/`
- Read: `manifests/`
- Read: `docs/kubernetes-integration.md`
- Read: `docs/configuration.md`

**Interfaces:**
- Consumes: matrix rows `13`, `14`, `15`, `16`, `21`, `17`, `18`, `19`, `20b`, and `20`.
- Produces: ten decided rows with current evidence and final actions.

- [ ] **Step 1: Inspect sidecar, workspace, artifact, call-step, config, and SSO paths**

Run:

```powershell
rg -n "sidecarImage|sidecar.image|unified-sidecar|ImagePull|distroless" cmd internal manifests docs
rg -n "bash|sh -c|ExecStep|artifact|upload|download|idle" internal/k8sagent cmd/unified-sidecar internal/artifact
rg -n "WorkingDir|workingDir|/workspace|mountPath" internal/k8sagent internal/agent docs test
rg -n "single.file|regular file|UploadArtifact|Succeeded|Failed" internal/k8sagent internal/artifact test
rg -n "kind: Schedule|ScheduleKind|apply" internal/cli internal/dsl examples docs test
rg -n "destination|dest|filepath.Clean|MkdirAll|artifact download" internal/cli internal/artifact docs test
rg -n "call:|executeCall|CreateRun|maxParallel|slot|deadlock" internal/agent internal/k8sagent internal/controller test
rg -n "UNIFIED_SERVER|config|environment|flag|precedence" internal/config internal/cli cmd docs test
rg -n "authReady|hash|location|redirect|OIDC|SSO" web/src internal/controller docs test
```

- [ ] **Step 2: Run focused Kubernetes and artifact tests**

Run:

```powershell
go test -buildvcs=false ./internal/k8sagent ./internal/artifact ./cmd/unified-sidecar ./internal/config -count=1
```

Expected: PASS. Tests that require a live Kubernetes cluster must be identified
by their build tag or skip condition and may support a `Needs verification`
outcome only when static implementation and unit coverage cannot decide the
legacy claim.

- [ ] **Step 3: Decide and review every Wave 3 row**

Fill all ten rows. A historical Docker Desktop or image-registry observation
must become `Environment-only` if it describes only the old setup, or `Needs
verification` if a current published-image experiment is necessary. A retained
verification row must state the exact image, command or manifest, and observable
success condition in its final action notes.

Run:

```powershell
$keys = @('13','14','15','16','21','17','18','19','20b','20')
$rows = foreach ($key in $keys) {
  Select-String -Path .todo-audit-matrix.md -Pattern "^\| $([regex]::Escape($key)) \|" -Encoding utf8
}
if ($rows.Count -ne $keys.Count) { throw 'Wave 3 row count mismatch' }
if ($rows.Line -match 'Audit required|Current-tree inspection required|Decision required') {
  throw 'Wave 3 contains an undecided row'
}
```

Expected: exit code 0 with no output.

- [ ] **Step 4: Commit the Wave 3 audit**

```powershell
git add .todo-audit-matrix.md
git commit -m "docs: audit kubernetes and adjacent backlog"
```

---

### Task 5: Audit Regression-Review and Coverage Candidates

**Files:**
- Modify: `.todo-audit-matrix.md`
- Read: `internal/agent/`
- Read: `internal/agent/` — shared orchestration and host execution.
- Read: `internal/controller/appsource*.go`
- Read: `internal/dsl/appsource*.go`
- Read: `internal/cli/appsource*.go`
- Read: `internal/artifact/`
- Read: `internal/cli/`
- Read: `internal/controller/`
- Read: `internal/dsl/`
- Read: `internal/k8sagent/`
- Read: `internal/store/`
- Read: `internal/runtime/`
- Read: `templates/`
- Read: `docker-compose.yaml`
- Read: `deployments/docker/docker-compose.yaml`
- Read: `docker/*.Dockerfile`
- Read: `test/`

**Interfaces:**
- Consumes: rows `22`, `23`, `25` through `36`, including `32a-c`, `33a-e`, and `35a-f`.
- Produces: a separate evidence-backed decision for every independent embedded follow-up instead of hiding open work beneath umbrella IDs.

- [ ] **Step 1: Inspect lifecycle, data-safety, security, and scale claims**

Run:

```powershell
rg -n "Job Object|CloseHandle|AssignProcess|TerminateJob" internal/agent internal/runtime test
rg -n "labels|authorized_labels|UpsertAgent|register|enroll" internal/controller internal/store internal/agent test
rg -n "prune|managed|orphan|first sync|AppSource" internal/controller internal/store internal/dsl test
rg -n "ArtifactKey|artifact.*name|path traversal|filepath.Clean|RelDir|safe" internal/artifact internal/controller internal/agent internal/k8sagent test
rg -n "heartbeat|claimCtx|drain|reaper|stuck" internal/agent internal/k8sagent internal/controller internal/store test
rg -n "Pod GC|podgc|NotFound|404|StatusNotFound" internal/k8sagent internal/controller test
rg -n "migrations|schema_migrations|version|squash|001_init" internal/store docs test
rg -n "shell|quote|printf|\\$\\{|\\{\\{" templates internal/dsl test
rg -n "MAX_COMBINATIONS|maxCombos|Cartesian|MatrixCombo|exclude" internal/dsl internal/agent internal/k8sagent test
```

- [ ] **Step 2: Inspect cache and embedded row 32 follow-ups separately**

Run:

```powershell
rg -n "cache.path|CachePath|resolve.*cache|RestoreCache|SaveCache|mountPath" internal/agent internal/k8sagent cmd/unified-sidecar test docs
rg -n "resolveParams|Default|default|empty string" internal/controller internal/dsl test
rg -n "filepath.Abs|filepath.Join|path.Join|workingDir|workspace" internal/agent internal/k8sagent test
rg -n "\\.\\.|RelDir|confine|traversal|escape" internal/k8sagent cmd/unified-sidecar internal/artifact test
```

Rows `32`, `32a`, `32b`, and `32c` must each receive an independent outcome.
If they share one remaining root cause, consolidate them only after recording
all legacy keys in the matrix.

- [ ] **Step 3: Inspect embedded row 33 issues separately**

Run:

```powershell
rg -n "hash|deep.link|redirect|authReady|OIDC" web/src internal/controller test
rg -n "Syncing|last_sync|lastError|SetAppSourceSyncStatus|sync reaper|panic" internal/controller internal/store web/src test
rg -n "repoURL|userinfo|password|redact|sanitize|last_error|lastError" internal/controller internal/dsl internal/gittemplate web/src test docs
rg -n "RowsAffected|MarkRunFinished|terminal|step report|late report|CAS" internal/controller internal/store test
rg -n "healthcheck|wget|controller:|ghcr.io|distroless|alpine" docker-compose.yaml deployments/docker/docker-compose.yaml docker docs README.md .github
```

Rows `33a` through `33e` must each receive an independent outcome. The
published-image check in `33e` may be `Needs verification` only with an exact
registry image/tag and a health-check command that can be copied into the final
`Done when`.

- [ ] **Step 4: Inspect stale-test and coverage rows separately**

Run:

```powershell
rg -n "TestPhase8_FullOIDCFlow|AuthSetup|heartbeat|captureTransport|NewRequest|GetStepOutputs|artifact download|parentRunId|CancelRun" internal web/src test
rg -n "DefaultRole|RoleMap|groups|currentUser|logout" internal/controller web/src test
rg -n "RowsAffected|parent_run_id|ListChildRun|cancelDescendant|orphaned" internal/controller internal/store internal/agent test
```

Run focused suites:

```powershell
go test -buildvcs=false ./internal/controller ./internal/dsl ./internal/artifact -count=1
go test -buildvcs=false ./internal/cli -count=1
Push-Location web
npm test
Pop-Location
```

Expected: PASS except for an exact documented baseline failure. Rows `34`,
`35a` through `35f`, and `36` must be decided from current tests and
implementation rather than the legacy prose.

- [ ] **Step 5: Decide and review every Wave 4 row**

Run:

```powershell
$keys = @(
  '22','23','25','26','27','28','29','30','31',
  '32','32a','32b','32c',
  '33a','33b','33c','33d','33e',
  '34','35a','35b','35c','35d','35e','35f','36'
)
$rows = foreach ($key in $keys) {
  Select-String -Path .todo-audit-matrix.md -Pattern "^\| $([regex]::Escape($key)) \|" -Encoding utf8
}
if ($rows.Count -ne $keys.Count) { throw 'Wave 4 row count mismatch' }
if ($rows.Line -match 'Audit required|Current-tree inspection required|Decision required') {
  throw 'Wave 4 contains an undecided row'
}
```

Expected: exit code 0 with no output.

- [ ] **Step 6: Commit the Wave 4 audit**

```powershell
git add .todo-audit-matrix.md
git commit -m "docs: audit regression and coverage backlog"
```

---

### Task 6: Audit Parity, Feature, Low, and Contextual Candidates

**Files:**
- Modify: `.todo-audit-matrix.md`
- Read: `internal/agent/` — shared orchestration and host execution.
- Read: `internal/k8sagent/`
- Read: `internal/secrets/`
- Read: `internal/controller/audit*.go`
- Read: `internal/store/postgres_audit*.go`
- Read: `internal/cli/audit*.go`
- Read: `web/`
- Read: `docs/`
- Read: `README.md`
- Read: `docker-compose.yaml`
- Read: `deployments/docker/docker-compose.yaml`
- Read: `docker/*.Dockerfile`
- Read: `manifests/`

**Interfaces:**
- Consumes: rows `37` through `44`, `24`, `L1` through `L4`, and all three `CTX-*` rows.
- Produces: decided parity and audit-feature rows, assigned Low IDs, and explicit removal or bounded verification decisions for historical context.

- [ ] **Step 1: Verify each parity claim against shared orchestration and tests**

Run:

```powershell
rg -n "Secrets|secret.*mask|Masker|call:|timeout|post:|stderr|env|UNIFIED_AGENT_OS" internal/agent internal/k8sagent internal/secrets test
rg -n "Parity|parity|ExecBackend|Orchestrat|RunPlan|ConcurrencyMode" internal/agent internal/k8sagent internal/paritycases test
git log --oneline --all -- internal/agent internal/k8sagent internal/paritycases
```

Rows `37` through `44` require current source and test evidence even though
their legacy titles claim completion.

- [ ] **Step 2: Verify audit logging**

Run:

```powershell
rg -n "audit|Audit" internal/controller internal/store internal/cli web/src docs README.md test
go test -buildvcs=false ./internal/controller ./internal/store ./internal/cli -run 'Audit|audit' -count=1
```

Expected: the focused tests PASS. Decide row `24` from the current API, store,
CLI, authorization, retention, and redaction coverage.

- [ ] **Step 3: Inspect each Low item and assign stable IDs**

Run:

```powershell
rg -n "favicon|rel=.icon|lang=|Logout|UNIFIED_SERVER|AppSource|lastError|sync status|[\x{3040}-\x{30ff}\x{3400}-\x{9fff}]" web docs README.md internal/cli internal/controller
```

Audit the four legacy bullets as:

```text
L1 Missing favicon
L2 Mixed UI language and HTML language metadata
L3 login command server configuration parity
L4 AppSource mixed-kind documentation and sync-status visibility
```

If `L4` contains two independently open deliverables, split the retained
backlog into `L4a` and `L4b` while preserving `Legacy IDs: L4` on both. Do not
combine them merely because they originated in one bullet.

- [ ] **Step 4: Classify contextual sections**

Inspect `CTX-K8S`, `CTX-HEALTHY`, and `CTX-ENV` line by line. Move any concrete,
still-active product defect into its own existing or Low candidate row before
classifying the context row. The contextual row itself must end as
`Environment-only`, `Accepted behavior`, or `Resolved`; it cannot survive as a
historical section in the active backlog.

Run:

```powershell
rg -n "docker desktop|Docker Desktop|localhost|temporary|[\x{3040}-\x{30ff}\x{3400}-\x{9fff}]" TODO.md
```

Expected before the rewrite: matches identify every historical context block
that must not be copied into the final file.

- [ ] **Step 5: Decide and review every Wave 5 row**

Run:

```powershell
$keys = @(
  '37','38','39','40','41','42','43','44','24',
  'L1','L2','L3','L4','CTX-K8S','CTX-HEALTHY','CTX-ENV'
)
$rows = foreach ($key in $keys) {
  Select-String -Path .todo-audit-matrix.md -Pattern "^\| $([regex]::Escape($key)) \|" -Encoding utf8
}
if ($rows.Count -ne $keys.Count) { throw 'Wave 5 row count mismatch' }
if ($rows.Line -match 'Audit required|Current-tree inspection required|Decision required') {
  throw 'Wave 5 contains an undecided row'
}
```

Expected: exit code 0 with no output.

- [ ] **Step 6: Confirm the entire matrix is decided**

Run:

```powershell
$matrix = Get-Content -Raw -Encoding utf8 .todo-audit-matrix.md
if ($matrix -match 'Audit required|Current-tree inspection required|Decision required') {
  throw 'The audit matrix still contains undecided candidates'
}
$allowed = 'Resolved|Accepted behavior|Environment-only|Open|Partial|Needs verification'
$bad = Select-String -Path .todo-audit-matrix.md -Pattern '^\| (?!Key |---)([^|]+) \|[^|]+\|[^|]+\| (?!Resolved |Accepted behavior |Environment-only |Open |Partial |Needs verification )' -Encoding utf8
if ($bad) { throw "Matrix contains invalid outcomes: $($bad.Line -join '; ')" }
```

Expected: exit code 0 with no output.

- [ ] **Step 7: Commit the Wave 5 audit**

```powershell
git add .todo-audit-matrix.md
git commit -m "docs: audit parity feature and low backlog"
```

---

### Task 7: Consolidate Decisions and Rewrite the Active Backlog

**Files:**
- Modify: `TODO.md`
- Read: `.todo-audit-matrix.md`

**Interfaces:**
- Consumes: a fully decided matrix with evidence and final actions.
- Produces: the only user-facing backlog, containing one entry per retained root cause and no historical material.

- [ ] **Step 1: Build the retained-entry list**

Run:

```powershell
Select-String -Path .todo-audit-matrix.md -Pattern '\| (Open|Partial|Needs verification) \|' -Encoding utf8
```

For each match, copy its stable ID, remaining problem, outcome, evidence, and
completion condition into a review checklist. Before writing, consolidate rows
only when they share the same remaining root cause and completion test. Record
all removed duplicate keys in the survivor's `Legacy IDs` line.

- [ ] **Step 2: Replace the historical file with the exact active-backlog structure**

Write `TODO.md` in English using:

```markdown
# Active Backlog

This file contains active work only. Completed history is available in Git.
Last audited: 2026-07-26 against `1e46459`.

## Critical

### <stable ID>. <action-oriented remaining problem>

- **Status:** Open
- **Impact:** <concrete affected behavior and user or system>
- **Evidence:** `<current/path>` names the current implementation or missing coverage that demonstrates the gap.
- **Done when:** <observable behavior plus required regression or integration coverage>

## High

## Medium

## Low
```

The angle-bracket text above describes the required value shape; no angle
brackets may remain in the written file. For every retained row:

- use `Open`, `Partial`, or `Needs verification` exactly as decided,
- translate the remaining problem into an action-oriented English title,
- cite current paths and symbols rather than stale branch names or obsolete
  source line numbers,
- state a measurable `Done when` condition,
- add `- **Legacy IDs:** ...` only when consolidation occurred,
- omit proposed implementations unless the matrix identified a constraint
  required to prevent regression.

If a severity section has no retained entries, keep its heading and add
`No active items.` so that all four severity levels remain explicit.

- [ ] **Step 3: Review ordering and identity**

Check that:

```text
Critical precedes High, High precedes Medium, and Medium precedes Low.
Within a severity, numeric IDs are ordered numerically, then lettered IDs.
Legacy IDs are preserved; gaps are not renumbered.
Low entries use L1, L2, and so on.
No two retained entries describe the same root cause and completion test.
Distinct independently testable symptoms remain separate.
```

- [ ] **Step 4: Validate required fields mechanically**

Run:

```powershell
$text = Get-Content -Raw -Encoding utf8 TODO.md
$entries = ([regex]::Matches($text, '(?m)^### ')).Count
$status = ([regex]::Matches($text, '(?m)^- \*\*Status:\*\* (Open|Partial|Needs verification)$')).Count
$impact = ([regex]::Matches($text, '(?m)^- \*\*Impact:\*\* .+')).Count
$evidence = ([regex]::Matches($text, '(?m)^- \*\*Evidence:\*\* .+')).Count
$done = ([regex]::Matches($text, '(?m)^- \*\*Done when:\*\* .+')).Count
if (@($status,$impact,$evidence,$done) | Where-Object { $_ -ne $entries }) {
  throw "Field counts do not match entry count $entries: status=$status impact=$impact evidence=$evidence done=$done"
}
if ($text -match '<[^>]+>') { throw 'Template angle-bracket text remains in TODO.md' }
```

Expected: exit code 0 with no output.

- [ ] **Step 5: Commit the active backlog rewrite**

```powershell
git add TODO.md
git commit -m "docs: rewrite active backlog from audited evidence"
```

---

### Task 8: Verify Accounting, Remove the Matrix, and Run Final Checks

**Files:**
- Delete: `.todo-audit-matrix.md`
- Verify: `TODO.md`
- Modify only if needed: a live Markdown file containing a broken `TODO.md` anchor or legacy-ID link.

**Interfaces:**
- Consumes: the decided matrix and rewritten backlog.
- Produces: a clean final tree with the active backlog, no temporary ledger, and verification evidence.

- [ ] **Step 1: Perform a one-to-one accounting review before deletion**

For every matrix row:

- `Resolved`, `Accepted behavior`, and `Environment-only` must have final action
  `Remove from active backlog`.
- `Open`, `Partial`, and `Needs verification` must appear in `TODO.md` under
  their retained ID or be named in a survivor's `Legacy IDs` line.
- No completed or contextual row may appear as an active entry.
- Every matrix evidence path must exist, unless the evidence explicitly states
  a repository-wide absence search.

Run:

```powershell
$rows = Select-String -Path .todo-audit-matrix.md -Pattern '^\| (?!Key |---)' -Encoding utf8
$active = $rows | Where-Object { $_.Line -match '\| (Open|Partial|Needs verification) \|' }
$removed = $rows | Where-Object { $_.Line -match '\| (Resolved|Accepted behavior|Environment-only) \|' }
if (($active.Count + $removed.Count) -ne $rows.Count) {
  throw 'A matrix row is not accounted for by an allowed outcome'
}
"accounted=$($rows.Count) active=$($active.Count) removed=$($removed.Count)"
```

Expected: one summary line where `accounted` equals `active + removed`.
Manually compare every active row with its `TODO.md` entry or `Legacy IDs`
line before proceeding.

- [ ] **Step 2: Scan for forbidden historical content**

Run:

```powershell
$text = Get-Content -Raw -Encoding utf8 TODO.md
if ($text -match '[\u3040-\u30ff\u3400-\u9fff]') { throw 'Japanese characters remain in TODO.md' }
if ($text -match '(?i)\bIMPLEMENTED\b|\bRESOLVED\b') { throw 'Completion markers remain in TODO.md' }
if ($text -match '(?i)validation environment|verified healthy|commit-by-commit|investigation log') {
  throw 'Historical validation or investigation content remains in TODO.md'
}
if ($text -match '(?i)\bprobably\b|\bsuspected\b|\bmay be broken\b') {
  throw 'Unqualified uncertainty remains; use Needs verification'
}
```

Expected: exit code 0 with no output.

- [ ] **Step 3: Scan live documentation for broken backlog references**

Run:

```powershell
rg -n 'TODO\.md(?:#[A-Za-z0-9_-]+)?|TODO #[0-9A-Za-z]+' README.md docs examples templates manifests .github --glob '!docs/superpowers/**'
```

Expected: no live anchor or legacy-ID link targets an entry removed or renamed
by the rewrite. If a live reference exists, update only that referring Markdown
file to point at the retained stable ID or remove the obsolete historical link,
then repeat the scan.

- [ ] **Step 4: Remove the temporary matrix**

Use `apply_patch` to delete `.todo-audit-matrix.md`. Do not use a recursive
filesystem command.

- [ ] **Step 5: Run formatting and content verification**

Run:

```powershell
git diff --check
rg -n '^\s+$' TODO.md
rg -n '(?i)\b(TBD|fill in details|implement later)\b' TODO.md
git status --short
git diff --stat 1e46459...HEAD
git diff 1e46459...HEAD -- TODO.md README.md docs examples templates manifests .github
```

Expected:

- `git diff --check` exits 0.
- The whitespace and placeholder searches produce no matches.
- `.todo-audit-matrix.md` is shown as deleted until committed.
- The content diff contains only the active-backlog rewrite, the approved
  spec/plan, and any narrowly necessary live-reference repair.

- [ ] **Step 6: Run repository verification**

Run:

```powershell
go test -buildvcs=false ./... -count=1
Push-Location web
npm test
npm run build
Pop-Location
```

Expected:

- Web tests and build PASS.
- The full Go suite PASS, or reproduces only the five exact known local
  baseline failures listed above with no additional failure.
- Any additional failure is investigated before the branch is offered for
  integration.

- [ ] **Step 7: Commit matrix removal and any live-reference repair**

```powershell
git add -A
git commit -m "docs: finalize active backlog audit"
```

- [ ] **Step 8: Verify the committed result**

Run:

```powershell
git status --short
git log --oneline 1e46459..HEAD
git diff --check 1e46459..HEAD
Test-Path .todo-audit-matrix.md
```

Expected:

- `git status --short` has no output.
- The log shows the audit-wave commits and final rewrite commits.
- `git diff --check` exits 0.
- `Test-Path` prints `False`.

- [ ] **Step 9: Use the branch-finishing workflow**

Use `superpowers:verification-before-completion`, then
`superpowers:finishing-a-development-branch`. Before integration, create or
update the pull request and require all five GitHub CI jobs to pass on the
branch. Because the base commit's GitHub CI is green, a failed branch CI job is
not waived by the local Windows baseline exceptions.

## Self-Review

### Spec Coverage

- Complete legacy-candidate accounting: Tasks 1-6.
- Current code, test, documentation, and history evidence: Tasks 2-6.
- Six allowed audit outcomes and strict retention rule: Global Constraints and Tasks 2-6.
- English-only active backlog with exact header and audit baseline: Task 7.
- Stable IDs, Low IDs, duplicate consolidation, and ordering: Tasks 6-7.
- Removal of completed work, investigation logs, environment notes, and healthy-feature lists: Tasks 6 and 8.
- Required `Status`, `Impact`, `Evidence`, and `Done when` fields: Task 7.
- Temporary audit matrix review and deletion: Tasks 1 and 8.
- Japanese, completion-marker, uncertainty, whitespace, and placeholder scans: Task 8.
- Live legacy-reference scan: Task 8.
- Documentation-relevant tests, full local verification, and green GitHub CI: Tasks 3-8.
- Documentation-only scope and narrow live-reference exception: Global Constraints and Task 8.

### Placeholder Scan

All uses of the word `TODO` identify the repository file or a legacy reference
syntax. The angle-bracket examples in Task 7 are explicitly forbidden from the
resulting file and are complete format definitions, not unfinished plan
content. The plan contains no `TBD`, deferred implementation instruction, or
unspecified error-handling step.

### Interface and Naming Consistency

The matrix schema is created in Task 1, populated without renaming keys in
Tasks 2-6, consumed by Task 7, and deleted in Task 8. The only final status
values are `Open`, `Partial`, and `Needs verification`; the only audit outcomes
are the six values defined in Global Constraints and Task 1.
