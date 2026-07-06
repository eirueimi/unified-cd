# GitOps強化（管理リソース書き込み保護 + export）実装プラン

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** AppSource管理下リソースへの直接apply/deleteを409で拒否し（`syncPolicy.allowManualOverride`で緩和可）、全リソースをAppSourceが読めるYAMLツリーへ書き出す `unified-cli export` を追加する。

**Architecture:** 保護は書き込み時に `app_sources.managed_resources`（jsonb）を `@>` で検索する共通ガード関数（controller層、fail-close）。exportはCLI側で既存リストAPIを合成し、Jobは修飾パス・他kindは専用ディレクトリに配置して往復可能なツリーを出力する。リストAPIに不足するフィールド（Schedule params / Webhook spec / AppSource syncPolicy+managedResources）はadditiveに追補する。

**Tech Stack:** Go 1.26+ / chi / pgx / cobra / gopkg.in/yaml.v3 / testify。テストDBは `store.NewTestPostgres(t)`（Docker必須 — 起動していることを確認してから始める）。

**Spec:** [docs/superpowers/specs/2026-07-06-gitops-protect-and-export-design.md](../specs/2026-07-06-gitops-protect-and-export-design.md)

## Global Constraints

- managed判定は `{kind, name}` の**完全一致**（Jobは修飾名）。leaf名でのフォールバック照合はしない。
- ガードは **fail-close**: store エラー・管理元spec parse エラー時は書き込みを拒否する。
- API型の変更はすべて **additive + `omitempty`**（後方互換）。
- リコンサイラの書き込み経路（`applyResource` → store直接）にはガードを入れない。
- exportの予約ディレクトリ名: `schedules` / `webhookreceivers` / `gitcredentials` / `appsources`。
- コミットメッセージ末尾に `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>` を付ける。
- 各タスクのテスト実行は `go test ./internal/<pkg>/ -run <TestName> -v`。最後に `make test-short` 相当（`go build ./... && go vet ./...` + 変更パッケージのテスト）で回帰確認。

---

### Task 1: store — `FindManagingAppSource`

**Files:**
- Modify: `internal/store/store.go`（interface、`// AppSources` ブロック内）
- Modify: `internal/store/postgres.go`（`GetAppSource` の直後に追加）
- Test: `internal/store/postgres_appsources_test.go`

**Interfaces:**
- Produces: `FindManagingAppSource(ctx context.Context, kind, name string) (*AppSource, error)` — 管理元AppSourceを返す。未管理なら `(nil, nil)`。Task 3-4 のガードが使用。

- [ ] **Step 1: 失敗するテストを書く**

`internal/store/postgres_appsources_test.go` に追記:

```go
func TestPostgres_FindManagingAppSource(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.UpsertAppSource(ctx, "src1", []byte(`{"repoURL":"https://x/y","targetRevision":"main","path":"jobs"}`))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	refs := []ResourceRef{{Kind: "Job", Name: "team-a/build"}, {Kind: "Schedule", Name: "nightly"}}
	if err := pg.UpdateAppSourceSyncState(ctx, "src1", "sha1", time.Now(), refs); err != nil {
		t.Fatalf("update sync state: %v", err)
	}

	// ヒット: 修飾名のJob
	got, err := pg.FindManagingAppSource(ctx, "Job", "team-a/build")
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	if got == nil || got.Name != "src1" {
		t.Fatalf("got %+v, want src1", got)
	}

	// ミス: leaf名だけでは一致しない（完全一致のみ）
	got, err = pg.FindManagingAppSource(ctx, "Job", "build")
	if err != nil || got != nil {
		t.Fatalf("leaf-only lookup: got %+v, err %v; want nil, nil", got, err)
	}

	// ミス: kind不一致
	got, err = pg.FindManagingAppSource(ctx, "Schedule", "team-a/build")
	if err != nil || got != nil {
		t.Fatalf("kind mismatch: got %+v, err %v; want nil, nil", got, err)
	}

	// ミス: どのAppSourceにも無い
	got, err = pg.FindManagingAppSource(ctx, "Job", "unknown")
	if err != nil || got != nil {
		t.Fatalf("unknown: got %+v, err %v; want nil, nil", got, err)
	}
}
```

- [ ] **Step 2: テストが失敗する（コンパイルエラー）ことを確認**

Run: `go test ./internal/store/ -run TestPostgres_FindManagingAppSource -v`
Expected: FAIL — `pg.FindManagingAppSource undefined`

- [ ] **Step 3: 実装**

`internal/store/store.go` の `// AppSources` ブロック（`SetAppSourceSyncStatus` の下）に追加:

```go
	// FindManagingAppSource returns the AppSource whose managed_resources
	// contains {kind,name}, or nil when the resource is not managed by any
	// AppSource. Exact match only (Job names are qualified).
	FindManagingAppSource(ctx context.Context, kind, name string) (*AppSource, error)
```

`internal/store/postgres.go` の `GetAppSource` 直後に追加（Scanパターンは `GetAppSource` を踏襲）:

```go
// FindManagingAppSource returns the AppSource whose managed_resources contains
// {kind,name}, or nil when the resource is not managed by any AppSource.
func (p *Postgres) FindManagingAppSource(ctx context.Context, kind, name string) (*AppSource, error) {
	ref, err := json.Marshal([]ResourceRef{{Kind: kind, Name: name}})
	if err != nil {
		return nil, err
	}
	const q = `SELECT name, spec, last_synced_at, last_commit, managed_resources, updated_at, sync_status, last_error
FROM app_sources WHERE managed_resources @> $1::jsonb LIMIT 1`
	var a AppSource
	var mr []byte
	err = p.pool.QueryRow(ctx, q, ref).Scan(&a.Name, &a.Spec, &a.LastSyncedAt, &a.LastCommit, &mr, &a.UpdatedAt, &a.SyncStatus, &a.LastError)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := unmarshalManagedResources(mr, &a); err != nil {
		return nil, err
	}
	return &a, nil
}
```

- [ ] **Step 4: テストが通ることを確認**

Run: `go test ./internal/store/ -run TestPostgres_FindManagingAppSource -v`
Expected: PASS

- [ ] **Step 5: コンパイル全体確認（他にStore実装が無いことは確認済みだが念のため）**

Run: `go build ./...`
Expected: 成功

- [ ] **Step 6: コミット**

```bash
git add internal/store/store.go internal/store/postgres.go internal/store/postgres_appsources_test.go
git commit -m "feat(store): FindManagingAppSource jsonb containment lookup"
```

---

### Task 2: dsl — `syncPolicy.allowManualOverride` + スキーマ再生成

