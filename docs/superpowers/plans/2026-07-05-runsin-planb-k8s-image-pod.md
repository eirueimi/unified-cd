# runsIn Plan B — k8s `runsIn.image` 使い捨て pod 実装計画

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** k8s-agent で `runsIn.image: X` のステップを、image X を載せた使い捨て pod で隔離実行し、実行後に削除できるようにする（Plan A のハードエラーガードを実装に置換）。

**Architecture:** `buildImageStepPod` で単一コンテナ（`sleep infinity`、workspace/sidecar 無し、`container.Env` に展開済み env）の PodSpec を作り、`runImageStep` が `CreatePod → WaitForPodRunning → ExecStep("step") → DeletePod` を実行する。`stepExec` クロージャで image ステップをこの経路へディスパッチする。テスト容易化のため pm/exec を狭いインターフェイスにする。

**Tech Stack:** Go, client-go v0.36.1（`k8s.io/api/core/v1`, `k8s.io/client-go/kubernetes/fake`）, testify。

## Global Constraints

- Go モジュール: `github.com/eirueimi/unified-cd`。テストは testify。
- **no-silent-fallback**: image pull 失敗・pod 起動失敗・exec 失敗はすべてステップ失敗として明示（黙ってデフォルトコンテナ実行にしない）。
- 隔離: image pod は workspace volume も artifact sidecar も持たない。入力は `container.Env`、出力は stdout。
- `imagePullSecrets` は明示設定しない（既存 `BuildPod` と同様、namespace の default ServiceAccount に委ねる）。
- 既存の①デフォルト pod コンテナ / ②named container exec の挙動は不変。
- secret masker 経路（stdout/stderr）を既存と同一に保つ。
- 使い捨て pod は毎回新規（プーリングは使わない）。cancel/失敗時も必ず削除。orphan backstop に `activeDeadlineSeconds`。

---

### Task 1: `buildImageStepPod`（PodSpec ビルダー）

**Files:**
- Modify: `internal/k8sagent/podbuilder.go`（末尾に `buildImageStepPod` を追加）
- Test: `internal/k8sagent/podbuilder_image_test.go`（新規）

**Interfaces:**
- Produces:
  - `func buildImageStepPod(runID, namespace, image string, env map[string]string, deadlineSeconds int64) *corev1.Pod`
  - 生成 Pod: `GenerateName: "ucd-img-<runID先頭16>-"`, labels `{app: unified-cd-agent, unified-cd/runId: runID}`, 単一コンテナ `{Name:"step", Image:image, Command:["sleep","infinity"], Env: <env をソート済みで>}`, `RestartPolicy: Never`, `ActiveDeadlineSeconds: &deadlineSeconds`, workspace volume 無し・sidecar 無し。

- [ ] **Step 1: 失敗するテストを書く**

