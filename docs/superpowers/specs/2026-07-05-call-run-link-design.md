# call ステップ ↔ 子 run 双方向リンク 設計

**日付:** 2026-07-05
**背景:** `call:` ステップは独立した子 run（`CreateRun`）を作るが、子 run ID はログに出るだけで永続化されず、`step_reports`/`StepReport`/WebUI に紐付けが無い。そのため呼び出し元 run の詳細から子 run へ辿れず、子 run 側も呼び出し元が分からない。

## Goal

呼び出し元 run の詳細で **call ステップから子 run へリンク**でき、子 run の詳細で **「◯◯ から呼ばれた」**と辿れる（双方向）ようにする。

## スコープ

- **含む:** `step_reports` に子 run 紐付けカラム追加、agent の報告、controller/store の保存・取得、逆引き、run 詳細 API の `calledBy`、WebUI（前方向リンク + 逆方向パンくず）、テスト、マイグレーション。
- **含まない:** 呼び出しグラフの可視化（ツリー全体）、ネスト呼び出しの再帰表示、`runs.parent_run_id` の追加（下記のとおり不要）。

## データモデル（1カラムで双方向）

エッジは1箇所 `step_reports` に持ち、両方向をそこから引く（`runs.parent_run_id` は追加しない = DRY）。

```sql
-- 新規マイグレーション（次の連番）
ALTER TABLE step_reports
  ADD COLUMN child_run_id uuid,      -- この call ステップが起こした子 run の ID
  ADD COLUMN call_job_name text;     -- 呼び出したジョブ名（表示用）
CREATE INDEX step_reports_child_run_id_idx ON step_reports (child_run_id);
```
- **前方向**（呼び元 → 子）: 呼び元 run の step_reports 行に `child_run_id` が入る。
- **逆方向**（子 → 呼び元）: `step_reports WHERE child_run_id = <この run>` を引くと、呼び元 run ID・呼び元ジョブ名（`runs` へ JOIN）・呼び出しステップ名が得られる。index 済みで高速。
- **null 許容**: call でないステップ、call 由来でない run は null（＝リンク無し）。

## バックエンドの流れ

`executeCallStep`（`internal/agent/agent.go`）は既に `childRun.ID` を保持しているため、**ステップ報告に載せるだけ**（`CreateRun` は変更しない）。

1. `api.StepReportRequest` に `ChildRunID string` / `CallJobName string`（`omitempty`）を追加。
2. `executeCallStep` が子 run 作成後、そのステップの**終端報告**（Succeeded/Failed/Cancelled で step 行を確定する `ReportStep`）に `ChildRunID = childRun.ID`, `CallJobName = step.Call.Job` を含める。終端で載せることで、子 run の結果に関わらず紐付けが確実に永続化される（子作成に成功していれば ID は既知）。
3. controller の StepReport ハンドラ → store が `step_reports.child_run_id` / `call_job_name` に UPSERT。
4. **foreach/matrix**: call が variant を持つ場合、step_reports は `(run_id, step_index, variant)` 単位の行なので、**variant ごとに別の子 run が自動で紐づく**（追加対応不要）。

## API

- `api.StepReport`（run 詳細の各ステップ）に `ChildRunID string` / `CallJobName string`（`omitempty`）を追加。`GetRunSteps` の SELECT に2カラムを追加。
- run 詳細レスポンスに **`CalledBy *CalledBy`**（逆方向）を追加:
  ```go
  type CalledBy struct {
      ParentRunID   string `json:"parentRunId"`
      ParentJobName string `json:"parentJobName"`
      StepName      string `json:"stepName"`
  }
  ```
  store に `GetRunParent(ctx, childRunID) (*CalledBy, error)` を追加:
  ```sql
  SELECT sr.run_id, r.job_name, sr.step_name
  FROM step_reports sr JOIN runs r ON r.id = sr.run_id
  WHERE sr.child_run_id = $1
  LIMIT 1;
  ```
  call 由来でなければ行なし → `CalledBy` は nil（レスポンスで省略）。

## WebUI（`web/src/routes/RunDetail.svelte`）

- **前方向**: step のレンダリングで `s.childRunId` があれば、ステップ名の隣に `{s.callJobName} ↗` のリンク → `#/runs/${s.childRunId}`。無い場合は従来どおり。
- **逆方向**: run 詳細ヘッダ付近に、`run.calledBy` があれば `Called by {calledBy.parentJobName} ↗`（ツールチップ等に step 名）→ `#/runs/${calledBy.parentRunId}`。無ければ非表示。

## エラー処理 / 後方互換

- 既存 run（新カラム追加前の step_reports）は `child_run_id`/`call_job_name` が null → リンク非表示（グレースフル）。
- 子 run 作成に失敗して call ステップが失敗した場合は `child_run_id` を載せない（リンク先が無い）。
- `GetRunParent` が複数行を返すことは通常ない（1 run は高々1つの call ステップから作られる）。`LIMIT 1` で安全側。

## テスト

- **store**: `child_run_id`/`call_job_name` の保存・取得（`GetRunSteps`）、`GetRunParent` の逆引き（call 由来 run で親を返す／直接 run で nil）。
- **controller**: StepReport が `childRunId`/`callJobName` を返す、run 詳細が `calledBy` を返す（call 由来）／返さない（直接）。
- **agent**: `executeCallStep` が `ChildRunID`/`CallJobName` を報告に含める（フェイククライアントで検証）。
- **web（vitest）**: RunDetail が call ステップに子 run リンクを描画、`calledBy` パンくずを描画。
- **migration**: 新規番号の up/down（down は2カラム + index を drop）。

## スコープ外（別イテレーション）

- 呼び出しツリー全体の可視化。
- ネスト（子が孫を呼ぶ）の階層表示。
- 呼び出し元からの子 run ステータス集約表示。
