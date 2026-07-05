# call ステップ ↔ 子 run 双方向リンク 実装計画

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 呼び出し元 run の詳細で call ステップから子 run へリンクでき、子 run の詳細で「◯◯ から呼ばれた」と辿れるようにする（双方向）。

**Architecture:** エッジは `step_reports.child_run_id`（+ `call_job_name`）1本で保持。前方向は呼び元の step_reports 行、逆方向は `child_run_id` の逆引き（index 付き、`runs` JOIN で親ジョブ名）。`executeCallStep` は既知の `childRun.ID` を終端ステップ報告に載せるだけ（`CreateRun` 不変）。`runs.parent_run_id` は追加しない。

**Tech Stack:** Go, pgx/Postgres, golang-migrate, chi, Svelte + vitest。

## Global Constraints

- Go モジュール `github.com/eirueimi/unified-cd`。テストは testify（Go）/ vitest（web）。
- Postgres store。マイグレーションは連番（次は **007**）。
- エッジは `step_reports.child_run_id` 1本（DRY、`runs.parent_run_id` 不要）。
- 後方互換: 既存 run は `child_run_id`/`call_job_name` が NULL → リンク非表示。
- 実 Postgres 統合テストは `store.NewTestPostgres(t)`（`testing.Short()` で skip）。

---

### Task 1: マイグレーション 007 + store（保存・取得・逆引き）

**Files:**
- Create: `internal/store/migrations/007_step_call_link.up.sql`, `007_step_call_link.down.sql`
- Modify: `internal/api/types.go`（`StepReport` に2フィールド、`CalledBy` 型）
- Modify: `internal/store/postgres.go`（`UpsertStepReport` 拡張、`GetRunSteps` 拡張、`GetRunParent` 追加）
- Modify: `internal/store/store.go`（Store インターフェイスに `GetRunParent` と `UpsertStepReport` の新シグネチャ）
- Test: `internal/store/postgres_callrun_test.go`（新規）

**Interfaces:**
- Produces:
  - `StepReport.ChildRunID string` / `StepReport.CallJobName string`（json `childRunId`/`callJobName`, omitempty）
  - `type CalledBy struct { ParentRunID string; ParentJobName string; StepName string }`
  - `UpsertStepReport(ctx, runID string, stepIndex, stageIndex int, stepName, variant, status string, exitCode *int, startedAt, endedAt *time.Time, childRunID, callJobName string) error`
  - `GetRunParent(ctx, childRunID string) (*api.CalledBy, error)`（call 由来でなければ nil, nil）

- [ ] **Step 1: マイグレーションを書く**

`internal/store/migrations/007_step_call_link.up.sql`:
```sql
ALTER TABLE step_reports
  ADD COLUMN child_run_id uuid,
  ADD COLUMN call_job_name text;
CREATE INDEX step_reports_child_run_id_idx ON step_reports (child_run_id);
```
`internal/store/migrations/007_step_call_link.down.sql`:
```sql
DROP INDEX IF EXISTS step_reports_child_run_id_idx;
ALTER TABLE step_reports
  DROP COLUMN IF EXISTS call_job_name,
  DROP COLUMN IF EXISTS child_run_id;
```

- [ ] **Step 2: api 型を追加**

`internal/api/types.go` の `StepReport` に（`Variant` の後）:
```go
	ChildRunID  string `json:"childRunId,omitempty"`
	CallJobName string `json:"callJobName,omitempty"`
```
`Run` 型の近くに `CalledBy` 型を追加:
```go
// CalledBy identifies the call step (and its run) that launched this run.
type CalledBy struct {
	ParentRunID   string `json:"parentRunId"`
	ParentJobName string `json:"parentJobName"`
	StepName      string `json:"stepName"`
}
```

- [ ] **Step 3: 失敗するテストを書く**

