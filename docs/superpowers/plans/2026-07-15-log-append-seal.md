# Log Append Seal Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Silently drop agent log appends once a run's logs are archived (sealed), and 404 artifact uploads for nonexistent runs — closing the ghost-row / trim-blocking / orphan-object holes with zero agent changes.

**Architecture:** The seal lives in the `AppendLog` INSERT itself (`INSERT ... SELECT ... WHERE NOT EXISTS (run_log_archives record)`) — one statement, no extra round trip on the hottest write path; `(0, nil)` becomes the documented "dropped" return. Handlers warn-log drops and still return 204 (mirroring `handleAgentFinishRun`'s `alreadyFinalized` philosophy). Artifact upload gains a `GetRun` existence check.

**Tech Stack:** Go, pgx (via `internal/store`), testify, `store.NewTestPostgres` (dockerized Postgres).

**Spec:** `docs/superpowers/specs/2026-07-15-log-append-seal-design.md`

## Global Constraints

- All code, comments, commit messages, docs in **English** (AGENTS.md). No PII.
- Work in worktree `../unified-cd-log-seal`, branch `log-seal` (base main). Never commit from the main tree.
- `AppendLog` keeps its `(int64, error)` signature; `(0, nil)` = dropped (sealed). Real seqs start at 1.
- Drop responses stay **204**; observability is `slog.Warn` with the exact message `dropping log line for sealed run` (single) / `dropping log lines for sealed run` (bulk, once per request).
- Artifact upload: `store.ErrRunNotFound` → **404**; all other run states accepted unchanged.
- `CreateLogArchive` signature on main is `(ctx, runID, objectKey string, sizeBytes, lineCount, maxSeq int64)`.
- Store integration tests need Docker (skip under `-short`). `docs/field-reference.md` is generated — untouched.

---

### Task 1: Sealed `AppendLog` (store)

**Files:**
- Modify: `internal/store/postgres.go:758-772` (`AppendLog`)
- Modify: `internal/store/store.go:158` (interface doc comment)
- Test: `internal/store/postgres_log_seal_test.go` (create)

**Interfaces:**
- Consumes: existing `run_log_archives` table; `CreateLogArchive(ctx, runID, key, sizeBytes, lineCount, maxSeq)`.
- Produces: `AppendLog` unchanged signature; **`(0, nil)` = dropped because the run's logs are archived**. Task 2's handlers branch on `seq == 0`.

- [ ] **Step 1: Write the failing test**

Create `internal/store/postgres_log_seal_test.go`:

```go
package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPostgres_AppendLog_SealedAfterArchive: once a run_log_archives record
// exists, AppendLog drops the line — (0, nil), nothing stored, no error.
func TestPostgres_AppendLog_SealedAfterArchive(t *testing.T) {
	pg := NewTestPostgres(t)
	ctx := context.Background()
	_, err := pg.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)

	// Unsealed: insert works and returns a real seq (sequences start at 1).
	seq, err := pg.AppendLog(ctx, run.ID, 0, "stdout", time.Now(), "before seal")
	require.NoError(t, err)
	assert.Positive(t, seq)

	// Seal by creating the archive record (values irrelevant to the seal).
	require.NoError(t, pg.CreateLogArchive(ctx, run.ID, "runs/"+run.ID+"/logs.ndjson", 1, 1, seq))

	seq, err = pg.AppendLog(ctx, run.ID, 0, "stdout", time.Now(), "after seal")
	require.NoError(t, err, "sealed append must not be an error")
	assert.Zero(t, seq, "sealed append must report the dropped sentinel")

	count, _, _, err := pg.CountLogs(ctx, run.ID, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count, "the sealed line must not be stored")

	// A different, unsealed run is unaffected.
	other, err := pg.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	seq, err = pg.AppendLog(ctx, other.ID, 0, "stdout", time.Now(), "unsealed run")
	require.NoError(t, err)
	assert.Positive(t, seq)
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd /path/to/unified-cd-log-seal && go test ./internal/store/ -run TestPostgres_AppendLog_SealedAfterArchive -v`
Expected: FAIL — the post-seal append currently succeeds (`seq` positive, count 2).

- [ ] **Step 3: Implement the guarded insert**

Replace `AppendLog` in `internal/store/postgres.go` (the file already imports `errors` and `pgx`):

```go
// AppendLog stores one log line and notifies SSE listeners. Once the run's
// logs are archived (a run_log_archives record exists) the run is SEALED:
// the line is silently dropped and AppendLog returns (0, nil) — lines
// arriving after archival would never be captured by the archive, would
// block log trimming, and would be invisible ghost rows after a trim. The
// guard lives in the INSERT itself so the hot append path costs no extra
// round trip. Real seqs start at 1, so 0 is unambiguous.
func (p *Postgres) AppendLog(ctx context.Context, runID string, stepIndex int, stream string, ts time.Time, line string) (int64, error) {
	const q = `
		INSERT INTO logs(run_id, step_index, stream, ts, line)
		SELECT $1::uuid, $2::int, $3::text, $4::timestamptz, $5::text
		WHERE NOT EXISTS (SELECT 1 FROM run_log_archives WHERE run_id = $1::uuid)
		RETURNING seq;
	`
	var seq int64
	err := p.pool.QueryRow(ctx, q, runID, stepIndex, stream, ts, line).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil // sealed: dropped
	}
	if err != nil {
		return 0, err
	}
	// notify listeners of the new log entry (skipped for dropped lines, so
	// SSE clients stay consistent with what readers can see)
	_, _ = p.pool.Exec(ctx, "SELECT pg_notify($1, $2)", "log_appended:"+runID, fmt.Sprintf("%d", seq))
	return seq, nil
}
```

In `internal/store/store.go`, replace the bare `AppendLog(...)` interface line with a commented one:

```go
	// AppendLog stores one log line. Returns (0, nil) when the line was
	// DROPPED because the run's logs are already archived (sealed) — see
	// the Postgres implementation for rationale.
	AppendLog(ctx context.Context, runID string, stepIndex int, stream string, ts time.Time, line string) (int64, error)
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/store/ -run TestPostgres_AppendLog_SealedAfterArchive -v && go build ./...`
Then confirm no existing test relied on appending after archival: `go test ./internal/store/ ./internal/controller/ -count=1` (Docker required).
Expected: PASS everywhere. If a pre-existing test breaks because it appends after `CreateLogArchive`, reorder that test's seeding (append first, archive second) — that ordering is what the archiver does in production; report it in your summary rather than weakening assertions.

- [ ] **Step 5: Commit**

```bash
git add internal/store/postgres.go internal/store/store.go internal/store/postgres_log_seal_test.go
git commit -m "feat(store): seal log appends once the run's logs are archived"
```

---

### Task 2: Handler drop accounting + artifact upload existence check

**Files:**
- Modify: `internal/controller/api_agent.go:451-466` (`handleAgentLogAppend`), `:559-575` (`handleAgentLogBulk`)
- Modify: `internal/controller/api_artifacts.go:18-36` (`handleArtifactUpload`)
- Test: `internal/controller/api_agent_seal_test.go` (create)

**Interfaces:**
- Consumes: `AppendLog` returning `(0, nil)` when sealed (Task 1); `GetRun(ctx, id)` returning `store.ErrRunNotFound`; `newTestServer(t) (*Server, store.Store)` (agent bearer token literal `agent-secret`, human token `secret`).
- Produces: no new symbols. Sealed appends → 204 + `slog.Warn`; artifact upload to a nonexistent run → 404.

- [ ] **Step 1: Write the failing tests**

Create `internal/controller/api_agent_seal_test.go`:

```go
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/api"
	"github.com/eirueimi/unified-cd/internal/objectstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sealRun creates a finished run with one stored log line and an archive
// record, i.e. a run whose logs are sealed.
func sealRun(t *testing.T, st store.Store) string {
	t.Helper()
	ctx := context.Background()
	_, err := st.UpsertJob(ctx, "j", "unified-cd/v1", []byte(`{}`))
	require.NoError(t, err)
	run, err := st.CreateRun(ctx, "j", nil, []byte(`{}`), nil, nil, "")
	require.NoError(t, err)
	seq, err := st.AppendLog(ctx, run.ID, 0, "stdout", time.Now(), "real line")
	require.NoError(t, err)
	require.NoError(t, st.MarkRunFinished(ctx, run.ID, api.RunSucceeded))
	require.NoError(t, st.CreateLogArchive(ctx, run.ID, "runs/"+run.ID+"/logs.ndjson", 1, 1, seq))
	return run.ID
}

func TestAgentLogAppend_SealedRunDropsLine(t *testing.T) {
	s, st := newTestServer(t)
	runID := sealRun(t, st)

	body, _ := json.Marshal(api.LogAppendRequest{RunID: runID, StepIndex: 0, Stream: "stdout", Line: "late line"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/logs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	count, _, _, err := st.CountLogs(context.Background(), runID, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count, "late line must not be stored")
}

func TestAgentLogBulk_SealedRunDropsLines(t *testing.T) {
	s, st := newTestServer(t)
	runID := sealRun(t, st)

	lines := []api.LogAppendRequest{
		{RunID: runID, StepIndex: 1, Stream: "stdout", Line: "late 1"},
		{RunID: runID, StepIndex: 1, Stream: "stderr", Line: "late 2"},
	}
	body, _ := json.Marshal(lines)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/a1/runs/"+runID+"/steps/1/logs/bulk", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	count, _, _, err := st.CountLogs(context.Background(), runID, nil)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)
}

func TestArtifactUpload_NonexistentRun404(t *testing.T) {
	s, _ := newTestServer(t)
	s.SetObjectStore(objectstore.NewLocalObjectStore(t.TempDir()))

	req := httptest.NewRequest(http.MethodPut,
		"/api/v1/runs/00000000-0000-0000-0000-000000000000/artifacts/out",
		strings.NewReader("data"))
	req.Header.Set("Authorization", "Bearer agent-secret")
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}
```

Add the `store` import (`github.com/eirueimi/unified-cd/internal/store`) — the compiler will confirm what's needed.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/controller/ -run 'TestAgentLog.*Sealed|TestArtifactUpload_NonexistentRun404' -v`
Expected: the two append tests PASS already at the HTTP level (204) but... they assert `count == 1`, which passes only after Task 1 — if Task 1 is merged they pass fully; the artifact test FAILS (204 instead of 404, and the orphan object is created). At minimum the artifact test must fail before Step 3.

- [ ] **Step 3: Implement**

`internal/controller/api_agent.go` — `handleAgentLogAppend`, replace the `AppendLog` call block:

```go
	seq, err := s.store.AppendLog(r.Context(), req.RunID, req.StepIndex, req.Stream, req.Timestamp, req.Line)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if seq == 0 {
		// Sealed (logs already archived): dropped. 204 keeps unmodified
		// agents from retry-storming — same philosophy as FinishRun's
		// alreadyFinalized response.
		slog.Warn("dropping log line for sealed run", "run", req.RunID)
	}
	w.WriteHeader(http.StatusNoContent)
```

`handleAgentLogBulk` — replace the loop body and add drop accounting around it:

```go
	dropped := 0
	var droppedRun string
	for _, req := range lines {
		if req.Timestamp.IsZero() {
			req.Timestamp = time.Now().UTC()
		}
		seq, err := s.store.AppendLog(r.Context(), req.RunID, req.StepIndex, req.Stream, req.Timestamp, req.Line)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if seq == 0 {
			dropped++
			droppedRun = req.RunID
		}
	}
	if dropped > 0 {
		slog.Warn("dropping log lines for sealed run", "run", droppedRun, "dropped", dropped)
	}
	w.WriteHeader(http.StatusNoContent)
```

Add `"log/slog"` to api_agent.go's imports if absent.

`internal/controller/api_artifacts.go` — `handleArtifactUpload`, insert after the `objStore == nil` check and before computing `key`:

```go
	runID := chi.URLParam(r, "runID")
	if _, err := s.store.GetRun(r.Context(), runID); err != nil {
		if errors.Is(err, store.ErrRunNotFound) {
			// A late upload for a deleted run would create an orphaned
			// object nothing ever cleans up (deleteRunEverywhere already
			// ran its prefix delete). Terminal-but-existing runs are still
			// accepted: their objects stay referenced and are removed with
			// the run.
			http.Error(w, "run not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
```

(The existing `runID := chi.URLParam(r, "runID")` a few lines below becomes redundant — remove the duplicate.) Add the `store` import (`github.com/eirueimi/unified-cd/internal/store`).

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/controller/ -run 'TestAgentLog|TestArtifact' -count=1 -v && go build ./...`
Expected: PASS, including all pre-existing artifact and agent-log tests (the upload round-trip tests use runs that exist... if a pre-existing artifact test uploads for a run that was never created in the DB, seed that run in the test rather than weakening the 404 — check `api_artifacts_test.go`'s `run1` usage and add the missing `UpsertJob`/`CreateRun` seeding where needed; `newArtifactTestServer` has a nil store, whose tests may need the seeded `newTestServer` variant instead. Report whatever you had to touch.)

- [ ] **Step 5: Commit**

```bash
git add internal/controller/api_agent.go internal/controller/api_artifacts.go internal/controller/api_agent_seal_test.go
git commit -m "feat(api): drop sealed-run log appends; 404 artifact uploads for missing runs"
```

---

### Task 3: Documentation + sweep

**Files:**
- Modify: `docs/troubleshooting.md` (new symptom entry)
- Modify: `docs/operations.md` (one sentence in the tiered-log paragraph)

**Interfaces:**
- Consumes: the exact warn strings from Task 2.
- Produces: docs only.

- [ ] **Step 1: `docs/troubleshooting.md`**

Mirror the file's existing symptom-entry format, with this content:

> **Symptom:** controller logs `dropping log line for sealed run` (or `dropping log lines for sealed run`).
> **Meaning:** an agent sent log lines for a run whose logs were already archived (~30 s after the run finished). The archive is the sealed source of truth, so the lines were discarded — storing them would make the run untrimmable and, after a trim, invisible anyway.
> **Common causes:** an agent retrying after a network partition (the run was finalized by the stuck-run reaper meanwhile); teardown/buffer flushes arriving later than the archiver delay. Occasional occurrences are expected noise; sustained streams for the same run indicate a stuck agent worth restarting.

- [ ] **Step 2: `docs/operations.md`**

Append one sentence to the tiered-log-storage paragraph:

> Log lines arriving after a run's archive was written are discarded with a controller warning (`dropping log line for sealed run`) — see Troubleshooting.

- [ ] **Step 3: Sweep and full test run**

```bash
grep -rn "sealed run" docs/ internal/ | grep -v _test.go
go build ./... && go test ./internal/controller/ ./internal/store/ -count=1
git status   # only the two doc files modified in this task
```

Expected: warn strings in code and docs match exactly; tests PASS.

- [ ] **Step 4: Commit**

```bash
git add docs/troubleshooting.md docs/operations.md
git commit -m "docs: sealed-run log drop troubleshooting entry"
```
