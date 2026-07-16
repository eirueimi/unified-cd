# Streaming Uploads Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Eliminate whole-payload RAM buffering at the three agent-side upload sites so a large artifact/cache archive cannot OOM the agent.

**Architecture:** Add one exported helper `artifact.StreamTarZstd(path) io.ReadCloser` that runs `WriteTarZstd` in a goroutine writing into an `io.Pipe`, and rewrite the three sites to consume it as a stream: `artifact.Upload` and `cache.Save` pass the pipe reader to `objectstore.Put(..., -1)` (both stores already support unknown size); `client.UploadArtifact` sets it as the HTTP request body with no `Content-Length` (chunked). `cache.Save` captures the archive size via a `countingReader` and writes its `.meta` after the archive upload.

**Tech Stack:** Go, `io.Pipe`, `github.com/klauspost/compress/zstd`, `archive/tar`, minio-go v7 (via `internal/objectstore`), testify (`require`/`assert`), `net/http/httptest`.

## Global Constraints

- The helper is exported as `StreamTarZstd` and lives in `internal/artifact/store.go` beside `WriteTarZstd` (consumed by `internal/agent` and `internal/cache`).
- `objectstore.Put(ctx, key, content, size)` is called with `size = -1` for streamed archives; the `.meta` object keeps a known-length `bytes.NewReader` Put.
- `Meta.Size` in `cache.Save` is the count of bytes actually streamed to the store (a `countingReader.n`), written **after** the archive Put succeeds — preserving today's archive-then-meta ordering and the compensating-delete-on-`.meta`-failure cleanup.
- Every consumer `defer`s `Close()` on the reader returned by `StreamTarZstd` so the producer goroutine cannot leak on an early abort.
- `client.UploadArtifact` must NOT set `req.ContentLength` (so `net/http` sends chunked). No controller-side change (`api_artifacts.go` already reads `r.ContentLength == -1` into `Put`).
- Behavior-preserving: stored objects and downloads are byte-identical; existing round-trip tests stay green. No change to `objectstore.ObjectStore`, the download/restore paths, the DSL, the API, or the schema.
- Full `go test ./...` green before finishing (known transient `internal/cli` flake: isolate-rerun 3×).

---

### Task 1: `StreamTarZstd` helper + convert `artifact.Upload`

**Files:**
- Modify: `internal/artifact/store.go` (add `StreamTarZstd`; rewrite `Upload`)
- Test: `internal/artifact/store_test.go`

**Interfaces:**
- Consumes: existing `WriteTarZstd(w io.Writer, path string) error`, `ExtractTarZstd(r io.Reader, dest string) error`, `objectstore.ObjectStore` (`Put(ctx, key string, content io.Reader, size int64) error`), `objectstore.NewLocalObjectStore(dir)`.
- Produces: `StreamTarZstd(path string) io.ReadCloser` (used by Tasks 2 and 3). `Upload` keeps its signature `Upload(ctx context.Context, store objectstore.ObjectStore, runID, name, dir string) error`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/artifact/store_test.go`:

```go
func TestStreamTarZstd_RoundTripEqualsBuffered(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(src, "sub"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("world"), 0o600))

	// Stream and extract; the tree must round-trip.
	rc := StreamTarZstd(src)
	defer rc.Close()
	dest := t.TempDir()
	require.NoError(t, ExtractTarZstd(rc, dest))
	got, err := os.ReadFile(filepath.Join(dest, "sub", "b.txt"))
	require.NoError(t, err)
	assert.Equal(t, "world", string(got))
	gotA, err := os.ReadFile(filepath.Join(dest, "a.txt"))
	require.NoError(t, err)
	assert.Equal(t, "hello", string(gotA))
}

func TestStreamTarZstd_SingleFile(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "out.txt"), []byte("data"), 0o600))

	rc := StreamTarZstd(filepath.Join(src, "out.txt"))
	defer rc.Close()
	dest := t.TempDir()
	require.NoError(t, ExtractTarZstd(rc, dest))
	got, err := os.ReadFile(filepath.Join(dest, "out.txt"))
	require.NoError(t, err)
	assert.Equal(t, "data", string(got))
}