`internal/store/postgres_callrun_test.go`:
```go
package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStepReport_ChildRunLink(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	p := NewTestPostgres(t)
	ctx := t.Context()

	// a parent run whose step calls a child, and the child run itself
	parent := mustCreateRun(t, p, "parent-job")
	child := mustCreateRun(t, p, "child-job")

	// report the call step with the child link
	require.NoError(t, p.UpsertStepReport(ctx, parent, 0, 0, "call-child", "", "Succeeded", nil, nil, nil, child, "child-job"))

	// forward: parent's steps carry the child link
	steps, err := p.GetRunSteps(ctx, parent)
	require.NoError(t, err)
	require.Len(t, steps, 1)
	assert.Equal(t, child, steps[0].ChildRunID)
	assert.Equal(t, "child-job", steps[0].CallJobName)

	// reverse: the child resolves its caller
	cb, err := p.GetRunParent(ctx, child)
	require.NoError(t, err)
	require.NotNil(t, cb)
	assert.Equal(t, parent, cb.ParentRunID)
	assert.Equal(t, "parent-job", cb.ParentJobName)
	assert.Equal(t, "call-child", cb.StepName)

	// a run not created by a call has no parent
	cbNone, err := p.GetRunParent(ctx, parent)
	require.NoError(t, err)
	assert.Nil(t, cbNone)
}
```
（`mustCreateRun` ヘルパーが既存テストに無ければ、既存の run 作成ヘルパー（`internal/store/*_test.go` の `testutil` / `insertRun` 等）に倣って作る。run の作成方法は既存 store テストを確認して合わせること。）

- [ ] **Step 4: RED 確認**

Run: `go test ./internal/store/ -run TestStepReport_ChildRunLink -count=1`
Expected: FAIL（`UpsertStepReport` のシグネチャ不一致 / `GetRunParent` 未定義）

- [ ] **Step 5: store を実装**

`internal/store/postgres.go` の `UpsertStepReport` を拡張（引数追加、INSERT に2カラム、ON CONFLICT で COALESCE 保持）:
```go
func (p *Postgres) UpsertStepReport(ctx context.Context, runID string, stepIndex int, stageIndex int, stepName, variant, status string, exitCode *int, startedAt, endedAt *time.Time, childRunID, callJobName string) error {
	const q = `
		INSERT INTO step_reports(run_id, step_index, variant, stage_index, step_name, status, exit_code, started_at, ended_at, child_run_id, call_job_name)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULLIF($10,'')::uuid, NULLIF($11,''))
		ON CONFLICT (run_id, step_index, variant) DO UPDATE
		  SET stage_index   = EXCLUDED.stage_index,
		      step_name     = EXCLUDED.step_name,
		      status        = EXCLUDED.status,
		      exit_code     = COALESCE(EXCLUDED.exit_code, step_reports.exit_code),
		      started_at    = COALESCE(EXCLUDED.started_at, step_reports.started_at),
		      ended_at      = COALESCE(EXCLUDED.ended_at, step_reports.ended_at),
		      child_run_id  = COALESCE(EXCLUDED.child_run_id, step_reports.child_run_id),
		      call_job_name = COALESCE(EXCLUDED.call_job_name, step_reports.call_job_name);
	`
	_, err := p.pool.Exec(ctx, q, runID, stepIndex, variant, stageIndex, stepName, status, exitCode, startedAt, endedAt, childRunID, callJobName)
	return err
}
```
`GetRunSteps` の SELECT/Scan を拡張（NULL は COALESCE で空文字に）:
```go
	const q = `
		SELECT step_index, stage_index, step_name, status, exit_code, started_at, ended_at, variant,
		       COALESCE(child_run_id::text, ''), COALESCE(call_job_name, '')
		FROM step_reports
		WHERE run_id = $1
		ORDER BY step_index, variant;
	`
```
Scan に2カラム追加:
```go
		if err := rows.Scan(&s.Index, &s.StageIndex, &s.Name, &s.Status, &s.ExitCode, &s.StartedAt, &s.EndedAt, &s.Variant, &s.ChildRunID, &s.CallJobName); err != nil {
```
`GetRunParent` を追加（`GetRunSteps` の近く）:
```go
// GetRunParent returns the call step (and parent run) that launched childRunID,
// or nil if the run was not created by a call step.
func (p *Postgres) GetRunParent(ctx context.Context, childRunID string) (*api.CalledBy, error) {
	const q = `
		SELECT sr.run_id::text, r.job_name, sr.step_name
		FROM step_reports sr
		JOIN runs r ON r.id = sr.run_id
		WHERE sr.child_run_id = $1::uuid
		LIMIT 1;
	`
	var cb api.CalledBy
	err := p.pool.QueryRow(ctx, q, childRunID).Scan(&cb.ParentRunID, &cb.ParentJobName, &cb.StepName)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &cb, nil
}
```
（`pgx` は postgres.go で import 済み。無ければ `github.com/jackc/pgx/v5` を確認。）

