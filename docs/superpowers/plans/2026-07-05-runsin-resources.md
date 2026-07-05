# runsIn.resources 実装計画（Plan B-2 ①: CPU/メモリ制限）

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `runsIn.image` ステップに CPU/メモリの requests/limits を宣言でき、k8s では使い捨て pod の `container.Resources`、host では OCI-CLI の `--cpus`/`--memory` に反映する。

**Architecture:** DSL `runsIn.resources`（k8s quantity 文字列）を追加し apply 時に検証。`api.ClaimStep.RunsIn` がポインタごと運ぶので配線は agent 側のみ。k8s は `buildImageStepPod` が `container.Resources` を設定、host は agent が limits を（cpu=コア小数/mem=バイト）に変換して `RunSpec` に載せ `ociCLI.runArgs` がフラグ化。

**Tech Stack:** Go, `k8s.io/apimachinery/pkg/api/resource`（quantity 検証・変換）, client-go `corev1`, testify。

## Global Constraints

- Go モジュール: `github.com/eirueimi/unified-cd`。テストは testify。
- 値は k8s quantity 文字列。**apply 時（`dsl.Parse`）に `resource.ParseQuantity` で検証、不正値はハードエラー**。
- `runsIn.resources` は **image ステップ専用**: `Resources` 非 nil かつ `Image` 空 → ハードエラー。未指定は制限なし（既定値・継承なし）。
- **host は limits のみ**マッピング（requests は k8s スケジューリング専用、host では無視）。
- **Apple `container` ドライバは resource 未対応（据え置き）** — `appleContainer.runArgs` は変更しない。
- 既存の①②（デフォルト/named container）挙動は不変。

---

### Task 1: DSL 型・バリデーション・スキーマ再生成

**Files:**
- Modify: `internal/dsl/types.go`（`RunsIn` に `Resources`、`ResourceSpec`/`ResourceList` 追加）
- Modify: `internal/dsl/parse.go`（`normalizeRunsIn` に resources 検証、`validateResources` ヘルパー、`resource` import）
- Test: `internal/dsl/runsin_resources_test.go`（新規）
- 生成物: `schemas/unified-cd.schema.json`, `docs/field-reference.md`（`go generate`）

**Interfaces:**
- Produces:
  - `RunsIn.Resources *ResourceSpec`
  - `type ResourceSpec struct { Requests *ResourceList; Limits *ResourceList }`
  - `type ResourceList struct { CPU string; Memory string }`
  - 検証: 不正 quantity → error、`Resources` 非 nil で `Image` 空 → error（`runsIn.resources requires runsIn.image`）。

- [ ] **Step 1: 失敗するテストを書く**

`internal/dsl/runsin_resources_test.go`:
```go
package dsl

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func parseJob(t *testing.T, stepsYAML string) (*Job, error) {
	t.Helper()
	input := "apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: j\nspec:\n  steps:\n" + stepsYAML
	return Parse(strings.NewReader(input))
}

func TestRunsInResources_Valid(t *testing.T) {
	job, err := parseJob(t, "    - name: build\n      run: go build\n      runsIn:\n        image: golang:1.22\n        resources:\n          requests:\n            cpu: \"500m\"\n            memory: \"256Mi\"\n          limits:\n            cpu: \"1\"\n            memory: \"512Mi\"\n")
	require.NoError(t, err)
	rs := job.Spec.Steps[0].RunsIn.Resources
	require.NotNil(t, rs)
	assert.Equal(t, "500m", rs.Requests.CPU)
	assert.Equal(t, "512Mi", rs.Limits.Memory)
}

func TestRunsInResources_InvalidQuantity(t *testing.T) {
	_, err := parseJob(t, "    - name: build\n      run: go build\n      runsIn:\n        image: golang:1.22\n        resources:\n          limits:\n            memory: \"512Megabytes\"\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resources")
}

func TestRunsInResources_RequiresImage(t *testing.T) {
	_, err := parseJob(t, "    - name: build\n      run: go build\n      runsIn:\n        container: job\n        resources:\n          limits:\n            cpu: \"1\"\n")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runsIn.resources requires runsIn.image")
}
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/dsl/ -run TestRunsInResources -v`
Expected: FAIL（`Resources` 未定義でコンパイルエラー）

- [ ] **Step 3: 型を追加**