`internal/k8sagent/podbuilder_image_test.go`:
```go
package k8sagent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestBuildImageStepPod(t *testing.T) {
	pod := buildImageStepPod("run-abcdef0123456789xyz", "ci", "alpine:3.20",
		map[string]string{"FOO": "bar", "UNIFIED_AGENT_OS": "linux"}, 1800)

	// naming + labels (GenerateName suffix = first 16 chars of runID: "run-abcdef012345")
	assert.Equal(t, "ucd-img-run-abcdef012345-", pod.GenerateName)
	assert.Empty(t, pod.Name, "must use GenerateName, not a fixed Name")
	assert.Equal(t, "ci", pod.Namespace)
	assert.Equal(t, "unified-cd-agent", pod.Labels["app"])
	assert.Equal(t, "run-abcdef0123456789xyz", pod.Labels["unified-cd/runId"])

	// single container, sleep infinity, image
	require.Len(t, pod.Spec.Containers, 1)
	c := pod.Spec.Containers[0]
	assert.Equal(t, "step", c.Name)
	assert.Equal(t, "alpine:3.20", c.Image)
	assert.Equal(t, []string{"sleep", "infinity"}, c.Command)

	// env present (sorted, deterministic)
	require.Len(t, c.Env, 2)
	assert.Equal(t, "FOO", c.Env[0].Name)
	assert.Equal(t, "bar", c.Env[0].Value)
	assert.Equal(t, "UNIFIED_AGENT_OS", c.Env[1].Name)
	assert.Equal(t, "linux", c.Env[1].Value)

	// isolation: no workspace volume, no sidecar container
	assert.Empty(t, pod.Spec.Volumes, "image pod must not mount a workspace volume")
	for _, cc := range pod.Spec.Containers {
		assert.NotEqual(t, artifactSidecarName, cc.Name, "image pod must not inject the artifact sidecar")
	}

	// lifecycle guards
	assert.Equal(t, corev1.RestartPolicyNever, pod.Spec.RestartPolicy)
	require.NotNil(t, pod.Spec.ActiveDeadlineSeconds)
	assert.Equal(t, int64(1800), *pod.Spec.ActiveDeadlineSeconds)
}

func TestBuildImageStepPod_EmptyEnv(t *testing.T) {
	pod := buildImageStepPod("r", "ci", "busybox", nil, 3600)
	assert.Empty(t, pod.Spec.Containers[0].Env)
	assert.Equal(t, "ucd-img-r-", pod.GenerateName)
}
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/k8sagent/ -run TestBuildImageStepPod -v`
Expected: FAIL（`buildImageStepPod` 未定義でコンパイルエラー）

- [ ] **Step 3: `buildImageStepPod` を実装**

`internal/k8sagent/podbuilder.go` の末尾に追加（既存 import に `sort` が無ければ追加）:
```go
// buildImageStepPod builds a throwaway pod that runs a single runsIn.image step
// in isolation: one container from the given image, kept alive with
// `sleep infinity` so the step script can be exec'd into it. No workspace
// volume and no artifact sidecar are attached (inputs arrive via env, output
// via stdout). imagePullSecrets are intentionally NOT set — the pod uses the
// namespace's default ServiceAccount, exactly like BuildPod.
func buildImageStepPod(runID, namespace, image string, env map[string]string, deadlineSeconds int64) *corev1.Pod {
	suffix := runID
	if len(suffix) > 16 {
		suffix = suffix[:16]
	}

	// Deterministic, sorted env for a stable PodSpec (and stable tests).
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	envVars := make([]corev1.EnvVar, 0, len(keys))
	for _, k := range keys {
		envVars = append(envVars, corev1.EnvVar{Name: k, Value: env[k]})
	}

	deadline := deadlineSeconds
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: fmt.Sprintf("ucd-img-%s-", suffix),
			Namespace:    namespace,
			Labels: map[string]string{
				"app":              "unified-cd-agent",
				"unified-cd/runId": runID,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:         corev1.RestartPolicyNever,
			ActiveDeadlineSeconds: &deadline,
			Containers: []corev1.Container{{
				Name:    "step",
				Image:   image,
				Command: []string{"sleep", "infinity"},
				Env:     envVars,
			}},
		},
	}
}
```
（`corev1 "k8s.io/api/core/v1"`, `metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"`, `fmt`, `sort` を import。既存の podbuilder.go は corev1/metav1/fmt を既に import 済みのはず。`sort` のみ追加が必要なら追加。）

- [ ] **Step 4: テストがパスすることを確認**

Run: `go test ./internal/k8sagent/ -run TestBuildImageStepPod -v`
Expected: PASS（2 テスト）

- [ ] **Step 5: Commit**

```bash
git add internal/k8sagent/podbuilder.go internal/k8sagent/podbuilder_image_test.go
git commit -m "feat(k8sagent): buildImageStepPod for isolated runsIn.image throwaway pods"
```

---

### Task 2: テスト容易化インターフェイス ＋ `runImageStep`

