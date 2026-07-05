# runsIn (Plan A: ホスト側) 実装計画

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 通常 agent（`internal/agent`）が `runsIn.image` でステップをコンテナ実行できるようにする。`runsIn` 未指定はホスト実行、`runsIn.container` はホストでは run 時エラー。

**Architecture:** DSL に `runsIn:{image|container}` の排他 union を追加し、フラット `container:` を後方互換エイリアスとして正規化する。`uses` はインライン展開時に `runsIn` を各ステップへ伝播。新パッケージ `internal/runtime` に `ContainerRuntime` ドライバ抽象（OCI-CLI + Apple `container`）を置き、host-agent が `runsIn` を解決して実行を振り分ける。隔離環境は workspace 非共有・入力は env 経由。

**Tech Stack:** Go, `gopkg.in/yaml.v3`, testify (`assert`/`require`), 標準 `os/exec`。

## Global Constraints

- Go モジュール: `github.com/eirueimi/unified-cd`。
- DSL パーサは `KnownFields(true)`（未知フィールドはハードフェイル）。新フィールドは必ず struct タグで定義する。
- `needs:` は廃止済み。並行は `parallel:` のみ。
- テストは testify。パーサ検証は `dsl.Parse` を使う（オフライン `check-jsonschema` は単体 Job に使えない）。
- secret マスクは既存機構（hyphen-aware collector）を壊さない。
- `runsIn.image` 指定でランタイム不在は **run 時ハードエラー**（サイレントにホスト実行へフォールバックしない）。
- `internal/runtime` は `internal/agent` を import しない（依存の向きは agent → runtime）。

---

### Task 1: DSL `RunsIn` 型・正規化・バリデーション

**Files:**
- Modify: `internal/dsl/types.go`（`RunsIn` 型追加、`StepEntry`/`Step` にフィールド追加）
- Modify: `internal/dsl/parse.go`（正規化＋排他バリデーション）
- Test: `internal/dsl/runsin_test.go`（新規）

**Interfaces:**
- Produces:
  - `type RunsIn struct { Image string; Container string }`（`yaml:"image,omitempty"` / `yaml:"container,omitempty"`）
  - `StepEntry.RunsIn *RunsIn` / `Step.RunsIn *RunsIn`（`yaml:"runsIn,omitempty"`）
  - 正規化後: フラット `Container` が使われた場合 `RunsIn.Container` に移し替え、フラット `Container` は空にする。

- [ ] **Step 1: Write the failing test**

`internal/dsl/runsin_test.go`:
```go
package dsl

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func parseSteps(t *testing.T, stepsYAML string) *Job {
	t.Helper()
	input := "apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: j\nspec:\n  steps:\n" + stepsYAML
	job, err := Parse(strings.NewReader(input))
	require.NoError(t, err)
	return job
}

func TestRunsIn_Image(t *testing.T) {
	job := parseSteps(t, "    - name: build\n      run: go build\n      runsIn:\n        image: golang:1.22\n")
	ri := job.Spec.Steps[0].RunsIn
	require.NotNil(t, ri)
	assert.Equal(t, "golang:1.22", ri.Image)
	assert.Equal(t, "", ri.Container)
}

func TestRunsIn_FlatContainerNormalized(t *testing.T) {
	job := parseSteps(t, "    - name: build\n      run: go build\n      container: job\n")
	ri := job.Spec.Steps[0].RunsIn
	require.NotNil(t, ri)
	assert.Equal(t, "job", ri.Container)
	assert.Equal(t, "", job.Spec.Steps[0].Container, "flat container must be cleared after normalization")
}

func TestRunsIn_ImageAndContainerMutuallyExclusive(t *testing.T) {
	input := "apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: j\nspec:\n  steps:\n    - name: s\n      run: x\n      runsIn:\n        image: golang:1.22\n        container: job\n"
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runsIn.image and runsIn.container are mutually exclusive")
}

func TestRunsIn_FlatAndRunsInConflict(t *testing.T) {
	input := "apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: j\nspec:\n  steps:\n    - name: s\n      run: x\n      container: job\n      runsIn:\n        image: golang:1.22\n"
	_, err := Parse(strings.NewReader(input))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot set both container: and runsIn:")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/dsl/ -run TestRunsIn -v`
