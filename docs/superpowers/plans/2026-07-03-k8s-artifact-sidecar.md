# k8s-agent Artifact Sidecar (A3) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `uploadArtifact` / `downloadArtifact` work on the Kubernetes agent for any pod image, by auto-injecting a `unified-artifact` sidecar (tar+zstd+curl) that shares the pod workspace volume and transfers files to/from the controller's artifact endpoint.

**Architecture:** The k8s pod model already supports multi-container pods with the workspace volume auto-mounted into every container and per-step container routing. We add a reserved sidecar container in `BuildPod`, and dispatch artifact steps in `orchestrate` by exec-ing a `tar|zstd|curl` transfer into that sidecar (token via the sidecar's isolated env). The transfer is behind an injectable `artifactExec` seam so the dispatch is unit-testable without a cluster.

**Tech Stack:** Go 1.26, k8s client-go (`ExecStep`), the controller artifact endpoint (`PUT`/`GET /api/v1/runs/{runID}/artifacts/{name}`, tar+zstd, `BearerAuth(AgentToken)`), alpine sidecar image.

## Global Constraints

- Go module `github.com/eirueimi/unified-cd`, Go 1.26.2.
- Spec: `docs/superpowers/specs/2026-07-03-k8s-artifact-sidecar-design.md`.
- Scope: k8s `uploadArtifact` + `downloadArtifact` ONLY. **Cache is out of scope** (direct-S3 storage; needs a controller cache-proxy — future). Keep the sidecar/`artifactExec`/transfer helpers reusable (not artifact-specific) so cache can plug in later.
- Wire format = tar + zstd, endpoint `PUT`/`GET /api/v1/runs/{runID}/artifacts/{name}` (cross-agent compatible with the standard agent's `client.UploadArtifact`/`DownloadArtifact`).
- Sidecar container name `unified-artifact` (reserved); token via the sidecar's env `UNIFIED_AGENT_TOKEN`, controller URL via `UNIFIED_SERVER`. Token is isolated from the job container (container boundary); it DOES appear in the pod spec — documented caveat, Secret hardening is a follow-up.
- Do NOT change the standard agent (`internal/agent/*`).
- New cluster-dependent tests are `//go:build k8s` (not run in this environment); the sidecar injection + dispatch are unit-tested cluster-free.
- Path joins for the in-pod (Linux) scripts use `path.Join` (forward slash), NOT `filepath.Join`.

---

## File map

| Path | Responsibility |
|---|---|
| `docker/artifact-sidecar.Dockerfile` | sidecar image (alpine + tar/zstd/curl/ca-certificates) |
| `internal/k8sagent/config.go` | `SidecarImage` config field + default |
| `internal/k8sagent/podbuilder.go` | inject the `unified-artifact` sidecar (workspace mount + env); reserve the name |
| `internal/k8sagent/podbuilder_test.go` | unit test: pod includes the sidecar with mount + env |
| `internal/k8sagent/agent.go` | thread server/token/sidecar-image into `BuildPod`; `artifactExec` seam; dispatch upload/download in `orchestrate` |
| `internal/k8sagent/orchestrate_test.go` | unit test: artifact steps dispatch to the sidecar exec with correct commands |
| `internal/k8sagent/artifact_k8s_test.go` | `//go:build k8s` integration round-trip (needs a cluster) |
| `docs/kubernetes-integration.md`, `docs/jobs.md` | document behavior + caveats |

---

## Task 1: Sidecar image + auto-injection in BuildPod

**Files:**
- Create: `docker/artifact-sidecar.Dockerfile`
- Modify: `internal/k8sagent/config.go`, `internal/k8sagent/podbuilder.go`, `internal/k8sagent/agent.go` (caller), `internal/k8sagent/pool.go` (caller)
- Test: `internal/k8sagent/podbuilder_test.go`

**Interfaces:**
- Produces: `const artifactSidecarName = "unified-artifact"`; `BuildPod` gains a `sidecar SidecarSpec` param where `type SidecarSpec struct { Image, Server, Token string }`; `Config.SidecarImage string`.

- [ ] **Step 1: Sidecar Dockerfile**

Create `docker/artifact-sidecar.Dockerfile`:

```dockerfile
# Minimal image with the tools needed to transfer artifacts between the pod
# workspace volume and the controller's artifact endpoint.
FROM alpine:3.20
RUN apk add --no-cache tar zstd curl ca-certificates
```

- [ ] **Step 2: Config field**

In `internal/k8sagent/config.go`, add a `SidecarImage` field to `Config` (after `PodImage`) with a default in the same place `PodImage` is defaulted:

```go
	SidecarImage  string                      `yaml:"sidecarImage"`
```

and in the defaulting logic (next to `if c.PodImage == "" { c.PodImage = "…" }`):

```go
	if c.SidecarImage == "" {
		c.SidecarImage = "ghcr.io/eirueimi/unified-cd-artifact-sidecar:latest"
	}
```

(Use the same registry/naming convention as `PodImage`. The exact published tag can be adjusted later; the default just needs to be a sensible non-empty value.)

- [ ] **Step 3: Write the failing test**

Add to `internal/k8sagent/podbuilder_test.go`:

```go
func TestBuildPod_InjectsArtifactSidecar(t *testing.T) {
	pod, err := BuildPod("run1", "ns", nil, nil, "job-image:latest",
		SidecarSpec{Image: "sidecar:latest", Server: "http://ctrl:8080", Token: "tok"})
	require.NoError(t, err)

	var sidecar *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == artifactSidecarName {
			sidecar = &pod.Spec.Containers[i]
		}
	}
	require.NotNil(t, sidecar, "pod must include the unified-artifact sidecar")
	assert.Equal(t, "sidecar:latest", sidecar.Image)

	// Sidecar shares the workspace mount.
	var hasWorkspace bool
	for _, m := range sidecar.VolumeMounts {
		if m.Name == "workspace" {
			hasWorkspace = true
		}
	}
	assert.True(t, hasWorkspace, "sidecar must mount the workspace volume")

	// Sidecar has the controller URL + token env (job container must NOT).
	env := map[string]string{}
	for _, e := range sidecar.Env {
		env[e.Name] = e.Value
	}
	assert.Equal(t, "http://ctrl:8080", env["UNIFIED_SERVER"])
	assert.Equal(t, "tok", env["UNIFIED_AGENT_TOKEN"])
}
```

(Check the existing `podbuilder_test.go` for the `corev1` import + test style; match it. Verify the workspace volume name is `"workspace"` — per `injectWorkspace` it is.)

- [ ] **Step 4: Run test to verify it fails**

Run: `go test ./internal/k8sagent/ -run TestBuildPod_InjectsArtifactSidecar -v`
Expected: FAIL — `SidecarSpec`/`artifactSidecarName` undefined and `BuildPod` arity mismatch.

- [ ] **Step 5: Implement the sidecar injection**

In `internal/k8sagent/podbuilder.go`:

```go
const artifactSidecarName = "unified-artifact"

// SidecarSpec configures the injected artifact-transfer sidecar.
type SidecarSpec struct {
	Image  string
	Server string // controller base URL reachable from within the pod
	Token  string // bearer token for the controller artifact endpoint
}
```

Change `BuildPod`'s signature to append `sidecar SidecarSpec`, and inject the
container BEFORE `injectWorkspace(podSpec, wsCfg)` so it receives the workspace
mount from the existing all-containers loop. Guard against a name collision with
a user-supplied container:

```go
	for _, c := range podSpec.Containers {
		if c.Name == artifactSidecarName {
			return nil, fmt.Errorf("container name %q is reserved for the artifact sidecar", artifactSidecarName)
		}
	}
	if sidecar.Image != "" {
		podSpec.Containers = append(podSpec.Containers, corev1.Container{
			Name:    artifactSidecarName,
			Image:   sidecar.Image,
			Command: []string{"sleep", "infinity"},
			Env: []corev1.EnvVar{
				{Name: "UNIFIED_SERVER", Value: sidecar.Server},
				{Name: "UNIFIED_AGENT_TOKEN", Value: sidecar.Token},
			},
		})
	}
	injectWorkspace(podSpec, wsCfg)
```

- [ ] **Step 6: Update the two callers**

In `internal/k8sagent/agent.go:115` and `internal/k8sagent/pool.go:138`, pass the
sidecar spec from config. For agent.go:

```go
	pod, err := BuildPod(c.RunID, a.cfg.Namespace, a.cfg.PodTemplates, c.PodTemplate, a.cfg.PodImage,
		SidecarSpec{Image: a.cfg.SidecarImage, Server: a.cfg.Server, Token: a.cfg.Token})
```

For `pool.go` (the pooled-pod path), thread the same values — check what `pool` has access to (it takes `namespace`, `fallbackImage`; it may need the sidecar spec passed into `ClaimPod`/`BuildPod` from the agent). Add a `SidecarSpec` parameter to the pool's build path so pooled pods also get the sidecar. (Read `pool.go` to thread it cleanly; the agent has `a.cfg.Server/Token/SidecarImage`.)

- [ ] **Step 7: Run tests + build**

Run: `go test ./internal/k8sagent/ -run TestBuildPod -v && go build ./... && go build -tags k8s ./internal/k8sagent/`
Expected: PASS; both build variants compile (fix any other `BuildPod` call sites the compiler flags, e.g. in tests).

- [ ] **Step 8: Commit**

```bash
git add docker/artifact-sidecar.Dockerfile internal/k8sagent/config.go internal/k8sagent/podbuilder.go internal/k8sagent/agent.go internal/k8sagent/pool.go internal/k8sagent/podbuilder_test.go
git commit -m "feat(k8sagent): inject artifact-transfer sidecar into pods"
```

---

## Task 2: Dispatch uploadArtifact/downloadArtifact via the sidecar

**Files:**
- Modify: `internal/k8sagent/agent.go` (`executeRun`, `orchestrate`, `makeRunStep`)
- Test: `internal/k8sagent/orchestrate_test.go`; Create: `internal/k8sagent/artifact_k8s_test.go` (`//go:build k8s`)

**Interfaces:**
- Consumes: the sidecar from Task 1 (`artifactSidecarName`).
- Produces: `orchestrate` gains an `artifactExec func(ctx context.Context, container, script string) (exitCode int, err error)` param; `makeRunStep` dispatches `step.UploadArtifact`/`step.DownloadArtifact` by building a shell script and calling `artifactExec(ctx, artifactSidecarName, script)`.

- [ ] **Step 1: Write the failing unit tests**

In `internal/k8sagent/orchestrate_test.go`, extend the harness to inject a fake
`artifactExec` that records calls, and add tests. First adapt `runOrchestrate`
(and its variants) to pass an `artifactExec` — a fake that records
`(container, script)` and returns a configurable exit code (default 0). Then:

```go
func TestOrchestrate_UploadArtifactDispatchesToSidecar(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "up",
			UploadArtifact: &api.UploadArtifactStep{Name: "app", Path: "bin/app"}}},
	}}
	rec, statuses, final := runOrchestrateArtifact(t, c, 0 /*exit*/)
	require.Len(t, rec, 1)
	assert.Equal(t, artifactSidecarName, rec[0].container)
	assert.Contains(t, rec[0].script, "tar cf -")
	assert.Contains(t, rec[0].script, "zstd")
	assert.Contains(t, rec[0].script, "-X PUT")
	assert.Contains(t, rec[0].script, "/api/v1/runs/r1/artifacts/app")
	assert.Equal(t, "Succeeded", statuses["up"])
	assert.Equal(t, "Succeeded", final)
}

func TestOrchestrate_DownloadArtifactDispatchesToSidecar(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "dl",
			DownloadArtifact: &api.DownloadArtifactStep{Name: "app", DestDir: "out"}}},
	}}
	rec, statuses, _ := runOrchestrateArtifact(t, c, 0)
	require.Len(t, rec, 1)
	assert.Equal(t, artifactSidecarName, rec[0].container)
	assert.Contains(t, rec[0].script, "curl")
	assert.Contains(t, rec[0].script, "zstd -d")
	assert.Contains(t, rec[0].script, "tar xf -")
	assert.Contains(t, rec[0].script, "/api/v1/runs/r1/artifacts/app")
	assert.Equal(t, "Succeeded", statuses["dl"])
}

func TestOrchestrate_ArtifactExecFailureFailsRun(t *testing.T) {
	c := api.ClaimResponse{RunID: "r1", Stages: []api.ClaimStage{
		{Step: &api.ClaimStep{Index: 0, StageIndex: 0, Name: "up",
			UploadArtifact: &api.UploadArtifactStep{Name: "app", Path: "bin/app"}}},
	}}
	_, statuses, final := runOrchestrateArtifact(t, c, 1 /*non-zero exit*/)
	assert.Equal(t, "Failed", statuses["up"])
	assert.Equal(t, "Failed", final)
}
```

Write the `runOrchestrateArtifact(t, c, exitCode) (recorded, statuses, final)`
harness helper alongside the existing `runOrchestrate`: it stands up the same
mock controller, passes a fake `stepExec` (unused here) AND a fake `artifactExec`
that appends `{container, script}` to a slice and returns `exitCode`. Reuse the
existing status-recording + FinishRun capture.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/k8sagent/ -run 'TestOrchestrate_(Upload|Download|ArtifactExec)' -v`
Expected: FAIL — `orchestrate` has no `artifactExec` param and no artifact dispatch.

- [ ] **Step 3: Add the `artifactExec` seam + dispatch**

Change `orchestrate`'s signature to accept `artifactExec func(ctx context.Context, container, script string) (int, error)`. In `executeRun`, build the production `artifactExec` (capturing `podName`) and pass it:

```go
	artifactExec := func(execCtx context.Context, container, script string) (int, error) {
		stderrPusher := agentlib.NewLogPusher(a.client, a.cfg.AgentID, c.RunID, 0, "stderr")
		ec, err := a.exec.ExecStep(execCtx, podName, container, script, io.Discard, stderrPusher)
		stderrPusher.Flush(execCtx)
		return ec, err
	}
```

Compute the workspace mount path once in `executeRun` (mirror the existing
`cleanWorkspace` block's logic): `mountPath := "/workspace"; if c.PodTemplate != nil && c.PodTemplate.Workspace != nil && c.PodTemplate.Workspace.MountPath != "" { mountPath = c.PodTemplate.Workspace.MountPath }`, and pass `mountPath` + `artifactExec` into `orchestrate`.

In `makeRunStep` (or the step body), AFTER the `if:` gate and approval branch,
BEFORE the run/exec branch, add (using `path.Join`, not `filepath.Join`):

```go
		if step.UploadArtifact != nil {
			started := time.Now().UTC()
			_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
				RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.Name, Status: "Running", StartedAt: started,
			})
			src := path.Join(mountPath, step.UploadArtifact.Path)
			url := fmt.Sprintf("$UNIFIED_SERVER/api/v1/runs/%s/artifacts/%s", c.RunID, step.UploadArtifact.Name)
			script := fmt.Sprintf(
				`set -e; tar cf - -C %q . | zstd -q | curl -fsS -X PUT -H "Authorization: Bearer $UNIFIED_AGENT_TOKEN" --data-binary @- %q`,
				src, url)
			ec, err := artifactExec(ctx, artifactSidecarName, script)
			status := "Succeeded"
			if err != nil || ec != 0 {
				status = "Failed"
			}
			_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
				RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.Name, Status: status, ExitCode: ec, StartedAt: started, EndedAt: time.Now().UTC(),
			})
			if status == "Failed" && !step.ContinueOnError {
				failedFlag.Store(true)
			}
			return
		}
		if step.DownloadArtifact != nil {
			started := time.Now().UTC()
			_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
				RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.Name, Status: "Running", StartedAt: started,
			})
			dest := step.DownloadArtifact.DestDir
			if dest == "" {
				dest = "."
			}
			destAbs := path.Join(mountPath, dest)
			url := fmt.Sprintf("$UNIFIED_SERVER/api/v1/runs/%s/artifacts/%s", c.RunID, step.DownloadArtifact.Name)
			script := fmt.Sprintf(
				`set -e; mkdir -p %q; curl -fsS -H "Authorization: Bearer $UNIFIED_AGENT_TOKEN" %q | zstd -dq | tar xf - -C %q`,
				destAbs, url, destAbs)
			ec, err := artifactExec(ctx, artifactSidecarName, script)
			status := "Succeeded"
			if err != nil || ec != 0 {
				status = "Failed"
			}
			_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
				RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.Name, Status: status, ExitCode: ec, StartedAt: started, EndedAt: time.Now().UTC(),
			})
			if status == "Failed" && !step.ContinueOnError {
				failedFlag.Store(true)
			}
			return
		}