`internal/dsl/types.go` の `RunsIn`（157行付近）を拡張し、直後に型を追加:
```go
type RunsIn struct {
	Image     string        `yaml:"image,omitempty" json:"image,omitempty"`
	Container string        `yaml:"container,omitempty" json:"container,omitempty"`
	Resources *ResourceSpec `yaml:"resources,omitempty" json:"resources,omitempty"`
}

// ResourceSpec declares CPU/memory requests and limits for a runsIn.image step.
type ResourceSpec struct {
	Requests *ResourceList `yaml:"requests,omitempty" json:"requests,omitempty"`
	Limits   *ResourceList `yaml:"limits,omitempty" json:"limits,omitempty"`
}

// ResourceList is a cpu/memory pair using Kubernetes quantity strings
// (e.g. "500m", "1", "256Mi", "1Gi").
type ResourceList struct {
	CPU    string `yaml:"cpu,omitempty" json:"cpu,omitempty"`
	Memory string `yaml:"memory,omitempty" json:"memory,omitempty"`
}
```

- [ ] **Step 4: バリデーションを追加**

`internal/dsl/parse.go` の import に `"k8s.io/apimachinery/pkg/api/resource"` を追加。`normalizeRunsIn`（253行）の最後の `return runsIn, nil` の直前に resources 検証を挟み、ヘルパーを追加:
```go
	if runsIn != nil && runsIn.Resources != nil {
		if runsIn.Image == "" {
			return nil, fmt.Errorf("%s (%s): runsIn.resources requires runsIn.image", path, name)
		}
		if err := validateResources(runsIn.Resources); err != nil {
			return nil, fmt.Errorf("%s (%s): %w", path, name, err)
		}
	}
	return runsIn, nil
}

// validateResources parses every non-empty cpu/memory quantity, rejecting
// malformed values at apply time.
func validateResources(rs *ResourceSpec) error {
	for _, rl := range []*ResourceList{rs.Requests, rs.Limits} {
		if rl == nil {
			continue
		}
		for field, v := range map[string]string{"cpu": rl.CPU, "memory": rl.Memory} {
			if v == "" {
				continue
			}
			if _, err := resource.ParseQuantity(v); err != nil {
				return fmt.Errorf("invalid resources %s quantity %q: %w", field, v, err)
			}
		}
	}
	return nil
}
```

- [ ] **Step 5: テストがパスすることを確認**

Run: `go test ./internal/dsl/ -run TestRunsInResources -v`
Expected: PASS（3 テスト）

- [ ] **Step 6: dsl スイート回帰**

Run: `go test ./internal/dsl/...`
Expected: PASS

- [ ] **Step 7: スキーマ/docs 再生成**

Run: `go generate ./internal/dsl/`
Run: `git status --short`
Expected: `schemas/unified-cd.schema.json` と `docs/field-reference.md` が更新され、`runsIn.resources`（`ResourceSpec`/`ResourceList`）が反映される。

- [ ] **Step 8: Commit**

```bash
git add internal/dsl/types.go internal/dsl/parse.go internal/dsl/runsin_resources_test.go schemas/unified-cd.schema.json docs/field-reference.md
git commit -m "feat(dsl): add runsIn.resources with apply-time quantity validation"
```

---

### Task 2: k8s — 使い捨て pod に `container.Resources`

**Files:**
- Modify: `internal/k8sagent/podbuilder.go`（`buildImageStepPod` に resources 引数、`toResourceRequirements` ヘルパー）
- Modify: `internal/k8sagent/agent.go`（`runImageStep` に resources 引数、`stepExec` の image 分岐で `step.RunsIn.Resources` を渡す）
- Test: `internal/k8sagent/podbuilder_resources_test.go`（新規）

**Interfaces:**
- Consumes: `dsl.ResourceSpec`/`dsl.ResourceList`（Task 1）
- Produces:
  - `func buildImageStepPod(runID, namespace, image string, env map[string]string, deadlineSeconds int64, resources *dsl.ResourceSpec) *corev1.Pod`
  - `func (a *K8sAgent) runImageStep(ctx, runID, image string, env map[string]string, deadlineSeconds int64, resources *dsl.ResourceSpec, script string, stdout, stderr io.Writer) (int, error)`

- [ ] **Step 1: 失敗するテストを書く**