Expected: FAIL（`RunsIn` フィールド未定義でコンパイルエラー）

- [ ] **Step 3: Add the type and fields**

`internal/dsl/types.go` — `MatrixDimension` の近くに型を追加:
```go
// RunsIn declares the execution context for a step. Image and Container are
// mutually exclusive; both empty (or RunsIn nil) means the default/shared
// environment (host process, or the default pod container on k8s).
//
//	image:     run in a fresh isolated env from this image (host: `<rt> run`;
//	           k8s: a throwaway pod). No workspace is shared — pass inputs via
//	           with:/env, return outputs via outputs:/stdout.
//	container: exec into a pre-provisioned named env (k8s pod container only;
//	           an error on the host agent).
type RunsIn struct {
	Image     string `yaml:"image,omitempty" json:"image,omitempty"`
	Container string `yaml:"container,omitempty" json:"container,omitempty"`
}
```
そして `StepEntry`（`Container` フィールドの直後）に:
```go
	RunsIn *RunsIn `yaml:"runsIn,omitempty"`
```
`Step`（`Container` フィールドの直後）にも同じ行を追加。

- [ ] **Step 4: Normalize and validate in parse.go**

`internal/dsl/parse.go` の `Job.Validate()`（89行〜）内で、各ステップを走査する既存ループの前後に正規化を挟む。ステップ検証を行う関数（foreach/matrix を見ている `validateStepEntry` 相当、296行付近）に以下を追加。まず正規化ヘルパーを parse.go に追加:
```go
// normalizeRunsIn folds the deprecated flat `container:` into RunsIn.Container
// and rejects conflicting/exclusive combinations. path/name are for error text.
func normalizeRunsIn(container string, runsIn *RunsIn, path, name string) (*RunsIn, error) {
	if container != "" && runsIn != nil {
		return nil, fmt.Errorf("%s (%s): cannot set both container: and runsIn:", path, name)
	}
	if container != "" {
		return &RunsIn{Container: container}, nil
	}
	if runsIn != nil && runsIn.Image != "" && runsIn.Container != "" {
		return nil, fmt.Errorf("%s (%s): runsIn.image and runsIn.container are mutually exclusive", path, name)
	}
	return runsIn, nil
}
```
`Job.Validate()` のステップ走査で各 `StepEntry`（および `Parallel` 内の各 `Step`）に対し:
```go
		ri, err := normalizeRunsIn(entry.Container, entry.RunsIn, path, name)
		if err != nil {
			return err
		}
		entry.RunsIn = ri
		entry.Container = ""
		j.Spec.Steps[i] = entry
```
（`Parallel` ブロック内の `Step` も同様に `container`→`runsIn` 正規化する。走査で index を保持し書き戻すこと。既存の foreach/matrix 検証と同じループ内に置く。）

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/dsl/ -run TestRunsIn -v`
Expected: PASS（4 テスト）

- [ ] **Step 6: Run the full dsl suite (regression)**

Run: `go test ./internal/dsl/...`
Expected: PASS（既存テストが `container:` 正規化で壊れていないこと）

- [ ] **Step 7: Commit**

```bash
git add internal/dsl/types.go internal/dsl/parse.go internal/dsl/runsin_test.go
git commit -m "feat(dsl): add runsIn execution-context field with flat container normalization"
```

---

### Task 2: `uses` 展開時の `runsIn` 伝播

**Files:**
- Modify: `internal/gittemplate/inline.go`（インライン化する具体/parallel ステップに `RunsIn` をコピー＋外側 `runsIn` の継承）
- Test: `internal/gittemplate/inline_runsin_test.go`（新規）

**Interfaces:**
- Consumes: `dsl.StepEntry.RunsIn`, `dsl.Step.RunsIn`（Task 1）
- Produces: 展開後の各ステップに、内側 `runsIn` を優先、無ければ `uses` ステップの `runsIn` を継承した値が入る。

- [ ] **Step 1: Write the failing test**

`internal/gittemplate/inline_runsin_test.go`（既存 `inline_test.go` のヘルパー命名に合わせる。テンプレート2ステップ、片方は自前 `runsIn` を持つ）:
```go
package gittemplate

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExpandUses_PropagatesRunsIn(t *testing.T) {
	inner := []dsl.StepEntry{
		{Name: "compile", Run: "go build"},
		{Name: "special", Run: "echo hi", RunsIn: &dsl.RunsIn{Image: "alpine:3"}},
	}
	usesStep := dsl.StepEntry{
		Name:   "tmpl",
		Uses:   &dsl.UsesStep{Git: "repo", Path: "p"},
		RunsIn: &dsl.RunsIn{Image: "golang:1.22"},
	}

	out, err := expandUsesStep(usesStep, inner)
	require.NoError(t, err)
	require.Len(t, out, 2)

	// inherits the uses runsIn
	require.NotNil(t, out[0].RunsIn)
	assert.Equal(t, "golang:1.22", out[0].RunsIn.Image)

	// keeps its own runsIn (no override)
	require.NotNil(t, out[1].RunsIn)
	assert.Equal(t, "alpine:3", out[1].RunsIn.Image)
}
```
（`expandUsesStep` の実シグネチャは `internal/gittemplate/inline.go` を確認して合わせること。第2引数がテンプレート内ステップでない場合は、既存テスト `inline_test.go` の呼び出し方に倣ってテンプレート解決経由で組み立てる。）

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/gittemplate/ -run TestExpandUses_PropagatesRunsIn -v`
Expected: FAIL（`RunsIn` がコピーされず nil）

