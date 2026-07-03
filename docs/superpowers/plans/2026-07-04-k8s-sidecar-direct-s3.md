# k8s Sidecar Direct-to-S3 (unified-sidecar) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make k8s `cache` restore/save work and unify k8s cache + artifacts on a single `unified-sidecar` Go binary that talks direct to S3, with credentials from an operator-provisioned Secret.

**Architecture:** A new `unified-sidecar` binary reuses `internal/cache` and `internal/artifact` against the object store, reading S3 config from env. The k8s agent injects the binary's image as the `unified-artifact` sidecar (S3 creds via `envFrom` Secret) and execs it via a new no-shell `ExecStepArgv`. The orchestrator gains a `cache` branch (restore at step time, save deferred to end-of-run) and its artifact branches switch from `tar|zstd|curl → controller` to `unified-sidecar artifact … → S3`. The controller and standard agent are untouched.

**Tech Stack:** Go, cobra, klauspost/compress (zstd), archive/tar, minio-go, client-go (exec), corev1.

## Global Constraints

- Module path: `github.com/eirueimi/unified-cd`.
- Object-store key layouts are FIXED: cache = `caches/<sha>.tar.zst` + `caches/<sha>.meta` (owned by `internal/cache`); artifacts = `artifacts/{runID}/{name}.tar.gz`.
- Cache is **best-effort**: a cache restore miss OR error must NEVER fail the step; a cache save error must NEVER fail the run (log only). Parity with the standard agent (`internal/agent/agent.go` cache handling).
- Artifacts are **not** best-effort: a failed artifact upload/download fails the step (matches current k8s behavior — `recordFailure`).
- The sidecar is invoked via **argv** (no `bash -lc`); values (key/path/name/runID) are passed as separate argv elements, never interpolated into a shell string.
- S3 env var names match the controller/agent: `UNIFIED_S3_ENDPOINT`, `UNIFIED_S3_BUCKET`, `UNIFIED_S3_KEY`, `UNIFIED_S3_SECRET`, `UNIFIED_S3_USE_SSL` (bool), `UNIFIED_S3_REGION`.
- Reserved sidecar container name: `unified-artifact` (`artifactSidecarName`).
- Every task must leave `go build ./...` green and its own tests passing. `SidecarSpec` field changes are ADDITIVE until Task 8 (keep `Server`/`Token` so intermediate tasks compile and the existing artifact path keeps working).

---

### Task 1: `objectstore.S3ConfigFromEnv`

**Files:**
- Create: `internal/objectstore/env.go`
- Create: `internal/objectstore/env_test.go`

**Interfaces:**
- Produces: `func S3ConfigFromEnv() (S3Config, error)` — reads the `UNIFIED_S3_*` env vars; errors if any of endpoint/bucket/key/secret is empty.

- [ ] **Step 1: Write the failing test**

Create `internal/objectstore/env_test.go`:

```go
package objectstore

import "testing"

func TestS3ConfigFromEnv_OK(t *testing.T) {
	t.Setenv("UNIFIED_S3_ENDPOINT", "s3:9000")
	t.Setenv("UNIFIED_S3_BUCKET", "b")
	t.Setenv("UNIFIED_S3_KEY", "k")
	t.Setenv("UNIFIED_S3_SECRET", "s")
	t.Setenv("UNIFIED_S3_USE_SSL", "true")
	t.Setenv("UNIFIED_S3_REGION", "us-east-1")
	cfg, err := S3ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoint != "s3:9000" || cfg.Bucket != "b" || cfg.AccessKeyID != "k" || cfg.SecretAccessKey != "s" || !cfg.UseSSL || cfg.Region != "us-east-1" {
		t.Fatalf("got %+v", cfg)
	}
}

func TestS3ConfigFromEnv_MissingRequired(t *testing.T) {
	t.Setenv("UNIFIED_S3_ENDPOINT", "s3:9000")
	t.Setenv("UNIFIED_S3_BUCKET", "")
	t.Setenv("UNIFIED_S3_KEY", "k")
	t.Setenv("UNIFIED_S3_SECRET", "s")
	if _, err := S3ConfigFromEnv(); err == nil {
		t.Fatal("expected error for missing bucket")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/objectstore/ -run TestS3ConfigFromEnv`
Expected: FAIL — `undefined: S3ConfigFromEnv`.

- [ ] **Step 3: Write the implementation**

Create `internal/objectstore/env.go`:

```go
package objectstore

import (
	"fmt"
	"os"
	"strings"
)

// S3ConfigFromEnv builds an S3Config from the UNIFIED_S3_* environment variables.
// Endpoint, bucket, key, and secret are required. UseSSL parses UNIFIED_S3_USE_SSL
// ("true"/"1" ⇒ true); Region is optional.
func S3ConfigFromEnv() (S3Config, error) {
	cfg := S3Config{
		Endpoint:        os.Getenv("UNIFIED_S3_ENDPOINT"),
		Bucket:          os.Getenv("UNIFIED_S3_BUCKET"),
		AccessKeyID:     os.Getenv("UNIFIED_S3_KEY"),
		SecretAccessKey: os.Getenv("UNIFIED_S3_SECRET"),
		Region:          os.Getenv("UNIFIED_S3_REGION"),
	}
	switch strings.ToLower(os.Getenv("UNIFIED_S3_USE_SSL")) {
	case "true", "1", "yes":
		cfg.UseSSL = true
	}
	var missing []string
	if cfg.Endpoint == "" {
		missing = append(missing, "UNIFIED_S3_ENDPOINT")
	}
	if cfg.Bucket == "" {
		missing = append(missing, "UNIFIED_S3_BUCKET")
	}
	if cfg.AccessKeyID == "" {
		missing = append(missing, "UNIFIED_S3_KEY")
	}
	if cfg.SecretAccessKey == "" {
		missing = append(missing, "UNIFIED_S3_SECRET")
	}
	if len(missing) > 0 {
		return cfg, fmt.Errorf("missing required S3 env vars: %s", strings.Join(missing, ", "))
	}
	return cfg, nil
}
```

