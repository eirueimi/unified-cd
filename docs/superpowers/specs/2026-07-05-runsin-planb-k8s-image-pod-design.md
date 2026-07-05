# runsIn Plan B: k8s `runsIn.image` 使い捨て pod 設計

**日付:** 2026-07-05
**前提:** Plan A（host 側 runsIn + k8s の①デフォルト/②named container）はマージ済み。k8s の③別pod（`runsIn.image`）は現在ハードエラーガード中。本設計はそのガードを実装に置き換える。

## Goal

k8s-agent で `runsIn.image: X` のステップを、**image X を載せた使い捨て pod で隔離実行**できるようにする。実行後は pod を削除する。既存の①デフォルト pod コンテナ / ②named container exec は不変。

## スコープ

- **含む:** 使い捨て pod の生成・実行・削除、隔離実行（workspace 非共有、入力は env）、stdout 捕捉/stderr ストリーム、エラー処理、orphan backstop。
- **含まない（別イテレーション = Plan B-2 以降）:** リソース制限（CPU/メモリ）の設定、ネットワーク設定/ポリシー、追加 volume の選択的マウント（③の「引き継ぎ」制御）、Apple `container` CLI の実機検証。

## 3実行コンテキストの最終形（本設計で③k8s が埋まる）

| `runsIn` | host-agent | k8s-agent |
|---|---|---|
| 省略 | ホスト実行 | デフォルト pod コンテナに exec（既存） |
| `container: N` | run 時エラー | pod 内コンテナ N に exec（Plan A） |
| `image: X` | `<rt> run --rm X`（Plan A） | **X で使い捨て pod（本設計）** |

## アーキテクチャ / 実行フロー

Plan A のガード `runsInImageUnsupported`（`internal/k8sagent/agent.go:634`）を**ディスパッチャ化**する。`executeRun` 内の `stepExec` クロージャ（agent.go:166）で分岐する:

- `step.RunsIn != nil && step.RunsIn.Image != ""` → `a.runImageStep(execCtx, step, expandedRun)`（使い捨て pod）
- それ以外 → 既存 `a.exec.ExecStep(podName, execContainer(step), …)`（共有 run pod）

### `runImageStep(ctx, step, script) (exitCode int, stdout string, err error)`

1. **PodSpec 生成**（新設 `buildImageStepPod`）:
   - 単一コンテナ（名 `step`）、`Image: step.RunsIn.Image`
   - `Command: ["sleep", "infinity"]`（起こしたまま exec するため。既存の named container も同様に sleep infinity 起動 → exec している: podbuilder.go:128）
   - `RestartPolicy: Never`
   - `container.Env`: 展開済み `step.Env`（host と同じく `dsl.ExpandTemplate` で各値を展開）＋ `UNIFIED_AGENT_OS`
   - `imagePullSecrets`: job の PodTemplate から継承（私設レジストリの image を引くため）
   - **workspace volume を注入しない**、**artifact sidecar を注入しない**（隔離）
   - `metadata.generateName: <runID先頭>-img-`（並列 image ステップでも一意）
   - `metadata.labels`: `unified-cd/runId`（既存慣習を踏襲、GC/可観測性）
   - `activeDeadlineSeconds`: step の timeout（`step.TimeoutMinutes`）があればそれ、無ければデフォルト（例 3600s）。エージェント異常終了時の orphan を k8s が自動 kill する backstop。
2. `created, err := a.pm.CreatePod(ctx, pod)` → `name := created.Name`
3. `defer a.pm.DeletePod(context.WithoutCancel(ctx), name)` — cancel/失敗/panic 時も**必ず削除**（finally/cache が使う非キャンセル context と同じ考え方）
4. `a.pm.WaitForPodRunning(ctx, name)` — image pull 失敗（ImagePullBackOff→Failed/timeout）はここで error 化 → ステップ失敗
5. `ec, execErr := a.exec.ExecStep(ctx, name, "step", script, stdoutWriter, stderrPusher)` — exit code・stdout 捕捉・stderr ストリーム
6. `return ec, stdout, execErr`

