package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

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