**Files:**
- Modify: `internal/dsl/appsource_types.go:19-22`（`AppSyncPolicy`）
- Test: `internal/dsl/appsource_test.go`（無ければ作成）
- Regenerate: `schemas/unified-cd.schema.json`

**Interfaces:**
- Produces: `dsl.AppSyncPolicy.AllowManualOverride bool`（yaml: `allowManualOverride`）— Task 3 のガードが参照。

- [ ] **Step 1: 失敗するテストを書く**

`internal/dsl/appsource_test.go` に追記（ファイルが無ければ `package dsl` で作成）:

```go
func TestParseAppSource_AllowManualOverride(t *testing.T) {
	yamlDoc := `
apiVersion: unified-cd/v1
kind: AppSource
metadata:
  name: src
spec:
  repoURL: https://example.com/repo.git
  targetRevision: main
  path: jobs
  syncPolicy:
    allowManualOverride: true
`
	as, err := ParseAppSource(strings.NewReader(yamlDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !as.Spec.SyncPolicy.AllowManualOverride {
		t.Fatal("AllowManualOverride = false, want true")
	}
}
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/dsl/ -run TestParseAppSource_AllowManualOverride -v`
Expected: FAIL — `as.Spec.SyncPolicy.AllowManualOverride undefined`

- [ ] **Step 3: 実装**

`internal/dsl/appsource_types.go` の `AppSyncPolicy` を変更:

```go
type AppSyncPolicy struct {
	Interval string `yaml:"interval,omitempty"`
	Prune    bool   `yaml:"prune,omitempty"`
	// AllowManualOverride disables the managed-resource write guard for
	// resources managed by this AppSource (direct apply/delete is allowed).
	AllowManualOverride bool `yaml:"allowManualOverride,omitempty"`
}
```

- [ ] **Step 4: テストが通ることを確認 + スキーマ再生成**

Run: `go test ./internal/dsl/ -run TestParseAppSource_AllowManualOverride -v`
Expected: PASS

Run: `go generate ./internal/dsl/`
Expected: `schemas/unified-cd.schema.json` に `allowManualOverride` が追加される（`git diff schemas/` で確認）

- [ ] **Step 5: コミット**

```bash
git add internal/dsl/appsource_types.go internal/dsl/appsource_test.go schemas/unified-cd.schema.json
git commit -m "feat(dsl): syncPolicy.allowManualOverride field"
```

---

### Task 3: controller — 共通ガード + Jobハンドラ組み込み

**Files:**
- Create: `internal/controller/managed_guard.go`
- Modify: `internal/controller/api_jobs.go`（`handleApplyJob` / `handleDeleteJob`）
- Test: `internal/controller/managed_guard_test.go`

**Interfaces:**
- Consumes: Task 1 の `FindManagingAppSource`、Task 2 の `AllowManualOverride`。
- Produces: `(s *Server) guardManagedResource(ctx, kind, name string) error` と `writeGuardError(w, err)` — Task 4 が全kindのハンドラで使用。

- [ ] **Step 1: 失敗するテストを書く**

`internal/controller/managed_guard_test.go` を作成:

```go
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupManagedJob registers AppSource "src1" managing Job "hello" with the given spec JSON.
func setupManagedJob(t *testing.T, pg store.Store, srcSpec string) {
	t.Helper()
	ctx := context.Background()
	_, err := pg.UpsertAppSource(ctx, "src1", []byte(srcSpec))
	require.NoError(t, err)
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "src1", "sha", time.Now(),
		[]store.ResourceRef{{Kind: "Job", Name: "hello"}}))
}

const helloJobYAML = `
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: hello
spec:
  steps:
    - name: greet
      run: echo hi
`

func applyJob(t *testing.T, s *Server, yaml string) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(api.ApplyJobRequest{YAML: yaml})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/jobs", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

func TestAPI_ApplyJob_RejectedWhenManaged(t *testing.T) {
	s, pg := newTestServer(t)
	setupManagedJob(t, pg, `{"repoURL":"https://example.com/r.git","targetRevision":"main","path":"jobs"}`)

	rec := applyJob(t, s, helloJobYAML)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `managed by AppSource "src1"`)
	assert.Contains(t, rec.Body.String(), "allowManualOverride")
}

func TestAPI_DeleteJob_RejectedWhenManaged(t *testing.T) {
	s, pg := newTestServer(t)
	_, err := pg.UpsertJob(context.Background(), "hello", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	setupManagedJob(t, pg, `{"repoURL":"https://example.com/r.git","targetRevision":"main","path":"jobs"}`)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/jobs/hello", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	// 拒否されたので行は残っている
	_, err = pg.GetJob(context.Background(), "hello")
	assert.NoError(t, err)
}

func TestAPI_ApplyJob_AllowedWithManualOverride(t *testing.T) {
	s, pg := newTestServer(t)
	setupManagedJob(t, pg,
		`{"repoURL":"https://example.com/r.git","targetRevision":"main","path":"jobs","syncPolicy":{"allowManualOverride":true}}`)

	rec := applyJob(t, s, helloJobYAML)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestAPI_ApplyJob_AllowedWhenUnmanaged(t *testing.T) {
	s, _ := newTestServer(t)
	rec := applyJob(t, s, helloJobYAML)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
```

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/controller/ -run 'TestAPI_ApplyJob_Rejected|TestAPI_DeleteJob_Rejected|TestAPI_ApplyJob_Allowed' -v`
Expected: `Rejected` 系と `AllowedWithManualOverride` が FAIL（409にならず200が返る）。`AllowedWhenUnmanaged` は PASS。

- [ ] **Step 3: ガードを実装**

`internal/controller/managed_guard.go` を作成:

```go
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

// errManagedResource marks a write rejected because the target resource is
// managed by an AppSource. Handlers translate it to 409 Conflict.
type errManagedResource struct {
	Kind      string
	Name      string
	AppSource string
	RepoURL   string
}

func (e errManagedResource) Error() string {
	msg := fmt.Sprintf("resource %s %q is managed by AppSource %q; update it in Git", e.Kind, e.Name, e.AppSource)
	if e.RepoURL != "" {
		msg += " (" + e.RepoURL + ")"
	}
	return msg + ", or set syncPolicy.allowManualOverride: true on the AppSource"
}