```

Add imports `path`, `fmt`, `io` as needed. `makeRunStep` must capture `mountPath` and `artifactExec` (pass them into the factory or close over them in `orchestrate`).

- [ ] **Step 4: Run unit tests + build**

Run: `go test ./internal/k8sagent/ -run TestOrchestrate -v && go vet ./internal/k8sagent/ && go build ./... && go build -tags k8s ./internal/k8sagent/`
Expected: PASS; existing orchestrate tests still pass; both build variants compile.

- [ ] **Step 5: Integration test (`//go:build k8s`, cluster-required)**

Create `internal/k8sagent/artifact_k8s_test.go` with `//go:build k8s`. Model it
on the existing `agent_integration_test.go` harness (real fake/kind clientset +
mock controller + a real object store on the controller side). Run a claim whose
stages are: (1) `run: echo hi > f.txt` (in the workspace), (2) `uploadArtifact
{name: a, path: .}`, (3) `run: rm -f f.txt`, (4) `downloadArtifact {name: a,
destDir: restored}`, (5) `run: cat restored/f.txt` — assert the run Succeeds and
the final `cat` sees `hi`. Add a top-of-test note that this requires a real
cluster (the sidecar image must be loadable) and does not run in `-short`/CI
without k8s. (If the existing `//go:build k8s` harness cannot pull the alpine
sidecar image in the test cluster, document that as a prerequisite — do not fake
the transfer.)