func TestStreamTarZstd_ProducerErrorPropagates(t *testing.T) {
	// A path that does not exist makes WriteTarZstd's WalkDir fail; the error
	// must surface to the consumer as a read error, and the read must not hang.
	rc := StreamTarZstd(filepath.Join(t.TempDir(), "does-not-exist"))
	defer rc.Close()
	done := make(chan error, 1)
	go func() { _, err := io.ReadAll(rc); done <- err }()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("read hung waiting for producer error")
	}
}

func TestStreamTarZstd_CloseUnblocksProducer(t *testing.T) {
	// A source with many files keeps the producer writing; Close must unblock
	// it (io.Pipe is unbuffered) and a subsequent Read returns ErrClosedPipe.
	src := t.TempDir()
	for i := 0; i < 200; i++ {
		require.NoError(t, os.WriteFile(
			filepath.Join(src, "f"+strconv.Itoa(i)+".txt"),
			bytes.Repeat([]byte("x"), 1024), 0o600))
	}
	rc := StreamTarZstd(src)
	buf := make([]byte, 16)
	_, _ = rc.Read(buf) // consume one chunk; producer is now mid-archive
	require.NoError(t, rc.Close())
	_, err := rc.Read(buf)
	assert.ErrorIs(t, err, io.ErrClosedPipe)
}

// sizeSpyStore records the size argument passed to Put and delegates to an
// embedded real store so Get/List/Delete still work.
type sizeSpyStore struct {
	objectstore.ObjectStore
	lastSize int64
}

func (s *sizeSpyStore) Put(ctx context.Context, key string, content io.Reader, size int64) error {
	s.lastSize = size
	return s.ObjectStore.Put(ctx, key, content, size)
}

func TestUpload_StreamsWithUnknownSize(t *testing.T) {
	// Upload must stream: it passes size == -1 to Put (not the buffered length).
	spy := &sizeSpyStore{ObjectStore: objectstore.NewLocalObjectStore(t.TempDir())}
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o600))

	require.NoError(t, Upload(context.Background(), spy, "run1", "art1", src))
	assert.Equal(t, int64(-1), spy.lastSize, "Upload must stream with unknown size (-1)")
}
```

Ensure the test file imports: `bytes`, `context`, `io`, `os`, `path/filepath`, `strconv`, `time`, `objectstore "github.com/eirueimi/unified-cd/internal/objectstore"`, and testify `assert`/`require` (add any missing).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/artifact/ -run TestStreamTarZstd -v`
Expected: FAIL — `undefined: StreamTarZstd`.

- [ ] **Step 3: Add `StreamTarZstd`**

In `internal/artifact/store.go`, add after `WriteTarZstd` (the `io` import already exists):

```go
// StreamTarZstd returns an io.ReadCloser that yields the tar+zstd archive of
// path, produced by a background goroutine writing into an io.Pipe, so the
// whole archive is never held in memory. A production error from WriteTarZstd
// surfaces to the consumer as a read error (via pw.CloseWithError). Callers
// MUST Close the returned reader — even on an early abort such as an HTTP 4xx
// or a failed Put — so the producer goroutine cannot leak: Close delivers
// io.ErrClosedPipe to the producer's next Write, unwinding it.
func StreamTarZstd(path string) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		// CloseWithError(nil) behaves like Close(): the reader sees a clean io.EOF.
		pw.CloseWithError(WriteTarZstd(pw, path))
	}()
	return pr
}
```

- [ ] **Step 4: Run tests — helper passes, Upload-streaming test still red**

Run: `go test ./internal/artifact/ -run 'TestStreamTarZstd|TestUpload_StreamsWithUnknownSize' -v`
Expected: the four `TestStreamTarZstd_*` PASS; `TestUpload_StreamsWithUnknownSize` FAILS (current `Upload` passes the buffered length, not `-1`). This confirms the streaming assertion is a genuine red before Step 5.

- [ ] **Step 5: Convert `Upload` to stream**

In `internal/artifact/store.go`, replace the body of `Upload` (currently buffers into `bytes.Buffer`):

```go
// Upload tars+zstds dir and stores it at artifacts/{runID}/{name}.tar.gz,
// streaming the archive so it is never fully buffered in memory.
func Upload(ctx context.Context, store objectstore.ObjectStore, runID, name, dir string) error {
	key, err := artifactKey(runID, name)
	if err != nil {
		return fmt.Errorf("upload artifact: %w", err)
	}
	body := StreamTarZstd(dir)
	defer body.Close()
	return store.Put(ctx, key, body, -1)
}
```