- [ ] **Step 4: Run tests, build**

Run: `go test ./internal/objectstore/ -run TestS3ConfigFromEnv && go build ./...`
Expected: PASS, clean.

- [ ] **Step 5: Commit**

```bash
git add internal/objectstore/env.go internal/objectstore/env_test.go
git commit -m "feat(objectstore): S3ConfigFromEnv reads UNIFIED_S3_* env vars"
```

---

### Task 2: `internal/artifact` store-level Upload/Download + shared tar helper

**Files:**
- Create: `internal/artifact/store.go`
- Create: `internal/artifact/store_test.go`
- Modify: `internal/cache/cache.go` (factor its tar+zstd-directory walk into the shared helper)

**Interfaces:**
- Produces:
  - `func WriteTarZstd(w io.Writer, dir string) error` — walk `dir`, stream it as tar+zstd into `w` (the walk currently inline in `cache.Save`).
  - `func Upload(ctx context.Context, store objectstore.ObjectStore, runID, name, dir string) error` — tar+zstd `dir` → `store.Put("artifacts/{runID}/{name}.tar.gz", …)`.
  - `func Download(ctx context.Context, store objectstore.ObjectStore, runID, name, dest string) error` — `store.Get(...)` → `ExtractTarZstd`.
- Consumes: `objectstore.ObjectStore`, existing `ExtractTarZstd`.

- [ ] **Step 1: Write the failing test**

Create `internal/artifact/store_test.go`:

```go
package artifact

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/eirueimi/unified-cd/internal/objectstore"
)

func TestUploadDownload_RoundTrip(t *testing.T) {
	store := objectstore.NewLocalObjectStore(t.TempDir())
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("world"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := Upload(ctx, store, "run1", "build", src); err != nil {
		t.Fatalf("upload: %v", err)
	}
	dest := t.TempDir()
	if err := Download(ctx, store, "run1", "build", dest); err != nil {
		t.Fatalf("download: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "a.txt"))
	if err != nil || string(got) != "hello" {
		t.Fatalf("a.txt = %q, %v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(dest, "sub", "b.txt"))
	if err != nil || string(got) != "world" {
		t.Fatalf("sub/b.txt = %q, %v", got, err)
	}
}

func TestUpload_UsesArtifactKeyLayout(t *testing.T) {
	dir := t.TempDir()
	store := objectstore.NewLocalObjectStore(dir)
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "x"), []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Upload(context.Background(), store, "runXYZ", "myart", src); err != nil {
		t.Fatal(err)
	}
	// LocalObjectStore writes files under baseDir/<key>; assert the key path exists.
	if _, err := os.Stat(filepath.Join(dir, "artifacts", "runXYZ", "myart.tar.gz")); err != nil {
		t.Fatalf("expected artifacts/runXYZ/myart.tar.gz: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/artifact/ -run 'TestUpload'`
Expected: FAIL — `undefined: Upload` / `Download`.

- [ ] **Step 3: Write the implementation**

Create `internal/artifact/store.go`:

```go
package artifact

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
	"github.com/eirueimi/unified-cd/internal/objectstore"
)

// WriteTarZstd walks dir and streams its contents to w as a tar+zstd archive.
func WriteTarZstd(w io.Writer, dir string) error {
	enc, err := zstd.NewWriter(w)
	if err != nil {
		return fmt.Errorf("zstd writer: %w", err)
	}
	tw := tar.NewWriter(enc)
	if err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !d.IsDir() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(tw, f)
			return err
		}
		return nil
	}); err != nil {
		return fmt.Errorf("tar walk %q: %w", dir, err)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("tar close: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("zstd close: %w", err)
	}
	return nil
}

func artifactKey(runID, name string) string {
	return fmt.Sprintf("artifacts/%s/%s.tar.gz", runID, name)
}

// Upload tars+zstds dir and stores it at artifacts/{runID}/{name}.tar.gz.
func Upload(ctx context.Context, store objectstore.ObjectStore, runID, name, dir string) error {
	var buf bytes.Buffer
	if err := WriteTarZstd(&buf, dir); err != nil {
		return err
	}
	return store.Put(ctx, artifactKey(runID, name), bytes.NewReader(buf.Bytes()), int64(buf.Len()))
}

// Download fetches artifacts/{runID}/{name}.tar.gz and extracts it into dest.
func Download(ctx context.Context, store objectstore.ObjectStore, runID, name, dest string) error {
	rc, err := store.Get(ctx, artifactKey(runID, name))
	if err != nil {
		return fmt.Errorf("get artifact: %w", err)
	}
	defer rc.Close()
	if dest == "" {
		dest = "."
	}
	return ExtractTarZstd(rc, dest)
}
```

- [ ] **Step 4: Refactor `cache.Save` to reuse `WriteTarZstd`**

In `internal/cache/cache.go`, replace the inline tar+zstd walk in `Save` (the block building `buf` via `zstd.NewWriter` + `filepath.WalkDir` + `tw.Close`/`enc.Close`, roughly lines 42-88) with:

```go
	var buf bytes.Buffer
	if err := artifact.WriteTarZstd(&buf, path); err != nil {
		return err
	}
	archiveData := buf.Bytes()
```

Add the import `"github.com/eirueimi/unified-cd/internal/artifact"`. Remove now-unused imports from `cache.go` (`archive/tar`, `io/fs`, and possibly `io`/`os`/`filepath` if no longer used elsewhere in the file — `extract` still uses several, so verify each with a search before removing; let the compiler guide you). Keep `bytes`, `zstd` (still used by `extract`), `sha256`, `json`, etc.