`internal/store/store.go` の Store インターフェイス定義で、`UpsertStepReport` のシグネチャを新しいものに更新し、`GetRunParent(ctx context.Context, childRunID string) (*api.CalledBy, error)` を追加。

- [ ] **Step 6: GREEN 確認**

Run: `go test ./internal/store/ -run TestStepReport_ChildRunLink -count=1`
Expected: PASS

- [ ] **Step 7: ビルド（呼び出し元の壊れ確認）**

Run: `go build ./... 2>&1 | head`
Expected: `UpsertStepReport` のシグネチャ変更で、**全ての呼び出し元とモック実装**がコンパイルエラーになる。想定どおり。build を通すために:
- `internal/controller/api_agent.go` の `UpsertStepReport(...)` 呼び出しに **暫定で `"", ""` を末尾に追加**（Task 2 で実値に差し替える）。
- `Store` インターフェイスのモック/インメモリ実装（`grep -rln "UpsertStepReport" internal/ | grep -i "mock\|fake\|memory"` で探す）があれば、新シグネチャ（`childRunID, callJobName string` 追加、`GetRunParent` 追加）に合わせて更新する。
- その他の呼び出し元（`grep -rn "UpsertStepReport(" internal/ --include=*.go`）が壊れていれば末尾 `"", ""` で最小修正。
build が通ることを確認してから commit。

- [ ] **Step 8: Commit**

```bash
git add internal/store/migrations/007_step_call_link.up.sql internal/store/migrations/007_step_call_link.down.sql internal/api/types.go internal/store/postgres.go internal/store/store.go internal/store/postgres_callrun_test.go internal/controller/api_agent.go
git commit -m "feat(store): persist call step child_run_id and reverse-lookup parent"
```

---

### Task 2: api リクエスト型 + controller 配線

**Files:**
- Modify: `internal/api/types.go`（`StepReportRequest` に2フィールド、`Run` に `CalledBy`）
- Modify: `internal/controller/api_agent.go`（`handleAgentStepReport` が childRunId/callJobName を store へ）
- Modify: `internal/controller/api_runs.go`（`handleGetRun` が `CalledBy` を解決）
- Test: `internal/controller/api_callrun_test.go`（新規）

**Interfaces:**
- Consumes: `store.GetRunParent`、`StepReport.ChildRunID/CallJobName`、`CalledBy`（Task 1）
- Produces: `StepReportRequest.ChildRunID/CallJobName`、`Run.CalledBy *CalledBy`

- [ ] **Step 1: 失敗するテストを書く**