`internal/k8sagent/podbuilder_resources_test.go`:
```go
package k8sagent

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestBuildImageStepPod_Resources(t *testing.T) {
	rs := &dsl.ResourceSpec{
		Requests: &dsl.ResourceList{CPU: "500m", Memory: "256Mi"},
		Limits:   &dsl.ResourceList{CPU: "1", Memory: "512Mi"},
	}
	pod := buildImageStepPod("r", "ci", "golang:1.22", nil, 3600, rs)
	c := pod.Spec.Containers[0]
	assert.True(t, c.Resources.Requests[corev1.ResourceCPU].Equal(resource.MustParse("500m")))
	assert.True(t, c.Resources.Requests[corev1.ResourceMemory].Equal(resource.MustParse("256Mi")))
	assert.True(t, c.Resources.Limits[corev1.ResourceCPU].Equal(resource.MustParse("1")))
	assert.True(t, c.Resources.Limits[corev1.ResourceMemory].Equal(resource.MustParse("512Mi")))
}

func TestBuildImageStepPod_NilResources(t *testing.T) {
	pod := buildImageStepPod("r", "ci", "busybox", nil, 3600, nil)
	c := pod.Spec.Containers[0]
	require.Empty(t, c.Resources.Requests)
	require.Empty(t, c.Resources.Limits)
}
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/k8sagent/ -run TestBuildImageStepPod_Resources -v`
Expected: FAIL（`buildImageStepPod` の引数個数不一致でコンパイルエラー）

- [ ] **Step 3: `buildImageStepPod` に resources を実装**

`internal/k8sagent/podbuilder.go`。import に `"k8s.io/apimachinery/pkg/api/resource"` を追加（`corev1`/`dsl` は使用済み）。シグネチャに `resources *dsl.ResourceSpec` を追加し、コンテナに `Resources` を設定、ヘルパーを追加:
```go
func buildImageStepPod(runID, namespace, image string, env map[string]string, deadlineSeconds int64, resources *dsl.ResourceSpec) *corev1.Pod {
```
コンテナリテラルの `Env: envVars,` の直後に:
```go
				Resources: toResourceRequirements(resources),
```
ファイル末尾にヘルパー:
```go
// toResourceRequirements converts a validated dsl.ResourceSpec to k8s
// ResourceRequirements. Quantities are already validated at apply time, so a
// parse error here is treated defensively (the value is skipped).
func toResourceRequirements(rs *dsl.ResourceSpec) corev1.ResourceRequirements {
	var req corev1.ResourceRequirements
	if rs == nil {
		return req
	}
	fill := func(rl *dsl.ResourceList) corev1.ResourceList {
		if rl == nil {
			return nil
		}
		out := corev1.ResourceList{}
		if q, err := resource.ParseQuantity(rl.CPU); rl.CPU != "" && err == nil {
			out[corev1.ResourceCPU] = q
		}
		if q, err := resource.ParseQuantity(rl.Memory); rl.Memory != "" && err == nil {
			out[corev1.ResourceMemory] = q
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	req.Requests = fill(rs.Requests)
	req.Limits = fill(rs.Limits)
	return req
}
```

- [ ] **Step 4: `runImageStep` と `stepExec` を配線**

`internal/k8sagent/agent.go`:
- `runImageStep` のシグネチャに `resources *dsl.ResourceSpec` を追加（`deadlineSeconds int64,` の直後、`script string,` の前）し、内部の `buildImageStepPod(runID, a.cfg.Namespace, image, env, deadlineSeconds)` 呼び出しを `buildImageStepPod(runID, a.cfg.Namespace, image, env, deadlineSeconds, resources)` に変更。
- `stepExec` の image 分岐（203行）の `a.runImageStep(execCtx, c.RunID, step.RunsIn.Image, env, deadline, expandedRun, stdoutWriter, stderrPusher)` を、`deadline,` の直後に `step.RunsIn.Resources,` を挟む:
```go
			ec, execErr = a.runImageStep(execCtx, c.RunID, step.RunsIn.Image, env, deadline, step.RunsIn.Resources, expandedRun, stdoutWriter, stderrPusher)
```

- [ ] **Step 5: テストがパスすることを確認**

Run: `go test ./internal/k8sagent/ -run TestBuildImageStepPod -v`
Expected: PASS（resources + 既存 pod テスト）

- [ ] **Step 6: k8s パッケージ回帰**

Run: `go build ./... && go test ./internal/k8sagent/ -short`
Expected: ビルド成功、既存テスト PASS（`runimage_test.go` の `runImageStep` 呼び出しは引数追加で更新が要る場合は最小修正）。