**Import-cycle check:** `internal/artifact` must NOT import `internal/cache`. It does not (Task 2 Step 3 imports only stdlib + zstd + objectstore). `internal/cache` importing `internal/artifact` is therefore safe.

- [ ] **Step 5: Run tests, build**

Run: `go test ./internal/artifact/ ./internal/cache/ && go build ./...`
Expected: PASS (artifact round-trip + key layout; existing cache tests still green), clean.

- [ ] **Step 6: Commit**

```bash
git add internal/artifact/store.go internal/artifact/store_test.go internal/cache/cache.go
git commit -m "feat(artifact): store-level Upload/Download + shared WriteTarZstd (reused by cache.Save)"
```

---

### Task 3: `unified-sidecar` binary

**Files:**
- Create: `cmd/unified-sidecar/main.go`
- Create: `cmd/unified-sidecar/run.go` (testable dispatch given an object store)
- Create: `cmd/unified-sidecar/run_test.go`

**Interfaces:**
- Produces (package `main`): `func run(ctx context.Context, store objectstore.ObjectStore, args []string, stderr io.Writer) (exitCode int)` — dispatches subcommands against the provided store. `main` builds the store from `S3ConfigFromEnv` and calls `run`.
- Subcommands and argv:
  - `cache restore --key K [--restore-key R]... --path P` → `cache.Restore`; **exit 0 on miss/any error** (log to stderr).
  - `cache save --key K --ttl-days N --path P` → `cache.Save`; **exit 0 on error** (log).
  - `artifact upload --run R --name N --path P` → `artifact.Upload`; **exit 1 on error**.
  - `artifact download --run R --name N --dest D` → `artifact.Download`; **exit 1 on error**.

- [ ] **Step 1: Write the failing test**

Create `cmd/unified-sidecar/run_test.go`:

```go
package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/eirueimi/unified-cd/internal/objectstore"
)

func TestRun_CacheSaveThenRestore(t *testing.T) {
	store := objectstore.NewLocalObjectStore(t.TempDir())
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "dep.txt"), []byte("cached"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if code := run(ctx, store, []string{"cache", "save", "--key", "k1", "--ttl-days", "7", "--path", src}, io.Discard); code != 0 {
		t.Fatalf("cache save exit=%d", code)
	}
	dest := t.TempDir()
	if code := run(ctx, store, []string{"cache", "restore", "--key", "k1", "--path", dest}, io.Discard); code != 0 {
		t.Fatalf("cache restore exit=%d", code)
	}
	got, err := os.ReadFile(filepath.Join(dest, "dep.txt"))
	if err != nil || string(got) != "cached" {
		t.Fatalf("dep.txt = %q, %v", got, err)
	}
}

func TestRun_CacheRestoreMiss_Exit0(t *testing.T) {
	store := objectstore.NewLocalObjectStore(t.TempDir())
	dest := t.TempDir()
	if code := run(context.Background(), store, []string{"cache", "restore", "--key", "nope", "--path", dest}, io.Discard); code != 0 {
		t.Fatalf("cache restore miss should exit 0, got %d", code)
	}
}

func TestRun_ArtifactUploadDownload(t *testing.T) {
	store := objectstore.NewLocalObjectStore(t.TempDir())
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "out.txt"), []byte("art"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if code := run(ctx, store, []string{"artifact", "upload", "--run", "r1", "--name", "build", "--path", src}, io.Discard); code != 0 {
		t.Fatalf("artifact upload exit=%d", code)
	}
	dest := t.TempDir()
	if code := run(ctx, store, []string{"artifact", "download", "--run", "r1", "--name", "build", "--dest", dest}, io.Discard); code != 0 {
		t.Fatalf("artifact download exit=%d", code)
	}
	got, err := os.ReadFile(filepath.Join(dest, "out.txt"))
	if err != nil || string(got) != "art" {
		t.Fatalf("out.txt = %q, %v", got, err)
	}
}

func TestRun_ArtifactDownloadMissing_Exit1(t *testing.T) {
	store := objectstore.NewLocalObjectStore(t.TempDir())
	if code := run(context.Background(), store, []string{"artifact", "download", "--run", "r1", "--name", "nope", "--dest", t.TempDir()}, io.Discard); code == 0 {
		t.Fatal("missing artifact download should exit non-zero")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/unified-sidecar/`
Expected: FAIL — no such package / `undefined: run`.

- [ ] **Step 3: Write `run.go`**

Create `cmd/unified-sidecar/run.go`. Use the standard library `flag` package per-subcommand (avoids a cobra dependency in a tiny binary; if the repo strongly prefers cobra, mirror the CLI style instead — but `flag` keeps the static binary minimal):