`internal/controller/api_callrun_test.go`:
```go
package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetRun_CalledBy(t *testing.T) {
	if testing.Short() {
		t.Skip("requires postgres")
	}
	srv, pg := newTestServer(t) // use the existing controller test harness
	ctx := t.Context()

	parent := mustCreateRun(t, pg, "parent-job")
	child := mustCreateRun(t, pg, "child-job")
	require.NoError(t, pg.UpsertStepReport(ctx, parent, 0, 0, "call-child", "", "Succeeded", nil, nil, nil, child, "child-job"))

	// GET /runs/{child} → response.calledBy points at the parent
	run := getRunViaAPI(t, srv, child)
	require.NotNil(t, run.CalledBy)
	assert.Equal(t, parent, run.CalledBy.ParentRunID)
	assert.Equal(t, "parent-job", run.CalledBy.ParentJobName)

	// GET /runs/{parent} → no calledBy
	prun := getRunViaAPI(t, srv, parent)
	assert.Nil(t, prun.CalledBy)
}
```
（`newTestServer`/`getRunViaAPI`/`mustCreateRun` は既存 controller テスト（`internal/controller/api_runs_test.go` 等）のハーネスに合わせて用意すること。既存の GET /runs/{id} テスト（`TestAPI_GetRun`）の呼び出し方に倣う。）

- [ ] **Step 2: RED 確認**

Run: `go test ./internal/controller/ -run TestGetRun_CalledBy -count=1`
Expected: FAIL（`Run.CalledBy` 未定義）

- [ ] **Step 3: api 型を追加**

`internal/api/types.go` の `StepReportRequest` に（`Variant` の後）:
```go
	ChildRunID  string `json:"childRunId,omitempty"`
	CallJobName string `json:"callJobName,omitempty"`
```
`Run` 構造体（api/types.go:50）に:
```go
	CalledBy *CalledBy `json:"calledBy,omitempty"`
```

- [ ] **Step 4: controller を配線**

`internal/controller/api_agent.go` の `handleAgentStepReport` の `UpsertStepReport(...)` 呼び出しを、Task 1 で暫定追加した `"", ""` から実値へ:
```go
	if err := s.store.UpsertStepReport(r.Context(), req.RunID, req.StepIndex, req.StageIndex, req.StepName, req.Variant, req.Status, exit, startedAt, endedAt, req.ChildRunID, req.CallJobName); err != nil {
```
`internal/controller/api_runs.go` の `handleGetRun` を、run 取得後に CalledBy を解決するよう変更:
```go
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	run, err := s.store.GetRun(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if cb, cErr := s.store.GetRunParent(r.Context(), id); cErr == nil && cb != nil {
		run.CalledBy = cb
	}
	writeJSON(w, http.StatusOK, run)
}
```

- [ ] **Step 5: GREEN 確認**

Run: `go test ./internal/controller/ -run TestGetRun_CalledBy -count=1`
Expected: PASS

- [ ] **Step 6: 回帰**

Run: `go build ./... && go test ./internal/controller/ -run "StepReport|GetRun|CalledBy" -count=1 -timeout 20m`
Expected: ビルド成功、PASS（既存 `TestAgentAPI_ReportStep` は StepReportRequest の新フィールド追加でも壊れない。壊れたら最小修正）。

- [ ] **Step 7: Commit**

```bash
git add internal/api/types.go internal/controller/api_agent.go internal/controller/api_runs.go internal/controller/api_callrun_test.go
git commit -m "feat(controller): carry child_run_id through step report; expose calledBy on run"
```

---

### Task 3: agent — executeCallStep が childRunID を報告に載せる

**Files:**
- Modify: `internal/agent/agent.go`（`executeCallStep` が childRunID を返す、呼び出し元が終端報告に載せる）
- Test: `internal/agent/agent_callrun_test.go`（新規、フェイククライアントで報告を検証）

**Interfaces:**
- Consumes: `StepReportRequest.ChildRunID/CallJobName`（Task 2）
- Produces: `executeCallStep(...) (map[string]string, string, error)`（3値目 = childRunID）

- [ ] **Step 1: 失敗するテストを書く**

