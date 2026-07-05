# 実行予定ステップの事前表示（waiting）実装計画

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** run 詳細で、run の spec から導出した実行予定の全ステップ（種別付き）を事前に表示し、未実行分を `Pending`（waiting）状態で見せる。

**Architecture:** controller が `buildStages` を再利用して spec（Steps+Finally）から予定ステップを導出（`plannedSteps`）。`/steps` が予定と報告済みを Index 単位でマージ（未報告=合成 `Pending`）。`api.StepReport` に `Kind`/`Section`/`Matrix` を追加。RunDetail は section 分割 + stageIndex グループ + 種別/Pending 表示。

**Tech Stack:** Go（controller/api）, Svelte + vitest, 実 Postgres 統合テスト（`store.NewTestPostgres`）。

## Global Constraints

- Go モジュール `github.com/eirueimi/unified-cd`。テストは testify（Go）/ vitest（web）。
- 合成 `Pending` は **DB 非保存**（step_reports の status CHECK 制約に無関係）。API レスポンス上のみ。
- 予定導出は `buildStages` 再利用（agent と同じ stageIndex）。matrix/foreach は予定段階で1エントリ。
- spec 取得/parse 失敗時は報告のみ返す（run 詳細を壊さない）。
- 既存の①②（デフォルト/matrix 等）挙動は不変。

---

### Task 1: api 型 + `plannedSteps` 導出（純関数）

**Files:**
- Modify: `internal/api/types.go`（`StepReport` に `Kind`/`Section`/`Matrix`）
- Create: `internal/controller/planned_steps.go`（`plannedSteps` + `stepKind`）
- Test: `internal/controller/planned_steps_test.go`（新規）

**Interfaces:**
- Produces:
  - `StepReport.Kind string` / `Section string` / `Matrix bool`
  - `func plannedSteps(spec dsl.Spec) []api.StepReport`（各予定ステップ; Status="Pending"）
  - `func stepKind(cs api.ClaimStep) string`

- [ ] **Step 1: api 型を追加**

`internal/api/types.go` の `StepReport`（170行、`CallJobName` の後）に:
```go
	Kind    string `json:"kind,omitempty"`    // run|cache|call|uses|uploadArtifact|downloadArtifact|approval
	Section string `json:"section,omitempty"` // main|finally
	Matrix  bool   `json:"matrix,omitempty"`  // true if the (planned) step is a matrix/foreach step
```

- [ ] **Step 2: 失敗するテストを書く**

`internal/controller/planned_steps_test.go`:
```go
package controller

import (
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlannedSteps(t *testing.T) {
	const y = `apiVersion: unified-cd/v1
kind: Job
metadata:
  name: j
spec:
  steps:
    - name: checkout
      run: echo hi
    - name: restore-cache
      cache:
        path: p
        key: k
    - name: build
      matrix:
        dimensions:
          - name: os
            source:
              literal: [linux, windows]
      run: echo build
    - name: upload
      uploadArtifact:
        name: a
        path: p
  finally:
    - name: notify
      run: echo done
