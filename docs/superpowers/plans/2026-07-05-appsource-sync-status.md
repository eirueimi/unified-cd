# AppSource Sync Status/Error 実装計画

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** WebUI の AppSource Sync ボタンを実 sync 完了までローディングにし、失敗時はエラーを UI に表示する。そのため sync 状態(`syncStatus`)とエラー(`lastError`)を store/API に公開する。

**Architecture:** `app_sources` に `sync_status`/`last_error` 列を追加。sync トリガで `Syncing`、reconcile 成功で `Synced`(+error クリア)、失敗で `Failed`(+error) を記録。API が両値を返し、フロントは対象行が非 `Syncing` になるまで poll(＋60s timeout)してローディングを維持、失敗は表示。

**Tech Stack:** Go, pgx (Postgres), Svelte, testify。store テストは実 Postgres（Docker 必須、当環境で利用可）。

## Global Constraints

- Go モジュール: `github.com/eirueimi/unified-cd`。
- `sync_status` の値: `''`(未同期) | `Syncing` | `Synced` | `Failed`。この4値のみ。
- `last_error`/`sync_status` 列は `NOT NULL DEFAULT ''`。
- マイグレーション番号は `003_appsource_sync_status`。**別ブランチが先に 003 を main へマージしたら次の空き番号へ renumber**（matrix `003_matrix_variant` 等）。
- store テストは既存 `internal/store/postgres_appsources_test.go` と同じ test-Postgres セットアップを使う。
- 秘密情報はログ/エラーに出さない（`last_error` は git のエラー文言で、既存もマスク対象外の一般メッセージ）。

---

### Task 1: マイグレーション ＋ store モデル/メソッド

**Files:**
- Create: `internal/store/migrations/003_appsource_sync_status.up.sql`
- Create: `internal/store/migrations/003_appsource_sync_status.down.sql`
- Modify: `internal/store/store.go`（`AppSource` 構造体 ＋ `Store` interface）
- Modify: `internal/store/postgres.go`（Scan 3箇所、`UpdateAppSourceSyncState` 拡張、`SetAppSourceSyncStatus` 追加）
- Test: `internal/store/postgres_appsources_test.go`

**Interfaces:**
- Produces:
  - `store.AppSource` に `SyncStatus string` / `LastError string`
  - `Store.SetAppSourceSyncStatus(ctx context.Context, name, status, lastError string) error`
  - `UpdateAppSourceSyncState` は従来引数のまま、内部で `sync_status='Synced', last_error=''` も設定

- [ ] **Step 1: Write the migration**

`internal/store/migrations/003_appsource_sync_status.up.sql`:
```sql
ALTER TABLE app_sources
  ADD COLUMN sync_status text NOT NULL DEFAULT '',
  ADD COLUMN last_error  text NOT NULL DEFAULT '';
```
`internal/store/migrations/003_appsource_sync_status.down.sql`:
```sql
ALTER TABLE app_sources
  DROP COLUMN sync_status,
  DROP COLUMN last_error;
```

- [ ] **Step 2: Write the failing test**

`internal/store/postgres_appsources_test.go` に追記（既存のこのファイルの test-Postgres セットアップヘルパーに倣うこと）:
```go
func TestAppSource_SyncStatusLifecycle(t *testing.T) {
	pg, cleanup := newTestPostgres(t) // 既存ヘルパー名に合わせる
	defer cleanup()
	ctx := context.Background()

	_, err := pg.UpsertAppSource(ctx, "s1", []byte(`{}`))
	require.NoError(t, err)

	// 初期は空
	got, err := pg.GetAppSource(ctx, "s1")
	require.NoError(t, err)
	assert.Equal(t, "", got.SyncStatus)
	assert.Equal(t, "", got.LastError)

	// Syncing へ
	require.NoError(t, pg.SetAppSourceSyncStatus(ctx, "s1", "Syncing", ""))
	got, _ = pg.GetAppSource(ctx, "s1")
	assert.Equal(t, "Syncing", got.SyncStatus)

	// Failed + error
	require.NoError(t, pg.SetAppSourceSyncStatus(ctx, "s1", "Failed", "boom"))
	got, _ = pg.GetAppSource(ctx, "s1")
	assert.Equal(t, "Failed", got.SyncStatus)
	assert.Equal(t, "boom", got.LastError)

	// 成功記録で Synced + error クリア
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "s1", "sha1", time.Now(), []string{"j"}))
	got, _ = pg.GetAppSource(ctx, "s1")
	assert.Equal(t, "Synced", got.SyncStatus)
	assert.Equal(t, "", got.LastError)
	assert.Equal(t, "sha1", got.LastCommit)
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/store/ -run TestAppSource_SyncStatusLifecycle -v`
Expected: FAIL（`SyncStatus` フィールド/`SetAppSourceSyncStatus` 未定義でコンパイルエラー）

