package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eirueimi/unified-cd/internal/cache"
	"github.com/eirueimi/unified-cd/internal/objectstore"
)

// seededStore returns a LocalObjectStore (rooted at a fresh t.TempDir) with a
// cache entry already saved under key, so a "restore --key <key>" call hits.
func seededStore(t *testing.T, key string) objectstore.ObjectStore {
	t.Helper()
	store := objectstore.NewLocalObjectStore(t.TempDir())
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "dep.txt"), []byte("cached"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cache.Save(context.Background(), store, "test-job", src, key, 7); err != nil {
		t.Fatal(err)
	}
	return store
}

// emptyStore returns a fresh, empty LocalObjectStore, so any "restore" call
// against it misses.
func emptyStore(t *testing.T) objectstore.ObjectStore {
	t.Helper()
	return objectstore.NewLocalObjectStore(t.TempDir())
}

// localProvider returns a store provider backed by a LocalObjectStore rooted at dir.
func localProvider(dir string) func(context.Context) (objectstore.ObjectStore, error) {
	store := objectstore.NewLocalObjectStore(dir)
	return func(context.Context) (objectstore.ObjectStore, error) {
		return store, nil
	}
}

// erroringProvider returns a store provider that always fails. Used to prove
// that "idle" never invokes the provider, and that cache/artifact subcommands
// fail loudly when it does.
func erroringProvider(ctx context.Context) (objectstore.ObjectStore, error) {
	return nil, errors.New("boom: no S3 config")
}

func TestRunCache_RestoreEmitsHitMarkerToStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ec := runCache(context.Background(), seededStore(t, "k"), "restore",
		[]string{"--key", "k", "--path", t.TempDir(), "--job", "test-job"}, &stdout, &stderr)
	if ec != 0 {
		t.Fatalf("exit=%d", ec)
	}
	if !strings.Contains(stdout.String(), "UCD_CACHE_RESULT=hit") {
		t.Fatalf("stdout = %q, want UCD_CACHE_RESULT=hit", stdout.String())
	}
	if strings.Contains(stderr.String(), "UCD_CACHE_RESULT") {
		t.Fatalf("marker leaked onto stderr: %q", stderr.String())
	}
}

func TestRunCache_RestoreEmitsMissMarkerToStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ec := runCache(context.Background(), emptyStore(t), "restore",
		[]string{"--key", "absent", "--path", t.TempDir(), "--job", "test-job"}, &stdout, &stderr)
	if ec != 0 {
		t.Fatalf("exit=%d", ec)
	}
	if !strings.Contains(stdout.String(), "UCD_CACHE_RESULT=miss") {
		t.Fatalf("stdout = %q, want UCD_CACHE_RESULT=miss", stdout.String())
	}
	if strings.Contains(stderr.String(), "UCD_CACHE_RESULT") {
		t.Fatalf("marker leaked onto stderr: %q", stderr.String())
	}
}

func TestRun_CacheSaveThenRestore(t *testing.T) {
	prov := localProvider(t.TempDir())
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "dep.txt"), []byte("cached"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if code := run(ctx, prov, []string{"cache", "save", "--key", "k1", "--ttl-days", "7", "--path", src, "--job", "test-job"}, io.Discard); code != 0 {
		t.Fatalf("cache save exit=%d", code)
	}
	dest := t.TempDir()
	if code := run(ctx, prov, []string{"cache", "restore", "--key", "k1", "--path", dest, "--job", "test-job"}, io.Discard); code != 0 {
		t.Fatalf("cache restore exit=%d", code)
	}
	got, err := os.ReadFile(filepath.Join(dest, "dep.txt"))
	if err != nil || string(got) != "cached" {
		t.Fatalf("dep.txt = %q, %v", got, err)
	}
}

func TestRun_CacheRestoreMiss_Exit0(t *testing.T) {
	prov := localProvider(t.TempDir())
	dest := t.TempDir()
	if code := run(context.Background(), prov, []string{"cache", "restore", "--key", "nope", "--path", dest, "--job", "test-job"}, io.Discard); code != 0 {
		t.Fatalf("cache restore miss should exit 0, got %d", code)
	}
}

