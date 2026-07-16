# Streaming Uploads — Eliminate RAM-Buffering on the Agent Upload Path — Design

**Status:** Approved 2026-07-16 (Branch D of the A→B→C→D program; chunked transfer-encoding for the agent→controller PUT confirmed acceptable — no length-sensitive proxy in front of the controller).

**Goal:** Remove whole-payload RAM buffering at the three agent-side upload sites so a large artifact or cache archive cannot OOM the agent. Peak memory becomes bounded (the zstd compression window plus a single multipart part buffer) regardless of archive size, instead of scaling with the compressed archive.

## Problem (verified against code)

Three sites build the entire tar+zstd archive into a `bytes.Buffer` in memory before handing it off:

1. `internal/agent/client.go` `UploadArtifact` — host agent → controller over HTTP. `WriteTarZstd(&buf, path)` then `http.NewRequestWithContext(..., bytes.NewReader(buf.Bytes()))` with `req.ContentLength = int64(buf.Len())`.
2. `internal/cache/cache.go` `Save` — writes directly to `objectstore.Put`. Callers: host agent (`internal/agent/backend_host.go`, via `b.a.CacheStore`) and the k8s `cmd/unified-sidecar/run.go`. `WriteTarZstd(&buf, path)` then `store.Put(ctx, oKey+".tar.zst", bytes.NewReader(archiveData), int64(len(archiveData)))`.
3. `internal/artifact/store.go` `Upload` — writes directly to `objectstore.Put`. Caller: `cmd/unified-sidecar/run.go`. `WriteTarZstd(&buf, dir)` then `store.Put(ctx, key, bytes.NewReader(buf.Bytes()), int64(buf.Len()))`.

The receive/download paths are already streaming and are **out of scope**: the controller handler `internal/controller/api_artifacts.go` reads `size := r.ContentLength` and passes `r.Body` straight into `objStore.Put`; `Download`/`DownloadArtifact`/`cache.Restore` already stream via `ExtractTarZstd`/`extract`.

### Object-store size semantics (verified)

`objectstore.ObjectStore.Put(ctx, key, content io.Reader, size int64)` already supports unknown size:
- `LocalObjectStore.Put` ignores `size` entirely (`_ int64`) — it is a plain `io.Copy(f, content)`.
- `S3ObjectStore.Put` calls minio-go v7 `PutObject(ctx, bucket, key, content, size, opts)`. With `size = -1`, minio-go streams the object as a multipart upload, buffering one part at a time (~16 MiB default part size) rather than the whole object.

So passing `-1` yields bounded-memory streaming on both stores today; no `ObjectStore` interface or implementation change is required.

## Core mechanism (shared helper)

Add one exported helper beside `WriteTarZstd` in `internal/artifact/store.go` (exported because `internal/agent` and `internal/cache` both consume it):

```go
// StreamTarZstd returns an io.ReadCloser that yields the tar+zstd archive of
// path, produced by a background goroutine writing into an io.Pipe. The
// consumer (an HTTP request body or objectstore.Put) reads the archive as it
// is produced, so the whole archive is never held in memory.
//
// A production error from WriteTarZstd surfaces to the consumer as a read
// error (via pw.CloseWithError), preserving the existing error contract at
// every call site. Callers MUST Close the returned reader (even on an early
// abort such as an HTTP 4xx) so the producer goroutine cannot leak; Close
// unblocks a producer still trying to write by delivering io.ErrClosedPipe to
// its next Write.
func streamTarZstd(path string) io.ReadCloser
```

Implementation shape:
```go
func StreamTarZstd(path string) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		// CloseWithError(nil) is equivalent to Close() → clean io.EOF for the reader.
		pw.CloseWithError(WriteTarZstd(pw, path))
	}()
	return pr
}
```
`io.PipeReader.Close()` causes any in-flight or subsequent `pw.Write` in `WriteTarZstd` to return `io.ErrClosedPipe`, so the goroutine unwinds and returns; there is no separate cancellation channel to manage.

## Section 1 — Site 1: `client.UploadArtifact` (agent → controller HTTP)

Replace the buffer with the streaming body and drop the explicit length:
```go
func (c *Client) UploadArtifact(ctx context.Context, runID, name, path string) error {
	body := artifact.StreamTarZstd(path) // exported for cross-package use
	defer body.Close()

	url := c.base + fmt.Sprintf("/api/v1/runs/%s/artifacts/%s", runID, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, body)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/octet-stream")
	// No req.ContentLength → net/http uses chunked transfer-encoding.

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
With no `ContentLength`, `net/http` sends `Transfer-Encoding: chunked`. The controller reads `r.ContentLength == -1` and passes it to `objStore.Put`, which becomes an S3 multipart streaming upload (or a plain streaming copy for `LocalObjectStore`). No controller-side change. Because the helper is consumed cross-package, it is exported as `StreamTarZstd`.

Note: `c.http.Do` fully consumes and closes `req.Body`; the extra `defer body.Close()` is a defensive no-op-after-close that also covers the error paths where the request is never sent.

## Section 2 — Sites 2 & 3: `cache.Save` and `artifact.Upload`

### `artifact.Upload`
```go
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