**Files:**
- Modify: `internal/k8sagent/agent.go`（`podManager`/`stepExecutor` インターフェイス追加、`K8sAgent` フィールド型変更、`runImageStep` 追加）
- Test: `internal/k8sagent/runimage_test.go`（新規、フェイクで検証）

**Interfaces:**
- Consumes: `buildImageStepPod`（Task 1）
- Produces:
  - `type podManager interface { CreatePod(ctx, *corev1.Pod)(*corev1.Pod,error); WaitForPodRunning(ctx, name string) error; DeletePod(ctx, name string) error }`
  - `type stepExecutor interface { ExecStep(ctx, podName, container, script string, stdout, stderr io.Writer)(int,error); ExecStepArgv(ctx, podName, container string, argv []string, stdout, stderr io.Writer)(int,error) }`
  - `func (a *K8sAgent) runImageStep(ctx context.Context, runID, image string, env map[string]string, deadlineSeconds int64, script string, stdout, stderr io.Writer) (int, error)`
    - CreatePod→（defer DeletePod, 非キャンセル）→WaitForPodRunning→ExecStep(name,"step",script,stdout,stderr) を実行し `(exitCode, err)` を返す。

- [ ] **Step 1: 失敗するテストを書く**

`internal/k8sagent/runimage_test.go`（`*PodManager`/`*Executor` を使わず、インターフェイスをフェイクして順序・引数・クリーンアップを検証）:
```go
package k8sagent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

type fakePM struct {
	created   *corev1.Pod
	createdNm string
	waitErr   error
	deleted   []string
}

func (f *fakePM) CreatePod(_ context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	f.created = pod
	out := pod.DeepCopy()
	out.Name = "ucd-img-generated-xyz" // simulate server-assigned name from GenerateName
	f.createdNm = out.Name
	return out, nil
}
func (f *fakePM) WaitForPodRunning(_ context.Context, _ string) error { return f.waitErr }
func (f *fakePM) DeletePod(_ context.Context, name string) error {
	f.deleted = append(f.deleted, name)
	return nil
}

type fakeExec struct {
	gotPod, gotContainer, gotScript string
	stdout                          string
	exit                            int
	err                             error
}

func (f *fakeExec) ExecStep(_ context.Context, podName, container, script string, stdout, _ io.Writer) (int, error) {
	f.gotPod, f.gotContainer, f.gotScript = podName, container, script
	_, _ = stdout.Write([]byte(f.stdout))
	return f.exit, f.err
}
func (f *fakeExec) ExecStepArgv(context.Context, string, string, []string, io.Writer, io.Writer) (int, error) {
	return 0, nil
}

func TestRunImageStep_CreatesExecsDeletes(t *testing.T) {
	pm := &fakePM{}
	ex := &fakeExec{stdout: "hi\n", exit: 0}
	a := &K8sAgent{pm: pm, exec: ex}

	var out, errBuf bytes.Buffer
	code, err := a.runImageStep(context.Background(), "run-1", "alpine:3.20",
		map[string]string{"FOO": "bar"}, 1800, "echo hi", &out, &errBuf)

	require.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Equal(t, "hi\n", out.String())
	// created the right pod
	require.NotNil(t, pm.created)
	assert.Equal(t, "alpine:3.20", pm.created.Spec.Containers[0].Image)
	// exec targeted the created pod + "step" container + script
	assert.Equal(t, "ucd-img-generated-xyz", ex.gotPod)
	assert.Equal(t, "step", ex.gotContainer)
	assert.Equal(t, "echo hi", ex.gotScript)
	// pod deleted exactly once
	assert.Equal(t, []string{"ucd-img-generated-xyz"}, pm.deleted)
}

func TestRunImageStep_DeletesOnWaitFailure(t *testing.T) {
	pm := &fakePM{waitErr: errors.New("ImagePullBackOff")}
	ex := &fakeExec{}
	a := &K8sAgent{pm: pm, exec: ex}

	code, err := a.runImageStep(context.Background(), "run-1", "no/such:img",
		nil, 3600, "true", io.Discard, io.Discard)

	require.Error(t, err)
	assert.Equal(t, -1, code)
	assert.Empty(t, ex.gotPod, "exec must not run when the pod never becomes ready")
	// cleanup still happened despite the failure
	assert.Equal(t, []string{"ucd-img-generated-xyz"}, pm.deleted)
}
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/k8sagent/ -run TestRunImageStep -v`
Expected: FAIL（`runImageStep` 未定義、`K8sAgent{pm:...,exec:...}` がインターフェイス型でないためコンパイルエラー）