`
	job, err := dsl.Parse(strings.NewReader(y))
	require.NoError(t, err)
	ps := plannedSteps(job.Spec)

	require.Len(t, ps, 5) // matrix counts as ONE planned entry
	// index/stageIndex are position-based across steps then finally (shared counter)
	assert.Equal(t, "checkout", ps[0].Name)
	assert.Equal(t, "run", ps[0].Kind)
	assert.Equal(t, "main", ps[0].Section)
	assert.Equal(t, 0, ps[0].StageIndex)
	assert.Equal(t, "Pending", ps[0].Status)

	assert.Equal(t, "restore-cache", ps[1].Name)
	assert.Equal(t, "cache", ps[1].Kind)
	assert.Equal(t, 1, ps[1].StageIndex)

	assert.Equal(t, "build", ps[2].Name)
	assert.Equal(t, "run", ps[2].Kind)
	assert.True(t, ps[2].Matrix)
	assert.Equal(t, 2, ps[2].StageIndex)

	assert.Equal(t, "upload", ps[3].Name)
	assert.Equal(t, "uploadArtifact", ps[3].Kind)
	assert.Equal(t, 3, ps[3].StageIndex)

	// finally: section=finally, stageIndex restarts at 0, stepIndex continues
	assert.Equal(t, "notify", ps[4].Name)
	assert.Equal(t, "finally", ps[4].Section)
	assert.Equal(t, 4, ps[4].Index)
	assert.Equal(t, 0, ps[4].StageIndex)
}
```
**注意（matrix の YAML 記法）:** DSL の `matrix:` はカスタム `UnmarshalYAML` を持つ（dimension 名→値のマップ形式の可能性あり）。上記テストの `matrix:` ブロックがそのまま `dsl.Parse` を通らない場合は、**`internal/dsl/matrix_test.go` や既存 job 定義の実際の matrix 記法に合わせて修正**すること（RED が「plannedSteps 未定義」ではなく parse エラーになったら記法ミス）。matrix ステップが1エントリで `Matrix=true` になることの検証が主眼。

- [ ] **Step 3: RED 確認**

Run: `go test ./internal/controller/ -run TestPlannedSteps -count=1`
Expected: FAIL（`plannedSteps`/`StepReport.Kind` 未定義。もし parse エラーなら matrix YAML 記法を上記注意に従い修正）

- [ ] **Step 4: `plannedSteps` を実装**

`internal/controller/planned_steps.go`:
```go
package controller

import (
	"strings"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/dsl"
)

// plannedSteps derives the full list of steps a run's spec will execute, in
// order, each with Status "Pending". It reuses buildStages so stage indexing
// matches what the agent receives. Matrix/foreach steps are a single entry
// (they expand into variants only at runtime).
func plannedSteps(spec dsl.Spec) []api.StepReport {
	stepIdx := 0
	secrets := map[string]struct{}{} // unused here; buildStages requires it
	var out []api.StepReport
	add := func(stages []api.ClaimStage, section string) {
		for _, st := range stages {
			for _, cs := range api.StageSteps(st) {
				out = append(out, api.StepReport{
					Index:      cs.Index,
					StageIndex: cs.StageIndex,
					Name:       cs.Name,
					Kind:       stepKind(cs),
					Section:    section,
					Matrix:     cs.Matrix != nil,
					Status:     "Pending",
				})
			}
		}
	}
	add(buildStages(spec.Steps, &stepIdx, secrets), "main")
	add(buildStages(spec.Finally, &stepIdx, secrets), "finally")
	return out
}

// stepKind classifies a ClaimStep by its primary action for display.
func stepKind(cs api.ClaimStep) string {
	switch {
	case cs.Cache != nil:
		return "cache"
	case cs.Call != nil:
		if strings.HasPrefix(cs.Call.Job, "git://") {
			return "uses"
		}
		return "call"
	case cs.UploadArtifact != nil:
		return "uploadArtifact"
	case cs.DownloadArtifact != nil:
		return "downloadArtifact"
	case cs.Approval != nil:
		return "approval"
	default:
		return "run"
	}
}
```

- [ ] **Step 5: GREEN 確認**

Run: `go test ./internal/controller/ -run TestPlannedSteps -count=1`
Expected: PASS

- [ ] **Step 6: ビルド**

Run: `go build ./...`
Expected: 成功

- [ ] **Step 7: Commit**

```bash
git add internal/api/types.go internal/controller/planned_steps.go internal/controller/planned_steps_test.go
git commit -m "feat(controller): derive planned steps (kind/section) from run spec"
```

---

### Task 2: `/steps` マージ（予定 + 報告）

**Files:**
- Modify: `internal/controller/planned_steps.go`（`mergedRunSteps` 追加）
- Modify: `internal/controller/api_runs.go`（`handleGetRunSteps` で spec 取得→マージ）
- Test: `internal/controller/planned_steps_test.go`（`mergedRunSteps` の単体テストを追記）

