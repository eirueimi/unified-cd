package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// The agent passes --exclude for every volume mounted below an archived path; a
// sidecar that ignored or rejected it would archive the volume in full (or fail
// the step), so both subcommands must honour it, repeated.
func TestRun_ExcludeLeavesPathsOutOfTheArchive(t *testing.T) {
	src := t.TempDir()
	for _, f := range []string{"keep.txt", "vol1/big.bin", "vol2/deep/big.bin"} {
		p := filepath.Join(src, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	excludes := []string{"--exclude", filepath.Join(src, "vol1"), "--exclude", filepath.Join(src, "vol2")}
	prov := localProvider(t.TempDir())
	ctx := context.Background()

	if code := run(ctx, prov, append([]string{"artifact", "upload", "--run", "r1", "--name", "a", "--path", src}, excludes...), io.Discard); code != 0 {
		t.Fatalf("artifact upload exit=%d", code)
	}
	artifactDest := t.TempDir()
	if code := run(ctx, prov, []string{"artifact", "download", "--run", "r1", "--name", "a", "--dest", artifactDest}, io.Discard); code != 0 {
		t.Fatalf("artifact download exit=%d", code)
	}

	if code := run(ctx, prov, append([]string{"cache", "save", "--key", "k", "--ttl-days", "7", "--path", src, "--job", "j"}, excludes...), io.Discard); code != 0 {
		t.Fatalf("cache save exit=%d", code)
	}
	cacheDest := t.TempDir()
	if code := run(ctx, prov, []string{"cache", "restore", "--key", "k", "--path", cacheDest, "--job", "j"}, io.Discard); code != 0 {
		t.Fatalf("cache restore exit=%d", code)
	}

	// A restore leaves the excluded paths alone too, even from an archive that
	// holds them (this one was saved without the exclusions).
	if code := run(ctx, prov, []string{"cache", "save", "--key", "old", "--ttl-days", "7", "--path", src, "--job", "j"}, io.Discard); code != 0 {
		t.Fatalf("cache save exit=%d", code)
	}
	restoreDest := t.TempDir()
	if code := run(ctx, prov, append([]string{"cache", "restore", "--key", "old", "--path", restoreDest, "--job", "j"}, "--exclude", filepath.Join(restoreDest, "vol1"), "--exclude", filepath.Join(restoreDest, "vol2")), io.Discard); code != 0 {
		t.Fatalf("cache restore exit=%d", code)
	}

	for name, dest := range map[string]string{"artifact": artifactDest, "cache": cacheDest, "restore": restoreDest} {
		if _, err := os.Stat(filepath.Join(dest, "keep.txt")); err != nil {
			t.Errorf("%s: keep.txt is missing: %v", name, err)
		}
		for _, gone := range []string{"vol1", "vol2"} {
			if _, err := os.Stat(filepath.Join(dest, gone)); !os.IsNotExist(err) {
				t.Errorf("%s: %s was archived (stat err=%v)", name, gone, err)
			}
		}
	}
}