`internal/agent/agent_callrun_test.go`（既存の agent フェイククライアント/`executeRun` ハーネス（`agent_if_test.go`/`agent_finally_test.go`）に倣い、`call:` ステップを1つ持つ run を実行し、報告された StepReportRequest に ChildRunID/CallJobName が入ることを検証。ハーネスの都合上、最小構成でよい）:
```go
package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecuteRun_CallStep_ReportsChildLink(t *testing.T) {
	// Drive a run whose single step is `call: { job: child-job }` through the
	// fake-client harness (mirror agent_finally_test.go). The fake CreateRun
	// returns a known child id; the fake child run reports Succeeded so the
	// call completes. Assert the terminal StepReport for the call step carries
	// ChildRunID == <that id> and CallJobName == "child-job".
	rec := runCallStepThroughFakeClient(t, "child-job", "fixed-child-run-id")
	require.NotNil(t, rec)
	assert.Equal(t, "fixed-child-run-id", rec.ChildRunID)
	assert.Equal(t, "child-job", rec.CallJobName)
}
```
（`runCallStepThroughFakeClient` は既存のフェイククライアント（`agent_finally_test.go` 等の mock HTTP server もしくは fake `Client`）を使い、`CreateRun` が固定 ID を返し、`GetRun` が `Succeeded` を返すよう仕込む。報告された `StepReportRequest` のうち **call ステップの終端報告**を捕捉して返すこと。既存ハーネスの構造を読んで合わせる。もし既存ハーネスが `call` を通せない/報告を捕捉できない形なら NEEDS_CONTEXT で相談。）

- [ ] **Step 2: RED 確認**

Run: `go test ./internal/agent/ -run TestExecuteRun_CallStep_ReportsChildLink -count=1`
Expected: FAIL（childRunID が報告に載らない）

- [ ] **Step 3: executeCallStep を childRunID を返すよう変更**

`internal/agent/agent.go` の `executeCallStep`（656行）のシグネチャと return を変更:
```go
func (a *Agent) executeCallStep(ctx context.Context, step api.ClaimStep, tplData dsl.TemplateData) (outputs map[string]string, childRunID string, err error) {
```
`childRun, err := a.Client.CreateRun(...)` の失敗時 return を `return nil, "", fmt.Errorf(...)` に。成功後、以降の全 return に childRunID を載せる:
- 成功: `return outputs, childRun.ID, nil`
- 失敗/キャンセル/タイムアウト: `return nil, childRun.ID, fmt.Errorf(...)`（childRun.ID は既知なので載せる — 失敗でも子 run へリンクできる）

- [ ] **Step 4: 呼び出し元で終端報告に載せる**

`internal/agent/agent.go` の call ディスパッチ（439行付近）:
```go
			if step.Call != nil {
				childOutputs, childRunID, callErr := a.executeCallStep(stepCtx, step, tplData)
```
このステップの**終端 `ReportStep`（Succeeded/Failed で step を確定する報告）**の `api.StepReportRequest{...}` に、call ステップのとき:
```go
					ChildRunID:  childRunID,
					CallJobName: step.Call.Job,
```
を追加する。終端報告サイトが call 分岐の外（全ステップ共通）なら、`var callChildRunID, callJobName string` を dispatch 前に宣言し、call 分岐で `callChildRunID = childRunID; callJobName = step.Call.Job` を代入、共通の終端 `StepReportRequest` に `ChildRunID: callChildRunID, CallJobName: callJobName`（非 call は空 → omitempty）を無条件で載せる。**終端報告サイトの正確な位置は agent.go を読んで特定すること**（`status` を計算して `ReportStep` する箇所）。

- [ ] **Step 5: GREEN 確認**

Run: `go test ./internal/agent/ -run TestExecuteRun_CallStep_ReportsChildLink -count=1`
Expected: PASS

- [ ] **Step 6: agent 回帰 + ビルド**

Run: `go build ./... && go test ./internal/agent/... -short`
Expected: ビルド成功、PASS。

- [ ] **Step 7: Commit**

```bash
git add internal/agent/agent.go internal/agent/agent_callrun_test.go
git commit -m "feat(agent): report child_run_id and call job name on call steps"
```

---

### Task 4: WebUI — 前方向リンク + 逆方向パンくず