```go
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/eirueimi/unified-cd/internal/artifact"
	"github.com/eirueimi/unified-cd/internal/cache"
	"github.com/eirueimi/unified-cd/internal/objectstore"
)

// stringSlice collects repeated flag values (e.g. --restore-key a --restore-key b).
type stringSlice []string

func (s *stringSlice) String() string     { return fmt.Sprint([]string(*s)) }
func (s *stringSlice) Set(v string) error { *s = append(*s, v); return nil }

// run dispatches the sidecar subcommands against store. Cache operations are
// best-effort (always exit 0); artifact operations exit non-zero on failure.
func run(ctx context.Context, store objectstore.ObjectStore, args []string, stderr io.Writer) int {
	if len(args) < 2 {
		fmt.Fprintln(stderr, "usage: unified-sidecar <cache|artifact> <subcommand> [flags]")
		return 2
	}
	group, sub, rest := args[0], args[1], args[2:]
	switch group {
	case "cache":
		return runCache(ctx, store, sub, rest, stderr)
	case "artifact":
		return runArtifact(ctx, store, sub, rest, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command group %q\n", group)
		return 2
	}
}

func runCache(ctx context.Context, store objectstore.ObjectStore, sub string, args []string, stderr io.Writer) int {
	switch sub {
	case "restore":
		fs := flag.NewFlagSet("cache restore", flag.ContinueOnError)
		fs.SetOutput(stderr)
		key := fs.String("key", "", "cache key")
		path := fs.String("path", "", "destination path")
		var restoreKeys stringSlice
		fs.Var(&restoreKeys, "restore-key", "fallback restore key prefix (repeatable)")
		if err := fs.Parse(args); err != nil {
			return 2
		}
		hit, err := cache.Restore(ctx, store, *path, *key, restoreKeys)
		if err != nil && !errors.Is(err, cache.ErrCacheMiss) {
			fmt.Fprintf(stderr, "cache restore error (ignored): %v\n", err)
		} else if hit {
			fmt.Fprintf(stderr, "cache hit: %s\n", *key)
		} else {
			fmt.Fprintf(stderr, "cache miss: %s\n", *key)
		}
		return 0 // best-effort: never fail the step
	case "save":
		fs := flag.NewFlagSet("cache save", flag.ContinueOnError)
		fs.SetOutput(stderr)
		key := fs.String("key", "", "cache key")
		path := fs.String("path", "", "source path")
		ttlDays := fs.Int("ttl-days", 30, "TTL in days")
		if err := fs.Parse(args); err != nil {
			return 2
		}
		if err := cache.Save(ctx, store, *path, *key, *ttlDays); err != nil {
			fmt.Fprintf(stderr, "cache save error (ignored): %v\n", err)
		} else {
			fmt.Fprintf(stderr, "cache saved: %s\n", *key)
		}
		return 0 // best-effort
	default:
		fmt.Fprintf(stderr, "unknown cache subcommand %q\n", sub)
		return 2
	}
}

func runArtifact(ctx context.Context, store objectstore.ObjectStore, sub string, args []string, stderr io.Writer) int {
	switch sub {
	case "upload":
		fs := flag.NewFlagSet("artifact upload", flag.ContinueOnError)
		fs.SetOutput(stderr)
		runID := fs.String("run", "", "run ID")
		name := fs.String("name", "", "artifact name")
		path := fs.String("path", "", "source path")
		if err := fs.Parse(args); err != nil {
			return 2
		}
		if err := artifact.Upload(ctx, store, *runID, *name, *path); err != nil {
			fmt.Fprintf(stderr, "artifact upload failed: %v\n", err)
			return 1
		}
		return 0
	case "download":
		fs := flag.NewFlagSet("artifact download", flag.ContinueOnError)
		fs.SetOutput(stderr)
		runID := fs.String("run", "", "run ID")
		name := fs.String("name", "", "artifact name")
		dest := fs.String("dest", ".", "destination directory")
		if err := fs.Parse(args); err != nil {
			return 2
		}
		if err := artifact.Download(ctx, store, *runID, *name, *dest); err != nil {
			fmt.Fprintf(stderr, "artifact download failed: %v\n", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(stderr, "unknown artifact subcommand %q\n", sub)
		return 2
	}
}
```

- [ ] **Step 4: Write `main.go`**

Create `cmd/unified-sidecar/main.go`:

```go
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/eirueimi/unified-cd/internal/objectstore"
)

func main() {
	ctx := context.Background()
	cfg, err := objectstore.S3ConfigFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	store, err := objectstore.NewS3ObjectStore(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "s3 store: %v\n", err)
		os.Exit(2)
	}
	os.Exit(run(ctx, store, os.Args[1:], os.Stderr))
}
```

- [ ] **Step 5: Run tests, build**

Run: `go test ./cmd/unified-sidecar/ && go build ./...`
Expected: PASS (4 tests), clean.

- [ ] **Step 6: Commit**

```bash
git add cmd/unified-sidecar/
git commit -m "feat(unified-sidecar): cache + artifact subcommands over direct-S3 object store"
```

---

### Task 4: Sidecar image — static binary, no shell

**Files:**
- Modify: `docker/artifact-sidecar.Dockerfile`

- [ ] **Step 1: Rewrite the Dockerfile**

Replace the contents of `docker/artifact-sidecar.Dockerfile` with a multi-stage build producing a minimal image containing only the static `unified-sidecar` binary + CA certs. Match the Go builder image/version used by the other Dockerfiles in `docker/` (inspect `docker/agent.Dockerfile` for the exact `golang:...` base and any module-cache flags, and mirror them):

```dockerfile
# Build the unified-sidecar binary (cache + artifact transfer, direct to S3).
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/unified-sidecar ./cmd/unified-sidecar

# Minimal runtime: just the static binary + CA certs. The k8s agent execs the
# binary via argv (no shell), so no bash/curl/tar/zstd are needed.
FROM gcr.io/distroless/static-debian12
COPY --from=build /out/unified-sidecar /usr/local/bin/unified-sidecar
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
ENTRYPOINT ["/usr/local/bin/unified-sidecar"]
```

Note: the k8s agent overrides the container command with `sleep infinity` to keep it exec-able (`injectSleepInfinity`); the distroless `static` image has no shell, so the pod must set the command explicitly — the agent already does this. **Verify** distroless-static can run `sleep infinity`: it CANNOT (no `sleep` binary). Therefore add a tiny sleep shim: give the binary a `sleep` subcommand OR keep the command as the binary in a long-lived no-op. Simplest: add a hidden `sidecar-idle` mode. Implement it in `run.go` — extend `run` so `args == ["idle"]` blocks on `ctx.Done()`:

```go
	case "idle":
		<-ctx.Done()
		return 0
```