- [ ] **Step 6: Commit**

```bash
git add internal/k8sagent/agent.go internal/k8sagent/orchestrate_test.go internal/k8sagent/artifact_k8s_test.go
git commit -m "feat(k8sagent): dispatch uploadArtifact/downloadArtifact via the sidecar"
```

---

## Task 3: Docs

**Files:**
- Modify: `docs/kubernetes-integration.md`, `docs/jobs.md`

- [ ] **Step 1: Document k8s artifact support**

In `docs/kubernetes-integration.md`, add a short "Artifacts" subsection: the
k8s-agent supports `uploadArtifact`/`downloadArtifact` via an auto-injected
`unified-artifact` sidecar (alpine + tar/zstd/curl) that shares the pod
workspace; the container name `unified-artifact` is reserved (a podTemplate must
not define a container with that name); the bearer token is set on the sidecar's
env (isolated from job-container steps by the container boundary, but visible in
the pod spec — a Secret-based hardening is a planned follow-up); **cache is not
yet supported on the k8s-agent** (a separate follow-up will route it through the
same sidecar via a controller cache-proxy).

In `docs/jobs.md` Artifacts section, add one line: artifacts work on both the
standard and Kubernetes agents (k8s via the workspace sidecar).

- [ ] **Step 2: Verify + commit**