**Files:**
- Modify: `web/src/routes/RunDetail.svelte`（call ステップにリンク、`calledBy` パンくず）
- Test: `web/src/routes/RunDetail.test.js`（既存に追記、無ければ新規）

**Interfaces:**
- Consumes: steps の `s.childRunId`/`s.callJobName`、run の `run.calledBy.{parentRunId,parentJobName,stepName}`（Task 1/2）

- [ ] **Step 1: 失敗するテストを書く**

`web/src/routes/RunDetail.test.js` に（既存の RunDetail テストのモック/レンダリング方式に倣う。API モックで run に `calledBy`、steps に `childRunId`/`callJobName` を含める）:
```js
it("renders a link to the child run on a call step", async () => {
  // mock GET /runs/:id → { ..., status, ... } and GET /runs/:id/steps →
  // [{ index:0, name:"call-child", status:"Succeeded", childRunId:"c1", callJobName:"child-job" }]
  // render RunDetail; expect an anchor with href "#/runs/c1" and text containing "child-job"
});

it("renders a 'Called by' breadcrumb when run.calledBy is present", async () => {
  // mock GET /runs/:id → { ..., calledBy:{ parentRunId:"p1", parentJobName:"parent-job", stepName:"call-child" } }
  // render; expect an anchor with href "#/runs/p1" and text containing "parent-job"
});
```
（実際のアサーションは既存 `RunDetail.test.js` の testing-library 記法に合わせて具体化する。既存テストのモック方式（`apiFetch` のモック等）を読んで倣うこと。）

- [ ] **Step 2: RED 確認**

Run: `cd web && npm test -- RunDetail`
Expected: FAIL（リンク/パンくず未実装）

- [ ] **Step 3: 前方向リンクを描画**

`web/src/routes/RunDetail.svelte` のステップ描画（288行付近 `<span class="step-name">{s.name}</span>`）の直後に:
```svelte
                {#if s.childRunId}
                  <a class="call-link" href="#/runs/{s.childRunId}" title="Called job run">{s.callJobName || 'child run'} ↗</a>
                {/if}
```

- [ ] **Step 4: 逆方向パンくずを描画**

`web/src/routes/RunDetail.svelte` の run ヘッダ付近（タブ行 220行付近の前、run が読めている `{#if run}` ブロック内）に:
```svelte
                {#if run.calledBy}
                  <div class="called-by meta">
                    Called by <a href="#/runs/{run.calledBy.parentRunId}" title="Caller step: {run.calledBy.stepName}">{run.calledBy.parentJobName} ↗</a>
                  </div>
                {/if}
```
（挿入位置の正確な行は RunDetail.svelte の `{#if run}` 構造を読んで、params 表示の近く等に合わせる。）

- [ ] **Step 5: GREEN 確認**

Run: `cd web && npm test -- RunDetail`
Expected: PASS

- [ ] **Step 6: web 一式テスト**

Run: `cd web && npm test`
Expected: PASS（既存 RunDetail テストが壊れていないこと）

- [ ] **Step 7: Commit**

```bash
git add web/src/routes/RunDetail.svelte web/src/routes/RunDetail.test.js
git commit -m "feat(web): link call steps to child run and show 'called by' breadcrumb"
```

---

## 最終確認（全タスク後）

- [ ] `go build ./...` 成功
- [ ] `go test ./internal/store/... ./internal/controller/... ./internal/agent/... -count=1 -timeout 30m` パス（実 Postgres 込み。既存の無関係 e2e 失敗 `TestPhase8_FullOIDCFlow` は対象外）
- [ ] `go vet ./...` クリーン
- [ ] `cd web && npm test` パス
- [ ] マイグレーション往復（up→down→up）が通ること
- [ ] 手動 smoke（任意）: call を含むジョブを実行 →呼び出し元 run 詳細で子 run リンク表示、子 run 詳細で「Called by」表示。

## スコープ外（別イテレーション）

- 呼び出しツリー全体の可視化 / ネスト階層表示。
- 呼び出し元からの子 run ステータス集約表示。