and in `podbuilder.go` (Task 6) set the sidecar `Command: []string{"unified-sidecar", "idle"}` instead of `sleep infinity`. (Task 6 covers the command; this step just records the requirement and adds the `idle` case + a test.)

Add to `cmd/unified-sidecar/run_test.go`:

```go
func TestRun_Idle_BlocksUntilCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- run(ctx, nil, []string{"idle"}, io.Discard) }()
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("idle exit=%d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idle did not return after cancel")
	}
}
```

(Import `time` in the test.) Handle the `idle` case in `run` BEFORE the `len(args) < 2` guard so a single-arg `idle` works:

```go
	if len(args) == 1 && args[0] == "idle" {
		<-ctx.Done()
		return 0
	}
```

- [ ] **Step 2: Verify build + idle test**

Run: `go test ./cmd/unified-sidecar/ -run 'TestRun_Idle' && go build ./cmd/unified-sidecar/`
Expected: PASS, clean. (Docker image build is validated in CI / by the operator; not built here.)

- [ ] **Step 3: Commit**

```bash
git add docker/artifact-sidecar.Dockerfile cmd/unified-sidecar/run.go cmd/unified-sidecar/run_test.go
git commit -m "feat(sidecar): distroless static image + idle command for the sidecar container"
```

---

### Task 5: `ExecStepArgv` — no-shell argv exec

**Files:**
- Modify: `internal/k8sagent/executor.go`
- Test: `internal/k8sagent/executor_argv_test.go` (only if a cluster-free unit is feasible; otherwise assert via the orchestrator test in Task 8 — see note)

**Interfaces:**
- Produces: `func (e *Executor) ExecStepArgv(ctx context.Context, podName, container string, argv []string, stdout, stderr io.Writer) (int, error)` — identical to `ExecStep` but `Command: argv` (no `bash -lc` wrap).

- [ ] **Step 1: Add the method**

In `internal/k8sagent/executor.go`, add:

```go
// ExecStepArgv runs argv directly (no shell) inside the specified container,
// streaming stdout/stderr. If container is empty, the "job" container is used.
// Used for the unified-sidecar binary so values are never shell-interpolated.
func (e *Executor) ExecStepArgv(ctx context.Context, podName, container string, argv []string, stdout, stderr io.Writer) (int, error) {
	if container == "" {
		container = "job"
	}
	req := e.client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(e.namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   argv,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(e.restCfg, "POST", req.URL())
	if err != nil {
		return -1, fmt.Errorf("failed to create exec executor: %w", err)
	}
	streamErr := exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: stdout, Stderr: stderr})
	if streamErr != nil {
		var codeErr uexec.CodeExitError
		if errors.As(streamErr, &codeErr) {
			return codeErr.Code, nil
		}
		return 1, streamErr
	}
	return 0, nil
}
```

**DRY note:** `ExecStep` and `ExecStepArgv` now differ only in `Command:` (`buildShellCommand(script)` vs `argv`). If desired, extract a private `execArgv(ctx, podName, container string, cmd []string, stdout, stderr)` that both call (`ExecStep` passes `buildShellCommand(script)`), and keep the two public methods as thin wrappers. Do this refactor in the same commit to avoid duplication.

- [ ] **Step 2: Build**

