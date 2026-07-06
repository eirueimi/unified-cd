# 共有オーケストレータ抽出 実装プラン

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** host/k8s 両エージェントのステップオーケストレーションを agentlib の共有実装+`ExecBackend` インターフェースに一本化する(TODO #44 段階2)。

**Architecture:** 9タスクの漸進移行。①無挙動変更のヘルパー抽出 → ②③挙動統一(単独タスク) → ④⑤実行構造の同型化(RunPipeline) → ⑥⑦ExecBackend 抽出 → ⑧共有ループ完成 → ⑨後始末。**各タスク完了時にパリティスイート(`go test ./internal/agent/ -run TestParity` と `./internal/k8sagent/ -run TestParity`)+両パッケージのフルスイートが緑であることが絶対条件。**

**Tech Stack:** Go 1.26+。テストは testify+httptest フェイクcontroller(既存ハーネス流用)。Docker 不要(agent/k8sagent のみ)。

**Spec:** [2026-07-06-shared-orchestrator-design.md](../specs/2026-07-06-shared-orchestrator-design.md)
**構造分析:** 各所の file:line は 2026-07-06 の構造分析時点(main=aa6841b 相当)。ズレていたら周辺を読んで特定すること。

## Global Constraints

- 各タスクの完了ゲート: `go build ./... && go vet ./internal/agent/ ./internal/k8sagent/ && go test -count=1 ./internal/agent/ ./internal/k8sagent/` 全緑。
- ホストの `executeRun(ctx, claim, workDir)`・k8s の `executeRun(ctx, claim)` の**シグネチャは全タスクを通じて不変**(直呼びテスト計30箇所超を守る)。
- 挙動変更はタスク2(k8s報告リトライ)とタスク3(cache空key/path)のみ。他タスクで挙動が変わったらそれはバグ。
- 意図的差分の維持: host=Concurrent / k8s=Sequential、runsIn.container は host で明示エラー、k8s stdout は行単位 logLineWriter。
- コミットメッセージ末尾に `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`。

---

### Task 1: 共有 `ApplyStepOutputs` ヘルパー抽出(無挙動変更)

**Files:**
- Create: `internal/agent/stepoutputs.go`, `internal/agent/stepoutputs_test.go`
- Modify: `internal/agent/pipeline.go`(`safeStepCtx.setStep`/`setStepMatrixOutputs`, 行41-92 — ロック内の本体マージロジックを新ヘルパー呼び出しに置換)
- Modify: `internal/k8sagent/agent.go`(手書きの map 操作 2箇所: 行750-763 付近(call分岐)と 806-824 付近(run分岐)を新ヘルパー呼び出しに置換)

**Interfaces:**
- Produces: `func ApplyStepOutputs(steps map[string]dsl.StepData, stepName, matrixKey string, outputs map[string]string)` — 純関数(ロックしない)。matrixKey=="" なら `steps[stepName] = dsl.StepData{Outputs: dsl.StringOutputs(outputs)}` 相当。matrixKey!="" なら既存の**コピーオンライト集約**(pipeline.go の setStepMatrixOutputs の現行ロジックを逐語移植: 既存 StepData を書き換えず新 map を作り、`outputs[key]` を combination-key 別 map に積む)。呼び出し側がロックを持つ。

- [ ] **Step 1: 失敗するユニットテストを書く**(`internal/agent/stepoutputs_test.go`)

```go
package agent

import (
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
)

func TestApplyStepOutputs_Bare(t *testing.T) {
	steps := map[string]dsl.StepData{}
	ApplyStepOutputs(steps, "build", "", map[string]string{"bin": "app"})
	assert.Equal(t, "app", steps["build"].Outputs["bin"])
}

func TestApplyStepOutputs_MatrixAggregatesByCombinationKey(t *testing.T) {
	steps := map[string]dsl.StepData{}
	ApplyStepOutputs(steps, "build", "linux/amd64", map[string]string{"bin": "a"})
	ApplyStepOutputs(steps, "build", "linux/arm64", map[string]string{"bin": "b"})
	// 期待値は pipeline.go setStepMatrixOutputs の現行挙動と厳密一致させること:
	// Outputs["bin"] が combination-key→値 の map になる(dsl.StepData の実型を読んで
	// アサーションを現実装の形に合わせて書き直してよい。挙動を変えないことが正)。
	got := steps["build"].Outputs
	assert.Contains(t, got, "bin")
}

func TestApplyStepOutputs_MatrixDoesNotMutatePriorSnapshot(t *testing.T) {
	steps := map[string]dsl.StepData{}
	ApplyStepOutputs(steps, "build", "v1", map[string]string{"k": "1"})
	before := steps["build"]
	ApplyStepOutputs(steps, "build", "v2", map[string]string{"k": "2"})
	// コピーオンライト: 以前取得した StepData スナップショットは変化しない
	assert.NotSame(t, &before, &steps)
	_ = before
}
```

先に `internal/agent/pipeline.go:41-92` と `internal/dsl` の `StepData`/`StringOutputs` を読み、上のテストのアサーションを**現行ホスト挙動の厳密な形**に具体化する(挙動定義はホストが正)。

- [ ] **Step 2: RED 確認** — `go test ./internal/agent/ -run TestApplyStepOutputs -v` → `undefined: ApplyStepOutputs`
- [ ] **Step 3: 実装** — `stepoutputs.go` に純関数として、`safeStepCtx.setStep`/`setStepMatrixOutputs` の本体を逐語移植。次に pipeline.go の両メソッドを「ロック取得 → `ApplyStepOutputs(s.data.Steps, ...)` → 解放」に縮退。最後に k8sagent/agent.go の手書き map 操作 2箇所を `agentlib.ApplyStepOutputs(stepCtx.Steps, step.Name, step.MatrixKey, outputs)` に置換(k8s は逐次実行なのでロック不要 — 既存コメントを維持)。
- [ ] **Step 4: GREEN + ゲート** — 新テスト PASS、続けて Global Constraints のフルゲート実行。
- [ ] **Step 5: Commit** — `refactor(agent): extract shared ApplyStepOutputs; k8s uses it (no behavior change)`

---

### Task 2: k8s 報告系に retryUntilSuccess(挙動変更・単独)

**Files:**
- Modify: `internal/agent/retry.go`(`retryUntilSuccess` の定義箇所 — grep で特定)に公開ラッパー追加
- Modify: `internal/k8sagent/agent.go` — orchestrate 内の全報告系呼び出し
- Test: `internal/k8sagent/report_retry_test.go`(新規)

**Interfaces:**
- Produces: `func RetryUntilSuccess(ctx context.Context, fn func(context.Context) error)`(agentlib、既存 unexported 実装の公開エイリアス。セマンティクス: fn が nil を返すか ctx が done になるまで再試行)。

- [ ] **Step 1: 失敗するテストを書く** — 「最初の2回を 500 で拒否し3回目で受理するフェイクcontroller」で orchestrate を1ステップ実行し、**ステップの終端報告と FinishRun が最終的に記録される**ことをアサート(現状は単発送信なので届かず FAIL)。ハーネスは `internal/k8sagent/orchestrate_test.go` の既存パターン(K8sAgent 構築+httptest)を流用し、steps ハンドラに `failFirst int32` カウンタを持たせる。

```go
// report_retry_test.go の骨子(ハーネス詳細は orchestrate_test.go を踏襲して具体化):
// - POST /agents/{id}/steps: atomic カウンタで最初の2リクエストに 500 を返し、以降 204。受理分を記録。
// - POST /agents/{id}/runs/{run}/finish: 同様に最初の1回 500。
// - 1ステップ(run: echo ok)の claim を orchestrate に流す。
// - 期待: 受理された terminal report(Succeeded)が存在し、finish も受理されている。
// - リトライ間隔で膝を打たないよう、agentlib の retry backoff が var なら test で短縮
//   (retryUntilSuccess の実装を読み、短縮手段が無ければ実時間で数秒待つ設計にする)。
```

- [ ] **Step 2: RED 確認** — 500 で捨てられ terminal report が届かず FAIL することを確認。
- [ ] **Step 3: 実装** — agentlib に `RetryUntilSuccess`(既存 retryUntilSuccess を呼ぶだけの公開関数、doc comment 付き)を追加。k8sagent/agent.go の以下を `agentlib.RetryUntilSuccess(reportCtx, func(c context.Context) error { return a.client.XXX(c, ...) })` で包む: Skipped 報告(行528-530)、cache/artifact 分岐内の終端報告(行645-648 ほか同型)、run/call 分岐の終端報告(行839-850)、`SetStepOutputs`(行826 付近)、`SetRunOutputs`(行925 付近)、`FinishRun`(行987-989)。**Running 報告と log append は包まない**(ホストも包んでいない — ホストの現状と厳密対称にすること。ホスト側の包み方は internal/agent/agent.go:451-461, 693-715, 806-811 を読んで確認)。
- [ ] **Step 4: GREEN + フルゲート**
- [ ] **Step 5: Commit** — `fix(k8sagent): retry step/finish reports until success (parity with host)`

---

### Task 3: cache 空 key/path の意味論統一(挙動変更・単独)

**Files:**
- Modify: `internal/paritycases/scenarios.go`(シナリオ追加)
- Modify: `internal/agent/agent.go` `executeCacheStep`(行976-1092 付近)
- Modify: `internal/agent/parity_host_test.go` / `internal/k8sagent/parity_k8s_test.go`(新シナリオが cache 経路を通るよう、必要ならフェイク cache 基盤を配線)
- Test: `internal/agent/agent_cache_test.go`(既存の直呼びテストに追随修正+新ケース)

**確定挙動(スペック判断1):** テンプレート展開が**成功して**空文字になった `cache.key` / `cache.path` は、両エージェントとも **warn ログ+キャッシュ操作スキップ+ステップ Succeeded**。テンプレート**展開エラー**は従来どおり両者ハード失敗。k8s は既にこの挙動(`k8sagent/agent.go:613-622`)なので**変更はホスト側のみ**。

- [ ] **Step 1: paritycases にシナリオ追加(RED)**

```go
// scenarios.go に追加(既存 Case 構造に合わせて具体化):
// Name: "cache-empty-key-skips"
// Claim: 1ステップ目 cache: {key: "{{ .Params.novalue }}", path: "/tmp/x"}(params で novalue="" を supply)、
//        2ステップ目 run: echo after-cache
// Expect: 両ステップ Succeeded、run Succeeded、ログに after-cache。
// 注意: ホストのパリティドライバは CacheStore 未設定の可能性がある。executeCacheStep が
// nil ストアで早期 return する場合はこのシナリオが経路を通らないため、ドライバの Agent 構築に
// フェイク objectstore.ObjectStore(メモリ実装 or 既存テストのフェイク)を設定して cache 経路を
// 実際に通すこと(k8s 側は sidecarExec が noop 記録で既に通る)。
```

- [ ] **Step 2: RED 確認** — host 側パリティで「空 path はハード失敗」または「空 key で cache.Restore まで到達」のどちらかで期待と食い違い FAIL(k8s 側は PASS のはず — それ自体が現ドリフトの実証)。
- [ ] **Step 3: ホスト実装を変更** — `executeCacheStep` で、展開後の `cacheKey == ""` または `cachePath == ""` の場合に `slog.Warn(...)` してエラーなし(スキップ)で return。既存の「空 path でエラー」分岐(agent.go:1002-1004)を置換。展開エラー分岐は不変。既存 `agent_cache_test.go` の直呼びテストが空 path 失敗を期待していれば新挙動へ書き換え、空 key スキップの新ケースを追加。
- [ ] **Step 4: GREEN + フルゲート**(両パリティドライバで新シナリオ PASS)
- [ ] **Step 5: Commit** — `fix(agent): empty cache key/path skips the cache op instead of failing (parity, TODO #44)`

---

### Task 4: `RunPipeline` に ConcurrencyMode 導入(無挙動変更)

**Files:**
- Modify: `internal/agent/pipeline.go`(RunPipeline シグネチャ+Sequential 分岐)
- Modify: `internal/agent/agent.go`(RunPipeline 呼び出し 2箇所: main 行724-725 / finally 行760-775 に `Concurrent` を渡す)
- Modify: `internal/agent/pipeline_test.go`(8箇所の直呼びに `Concurrent` 追加+新テスト)

**Interfaces:**
- Produces:

```go
// pipeline.go
type ConcurrencyMode int

const (
	// Concurrent runs parallel-group / matrix-expanded members as goroutines
	// (the host agent's historical behavior).
	Concurrent ConcurrencyMode = iota
	// Sequential runs them one at a time in declaration order (the k8s
	// agent's documented behavior — its scope-pod map and hook stack are not
	// concurrency-safe).
	Sequential
)

func RunPipeline(ctx context.Context, stages []api.ClaimStage, maxCombinations int, mode ConcurrencyMode, run RunStepFunc) // 既存シグネチャに mode を挿入。
// 既存 RunPipeline のシグネチャ・run コールバック型は現物を読んで正確に把握し、mode 引数の
// 挿入位置は stages の直後で統一する。Sequential のときは runParallel を呼ばず、展開済み
// メンバーを for ループで runOne 相当の順次実行(ContinueOnError 抑制は runOne と同一)にする。
```

- [ ] **Step 1: 失敗するテストを書く**(pipeline_test.go に追加)

```go
// TestRunPipeline_SequentialRunsMembersOneAtATime:
// 3メンバーの parallel グループを Sequential で実行し、run コールバック内で
// atomic に inFlight++/max 記録/inFlight-- して maxInFlight==1 を assert。
// 実行順が宣言順であることも順序記録で assert。
// TestRunPipeline_ConcurrentRunsMembersTogether(挙動固定):
// 同じ3メンバーを Concurrent で実行し、全員が同時に in-flight になるまで
// チャネルで待ち合わせ(デッドロックタイムアウト付き)、maxInFlight==3 を assert。
```

- [ ] **Step 2: RED 確認** — mode 引数が無いのでコンパイルエラー。
- [ ] **Step 3: 実装** — 上記のとおり。既存 8 呼び出しと agent.go 2箇所に `Concurrent` を機械的に追加。
- [ ] **Step 4: GREEN + フルゲート**
- [ ] **Step 5: Commit** — `refactor(agent): RunPipeline gains ConcurrencyMode (host stays Concurrent)`

---

### Task 5: k8s のステージ/finally ループを RunPipeline(Sequential) へ移行

**Files:**
- Modify: `internal/k8sagent/agent.go` — main ループ(行864-878)と finally ループ(行950-975)を `agentlib.RunPipeline(..., agentlib.Sequential, mainRun/finallyRun)` に置換。`agentlib.ExpandMatrixStep` の直接呼び出しはこの2ループから消える(RunPipeline 内部に集約)。
- Test: 既存 `orchestrate_*_test.go` / `callstep_test.go` / `parity_k8s_test.go` は**そのまま通ること**(orchestrate のシグネチャは本タスクでは不変)。

**Interfaces:**
- Consumes: Task 4 の `RunPipeline(ctx, stages, maxCombinations, mode, run)`。
- 注意: host の run コールバック型(`RunStepFunc`)と k8s の `makeRunStep` が返すクロージャの型を一致させる。差異(引数に variant 済み step を取るか等)は現物を読んで k8s 側クロージャを適合させる。matrix 上限(`c.MatrixMaxCombinations`)の伝播を忘れない(現行 k8s ループが渡している値をそのまま)。

- [ ] **Step 1: 挙動固定テストの確認** — 新テストは書かない。既存の `orchestrate_test.go`(matrix/parallel 含む)、`orchestrate_post_test.go`(LIFO)、`parity_k8s_test.go` が挙動固定テストとして機能する。移行前に一度実行して緑を確認(ベースライン)。
- [ ] **Step 2: 置換実装** — 2ループを RunPipeline 呼び出しへ。`hookStack`/`scopePods`/`cacheSaves` への逐次アクセス前提コメント(行247-253 等)は「Sequential モードで RunPipeline を使うため」に文言更新。
- [ ] **Step 3: フルゲート** — とくに `orchestrate_post_test.go` の LIFO 順、matrix の per-variant 報告、`parity_k8s_test.go` 10ケース。
- [ ] **Step 4: Commit** — `refactor(k8sagent): drive stages/finally through shared RunPipeline (Sequential)`

---

### Task 6: `ExecBackend` 定義+host 実装

**Files:**
- Create: `internal/agent/backend.go`(インターフェース+ScopeHandle)
- Create: `internal/agent/backend_host.go`(hostBackend)
- Modify: `internal/agent/agent.go` — `executeRun` 内の exec ディスパッチ(行606-638)、scope 取得(行383-402)、cache/artifact 分岐(行490-536)、post フック実行(行733-754)を backend 経由に置換。**executeRun のシグネチャと外部挙動は不変。**
- Test: `internal/agent/backend_host_test.go`(新規・薄く)

**Interfaces:**
- Produces(spec 確定案を Go として具体化。step 引数は `api.ClaimStep`):

```go
// backend.go
package agent

// ExecBackend is the narrow seam between the shared step-orchestration loop
// and a concrete execution environment (host process / k8s pod).
type ExecBackend interface {
	RunDefault(ctx context.Context, step api.ClaimStep, script string, env []string, stdout, stderr io.Writer) (int, error)
	RunImage(ctx context.Context, step api.ClaimStep, script string, env []string, stdout, stderr io.Writer) (int, error)
	RunNamedContainer(ctx context.Context, step api.ClaimStep, container, script string, env []string, stdout, stderr io.Writer) (int, error)

	EnsureScope(ctx context.Context, step api.ClaimStep, env []string) (ScopeHandle, error)
	RunInScope(ctx context.Context, h ScopeHandle, script string, env []string, stdout, stderr io.Writer) (int, error)
	CloseScopes(ctx context.Context)

	CacheRestore(ctx context.Context, scope ScopeHandle, key string, restoreKeys []string, path string) (bool, error)
	CacheSave(ctx context.Context, scope ScopeHandle, key, path string, ttlDays int) error
	UploadArtifact(ctx context.Context, scope ScopeHandle, runID, name, path string) error
	DownloadArtifact(ctx context.Context, scope ScopeHandle, runID, name, destDir string) error

	RunPostHook(ctx context.Context, scope ScopeHandle, container, script string, env []string) error

	// SetMasker installs the secret masker for all subsequently-created log
	// writers. Called once by the shared loop right after it fetches secrets
	// (the masker is born inside the loop, after backend construction).
	SetMasker(m *secrets.Masker)

	// StepLogWriters returns the SHIPPING writers for one step's output and a
	// finish func called at step end. Flush/liveness semantics are backend-
	// specific and intentionally asymmetric: host returns LogPusher for both
	// streams (with StartAutoFlush bound to ctx); k8s returns its per-line
	// stdout logLineWriter and a LogPusher (auto-flushed) for stderr. The
	// {{ .Stdout }} capture buffer is the ORCHESTRATOR's concern — it tees
	// stdout via io.MultiWriter, so backends return shipping writers only.
	StepLogWriters(ctx context.Context, stepIndex int) (stdout, stderr io.Writer, finish func(ctx context.Context))

	ConcurrencyMode() ConcurrencyMode
}

// ScopeHandle is an opaque per-(ScopeID,MatrixKey) scope identity.
// Zero value = no scope / default location.
type ScopeHandle struct{ opaque any }

func (h ScopeHandle) IsZero() bool { return h.opaque == nil }
```

(spec の `AcquireRun` は Task 8 で共有ループ側の関数引数として扱う — インターフェースには入れない。host の workDir / k8s の podName はバックエンド構造体のフィールドとして保持する方が Go 的に素直なため。)

- `backend_host.go`: `type hostBackend struct { a *Agent; workDir string; scopes *scopeManager; masker *secrets.Masker; ... }` — 各メソッドは既存関数への委譲: RunDefault→`RunStep`、RunImage→`a.containerRuntime()`+`RunStepContainer`+`hostContainerLimits`、RunNamedContainer→明示エラー(現行文言を流用)、EnsureScope/RunInScope/CloseScopes→`scopeManager`、Cache/Artifact→`executeCacheStep` 系の**転送部分**(cache.Restore/Save、upload/download 実体)、RunPostHook→scope 分岐+`RunStepCapture`、SetMasker→フィールド保存、StepLogWriters→現行の stdout/stderr LogPusher 構築+SetMasker+StartAutoFlush(agent.go:592-603 の内容。finish=stopAutoFlush+両 Flush)、ConcurrencyMode→`Concurrent`。executeRun 内のログ配管(行592-603, 639-643)は StepLogWriters 呼び出し+orchestrator 側 `io.MultiWriter(&stdoutBuf, shipStdout)` の tee に置換。
- executeRun 側: exec ディスパッチの switch は残し、各腕が backend メソッドを呼ぶ形に(分岐判定はオーケストレータの責務)。cache/artifact ステップの**テンプレート展開・スキップ判定・報告**はオーケストレータに残し、転送だけ backend へ。
- 既存 `agent_cache_test.go` 等の `executeCacheStep` 直呼び(5箇所)は、リファクタ後の該当ユニット(backend メソッド or 残存ヘルパー)に追随変更。**テストが検証している挙動自体は維持**。

- [ ] **Step 1: ベースライン緑確認 → 実装 → フルゲート**(このタスクは移動が主体。TDD は backend_host_test.go の「RunNamedContainer がエラーを返す」「ConcurrencyMode()==Concurrent」等の薄いユニット+既存スイートを挙動固定に使う)
- [ ] **Step 2: Commit** — `refactor(agent): define ExecBackend; host executes through it`

---

### Task 7: k8s の `ExecBackend` 実装(orchestrate の8引数シグネチャ廃止)

**Files:**
- Create: `internal/k8sagent/backend.go`(k8sBackend)
- Modify: `internal/k8sagent/agent.go` — `executeRun` がクロージャ束の代わりに `k8sBackend` を構築、`orchestrate` シグネチャを `orchestrate(ctx context.Context, c api.ClaimResponse, b agentlib.ExecBackend, secretValues map[string]string)` に変更(mountPath は backend フィールドへ)。
- Modify(全面): `orchestrate_test.go`(4)/`orchestrate_cancel_test.go`/`orchestrate_post_test.go`(3)/`orchestrate_timeout_test.go`/`callstep_test.go`/`parity_k8s_test.go` — フェイク `ExecBackend` 構築への書き換え。共通フェイクを `internal/k8sagent/fakebackend_test.go` に1つ定義して全テストで共有(現行の stepExec/sidecarExec/postExec クロージャ束フェイクを1構造体に集約。既存テストの記録・アサーション対象は維持)。

**Interfaces:**
- Consumes: Task 6 の `agentlib.ExecBackend` / `ScopeHandle`。
- k8sBackend 実装マップ: RunDefault→`exec.ExecStep(pod, execContainer(step), ...)`、RunImage→`runImageStep`、RunNamedContainer→`exec.ExecStep`(named container)、EnsureScope→`ensureScopePod`(ScopeHandle.opaque=pod名)、RunInScope→scope pod への ExecStep、CloseScopes→scope pod 一括削除(現行 defer 内容)、Cache/Artifact→sidecar argv 実行、RunPostHook→現行 postExec、SetMasker→フィールド保存、StepLogWriters→stdout=現行 `logLineWriter`(masker 付き)/stderr=`LogPusher`+SetMasker+StartAutoFlush(現行 stepExec 冒頭の構築を移設。finish=autoflush 停止+stderr Flush)、ConcurrencyMode→`Sequential`。

- [ ] **Step 1: フェイクバックエンド定義 → テスト書き換え → 実装 → フルゲート**(挙動固定は既存テスト群+パリティ。書き換え時に**アサーションを弱めない**こと — レビューで最重要チェック項目)
- [ ] **Step 2: Commit** — `refactor(k8sagent): implement ExecBackend; retire 8-closure orchestrate signature`

---

### Task 8: 共有オーケストレータ本体の確立

**Files:**
- Create: `internal/agent/orchestrator.go` — `func RunClaim(ctx context.Context, client *Client, agentID string, c api.ClaimResponse, b ExecBackend)`(secrets 取得→masker 生成→`b.SetMasker(m)`→キャンセルポーラ→stepCtx→makeStepRunner(ログは `b.StepLogWriters` + orchestrator 側 stdout tee)→RunPipeline(main, `b.ConcurrencyMode()`)→post フック LIFO→cache 保存ドレイン→finally→outputs 昇格→FinishRun の全順序を host `executeRun` 本体(行283-811)から移植)
- Modify: `internal/agent/agent.go` — `executeRun` を「hostBackend 構築+RunClaim 呼び出し」の薄いラッパーへ(シグネチャ不変。podTemplate 拒否(行276-282)はラッパーに残す)
- Modify: `internal/k8sagent/agent.go` — `executeRun` を「Pod 取得(AcquireRun 相当の既存コード)+k8sBackend 構築+`agentlib.RunClaim`」へ。`orchestrate` 関数を削除。
- Modify: k8s テスト群 — `a.orchestrate(...)` 呼び出しを `agentlib.RunClaim(ctx, client, agentID, c, fakeBackend, ...)` へ機械置換(Task 7 でフェイク化済みなので差分は小さい)。

**挙動注意(構造分析より):** 共有ループの報告リトライは host 方式(Task 2 で k8s も同方式になっている)。Cancelled/Failed の判別(`cancelledByMaster`)は host 実装を正とする(k8s は従来この判別を持たない — 統合で k8s にも host 挙動が入るのは**意図された改善**であり、パリティスイートの cancel シナリオがあれば期待値を更新、なければ現状ケース追加は任意)。job/step タイムアウト・reportCtx=WithoutCancel の前後関係は k8s 実装(Task B 由来)と host 実装で一致していることを移植時に確認。

- [ ] **Step 1: 移植 → 両 executeRun 縮退 → フルゲート**(パリティ10+3ケースが「共有ループ1本を両バックエンドで回した」状態で緑になることが本タスクの合格条件)
- [ ] **Step 2: Commit** — `refactor(agent): single shared orchestrator RunClaim drives both backends`

---

### Task 9: 後始末

**Files:**
- Modify: `internal/k8sagent/agent.go` — 死んだ型(旧 postHookEntry/cacheSaveSpec 等が共有型に置換済みなら削除)、`"this mirrors the host agent"` 系コメント(行97-99, 122-124, 249-253 ほか grep `mirror` で列挙)を削除/更新
- Modify: `docs/kubernetes-integration.md` — パリティ節に「オーケストレーションは共有実装(`internal/agent` RunClaim)、バックエンドのみ別」の1文を追加
- Modify: `TODO.md` — #44 段階2を対応済み(コミット参照付き)に更新
- Modify: `docs/superpowers/specs/2026-07-06-shared-orchestrator-design.md` — ステータスを「実装済み」に

- [ ] **Step 1: 掃除 → 最終フルゲート**(`go build ./... && go vet ./... && go test -count=1 ./internal/agent/ ./internal/k8sagent/ ./internal/paritycases/` に加え、touched してないが `./internal/controller/ ./internal/store/` も1回)
- [ ] **Step 2: Commit** — `chore: finish shared-orchestrator extraction (TODO #44 stage 2 complete)`