Run: `go build ./...` (sanity).

```bash
git add docs/kubernetes-integration.md docs/jobs.md
git commit -m "docs: document k8s artifact sidecar support and caveats"
```

---

## Final verification

- [ ] `go test ./internal/k8sagent/ -v` — sidecar injection + artifact-dispatch unit tests pass; existing orchestrate tests unchanged.
- [ ] `go build ./...` and `go build -tags k8s ./internal/k8sagent/` compile.
- [ ] `go test ./... -short` still green.
- [ ] The `//go:build k8s` integration test exists (compiles under `-tags k8s`); documented as cluster-required (not run here).

## Self-review notes (coverage vs spec)

- Sidecar image (tar+zstd+curl) → Task 1 Step 1.
- Auto-inject sidecar with workspace mount + env, reserve name → Task 1 Steps 3/5, both callers Step 6.
- Token via isolated sidecar env → Task 1 Step 5 (env), documented caveat Task 3.
- Dispatch upload/download via sidecar exec, tar+zstd+curl, controller endpoint → Task 2 Step 3.
- `artifactExec` seam → cluster-free unit tests → Task 2 Steps 1/3.
- `//go:build k8s` real round-trip → Task 2 Step 5.
- Cache out of scope, sidecar/seam kept reusable → Global Constraints + Task 3 doc note.
- Docs (behavior + token caveat + cache-unsupported) → Task 3.