If `bytes` is now unused in `store.go`, remove it from the import block.

- [ ] **Step 6: Run the package tests — all green**

Run: `go test ./internal/artifact/ -count=1`
Expected: PASS — `TestUpload_StreamsWithUnknownSize` now passes (`-1`), and `TestUploadDownload_RoundTrip`, `TestUploadDownload_SingleFile`, `TestUpload_UsesArtifactKeyLayout`, and the traversal tests all pass unchanged.

- [ ] **Step 7: Commit**

```bash
git add internal/artifact/store.go internal/artifact/store_test.go
git commit -m "feat(artifact): StreamTarZstd pipe helper; stream artifact.Upload"
```

---

### Task 2: Stream `cache.Save` with `countingReader`

**Files:**
- Modify: `internal/cache/cache.go` (rewrite `Save`; add `countingReader`)
- Test: `internal/cache/cache_test.go`

**Interfaces:**
- Consumes: `artifact.StreamTarZstd(path string) io.ReadCloser` (Task 1); `objectstore.ObjectStore.Put`; existing `objectKey`, `Meta`.
- Produces: `Save(ctx, store, path, key string, ttlDays int) error` (unchanged signature); private `countingReader`.

- [ ] **Step 1: Write the failing test**

Add to `internal/cache/cache_test.go`. There is already a `putFailingStore` (fails `.meta` Puts) and `TestSave_MetaFailureDeletesArchive`. Add a size-recording spy so the streaming change (archive Put with `-1`) is a genuine red, and assert `Meta.Size` equals the streamed length:

```go
// archiveSizeSpyStore records the size passed to Put for the .tar.zst archive
// key (ignoring the small .meta Put) and delegates to an embedded real store.
type archiveSizeSpyStore struct {
	objectstore.ObjectStore
	archiveSize int64
}

func (s *archiveSizeSpyStore) Put(ctx context.Context, key string, content io.Reader, size int64) error {
	if strings.HasSuffix(key, ".tar.zst") {
		s.archiveSize = size
	}
	return s.ObjectStore.Put(ctx, key, content, size)
}

func TestSave_StreamsArchiveAndCountsMetaSize(t *testing.T) {
	ctx := context.Background()
	spy := &archiveSizeSpyStore{ObjectStore: objectstore.NewLocalObjectStore(t.TempDir())}
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "f.txt"), bytes.Repeat([]byte("z"), 4096), 0o600))

	require.NoError(t, cache.Save(ctx, spy, src, "mykey", 7))

	// The archive Put must be streamed (unknown size), NOT the buffered length.
	assert.Equal(t, int64(-1), spy.archiveSize, "archive must be streamed with size -1")

	// Independently compute the archive length; Meta.Size must equal it.
	var buf bytes.Buffer
	require.NoError(t, artifact.WriteTarZstd(&buf, src))
	wantSize := int64(buf.Len())

	oKey := "caches/" + base64.RawURLEncoding.EncodeToString(func() []byte {
		h := sha256.Sum256([]byte("mykey"))
		return h[:]
	}())
	rc, err := spy.Get(ctx, oKey+".meta")
	require.NoError(t, err)
	defer rc.Close()
	var m cache.Meta
	require.NoError(t, json.NewDecoder(rc).Decode(&m))
	assert.Equal(t, wantSize, m.Size)
	assert.Equal(t, "mykey", m.OriginalKey)
}
```

