# Run Retention (TTL) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Age-based auto-deletion of terminal runs — DB rows *and* their object-store data (log archives, artifacts) — behind a `--run-retention-days` flag (default 0 = disabled), plus fixing the manual `DELETE /runs/{id}` object leak.

**Architecture:** A leader-elected hourly sweeper (`internal/controller/run_retention.go`, modeled on `audit_retention.go`) fetches expired run IDs in batches and deletes each via a shared `deleteRunEverywhere` helper. The helper deletes object-store objects first and the `runs` row last, so a surviving DB row always still references any surviving objects (no orphans, retries are idempotent). The manual DELETE API handler switches to the same helper.

**Tech Stack:** Go, pgx (via `internal/store`), PostgreSQL advisory locks, `internal/objectstore` (S3/local), testify, `store.NewTestPostgres` (dockerized Postgres).

**Spec:** `docs/superpowers/specs/2026-07-14-run-retention-design.md`

## Global Constraints

- All code, comments, commit messages, and docs in **English** (AGENTS.md).
- Work happens in the existing worktree `../unified-cd-run-retention`, branch `run-retention`. Never commit from the main tree.
- No PII in any file; use placeholder paths/usernames in docs.
- Store integration tests require Docker (`store.NewTestPostgres`); they skip under `-short`.
- New advisory lock key must be unique: existing keys are scheduler `0x65786364`, approval `0x61707276`, cache `0x63616368`, logArchiver `0x6C6F6761`, appSource `0x61707073`, stuckRun `0x7374756B`, auditRetention `0x61756474`. This feature uses `0x7272746E` ('rrtn').
- Docs hygiene (AGENTS.md): after behavior/flag changes, update `docs/configuration.md`, `docs/operations.md`, `docs/high-availability.md`; `docs/field-reference.md` is DSL-generated and is NOT touched (no DSL change here).

---

### Task 1: Store method `ListExpiredRuns`

**Files:**
- Modify: `internal/store/store.go` (interface, next to the "Log Archives" block around line 230)
- Modify: `internal/store/postgres.go` (implementation, next to `ListRunsNeedingArchival` around line 1285)
- Test: `internal/store/postgres_run_retention_test.go` (create)

**Interfaces:**
- Consumes: existing `runs` table; `NewTestPostgres`, `UpsertJob`, `CreateRun`, `MarkRunFinished` test helpers.
- Produces: `ListExpiredRuns(ctx context.Context, cutoff time.Time, limit int) ([]string, error)` on `store.Store` — returns IDs of terminal runs with `updated_at < cutoff`, **oldest first**, at most `limit`. Task 3's sweeper and its fake store implement/consume exactly this signature.

- [ ] **Step 1: Write the failing integration test**

Create `internal/store/postgres_run_retention_test.go`:

```go
package store

import (
	"context"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPostgres_ListExpiredRuns verifies selection (terminal + older than
// cutoff), exclusion (recent rows, non-terminal rows), oldest-first order,
// and LIMIT.
func TestPostgres_ListExpiredRuns(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	// mkRun creates a run, optionally finishes it, and backdates updated_at.
	// A terminal run's updated_at never changes again, so it is the finish time.
	mkRun := func(status api.RunStatus, age string) string {
		t.Helper()
		run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
		require.NoError(t, err)
		if status != "" {
			require.NoError(t, pg.MarkRunFinished(ctx, run.ID, status))
		}
		if age != "" {
			_, err = pg.pool.Exec(ctx,
				`UPDATE runs SET updated_at = NOW() - $1::interval WHERE id = $2`, age, run.ID)
			require.NoError(t, err)
		}
		return run.ID
	}

	oldest := mkRun(api.RunSucceeded, "40 days")
	older := mkRun(api.RunFailed, "35 days")
	oldCancelled := mkRun(api.RunCancelled, "31 days")
	_ = mkRun(api.RunSucceeded, "10 days") // recent: kept
	_ = mkRun("", "40 days")               // old but still Pending: kept

	cutoff := time.Now().AddDate(0, 0, -30)

	ids, err := pg.ListExpiredRuns(ctx, cutoff, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{oldest, older, oldCancelled}, ids, "terminal+old only, oldest first")

	// LIMIT is respected and keeps the oldest-first prefix.
	ids, err = pg.ListExpiredRuns(ctx, cutoff, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{oldest, older}, ids)
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd /path/to/unified-cd-run-retention && go test ./internal/store/ -run TestPostgres_ListExpiredRuns -v`
Expected: compile error `pg.ListExpiredRuns undefined`.