### `cache.Save`
`Meta.Size` is informational — `DeleteExpired` and `findBestMatch` read only `ExpiresAt` and `OriginalKey`, and `Restore` reads neither. So the exact archive size is not needed before the Put; capture it during the stream with a counting wrapper and write `.meta` after the archive Put succeeds (unchanged ordering; the compensating-delete-on-`.meta`-failure path is preserved):
```go
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
		Size:        counter.n, // bytes streamed to the store
	}
	metaData, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("marshal meta: %w", err)
	}
	if err := store.Put(ctx, oKey+".meta", bytes.NewReader(metaData), int64(len(metaData))); err != nil {
		if derr := store.Delete(ctx, oKey+".tar.zst"); derr != nil {
			slog.Warn("cache save: cleanup of orphaned archive failed", "key", oKey, "error", derr)
		}
		return fmt.Errorf("put meta: %w", err)
	}
	return nil
}

// countingReader counts bytes read through it (the bytes the store consumed).
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
The `.meta` object stays small and fixed-size, so it keeps a known-length `bytes.NewReader` Put. `countingReader` lives in `cache.go` (it is cache-specific; `artifact.Upload` needs no size).

## Section 3 — Error handling & lifecycle

- **Producer error:** `WriteTarZstd` failure (bad walk, read error mid-file) → `pw.CloseWithError(err)` → the consumer's next `Read` returns `err`, so `c.http.Do` / `store.Put` returns it. Every call site's error string wrapping is unchanged.
- **Consumer abort / early return:** every site `defer body.Close()`s the reader. If the controller returns a 4xx before the body is fully read, or `store.Put` fails early, `Close()` delivers `io.ErrClosedPipe` to the blocked producer `Write`, unwinding the goroutine — no leak.
- **No transparent retry of a partial upload:** a streamed (unseekable) body means a mid-upload network failure fails the upload rather than being silently retried from the start, unlike the old seekable `bytes.Reader`. This matches current behavior in effect (uploads already surface-and-fail), and is documented as a known trade-off. No new retry logic is added (YAGNI).

## Section 4 — Testing

- **Round-trip equivalence:** `StreamTarZstd(dir)` piped through `ExtractTarZstd` reproduces the source tree byte-for-byte, and its bytes equal those of the old buffered `WriteTarZstd(&buf, dir)` for the same input (both a directory tree and a single file, matching `WriteTarZstd`'s dual behavior).
- **Producer error propagation:** a path that fails mid-walk (e.g. a file removed / unreadable after the walk starts, or a nonexistent path) surfaces as a non-nil error from the consumer and the test does not hang (guard with a timeout).
- **`cache.Save` size + compensation:** `Meta.Size` equals the streamed archive length (assert against a direct `WriteTarZstd` length for the same input); when the `.meta` Put fails (fake store erroring on `.meta` keys), the archive object is compensating-deleted.
- **Producer unblocks on early Close (no leak):** read one chunk from `StreamTarZstd` for a multi-file source (so the producer is mid-archive and blocked on an unbuffered-pipe `Write`), then `Close()` the reader; assert `Close()` returns nil and a subsequent `Read` returns `io.ErrClosedPipe` — proving the producer is unblocked rather than deadlocked. The test uses only the public reader API (no test-only hook on the helper).
- **Existing integration tests stay green:** the artifact upload→download and cache save→restore round-trips (`internal/agent`, `internal/artifact`, `internal/cache`, `internal/controller/api_artifacts_test.go`) pass unchanged — behavior is preserved end to end.
- No dedicated peak-RAM assertion: the memory bound is structural (no full-archive allocation exists after the change), not something a unit test measures reliably.

## Components / files

- `internal/artifact/store.go` — add exported `StreamTarZstd(path string) io.ReadCloser`; rewrite `Upload` to stream via it with `Put(..., -1)`.
- `internal/agent/client.go` — `UploadArtifact` streams `req.Body`, drops `req.ContentLength`.
- `internal/cache/cache.go` — `Save` streams the archive via `StreamTarZstd` + `countingReader`, `Put(..., -1)`, writes `.meta` (with counted `Size`) after; add `countingReader`.
- Tests: `internal/artifact/store_test.go`, `internal/agent/client_test.go` (or the existing agent artifact test), `internal/cache/cache_test.go`.
- Docs: `docs/operations.md` — note that artifact/cache uploads now stream (bounded agent memory) and that a mid-upload network failure fails the upload without transparent retry.

## Backward / forward compatibility

- Wire: the agent→controller PUT switches from `Content-Length` to chunked. The controller already handles `ContentLength == -1`. A new agent against an old controller still works iff that controller reads `r.Body` without requiring a length — which it already does (this is the same handler). No API, DSL, schema, or object-layout change.
- Object store: `-1` is already accepted by both implementations; stored objects are byte-identical to before, so existing archives and downloads are unaffected.

## Out of scope

- Download/restore paths (already streaming).
- Configurable multipart part size / tuning (YAGNI).
- Controller receive path (already streams `r.Body` → `Put`).
- Any change to `objectstore.ObjectStore`.