→ 既存の「sleep infinity ＋ exec」モデルと完全一致。`CreatePod / WaitForPodRunning / DeletePod / ExecStep` をそのまま再利用。プーリング（pool.go）は image ステップでは使わない（毎回新規・使い捨て）。

## コンポーネント / 変更ファイル

- `internal/k8sagent/agent.go`:
  - ガード呼び出し（stepExec 冒頭 167-169）を**ディスパッチ分岐**に置換。
  - `runImageStep` メソッド追加。stdout/stderr writer は既存 stepExec（167-174）の書き方（`logLineWriter` ＋ `stderrPusher`）を再利用し、共有 pod パスと同一のログ経路を通す（masker は k8s agent では未適用、共有 pod パスと同様）。
  - `runsInImageUnsupported` 関数は削除（役目終了）。
- `internal/k8sagent/podbuilder.go`:
  - `buildImageStepPod(runID, namespace, image string, env []corev1.EnvVar, pullSecrets []corev1.LocalObjectReference, deadlineSeconds int64) *corev1.Pod` を新設。`injectWorkspace` / sidecar を**呼ばない**専用の最小ビルダー。ラベル/命名慣習は `BuildPod` と揃える。
- `internal/k8sagent/executor.go`・`cmd/k8s-agent/main.go`: **変更なし**。

## データフロー（隔離）

- **入力:** `step.Env`（`dsl.ExpandTemplate` で展開）＋ `UNIFIED_AGENT_OS` を `container.Env` として pod 定義に載せる。workspace 非共有なのでホスト/共有 FS は見えない。→ Plan A 設計の「入力は with:/env、出力は outputs:/stdout」に一致。
- **出力:** stdout は `ExecStep` の stdout writer で捕捉し、既存どおり出力テンプレート（`outputs:`）に供給。stderr は log pusher でストリーム。k8s agent では secret masker は適用されない（host agent 専用の仕組みであり、共有 pod 経路も同様に未適用）。exit code は `ExecStep` 返り値。

## エラー処理 / クリーンアップ

- **pod 作成失敗** → ステップ失敗（error 返却）。
- **image pull 失敗 / pod が Running にならない** → `WaitForPodRunning` が timeout/Failed で error → ステップ失敗（image 名を含む明示メッセージ）。**サイレントにデフォルトコンテナへフォールバックしない**（Plan A の no-silent-fallback 原則を維持）。
- **exec 失敗** → exit code / error を既存 `ExecStep` と同様に surface。
- **クリーンアップ:** `defer DeletePod`（`context.WithoutCancel`）で常に削除。並列 image ステップは `generateName` により各自の pod → 競合なし。
- **orphan backstop:** `activeDeadlineSeconds` で、エージェントが defer 実行前に落ちても k8s が pod を終了。

## テスト

- **単体:** `buildImageStepPod` の PodSpec 検証 — 単一コンテナ / `Image` / `sleep infinity` / `RestartPolicy: Never` / `container.Env`（step.Env＋UNIFIED_AGENT_OS）/ workspace volume 無し / sidecar 無し / `imagePullSecrets` / `generateName` / `activeDeadlineSeconds`。
- **単体:** ディスパッチャのルーティング — image ステップは `runImageStep`（pm/exec をフェイクし CreatePod→WaitForPodRunning→ExecStep→DeletePod の順序と引数を検証）、非 image ステップは共有 pod exec。
- **統合:** 既存のフェイククライアント harness（`agent.go` orchestrate 経由）で、image ステップが pod 作成→exec→**削除**まで行い status を報告することを検証。cancel 時も DeletePod が呼ばれること。
- **置換:** Plan A の `TestRunsInImageUnsupported_OnK8s`（ガードでハードエラー）は削除し、「image がディスパッチされる」テストに置換する。

## Plan B-2 以降（本スコープ外）

- 使い捨て pod の resources（requests/limits）設定。
- ネットワーク設定 / NetworkPolicy 継承。
- workspace/指定 volume の選択的マウント（隔離を緩めるデータ受け渡し）。
- Apple `container` CLI フラグの実機検証。