- [ ] **Step 3: Add the interface method and implementation**

In `internal/store/store.go`, inside the `Store` interface directly under the `GetLogArchive` line in the `// Log Archives` block:

```go
	// Run retention
	// ListExpiredRuns returns IDs of terminal (Succeeded/Failed/Cancelled)
	// runs whose updated_at is older than cutoff, oldest first, up to limit.
	ListExpiredRuns(ctx context.Context, cutoff time.Time, limit int) ([]string, error)
```

In `internal/store/postgres.go`, directly after `ListRunsNeedingArchival`:

```go
// ListExpiredRuns returns IDs of terminal runs whose updated_at is older than
// cutoff, oldest first. A terminal run's updated_at no longer changes, so it
// is effectively the finish time. Used by the run-retention sweeper.
func (p *Postgres) ListExpiredRuns(ctx context.Context, cutoff time.Time, limit int) ([]string, error) {
	const q = `
		SELECT id FROM runs
		WHERE status IN ('Succeeded', 'Failed', 'Cancelled')
		  AND updated_at < $1
		ORDER BY updated_at
		LIMIT $2;
	`
	rows, err := p.pool.Query(ctx, q, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/store/ -run TestPostgres_ListExpiredRuns -v`
Expected: PASS. Also run `go build ./...` — other `store.Store` implementors: check for compile errors from the new interface method (test fakes embed `store.Store` and are unaffected; if a concrete mock in `internal/controller` fails to compile, add the method returning `nil, nil`).

- [ ] **Step 5: Commit**

```bash
git add internal/store/store.go internal/store/postgres.go internal/store/postgres_run_retention_test.go
git commit -m "feat(store): ListExpiredRuns for run retention"
```

---

### Task 2: `deleteRunEverywhere` helper

**Files:**
- Create: `internal/controller/run_retention.go`
- Test: `internal/controller/run_retention_test.go` (create)

**Interfaces:**
- Consumes: `store.Store.GetLogArchive(ctx, runID) (*store.LogArchive, error)` (returns `nil, nil` when no archive), `store.Store.DeleteRun(ctx, id) error`, `objectstore.ObjectStore` (`List(ctx, prefix) ([]string, error)`, `Delete(ctx, key) error` — Delete returns nil for missing keys).
- Produces: `deleteRunEverywhere(ctx context.Context, st store.Store, obj objectstore.ObjectStore, runID string) error` — package-private in `internal/controller`; Task 3 (sweeper) and Task 4 (DELETE API) call it. Object deletion first, DB row last; nil `obj` skips object deletion.

- [ ] **Step 1: Write the failing tests**

Create `internal/controller/run_retention_test.go`:

```go
package controller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRetentionStore is a minimal store.Store stand-in implementing only the
// methods run retention uses (same pattern as fakeAuditRetentionStore).
type fakeRetentionStore struct {
	store.Store

	lockAcquired bool
	archives     map[string]*store.LogArchive // runID -> archive record (nil map = none)
	expired      [][]string                   // successive ListExpiredRuns results
	listCalls    int
	deleted      []string
}

func (f *fakeRetentionStore) AcquireAdvisoryLock(ctx context.Context, key int64) (func(), error) {
	if !f.lockAcquired {
		return nil, nil
	}
	return func() {}, nil
}

func (f *fakeRetentionStore) ListExpiredRuns(ctx context.Context, cutoff time.Time, limit int) ([]string, error) {
	if f.listCalls >= len(f.expired) {
		return nil, nil
	}
	ids := f.expired[f.listCalls]
	f.listCalls++
	return ids, nil
}

func (f *fakeRetentionStore) GetLogArchive(ctx context.Context, runID string) (*store.LogArchive, error) {
	return f.archives[runID], nil
}

func (f *fakeRetentionStore) DeleteRun(ctx context.Context, id string) error {
	f.deleted = append(f.deleted, id)
	return nil
}

// failingObjStore wraps a real object store but fails every Delete.
type failingObjStore struct {
	objectstore.ObjectStore
}

func (f *failingObjStore) Delete(ctx context.Context, key string) error {
	return errors.New("object store down")
}

func TestDeleteRunEverywhere_DeletesObjectsThenRow(t *testing.T) {
	ctx := context.Background()
	obj := objectstore.NewLocalObjectStore(t.TempDir())
	require.NoError(t, obj.Put(ctx, "runs/r1/logs.ndjson", strings.NewReader("{}"), 2))
	require.NoError(t, obj.Put(ctx, "artifacts/r1/out.tar.gz", strings.NewReader("x"), 1))
	require.NoError(t, obj.Put(ctx, "artifacts/r2/keep.tar.gz", strings.NewReader("x"), 1))
	st := &fakeRetentionStore{
		archives: map[string]*store.LogArchive{
			"r1": {RunID: "r1", ObjectKey: "runs/r1/logs.ndjson"},
		},
	}

	require.NoError(t, deleteRunEverywhere(ctx, st, obj, "r1"))

	assert.Equal(t, []string{"r1"}, st.deleted)
	_, err := obj.Get(ctx, "runs/r1/logs.ndjson")
	assert.ErrorIs(t, err, objectstore.ErrNotFound, "log archive object gone")
	keys, err := obj.List(ctx, "artifacts/r1/")
	require.NoError(t, err)
	assert.Empty(t, keys, "r1 artifacts gone")
	keys, err = obj.List(ctx, "artifacts/r2/")
	require.NoError(t, err)
	assert.Len(t, keys, 1, "other runs' artifacts untouched")
}

func TestDeleteRunEverywhere_ObjectFailureKeepsDBRow(t *testing.T) {
	ctx := context.Background()
	inner := objectstore.NewLocalObjectStore(t.TempDir())
	require.NoError(t, inner.Put(ctx, "runs/r1/logs.ndjson", strings.NewReader("{}"), 2))
	st := &fakeRetentionStore{
		archives: map[string]*store.LogArchive{
			"r1": {RunID: "r1", ObjectKey: "runs/r1/logs.ndjson"},
		},
	}

	err := deleteRunEverywhere(ctx, st, &failingObjStore{ObjectStore: inner}, "r1")

	assert.Error(t, err)
	assert.Empty(t, st.deleted, "DB row must survive an object-delete failure")
}

func TestDeleteRunEverywhere_NoRecordsIsIdempotent(t *testing.T) {
	// No archive record, no artifact objects: a retry after a partial
	// earlier attempt must still delete the DB row without error.
	ctx := context.Background()
	obj := objectstore.NewLocalObjectStore(t.TempDir())
	st := &fakeRetentionStore{}

	require.NoError(t, deleteRunEverywhere(ctx, st, obj, "r1"))
	assert.Equal(t, []string{"r1"}, st.deleted)
}

func TestDeleteRunEverywhere_NilObjectStoreDeletesRow(t *testing.T) {
	ctx := context.Background()
	st := &fakeRetentionStore{}

	require.NoError(t, deleteRunEverywhere(ctx, st, nil, "r1"))
	assert.Equal(t, []string{"r1"}, st.deleted)
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/controller/ -run TestDeleteRunEverywhere -v`
Expected: compile error `undefined: deleteRunEverywhere`.

- [ ] **Step 3: Implement the helper**

Create `internal/controller/run_retention.go`:

```go
package controller

import (
	"context"
	"fmt"

	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/store"
)

// deleteRunEverywhere removes a run's object-store data and then its DB row.
// Object deletion goes first so a surviving DB row always still references
// any surviving objects: a failure leaves the run intact for a later retry,
// never an orphaned object. ObjectStore.Delete is nil for missing keys, so
// retries after a partial failure are idempotent. Both the retention sweeper
// and the manual DELETE /runs/{id} handler use this helper. A nil obj
// (object store not configured) skips object deletion — nothing was ever
// uploaded in such deployments.
func deleteRunEverywhere(ctx context.Context, st store.Store, obj objectstore.ObjectStore, runID string) error {
	if obj != nil {
		arch, err := st.GetLogArchive(ctx, runID)
		if err != nil {
			return fmt.Errorf("get log archive: %w", err)
		}
		if arch != nil {
			if err := obj.Delete(ctx, arch.ObjectKey); err != nil {
				return fmt.Errorf("delete log archive object %s: %w", arch.ObjectKey, err)
			}
		}
		keys, err := obj.List(ctx, "artifacts/"+runID+"/")
		if err != nil {
			return fmt.Errorf("list artifact objects: %w", err)
		}
		for _, key := range keys {
			if err := obj.Delete(ctx, key); err != nil {
				return fmt.Errorf("delete artifact object %s: %w", key, err)
			}
		}
	}
	return st.DeleteRun(ctx, runID)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/controller/ -run TestDeleteRunEverywhere -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/controller/run_retention.go internal/controller/run_retention_test.go
git commit -m "feat(controller): deleteRunEverywhere removes objects then the runs row"
```