- [ ] **Step 3: インターフェイス導入＋フィールド型変更**

`internal/k8sagent/agent.go` の `K8sAgent` 構造体定義（35-40行付近）付近に型を追加し、フィールド型を変更:
```go
// podManager and stepExecutor are the narrow slices of *PodManager / *Executor
// that K8sAgent depends on. Interfaces (satisfied by the concrete types) make
// pod-lifecycle and exec paths unit-testable with fakes.
type podManager interface {
	CreatePod(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error)
	WaitForPodRunning(ctx context.Context, name string) error
	DeletePod(ctx context.Context, name string) error
}

type stepExecutor interface {
	ExecStep(ctx context.Context, podName, container, script string, stdout, stderr io.Writer) (int, error)
	ExecStepArgv(ctx context.Context, podName, container string, argv []string, stdout, stderr io.Writer) (int, error)
}
```
`K8sAgent` の `pm *PodManager` を `pm podManager` に、`exec *Executor` を `exec stepExecutor` に変更。`NewK8sAgent(cfg, agentClient, pm *PodManager, exec *Executor, pool *PodPool)` の引数は具象のままでよい（代入時に自動でインターフェイスを満たす）。`corev1`/`io`/`context` の import を確認（agent.go は既に使用済み）。

- [ ] **Step 4: `runImageStep` を実装**

`internal/k8sagent/agent.go` に追加（`execContainer` の近く、末尾ヘルパー群）:
```go
// runImageStep runs a runsIn.image step in a throwaway, isolated pod: create a
// pod from the image (kept alive with sleep infinity), wait until it is
// running, exec the step's script into the single "step" container, then delete
// the pod. The pod is always deleted (defer, non-cancellable context) so a
// cancelled or failed step never leaks a pod. A failure to create/start the pod
// is a hard error surfaced to the step — never a silent fallback.
func (a *K8sAgent) runImageStep(ctx context.Context, runID, image string, env map[string]string, deadlineSeconds int64, script string, stdout, stderr io.Writer) (int, error) {
	pod := buildImageStepPod(runID, a.cfg.Namespace, image, env, deadlineSeconds)
	created, err := a.pm.CreatePod(ctx, pod)
	if err != nil {
		return -1, fmt.Errorf("runsIn.image %q: create pod: %w", image, err)
	}
	name := created.Name
	defer func() {
		if derr := a.pm.DeletePod(context.WithoutCancel(ctx), name); derr != nil {
			slog.Warn("k8s: failed to delete image-step pod", "pod", name, "error", derr)
		}
	}()

	if err := a.pm.WaitForPodRunning(ctx, name); err != nil {
		return -1, fmt.Errorf("runsIn.image %q: pod did not start: %w", image, err)
	}
	return a.exec.ExecStep(ctx, name, "step", script, stdout, stderr)
}
```
（`context`, `fmt`, `log/slog` は agent.go で import 済み。）

- [ ] **Step 5: テストがパスすることを確認**

Run: `go test ./internal/k8sagent/ -run TestRunImageStep -v`
Expected: PASS（2 テスト）

- [ ] **Step 6: パッケージのリグレッション（型変更の波及確認）**

Run: `go build ./... && go test ./internal/k8sagent/ -short`
Expected: ビルド成功、既存テスト PASS（`NewK8sAgent` に具象 `*PodManager`/`*Executor` を渡す既存コード・テストはインターフェイスを満たすのでそのまま通る）

- [ ] **Step 7: Commit**