**Interfaces:**
- Consumes: `plannedSteps`（Task 1）, `store.GetRunSpec`
- Produces: `func mergedRunSteps(reported []api.StepReport, spec dsl.Spec) []api.StepReport`

- [ ] **Step 1: 失敗するテストを書く**

`internal/controller/planned_steps_test.go` に追記:
```go
func TestMergedRunSteps(t *testing.T) {
	const y = `apiVersion: unified-cd/v1
kind: Job
metadata:
  name: j
spec:
  steps:
    - name: a
      run: echo a
    - name: b
      cache:
        path: p
        key: k
    - name: c
      run: echo c
`
	job, err := dsl.Parse(strings.NewReader(y))
	require.NoError(t, err)

	// only step 0 (a) reported so far
	reported := []api.StepReport{{Index: 0, StageIndex: 0, Name: "a", Status: "Succeeded"}}
	m := mergedRunSteps(reported, job.Spec)

	require.Len(t, m, 3)
	assert.Equal(t, "Succeeded", m[0].Status) // reported wins
	assert.Equal(t, "run", m[0].Kind)         // kind attached from planned
	assert.Equal(t, "Pending", m[1].Status)   // b not reported → pending
	assert.Equal(t, "cache", m[1].Kind)
	assert.Equal(t, "Pending", m[2].Status)   // c not reported → pending
}
```

- [ ] **Step 2: RED 確認**

Run: `go test ./internal/controller/ -run TestMergedRunSteps -count=1`
Expected: FAIL（`mergedRunSteps` 未定義）

- [ ] **Step 3: `mergedRunSteps` を実装**

`internal/controller/planned_steps.go` に追加:
```go
// mergedRunSteps overlays reported step statuses onto the planned step list.
// For each planned step index: if the agent has reported it (possibly as
// multiple matrix variants), use the reported rows (with kind/section attached
// from the plan); otherwise emit the planned "Pending" entry. Reported rows
// whose index is not in the plan (shouldn't happen) are appended verbatim so
// real data is never dropped.
func mergedRunSteps(reported []api.StepReport, spec dsl.Spec) []api.StepReport {
	planned := plannedSteps(spec)
	byIndex := map[int][]api.StepReport{}
	for _, r := range reported {
		byIndex[r.Index] = append(byIndex[r.Index], r)
	}
	plannedIdx := map[int]bool{}
	var out []api.StepReport
	for _, p := range planned {
		plannedIdx[p.Index] = true
		if rs, ok := byIndex[p.Index]; ok {
			for _, r := range rs {
				r.Kind, r.Section, r.Matrix = p.Kind, p.Section, p.Matrix
				out = append(out, r)
			}
			continue
		}
		out = append(out, p)
	}
	for _, r := range reported {
		if !plannedIdx[r.Index] {
			out = append(out, r)
		}
	}
	return out
}
```

- [ ] **Step 4: GREEN 確認**

Run: `go test ./internal/controller/ -run TestMergedRunSteps -count=1`
Expected: PASS

- [ ] **Step 5: handler を配線**

`internal/controller/api_runs.go` の `handleGetRunSteps`（171行）を、報告取得後に spec マージするよう変更:
```go
func (s *Server) handleGetRunSteps(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	steps, err := s.store.GetRunSteps(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Overlay onto the planned step list so not-yet-run steps show as Pending.
	// Best-effort: if the spec is missing/unparseable, fall back to reported.
	if specJSON, sErr := s.store.GetRunSpec(r.Context(), id); sErr == nil && len(specJSON) > 0 {
		var spec dsl.Spec
		if json.Unmarshal(specJSON, &spec) == nil {
			steps = mergedRunSteps(steps, spec)
		}
	}
	if steps == nil {
		steps = []api.StepReport{}
	}
	writeJSON(w, http.StatusOK, steps)
}
```
（`encoding/json` と `internal/dsl` が api_runs.go で import 済みか確認。dsl は import 済み、json も既存。無ければ追加。）