- [ ] **Step 7: Commit**

```bash
git add internal/k8sagent/podbuilder.go internal/k8sagent/agent.go internal/k8sagent/podbuilder_resources_test.go
git commit -m "feat(k8sagent): apply runsIn.resources to the throwaway image pod"
```

---

### Task 3: host — OCI-CLI の `--cpus`/`--memory`

**Files:**
- Modify: `internal/runtime/runtime.go`（`RunSpec` に `CPULimit`/`MemLimit`）
- Modify: `internal/runtime/ocicli.go`（`runArgs` にフラグ）
- Modify: `internal/agent/runner.go`（`RunStepContainer` に limits 引数）
- Modify: `internal/agent/agent.go`（`hostContainerLimits` ヘルパー、host dispatch で変換して渡す）
- Test: `internal/runtime/ocicli_resources_test.go`（新規）、`internal/agent/runner_resources_test.go`（新規）

**Interfaces:**
- Consumes: `dsl.ResourceSpec`（Task 1）
- Produces:
  - `RunSpec.CPULimit string`（コア小数）, `RunSpec.MemLimit string`（バイト）
  - `func RunStepContainer(ctx, rt crt.ContainerRuntime, image, script string, stderr io.Writer, extraEnv []string, cpuLimit, memLimit string) (stdout string, exitCode int, err error)`
  - `func hostContainerLimits(rs *dsl.ResourceSpec) (cpu, mem string)`（内部関数、`internal/agent`）

- [ ] **Step 1: 失敗するテストを書く（runtime）**

`internal/runtime/ocicli_resources_test.go`:
```go
package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOCICLI_RunArgs_Resources(t *testing.T) {
	r := &ociCLI{bin: "docker"}
	args := r.runArgs(RunSpec{
		Image:    "golang:1.22",
		Script:   "go build",
		CPULimit: "0.5",
		MemLimit: "536870912",
	})
	assert.Equal(t, []string{
		"run", "--rm",
		"--cpus", "0.5",
		"--memory", "536870912",
		"golang:1.22",
		"sh", "-c", "go build",
	}, args)
}

func TestOCICLI_RunArgs_NoResources(t *testing.T) {
	r := &ociCLI{bin: "docker"}
	args := r.runArgs(RunSpec{Image: "alpine", Script: "true"})
	assert.Equal(t, []string{"run", "--rm", "alpine", "sh", "-c", "true"}, args)
}
```

- [ ] **Step 2: RED 確認**

Run: `go test ./internal/runtime/ -run TestOCICLI_RunArgs_Resources -v`
Expected: FAIL（`CPULimit`/`MemLimit` 未定義）

- [ ] **Step 3: RunSpec 拡張 + runArgs フラグ**

`internal/runtime/runtime.go` の `RunSpec` に追加:
```go
	CPULimit string // container CPU limit in cores (e.g. "0.5"); empty = no limit
	MemLimit string // container memory limit in bytes (e.g. "536870912"); empty = no limit
```
`internal/runtime/ocicli.go` の `runArgs`、`args := []string{"run", "--rm"}` の直後に:
```go
	if spec.CPULimit != "" {
		args = append(args, "--cpus", spec.CPULimit)
	}
	if spec.MemLimit != "" {
		args = append(args, "--memory", spec.MemLimit)
	}
```
（`appleContainer.runArgs` は変更しない — Apple は resource 未対応・据え置き。）

- [ ] **Step 4: GREEN 確認（runtime）**

Run: `go test ./internal/runtime/ -v`
Expected: PASS（新規 + 既存 ociCLI/apple/Detect テスト）

- [ ] **Step 5: 失敗するテストを書く（host 変換 + RunStepContainer）**

`internal/agent/runner_resources_test.go`:
```go
package agent

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
)

func TestHostContainerLimits(t *testing.T) {
	// nil / no-limits → empty
	c, m := hostContainerLimits(nil)
	assert.Equal(t, "", c)
	assert.Equal(t, "", m)

	// limits only; requests ignored on host
	rs := &dsl.ResourceSpec{
		Requests: &dsl.ResourceList{CPU: "250m", Memory: "128Mi"},
		Limits:   &dsl.ResourceList{CPU: "500m", Memory: "512Mi"},
	}
	c, m = hostContainerLimits(rs)
	assert.Equal(t, "0.5", c)          // 500m -> 0.5 cores
	assert.Equal(t, "536870912", m)    // 512Mi -> bytes
}
```