```bash
git add internal/k8sagent/agent.go internal/k8sagent/runimage_test.go
git commit -m "feat(k8sagent): runImageStep runs runsIn.image in a throwaway pod (create/wait/exec/delete)"
```

---

### Task 3: ディスパッチャ配線 ＋ env 展開 ＋ ガード撤去

**Files:**
- Modify: `internal/k8sagent/agent.go`（`stepExec` の分岐、`runsInImageUnsupported` 削除、env 展開ヘルパー、orchestrate 呼び出し）
- Test: `internal/k8sagent/agent_runsin_test.go`（既存。ガードテストを置換）

**Interfaces:**
- Consumes: `runImageStep`（Task 2）、`buildImageStepPod`（Task 1）
- Produces: image ステップは使い捨て pod 経路、それ以外は共有 pod exec。`step.Env` は orchestrate でテンプレート展開されて image コンテナに載る。

- [ ] **Step 1: 失敗するテストを書く（ガードテストを置換）**

`internal/k8sagent/agent_runsin_test.go` の `TestRunsInImageUnsupported_OnK8s` 関数を**削除**し、代わりに env 展開ヘルパーのテストを追加（ディスパッチ自体は runImageStep のフェイクテストで担保済みなので、ここでは新規ヘルパーを検証）:
```go
func TestExpandStepEnv(t *testing.T) {
	td := dsl.TemplateData{Stdout: "v1"}
	// literal passes through; a template value is expanded
	out := expandStepEnv(map[string]string{
		"LIT": "plain",
		"TPL": "${{ stdout }}",
	}, td)
	assert.Equal(t, "plain", out["LIT"])
	assert.Equal(t, "v1", out["TPL"])
	// nil in, nil-safe out
	assert.Nil(t, expandStepEnv(nil, td))
}
```
（既存 import に `dsl` があることを確認。無ければ `"github.com/eirueimi/unified-cd/internal/dsl"` を追加。`runsInImageUnsupported` を参照していた行はテスト削除で消える。）

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/k8sagent/ -run TestExpandStepEnv -v`
Expected: FAIL（`expandStepEnv` 未定義）

- [ ] **Step 3: env 展開ヘルパーを追加＋ガード撤去**

`internal/k8sagent/agent.go`:
1. `runsInImageUnsupported` 関数（634-642行付近）を**削除**。
2. env 展開ヘルパーを追加:
```go
// expandStepEnv template-expands each env value against the run's template data
// so a runsIn.image container receives resolved values (mirrors the host agent).
func expandStepEnv(env map[string]string, td dsl.TemplateData) map[string]string {
	if len(env) == 0 {
		return nil
	}
	out := make(map[string]string, len(env)+1)
	for k, v := range env {
		ev, err := dsl.ExpandTemplate(v, td)
		if err != nil {
			ev = v
		}
		out[k] = ev
	}
	return out
}
```

- [ ] **Step 4: `stepExec` を分岐に置換**

`internal/k8sagent/agent.go` の `stepExec` クロージャ（166-178行）を次に置換（ガード呼び出しを撤去し、image ステップを `runImageStep` へルーティング）:
```go
	stepExec := func(execCtx context.Context, step api.ClaimStep, expandedRun string) (int, string, error) {
		var stdoutBuf strings.Builder
		stderrPusher := agentlib.NewLogPusher(a.client, a.cfg.AgentID, c.RunID, step.Index, "stderr")
		stdoutWriter := io.MultiWriter(&stdoutBuf, &logLineWriter{
			client: a.client, agentID: a.cfg.AgentID, runID: c.RunID, stepIdx: step.Index, stream: "stdout",
		})

		var ec int
		var execErr error
		if step.RunsIn != nil && step.RunsIn.Image != "" {
			// Isolated throwaway pod. UNIFIED_AGENT_OS mirrors the host agent's
			// convention; step.Env arrives already template-expanded (orchestrate).
			env := step.Env
			if env == nil {
				env = map[string]string{}
			}
			env["UNIFIED_AGENT_OS"] = runtime.GOOS
			deadline := int64(3600)
			if step.TimeoutMinutes > 0 {
				deadline = int64(step.TimeoutMinutes * 60)
			}
			ec, execErr = a.runImageStep(execCtx, c.RunID, step.RunsIn.Image, env, deadline, expandedRun, stdoutWriter, stderrPusher)
		} else {
			ec, execErr = a.exec.ExecStep(execCtx, podName, execContainer(step), expandedRun, stdoutWriter, stderrPusher)
		}

		stderrPusher.Flush(execCtx)
		return ec, stdoutBuf.String(), execErr
	}
