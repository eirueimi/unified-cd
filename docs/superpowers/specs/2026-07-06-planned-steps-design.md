# 実行予定ステップの事前表示（waiting）設計

**日付:** 2026-07-06
**背景:** run 詳細のステップ一覧は step_reports（agent が実行時に報告）だけを表示するため、実行前・実行中は**未着手のステップが見えない**。「どういうステップが実行されるのか」を事前に把握できない。

## Goal

run 詳細で、その run の spec から導出した**実行予定の全ステップ**（種別付き）を事前に表示し、未実行分は **`Pending`（waiting）**状態で見せる。実行が進むにつれ各ステップの status がライブ更新される。**実行フローは止めない（表示のみ）。**

## スコープ

- **含む:** 予定ステップの backend 導出（`buildStages` 再利用）、`/steps` での予定+報告マージ、`StepReport` への `Kind`/`Section` 追加、合成 `Pending` status、RunDetail の section 分割 + Kind 表示 + Pending バッジ、テスト。副次的に finally の stageIndex 衝突（誤 parallel 表示）解消。
- **含まない:** 実行前の手動ゲート/一時停止（既存 approval とは別）、matrix/foreach の予定段階での変数展開（実行時展開のまま、予定は1エントリ）、予定行の DB 事前挿入。

## 決定事項

- 表示のみ（実行は止めない）。
- 各予定ステップに **種別（Kind）** を表示（run / cache / call / uploadArtifact / downloadArtifact / approval、matrix/foreach 印）。
- **`finally:` ステップも予定として表示**（section = finally）。
- matrix/foreach は**予定段階では1エントリ**（実行時に variant 展開されたら報告 variant を表示）。
- 導出は **backend**（`buildStages` 再利用で agent と同じ stageIndex ロジック＝DRY）。

## アーキテクチャ / データフロー

### 1. backend — 予定ステップ導出
`internal/controller` に `plannedSteps(spec dsl.Spec) []api.StepReport` を新設:
- `buildClaimResponse` と同様に、共有 `stepIdx` で `buildStages(spec.Steps, &stepIdx, …)` と `buildStages(spec.Finally, &stepIdx, …)` を実行し、`api.StageSteps` で各 `ClaimStep` に平坦化する。
- 各 ClaimStep から予定 `api.StepReport` を生成: `{Index, StageIndex, Name, Kind, Section, Status: "Pending"}`。
- **Kind 導出**（ClaimStep のフィールドから、優先順）: `Cache != nil`→`cache` / `Call != nil`→`call`（`Job` が `git://` 始まりなら `uses`）/ `UploadArtifact != nil`→`uploadArtifact` / `DownloadArtifact != nil`→`downloadArtifact` / `Approval != nil`→`approval` / それ以外→`run`。`Matrix != nil` の場合は Kind を基底アクション（通常 `run`）のままとし、matrix/foreach である旨を UI で併記できるよう planned エントリにフラグ（`api.StepReport` の bool `Matrix` など）を持たせる。
- **Section**: `spec.Steps` 由来は `"main"`、`spec.Finally` 由来は `"finally"`。

### 2. backend — `/steps` マージ
`handleGetRunSteps`（`internal/controller/api_runs.go`）を拡張:
1. `reported := store.GetRunSteps(id)`（既存、報告済み step_reports）。
2. `specJSON := store.GetRunSpec(id)` → `dsl.Spec` に unmarshal → `planned := plannedSteps(spec)`。
3. **Index 単位でマージ**: 報告を Index でグルーピング。planned を順に走査し、その Index に報告行があれば**報告行**を採用（実 status・variant を保持）、無ければ planned の **`Pending`** 行。いずれの場合も planned から **`Kind`/`Section` を付与**（報告行にも）。
4. 返却は planned の順（section→stageIndex→index）に、報告 variant はその Index の位置に展開。
- 結果: `/steps` が**常に全予定フロー**を返す（pending run=全 Pending、実行中=済み+Pending、完了=全報告）。spec 取得/parse 失敗時は従来どおり報告のみ返す（グレースフル）。

### 3. api 型
- `api.StepReport` に追加:
  ```go
  Kind    string `json:"kind,omitempty"`    // run|cache|call|uses|uploadArtifact|downloadArtifact|approval
  Section string `json:"section,omitempty"` // main|finally
  ```
- 合成 status **`"Pending"`** は **DB 非保存**（step_reports の status CHECK 制約に無関係）。API レスポンス上だけの状態。

### 4. frontend（`web/src/routes/RunDetail.svelte`）
- `/steps` に `Pending`/`Kind`/`Section` が乗る。
- **section で main/finally に分割 → 各々 stageIndex でグループ化**して描画（「Steps」「Finally」見出し）。これにより finally の stageIndex（buildStages で 0 から振り直し）が main と衝突して誤 parallel 表示になる**既存の潜在バグも解消**。
- 各ステップ行に **種別 `[cache]` 等**を表示。status バッジは **`Pending`** を **waiting 見た目（グレー系、`.badge-pending`）**で表示。
- 既存の step ポーリング（`startStepPolling`）で、実行が進むと Pending → Running → Succeeded にライブ更新。

## エラー処理 / 後方互換

- spec 取得/parse 失敗 → planned をスキップし報告のみ返す（run 詳細は壊さない）。
- 既存の完了 run: 全ステップが報告済みなのでマージは実質無変化（+ Kind/Section が付く、+ finally が section 分割される）。
- 合成 `Pending` は保存されないので、agent の報告や DB 制約に影響しない。
- matrix/foreach: 予定は1エントリ（`Pending`）。実行時に variant が報告されたら、その Index は報告 variant 群に置き換わる（二重表示しない）。

## テスト

- **backend `plannedSteps`**: run/cache/call/uploadArtifact/downloadArtifact/approval/parallel/matrix/foreach + finally を含む spec で、各予定エントリの `Index/StageIndex/Name/Kind/Section` を検証。matrix は1エントリ。
- **backend `/steps` マージ**: (a) 未実行 run=全 `Pending`、(b) 一部報告=報告+残 `Pending`、(c) matrix=展開前1件 `Pending`→報告後は variant 群、(d) 報告行に Kind/Section が付く、(e) spec 取得失敗時は報告のみ。
- **frontend(vitest)**: `Pending` バッジ・種別表示・main/finally section 分割・混在（報告+Pending）描画。

## スコープ外（別イテレーション）

- 実行前の手動開始/一時停止ゲート。
- matrix/foreach の予定段階での variant 事前展開（値が定数リテラルなら理論上可能だが本スコープ外）。
- 予定ステップの依存グラフ（DAG）可視化。