- [ ] **Step 6: handler の統合テスト（実 Postgres）**

`internal/controller/planned_steps_test.go` に、既存の controller テストハーネス（`internal/controller/api_runs_test.go` の `TestAPI_GetRun` 等）に倣って、run と spec を保存 → `GET /runs/{id}/steps` → レスポンスに Pending 予定ステップが含まれることを検証するテストを追加する。ハーネス（`newTestServer`/run 作成/HTTP GET）は既存 controller テストの書き方に合わせること。実 Postgres 前提（`testing.Short()` skip）。もし既存ハーネスで run の spec を保存する方法が不明なら、`store` の run 作成/spec 保存 API（`GetRunSpec` の対になる保存経路）を読んで合わせる。

- [ ] **Step 7: 回帰 + ビルド**

Run: `go build ./... && go test ./internal/controller/ -run "PlannedSteps|MergedRunSteps|RunSteps|GetRun" -count=1 -timeout 20m`
Expected: ビルド成功、PASS（既存の `/steps` を使うテストが Pending 追加で壊れないこと。壊れたら、そのテストは spec 未保存の run を使っている可能性 → マージは spec 無し時に報告のみ返すので影響しないはず。壊れたら最小修正）。

- [ ] **Step 8: Commit**

```bash
git add internal/controller/planned_steps.go internal/controller/api_runs.go internal/controller/planned_steps_test.go
git commit -m "feat(controller): /steps merges planned (Pending) steps with reported"
```

---

### Task 3: frontend — section 分割 + 種別 + Pending 表示

**Files:**
- Modify: `web/src/lib/utils.js`（`statusBadge` に `Pending`）
- Modify: `web/src/routes/RunDetail.svelte`（`groupedStages`→section 分割、種別表示、Steps/Finally 見出し）
- Test: `web/src/routes/RunDetail.test.js`（追記）

**Interfaces:**
- Consumes: `/steps` の各要素 `{ index, stageIndex, name, status, kind, section, matrix, ... }`（Task 1/2）

- [ ] **Step 1: 失敗するテストを書く**

`web/src/routes/RunDetail.test.js` に、既存のモック方式に倣って追記（`/steps` に Pending/kind/section を含める）:
```js
it("shows planned steps as Pending with kind, split into Steps/Finally sections", async () => {
  // mock GET /runs/:id → { status:"Running", ... }
  // mock GET /runs/:id/steps → [
  //   { index:0, stageIndex:0, name:"build", status:"Succeeded", kind:"run", section:"main" },
  //   { index:1, stageIndex:1, name:"restore-cache", status:"Pending", kind:"cache", section:"main" },
  //   { index:2, stageIndex:0, name:"notify", status:"Pending", kind:"run", section:"finally" },
  // ]
  // render RunDetail; expect:
  //  - a "Pending" badge for restore-cache
  //  - the text "[cache]" (or kind) near restore-cache
  //  - a "Finally" section heading, with "notify" under it
});
```
（実アサーションは既存 `RunDetail.test.js` の testing-library 記法に合わせて具体化する。）

- [ ] **Step 2: RED 確認**

Run: `cd web && npm test -- RunDetail`
Expected: FAIL（section 見出し/種別/Pending 未実装）

- [ ] **Step 3: `statusBadge` に Pending を追加**

`web/src/lib/utils.js` の `statusBadge` に `Pending` → `badge-pending` のマッピングを追加（`.badge-pending` クラスと `--badge-pending-*` 変数は app.css に既存）。既存の switch/マップに1行足す形。例:
```js
// 既存の分岐に:
if (status === "Pending") return "badge badge-pending";
```
（既存の `statusBadge` の返り値形式（`"badge badge-xxx"` か `"badge-xxx"` か）を読んで合わせること。）

- [ ] **Step 4: `groupedStages` を section 分割に**

