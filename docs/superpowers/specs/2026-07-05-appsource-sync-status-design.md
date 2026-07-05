# AppSource Sync Status/Error 設計（WebUI sync ボタンの完了ローディング）

- 日付: 2026-07-05
- ステータス: 設計承認済み

## 背景と動機

WebUI（[AppSourceList.svelte](web/src/routes/AppSourceList.svelte)）の Sync ボタンは、押下時に `syncing` フラグで一瞬ローディングになるが、`POST /api/v1/appsources/{name}/sync` は [`ResetAppSourceCommit`](internal/controller/api_appsources.go:81) を呼んで**即 204 を返すだけ**（次回 reconcile に差分検知させる仕掛け）で、実際の Git sync 完了を待たない。結果、ローディングがほぼ即終了し、ユーザーは「押したのに変化が分からない」。さらに sync 失敗（例: 認証エラー）はサーバログに出るだけで **API/UI には出てこない**。

目的: Sync ボタンを**実 sync 完了まで**ローディングにし、**失敗時はエラーを UI に表示**する。

## 制約（現状）

- `AppSourceMeta` API は `lastSyncedAt` / `lastCommit` / `updatedAt` のみ。sync 状態やエラーが無い。
- 成功検知は `lastSyncedAt` の更新で可能だが、**失敗は API から判別できない** → 完了検知には状態フィールドが必要。

## 設計

### データモデル（store）

`app_sources` テーブルに列を追加（マイグレーション）:
- `sync_status text NOT NULL DEFAULT ''` — `''`(未同期) | `Syncing` | `Synced` | `Failed`
- `last_error text NOT NULL DEFAULT ''` — 失敗時のメッセージ。成功時は空。

`store.AppSource` 構造体に `SyncStatus string` / `LastError string` を追加。全 Scan 箇所（Upsert/Get/List）で両列を読む。

### store メソッド

- 既存 [`UpdateAppSourceSyncState`](internal/store/postgres.go:1591)（成功時に呼ばれる）を拡張: `last_commit`/`synced_at`/`managed_jobs` 更新に加え **`sync_status='Synced'`, `last_error=''`** もセット（シグネチャ変更なし）。
- 新規 `SetAppSourceSyncStatus(ctx, name, status, lastError string) error` — 任意状態へ更新。`Syncing`（トリガ時）と `Failed`（reconcile 失敗時）に使う。`store.Store` interface（[store.go:215-221](internal/store/store.go:215)）とテスト用 fake store の両方に追加。

### reconciler

- 成功: 既存どおり `UpdateAppSourceSyncState`（→ Synced + error クリア）。
- 失敗: 現状 [appsource_reconciler.go:75-76](internal/controller/appsource_reconciler.go:75) で WARN ログのみ → **`SetAppSourceSyncStatus(name, "Failed", err.Error())` を追加**。
- 定期 reconcile は終状態のみ記録（毎周期 `Syncing` にはしない＝行がチラつかない）。SHA 未変化の早期 return は状態を変えない（既に Synced 前提）。

### sync エンドポイント

[`handleSyncAppSource`](internal/controller/api_appsources.go:79): `ResetAppSourceCommit` に加え **`SetAppSourceSyncStatus(name, "Syncing", "")`** を呼んでから 204。→ UI が即座に進行中を認識、reconciler が Synced/Failed へ遷移させる。

### API

`AppSourceMeta` に `syncStatus string` / `lastError string`（`json:"...,omitempty"`）を追加。[`appSourceToMeta`](internal/controller/api_appsources.go:88) でマップ。

### フロントエンド（AppSourceList.svelte）

- クリック → `POST .../sync` → `GET /api/v1/appsources` を **~1.5s 間隔で poll**。対象行の `syncStatus` が `Syncing` でなくなったら loading 解除。
- **client 側 timeout 60s** で保険（reconciler 停止時の無限 poll 防止）。timeout 時は「sync が想定より長い/失敗の可能性 — ログ確認」を表示。
- `Failed` → ボタン loading 解除 ＋ その行に `lastError` を表示。
- `Synced` → 更新後の `lastCommit`/`lastSyncedAt` を反映。
- テーブルに status バッジ（Syncing/Synced/Failed）を常時表示。Failed は `lastError` を title 属性(tooltip)に。

## マイグレーション番号の注意

main(8be360d) の最新は `002_add_role`。本 feature は `003_appsource_sync_status` とする。**ただし** 別ブランチ（matrix の `003_matrix_variant`、appsource-multikind の `003_*`）が先に main へマージされると番号衝突する。マージ前に main の最新マイグレーションを確認し、必要なら次の空き番号へ renumber すること。

## スコープ外（YAGNI）

- sync の同期実行化（エンドポイントで reconcile 完了までブロック）。非同期＋poll を維持。
- observedGeneration 等の厳密な世代管理。`Syncing`→非`Syncing` 遷移 + client timeout で十分。
- WebSocket/SSE によるプッシュ更新。poll で足りる。