- [ ] **Step 6: RED 確認（host）**

Run: `go test ./internal/agent/ -run TestHostContainerLimits -v`
Expected: FAIL（`hostContainerLimits` 未定義）

- [ ] **Step 7: `hostContainerLimits` + `RunStepContainer` + dispatch を実装**

`internal/agent/agent.go` に import `"k8s.io/apimachinery/pkg/api/resource"`, `"strconv"`（未 import なら）を追加し、ヘルパー:
```go
// hostContainerLimits converts a validated runsIn.resources spec to the OCI-CLI
// limit values: cpu as a core decimal, memory as bytes. Only limits map on the
// host (docker has no request concept); requests are k8s-scheduling-only.
func hostContainerLimits(rs *dsl.ResourceSpec) (cpu, mem string) {
	if rs == nil || rs.Limits == nil {
		return "", ""
	}
	if rs.Limits.CPU != "" {
		if q, err := resource.ParseQuantity(rs.Limits.CPU); err == nil {
			cpu = strconv.FormatFloat(float64(q.MilliValue())/1000.0, 'g', -1, 64)
		}
	}
	if rs.Limits.Memory != "" {
		if q, err := resource.ParseQuantity(rs.Limits.Memory); err == nil {
			mem = strconv.FormatInt(q.Value(), 10)
		}
	}
	return cpu, mem
}
```
`internal/agent/runner.go` の `RunStepContainer` にシグネチャ `cpuLimit, memLimit string` を追加し、`RunSpec` に反映:
```go
func RunStepContainer(ctx context.Context, rt crt.ContainerRuntime, image, script string, stderr io.Writer, extraEnv []string, cpuLimit, memLimit string) (stdout string, exitCode int, err error) {
	var buf bytes.Buffer
	code, runErr := rt.Run(ctx, crt.RunSpec{
		Image:    image,
		Script:   script,
		Env:      extraEnv,
		CPULimit: cpuLimit,
		MemLimit: memLimit,
	}, &buf, stderr)
	return buf.String(), code, runErr
}
```
`internal/agent/agent.go` の host dispatch（461行）の呼び出しを、変換した limits を渡すよう変更:
```go
						cpuLimit, memLimit := hostContainerLimits(step.RunsIn.Resources)
						capturedStdout, ec, runErr = RunStepContainer(stepCtx, rt, step.RunsIn.Image, expandedRun, stderrPusher, extraEnv, cpuLimit, memLimit)
```

- [ ] **Step 8: GREEN 確認（host）**

Run: `go test ./internal/agent/ -run "TestHostContainerLimits|TestRunStepContainer" -v`
Expected: PASS（既存 `TestRunStepContainer` は引数追加で最小修正が要る場合は修正）

- [ ] **Step 9: 回帰 + ビルド**

Run: `go build ./... && go test ./internal/runtime/... ./internal/agent/... -short`
Expected: ビルド成功、テスト PASS。

- [ ] **Step 10: Commit**

```bash
git add internal/runtime/runtime.go internal/runtime/ocicli.go internal/runtime/ocicli_resources_test.go internal/agent/runner.go internal/agent/agent.go internal/agent/runner_resources_test.go
git commit -m "feat(agent): map runsIn.resources limits to OCI-CLI --cpus/--memory on the host"
```

---

## 最終確認（全タスク後）

- [ ] `go build ./...` 成功
- [ ] `go test ./internal/dsl/... ./internal/runtime/... ./internal/agent/... ./internal/k8sagent/... -short` 全パス
- [ ] `go vet ./...` クリーン
- [ ] スキーマに `runsIn.resources` が反映されている（`grep resources schemas/unified-cd.schema.json`）
- [ ] 手動 smoke（任意）: `runsIn:{image: alpine, resources:{limits:{memory: "64Mi"}}}` の Job を host（docker）で流し、`--memory` が効くこと／k8s で `container.Resources` が付くこと。

## スコープ外（別イテレーション）

- resources の PodTemplate/agent 既定・二段継承。
- ネットワーク / NetworkPolicy（Plan B-2 ②）。
- volume の選択的引き継ぎ（Plan B-2 ③）。
- Apple `container` CLI の resource フラグ実機検証。