Ensure the test imports: `bytes`, `context`, `crypto/sha256`, `encoding/base64`, `encoding/json`, `io`, `os`, `path/filepath`, `strings`, plus `artifact "github.com/eirueimi/unified-cd/internal/artifact"` and the existing `objectstore`, testify imports.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/cache/ -run TestSave_StreamsArchiveAndCountsMetaSize -v`
Expected: FAIL — the current `Save` buffers the archive and passes the buffered length to `Put`, so `spy.archiveSize` is a positive number, not `-1`. (The `Meta.Size` assertion coincidentally matches under old code; the `-1` assertion is the genuine red.)

- [ ] **Step 3: Rewrite `Save` to stream + count**

In `internal/cache/cache.go`, replace `Save` (lines that buffer into `bytes.Buffer`) with:

```go
// Save compresses path as tar+zstd and stores it in store under key, streaming
// the archive so it is never fully buffered in memory. A metadata object is
// stored alongside with TTL of ttlDays days; its Size is the number of bytes
// streamed to the store, captured during the upload.
func Save(ctx context.Context, store objectstore.ObjectStore, path, key string, ttlDays int) error {
	oKey := objectKey(key)

	body := artifact.StreamTarZstd(path)
	counter := &countingReader{r: body}
	if err := store.Put(ctx, oKey+".tar.zst", counter, -1); err != nil {
		body.Close()
		return fmt.Errorf("put archive: %w", err)
	}
	body.Close()

	meta := Meta{
		OriginalKey: key,
		ExpiresAt:   time.Now().Add(time.Duration(ttlDays) * 24 * time.Hour),
		Size:        counter.n,
	}
	metaData, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal meta: %w", err)
	}
	if err := store.Put(ctx, oKey+".meta", bytes.NewReader(metaData), int64(len(metaData))); err != nil {
		// The archive object was already written; without its .meta it is
		// invisible to both lookup and GC (which iterate .meta only), so it
		// would leak forever. Compensate best-effort, like the log archiver
		// does on CreateLogArchive failure.
		if derr := store.Delete(ctx, oKey+".tar.zst"); derr != nil {
			slog.Warn("cache save: cleanup of orphaned archive failed", "key", oKey, "error", derr)
		}
		return fmt.Errorf("put meta: %w", err)
	}
	return nil
}

// countingReader counts the bytes read through it — i.e. the bytes the object
// store consumed from the archive stream — so Save can record Meta.Size
// without buffering the whole archive to measure it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
```

Add the import `artifact "github.com/eirueimi/unified-cd/internal/artifact"` to `cache.go`. Keep `bytes` (still used for the `.meta` `bytes.NewReader`). `io` is already imported.

- [ ] **Step 4: Run the cache tests to verify pass**

Run: `go test ./internal/cache/ -count=1`
Expected: PASS — `TestSave_StreamsArchiveAndCountsMetaSize` (now `-1`), `TestCache_SaveAndRestore_RoundTrip`, `TestSave_MetaFailureDeletesArchive`, `TestCache_DeleteExpired_RemovesOldKeepsNew`, and the restore/fallback tests all pass. The compensating-delete test still passes because the archive Put runs before the failing `.meta` Put.

- [ ] **Step 5: Commit**

```bash
git add internal/cache/cache.go internal/cache/cache_test.go
git commit -m "feat(cache): stream Save to the object store; count Meta.Size"
```

---

### Task 3: Stream `client.UploadArtifact` (chunked)

**Files:**
- Modify: `internal/agent/client.go` (rewrite `UploadArtifact`)
- Test: `internal/agent/client_test.go`

**Interfaces:**
- Consumes: `artifact.StreamTarZstd` (Task 1; `client.go` already imports `internal/artifact`); `NewClient(baseURL, token string) *Client`; `artifact.ExtractTarZstd`.
- Produces: `UploadArtifact(ctx context.Context, runID, name, path string) error` (unchanged signature).

- [ ] **Step 1: Write the failing test**

Add to `internal/agent/client_test.go` (uses the same `httptest` pattern as the existing tests):

```go
func TestClient_UploadArtifact_StreamsChunked(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte("payload"), 0o600))

	var gotLen int64
	var extracted string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotLen = r.ContentLength // -1 for chunked (no Content-Length)
		dest := t.TempDir()
		if err := artifact.ExtractTarZstd(r.Body, dest); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		b, _ := os.ReadFile(filepath.Join(dest, "a.txt"))
		extracted = string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "t")
	require.NoError(t, c.UploadArtifact(t.Context(), "run1", "art1", src))
	assert.Equal(t, int64(-1), gotLen, "body must be chunked (no Content-Length)")
	assert.Equal(t, "payload", extracted)
}

func TestClient_UploadArtifact_HTTPErrorSurfaces(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.txt"), []byte("x"), 0o600))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "t")
	err := c.UploadArtifact(t.Context(), "run1", "art1", src)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "502")
}
```

Ensure the test imports include: `io`, `os`, `path/filepath`, `net/http`, `net/http/httptest`, `artifact "github.com/eirueimi/unified-cd/internal/artifact"`, and testify. (Most are already present from other tests in the file; add `os`, `path/filepath`, `artifact` if missing.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/agent/ -run TestClient_UploadArtifact -v`
Expected: FAIL — the current implementation sets `req.ContentLength`, so `gotLen` is the buffer length, not `-1`.