```
（`runtime`（stdlib）を agent.go に import。既に無ければ `"runtime"` を追加。`step.Env` を直接書き換えず一旦ローカル `env` に取るが、`env := step.Env` はマップ参照共有なので、`env == nil` の場合のみ新規マップにし、既存マップに `UNIFIED_AGENT_OS` を足すと元の `step.Env` も変わる点に注意 — 元 step はこの後 status 報告等で `.Env` を使わないため問題ないが、明示的に安全側へ倒すなら次の Step 5 の orchestrate 側コピーで吸収する。）

- [ ] **Step 5: orchestrate で env を展開して渡す**

`internal/k8sagent/agent.go` の orchestrate 内、`stepExec(execCtx, step, expandedRun)` 呼び出し（437行付近）の直前に、env を展開した step のコピーを作って渡す:
```go
			stepForExec := step
			stepForExec.Env = expandStepEnv(step.Env, tplData)
			ec, capturedStdout, execErr := stepExec(execCtx, stepForExec, expandedRun)
```
（`step` 本体は以降の status 報告等でそのまま使い、`stepForExec` のみ env 展開済み。これにより Step 4 の `env["UNIFIED_AGENT_OS"]=...` はコピー側マップに作用し、元 claim には波及しない。）

- [ ] **Step 6: テストがパスすることを確認**

Run: `go test ./internal/k8sagent/ -run TestExpandStepEnv -v`
Expected: PASS

- [ ] **Step 7: パッケージ全体のリグレッション**

Run: `go build ./... && go test ./internal/k8sagent/ -short`
Expected: ビルド成功、既存テスト PASS。`runsInImageUnsupported` 参照が全て消えていること（旧ガードテスト削除済み）。

- [ ] **Step 8: schema/docs は変更なしの確認**

Run: `go generate ./internal/dsl/ && git status --short`
Expected: `runsIn` は Plan A で既にスキーマ登録済みのため**差分なし**（本 Plan は DSL struct を変えない）。差分が出た場合はコミットに含める。

- [ ] **Step 9: Commit**

```bash
git add internal/k8sagent/agent.go internal/k8sagent/agent_runsin_test.go
git commit -m "feat(k8sagent): dispatch runsIn.image steps to throwaway pods; remove Plan A guard"
```

---

## 最終確認（全タスク後）

- [ ] `go build ./...` 成功
- [ ] `go test ./internal/k8sagent/... -short` 全パス（実クラスタ integration は skip 前提）
- [ ] `go vet ./internal/k8sagent/...` クリーン
- [ ] `runsInImageUnsupported` がコードベースから消えていること（`grep -rn runsInImageUnsupported .` が空）
- [ ] 既存の①②（デフォルト/named container）挙動が回帰していないこと（orchestrate_test 群がグリーン）
- [ ] 手動 smoke（任意・実クラスタ）: `runsIn:{image: alpine:3.20}` の1ステップ Job を k8s-agent で流し、使い捨て pod が作られ実行後に削除されること／存在しない image でステップがハードエラーになること

## スコープ外（Plan B-2 以降）

- 使い捨て pod の resources（requests/limits）設定。
- ネットワーク設定 / NetworkPolicy 継承。
- workspace/指定 volume の選択的マウント。
- Apple `container` CLI フラグの実機検証。