// guardManagedResource rejects direct writes (apply/delete) to resources managed
// by an AppSource, so Git stays the source of truth for synced resources.
// Fail-close: store errors reject the write (500), and an unparseable manager
// spec rejects it too (409, since the management fact itself is known).
// Exceptions: the managing AppSource's syncPolicy.allowManualOverride, and an
// AppSource that manages itself (app-of-apps root must stay repairable).
func (s *Server) guardManagedResource(ctx context.Context, kind, name string) error {
	src, err := s.store.FindManagingAppSource(ctx, kind, name)
	if err != nil {
		return fmt.Errorf("check managed resource: %w", err)
	}
	if src == nil {
		return nil
	}
	if kind == "AppSource" && src.Name == name {
		return nil
	}
	var spec dsl.AppSourceSpec
	if err := json.Unmarshal(src.Spec, &spec); err != nil {
		return errManagedResource{Kind: kind, Name: name, AppSource: src.Name}
	}
	if spec.SyncPolicy.AllowManualOverride {
		return nil
	}
	return errManagedResource{Kind: kind, Name: name, AppSource: src.Name, RepoURL: spec.RepoURL}
}

// writeGuardError maps a guardManagedResource error onto the HTTP response:
// 409 for managed-resource rejections, 500 for infrastructure failures.
func writeGuardError(w http.ResponseWriter, err error) {
	var m errManagedResource
	if errors.As(err, &m) {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}
```

`internal/controller/api_jobs.go` の `handleApplyJob` — `dsl.Parse` 成功直後（`s.storeJob` 呼び出しの前）に挿入:

```go
	if err := s.guardManagedResource(r.Context(), "Job", job.Metadata.QualifiedName()); err != nil {
		writeGuardError(w, err)
		return
	}
```

同 `handleDeleteJob` — `extractJobName` の直後に挿入:

```go
	if err := s.guardManagedResource(r.Context(), "Job", name); err != nil {
		writeGuardError(w, err)
		return
	}
```

（`handleDeleteJob` は既に `name := extractJobName(chi.URLParam(r, "*"))` を宣言しているので、その直後・`s.store.DeleteJob` の前に挿入するだけでよい。）

- [ ] **Step 4: テストが通ることを確認**

Run: `go test ./internal/controller/ -run 'TestAPI_ApplyJob|TestAPI_DeleteJob' -v`
Expected: 新規4テスト + 既存のJob系テストすべて PASS

- [ ] **Step 5: コミット**

```bash
git add internal/controller/managed_guard.go internal/controller/managed_guard_test.go internal/controller/api_jobs.go
git commit -m "feat(controller): reject direct writes to AppSource-managed jobs (409)"
```

---

### Task 4: controller — 残り4 kindへのガード組み込み

**Files:**
- Modify: `internal/controller/api_schedules.go`（`handleApplySchedule` / `handleDeleteSchedule`）
- Modify: `internal/controller/api_webhooks.go`（`handleApplyWebhook` / `handleDeleteWebhook`）
- Modify: `internal/controller/api_gitcredential.go`（`handleUpsertGitCredential` / `handleDeleteGitCredential`）
- Modify: `internal/controller/api_appsources.go`（`handleApplyAppSource` / `handleDeleteAppSource`）
- Test: `internal/controller/managed_guard_test.go`（追記）

**Interfaces:**
- Consumes: Task 3 の `guardManagedResource` / `writeGuardError`。

- [ ] **Step 1: 失敗するテストを書く**

`internal/controller/managed_guard_test.go` に追記:

```go
// manageResource marks {kind,name} as managed by AppSource "owner".
func manageResource(t *testing.T, pg store.Store, kind, name string) {
	t.Helper()
	ctx := context.Background()
	_, err := pg.UpsertAppSource(ctx, "owner", []byte(`{"repoURL":"https://example.com/r.git","targetRevision":"main","path":"jobs"}`))
	require.NoError(t, err)
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "owner", "sha", time.Now(),
		[]store.ResourceRef{{Kind: kind, Name: name}}))
}

func TestAPI_DeleteManagedResources_Rejected(t *testing.T) {
	cases := []struct {
		kind, name, path string
	}{
		{"Schedule", "nightly", "/api/v1/schedules/nightly"},
		{"WebhookReceiver", "gh", "/api/v1/webhooks/gh"},
		{"GitCredential", "github", "/api/v1/gitcredentials/github"},
		{"AppSource", "child", "/api/v1/appsources/child"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			s, pg := newTestServer(t)
			manageResource(t, pg, tc.kind, tc.name)
			req := httptest.NewRequest(http.MethodDelete, tc.path, nil)
			req.Header.Set("Authorization", "Bearer secret")
			rec := httptest.NewRecorder()
			s.Router().ServeHTTP(rec, req)
			require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), `managed by AppSource "owner"`)
		})
	}
}