// TestRunCache_RestoreMissingJob_RejectsWithClearError proves the sidecar
// requires --job for cache operations: without it, an entry saved under one
// job could otherwise be restored by any caller with no namespacing (the
// runtime path the unit tests for internal/cache/ don't themselves cover,
// since --job is a CLI-layer requirement, not a cache-package one).
func TestRunCache_RestoreMissingJob_RejectsWithClearError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ec := runCache(context.Background(), emptyStore(t), "restore",
		[]string{"--key", "k", "--path", t.TempDir()}, &stdout, &stderr)
	if ec == 0 {
		t.Fatalf("cache restore without --job should not exit 0")
	}
	if !strings.Contains(stderr.String(), "--job") {
		t.Fatalf("expected a clear --job-required error, got: %q", stderr.String())
	}
}

func TestRunCache_SaveMissingJob_RejectsWithClearError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	src := t.TempDir()
	ec := runCache(context.Background(), emptyStore(t), "save",
		[]string{"--key", "k", "--path", src}, &stdout, &stderr)
	if ec == 0 {
		t.Fatalf("cache save without --job should not exit 0")
	}
	if !strings.Contains(stderr.String(), "--job") {
		t.Fatalf("expected a clear --job-required error, got: %q", stderr.String())
	}
}

func TestRun_ArtifactUploadDownload(t *testing.T) {
	prov := localProvider(t.TempDir())
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "out.txt"), []byte("art"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if code := run(ctx, prov, []string{"artifact", "upload", "--run", "r1", "--name", "build", "--path", src}, io.Discard); code != 0 {
		t.Fatalf("artifact upload exit=%d", code)
	}
	dest := t.TempDir()
	if code := run(ctx, prov, []string{"artifact", "download", "--run", "r1", "--name", "build", "--dest", dest}, io.Discard); code != 0 {
		t.Fatalf("artifact download exit=%d", code)
	}
	got, err := os.ReadFile(filepath.Join(dest, "out.txt"))
	if err != nil || string(got) != "art" {
		t.Fatalf("out.txt = %q, %v", got, err)
	}
}

func TestRun_ArtifactDownloadMissing_Exit1(t *testing.T) {
	prov := localProvider(t.TempDir())
	if code := run(context.Background(), prov, []string{"artifact", "download", "--run", "r1", "--name", "nope", "--dest", t.TempDir()}, io.Discard); code == 0 {
		t.Fatal("missing artifact download should exit non-zero")
	}
}

func TestRun_Idle_BlocksUntilCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- run(ctx, erroringProvider, []string{"idle"}, io.Discard) }()
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

// Regression test for TODO #21: idle must survive degraded mode (no S3 config)
// by never invoking the store provider at all.
func TestRun_Idle_NeverInvokesProvider_DegradedMode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	called := false
	prov := func(context.Context) (objectstore.ObjectStore, error) {
		called = true
		return nil, errors.New("should never be called")
	}
	done := make(chan int, 1)
	go func() { done <- run(ctx, prov, []string{"idle"}, io.Discard) }()
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("idle exit=%d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idle did not return after cancel")
	}
	if called {
		t.Fatal("idle must not invoke the store provider (degraded mode should stay resident)")
	}
}

// Regression test for TODO #21: cache/artifact subcommands must fail loudly
// (non-zero exit, clear message) when the store provider errors, instead of
// crashing the whole process before dispatch.
func TestRun_CacheSubcommand_ProviderError_FailsLoudly(t *testing.T) {
	var stderr strings.Builder
	code := run(context.Background(), erroringProvider, []string{"cache", "restore", "--key", "k1", "--path", t.TempDir()}, &stderr)
	if code == 0 {
		t.Fatal("cache restore with erroring provider should exit non-zero")
	}
	if !strings.Contains(stderr.String(), "S3") {
		t.Fatalf("expected clear S3-config error message, got: %q", stderr.String())
	}
}

func TestRun_ArtifactSubcommand_ProviderError_FailsLoudly(t *testing.T) {
	var stderr strings.Builder
	code := run(context.Background(), erroringProvider, []string{"artifact", "upload", "--run", "r1", "--name", "n1", "--path", t.TempDir()}, &stderr)
	if code == 0 {
		t.Fatal("artifact upload with erroring provider should exit non-zero")
	}
	if !strings.Contains(stderr.String(), "S3") {
		t.Fatalf("expected clear S3-config error message, got: %q", stderr.String())
	}
}
