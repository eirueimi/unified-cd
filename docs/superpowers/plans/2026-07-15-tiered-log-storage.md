# Tiered Log Storage (DB Log Trim) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Trim `logs` rows out of PostgreSQL N days after a run's logs are archived, while serving every log read path (CLI tail, WebUI stats/range/search, SSE backfill) from the archive with identical contracts — zero client changes.

**Architecture:** A leader-elected hourly sweeper (`log_trim.go`, mirroring `run_retention.go`) marks archives `trimmed_at` and deletes the run's `logs` rows in one transaction, after verifying the archive object exists. A controller-layer archive reader (`archived_logs.go`) lazily fetches `runs/<runID>/logs.ndjson`, decodes it into `[]api.LogLine`, caches parsed slices in a byte-bounded LRU, and reproduces the store's five read contracts as pure functions. Each log handler branches on one cheap `GetLogArchive` lookup.

**Tech Stack:** Go, pgx (via `internal/store`), golang-migrate SQL files, `internal/objectstore`, testify, `store.NewTestPostgres` (dockerized Postgres).

**Spec:** `docs/superpowers/specs/2026-07-15-tiered-log-storage-design.md`

## Global Constraints

- All code, comments, commit messages, and docs in **English** (AGENTS.md). No PII.
- Work in the existing worktree `../unified-cd-log-trim`, branch `log-trim` (stacked on `run-retention`, PR #44). Never commit from the main tree.
- Flag `--log-trim-days` / env `UNIFIED_LOG_TRIM_DAYS`, default **0 = never trim**.
- New advisory lock key `logTrimLockKey = int64(0x6C74726D) // 'ltrm'` — existing keys: scheduler `0x65786364`, approval `0x61707276`, cache `0x63616368`, logArchiver `0x6C6F6761`, appSource `0x61707073`, stuckRun `0x7374756B`, auditRetention `0x61756474`, runRetention `0x7272746E` (plus `sync` `0x73796E63`, `queu` `0x71756575` — not all inventory comments list these two; do not remove them).
- Object-store failure on a trimmed run's read returns **503** with message prefix `log archive unavailable`.
- Every `internal/store/migrations/*.up.sql` needs a `schemaSentinels` entry in `internal/store/verify.go` (a test enforces this).
- Store integration tests require Docker; they skip under `-short`.
- `docs/field-reference.md` is generated — never hand-edit (no DSL change in this feature).

---

### Task 1: Migration 012 — `run_log_archives.trimmed_at` + `LogArchive.TrimmedAt`

**Files:**
- Create: `internal/store/migrations/012_run_log_archives_trimmed_at.up.sql`
- Create: `internal/store/migrations/012_run_log_archives_trimmed_at.down.sql`
- Modify: `internal/store/verify.go:40` (append sentinel entry)
- Modify: `internal/store/store.go:11-16` (`LogArchive` struct)
- Modify: `internal/store/postgres.go:1354-1366` (`GetLogArchive`)
- Test: `internal/store/postgres_log_trim_test.go` (create)

**Interfaces:**
- Consumes: existing `run_log_archives` table, `CreateLogArchive`, `GetLogArchive`.
- Produces: `store.LogArchive.TrimmedAt *time.Time` (nil = logs rows still in the DB). Tasks 2, 4, 5 rely on this exact field name and type.

- [ ] **Step 1: Write the failing test**

Create `internal/store/postgres_log_trim_test.go`:

```go
package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPostgres_LogArchive_TrimmedAt(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	require.NoError(t, pg.CreateLogArchive(ctx, run.ID, "runs/"+run.ID+"/logs.ndjson", 2))

	arch, err := pg.GetLogArchive(ctx, run.ID)
	require.NoError(t, err)
	require.NotNil(t, arch)
	assert.Nil(t, arch.TrimmedAt, "fresh archive record must not be marked trimmed")

	_, err = pg.pool.Exec(ctx,
		`UPDATE run_log_archives SET trimmed_at = NOW() WHERE run_id = $1`, run.ID)
	require.NoError(t, err)

	arch, err = pg.GetLogArchive(ctx, run.ID)
	require.NoError(t, err)
	require.NotNil(t, arch)
	assert.NotNil(t, arch.TrimmedAt)
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd /path/to/unified-cd-log-trim && go test ./internal/store/ -run TestPostgres_LogArchive_TrimmedAt -v`
Expected: FAIL — the `UPDATE ... trimmed_at` errors with `column "trimmed_at" ... does not exist` (or, if the migration existed but the struct didn't, a compile error on `arch.TrimmedAt`).

- [ ] **Step 3: Add migration, sentinel, struct field, scan**

`internal/store/migrations/012_run_log_archives_trimmed_at.up.sql`:

```sql
-- trimmed_at records when the run's logs rows were deleted from the DB after
-- archival (tiered log storage). NULL = rows still present. It is the single
-- source of truth for "serve this run's logs from the archive object", which
-- keeps 'genuinely empty logs' distinguishable from 'trimmed'.
ALTER TABLE public.run_log_archives ADD COLUMN IF NOT EXISTS trimmed_at timestamp with time zone;
```

`internal/store/migrations/012_run_log_archives_trimmed_at.down.sql`:

```sql
ALTER TABLE public.run_log_archives DROP COLUMN IF EXISTS trimmed_at;
```

In `internal/store/verify.go`, append to `schemaSentinels` (after the `{11, ...}` entry):

```go
	{12, "012_run_log_archives_trimmed_at", "run_log_archives", "trimmed_at", ""},
```

In `internal/store/store.go`, extend `LogArchive`:

```go
type LogArchive struct {
	RunID      string
	ObjectKey  string
	SizeBytes  int64
	ArchivedAt time.Time
	// TrimmedAt is set when the run's logs rows were deleted from the DB
	// after archival; nil means the rows are still present.
	TrimmedAt *time.Time
}
```

In `internal/store/postgres.go`, update `GetLogArchive` to select and scan the new column:

```go
func (p *Postgres) GetLogArchive(ctx context.Context, runID string) (*LogArchive, error) {
	var a LogArchive
	err := p.pool.QueryRow(ctx,
		`SELECT run_id, object_key, size_bytes, archived_at, trimmed_at FROM run_log_archives WHERE run_id = $1`,
		runID).Scan(&a.RunID, &a.ObjectKey, &a.SizeBytes, &a.ArchivedAt, &a.TrimmedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/store/ -run 'TestPostgres_LogArchive_TrimmedAt|TestSchemaSentinels' -v && go build ./...`
Expected: PASS (the sentinel-coverage test would fail loudly if the new entry were missing or misnumbered).

- [ ] **Step 5: Commit**

```bash
git add internal/store/migrations/012_run_log_archives_trimmed_at.up.sql internal/store/migrations/012_run_log_archives_trimmed_at.down.sql internal/store/verify.go internal/store/store.go internal/store/postgres.go internal/store/postgres_log_trim_test.go
git commit -m "feat(store): trimmed_at column on run_log_archives (migration 012)"
```

---

### Task 2: Store methods — `ListTrimCandidates`, `TrimRunLogs`, `DeleteLogArchive`

**Files:**
- Modify: `internal/store/store.go` (interface, in the `// Log Archives` block)
- Modify: `internal/store/postgres.go` (implementations, after `GetLogArchive`)
- Test: `internal/store/postgres_log_trim_test.go` (append)

**Interfaces:**
- Consumes: migration 012 (Task 1); `AppendLog(ctx, runID string, stepIndex int, stream string, ts time.Time, line string) (int64, error)` for test seeding.
- Produces (Task 6's sweeper and its fake store use these exact signatures):
  - `ListTrimCandidates(ctx context.Context, cutoff time.Time, limit int) ([]string, error)` — run IDs with an archive record, `trimmed_at IS NULL AND archived_at < cutoff`, oldest `archived_at` first, LIMIT.
  - `TrimRunLogs(ctx context.Context, runID string) (int64, error)` — one transaction: mark `trimmed_at = NOW()` (guard `trimmed_at IS NULL`), then `DELETE FROM logs`. Returns logs rows deleted; `(0, nil)` no-op when there is no untrimmed archive record (never deletes logs in that case).
  - `DeleteLogArchive(ctx context.Context, runID string) error` — removes a stale archive record so the archiver re-archives.

- [ ] **Step 1: Write the failing tests**

Append to `internal/store/postgres_log_trim_test.go` (add `"time"` and `api` imports to the existing import block):

```go
func TestPostgres_TrimRunLogs(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	// archived: has logs and an archive record -> trimmable.
	archived, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		_, err = pg.AppendLog(ctx, archived.ID, 0, "stdout", time.Now(), "line")
		require.NoError(t, err)
	}
	require.NoError(t, pg.CreateLogArchive(ctx, archived.ID, "runs/"+archived.ID+"/logs.ndjson", 10))

	// unarchived: has logs but NO archive record -> must never be trimmed.
	unarchived, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	_, err = pg.AppendLog(ctx, unarchived.ID, 0, "stdout", time.Now(), "keep me")
	require.NoError(t, err)

	n, err := pg.TrimRunLogs(ctx, archived.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(3), n)
	count, _, _, err := pg.CountLogs(ctx, archived.ID, nil)
	require.NoError(t, err)
	assert.Zero(t, count, "logs rows must be gone")
	arch, err := pg.GetLogArchive(ctx, archived.ID)
	require.NoError(t, err)
	require.NotNil(t, arch)
	assert.NotNil(t, arch.TrimmedAt, "trim must mark the archive record")

	// Second trim is a no-op.
	n, err = pg.TrimRunLogs(ctx, archived.ID)
	require.NoError(t, err)
	assert.Zero(t, n)

	// No archive record: no-op AND logs untouched (guard ordering).
	n, err = pg.TrimRunLogs(ctx, unarchived.ID)
	require.NoError(t, err)
	assert.Zero(t, n)
	count, _, _, err = pg.CountLogs(ctx, unarchived.ID, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count, "logs of an unarchived run must never be deleted")
}

func TestPostgres_ListTrimCandidates(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)

	mkArchived := func(age string, trimmed bool) string {
		t.Helper()
		run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
		require.NoError(t, err)
		require.NoError(t, pg.CreateLogArchive(ctx, run.ID, "runs/"+run.ID+"/logs.ndjson", 1))
		if age != "" {
			_, err = pg.pool.Exec(ctx,
				`UPDATE run_log_archives SET archived_at = NOW() - $1::interval WHERE run_id = $2`, age, run.ID)
			require.NoError(t, err)
		}
		if trimmed {
			_, err = pg.pool.Exec(ctx,
				`UPDATE run_log_archives SET trimmed_at = NOW() WHERE run_id = $1`, run.ID)
			require.NoError(t, err)
		}
		return run.ID
	}

	oldest := mkArchived("20 days", false)
	older := mkArchived("10 days", false)
	_ = mkArchived("20 days", true) // already trimmed: excluded
	_ = mkArchived("", false)       // fresh: excluded by cutoff

	cutoff := time.Now().AddDate(0, 0, -7)
	ids, err := pg.ListTrimCandidates(ctx, cutoff, 10)
	require.NoError(t, err)
	assert.Equal(t, []string{oldest, older}, ids, "untrimmed + old only, oldest archived_at first")

	ids, err = pg.ListTrimCandidates(ctx, cutoff, 1)
	require.NoError(t, err)
	assert.Equal(t, []string{oldest}, ids)
}

func TestPostgres_DeleteLogArchive(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	require.NoError(t, pg.CreateLogArchive(ctx, run.ID, "runs/"+run.ID+"/logs.ndjson", 1))

	require.NoError(t, pg.DeleteLogArchive(ctx, run.ID))
	arch, err := pg.GetLogArchive(ctx, run.ID)
	require.NoError(t, err)
	assert.Nil(t, arch)
	// Idempotent on a missing record.
	require.NoError(t, pg.DeleteLogArchive(ctx, run.ID))
}
```

Note: `TestPostgres_TrimRunLogs` uses `CountLogs` (exists) and does NOT need the `api` import after all — remove it from the instruction above if unused; the compiler will tell you.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/store/ -run 'TestPostgres_TrimRunLogs|TestPostgres_ListTrimCandidates|TestPostgres_DeleteLogArchive' -v`
Expected: compile errors — `pg.TrimRunLogs undefined`, `pg.ListTrimCandidates undefined`, `pg.DeleteLogArchive undefined`.

- [ ] **Step 3: Implement**

In `internal/store/store.go`, inside the `Store` interface's `// Log Archives` block (after `ListExpiredRuns`):

```go
	// Log trim (tiered log storage)
	// ListTrimCandidates returns run IDs whose logs are archived but not yet
	// trimmed, with archived_at older than cutoff, oldest first, up to limit.
	ListTrimCandidates(ctx context.Context, cutoff time.Time, limit int) ([]string, error)
	// TrimRunLogs marks the run's archive record trimmed and deletes its logs
	// rows in one transaction. Returns rows deleted. A run with no untrimmed
	// archive record is a (0, nil) no-op — logs are never deleted unarchived.
	TrimRunLogs(ctx context.Context, runID string) (int64, error)
	// DeleteLogArchive removes the archive record (e.g. when its object is
	// missing) so the archiver re-archives the run on its next tick.
	DeleteLogArchive(ctx context.Context, runID string) error
```

In `internal/store/postgres.go`, after `GetLogArchive`:

```go
// ListTrimCandidates returns run IDs whose logs are archived but not yet
// trimmed, with archived_at older than cutoff, oldest first. Archive records
// only exist for terminal runs, so no status filter is needed.
func (p *Postgres) ListTrimCandidates(ctx context.Context, cutoff time.Time, limit int) ([]string, error) {
	const q = `
		SELECT run_id FROM run_log_archives
		WHERE trimmed_at IS NULL AND archived_at < $1
		ORDER BY archived_at
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

// TrimRunLogs deletes a run's logs rows after marking its archive record
// trimmed, in one transaction. The mark goes FIRST and guards trimmed_at IS
// NULL: if there is no untrimmed archive record (never archived, already
// trimmed, or the run was deleted by retention) nothing is deleted and the
// call is a (0, nil) no-op.
func (p *Postgres) TrimRunLogs(ctx context.Context, runID string) (int64, error) {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	ct, err := tx.Exec(ctx,
		`UPDATE run_log_archives SET trimmed_at = NOW() WHERE run_id = $1 AND trimmed_at IS NULL`, runID)
	if err != nil {
		return 0, err
	}
	if ct.RowsAffected() == 0 {
		return 0, nil // no untrimmed archive record: never touch logs
	}
	tag, err := tx.Exec(ctx, `DELETE FROM logs WHERE run_id = $1`, runID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), tx.Commit(ctx)
}

func (p *Postgres) DeleteLogArchive(ctx context.Context, runID string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM run_log_archives WHERE run_id = $1`, runID)
	return err
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/store/ -run 'TestPostgres_TrimRunLogs|TestPostgres_ListTrimCandidates|TestPostgres_DeleteLogArchive' -v && go build ./...`
Expected: PASS. `go build` catches any other `store.Store` implementor (test fakes embed the interface and are unaffected).

- [ ] **Step 5: Commit**

```bash
git add internal/store/store.go internal/store/postgres.go internal/store/postgres_log_trim_test.go
git commit -m "feat(store): ListTrimCandidates, TrimRunLogs, DeleteLogArchive"
```

---

### Task 3: Archive-backed log reader (`archived_logs.go`)

**Files:**
- Create: `internal/controller/archived_logs.go`
- Test: `internal/controller/archived_logs_test.go` (create)

**Interfaces:**
- Consumes: `runLogArchiveKey(runID string) string` (exists in `internal/controller/archiver.go`), `objectstore.ObjectStore`, `api.LogLine`, `store.LogSearchMatch{Row int64; Seq int64; StepIndex int}`.
- Produces (Tasks 4 and 5 call these exact names):
  - `newArchivedLogs(obj objectstore.ObjectStore) *archivedLogs`
  - `(a *archivedLogs) lines(ctx context.Context, runID string) ([]api.LogLine, error)` — fetch+decode with byte-bounded LRU cache (`archivedLogsCacheBytes`, a `var` so tests can shrink it).
  - Pure view functions: `tailAfter(lines []api.LogLine, afterSeq int64, limit int) []api.LogLine`, `tailRecent(lines []api.LogLine, limit int) []api.LogLine`, `countArchivedLogs(lines []api.LogLine, steps []int) (count, minSeq, maxSeq int64)`, `archivedLogRange(lines []api.LogLine, steps []int, offset, limit int) []api.LogLine`, `searchArchivedLogs(lines []api.LogLine, steps []int, q string, capN int) (int64, []store.LogSearchMatch)`.

- [ ] **Step 1: Write the failing tests**

Create `internal/controller/archived_logs_test.go`. Two parts: (A) golden parity against the real store over identical data, (B) cache behavior.

```go
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedParityRun inserts a deliberately tricky log set into Postgres and
// uploads the equivalent ndjson archive, returning the run ID and the object
// store. Lines cover: multiple steps, mixed streams, ILIKE metacharacters
// (%, _, \), and mixed case.
func seedParityRun(t *testing.T, pg store.Store, obj objectstore.ObjectStore) string {
	t.Helper()
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	seed := []struct {
		step   int
		stream string
		line   string
	}{
		{0, "stdout", "Building target ALPHA"},
		{0, "stderr", "warn: 100% done_ok"},
		{1, "stdout", `path C:\tmp\x`},
		{1, "stdout", "building target alpha"},
		{2, "stderr", "under_score and per%cent"},
		{2, "stdout", "plain line"},
	}
	for _, l := range seed {
		_, err := pg.AppendLog(ctx, run.ID, l.step, l.stream, time.Now(), l.line)
		require.NoError(t, err)
	}
	// Build the archive exactly like archiveRunLogs does.
	lines, err := pg.TailLogs(ctx, run.ID, 0, 1_000_000)
	require.NoError(t, err)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, l := range lines {
		require.NoError(t, enc.Encode(l))
	}
	require.NoError(t, obj.Put(ctx, runLogArchiveKey(run.ID), &buf, int64(buf.Len())))
	return run.ID
}

// TestArchivedLogs_ParityWithStore asserts every reader contract returns
// results identical to the store methods over the same data.
func TestArchivedLogs_ParityWithStore(t *testing.T) {
	_, pg := newTestServer(t)
	obj := objectstore.NewLocalObjectStore(t.TempDir())
	runID := seedParityRun(t, pg, obj)
	ctx := context.Background()

	a := newArchivedLogs(obj)
	lines, err := a.lines(ctx, runID)
	require.NoError(t, err)
	require.Len(t, lines, 6)

	stepSets := [][]int{nil, {0}, {1, 2}, {5}}

	for _, steps := range stepSets {
		label := fmt.Sprintf("steps=%v", steps)

		wantCount, wantMin, wantMax, err := pg.CountLogs(ctx, runID, steps)
		require.NoError(t, err, label)
		gotCount, gotMin, gotMax := countArchivedLogs(lines, steps)
		assert.Equal(t, wantCount, gotCount, label)
		assert.Equal(t, wantMin, gotMin, label)
		assert.Equal(t, wantMax, gotMax, label)

		for _, window := range []struct{ offset, limit int }{{0, 10}, {1, 2}, {4, 10}, {99, 5}} {
			want, err := pg.ListLogsRange(ctx, runID, steps, window.offset, window.limit)
			require.NoError(t, err, label)
			got := archivedLogRange(lines, steps, window.offset, window.limit)
			assert.Equal(t, normalize(want), normalize(got), "%s offset=%d limit=%d", label, window.offset, window.limit)
		}

		for _, q := range []string{"alpha", "ALPHA", "100%", "under_score", `C:\tmp`, "nomatch", "_"} {
			wantTotal, wantMatches, err := pg.SearchLogs(ctx, runID, steps, q, 3)
			require.NoError(t, err, label)
			gotTotal, gotMatches := searchArchivedLogs(lines, steps, q, 3)
			assert.Equal(t, wantTotal, gotTotal, "%s q=%q", label, q)
			assert.Equal(t, normalizeMatches(wantMatches), normalizeMatches(gotMatches), "%s q=%q", label, q)
		}
	}

	// TailLogs paging parity over the full view.
	all, err := pg.TailLogs(ctx, runID, 0, 1000)
	require.NoError(t, err)
	for _, after := range []int64{0, all[0].Seq, all[2].Seq, all[5].Seq} {
		for _, limit := range []int{1, 3, 100} {
			want, err := pg.TailLogs(ctx, runID, after, limit)
			require.NoError(t, err)
			got := tailAfter(lines, after, limit)
			assert.Equal(t, normalize(want), normalize(got), "after=%d limit=%d", after, limit)
		}
	}

	// TailLogsRecent parity.
	for _, limit := range []int{2, 6, 100} {
		want, err := pg.TailLogsRecent(ctx, runID, limit)
		require.NoError(t, err)
		got := tailRecent(lines, limit)
		assert.Equal(t, normalize(want), normalize(got), "recent limit=%d", limit)
	}
}

// normalize maps nil/empty to empty and truncates timestamps to microseconds:
// Postgres stores timestamptz at microsecond precision, while the ndjson
// round-trip keeps Go's nanoseconds, so exact time.Time equality would fail
// spuriously.
func normalize(in []api.LogLine) []api.LogLine {
	out := make([]api.LogLine, 0, len(in))
	for _, l := range in {
		l.Timestamp = l.Timestamp.Truncate(time.Microsecond)
		out = append(out, l)
	}
	return out
}

func normalizeMatches(in []store.LogSearchMatch) []store.LogSearchMatch {
	if len(in) == 0 {
		return []store.LogSearchMatch{}
	}
	return in
}

func TestArchivedLogs_CacheEvictsByBytes(t *testing.T) {
	ctx := context.Background()
	obj := objectstore.NewLocalObjectStore(t.TempDir())
	put := func(runID, line string) {
		var buf bytes.Buffer
		require.NoError(t, json.NewEncoder(&buf).Encode(api.LogLine{Seq: 1, Line: line}))
		require.NoError(t, obj.Put(ctx, runLogArchiveKey(runID), &buf, int64(buf.Len())))
	}
	put("r1", strings.Repeat("a", 100))
	put("r2", strings.Repeat("b", 100))

	old := archivedLogsCacheBytes
	archivedLogsCacheBytes = 200 // each entry is ~180 bytes: two never fit
	defer func() { archivedLogsCacheBytes = old }()

	a := newArchivedLogs(obj)
	_, err := a.lines(ctx, "r1")
	require.NoError(t, err)
	_, err = a.lines(ctx, "r2")
	require.NoError(t, err)
	assert.Equal(t, 1, a.cacheLen(), "r1 must have been evicted to fit r2")

	// Oversized archive: served but never cached.
	put("big", strings.Repeat("c", 500))
	_, err = a.lines(ctx, "big")
	require.NoError(t, err)
	assert.Equal(t, 1, a.cacheLen())
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/controller/ -run TestArchivedLogs -v`
Expected: compile errors (`undefined: newArchivedLogs`, etc.).

- [ ] **Step 3: Implement the reader**

Create `internal/controller/archived_logs.go`:

```go
package controller

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/store"
)

// archivedLogsCacheBytes bounds the total raw-ndjson bytes of parsed archives
// kept in memory. Trimmed runs are terminal and their archives immutable, so
// caching is safe; an archive larger than the whole cap is decoded per
// request and never cached. A var (not const) so tests can shrink it.
var archivedLogsCacheBytes = int64(128 << 20) // 128 MiB

type archivedLogEntry struct {
	runID string
	lines []api.LogLine
	bytes int64
}

// archivedLogs serves the log read contracts for runs whose logs rows were
// trimmed from the DB, by fetching and decoding runs/<runID>/logs.ndjson.
type archivedLogs struct {
	obj objectstore.ObjectStore

	mu    sync.Mutex
	cache map[string]*list.Element // runID -> element holding *archivedLogEntry
	order *list.List               // front = most recently used
	total int64
}

func newArchivedLogs(obj objectstore.ObjectStore) *archivedLogs {
	return &archivedLogs{obj: obj, cache: map[string]*list.Element{}, order: list.New()}
}

func (a *archivedLogs) cacheLen() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.cache)
}

// lines returns the run's full archived log, seq-ascending (the archiver
// wrote it in TailLogs order). Callers must treat the slice as read-only —
// it may be shared via the cache.
func (a *archivedLogs) lines(ctx context.Context, runID string) ([]api.LogLine, error) {
	a.mu.Lock()
	if el, ok := a.cache[runID]; ok {
		a.order.MoveToFront(el)
		lines := el.Value.(*archivedLogEntry).lines
		a.mu.Unlock()
		return lines, nil
	}
	a.mu.Unlock()

	rc, err := a.obj.Get(ctx, runLogArchiveKey(runID))
	if err != nil {
		return nil, fmt.Errorf("fetch log archive: %w", err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("read log archive: %w", err)
	}
	var lines []api.LogLine
	dec := json.NewDecoder(bytes.NewReader(raw))
	for {
		var l api.LogLine
		if err := dec.Decode(&l); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode log archive: %w", err)
		}
		lines = append(lines, l)
	}

	size := int64(len(raw))
	if size <= archivedLogsCacheBytes {
		a.mu.Lock()
		if _, ok := a.cache[runID]; !ok {
			el := a.order.PushFront(&archivedLogEntry{runID: runID, lines: lines, bytes: size})
			a.cache[runID] = el
			a.total += size
			for a.total > archivedLogsCacheBytes {
				oldest := a.order.Back()
				e := oldest.Value.(*archivedLogEntry)
				a.order.Remove(oldest)
				delete(a.cache, e.runID)
				a.total -= e.bytes
			}
		}
		a.mu.Unlock()
	}
	return lines, nil
}

// filterSteps returns the step-filtered view; an empty steps set means the
// whole log (same convention as logsStepFilter in the store).
func filterSteps(lines []api.LogLine, steps []int) []api.LogLine {
	if len(steps) == 0 {
		return lines
	}
	want := make(map[int]bool, len(steps))
	for _, s := range steps {
		want[s] = true
	}
	out := make([]api.LogLine, 0, len(lines))
	for _, l := range lines {
		if want[l.StepIndex] {
			out = append(out, l)
		}
	}
	return out
}

// tailAfter mirrors store TailLogs: lines with seq > afterSeq, ascending, LIMIT.
func tailAfter(lines []api.LogLine, afterSeq int64, limit int) []api.LogLine {
	i := sort.Search(len(lines), func(i int) bool { return lines[i].Seq > afterSeq })
	out := lines[i:]
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// tailRecent mirrors store TailLogsRecent: the last `limit` lines, ascending.
func tailRecent(lines []api.LogLine, limit int) []api.LogLine {
	if len(lines) > limit {
		return lines[len(lines)-limit:]
	}
	return lines
}

// countArchivedLogs mirrors store CountLogs over the step-filtered view.
func countArchivedLogs(lines []api.LogLine, steps []int) (count, minSeq, maxSeq int64) {
	v := filterSteps(lines, steps)
	if len(v) == 0 {
		return 0, 0, 0
	}
	return int64(len(v)), v[0].Seq, v[len(v)-1].Seq
}

// archivedLogRange mirrors store ListLogsRange (view order, OFFSET/LIMIT).
func archivedLogRange(lines []api.LogLine, steps []int, offset, limit int) []api.LogLine {
	v := filterSteps(lines, steps)
	if offset >= len(v) {
		return nil
	}
	v = v[offset:]
	if len(v) > limit {
		v = v[:limit]
	}
	return v
}

// searchArchivedLogs mirrors store SearchLogs: case-insensitive literal
// substring match (the escaped-ILIKE semantics), row numbers are 0-based
// positions in the step-filtered view BEFORE the match filter, results
// seq-ordered and capped at capN with the uncapped total returned.
func searchArchivedLogs(lines []api.LogLine, steps []int, q string, capN int) (int64, []store.LogSearchMatch) {
	v := filterSteps(lines, steps)
	needle := strings.ToLower(q)
	var total int64
	var out []store.LogSearchMatch
	for row, l := range v {
		if strings.Contains(strings.ToLower(l.Line), needle) {
			total++
			if len(out) < capN {
				out = append(out, store.LogSearchMatch{Row: int64(row), Seq: l.Seq, StepIndex: l.StepIndex})
			}
		}
	}
	return total, out
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/controller/ -run TestArchivedLogs -v`
Expected: PASS. If the parity test fails only on `Timestamp` equality, the `normalize` truncation in the test is the intended comparison — investigate a real mismatch (seq/order/filter) before touching it.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/archived_logs.go internal/controller/archived_logs_test.go
git commit -m "feat(controller): archive-backed log reader with byte-bounded LRU"
```

---

### Task 4: Handler fallback — tail/stats/range/search serve trimmed runs from the archive

**Files:**
- Modify: `internal/controller/server.go` (Server struct + `SetObjectStore`)
- Modify: `internal/controller/api_runs.go:168-182` (`handleTailLogs`), `:414-426` (`handleLogStats`), `:430-461` (`handleLogRange`), `:466-486` (`handleLogSearch`)
- Modify: `internal/controller/archived_logs.go` (add the `logsTrimmed` helper)
- Test: `internal/controller/api_runs_trimmed_test.go` (create)

**Interfaces:**
- Consumes: `archivedLogs` and the pure view functions (Task 3); `store.LogArchive.TrimmedAt` (Task 1); `Server.objStore` field and `SetObjectStore` (exist).
- Produces:
  - `Server.archLogs *archivedLogs` field, initialized in `SetObjectStore`.
  - `(s *Server) logsTrimmed(ctx context.Context, runID string) (bool, error)` — true iff an archive record exists with `TrimmedAt != nil` AND `s.archLogs != nil`. Task 5 (SSE) uses it too.
  - HTTP behavior: identical responses before/after trim; 503 `log archive unavailable: ...` when the object store fails for a trimmed run.

- [ ] **Step 1: Write the failing test**

Create `internal/controller/api_runs_trimmed_test.go`:

```go
package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// get performs an authenticated GET and returns (status, body).
func get(t *testing.T, s *Server, path string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestLogEndpoints_IdenticalAfterTrim is the feature's core guarantee: every
// windowed-viewer and CLI log endpoint returns byte-identical responses
// before and after the run's logs rows are trimmed from the DB.
func TestLogEndpoints_IdenticalAfterTrim(t *testing.T) {
	s, pg := newTestServer(t)
	obj := objectstore.NewLocalObjectStore(t.TempDir())
	s.SetObjectStore(obj)
	runID := seedParityRun(t, pg, obj) // from archived_logs_test.go
	ctx := context.Background()
	require.NoError(t, pg.CreateLogArchive(ctx, runID, runLogArchiveKey(runID), 1))
	require.NoError(t, pg.MarkRunFinished(ctx, runID, api.RunSucceeded))

	paths := []string{
		"/api/v1/runs/" + runID + "/logs?after=0",
		"/api/v1/runs/" + runID + "/logs/stats",
		"/api/v1/runs/" + runID + "/logs/stats?steps=0,2",
		"/api/v1/runs/" + runID + "/logs/range?offset=1&limit=3",
		"/api/v1/runs/" + runID + "/logs/range?steps=1,2&offset=0&limit=10",
		"/api/v1/runs/" + runID + "/logs/search?q=alpha",
		"/api/v1/runs/" + runID + "/logs/search?q=100%25&steps=0",
	}
	before := map[string]string{}
	for _, p := range paths {
		code, body := get(t, s, p)
		require.Equal(t, http.StatusOK, code, p)
		before[p] = body
	}

	n, err := pg.TrimRunLogs(ctx, runID)
	require.NoError(t, err)
	require.Positive(t, n)

	for _, p := range paths {
		code, body := get(t, s, p)
		require.Equal(t, http.StatusOK, code, p)
		assert.Equal(t, before[p], body, "response changed after trim: %s", p)
	}
}

// Timestamps: the JSON encoding of a time scanned from Postgres (microsecond
// precision) and one decoded from the archive ndjson written by the SAME
// archiver flow are identical, because seedParityRun builds the archive from
// TailLogs — the same source the pre-trim responses used. If this test flakes
// on timestamp formatting, compare decoded JSON instead of raw strings.

func TestLogEndpoints_TrimmedButObjectMissing_Returns503(t *testing.T) {
	s, pg := newTestServer(t)
	obj := objectstore.NewLocalObjectStore(t.TempDir())
	s.SetObjectStore(obj)
	runID := seedParityRun(t, pg, obj)
	ctx := context.Background()
	require.NoError(t, pg.CreateLogArchive(ctx, runID, runLogArchiveKey(runID), 1))
	require.NoError(t, pg.MarkRunFinished(ctx, runID, api.RunSucceeded))
	_, err := pg.TrimRunLogs(ctx, runID)
	require.NoError(t, err)
	require.NoError(t, obj.Delete(ctx, runLogArchiveKey(runID)))

	code, body := get(t, s, "/api/v1/runs/"+runID+"/logs/stats")
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Contains(t, body, "log archive unavailable")
}

// TestTrimThenRetention_DeletesArchiveObject: run retention on a TRIMMED run
// must still remove the archive object and the run row (spec: combined
// trim -> retention case).
func TestTrimThenRetention_DeletesArchiveObject(t *testing.T) {
	s, pg := newTestServer(t)
	obj := objectstore.NewLocalObjectStore(t.TempDir())
	s.SetObjectStore(obj)
	runID := seedParityRun(t, pg, obj)
	ctx := context.Background()
	require.NoError(t, pg.CreateLogArchive(ctx, runID, runLogArchiveKey(runID), 1))
	require.NoError(t, pg.MarkRunFinished(ctx, runID, api.RunSucceeded))
	_, err := pg.TrimRunLogs(ctx, runID)
	require.NoError(t, err)

	require.NoError(t, deleteRunEverywhere(ctx, pg, obj, runID))

	_, err = obj.Get(ctx, runLogArchiveKey(runID))
	assert.ErrorIs(t, err, objectstore.ErrNotFound, "archive object must be gone")
	arch, err := pg.GetLogArchive(ctx, runID)
	require.NoError(t, err)
	assert.Nil(t, arch, "archive record must cascade away with the run")
}

func TestLogEndpoints_UntrimmedRunUnaffected(t *testing.T) {
	// An archive record WITHOUT trimmed_at must keep serving from the DB
	// even if the object store is empty.
	s, pg := newTestServer(t)
	s.SetObjectStore(objectstore.NewLocalObjectStore(t.TempDir()))
	runID := seedParityRun(t, pg, objectstore.NewLocalObjectStore(t.TempDir()))
	require.NoError(t, pg.CreateLogArchive(context.Background(), runID, runLogArchiveKey(runID), 1))

	code, _ := get(t, s, "/api/v1/runs/"+runID+"/logs/stats")
	assert.Equal(t, http.StatusOK, code)
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/controller/ -run 'TestLogEndpoints' -v`
Expected: `TestLogEndpoints_IdenticalAfterTrim` FAILS after the trim (endpoints return empty/zero results from the now-empty DB); the 503 test fails (200 with empty stats); the untrimmed test passes already.

- [ ] **Step 3: Implement the fallback**

In `internal/controller/server.go`: add field `archLogs *archivedLogs` to the `Server` struct (next to `objStore`), and in `SetObjectStore` (around line 119, after `s.objStore = obj`) add:

```go
	s.archLogs = newArchivedLogs(obj)
```

In `internal/controller/archived_logs.go`, add:

```go
// logsTrimmed reports whether the run's logs rows were trimmed from the DB
// (archive record with trimmed_at set) and the archive reader is available,
// i.e. log reads must be served from the archive object.
func (s *Server) logsTrimmed(ctx context.Context, runID string) (bool, error) {
	if s.archLogs == nil {
		return false, nil
	}
	arch, err := s.store.GetLogArchive(ctx, runID)
	if err != nil {
		return false, err
	}
	return arch != nil && arch.TrimmedAt != nil, nil
}
```

In `internal/controller/api_runs.go`, insert the branch into each handler AFTER parameter validation and BEFORE the store call. `handleTailLogs` becomes:

```go
func (s *Server) handleTailLogs(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	afterStr := r.URL.Query().Get("after")
	var after int64
	_, _ = fmt.Sscanf(afterStr, "%d", &after)
	trimmed, err := s.logsTrimmed(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var lines []api.LogLine
	if trimmed {
		all, err := s.archLogs.lines(r.Context(), id)
		if err != nil {
			http.Error(w, "log archive unavailable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		lines = tailAfter(all, after, 1000)
	} else {
		lines, err = s.store.TailLogs(r.Context(), id, after, 1000)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if lines == nil {
		lines = []api.LogLine{}
	}
	writeJSON(w, http.StatusOK, lines)
}
```

`handleLogStats` — replace the single `CountLogs` call block with:

```go
	id := chi.URLParam(r, "id")
	trimmed, err := s.logsTrimmed(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var count, minSeq, maxSeq int64
	if trimmed {
		all, err := s.archLogs.lines(r.Context(), id)
		if err != nil {
			http.Error(w, "log archive unavailable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		count, minSeq, maxSeq = countArchivedLogs(all, steps)
	} else {
		count, minSeq, maxSeq, err = s.store.CountLogs(r.Context(), id, steps)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]int64{"count": count, "minSeq": minSeq, "maxSeq": maxSeq})
```

`handleLogRange` — replace the `ListLogsRange` call block (keep all offset/limit validation above it unchanged):

```go
	id := chi.URLParam(r, "id")
	trimmed, err := s.logsTrimmed(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var lines []api.LogLine
	if trimmed {
		all, err := s.archLogs.lines(r.Context(), id)
		if err != nil {
			http.Error(w, "log archive unavailable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		lines = archivedLogRange(all, steps, offset, limit)
	} else {
		lines, err = s.store.ListLogsRange(r.Context(), id, steps, offset, limit)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if lines == nil {
		lines = []api.LogLine{}
	}
	writeJSON(w, http.StatusOK, lines)
```

`handleLogSearch` — replace the `SearchLogs` call block (keep steps/q validation unchanged):

```go
	id := chi.URLParam(r, "id")
	trimmed, err := s.logsTrimmed(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var total int64
	var matches []store.LogSearchMatch
	if trimmed {
		all, err := s.archLogs.lines(r.Context(), id)
		if err != nil {
			http.Error(w, "log archive unavailable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		total, matches = searchArchivedLogs(all, steps, q, 1000)
	} else {
		total, matches, err = s.store.SearchLogs(r.Context(), id, steps, q, 1000)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if matches == nil {
		matches = []store.LogSearchMatch{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "matches": matches})
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/controller/ -run 'TestLogEndpoints|TestArchivedLogs' -v && go build ./...`
Expected: PASS. Also run the pre-existing log endpoint tests: `go test ./internal/controller/ -run 'TestAPI.*Log|LogRange|LogStats|LogSearch' -v` — all green (untrimmed paths unchanged).

- [ ] **Step 5: Commit**

```bash
git add internal/controller/server.go internal/controller/api_runs.go internal/controller/archived_logs.go internal/controller/api_runs_trimmed_test.go
git commit -m "feat(api): serve trimmed runs' log reads from the archive"
```

---

### Task 5: SSE backfill fallback

**Files:**
- Modify: `internal/controller/sse.go:58-83` (backfill block in `handleRunEvents`)
- Test: `internal/controller/sse_trimmed_test.go` (create)

**Interfaces:**
- Consumes: `s.logsTrimmed`, `s.archLogs.lines`, `tailRecent` (Tasks 3-4); existing `sseBackfillLimit`, `writeSSE`, `isTerminalStatus`.
- Produces: no new symbols. For a trimmed run the SSE backfill replays the archive tail (same cap + `truncated` event semantics); the rest of the stream flow is unchanged (a trimmed run is terminal, so the handler emits the status event and returns as today).

- [ ] **Step 1: Write the failing test**

Create `internal/controller/sse_trimmed_test.go`:

```go
package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSSE_TrimmedRunBackfillsFromArchive verifies a trimmed terminal run's
// SSE stream still replays its log lines (from the archive) followed by the
// terminal status event. The handler returns for terminal runs, so the
// request completes without needing to cancel the stream.
func TestSSE_TrimmedRunBackfillsFromArchive(t *testing.T) {
	s, pg := newTestServer(t)
	obj := objectstore.NewLocalObjectStore(t.TempDir())
	s.SetObjectStore(obj)
	runID := seedParityRun(t, pg, obj) // 6 lines, from archived_logs_test.go
	ctx := context.Background()
	require.NoError(t, pg.CreateLogArchive(ctx, runID, runLogArchiveKey(runID), 1))
	require.NoError(t, pg.MarkRunFinished(ctx, runID, api.RunSucceeded))
	_, err := pg.TrimRunLogs(ctx, runID)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+runID+"/events", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := rec.Body.String()
	assert.Equal(t, 6, strings.Count(body, `"type":"log"`), "all archived lines replayed")
	assert.Contains(t, body, `"type":"status"`)
	assert.Contains(t, body, `"Succeeded"`)
	assert.Contains(t, body, "Building target ALPHA")
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/controller/ -run TestSSE_TrimmedRunBackfillsFromArchive -v`
Expected: FAIL — 0 log events in the body (DB backfill is empty after trim); the status event is still present.

- [ ] **Step 3: Implement**

In `internal/controller/sse.go`, replace the backfill fetch (the `existing, err := s.store.TailLogsRecent(...)` line and its error check, lines 62-67) with:

```go
	var lastSeq int64
	var existing []api.LogLine
	trimmed, err := s.logsTrimmed(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if trimmed {
		all, aerr := s.archLogs.lines(r.Context(), id)
		if aerr != nil {
			http.Error(w, "log archive unavailable: "+aerr.Error(), http.StatusServiceUnavailable)
			return
		}
		existing = tailRecent(all, sseBackfillLimit+1)
	} else {
		existing, err = s.store.TailLogsRecent(r.Context(), id, sseBackfillLimit+1)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
```

The existing `if len(existing) > sseBackfillLimit { ... truncated ... }` block and the replay loop stay exactly as they are (both branches deliver at most `sseBackfillLimit+1` lines so the truncated-marker semantics are identical). Add the `api` import if the file does not already have it.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/controller/ -run 'TestSSE' -v`
Expected: PASS, including all pre-existing SSE tests.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/sse.go internal/controller/sse_trimmed_test.go
git commit -m "feat(sse): backfill trimmed runs from the log archive"
```

---

### Task 6: Log-trim sweeper

**Files:**
- Create: `internal/controller/log_trim.go`
- Modify: `internal/controller/audit_retention.go:11-15` and `internal/controller/run_retention.go:13-17` (advisory-key inventory comments: append `logTrim(0x6C74726D)`)
- Test: `internal/controller/log_trim_test.go` (create)

**Interfaces:**
- Consumes: `store.Store.ListTrimCandidates`, `TrimRunLogs`, `DeleteLogArchive` (Task 2); `AcquireAdvisoryLock`; `runLogArchiveKey`; `objectstore.ObjectStore.List`.
- Produces (Task 7 wires it): `RunLogTrim(ctx context.Context, st store.Store, obj objectstore.ObjectStore, interval time.Duration, trimDays int)` — returns immediately when `st == nil || obj == nil || trimDays <= 0`; `runLogTrimOnce(ctx, st, obj, trimDays)`; consts `logTrimLockKey = int64(0x6C74726D)`, `logTrimBatchSize = 100`.

- [ ] **Step 1: Write the failing tests**

Create `internal/controller/log_trim_test.go`:

```go
package controller

import (
	"context"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/store"
	"github.com/stretchr/testify/assert"
)

// fakeTrimStore is a minimal store.Store stand-in for the log-trim sweeper.
type fakeTrimStore struct {
	store.Store

	lockAcquired   bool
	candidates     [][]string // successive ListTrimCandidates results
	listCalls      int
	trimmed        []string
	deletedRecords []string
	trimErr        map[string]error
}

func (f *fakeTrimStore) AcquireAdvisoryLock(ctx context.Context, key int64) (func(), error) {
	if !f.lockAcquired {
		return nil, nil
	}
	return func() {}, nil
}

func (f *fakeTrimStore) ListTrimCandidates(ctx context.Context, cutoff time.Time, limit int) ([]string, error) {
	if f.listCalls >= len(f.candidates) {
		return nil, nil
	}
	ids := f.candidates[f.listCalls]
	f.listCalls++
	return ids, nil
}

func (f *fakeTrimStore) TrimRunLogs(ctx context.Context, runID string) (int64, error) {
	if err := f.trimErr[runID]; err != nil {
		return 0, err
	}
	f.trimmed = append(f.trimmed, runID)
	return 1, nil
}

func (f *fakeTrimStore) DeleteLogArchive(ctx context.Context, runID string) error {
	f.deletedRecords = append(f.deletedRecords, runID)
	return nil
}

// seedArchiveObject writes a placeholder archive object for runID so the
// sweeper's existence check passes.
func seedArchiveObject(t *testing.T, obj objectstore.ObjectStore, runID string) {
	t.Helper()
	if err := obj.Put(context.Background(), runLogArchiveKey(runID),
		bytes.NewReader([]byte("{}")), 2); err != nil {
		t.Fatal(err)
	}
}

func TestLogTrim_FollowerDoesNothing(t *testing.T) {
	st := &fakeTrimStore{lockAcquired: false, candidates: [][]string{{"r1"}}}
	runLogTrimOnce(context.Background(), st, objectstore.NewLocalObjectStore(t.TempDir()), 7)
	assert.Zero(t, st.listCalls)
	assert.Empty(t, st.trimmed)
}

func TestLogTrim_DisabledOrNoObjectStore(t *testing.T) {
	st := &fakeTrimStore{lockAcquired: true, candidates: [][]string{{"r1"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	RunLogTrim(ctx, st, objectstore.NewLocalObjectStore(t.TempDir()), 10*time.Millisecond, 0)
	assert.Zero(t, st.listCalls, "trimDays<=0 must disable the loop")
	RunLogTrim(ctx, st, nil, 10*time.Millisecond, 7)
	assert.Zero(t, st.listCalls, "nil object store must disable the loop")
}

func TestLogTrim_TrimsCandidatesWithExistingObjects(t *testing.T) {
	obj := objectstore.NewLocalObjectStore(t.TempDir())
	seedArchiveObject(t, obj, "r1")
	seedArchiveObject(t, obj, "r2")
	st := &fakeTrimStore{lockAcquired: true, candidates: [][]string{{"r1", "r2"}}}
	runLogTrimOnce(context.Background(), st, obj, 7)
	assert.Equal(t, []string{"r1", "r2"}, st.trimmed)
	assert.Empty(t, st.deletedRecords)
}

func TestLogTrim_MissingObjectDeletesStaleRecordAndSkipsTrim(t *testing.T) {
	obj := objectstore.NewLocalObjectStore(t.TempDir())
	seedArchiveObject(t, obj, "r2")
	st := &fakeTrimStore{lockAcquired: true, candidates: [][]string{{"r1", "r2"}}}
	runLogTrimOnce(context.Background(), st, obj, 7)
	assert.Equal(t, []string{"r2"}, st.trimmed, "r1 must not be trimmed")
	assert.Equal(t, []string{"r1"}, st.deletedRecords, "r1's stale record must be deleted so the archiver re-archives")
}

func TestLogTrim_ZeroProgressBatchStopsTick(t *testing.T) {
	// Every candidate fails to trim and the same full batch would repeat
	// forever; the tick must stop after one zero-progress batch.
	obj := objectstore.NewLocalObjectStore(t.TempDir())
	full := make([]string, logTrimBatchSize)
	trimErr := map[string]error{}
	for i := range full {
		id := fmt.Sprintf("run-%03d", i)
		full[i] = id
		seedArchiveObject(t, obj, id)
		trimErr[id] = errors.New("db down")
	}
	st := &fakeTrimStore{lockAcquired: true, candidates: [][]string{full, full}, trimErr: trimErr}
	runLogTrimOnce(context.Background(), st, obj, 7)
	assert.Equal(t, 1, st.listCalls)
	assert.Empty(t, st.trimmed)
}
```

Add `"bytes"`, `"errors"`, `"fmt"` to the imports.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/controller/ -run TestLogTrim -v`
Expected: compile errors (`undefined: runLogTrimOnce`, `RunLogTrim`, `logTrimBatchSize`).

- [ ] **Step 3: Implement the sweeper**

Create `internal/controller/log_trim.go`:

```go
package controller

import (
	"context"
	"log/slog"
	"time"

	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/eirueimi/unified-cd/internal/store"
)

// logTrimLockKey is the advisory lock key for the log-trim sweeper. Distinct
// from scheduler(0x65786364), approval(0x61707276), cache(0x63616368),
// logArchiver(0x6C6F6761), appSource(0x61707073), stuckRun(0x7374756B),
// auditRetention(0x61756474), runRetention(0x7272746E).
const logTrimLockKey = int64(0x6C74726D) // 'ltrm'

// logTrimBatchSize is how many trim candidates one sweep fetches at a time.
const logTrimBatchSize = 100

// RunLogTrim periodically deletes the DB logs rows of runs whose logs were
// archived more than trimDays ago, marking run_log_archives.trimmed_at so
// reads switch to the archive object (tiered log storage). Leader-elected via
// an advisory lock. trimDays <= 0 disables trimming; a nil object store also
// disables it (nothing was ever archived, and trimming would destroy the only
// copy). Returns immediately if st is nil.
func RunLogTrim(ctx context.Context, st store.Store, obj objectstore.ObjectStore, interval time.Duration, trimDays int) {
	if st == nil || obj == nil || trimDays <= 0 {
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
		runLogTrimOnce(ctx, st, obj, trimDays)
	}
}

func runLogTrimOnce(ctx context.Context, st store.Store, obj objectstore.ObjectStore, trimDays int) {
	release, err := st.AcquireAdvisoryLock(ctx, logTrimLockKey)
	if err != nil {
		slog.Warn("log trim lock", "error", err)
		return
	}
	if release == nil {
		return // Another replica is leader.
	}
	defer release()

	cutoff := time.Now().AddDate(0, 0, -trimDays)
	totalRuns := 0
	for {
		ids, err := st.ListTrimCandidates(ctx, cutoff, logTrimBatchSize)
		if err != nil {
			slog.Error("log trim: list candidates", "error", err)
			return
		}
		if len(ids) == 0 {
			break
		}
		progressed := 0
		for _, id := range ids {
			if ctx.Err() != nil {
				return // shutting down; the next leader resumes
			}
			// Trimming is irreversible: never trust the DB record alone,
			// verify the archive object actually exists first.
			keys, err := obj.List(ctx, runLogArchiveKey(id))
			if err != nil {
				slog.Warn("log trim: verify archive object", "run", id, "error", err)
				continue
			}
			if len(keys) == 0 {
				// Stale record with no object (e.g. bucket tampering).
				// Delete the record so ListRunsNeedingArchival picks the run
				// up again and the archiver re-creates the archive; trimming
				// then happens on a later sweep.
				slog.Warn("log trim: archive object missing, deleting stale record for re-archival", "run", id)
				if err := st.DeleteLogArchive(ctx, id); err != nil {
					slog.Warn("log trim: delete stale archive record", "run", id, "error", err)
					continue
				}
				progressed++ // the candidate left the result set
				continue
			}
			n, err := st.TrimRunLogs(ctx, id)
			if err != nil {
				slog.Warn("log trim: trim failed, will retry next tick", "run", id, "error", err)
				continue
			}
			progressed++
			totalRuns++
			slog.Debug("log trim: trimmed run logs", "run", id, "rows", n)
		}
		// Candidates that failed stay in the (oldest-first) result set, so a
		// batch with no progress means the next fetch would return the same
		// IDs — stop and let the next tick retry.
		if progressed == 0 || len(ids) < logTrimBatchSize {
			break
		}
	}
	if totalRuns > 0 {
		slog.Info("log trim: trimmed archived runs' DB log rows", "runs", totalRuns, "olderThan", cutoff)
	}
}
```

In `internal/controller/audit_retention.go` and `internal/controller/run_retention.go`, append `, logTrim(0x6C74726D)` to the key-inventory comments (keep the existing entries untouched).

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/controller/ -run TestLogTrim -v && go build ./...`
Expected: PASS (6 tests).

- [ ] **Step 5: Commit**

```bash
git add internal/controller/log_trim.go internal/controller/log_trim_test.go internal/controller/audit_retention.go internal/controller/run_retention.go
git commit -m "feat(controller): leader-elected log-trim sweeper"
```

---

### Task 7: Controller wiring (`--log-trim-days`)

**Files:**
- Modify: `cmd/controller/main.go` (resolver after `runRetentionDaysDefault`; flag under `runRetentionDays`; goroutine + warning after the `RunRunRetention` start)

**Interfaces:**
- Consumes: `controller.RunLogTrim` (Task 6); the `obj` variable (may be nil) and `st`; the existing `runRetentionDays` flag variable.
- Produces: flag `--log-trim-days`, env `UNIFIED_LOG_TRIM_DAYS`, default 0 = never trim; startup warning when `logTrimDays >= runRetentionDays` and both are > 0.

- [ ] **Step 1: Add the default resolver**

After `runRetentionDaysDefault` in `cmd/controller/main.go`:

```go
// logTrimDaysDefault resolves the --log-trim-days flag default from
// UNIFIED_LOG_TRIM_DAYS, falling back to 0 (never trim) when unset or
// invalid. Trimming deletes the DB copy of archived logs; reads then come
// from the object store, so this is opt-in like run retention.
func logTrimDaysDefault() int {
	v := os.Getenv("UNIFIED_LOG_TRIM_DAYS")
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		slog.Warn("invalid UNIFIED_LOG_TRIM_DAYS, never trimming", "value", v)
		return 0
	}
	return n
}
```

- [ ] **Step 2: Register the flag**

Directly under the `runRetentionDays` flag registration:

```go
	logTrimDays := flag.Int("log-trim-days", logTrimDaysDefault(), "days after archival to delete a run's DB log rows (reads switch to the archive); 0 = never trim (env: UNIFIED_LOG_TRIM_DAYS)")
```

- [ ] **Step 3: Start the sweeper**

Directly after the `go controller.RunRunRetention(...)` line:

```go
	if *logTrimDays > 0 {
		slog.Info("log trim enabled", "trimDays", *logTrimDays)
		if *runRetentionDays > 0 && *logTrimDays >= *runRetentionDays {
			slog.Warn("log trim will never fire: --log-trim-days >= --run-retention-days deletes runs before their logs qualify for trimming",
				"logTrimDays", *logTrimDays, "runRetentionDays", *runRetentionDays)
		}
	} else {
		slog.Info("log trim disabled (DB log rows kept forever)")
	}
	go controller.RunLogTrim(ctx, st, obj, time.Hour, *logTrimDays)
```

(`obj` may be nil — `RunLogTrim` disables itself in that case.)

- [ ] **Step 4: Build and smoke-check the flag**

Run: `go build ./... && go run ./cmd/controller --help 2>&1 | grep -A1 log-trim-days`
Expected: the flag and its help text are listed.

- [ ] **Step 5: Commit**

```bash
git add cmd/controller/main.go
git commit -m "feat(controller): --log-trim-days flag wires the log-trim sweeper"
```

---

### Task 8: Documentation + final sweep

**Files:**
- Modify: `docs/configuration.md` (mirror the `run-retention-days` entries added by PR #44 — flag block row + env table row)
- Modify: `docs/operations.md` (extend the "Run retention" paragraph into the tiering story)
- Modify: `docs/high-availability.md` (leader table row + advisory-key prose)

**Interfaces:**
- Consumes: final flag/env names from Task 7.
- Produces: user-facing docs only.

- [ ] **Step 1: `docs/configuration.md`**

Add directly after the `--run-retention-days` flag entry and `UNIFIED_RUN_RETENTION_DAYS` env row, mirroring their format:

> `--log-trim-days` / `UNIFIED_LOG_TRIM_DAYS` — days after a run's logs are archived before its database log rows are deleted (tiered log storage). Reads (WebUI viewer, CLI, SSE) transparently switch to the archived `logs.ndjson` in the object store; the first view of a trimmed run pays one object fetch. `0` (default) never trims. Requires an object store; must be smaller than `--run-retention-days` when both are set (otherwise retention deletes runs before trimming would fire — the controller logs a warning).

- [ ] **Step 2: `docs/operations.md`**

Extend the "Run retention." paragraph (added by PR #44) with a following paragraph:

> **Tiered log storage.** Even before run retention fires, `--log-trim-days` (env `UNIFIED_LOG_TRIM_DAYS`) can reclaim the largest table: N days after a run's logs are archived to the object store, an hourly leader-elected sweep deletes the run's `logs` rows and marks the archive record. All log reads for such runs are then served from the archive — the WebUI viewer, CLI, and SSE work unchanged, with a small first-view latency (one object fetch; parsed archives are cached in memory up to 128 MiB). The sweeper verifies the archive object exists before trimming and never trims unarchived runs. Typical setup: `--log-trim-days` a few days, `--run-retention-days` much larger.

- [ ] **Step 3: `docs/high-availability.md`**

Leader-election table row (matching format):

```
| Log trim (`RunLogTrim`) | advisory lock (`logTrimLockKey`) | Only the leader trims archived runs' log rows |
```

Append `/log-trim` to the advisory-key prose enumeration if it lists job names (same spot updated by PR #44).

- [ ] **Step 4: Hygiene sweep and full test run**

```bash
grep -rn "log-trim" docs/ README.md
grep -rn "LOG_TRIM" docs/
go build ./...
go test ./internal/controller/ ./internal/store/
```

Expected: docs hits only where intended; tests PASS (Docker required). Confirm with `git status` that only the three doc files changed in this task and that `docs/field-reference.md`, `schemas/`, `examples/`, `templates/` are untouched by the whole branch (`git diff --stat run-retention..HEAD -- docs/field-reference.md schemas/ examples/ templates/` is empty).

- [ ] **Step 5: Commit**

```bash
git add docs/configuration.md docs/operations.md docs/high-availability.md
git commit -m "docs: tiered log storage flag, operations guidance, HA leader table"
```