func TestAPI_ApplySchedule_RejectedWhenManaged(t *testing.T) {
	s, pg := newTestServer(t)
	_, err := pg.UpsertJob(context.Background(), "hello", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	manageResource(t, pg, "Schedule", "nightly")
	b, _ := json.Marshal(api.ApplyScheduleRequest{YAML: `
apiVersion: unified-cd/v1
kind: Schedule
metadata:
  name: nightly
spec:
  cron: "0 3 * * *"
  job: hello
`})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/schedules", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
}

func TestAPI_ApplyWebhook_RejectedWhenManaged(t *testing.T) {
	s, pg := newTestServer(t)
	manageResource(t, pg, "WebhookReceiver", "gh")
	b, _ := json.Marshal(api.ApplyWebhookRequest{YAML: `
apiVersion: unified-cd/v1
kind: WebhookReceiver
metadata:
  name: gh
spec:
  trigger:
    job: hello
  auth:
    type: none
`})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
}

func TestAPI_UpsertGitCredential_RejectedWhenManaged(t *testing.T) {
	s, pg := newTestServer(t)
	manageResource(t, pg, "GitCredential", "github")
	b, _ := json.Marshal(api.UpsertGitCredentialRequest{
		Name: "github", Host: "github.com", CredType: "token", SecretRef: "gh-token",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/gitcredentials/", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
}

func TestAPI_ApplyAppSource_RejectedWhenManagedByOther(t *testing.T) {
	s, pg := newTestServer(t)
	manageResource(t, pg, "AppSource", "child")
	b, _ := json.Marshal(api.ApplyAppSourceRequest{YAML: `
apiVersion: unified-cd/v1
kind: AppSource
metadata:
  name: child
spec:
  repoURL: https://example.com/child.git
  targetRevision: main
  path: jobs
`})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/appsources", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
}

// app-of-apps: 自分自身をmanaged_resourcesに含むAppSourceのapplyは許可される。
func TestAPI_ApplyAppSource_SelfManagedAllowed(t *testing.T) {
	s, pg := newTestServer(t)
	ctx := context.Background()
	_, err := pg.UpsertAppSource(ctx, "root", []byte(`{"repoURL":"https://example.com/r.git","targetRevision":"main","path":"apps"}`))
	require.NoError(t, err)
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "root", "sha", time.Now(),
		[]store.ResourceRef{{Kind: "AppSource", Name: "root"}}))
	b, _ := json.Marshal(api.ApplyAppSourceRequest{YAML: `
apiVersion: unified-cd/v1
kind: AppSource
metadata:
  name: root
spec:
  repoURL: https://example.com/r.git
  targetRevision: main
  path: apps
`})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/appsources", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
```

注意: `handleUpsertGitCredential` のルートは `r.Post("/", ...)` なのでテストのパスは `/api/v1/gitcredentials/`（末尾スラッシュ）。`api.UpsertGitCredentialRequest` のフィールド名は `internal/api/types.go` の定義（`Name`/`Host`/`CredType`/`SecretRef`）を実装前に確認し、異なる場合はテスト側を合わせる。

- [ ] **Step 2: テストが失敗することを確認**

Run: `go test ./internal/controller/ -run 'RejectedWhenManaged|DeleteManagedResources|SelfManagedAllowed' -v`
Expected: `SelfManagedAllowed` 以外 FAIL（拒否されず成功が返る）

- [ ] **Step 3: 4 kindのハンドラにガードを挿入**

`internal/controller/api_schedules.go` — `handleApplySchedule` の `dsl.ParseSchedule` 成功直後:

```go
	if err := s.guardManagedResource(r.Context(), "Schedule", sc.Metadata.Name); err != nil {
		writeGuardError(w, err)
		return
	}
```

`handleDeleteSchedule` の `name := chi.URLParam(r, "name")` 直後:

```go
	if err := s.guardManagedResource(r.Context(), "Schedule", name); err != nil {
		writeGuardError(w, err)
		return
	}
```

`internal/controller/api_webhooks.go` — `handleApplyWebhook` の `dsl.ParseWebhookReceiver` 成功直後に kind `"WebhookReceiver"` / `wr.Metadata.Name` で同型のガード、`handleDeleteWebhook` の `name :=` 直後に kind `"WebhookReceiver"` / `name` で同型のガード:

```go
	if err := s.guardManagedResource(r.Context(), "WebhookReceiver", wr.Metadata.Name); err != nil {
		writeGuardError(w, err)
		return
	}
```

```go
	if err := s.guardManagedResource(r.Context(), "WebhookReceiver", name); err != nil {
		writeGuardError(w, err)
		return
	}
```

`internal/controller/api_gitcredential.go` — `handleUpsertGitCredential` のバリデーション（credTypeチェック）直後・store書き込み前:

```go
	if err := s.guardManagedResource(r.Context(), "GitCredential", req.Name); err != nil {
		writeGuardError(w, err)
		return
	}
```

`handleDeleteGitCredential` の `name :=` 直後:

```go
	if err := s.guardManagedResource(r.Context(), "GitCredential", name); err != nil {
		writeGuardError(w, err)
		return
	}
```

`internal/controller/api_appsources.go` — `handleApplyAppSource` の `dsl.ParseAppSource` 成功直後:

```go
	if err := s.guardManagedResource(r.Context(), "AppSource", as.Metadata.Name); err != nil {
		writeGuardError(w, err)
		return
	}
```

`handleDeleteAppSource` の `name :=` 直後:

```go
	if err := s.guardManagedResource(r.Context(), "AppSource", name); err != nil {
		writeGuardError(w, err)
		return
	}
```

- [ ] **Step 4: テストが通ることを確認**

Run: `go test ./internal/controller/ -v`
Expected: 全PASS（既存のAppSource同期・webhook・schedule系テストの回帰も無いこと）

- [ ] **Step 5: コミット**

```bash
git add internal/controller/api_schedules.go internal/controller/api_webhooks.go internal/controller/api_gitcredential.go internal/controller/api_appsources.go internal/controller/managed_guard_test.go
git commit -m "feat(controller): managed-resource write guard for all resource kinds"
```

---

### Task 5: docs — 保護機能のドキュメント

**Files:**
- Modify: `docs/resources.md`（AppSource節の末尾に追記）

**Interfaces:** なし（ドキュメントのみ）

- [ ] **Step 1: resources.md のAppSource節に以下を追記**

（AppSource節の見出し構成を実際に開いて確認し、`syncPolicy` を説明している箇所の近くに置く）

```markdown
### Managed-resource protection

Resources synced by an AppSource (listed in its managed resources) are
protected from direct modification: `unified-cli apply` and REST API
writes/deletes targeting them are rejected with **409 Conflict**, keeping Git
the source of truth. The error names the managing AppSource and its repoURL.

To edit such a resource, change it in the Git repository and let the AppSource
sync it. To intentionally allow manual overrides (e.g. during an incident),
set on the AppSource:

```yaml
spec:
  syncPolicy:
    allowManualOverride: true
```

Notes:

- Matching is exact on `{kind, qualified name}`.
- An AppSource that manages **itself** (app-of-apps root) can always be
  re-applied directly, so a broken Git state stays repairable.
- The guard fails closed: if the controller cannot check the management state
  (DB error), the write is rejected.
```

- [ ] **Step 2: コミット**

```bash
git add docs/resources.md
git commit -m "docs: document AppSource managed-resource protection"
```

---

### Task 6: api — リストAPIへのadditiveフィールド追補

**Files:**
- Modify: `internal/api/types.go`（`ScheduleMeta` / `WebhookReceiverMeta` / `AppSourceMeta` + 新型2つ）
- Modify: `internal/controller/api_schedules.go`（apply/list両方のレスポンス組み立て）
- Modify: `internal/controller/api_webhooks.go`（同上）
- Modify: `internal/controller/api_appsources.go`（`appSourceToMeta`）
- Test: `internal/controller/api_schedules_test.go` / `internal/controller/api_webhooks_test.go` / `internal/controller/appsource_reconciler_test.go` と同居でも可、新規は `internal/controller/api_export_fields_test.go`

**Interfaces:**
- Produces（Task 7のCLIが消費）:
  - `api.ScheduleMeta.Params map[string]string`（json: `params,omitempty`）
  - `api.WebhookReceiverMeta.Spec []byte`（json: `spec,omitempty`）
  - `api.AppSourceMeta.SyncPolicy *api.AppSourceSyncPolicy`（json: `syncPolicy,omitempty`）
  - `api.AppSourceMeta.ManagedResources []api.ResourceRef`（json: `managedResources,omitempty`）
  - `api.ResourceRef{Kind, Name string}` / `api.AppSourceSyncPolicy{Interval string, Prune bool, AllowManualOverride bool}`

- [ ] **Step 1: 失敗するテストを書く**

`internal/controller/api_export_fields_test.go` を作成:

```go
package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func getList(t *testing.T, s *Server, path string, v any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), v))
}

