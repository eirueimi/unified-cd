# エージェント再起動・強制終了時の孤児 Run 回収 設計

- 日付: 2026-07-06
- ステータス: 設計承認済み（対話で ①+② を承認）

## 背景と動機

実機事故: docker compose の再起動で標準エージェントのプロセスが SIGKILL され、旧プロセスが claim していた親 Run（call ステップの子 Run ポーリング中）が孤児化した。既存の stuck-run reaper（[stuckrun_reaper.go](../../internal/controller/stuckrun_reaper.go)）は「ハートビート 90 秒途絶 or エージェント行消滅」を条件とするため、**同じ agent ID が即座に再登録してハートビートを再開すると発火しない**。Run は永久に Running のまま残る。

## スコープ

- **① 起動時リコンサイル（本命）**: エージェントは register 成功後・claim ループ開始前に、「自分の ID で claim されたまま Running の Run」をコントローラに失敗させる。SIGKILL→再起動の穴を塞ぐ。
- **② 強制終了時の即時報告（おまけ）**: 2 発目のシグナルで `os.Exit(130)` する強制終了パス（[shutdown.go](../../internal/agent/shutdown.go)）で、exit 直前に同じ回収 API を best-effort（タイムアウト 3 秒）で呼ぶ。エージェントが戻ってこないケースで reaper の 90 秒を待たずに済む。
- **スコープ外**: drain タイムアウト経過時（プロセス存命のままステップ失敗→FinishRun まで既存コードが走るため対処不要）。k8s-agent（Deployment 再起動は同一 ID 再登録なので①の恩恵を受けるが、k8s-agent 側の組み込みは本設計では標準エージェントと同じ Client 呼び出しを cmd/k8s-agent 起動列に追加するのみ）。

## 意味論（既存 reaper に準拠）

- 孤児 Run は **Failed**（re-queue しない — 部分実行済みステップの再実行は副作用が二重になる）。
- `MarkRunFinished` 経由で mutex / named-lock を解放。
- 子孫 Run は **カスケードキャンセル**（`cancelDescendantRuns`、reaper と同一）。
- 既に terminal な Run は対象外（SQL で `status='Running'` に限定 = 冪等）。

## API（新設 1 本）

`POST /api/v1/agents/{agentId}/runs/reconcile` — BearerAuth（エージェントトークン）、既存のエージェントルート群（server.go:376-388）に追加。

- ハンドラ: `claimed_by = {agentId} AND status = 'Running'` の Run ID を列挙し、各 ID に `MarkRunFinished(Failed)` + `cancelDescendantRuns`。reaper のループ体と同じ処理を関数化して共有する。
- レスポンス: `200 {"failedRuns": <n>}`。
- 対象ゼロは正常（`{"failedRuns": 0}`）。

## store（新設 1 メソッド）

```go
// ListRunningRunIDsByAgent returns IDs of Running runs claimed by agentID.
ListRunningRunIDsByAgent(ctx context.Context, agentID string) ([]string, error)
```

SQL: `SELECT id FROM runs WHERE claimed_by = $1 AND status = 'Running'`。

## agent

- Client に `ReconcileRuns(ctx, agentID string) (int, error)` を追加。
- **①**: `Agent.Run` の register 成功直後（claim ループ開始前）に呼ぶ。失敗はリトライ（`retryUntilSuccess`）— 回収できないまま claim を始めると穴が残るため。回収数 > 0 なら WARN ログ。
  - claim ループ開始前なので、新プロセス自身の claim と競合しない。
  - 同一 ID の別プロセスが並走している誤設定では相手の Run を殺すが、ID 重複自体が構成エラーであり許容（ドキュメントに注記）。
- **②**: `ShutdownContext()` を `ShutdownContext(onForce func())` に変更（内部 API、呼び出し元は cmd/agent と cmd/k8s-agent）。2 発目シグナル受信時、`os.Exit(130)` の前に `onForce()` を呼ぶ。main は onForce に「3 秒タイムアウトの context で ReconcileRuns を 1 回」を渡す。ここは best-effort（エラーでも exit を止めない）。

## テスト

- store: `ListRunningRunIDsByAgent` — claim 済み Running のみ返し、他エージェント・terminal は返さない。
- controller（実 PG + ルータ）: claim 済み Running Run + step_reports 経由の子 Run を用意 → agent トークンで reconcile POST → 親 Failed・子 Cancelled・`{"failedRuns":1}`。二回目の POST は `{"failedRuns":0}`（冪等）。ユーザートークンでは 401。
- agent（httptest ハーネス）: `Run` 起動時、`/reconcile` が最初の `/claim` より先に呼ばれる。force フック関数はシグナル配線から切り出して直接テスト（Windows ではプロセスへのシグナル送出テストが困難なため、配線はレビューで担保）。

## スコープ外（YAGNI）

- Run の re-queue / resume（部分実行の再開はステップ冪等性の設計が必要 — 別件）。
- reaper の staleAfter 調整、incarnation ID 方式（①で不要になる）。