- [ ] **Step 3: Propagate in inline.go**

`internal/gittemplate/inline.go` の具体ステップ組み立て（`ns := dsl.StepEntry{...}`、168行付近、`Container: inner.Container` の隣）に:
```go
			ns.RunsIn = inner.RunsIn
			if ns.RunsIn == nil {
				ns.RunsIn = usesStep.RunsIn // inherit template-level runsIn
			}
```
`Parallel` ブロック内の各ステップ組み立て（`ns := dsl.Step{...}` 相当）にも同じ継承を追加。`usesStep`（外側 `StepEntry`）がスコープに無ければ引数として渡す。

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/gittemplate/ -run TestExpandUses_PropagatesRunsIn -v`
Expected: PASS

- [ ] **Step 5: Run the gittemplate suite (regression)**

Run: `go test ./internal/gittemplate/...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/gittemplate/inline.go internal/gittemplate/inline_runsin_test.go
git commit -m "feat(gittemplate): propagate runsIn from uses step to inlined steps"
```

---

### Task 3: api 型と controller 変換への `runsIn` 配線

**Files:**
- Modify: `internal/api/types.go`（`ClaimStep` に `RunsIn *dsl.RunsIn`）
- Modify: `internal/controller/api_agent.go`（`buildOneClaimStep` と `stepToStepEntry` で `RunsIn` を伝播）
- Test: `internal/controller/api_agent_runsin_test.go`（新規）

**Interfaces:**
- Consumes: `dsl.RunsIn`（Task 1）
- Produces: `api.ClaimStep.RunsIn *dsl.RunsIn`（agent が読む）

- [ ] **Step 1: Write the failing test**

`internal/controller/api_agent_runsin_test.go`:
```go
package controller

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildOneClaimStep_CopiesRunsIn(t *testing.T) {
	entry := dsl.StepEntry{
		Name:   "build",
		Run:    "go build",
		RunsIn: &dsl.RunsIn{Image: "golang:1.22"},
	}
	cs := buildOneClaimStep(0, 0, entry)
	require.NotNil(t, cs.RunsIn)
	assert.Equal(t, "golang:1.22", cs.RunsIn.Image)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestBuildOneClaimStep_CopiesRunsIn -v`
Expected: FAIL（`cs.RunsIn` 未定義でコンパイルエラー）

- [ ] **Step 3: Add api field and wire conversion**

`internal/api/types.go` の `ClaimStep` に（`Container` の隣）:
```go
	RunsIn *dsl.RunsIn `json:"runsIn,omitempty"`
```
`internal/controller/api_agent.go` の `buildOneClaimStep`、`cs := api.ClaimStep{...}` リテラルに:
```go
		RunsIn: entry.RunsIn,
```
`stepToStepEntry` の返す `dsl.StepEntry{...}` にも `RunsIn: st.RunsIn,` を追加（parallel 内 Step 用）。

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/controller/ -run TestBuildOneClaimStep_CopiesRunsIn -v`
Expected: PASS

- [ ] **Step 5: Regression**

Run: `go test ./internal/controller/... ./internal/api/...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/api/types.go internal/controller/api_agent.go internal/controller/api_agent_runsin_test.go
git commit -m "feat(api): carry runsIn through ClaimStep and controller conversion"
```

---

### Task 4: `internal/runtime` — ドライバ interface ＋ OCI-CLI ドライバ ＋ 検出

**Files:**
- Create: `internal/runtime/runtime.go`（interface + `RunSpec` + `Detect`）
- Create: `internal/runtime/ocicli.go`（docker/podman/nerdctl/wslc 共通ドライバ）
- Test: `internal/runtime/ocicli_test.go`（新規）

**Interfaces:**
- Produces:
  - `type RunSpec struct { Image string; Script string; Env []string; Shell []string }`
  - `type ContainerRuntime interface { Name() string; Available() bool; Pull(ctx context.Context, image string) error; Run(ctx context.Context, spec RunSpec, stdout, stderr io.Writer) (int, error) }`
  - `func Detect(preferred string) (ContainerRuntime, error)`
  - `func (*ociCLI) runArgs(spec RunSpec) []string`（テスト用に小文字だが同パッケージから検証）

- [ ] **Step 1: Write the failing test**

`internal/runtime/ocicli_test.go`:
```go
package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOCICLI_RunArgs_Default(t *testing.T) {
	r := &ociCLI{bin: "docker"}
	args := r.runArgs(RunSpec{
		Image:  "golang:1.22",
		Script: "go build",
		Env:    []string{"FOO=bar", "BAZ=qux"},
	})
	assert.Equal(t, []string{
		"run", "--rm",
		"-e", "FOO=bar",
		"-e", "BAZ=qux",
		"golang:1.22",
		"sh", "-c", "go build",
	}, args)
}

func TestOCICLI_RunArgs_CustomShell(t *testing.T) {
	r := &ociCLI{bin: "podman"}
	args := r.runArgs(RunSpec{
		Image:  "alpine",
		Script: "echo hi",
		Shell:  []string{"bash", "-lc"},
	})
	assert.Equal(t, []string{"run", "--rm", "alpine", "bash", "-lc", "echo hi"}, args)
}

func TestDetect_UnknownPreferredIsError(t *testing.T) {
	_, err := Detect("no-such-runtime-xyz")
	assert.Error(t, err)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/runtime/ -v`
Expected: FAIL（パッケージ未作成でコンパイルエラー）

- [ ] **Step 3: Implement runtime.go**

`internal/runtime/runtime.go`:
```go
// Package runtime abstracts container runtimes behind a small, CRI-inspired
// lifecycle interface (image pull + run). Implementations shell out to a CLI
// (docker/podman/nerdctl/wslc/Apple container) — CRI/gRPC is intentionally
// NOT used; the target runtimes are CLI tools, not CRI endpoints.
package runtime

import (
	"context"
	"fmt"
	"io"
	"os/exec"
)

// RunSpec describes a one-shot containerized step execution.
type RunSpec struct {
	Image  string   // OCI image reference
	Script string   // shell script to run (the step's run:)
	Env    []string // KEY=VALUE, injected as -e
	Shell  []string // entrypoint; defaults to {"sh","-c"}
}

// ContainerRuntime runs a step in a fresh, isolated container. No host
// workspace is mounted — inputs arrive via Env, outputs via stdout.
type ContainerRuntime interface {
	Name() string
	Available() bool
	Pull(ctx context.Context, image string) error
	Run(ctx context.Context, spec RunSpec, stdout, stderr io.Writer) (int, error)
}

// detectOrder is the auto-detection preference order.
var detectOrder = []string{"docker", "podman", "nerdctl", "wslc", "container"}

// Detect returns the first available runtime. If preferred is non-empty, only
// that runtime is considered (and it must be a known driver).
func Detect(preferred string) (ContainerRuntime, error) {
	order := detectOrder
	if preferred != "" {
		order = []string{preferred}
	}
	for _, name := range order {
		r := driverFor(name)
		if r == nil {
			if preferred != "" {
				return nil, fmt.Errorf("unknown container runtime %q", preferred)
			}
			continue
		}
		if r.Available() {
			return r, nil
		}
	}
	return nil, fmt.Errorf("no container runtime available (looked for %v)", order)
}

// driverFor maps a runtime name to a driver, or nil if unknown.
// Apple's "container" driver is added in a later task.
func driverFor(name string) ContainerRuntime {
	switch name {
	case "docker", "podman", "nerdctl", "wslc":
		return &ociCLI{bin: name}
	default:
		return nil
	}
}

// lookPath is indirected for testability.
var lookPath = exec.LookPath
```

- [ ] **Step 4: Implement ocicli.go**

`internal/runtime/ocicli.go`:
```go
package runtime

import (
	"context"
	"io"
	"os/exec"
)

// ociCLI drives any runtime whose CLI is docker-compatible:
// docker, podman, nerdctl, and Microsoft's wslc.
type ociCLI struct {
	bin string
}

func (r *ociCLI) Name() string { return r.bin }

func (r *ociCLI) Available() bool {
	_, err := lookPath(r.bin)
	return err == nil
}

func (r *ociCLI) runArgs(spec RunSpec) []string {
	args := []string{"run", "--rm"}
	for _, e := range spec.Env {
		args = append(args, "-e", e)
	}
	args = append(args, spec.Image)
	shell := spec.Shell
	if len(shell) == 0 {
		shell = []string{"sh", "-c"}
	}
	args = append(args, shell...)
	args = append(args, spec.Script)
	return args
}

func (r *ociCLI) Pull(ctx context.Context, image string) error {
	return exec.CommandContext(ctx, r.bin, "pull", image).Run()
}

func (r *ociCLI) Run(ctx context.Context, spec RunSpec, stdout, stderr io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, r.bin, r.runArgs(spec)...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	return -1, err
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/runtime/ -v`
Expected: PASS（3 テスト）

- [ ] **Step 6: Commit**

```bash
git add internal/runtime/
git commit -m "feat(runtime): container runtime driver abstraction with OCI-CLI driver"
```

---

### Task 5: host-agent 統合 — `runsIn` を runtime/host/error に振り分け

**Files:**
- Modify: `internal/agent/runner.go`（`RunStepContainer` 追加）
- Modify: `internal/agent/agent.go`（ステップ実行分岐、~423 付近）
- Modify: `cmd/agent/main.go`（`--container-runtime` フラグ → agent へ）
- Test: `internal/agent/runner_container_test.go`（新規、runtime のフェイク実装で検証）

**Interfaces:**
- Consumes: `api.ClaimStep.RunsIn`（Task 3）、`runtime.ContainerRuntime` / `runtime.RunSpec` / `runtime.Detect`（Task 4）
- Produces: `func RunStepContainer(ctx context.Context, rt runtime.ContainerRuntime, image, script string, stderr io.Writer, extraEnv []string) (stdout string, exitCode int, err error)`

- [ ] **Step 1: Write the failing test**

`internal/agent/runner_container_test.go`（runtime をフェイクして stdout 捕捉と env 受け渡しを検証。実際の docker には依存しない）:
```go
package agent

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeRuntime struct {
	gotSpec runtime.RunSpec
	stdout  string
	exit    int
}

func (f *fakeRuntime) Name() string    { return "fake" }
func (f *fakeRuntime) Available() bool  { return true }
func (f *fakeRuntime) Pull(context.Context, string) error { return nil }
func (f *fakeRuntime) Run(_ context.Context, spec runtime.RunSpec, stdout, _ io.Writer) (int, error) {
	f.gotSpec = spec
	_, _ = stdout.Write([]byte(f.stdout))
	return f.exit, nil
}

func TestRunStepContainer_CapturesStdoutAndPassesEnv(t *testing.T) {
	f := &fakeRuntime{stdout: "built\n", exit: 0}
	var stderr bytes.Buffer
	out, code, err := RunStepContainer(t.Context(), f, "golang:1.22", "go build",
		&stderr, []string{"FOO=bar"})
	require.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Equal(t, "built\n", out)
	assert.Equal(t, "golang:1.22", f.gotSpec.Image)
	assert.Equal(t, "go build", f.gotSpec.Script)
	assert.Contains(t, f.gotSpec.Env, "FOO=bar")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agent/ -run TestRunStepContainer -v`
Expected: FAIL（`RunStepContainer` 未定義）

- [ ] **Step 3: Implement RunStepContainer**

`internal/agent/runner.go` に追加:
```go
// RunStepContainer runs script inside a fresh container via rt, capturing
// stdout (like RunStepCapture) and streaming stderr to the provided writer.
// No host workspace is mounted — this is the isolated runsIn.image path.
func RunStepContainer(ctx context.Context, rt runtime.ContainerRuntime, image, script string, stderr io.Writer, extraEnv []string) (stdout string, exitCode int, err error) {
	var buf bytes.Buffer
	code, runErr := rt.Run(ctx, runtime.RunSpec{
		Image:  image,
		Script: script,
		Env:    extraEnv,
	}, &buf, stderr)
	return buf.String(), code, runErr
}
```
`import` に `"github.com/eirueimi/unified-cd/internal/runtime"` を追加。

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/agent/ -run TestRunStepContainer -v`
Expected: PASS

- [ ] **Step 5: Wire the dispatch in agent.go**

`internal/agent/agent.go` の `RunStepCapture(stepCtx, expandedRun, ...)` 呼び出し（~423）を分岐に置換:
```go
				var capturedStdout string
				var ec int
				var runErr error
				switch {
				case step.RunsIn != nil && step.RunsIn.Container != "":
					runErr = fmt.Errorf("runsIn.container (%q) is not supported on the host agent; use runsIn.image or the k8s agent", step.RunsIn.Container)
					ec = -1
				case step.RunsIn != nil && step.RunsIn.Image != "":
					rt, derr := a.containerRuntime()
					if derr != nil {
						runErr = fmt.Errorf("runsIn.image %q requires a container runtime: %w", step.RunsIn.Image, derr)
						ec = -1
					} else {
						capturedStdout, ec, runErr = RunStepContainer(stepCtx, rt, step.RunsIn.Image, expandedRun, stderrPusher, extraEnv)
					}
				default:
					capturedStdout, ec, runErr = RunStepCapture(stepCtx, expandedRun, stderrPusher, extraEnv, workDir)
				}
```
`fmt` が未 import なら追加。`step` は `api.ClaimStep`（`RunsIn *dsl.RunsIn`）。

- [ ] **Step 6: Add the cached runtime resolver**

`internal/agent/agent.go` の Agent 構造体に `runtimePref string`（設定注入）と遅延キャッシュ用 `resolvedRuntime runtime.ContainerRuntime` / `runtimeErr error` / `runtimeOnce sync.Once` を追加し:
```go
// containerRuntime resolves (once) the container runtime for runsIn.image
// steps, honoring the configured preference. A missing runtime is a hard
// error surfaced to the step (no silent host fallback).
func (a *Agent) containerRuntime() (runtime.ContainerRuntime, error) {
	a.runtimeOnce.Do(func() {
		a.resolvedRuntime, a.runtimeErr = runtime.Detect(a.runtimePref)
	})
	return a.resolvedRuntime, a.runtimeErr
}
```
（`sync` / `runtime` import を追加。Agent 構造体・コンストラクタの実フィールド名は `internal/agent/agent.go` を確認して合わせる。）

- [ ] **Step 7: Wire the CLI flag**

`cmd/agent/main.go` に `--container-runtime` フラグ（デフォルト空＝自動検出）を追加し、Agent の `runtimePref` に渡す。既存フラグ定義に倣う:
```go
	containerRuntime := flag.String("container-runtime", "", "container runtime for runsIn.image steps (docker|podman|nerdctl|wslc|container); empty = auto-detect")
```
Agent 生成箇所へ `*containerRuntime` を配線。

- [ ] **Step 8: Run agent suite (regression + new)**

Run: `go test ./internal/agent/...`
Expected: PASS

- [ ] **Step 9: Build the agent binary**

Run: `go build ./cmd/agent`
Expected: ビルド成功（フラグ配線・import が通る）

- [ ] **Step 10: Commit**

```bash
git add internal/agent/runner.go internal/agent/agent.go internal/agent/runner_container_test.go cmd/agent/main.go
git commit -m "feat(agent): execute runsIn.image steps in a container, error on runsIn.container"
```

---

### Task 6: Apple `container` ドライバ

**Files:**
- Create: `internal/runtime/apple.go`
- Modify: `internal/runtime/runtime.go`（`driverFor` に `"container"` を追加）
- Test: `internal/runtime/apple_test.go`（新規）

**Interfaces:**
- Consumes: `RunSpec`, `ContainerRuntime`, `lookPath`（Task 4）
- Produces: `driverFor("container")` が `*appleContainer` を返す。

> 注意: Apple の `container` CLI のフラグ表面（`--rm`/`-e`/エントリポイント指定）は実機で要確認。差異があれば `runArgs` を Apple 用に調整すること。ここでは docker 互換を仮置きする。

- [ ] **Step 1: Write the failing test**

`internal/runtime/apple_test.go`:
```go
package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDriverFor_AppleContainer(t *testing.T) {
	r := driverFor("container")
	require.NotNil(t, r)
	assert.Equal(t, "container", r.Name())
}

func TestAppleContainer_RunArgs(t *testing.T) {
	r := &appleContainer{}
	args := r.runArgs(RunSpec{Image: "alpine", Script: "echo hi", Env: []string{"A=b"}})
	assert.Equal(t, []string{"run", "--rm", "-e", "A=b", "alpine", "sh", "-c", "echo hi"}, args)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/runtime/ -run "Apple|DriverFor_AppleContainer" -v`
Expected: FAIL（`appleContainer` 未定義／`driverFor("container")` が nil）

- [ ] **Step 3: Implement apple.go**

`internal/runtime/apple.go`:
```go
package runtime

import (
	"context"
	"io"
	"os/exec"
)

// appleContainer drives Apple's native `container` CLI on macOS. Its surface
// is docker-like; verify flags against the installed CLI and adjust runArgs
// if they diverge.
type appleContainer struct{}

func (a *appleContainer) Name() string { return "container" }

func (a *appleContainer) Available() bool {
	_, err := lookPath("container")
	return err == nil
}

func (a *appleContainer) runArgs(spec RunSpec) []string {
	args := []string{"run", "--rm"}
	for _, e := range spec.Env {
		args = append(args, "-e", e)
	}
	args = append(args, spec.Image)
	shell := spec.Shell
	if len(shell) == 0 {
		shell = []string{"sh", "-c"}
	}
	args = append(args, shell...)
	args = append(args, spec.Script)
	return args
}

func (a *appleContainer) Pull(ctx context.Context, image string) error {
	return exec.CommandContext(ctx, "container", "pull", image).Run()
}

func (a *appleContainer) Run(ctx context.Context, spec RunSpec, stdout, stderr io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, "container", a.runArgs(spec)...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	return -1, err
}
```

- [ ] **Step 4: Register the driver**

`internal/runtime/runtime.go` の `driverFor` の `switch` に:
```go
	case "container":
		return &appleContainer{}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/runtime/ -v`
Expected: PASS（全テスト）

- [ ] **Step 6: Commit**

```bash
git add internal/runtime/apple.go internal/runtime/runtime.go internal/runtime/apple_test.go
git commit -m "feat(runtime): add Apple container CLI driver"
```

---

## 最終確認（全タスク後）

- [ ] `go build ./...` 成功
- [ ] `go test ./...` 全パス
- [ ] `go vet ./...` クリーン
- [ ] docs 再生成が要るか確認: DSL struct を変えたので `go generate ./internal/dsl/`（schemagen/docgen）を実行し、生成物（`schemas/`, `docs/`）の差分をコミット。
- [ ] 手動 smoke: docker がある環境で `runsIn: { image: alpine }` の1ステップ Job を `unified-cd apply` し、コンテナ実行されること／ランタイム無し環境で `runsIn.image` がハードエラーになることを確認。

## Plan A スコープ外（Plan B 以降）

- k8s-agent の `runsIn.image`（使い捨て pod）対応。
- `runsIn.container` の k8s 名前参照実行（既存挙動の `runsIn` 経由への統合）。
- コンテナのリソース制限・ネットワーク・volume 引き継ぎ。