func TestAPI_ListSchedules_IncludesParams(t *testing.T) {
	s, pg := newTestServer(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "hello", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	_, err = pg.UpsertSchedule(ctx, "nightly", "0 3 * * *", "hello", map[string]string{"env": "prod"})
	require.NoError(t, err)

	var got []api.ScheduleMeta
	getList(t, s, "/api/v1/schedules", &got)
	require.Len(t, got, 1)
	assert.Equal(t, map[string]string{"env": "prod"}, got[0].Params)
}

func TestAPI_ListWebhooks_IncludesSpec(t *testing.T) {
	s, pg := newTestServer(t)
	spec := []byte(`{"trigger":{"job":"hello"},"auth":{"type":"none"}}`)
	_, err := pg.UpsertWebhookReceiver(context.Background(), "gh", spec)
	require.NoError(t, err)

	var got []api.WebhookReceiverMeta
	getList(t, s, "/api/v1/webhooks", &got)
	require.Len(t, got, 1)
	assert.JSONEq(t, string(spec), string(got[0].Spec))
}

func TestAPI_ListAppSources_IncludesSyncPolicyAndManaged(t *testing.T) {
	s, pg := newTestServer(t)
	ctx := context.Background()
	_, err := pg.UpsertAppSource(ctx, "src1",
		[]byte(`{"repoURL":"https://x/y.git","targetRevision":"main","path":"jobs","syncPolicy":{"prune":true,"allowManualOverride":true}}`))
	require.NoError(t, err)
	require.NoError(t, pg.UpdateAppSourceSyncState(ctx, "src1", "sha", time.Now(),
		[]store.ResourceRef{{Kind: "Job", Name: "team-a/build"}}))

	var got []api.AppSourceMeta
	getList(t, s, "/api/v1/appsources", &got)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].SyncPolicy)
	assert.True(t, got[0].SyncPolicy.Prune)
	assert.True(t, got[0].SyncPolicy.AllowManualOverride)
	require.Len(t, got[0].ManagedResources, 1)
	assert.Equal(t, api.ResourceRef{Kind: "Job", Name: "team-a/build"}, got[0].ManagedResources[0])
}
```

- [ ] **Step 2: テストが失敗する（コンパイルエラー）ことを確認**

Run: `go test ./internal/controller/ -run 'IncludesParams|IncludesSpec|IncludesSyncPolicy' -v`
Expected: FAIL — `got[0].Params` / `got[0].Spec` / `api.ResourceRef` undefined

- [ ] **Step 3: 型とハンドラを実装**

`internal/api/types.go`:

`ScheduleMeta` に追加:

```go
	Params      map[string]string `json:"params,omitempty"`
```

`WebhookReceiverMeta` に追加:

```go
	Spec      []byte    `json:"spec,omitempty"`
```

`AppSourceMeta` の直前に新型を追加し、`AppSourceMeta` にフィールドを足す:

```go
// ResourceRef identifies a resource managed by an AppSource
// (API mirror of store.ResourceRef).
type ResourceRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// AppSourceSyncPolicy is the API mirror of dsl.AppSyncPolicy.
type AppSourceSyncPolicy struct {
	Interval            string `json:"interval,omitempty"`
	Prune               bool   `json:"prune,omitempty"`
	AllowManualOverride bool   `json:"allowManualOverride,omitempty"`
}
```

`AppSourceMeta` に追加:

```go
	SyncPolicy       *AppSourceSyncPolicy `json:"syncPolicy,omitempty"`
	ManagedResources []ResourceRef        `json:"managedResources,omitempty"`
```

`internal/controller/api_schedules.go` — `handleApplySchedule` と `handleListSchedules` の `api.ScheduleMeta{...}` 組み立て両方に `Params: stored.Params,`（listでは `Params: sc.Params,`）を追加。

`internal/controller/api_webhooks.go` — `handleApplyWebhook` の組み立てに `Spec: stored.Spec,`、`handleListWebhooks` の組み立てに `Spec: wr.Spec,` を追加。

`internal/controller/api_appsources.go` — `appSourceToMeta` を変更:

```go
// appSourceToMeta converts a store.AppSource and dsl.AppSourceSpec into an api.AppSourceMeta.
func appSourceToMeta(a *store.AppSource, spec dsl.AppSourceSpec) api.AppSourceMeta {
	m := api.AppSourceMeta{
		Name:           a.Name,
		RepoURL:        spec.RepoURL,
		TargetRevision: spec.TargetRevision,
		Path:           spec.Path,
		LastSyncedAt:   a.LastSyncedAt,
		LastCommit:     a.LastCommit,
		SyncStatus:     a.SyncStatus,
		LastError:      a.LastError,
		UpdatedAt:      a.UpdatedAt,
	}
	if spec.SyncPolicy != (dsl.AppSyncPolicy{}) {
		m.SyncPolicy = &api.AppSourceSyncPolicy{
			Interval:            spec.SyncPolicy.Interval,
			Prune:               spec.SyncPolicy.Prune,
			AllowManualOverride: spec.SyncPolicy.AllowManualOverride,
		}
	}
	for _, ref := range a.ManagedResources {
		m.ManagedResources = append(m.ManagedResources, api.ResourceRef{Kind: ref.Kind, Name: ref.Name})
	}
	return m
}
```

- [ ] **Step 4: テストが通ることを確認**

Run: `go test ./internal/controller/ -run 'IncludesParams|IncludesSpec|IncludesSyncPolicy' -v`
Expected: PASS

Run: `go build ./... && go test ./internal/controller/ ./internal/cli/`
Expected: 全PASS（additive変更なので既存テストは壊れない）

- [ ] **Step 5: コミット**

```bash
git add internal/api/types.go internal/controller/api_schedules.go internal/controller/api_webhooks.go internal/controller/api_appsources.go internal/controller/api_export_fields_test.go
git commit -m "feat(api): expose schedule params, webhook spec, appsource syncPolicy/managedResources"
```

---

### Task 7: cli — `unified-cli export`

**Files:**
- Create: `internal/cli/export.go`
- Modify: `internal/cli/root.go:42-57`（`newExportCmd` 登録）
- Test: `internal/cli/export_test.go`

**Interfaces:**
- Consumes: Task 6 のAPIフィールド。CLIテストは `internal/cli/apply_test.go:18` の `captureTransport`（`responseFor func(path string) (int, []byte)`）を再利用。
- Produces: `unified-cli export -o <dir> [--unmanaged-only] [--force]`

- [ ] **Step 1: 失敗するテストを書く**

`internal/cli/export_test.go` を作成:

```go
package cli

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/spf13/cobra"
)