`web/src/routes/RunDetail.svelte` の `groupedStages`（25-34行）を、section ごとにグループ化する `stepSections` に置換（finally の stageIndex は 0 から振り直しなので main と混ざらないよう section 内でグループ化 → 既存の finally 衝突も解消）:
```js
  $: stepSections = (() => {
    const bySection = { main: [], finally: [] };
    for (const s of steps) {
      (s.section === "finally" ? bySection.finally : bySection.main).push(s);
    }
    const group = (arr) => {
      const map = new Map();
      for (const s of arr) {
        if (!map.has(s.stageIndex)) map.set(s.stageIndex, []);
        map.get(s.stageIndex).push(s);
      }
      return [...map.entries()]
        .sort(([a], [b]) => a - b)
        .map(([stageIndex, stageSteps]) => ({ stageIndex, steps: stageSteps }));
    };
    const out = [{ section: "main", label: "Steps", groups: group(bySection.main) }];
    if (bySection.finally.length)
      out.push({ section: "finally", label: "Finally", groups: group(bySection.finally) });
    return out;
  })();
```
`groupedStages` を他で参照している箇所がないか grep（`groupedStages`）で確認。ログのフィルタ等は `stepIndex` ベースで `groupedStages` に依存しないはずだが、依存が残る場合は `stepSections.flatMap(s => s.groups)` で後方互換の派生を用意する。

- [ ] **Step 5: 描画を section ループ + 種別に**

`web/src/routes/RunDetail.svelte` のステップ描画（372行 `<h2>Steps</h2>` 〜 `<h2>Logs</h2>` の手前）を、section ループに変更:
- 外側を `{#each stepSections as sec}` にし、各 section で見出し `<h2>{sec.label}</h2>` を出す。
- 内側の `{#each groupedStages as group ...}` を `{#each sec.groups as group (group.stageIndex)}` に変更。
- parallel-indented 行と single-step 行の**両方**で、`<span class="step-name">{s.name}</span>` / `{s0.name}` の隣に種別を表示:
```svelte
                {#if s.kind}<span class="step-kind meta">[{s.kind}]{#if s.matrix} matrix{/if}</span>{/if}
```
（single-step 側は `s0.kind`/`s0.matrix`。）
- status バッジ `{statusBadge(s.status)}` はそのまま（Task 3-3 で `Pending` 対応済み）。

`.step-kind` の最小スタイルを app.css に追加（既存 `.meta` を流用してもよい）:
```css
.step-kind { font-size: 0.7rem; margin-left: 0.4rem; }
```

- [ ] **Step 6: GREEN 確認**

Run: `cd web && npm test -- RunDetail`
Expected: PASS

- [ ] **Step 7: web 一式**

Run: `cd web && npm test`
Expected: PASS（既存の RunDetail/step テストが section 分割で壊れていないこと。壊れたら、そのテストのモック `/steps` に `section:"main"` を足すなど最小修正。既存の無関係な AuthSetup ja-locale 失敗は対象外）。

- [ ] **Step 8: Commit**

```bash
git add web/src/lib/utils.js web/src/routes/RunDetail.svelte web/src/routes/RunDetail.test.js web/src/app.css
git commit -m "feat(web): show planned steps (Pending) with kind, split Steps/Finally sections"
```

---

## 最終確認（全タスク後）

- [ ] `go build ./...` 成功
- [ ] `go test ./internal/controller/... ./internal/api/... -count=1 -timeout 20m` パス（実 Postgres 込み）
- [ ] `go vet ./...` クリーン
- [ ] `cd web && npm test` パス
- [ ] 手動 smoke: pending/running の run 詳細で全予定ステップが並び、未実行は `Pending`（waiting）バッジ + 種別表示、finally は別セクション、実行が進むと status がライブ更新。

## スコープ外（別イテレーション）

- 実行前の手動開始/一時停止ゲート。
- matrix/foreach の予定段階での variant 事前展開。
- 予定ステップの DAG 可視化。