Run: `go build ./... && go vet ./internal/k8sagent/`
Expected: clean. (No cluster-free unit test for the SPDY exec itself — it needs a real API server; the argv path is exercised by the orchestrator's fake `sidecarExec` in Task 8 and the `//go:build k8s` round-trip.)

- [ ] **Step 3: Commit**

```bash
git add internal/k8sagent/executor.go
git commit -m "feat(k8sagent): ExecStepArgv runs the sidecar via argv (no shell)"
```

---

### Task 6: Pod construction — Secret env + idle command (additive)

**Files:**
- Modify: `internal/k8sagent/podbuilder.go`
- Modify: `internal/k8sagent/podbuilder_test.go`

**Interfaces:**
- Consumes/Produces: `SidecarSpec` gains `S3SecretName string` (ADDITIVE — keep existing `Image`, `Server`, `Token` so all current call sites and Task 8's pre-rewrite state keep compiling). When injecting the sidecar: keep the existing `Env` (`UNIFIED_SERVER`/`UNIFIED_AGENT_TOKEN`) for now, ADD `EnvFrom` a `SecretRef` when `S3SecretName != ""`, and set the container `Command` to `[]string{"unified-sidecar", "idle"}` (the distroless image has no `sleep`).

- [ ] **Step 1: Write the failing test**

Add to `internal/k8sagent/podbuilder_test.go`:

```go
func TestBuildPod_SidecarSecretEnvAndIdle(t *testing.T) {
	pod, err := BuildPod("run1", "ns", nil, nil, "job-image:latest",
		SidecarSpec{Image: "sidecar:latest", S3SecretName: "ucd-s3"})
	if err != nil {
		t.Fatal(err)
	}
	var sc *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == artifactSidecarName {
			sc = &pod.Spec.Containers[i]
		}
	}
	if sc == nil {
		t.Fatal("sidecar container not found")
	}
	if len(sc.EnvFrom) != 1 || sc.EnvFrom[0].SecretRef == nil || sc.EnvFrom[0].SecretRef.Name != "ucd-s3" {
		t.Fatalf("expected EnvFrom secretRef ucd-s3, got %+v", sc.EnvFrom)
	}
	if len(sc.Command) < 2 || sc.Command[0] != "unified-sidecar" || sc.Command[1] != "idle" {
		t.Fatalf("expected command [unified-sidecar idle], got %v", sc.Command)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/k8sagent/ -run TestBuildPod_SidecarSecretEnvAndIdle`
Expected: FAIL — `S3SecretName` unknown / no EnvFrom / wrong command.

- [ ] **Step 3: Implement**

In `internal/k8sagent/podbuilder.go`:

1. Add the field to `SidecarSpec`:

```go
type SidecarSpec struct {
	Image        string
	Server       string // controller base URL (legacy artifact-via-controller path; removed in the direct-S3 rewrite)
	Token        string // bearer token (legacy)
	S3SecretName string // Secret providing UNIFIED_S3_* env for the direct-S3 sidecar
}
```

2. In `BuildPod`, change the sidecar injection block to set the command to the idle binary and add `EnvFrom` when a secret name is set:

```go
	if sidecar.Image != "" {
		sc := corev1.Container{
			Name:    artifactSidecarName,
			Image:   sidecar.Image,
			Command: []string{"unified-sidecar", "idle"},
			Env: []corev1.EnvVar{
				{Name: "UNIFIED_SERVER", Value: sidecar.Server},
				{Name: "UNIFIED_AGENT_TOKEN", Value: sidecar.Token},
			},
		}
		if sidecar.S3SecretName != "" {
			sc.EnvFrom = []corev1.EnvFromSource{
				{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: sidecar.S3SecretName}}},
			}
		}
		podSpec.Containers = append(podSpec.Containers, sc)
	}
```

Note: `injectSleepInfinity` only sets a command when none exists, so the explicit sidecar `Command` above is preserved (the sidecar already has a command, so it is not overwritten). Verify this ordering — the sidecar is appended after `injectSleepInfinity(podSpec)` is called on the base spec, so it is fine; the sidecar's own command is set here and never cleared.

- [ ] **Step 4: Run tests, build**

Run: `go test ./internal/k8sagent/ -run 'TestBuildPod' && go build ./...`
Expected: PASS (new + existing BuildPod tests), clean.

- [ ] **Step 5: Commit**

```bash
git add internal/k8sagent/podbuilder.go internal/k8sagent/podbuilder_test.go
git commit -m "feat(k8sagent): sidecar gets S3 Secret via EnvFrom + idle command (additive)"
```

---

### Task 7: Agent config `SidecarS3SecretName` + wiring

**Files:**
- Modify: `internal/k8sagent/config.go`
- Modify: `internal/k8sagent/agent.go` (both `SidecarSpec{...}` construction sites, ~lines 103 and 118)
- Modify: `internal/k8sagent/config_test.go` (assert the field round-trips from YAML)

**Interfaces:**
- Consumes: `Config.SidecarS3SecretName`.
- Produces: the `SidecarSpec` built in `agent.go` now sets `S3SecretName: a.cfg.SidecarS3SecretName`.

- [ ] **Step 1: Write the failing test**

Add to `internal/k8sagent/config_test.go` a case asserting `sidecarS3SecretName` parses from YAML (follow the file's existing config-loading test pattern; if it loads from a temp YAML file, add `sidecarS3SecretName: my-s3-secret` and assert `cfg.SidecarS3SecretName == "my-s3-secret"`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/k8sagent/ -run TestConfig`
Expected: FAIL — unknown field / zero value.

- [ ] **Step 3: Implement**

1. In `internal/k8sagent/config.go`, add to `Config`:

```go
	SidecarS3SecretName string `yaml:"sidecarS3SecretName,omitempty"`
```

2. In `internal/k8sagent/agent.go`, update BOTH `SidecarSpec{...}` literals (lines ~103 and ~118) to include the secret name:

```go
	SidecarSpec{Image: a.cfg.SidecarImage, Server: a.cfg.Server, Token: a.cfg.Token, S3SecretName: a.cfg.SidecarS3SecretName}
```

- [ ] **Step 4: Run tests, build**

Run: `go test ./internal/k8sagent/ -run TestConfig && go build ./...`
Expected: PASS, clean.

- [ ] **Step 5: Commit**

```bash
git add internal/k8sagent/config.go internal/k8sagent/agent.go internal/k8sagent/config_test.go
git commit -m "feat(k8sagent): thread sidecarS3SecretName into SidecarSpec"
```

---

### Task 8: Orchestrator — argv sidecarExec, cache branch, deferred saves, artifact rewrite

This is the integration task. It switches the `artifactExec` seam to argv, rewrites the artifact branches to invoke the binary, adds the cache restore branch + deferred saves, and finally removes the now-dead `Server`/`Token` from `SidecarSpec` and the sidecar `Env`.

**Files:**
- Modify: `internal/k8sagent/agent.go`
- Modify: `internal/k8sagent/podbuilder.go` (remove `Server`/`Token` + the sidecar `Env` after the orchestrator no longer needs them)
- Modify: `internal/k8sagent/orchestrate_test.go`, `internal/k8sagent/orchestrate_cancel_test.go` (fake signature change)
- Modify: `internal/k8sagent/podbuilder_test.go` (drop the Server/Token references) and any `//go:build k8s` test constructing `SidecarSpec{... Server/Token ...}`

**Interfaces:**
- Changes: the seam becomes `sidecarExec func(ctx context.Context, container string, argv []string) (int, error)` (was `artifactExec func(ctx, container, script string)`). `orchestrate`'s signature updates accordingly.
- Consumes: `dsl.ExpandTemplate`, `step.Cache` (`*dsl.CacheStep` with `Path`, `Key`, `RestoreKeys`, `TTLDays`), `artifactSidecarName`.

- [ ] **Step 1: Update the fakes + signatures (make tests compile against the new seam)**

In `orchestrate_test.go` and `orchestrate_cancel_test.go`, change the fake from `func(_ context.Context, _, _ string) (int, error)` to `func(_ context.Context, _ string, _ []string) (int, error)` and update the `a.orchestrate(...)` calls. Update `orchestrate`'s parameter type in `agent.go` to match:

```go
func (a *K8sAgent) orchestrate(ctx context.Context, c api.ClaimResponse, stepExec podStepExec, sidecarExec func(ctx context.Context, container string, argv []string) (int, error), mountPath string) {
```

- [ ] **Step 2: Write the failing test (cache dispatch)**

Add to `internal/k8sagent/orchestrate_test.go` a test that runs `orchestrate` over a claim containing a `cache` step and asserts (a) a `cache restore` argv is exec'd into `unified-artifact` during the step, and (b) a `cache save` argv is exec'd after the main stages. Follow the existing `runOrchestrateArtifact` harness shape — record every `(container, argv)` the fake `sidecarExec` receives, then assert on the recorded argv slices. Concretely, the fake appends each call to a slice; after `orchestrate` returns, assert one recorded call has `argv[0:2] == ["unified-sidecar","cache"]` with `"restore"` and another with `"save"`, both targeting container `unified-artifact`, and that the restore carries `--key <expanded>` and `--path <mountPath>/<cache path>`.

(Model the claim step after the existing artifact orchestrate test; set `step.Cache = &dsl.CacheStep{Path: "node_modules", Key: "npm-{{.Params.v}}", TTLDays: 7}` with `Params: {"v":"1"}` so you also assert the key expanded to `npm-1`.)

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/k8sagent/ -run TestOrchestrate`
Expected: FAIL — cache branch not implemented.

- [ ] **Step 4: Implement the artifact rewrite + cache branch**

In `agent.go` `makeRunStep` (the returned closure):

1. Rewrite the **artifact upload** branch body to use argv (replace the `script := fmt.Sprintf("set -e; tar ...")` + `artifactExec(execCtx, artifactSidecarName, script)` with):

```go
	argv := []string{"unified-sidecar", "artifact", "upload",
		"--run", c.RunID, "--name", step.UploadArtifact.Name,
		"--path", path.Join(mountPath, step.UploadArtifact.Path)}
	ec, err := sidecarExec(execCtx, artifactSidecarName, argv)
```

2. Rewrite the **artifact download** branch similarly:

```go
	dest := step.DownloadArtifact.DestDir
	if dest == "" {
		dest = "."
	}
	argv := []string{"unified-sidecar", "artifact", "download",
		"--run", c.RunID, "--name", step.DownloadArtifact.Name,
		"--dest", path.Join(mountPath, dest)}
	ec, err := sidecarExec(execCtx, artifactSidecarName, argv)
```

(Keep the surrounding Running/Succeeded/Failed reporting + `recordFailure` on failure — artifacts stay fail-loud.)

3. Add the **cache restore** branch AFTER the approval gate and BEFORE the artifact branches (order among sidecar actions does not matter; place it with the other sidecar branches):

```go
	if step.Cache != nil {
		started := time.Now().UTC()
		_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
			RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.Name, Status: "Running", StartedAt: started,
		})
		key, _ := dsl.ExpandTemplate(step.Cache.Key, tplData)
		var restoreKeys []string
		for _, rk := range step.Cache.RestoreKeys {
			if v, _ := dsl.ExpandTemplate(rk, tplData); v != "" {
				restoreKeys = append(restoreKeys, v)
			}
		}
		cachePath := path.Join(mountPath, step.Cache.Path)
		argv := []string{"unified-sidecar", "cache", "restore", "--key", key, "--path", cachePath}
		for _, rk := range restoreKeys {
			argv = append(argv, "--restore-key", rk)
		}
		// Best-effort: a miss/error never fails the step (the binary exits 0).
		_, _ = sidecarExec(execCtx, artifactSidecarName, argv)

		ttlDays := step.Cache.TTLDays
		if ttlDays == 0 {
			ttlDays = 30
		}
		registerCacheSave(cacheSaveSpec{key: key, ttlDays: ttlDays, path: cachePath})

		_ = a.client.ReportStep(ctx, a.cfg.AgentID, api.StepReportRequest{
			RunID: c.RunID, StepIndex: step.Index, StageIndex: step.StageIndex, StepName: step.Name, Status: "Succeeded", StartedAt: started, EndedAt: time.Now().UTC(),
		})
		return
	}
```

4. Add the deferred-save plumbing in `orchestrate` (outside `makeRunStep`, in the outer function scope so it is shared):

```go
	type cacheSaveSpec struct {
		key     string
		ttlDays int
		path    string
	}
	var cacheSavesMu sync.Mutex
	var cacheSaves []cacheSaveSpec
	registerCacheSave := func(s cacheSaveSpec) {
		cacheSavesMu.Lock()
		cacheSaves = append(cacheSaves, s)
		cacheSavesMu.Unlock()
	}
```

Make `registerCacheSave` and the `cacheSaveSpec` type visible to `makeRunStep` by declaring them ABOVE the `makeRunStep := func(...)` definition (closures capture outer scope). Add `"sync"` to imports if not present.

5. After the main-stage loop and output promotion, and BEFORE the `finally` block, drain the deferred saves:

```go
	// Deferred cache saves: capture the final workspace after the main stages
	// (before finally, which is cleanup/notify). Best-effort — never flips status.
	for _, s := range cacheSaves {
		argv := []string{"unified-sidecar", "cache", "save", "--key", s.key, "--ttl-days", strconv.Itoa(s.ttlDays), "--path", s.path}
		if _, err := sidecarExec(ctx, artifactSidecarName, argv); err != nil {
			slog.Warn("k8s: cache save exec failed", "key", s.key, "error", err)
		}
	}
```

Add `"strconv"` to imports.

6. Update the production `sidecarExec` in `executeRun` (the closure formerly named `artifactExec`, ~line 177) to the argv signature calling `ExecStepArgv`:

```go
	sidecarExec := func(execCtx context.Context, container string, argv []string) (int, error) {
		stderrPusher := agentlib.NewLogPusher(a.client, a.cfg.AgentID, c.RunID, 0, "stderr")
		ec, err := a.exec.ExecStepArgv(execCtx, podName, container, argv, io.Discard, stderrPusher)
		stderrPusher.Flush(execCtx)
		return ec, err
	}
	a.orchestrate(ctx, c, stepExec, sidecarExec, mountPath)
```

- [ ] **Step 5: Remove the dead Server/Token (cleanup)**

Now that no code path uses the controller from the sidecar:
1. In `podbuilder.go`, remove `Server` and `Token` from `SidecarSpec` and remove the `Env: []corev1.EnvVar{{UNIFIED_SERVER...},{UNIFIED_AGENT_TOKEN...}}` from the injected sidecar container (keep only `EnvFrom` the Secret).
2. In `agent.go`, change both `SidecarSpec{...}` literals to `SidecarSpec{Image: a.cfg.SidecarImage, S3SecretName: a.cfg.SidecarS3SecretName}`.
3. Update `podbuilder_test.go` (`TestBuildPod...` that referenced `Server`/`Token`, ~line 122) to drop those fields and drop assertions on the removed env; keep/adjust the EnvFrom assertion from Task 6.
4. Update any `//go:build k8s` test (`artifact_k8s_test.go`) that constructed `SidecarSpec{Server/Token}` or asserted the injected token env — switch it to the direct-S3 model (Secret env) or remove the token-specific assertions. Compile it with `go vet -tags k8s ./internal/k8sagent/`.

- [ ] **Step 6: Run tests, build**

Run: `go test ./internal/k8sagent/ && go vet -tags k8s ./internal/k8sagent/ && go build ./...`
Expected: PASS (orchestrate cache + artifact argv tests, podbuilder, config), k8s-tagged vet clean, build clean.

- [ ] **Step 7: Commit**

```bash
git add internal/k8sagent/
git commit -m "feat(k8sagent): cache restore/save + artifact via unified-sidecar argv (direct-S3)"
```

---

### Task 9: `//go:build k8s` round-trip + docs

**Files:**
- Create/Modify: `internal/k8sagent/cache_k8s_test.go` (`//go:build k8s`) — a real-pod cache round-trip (documented; needs a cluster; not run here).
- Modify: `docs/kubernetes-integration.md`, `docs/jobs.md`

- [ ] **Step 1: Write the k8s round-trip test**

Add `internal/k8sagent/cache_k8s_test.go` (`//go:build k8s`) modeled on the existing `artifact_k8s_test.go`: bring up a pod with the sidecar (S3 Secret env pointing at the test MinIO), run a step that writes a file into the cache path, exec `cache save`, delete/recreate into a fresh dir, exec `cache restore`, assert the file is back. Ensure it compiles under `go vet -tags k8s ./internal/k8sagent/`.

- [ ] **Step 2: Update docs**

- `docs/kubernetes-integration.md`: document that k8s cache + artifacts now transfer via the `unified-artifact` sidecar talking **direct to S3**; the operator must create a Secret with `UNIFIED_S3_ENDPOINT/BUCKET/KEY/SECRET` (+ optional `UNIFIED_S3_USE_SSL`, `UNIFIED_S3_REGION`) and set `sidecarS3SecretName` in the agent config; the sidecar holds bucket-scoped S3 credentials (Secret-mounted; job containers cannot read them; document the Argo/Tekton-equivalent threat model and note STS/IRSA as a future hardening). Note cache is best-effort (miss/error never fails a step); a missing Secret makes cache steps fail loudly.
- `docs/jobs.md`: in the Cache section, note that cache is now supported on the k8s agent (previously a no-op), with the same `key`/`restoreKeys`/`ttlDays` semantics.

- [ ] **Step 3: Commit**

```bash
git add internal/k8sagent/cache_k8s_test.go docs/kubernetes-integration.md docs/jobs.md
git commit -m "test(k8s)+docs: cache round-trip and direct-S3 sidecar documentation"
```

---

## Self-Review

**Spec coverage:** unified-sidecar binary → Task 3; artifact store layer → Task 2; env loader → Task 1; Dockerfile (static, no shell) → Task 4; ExecStepArgv → Task 5; podbuilder Secret + idle → Tasks 6 & 8; config threading → Task 7; orchestrator cache branch + deferred saves + artifact argv rewrite → Task 8; k8s round-trip + docs → Task 9. All spec sections covered.

**Placeholder scan:** No TBD/TODO. Where a task says "follow the existing harness" (Task 8 Step 2 cache test; Task 9 k8s test), it references a concrete existing file (`orchestrate_test.go` `runOrchestrateArtifact`, `artifact_k8s_test.go`) with the exact assertions to make — discovery-with-a-concrete-model, not a placeholder. Complete code is given for all new logic (env loader, artifact store, sidecar run dispatch, ExecStepArgv, podbuilder injection, orchestrator cache branch + deferred save).

**Type consistency:** `objectstore.S3ConfigFromEnv() (S3Config, error)` (Task 1) consumed by `cmd/unified-sidecar/main.go` (Task 3). `artifact.WriteTarZstd`/`Upload`/`Download` (Task 2) consumed by cache.Save (Task 2) and the sidecar (Task 3). `run(ctx, store, args, stderr) int` (Task 3) is the test seam. `ExecStepArgv(ctx, podName, container, argv, stdout, stderr) (int, error)` (Task 5) consumed by the production `sidecarExec` (Task 8). The seam `sidecarExec func(ctx, container string, argv []string) (int, error)` is defined once (Task 8 Step 1) and used by the fakes and production consistently. `SidecarSpec` is additive (Task 6) then trimmed (Task 8 Step 5); every intermediate task compiles. `cacheSaveSpec{key, ttlDays, path}` defined and consumed within `orchestrate` (Task 8). Cache best-effort (exit 0 / Succeeded) vs artifact fail-loud (exit 1 / recordFailure) applied consistently across Tasks 3 and 8.