// exportFixtures returns a captureTransport serving a small consistent dataset.
// managed controls whether AppSource src1 reports Job team-a/build as managed.
func exportFixtures(managed bool) *captureTransport {
	jobSpec, _ := json.Marshal(map[string]any{"steps": []map[string]any{{"name": "greet", "run": "echo hi"}}})
	whSpec, _ := json.Marshal(map[string]any{"trigger": map[string]any{"job": "hello"}, "auth": map[string]any{"type": "none"}})
	return &captureTransport{
		responseFor: func(path string) (int, []byte) {
			switch path {
			case "/api/v1/appsources":
				m := api.AppSourceMeta{Name: "src1", RepoURL: "https://x/y.git", TargetRevision: "main", Path: "jobs"}
				if managed {
					m.ManagedResources = []api.ResourceRef{{Kind: "Job", Name: "team-a/build"}}
				}
				b, _ := json.Marshal([]api.AppSourceMeta{m})
				return http.StatusOK, b
			case "/api/v1/jobs":
				b, _ := json.Marshal([]api.Job{
					{Name: "team-a/build", Path: "team-a", Leaf: "build", APIVersion: "unified-cd/v1", Spec: jobSpec},
					{Name: "hello", Path: "", Leaf: "hello", APIVersion: "unified-cd/v1", Spec: jobSpec},
				})
				return http.StatusOK, b
			case "/api/v1/schedules":
				b, _ := json.Marshal([]api.ScheduleMeta{{Name: "nightly", Cron: "0 3 * * *", JobName: "hello", Params: map[string]string{"env": "prod"}}})
				return http.StatusOK, b
			case "/api/v1/webhooks":
				b, _ := json.Marshal([]api.WebhookReceiverMeta{{Name: "gh", Spec: whSpec}})
				return http.StatusOK, b
			case "/api/v1/gitcredentials":
				b, _ := json.Marshal([]api.GitCredentialMeta{{Name: "github", Host: "github.com", CredType: "token", SecretRef: "gh-token"}})
				return http.StatusOK, b
			}
			return http.StatusNotFound, []byte("not found")
		},
	}
}

func newTestExportCmd(tr *captureTransport) (*cobra.Command, *strings.Builder) {
	cfg := Config{Server: "http://fake", Token: "tok"}
	cmd := newExportCmdWithClient(func() (Config, error) { return cfg, nil }, &http.Client{Transport: tr})
	var out strings.Builder
	cmd.SetOut(&out)
	return cmd, &out
}

func TestExport_WritesAllKinds(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	cmd, out := newTestExportCmd(exportFixtures(false))
	cmd.SetArgs([]string{"-o", dir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	for _, f := range []string{
		"team-a/build.yaml", "hello.yaml",
		"schedules/nightly.yaml", "webhookreceivers/gh.yaml",
		"gitcredentials/github.yaml", "appsources/src1.yaml",
	} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(f))); err != nil {
			t.Errorf("expected file %s: %v", f, err)
		}
	}
	b, _ := os.ReadFile(filepath.Join(dir, "team-a", "build.yaml"))
	s := string(b)
	for _, want := range []string{"apiVersion: unified-cd/v1", "kind: Job", "name: build", "run: echo hi"} {
		if !strings.Contains(s, want) {
			t.Errorf("job yaml missing %q:\n%s", want, s)
		}
	}
	sched, _ := os.ReadFile(filepath.Join(dir, "schedules", "nightly.yaml"))
	for _, want := range []string{"kind: Schedule", "cron:", "job: hello", "env: prod"} {
		if !strings.Contains(string(sched), want) {
			t.Errorf("schedule yaml missing %q:\n%s", want, string(sched))
		}
	}
	gc, _ := os.ReadFile(filepath.Join(dir, "gitcredentials", "github.yaml"))
	for _, want := range []string{"kind: GitCredential", "host: github.com", "type: token", "secretRef: gh-token"} {
		if !strings.Contains(string(gc), want) {
			t.Errorf("gitcredential yaml missing %q:\n%s", want, string(gc))
		}
	}
	if !strings.Contains(out.String(), "exported 6 resources (0 skipped as managed); secrets are not exported") {
		t.Errorf("unexpected summary: %s", out.String())
	}
}

func TestExport_UnmanagedOnlySkipsManaged(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	cmd, out := newTestExportCmd(exportFixtures(true))
	cmd.SetArgs([]string{"-o", dir, "--unmanaged-only"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "team-a", "build.yaml")); !os.IsNotExist(err) {
		t.Errorf("managed job must be skipped, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "hello.yaml")); err != nil {
		t.Errorf("unmanaged job must be exported: %v", err)
	}
	if !strings.Contains(out.String(), "exported 5 resources (1 skipped as managed)") {
		t.Errorf("unexpected summary: %s", out.String())
	}
}

func TestExport_RefusesNonEmptyDirWithoutForce(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "existing.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd, _ := newTestExportCmd(exportFixtures(false))
	cmd.SetArgs([]string{"-o", dir})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("expected not-empty error, got %v", err)
	}

	// --force なら書ける
	cmd2, _ := newTestExportCmd(exportFixtures(false))
	cmd2.SetArgs([]string{"-o", dir, "--force"})
	if err := cmd2.Execute(); err != nil {
		t.Fatalf("with --force: %v", err)
	}
}

func TestExport_RejectsReservedDirCollision(t *testing.T) {
	jobSpec, _ := json.Marshal(map[string]any{"steps": []map[string]any{{"name": "s", "run": "echo"}}})
	tr := &captureTransport{
		responseFor: func(path string) (int, []byte) {
			switch path {
			case "/api/v1/appsources":
				return http.StatusOK, []byte(`[]`)
			case "/api/v1/jobs":
				b, _ := json.Marshal([]api.Job{{Name: "schedules/evil", Path: "schedules", Leaf: "evil", APIVersion: "unified-cd/v1", Spec: jobSpec}})
				return http.StatusOK, b
			}
			return http.StatusOK, []byte(`[]`)
		},
	}
	cmd, _ := newTestExportCmd(tr)
	cmd.SetArgs([]string{"-o", filepath.Join(t.TempDir(), "out")})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("expected reserved-dir error, got %v", err)
	}
}
```

- [ ] **Step 2: テストが失敗する（コンパイルエラー）ことを確認**

Run: `go test ./internal/cli/ -run TestExport -v`
Expected: FAIL — `newExportCmdWithClient undefined`

- [ ] **Step 3: 実装**

`internal/cli/export.go` を作成:

```go
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// reservedExportDirs are the top-level directories export uses for non-Job
// kinds. A Job whose qualified path starts with one of these would change
// meaning on re-import through an AppSource, so export fails instead.
var reservedExportDirs = map[string]bool{
	"schedules": true, "webhookreceivers": true, "gitcredentials": true, "appsources": true,
}