- [ ] **Step 3: Rewrite `UploadArtifact` to stream**

In `internal/agent/client.go`, replace `UploadArtifact` (the `bytes.Buffer` + `bytes.NewReader` + `req.ContentLength` version) with:

```go
// UploadArtifact archives path as tar+zstd and uploads it to the master
// server, streaming the archive as a chunked request body so it is never
// fully buffered in memory. No Content-Length is set — net/http uses chunked
// transfer-encoding, and the controller reads r.Body straight into the object
// store (r.ContentLength == -1 → a multipart streaming Put).
func (c *Client) UploadArtifact(ctx context.Context, runID, name, path string) error {
	body := artifact.StreamTarZstd(path)
	defer body.Close()

	url := c.base + fmt.Sprintf("/api/v1/runs/%s/artifacts/%s", runID, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, body)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upload artifact http %d: %s", resp.StatusCode, string(b))
	}
	return nil
}
```

If `bytes` is now unused in `client.go`, remove it from the import block. (Check: `client.go:53` also uses `bytes.NewReader` elsewhere — if so, keep the import.)

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/agent/ -run TestClient_UploadArtifact -v`
Expected: PASS (both new tests).

- [ ] **Step 5: Run the agent package tests**

Run: `go test ./internal/agent/ -count=1`
Expected: PASS — no regression in the existing artifact/cache/upload agent tests.

- [ ] **Step 6: Commit**

```bash
git add internal/agent/client.go internal/agent/client_test.go
git commit -m "feat(agent): stream UploadArtifact as a chunked request body"
```

---

### Task 4: Docs + full sweep

**Files:**
- Modify: `docs/operations.md`

- [ ] **Step 1: Document the streaming behavior**

In `docs/operations.md`, find the artifact/cache operational section (search for "artifact" / "cache" / "workspace hygiene"). Integrate a short note matching the surrounding style, covering:
- Artifact and cache uploads now stream (tar+zstd is produced and sent incrementally), so the agent's peak memory during an upload is bounded (a compression window plus one object-store multipart part) rather than the full archive size — large artifacts/caches no longer risk OOMing the agent.
- The agent→controller artifact PUT uses chunked transfer-encoding (no `Content-Length`); a length-sensitive proxy in front of the controller must allow chunked request bodies.
- Trade-off: because the upload body is streamed (not rewindable), a mid-upload network failure fails that upload rather than being transparently retried; the step's normal retry/`continueOnError` semantics still apply.

- [ ] **Step 2: Full sweep**

Run each; all must be clean:
- `go build ./...`
- `go generate ./...` then `git status --porcelain` — MUST be drift-free. (Known Windows stat-cache artifact: if `git status` flags a generated file, verify with `git diff` that it is byte-identical and `git checkout` it; do not commit generated changes.)
- `go vet ./internal/... ./cmd/...`
- `go test ./... -count=1` — full suite. Known transient `internal/cli` flake (and occasional ubuntu dockertest setup flake): if only such a test fails, isolate-rerun that package up to 3× (`go test ./internal/cli/ -count=1`) to confirm it is the flake. Any other failure is a real regression — do not commit.

- [ ] **Step 3: Commit**

```bash
git add docs/operations.md
git commit -m "docs: streaming artifact/cache uploads (bounded agent memory)"
```

---

## Notes for the executor
- Order: Task 1 first (Tasks 2 and 3 both consume `StreamTarZstd`); Tasks 2 and 3 are independent of each other; Task 4 last.
- Verify every referenced signature against the code before writing tests: `WriteTarZstd`/`ExtractTarZstd` (`internal/artifact`), `objectstore.Put` and `NewLocalObjectStore`, `NewClient`, the existing `putFailingStore`/`errKeyStore` and `Meta` in `internal/cache/cache_test.go`, and whether `bytes` remains used in each edited file before removing the import.
- Do NOT change `objectstore`, the controller receive handler, the download/restore paths, the DSL, the API, or the schema — the spec scopes those out.
- Full-suite gate before finishing (merge discipline): wait for green locally, and after the PR is up, wait for CI before any admin merge.