- [ ] **Step 4: Add struct fields and interface method**

`internal/store/store.go` の `AppSource` 構造体に:
```go
	SyncStatus string
	LastError  string
```
`Store` interface の AppSource セクションに:
```go
	SetAppSourceSyncStatus(ctx context.Context, name, status, lastError string) error
```

- [ ] **Step 5: Update Scans and methods in postgres.go**

3つのクエリに列を追加し Scan を更新する。

`UpsertAppSource` の RETURNING と Scan:
```go
		RETURNING name, spec, last_synced_at, last_commit, managed_jobs, updated_at, sync_status, last_error`
	...
	err := p.pool.QueryRow(ctx, q, name, spec).
		Scan(&a.Name, &a.Spec, &a.LastSyncedAt, &a.LastCommit, &managedJobs, &a.UpdatedAt, &a.SyncStatus, &a.LastError)
```
`GetAppSource` の SELECT と Scan:
```go
	const q = `SELECT name, spec, last_synced_at, last_commit, managed_jobs, updated_at, sync_status, last_error FROM app_sources WHERE name = $1`
	...
		Scan(&a.Name, &a.Spec, &a.LastSyncedAt, &a.LastCommit, &managedJobs, &a.UpdatedAt, &a.SyncStatus, &a.LastError)
```
`ListAppSources` の SELECT と Scan（同様に末尾2列を追加）:
```go
	const q = `SELECT name, spec, last_synced_at, last_commit, managed_jobs, updated_at, sync_status, last_error FROM app_sources ORDER BY name`
	...
		if err := rows.Scan(&a.Name, &a.Spec, &a.LastSyncedAt, &a.LastCommit, &managedJobs, &a.UpdatedAt, &a.SyncStatus, &a.LastError); err != nil {
```
`UpdateAppSourceSyncState` を拡張（成功＝Synced、error クリア）:
```go
	_, err := p.pool.Exec(ctx,
		`UPDATE app_sources SET last_commit = $1, last_synced_at = $2, managed_jobs = $3, sync_status = 'Synced', last_error = '', updated_at = NOW() WHERE name = $4`,
		lastCommit, syncedAt, managedJobs, name)
```
新規メソッドを追加:
```go
// SetAppSourceSyncStatus sets the sync_status and last_error of an AppSource.
func (p *Postgres) SetAppSourceSyncStatus(ctx context.Context, name, status, lastError string) error {
	_, err := p.pool.Exec(ctx,
		`UPDATE app_sources SET sync_status = $1, last_error = $2, updated_at = NOW() WHERE name = $3`,
		status, lastError, name)
	return err
}
```

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./internal/store/ -run TestAppSource_SyncStatusLifecycle -v`
Expected: PASS

- [ ] **Step 7: Regression (store package)**

Run: `go test ./internal/store/...`
Expected: PASS（既存 appsource テストが新列で壊れていないこと）

- [ ] **Step 8: Commit**

```bash
git add internal/store/migrations/003_appsource_sync_status.up.sql internal/store/migrations/003_appsource_sync_status.down.sql internal/store/store.go internal/store/postgres.go internal/store/postgres_appsources_test.go
git commit -m "feat(store): add sync_status/last_error to app_sources with SetAppSourceSyncStatus"
```

---

### Task 2: API 公開 ＋ sync エンドポイントで Syncing 設定

**Files:**
- Modify: `internal/api/types.go`（`AppSourceMeta` に2フィールド）
- Modify: `internal/controller/api_appsources.go`（`appSourceToMeta` マップ、`handleSyncAppSource` で Syncing 設定）
- Test: `internal/controller/api_appsources_test.go`（無ければ新規。既存 controller テストの test-Postgres/Server セットアップに倣う）

**Interfaces:**
- Consumes: `store.AppSource.SyncStatus/LastError`、`Store.SetAppSourceSyncStatus`（Task 1）
- Produces: API JSON に `syncStatus` / `lastError`

- [ ] **Step 1: Write the failing test**

`internal/controller/api_appsources_test.go` に、sync POST 後に GET で `syncStatus == "Syncing"` になることを検証するテストを追加（既存 controller テストの Server 構築ヘルパーに倣う。無ければ最小の httptest + test-Postgres で）:
```go
func TestSyncAppSource_SetsSyncingStatus(t *testing.T) {
	srv, pg, cleanup := newTestServer(t) // 既存ヘルパー名に合わせる
	defer cleanup()
	ctx := context.Background()
	_, err := pg.UpsertAppSource(ctx, "s1", []byte(`{}`))
	require.NoError(t, err)

	// POST /sync
	req := httptest.NewRequest("POST", "/api/v1/appsources/s1/sync", nil)
	req = withAuth(req) // 既存の認証ヘルパーに合わせる
	rr := httptest.NewRecorder()
	srv.Router().ServeHTTP(rr, req)
	require.Equal(t, http.StatusNoContent, rr.Code)

	got, err := pg.GetAppSource(ctx, "s1")
	require.NoError(t, err)
	assert.Equal(t, "Syncing", got.SyncStatus)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestSyncAppSource_SetsSyncingStatus -v`
Expected: FAIL（status が `Syncing` にならない）

- [ ] **Step 3: Add API fields**

`internal/api/types.go` の `AppSourceMeta` に:
```go
	SyncStatus     string     `json:"syncStatus,omitempty"`
	LastError      string     `json:"lastError,omitempty"`
```

- [ ] **Step 4: Map in appSourceToMeta and set Syncing in handler**

`internal/controller/api_appsources.go` の `appSourceToMeta` の返り値に:
```go
		SyncStatus:     a.SyncStatus,
		LastError:      a.LastError,
```
`handleSyncAppSource` を更新（ResetAppSourceCommit の後に Syncing 設定）:
```go
func (s *Server) handleSyncAppSource(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := s.store.ResetAppSourceCommit(r.Context(), name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.store.SetAppSourceSyncStatus(r.Context(), name, "Syncing", ""); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/controller/ -run TestSyncAppSource_SetsSyncingStatus -v`
Expected: PASS

- [ ] **Step 6: Regression + build**

Run: `go test ./internal/controller/... ./internal/api/... && go build ./...`
Expected: PASS / ビルド成功

- [ ] **Step 7: Commit**

```bash
git add internal/api/types.go internal/controller/api_appsources.go internal/controller/api_appsources_test.go
git commit -m "feat(api): expose appsource syncStatus/lastError, set Syncing on sync trigger"
```

---

### Task 3: reconciler で失敗時に Failed を記録

**Files:**
- Modify: `internal/controller/appsource_reconciler.go`（:75-76 の失敗パス）
- Test: `internal/controller/appsource_reconciler_test.go`

**Interfaces:**
- Consumes: `Store.SetAppSourceSyncStatus`（Task 1）

- [ ] **Step 1: Write the failing test**

`internal/controller/appsource_reconciler_test.go` に、fetcher がエラーを返すと `sync_status=Failed` かつ `last_error` が入ることを検証（既存テストの `mockAppSourceFetcher` と実 Postgres パターンに倣う）:
```go
func TestReconcile_RecordsFailedStatusOnError(t *testing.T) {
	pg, cleanup := newTestPostgres(t) // 既存ヘルパーに合わせる
	defer cleanup()
	ctx := context.Background()
	_, err := pg.UpsertAppSource(ctx, "my-src", []byte(appSourceSpecJSON))
	require.NoError(t, err)

	fetcher := &mockAppSourceFetcher{
		resolveErr: fmt.Errorf("auth denied"), // mock に error フィールドが無ければ追加
	}
	reconcileAppSources(ctx, pg, fetcher, nil)

	src, err := pg.GetAppSource(ctx, "my-src")
	require.NoError(t, err)
	assert.Equal(t, "Failed", src.SyncStatus)
	assert.Contains(t, src.LastError, "auth denied")
}
```
（`mockAppSourceFetcher` が固定成功しか返さない場合、`resolveErr`/`fetchErr` フィールドを足して `ResolveCommitSHA`/`FetchDir` が非nilなら返すよう最小拡張する。既存テストは error 未設定なので影響なし。）

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/controller/ -run TestReconcile_RecordsFailedStatusOnError -v`
Expected: FAIL（`SyncStatus` が `Failed` にならない）

- [ ] **Step 3: Record Failed in the reconcile loop**

`internal/controller/appsource_reconciler.go` の失敗パス（現在 WARN のみ、:75-76）を:
```go
		if err := syncAppSource(ctx, st, fetcher, km, src, spec); err != nil {
			slog.Warn("appsource reconciler: sync failed", "name", src.Name, "error", err)
			if serr := st.SetAppSourceSyncStatus(ctx, src.Name, "Failed", err.Error()); serr != nil {
				slog.Warn("appsource reconciler: failed to record sync status", "name", src.Name, "error", serr)
			}
		}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/controller/ -run TestReconcile_RecordsFailedStatusOnError -v`
Expected: PASS

- [ ] **Step 5: Regression (controller)**

Run: `go test ./internal/controller/...`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/controller/appsource_reconciler.go internal/controller/appsource_reconciler_test.go
git commit -m "feat(controller): record Failed sync_status and last_error on reconcile failure"
```

---

### Task 4: フロントエンド — 完了まで poll ＋ status バッジ ＋ エラー表示

**Files:**
- Modify: `web/src/routes/AppSourceList.svelte`

**Interfaces:**
- Consumes: API の `syncStatus` / `lastError`（Task 2）

- [ ] **Step 1: Implement poll-until-complete with timeout**

`AppSourceList.svelte` の `sync` 関数を、対象行の `syncStatus` が `Syncing` でなくなるまで poll する実装に置換（既存の `apiFetch` / `load` を利用）:
```svelte
<script>
  // ...既存 import・state 維持...
  const POLL_MS = 1500;
  const TIMEOUT_MS = 60000;

  async function sync(name) {
    syncing = { ...syncing, [name]: true };
    error = '';
    try {
      await apiFetch(`/api/v1/appsources/${name}/sync`, { method: 'POST' });
      const started = Date.now();
      // 完了(=非Syncing)まで poll。timeout で打ち切り。
      while (Date.now() - started < TIMEOUT_MS) {
        await new Promise((r) => setTimeout(r, POLL_MS));
        sources = await apiFetch('/api/v1/appsources');
        const s = sources.find((x) => x.name === name);
        if (!s || s.syncStatus !== 'Syncing') break;
      }
      const s = sources.find((x) => x.name === name);
      if (s && s.syncStatus === 'Failed') {
        error = `${name}: ${s.lastError || 'sync failed'}`;
      } else if (s && s.syncStatus === 'Syncing') {
        error = `${name}: sync がまだ完了していません（タイムアウト）。controller のログを確認してください。`;
      }
    } catch (e) {
      error = e.message;
    } finally {
      syncing = { ...syncing, [name]: false };
    }
  }
</script>
```

- [ ] **Step 2: Add a status badge column**

テーブルヘッダに `<th>Status</th>` を追加（`Last synced` の前など）、各行に status バッジを表示。`Failed` は `lastError` を `title`(tooltip) に:
```svelte
      <tr><th>Name</th><th>Repo</th><th>Ref</th><th>Path</th><th>Status</th><th>Last synced</th><th>Commit</th><th></th></tr>
```
```svelte
          <td>
            {#if s.syncStatus === 'Failed'}
              <span class="badge badge-failed" title={s.lastError}>Failed</span>
            {:else if s.syncStatus === 'Syncing' || syncing[s.name]}
              <span class="badge badge-syncing">Syncing…</span>
            {:else if s.syncStatus === 'Synced'}
              <span class="badge badge-synced">Synced</span>
            {:else}
              <span class="meta">—</span>
            {/if}
          </td>
```
`<style>` に最小のバッジスタイルを追加（既存の色トークン/クラスがあれば流用。無ければ）:
```svelte
<style>
  .badge { padding: 0.1rem 0.45rem; border-radius: 0.5rem; font-size: 0.75rem; }
  .badge-failed  { background: #fde2e1; color: #b42318; }
  .badge-syncing { background: #fef3c7; color: #92400e; }
  .badge-synced  { background: #dcfce7; color: #166534; }
</style>
```

- [ ] **Step 3: Build the frontend**

Run: `cd web && npm ci && npm run build`
Expected: ビルド成功（構文エラー無し）

- [ ] **Step 4: Manual verification (docker preview)**

controller を再ビルド/起動し WebUI を開く。Sync を押すと:
- ボタンが `...`、行バッジが `Syncing…` になり、reconcile 完了まで維持される。
- 成功で `Synced` ＋ commit/時刻更新。失敗で `Failed`（tooltip にエラー）＋上部にエラー表示。
- 認証未設定の repo で押すと `Failed` になり `lastError` が見えること（本セッションで検出した状況）。

- [ ] **Step 5: Commit**

```bash
git add web/src/routes/AppSourceList.svelte
git commit -m "feat(web): poll appsource sync to completion, show status badge and error"
```

---

## 最終確認（全タスク後）

- [ ] `go build ./...` 成功
- [ ] `go test ./internal/store/... ./internal/controller/... ./internal/api/...` パス（Docker 起動下）
- [ ] `cd web && npm run build` 成功
- [ ] 手動: Sync ボタンが実完了まで loading、失敗時にエラー表示、status バッジが正しい
- [ ] マイグレーション番号 `003` が main の最新と衝突しないか最終確認（別ブランチの 003 マージ状況次第で renumber）