func newExportCmd(resolve func() (Config, error)) *cobra.Command {
	return newExportCmdWithClient(resolve, http.DefaultClient)
}

func newExportCmdWithClient(resolve func() (Config, error), httpClient *http.Client) *cobra.Command {
	var outDir string
	var unmanagedOnly, force bool
	cmd := &cobra.Command{
		Use:   "export",
		Short: "export all resources as a YAML tree consumable by an AppSource",
		Long: `Export Jobs, Schedules, WebhookReceivers, GitCredentials and AppSources as
one YAML file per resource. Jobs are placed at their qualified path so the
output directory can be committed to Git and used directly as an AppSource
path. Secret values cannot be exported.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := resolve()
			if err != nil {
				return err
			}
			return runExport(cmd.OutOrStdout(), cfg, httpClient, outDir, unmanagedOnly, force)
		},
	}
	cmd.Flags().StringVarP(&outDir, "output", "o", "", "output directory (required)")
	cmd.Flags().BoolVar(&unmanagedOnly, "unmanaged-only", false, "export only resources not managed by any AppSource")
	cmd.Flags().BoolVar(&force, "force", false, "write into a non-empty directory")
	_ = cmd.MarkFlagRequired("output")
	return cmd
}

// exportDoc is one exported resource document. Field order is fixed by the
// struct so every file starts with apiVersion/kind/metadata/spec.
type exportDoc struct {
	APIVersion string         `yaml:"apiVersion"`
	Kind       string         `yaml:"kind"`
	Metadata   exportMetadata `yaml:"metadata"`
	Spec       map[string]any `yaml:"spec"`
}

type exportMetadata struct {
	Name string `yaml:"name"`
}

func runExport(out io.Writer, cfg Config, httpClient *http.Client, outDir string, unmanagedOnly, force bool) error {
	if err := ensureExportDir(outDir, force); err != nil {
		return err
	}

	var appsources []api.AppSourceMeta
	if err := getJSON(cfg, httpClient, "/api/v1/appsources", &appsources); err != nil {
		return fmt.Errorf("list appsources: %w", err)
	}
	managed := map[string]bool{}
	for _, a := range appsources {
		for _, ref := range a.ManagedResources {
			managed[ref.Kind+"\x00"+ref.Name] = true
		}
	}
	skip := func(kind, name string) bool {
		return unmanagedOnly && managed[kind+"\x00"+name]
	}
	exported, skipped := 0, 0

	// Jobs: placed at their qualified path so an AppSource pointing at outDir
	// reproduces the same qualified names (metadata.name is the leaf).
	var jobs []api.Job
	if err := getJSON(cfg, httpClient, "/api/v1/jobs", &jobs); err != nil {
		return fmt.Errorf("list jobs: %w", err)
	}
	for _, j := range jobs {
		if skip("Job", j.Name) {
			skipped++
			continue
		}
		if first := strings.SplitN(j.Path, "/", 2)[0]; j.Path != "" && reservedExportDirs[first] {
			return fmt.Errorf("job %q: path segment %q collides with a reserved export directory (%s); rename the job path", j.Name, first, first)
		}
		spec, err := specToMap(j.Spec)
		if err != nil {
			return fmt.Errorf("job %q: parse spec: %w", j.Name, err)
		}
		apiVersion := j.APIVersion
		if apiVersion == "" {
			apiVersion = "unified-cd/v1"
		}
		doc := exportDoc{APIVersion: apiVersion, Kind: "Job", Metadata: exportMetadata{Name: j.Leaf}, Spec: spec}
		if err := writeExportDoc(filepath.Join(outDir, filepath.FromSlash(j.Path), j.Leaf+".yaml"), doc); err != nil {
			return err
		}
		exported++
	}

	var schedules []api.ScheduleMeta
	if err := getJSON(cfg, httpClient, "/api/v1/schedules", &schedules); err != nil {
		return fmt.Errorf("list schedules: %w", err)
	}
	for _, sc := range schedules {
		if skip("Schedule", sc.Name) {
			skipped++
			continue
		}
		spec := map[string]any{"cron": sc.Cron, "job": sc.JobName}
		if len(sc.Params) > 0 {
			spec["params"] = sc.Params
		}
		doc := exportDoc{APIVersion: "unified-cd/v1", Kind: "Schedule", Metadata: exportMetadata{Name: sc.Name}, Spec: spec}
		if err := writeExportDoc(filepath.Join(outDir, "schedules", sc.Name+".yaml"), doc); err != nil {
			return err
		}
		exported++
	}

	var webhooks []api.WebhookReceiverMeta
	if err := getJSON(cfg, httpClient, "/api/v1/webhooks", &webhooks); err != nil {
		return fmt.Errorf("list webhooks: %w", err)
	}
	for _, wr := range webhooks {
		if skip("WebhookReceiver", wr.Name) {
			skipped++
			continue
		}
		spec, err := specToMap(wr.Spec)
		if err != nil {
			return fmt.Errorf("webhookreceiver %q: parse spec: %w", wr.Name, err)
		}
		doc := exportDoc{APIVersion: "unified-cd/v1", Kind: "WebhookReceiver", Metadata: exportMetadata{Name: wr.Name}, Spec: spec}
		if err := writeExportDoc(filepath.Join(outDir, "webhookreceivers", wr.Name+".yaml"), doc); err != nil {
			return err
		}
		exported++
	}

	var creds []api.GitCredentialMeta
	if err := getJSON(cfg, httpClient, "/api/v1/gitcredentials", &creds); err != nil {
		return fmt.Errorf("list gitcredentials: %w", err)
	}
	for _, gc := range creds {
		if skip("GitCredential", gc.Name) {
			skipped++
			continue
		}
		spec := map[string]any{"host": gc.Host, "type": gc.CredType, "secretRef": gc.SecretRef}
		doc := exportDoc{APIVersion: "unified-cd/v1", Kind: "GitCredential", Metadata: exportMetadata{Name: gc.Name}, Spec: spec}
		if err := writeExportDoc(filepath.Join(outDir, "gitcredentials", gc.Name+".yaml"), doc); err != nil {
			return err
		}
		exported++
	}

	for _, a := range appsources {
		if skip("AppSource", a.Name) {
			skipped++
			continue
		}
		spec := map[string]any{"repoURL": a.RepoURL, "targetRevision": a.TargetRevision, "path": a.Path}
		if a.SyncPolicy != nil {
			sp := map[string]any{}
			if a.SyncPolicy.Interval != "" {
				sp["interval"] = a.SyncPolicy.Interval
			}
			if a.SyncPolicy.Prune {
				sp["prune"] = true
			}
			if a.SyncPolicy.AllowManualOverride {
				sp["allowManualOverride"] = true
			}
			if len(sp) > 0 {
				spec["syncPolicy"] = sp
			}
		}
		doc := exportDoc{APIVersion: "unified-cd/v1", Kind: "AppSource", Metadata: exportMetadata{Name: a.Name}, Spec: spec}
		if err := writeExportDoc(filepath.Join(outDir, "appsources", a.Name+".yaml"), doc); err != nil {
			return err
		}
		exported++
	}

	fmt.Fprintf(out, "exported %d resources (%d skipped as managed); secrets are not exported\n", exported, skipped)
	return nil
}

// ensureExportDir creates the output directory, refusing a non-empty one
// unless force is set.
func ensureExportDir(dir string, force bool) error {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return os.MkdirAll(dir, 0o755)
	}
	if err != nil {
		return err
	}
	if len(entries) > 0 && !force {
		return fmt.Errorf("output directory %s is not empty (use --force to overwrite)", dir)
	}
	return nil
}

// specToMap converts stored spec JSON into a map for YAML rendering.
func specToMap(spec []byte) (map[string]any, error) {
	m := map[string]any{}
	if len(spec) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(spec, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func writeExportDoc(path string, doc exportDoc) error {
	b, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

// getJSON performs an authenticated GET against the controller and decodes the
// JSON response into v.
func getJSON(cfg Config, httpClient *http.Client, path string, v any) error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, cfg.Server+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server: %s", string(b))
	}
	return json.Unmarshal(b, v)
}
```

注意: `getJSON` という名前が `internal/cli` 内で既に使われていないか `grep -n "func getJSON" internal/cli/` で確認し、衝突する場合は `exportGetJSON` に改名する（テストは触らない）。

`internal/cli/root.go` の `root.AddCommand(newApplyCmd(resolve))` の直後に追加:

```go
	root.AddCommand(newExportCmd(resolve))
```

- [ ] **Step 4: テストが通ることを確認**

Run: `go test ./internal/cli/ -run TestExport -v`
Expected: 4テストすべて PASS

Run: `go test ./internal/cli/`
Expected: 既存テスト含め全PASS

- [ ] **Step 5: コミット**

```bash
git add internal/cli/export.go internal/cli/export_test.go internal/cli/root.go
git commit -m "feat(cli): unified-cli export writes an AppSource-consumable YAML tree"
```

---

### Task 8: 往復（round-trip）検証テスト + ドキュメント

**Files:**
- Test: `internal/cli/export_roundtrip_test.go`
- Modify: `docs/cli.md`（exportコマンド節を追加）
- Modify: `docs/resources.md`（AppSource節にGit移行手順を追加）

**Interfaces:**
- Consumes: Task 7 の export 出力、`dsl.Parse` / `dsl.QualifyName`（`internal/dsl/qualname.go:8`）。

- [ ] **Step 1: 往復テストを書く（このテストはTask 7実装済みならそのまま通るはず — 検証が目的）**

`internal/cli/export_roundtrip_test.go` を作成:

```go
package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

// TestExport_RoundTripQualifiedNames verifies that parsing exported files with
// the same rules the AppSource reconciler uses (dir-based qualification)
// reproduces the original qualified job names.
func TestExport_RoundTripQualifiedNames(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	cmd, _ := newTestExportCmd(exportFixtures(false))
	cmd.SetArgs([]string{"-o", dir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	wantJobs := map[string]bool{"team-a/build": false, "hello": false}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(b), "kind: Job") {
			return nil
		}
		job, err := dsl.Parse(bytes.NewReader(b))
		if err != nil {
			t.Errorf("%s: exported job must re-parse: %v", path, err)
			return nil
		}
		rel, _ := filepath.Rel(dir, filepath.Dir(path))
		reldir := filepath.ToSlash(rel)
		if reldir == "." {
			reldir = ""
		}
		qualified := dsl.QualifyName(reldir, job.Metadata.Name)
		if _, ok := wantJobs[qualified]; !ok {
			t.Errorf("unexpected qualified name %q from %s", qualified, path)
			return nil
		}
		wantJobs[qualified] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	for name, seen := range wantJobs {
		if !seen {
			t.Errorf("job %q not reproduced by round-trip", name)
		}
	}
}
```

注意: `dsl.Parse` のシグネチャは `dsl.Parse(io.Reader)`（`internal/controller/api_jobs.go:29` の呼び出しと同じ）。`bytes.NewReader` でよい。

- [ ] **Step 2: テストを実行**

Run: `go test ./internal/cli/ -run TestExport_RoundTrip -v`
Expected: PASS（失敗する場合はexportのレイアウトかmetadata.nameの組み立てにバグがある — 修正してから進む）

- [ ] **Step 3: docs/cli.md にexport節を追加**

既存のコマンド説明の並び（apply等）に合わせて追加:

```markdown
## export

Export all resources (Jobs, Schedules, WebhookReceivers, GitCredentials,
AppSources) as one YAML file per resource:

```bash
unified-cli export -o ./exported/
```

- Jobs are written at their qualified path (`team-a/build` → `team-a/build.yaml`)
  so the output directory can be committed to Git and pointed at by an
  AppSource `path` directly — re-importing reproduces the same names.
- Non-Job kinds go under `schedules/`, `webhookreceivers/`,
  `gitcredentials/`, `appsources/`.
- `--unmanaged-only` exports only resources not already managed by an
  AppSource (useful for migrating manually-applied resources to Git).
- `--force` allows writing into a non-empty directory.
- Secret **values** are never exported (they are not retrievable via the API);
  re-create them with `unified-cli secret set` after a restore.
- Output is regenerated from the stored spec: comments and key order of the
  originally applied YAML are not preserved.
```

- [ ] **Step 4: docs/resources.md のAppSource節にGit移行手順を追加**

Task 5 で追記した「Managed-resource protection」の後に:

```markdown
### Migrating manually-applied resources to Git

1. `unified-cli export -o ./exported --unmanaged-only`
2. Commit the directory to a Git repository.
3. Apply an AppSource whose `path` points at the exported directory.
4. On the first sync each resource is upserted under its existing name and
   recorded as managed — no manual deletion is needed, and from then on the
   resources are protected from direct writes.
```

- [ ] **Step 5: 最終回帰 + コミット**

Run: `go build ./... && go vet ./... && go test ./internal/store/ ./internal/dsl/ ./internal/controller/ ./internal/cli/`
Expected: 全PASS

```bash
git add internal/cli/export_roundtrip_test.go docs/cli.md docs/resources.md
git commit -m "test(cli): export round-trip verification; docs for export and Git migration"
```