---

### Task 3: Retention sweeper

**Files:**
- Modify: `internal/controller/run_retention.go`
- Modify: `internal/controller/audit_retention.go:11-14` (advisory-key inventory comment)
- Test: `internal/controller/run_retention_test.go`

**Interfaces:**
- Consumes: `deleteRunEverywhere` (Task 2), `store.Store.ListExpiredRuns` (Task 1), `store.Store.AcquireAdvisoryLock(ctx, key) (func(), error)`.
- Produces: `RunRunRetention(ctx context.Context, st store.Store, obj objectstore.ObjectStore, interval time.Duration, retentionDays int)` — looping entrypoint, returns immediately when `st == nil || retentionDays <= 0`; `runRunRetentionOnce(ctx, st, obj, retentionDays)` — one leader-elected sweep (exported to tests only). Task 5 starts `RunRunRetention` from `main.go`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/controller/run_retention_test.go`:

```go
func TestRunRetention_FollowerDoesNothing(t *testing.T) {
	st := &fakeRetentionStore{lockAcquired: false, expired: [][]string{{"r1"}}}
	runRunRetentionOnce(context.Background(), st, nil, 30)
	assert.Zero(t, st.listCalls)
	assert.Empty(t, st.deleted)
}

func TestRunRetention_ZeroDaysMeansKeepForever(t *testing.T) {
	// RunRunRetention (the looping entrypoint) must return immediately
	// without touching the store when retentionDays <= 0.
	st := &fakeRetentionStore{lockAcquired: true, expired: [][]string{{"r1"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	RunRunRetention(ctx, st, nil, 10*time.Millisecond, 0)
	assert.Zero(t, st.listCalls)
}

func TestRunRetention_DeletesExpiredAcrossBatches(t *testing.T) {
	// A full first batch (batch size 100) triggers an immediate second fetch
	// within the same tick; a short second batch ends the sweep.
	full := make([]string, runRetentionBatchSize)
	for i := range full {
		full[i] = fmt.Sprintf("run-%03d", i)
	}
	st := &fakeRetentionStore{
		lockAcquired: true,
		expired:      [][]string{full, {"run-last"}},
	}
	runRunRetentionOnce(context.Background(), st, nil, 30)
	assert.Equal(t, 2, st.listCalls)
	assert.Len(t, st.deleted, runRetentionBatchSize+1)
}

func TestRunRetention_ZeroProgressBatchStopsTick(t *testing.T) {
	// Failed runs stay in the oldest-first result set, so a full batch where
	// every delete fails must stop the tick instead of refetching the same
	// IDs forever. Deletes fail via an object store whose Delete errors.
	full := make([]string, runRetentionBatchSize)
	archives := make(map[string]*store.LogArchive, runRetentionBatchSize)
	for i := range full {
		id := fmt.Sprintf("run-%03d", i)
		full[i] = id
		archives[id] = &store.LogArchive{RunID: id, ObjectKey: "runs/" + id + "/logs.ndjson"}
	}
	inner := objectstore.NewLocalObjectStore(t.TempDir())
	st := &fakeRetentionStore{
		lockAcquired: true,
		archives:     archives,
		// The same full batch would be returned forever; the sweep must
		// stop after the first zero-progress batch.
		expired: [][]string{full, full, full},
	}
	runRunRetentionOnce(context.Background(), st, &failingObjStore{ObjectStore: inner}, 30)
	assert.Equal(t, 1, st.listCalls, "must not refetch after a zero-progress batch")
	assert.Empty(t, st.deleted)
}
```

Add `"fmt"` to the test file's imports.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/controller/ -run TestRunRetention -v`
Expected: compile errors `undefined: runRunRetentionOnce`, `undefined: RunRunRetention`, `undefined: runRetentionBatchSize`.

- [ ] **Step 3: Implement the sweeper**

Add to `internal/controller/run_retention.go` (above `deleteRunEverywhere`); extend the imports with `"log/slog"` and `"time"`:

```go
// runRetentionLockKey is the advisory lock key for the run retention sweeper.
// Distinct from scheduler(0x65786364), approval(0x61707276), cache(0x63616368),
// logArchiver(0x6C6F6761), appSource(0x61707073), stuckRun(0x7374756B),
// auditRetention(0x61756474).
const runRetentionLockKey = int64(0x7272746E) // 'rrtn'

// runRetentionBatchSize is how many expired runs one sweep fetches at a time.
const runRetentionBatchSize = 100

// RunRunRetention periodically deletes terminal runs older than retentionDays,
// including their object-store data (log archives, artifacts). Leader-elected
// via an advisory lock so only one replica sweeps. retentionDays <= 0 disables
// retention entirely (keep forever). Returns immediately if st is nil.
func RunRunRetention(ctx context.Context, st store.Store, obj objectstore.ObjectStore, interval time.Duration, retentionDays int) {
	if st == nil || retentionDays <= 0 {
		return
	}
	if interval == 0 {
		interval = time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		runRunRetentionOnce(ctx, st, obj, retentionDays)
	}
}

func runRunRetentionOnce(ctx context.Context, st store.Store, obj objectstore.ObjectStore, retentionDays int) {
	release, err := st.AcquireAdvisoryLock(ctx, runRetentionLockKey)
	if err != nil {
		slog.Warn("run retention lock", "error", err)
		return
	}
	if release == nil {
		return // Another replica is leader.
	}
	defer release()

	cutoff := time.Now().AddDate(0, 0, -retentionDays)
	total := 0
	for {
		ids, err := st.ListExpiredRuns(ctx, cutoff, runRetentionBatchSize)
		if err != nil {
			slog.Error("run retention: list expired runs", "error", err)
			return
		}
		if len(ids) == 0 {
			break
		}
		deleted := 0
		for _, id := range ids {
			if err := deleteRunEverywhere(ctx, st, obj, id); err != nil {
				slog.Warn("run retention: delete failed, will retry next tick", "run", id, "error", err)
				continue
			}
			deleted++
		}
		total += deleted
		// Failed runs stay in the (oldest-first) result set, so a batch with
		// no progress means the next fetch would return the same IDs — stop
		// and let the next tick retry. A short batch means we drained the
		// backlog.
		if deleted == 0 || len(ids) < runRetentionBatchSize {
			break
		}
	}
	if total > 0 {
		slog.Info("run retention: deleted expired runs", "count", total, "olderThan", cutoff)
	}
}
```

In `internal/controller/audit_retention.go`, extend the key-inventory comment (lines 11-14) so it also lists the new key — change

```go
// auditRetentionLockKey is the advisory lock key for the audit log retention
// cleanup task. Distinct from scheduler(0x65786364), approval(0x61707276),
// cache(0x63616368), logArchiver(0x6C6F6761), appSource(0x61707073),
// stuckRun(0x7374756B).
```

to

```go
// auditRetentionLockKey is the advisory lock key for the audit log retention
// cleanup task. Distinct from scheduler(0x65786364), approval(0x61707276),
// cache(0x63616368), logArchiver(0x6C6F6761), appSource(0x61707073),
// stuckRun(0x7374756B), runRetention(0x7272746E).
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/controller/ -run 'TestRunRetention|TestDeleteRunEverywhere' -v`
Expected: PASS (8 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/controller/run_retention.go internal/controller/run_retention_test.go internal/controller/audit_retention.go
git commit -m "feat(controller): leader-elected run retention sweeper"
```

---

### Task 4: Manual DELETE API uses the helper (fixes object leak)

**Files:**
- Modify: `internal/controller/api_runs.go:352-355` (`handleDeleteRun`)
- Test: `internal/controller/api_runs_test.go` (append)

**Interfaces:**
- Consumes: `deleteRunEverywhere` (Task 2), `Server.store`, `Server.objStore` fields; `newTestServer(t) (*Server, store.Store)` helper (bootstrap PAT token literal is `"secret"`); `Server.SetObjectStore`.
- Produces: no new symbols — `DELETE /api/v1/runs/{id}` now also removes the run's log-archive and artifact objects; object-delete failure returns 500 and keeps the run.

- [ ] **Step 1: Write the failing regression test**

Append to `internal/controller/api_runs_test.go` (add any missing imports: `strings`, `github.com/eirueimi/unified-cd/internal/objectstore`, `github.com/eirueimi/unified-cd/internal/store` — check what the file already imports):

```go
// Regression test: DELETE /runs/{id} must remove the run's archived-log and
// artifact objects, not just the DB row (which used to leak them).
func TestAPI_DeleteRun_RemovesObjectStoreData(t *testing.T) {
	s, st := newTestServer(t)
	obj := objectstore.NewLocalObjectStore(t.TempDir())
	s.SetObjectStore(obj)
	ctx := context.Background()

	_, err := st.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := st.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	require.NoError(t, st.MarkRunFinished(ctx, run.ID, api.RunSucceeded))

	archiveKey := "runs/" + run.ID + "/logs.ndjson"
	require.NoError(t, obj.Put(ctx, archiveKey, strings.NewReader("{}"), 2))
	require.NoError(t, st.CreateLogArchive(ctx, run.ID, archiveKey, 2))
	artifactKey := "artifacts/" + run.ID + "/out.tar.gz"
	require.NoError(t, obj.Put(ctx, artifactKey, strings.NewReader("x"), 1))

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/runs/"+run.ID, nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	_, err = obj.Get(ctx, archiveKey)
	assert.ErrorIs(t, err, objectstore.ErrNotFound, "log archive object must be deleted")
	_, err = obj.Get(ctx, artifactKey)
	assert.ErrorIs(t, err, objectstore.ErrNotFound, "artifact object must be deleted")
	_, err = st.GetRun(ctx, run.ID)
	assert.ErrorIs(t, err, store.ErrRunNotFound)
}
```

If existing DELETE-run tests in `api_runs_test.go` authenticate differently than `Bearer secret`, mirror their pattern instead.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/controller/ -run TestAPI_DeleteRun_RemovesObjectStoreData -v`
Expected: FAIL — DELETE returns 204 but the two `obj.Get` assertions find the objects still present (this is the leak).

- [ ] **Step 3: Switch `handleDeleteRun` to the helper**

In `internal/controller/api_runs.go`, replace

```go
	if err := s.store.DeleteRun(r.Context(), id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
```

with

```go
	if err := deleteRunEverywhere(r.Context(), s.store, s.objStore, id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/controller/ -run 'TestAPI_DeleteRun' -v`
Expected: PASS, including all pre-existing delete-run tests (they exercise the nil-objStore path, which Task 2 covered).

- [ ] **Step 5: Commit**

```bash
git add internal/controller/api_runs.go internal/controller/api_runs_test.go
git commit -m "fix(api): DELETE /runs/{id} also removes archived logs and artifacts"
```

---

### Task 5: Controller wiring (`--run-retention-days`)

**Files:**
- Modify: `cmd/controller/main.go` (default-resolver next to `auditRetentionDaysDefault` at line 28; flag next to `audit-retention-days` at line 93; goroutine start next to `RunAuditRetention` at line 279)

**Interfaces:**
- Consumes: `controller.RunRunRetention` (Task 3), the `obj objectstore.ObjectStore` variable (declared at line 177, may be nil), `st` store.
- Produces: flag `--run-retention-days`, env `UNIFIED_RUN_RETENTION_DAYS`, default **0 = keep forever**.

- [ ] **Step 1: Add the default resolver**

After `auditRetentionDaysDefault` in `cmd/controller/main.go`:

```go
// runRetentionDaysDefault resolves the --run-retention-days flag default from
// UNIFIED_RUN_RETENTION_DAYS, falling back to 0 (keep forever) when unset or
// invalid. Unlike audit retention this is opt-in: deleting run history is
// irreversible (it also removes the spec snapshot `run replay` uses).
func runRetentionDaysDefault() int {
	v := os.Getenv("UNIFIED_RUN_RETENTION_DAYS")
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		slog.Warn("invalid UNIFIED_RUN_RETENTION_DAYS, keeping runs forever", "value", v)
		return 0
	}
	return n
}
```

- [ ] **Step 2: Register the flag**

Directly under the `auditRetentionDays` flag (line 93):

```go
	runRetentionDays := flag.Int("run-retention-days", runRetentionDaysDefault(), "days to keep terminal runs incl. their logs, log archives, and artifacts; 0 = keep forever (env: UNIFIED_RUN_RETENTION_DAYS)")
```

- [ ] **Step 3: Start the sweeper**

Directly after the `go controller.RunAuditRetention(...)` line (279):

```go
	if *runRetentionDays > 0 {
		slog.Info("run retention enabled", "retentionDays", *runRetentionDays)
	} else {
		slog.Info("run retention disabled (keep forever)")
	}
	go controller.RunRunRetention(ctx, st, obj, time.Hour, *runRetentionDays)
```

(`obj` may be nil here — `deleteRunEverywhere` handles that.)

- [ ] **Step 4: Build and smoke-check the flag**

Run: `go build ./... && go run ./cmd/controller --help 2>&1 | grep -A1 run-retention-days`
Expected: the flag and its help text are listed (the controller exits after `--help` without needing a DB).

- [ ] **Step 5: Commit**

```bash
git add cmd/controller/main.go
git commit -m "feat(controller): --run-retention-days flag wires the retention sweeper"
```

---

### Task 6: Documentation + final sweep

**Files:**
- Modify: `docs/configuration.md` (controller flags/env section — grep for `audit-retention-days` and mirror its presentation)
- Modify: `docs/operations.md` (storage/retention guidance)
- Modify: `docs/high-availability.md` (leader-election table around line 82; advisory-key list around line 359)

**Interfaces:**
- Consumes: final flag/env names from Task 5 (`--run-retention-days`, `UNIFIED_RUN_RETENTION_DAYS`, default 0).
- Produces: user-facing docs. No code.

- [ ] **Step 1: `docs/configuration.md`**

Find the `audit-retention-days` entry (`grep -n "audit-retention-days" docs/configuration.md`) and add a matching entry directly after it, in the same format (table row or definition), with this content:

> `--run-retention-days` / `UNIFIED_RUN_RETENTION_DAYS` — days to keep terminal (Succeeded/Failed/Cancelled) runs. When a run expires, its database records **and** its object-store data (archived logs, artifacts) are deleted. `0` (default) keeps runs forever. Deletion is irreversible — an expired run can no longer be replayed, because `run replay` uses the run's stored spec snapshot.

- [ ] **Step 2: `docs/operations.md`**

In the storage/data-management section (near the existing object-store durability paragraph at line ~37), add:

> **Run retention.** By default unified-cd keeps every run forever: `runs` rows, log rows, archived logs, and artifacts all accumulate. Note that log archival only *copies* logs to the object store — database log rows are never trimmed by it. Set `--run-retention-days` (env `UNIFIED_RUN_RETENTION_DAYS`) to enable an hourly, leader-elected sweep that deletes terminal runs older than N days, including their archived logs and artifacts. Audit logs have their own independent setting (`--audit-retention-days`).

- [ ] **Step 3: `docs/high-availability.md`**

Add a row to the leader-election table (line ~82), matching the existing format:

```
| Run retention (`RunRunRetention`) | advisory lock (`runRetentionLockKey`) | Only the leader deletes expired runs |
```

If the advisory-key enumeration around line 359 lists key names, append the run-retention key there in the same style.

- [ ] **Step 4: Hygiene sweep and full test run**

```bash
grep -rn "run-retention" docs/ README.md          # entries present and consistent
grep -rn "RUN_RETENTION" docs/                     # env var documented
go test ./internal/controller/ ./internal/store/   # affected packages green (needs Docker)
go build ./...
```

Expected: docs hits only where intended; tests PASS. `docs/field-reference.md`, `schemas/`, `examples/`, `templates/` are untouched (no DSL change) — spot-check with `git status`.

- [ ] **Step 5: Commit**

```bash
git add docs/configuration.md docs/operations.md docs/high-availability.md
git commit -m "docs: run retention flag, operations guidance, HA leader table"
```
