package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/eirueimi/unified-cd/internal/objectstore"
)

// The job controls symlinks in the workspace and the sidecar resolves them in
// its own filesystem, where the credential mounts live. Every subcommand is
// confined to --root, so a link that leads out of it is never followed.

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
}

// storedBytes returns everything under dir, decompressing what decompresses.
func storedBytes(t *testing.T, dir string) []byte {
	t.Helper()
	var all bytes.Buffer
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		all.Write(b)
		if dec, err := zstd.NewReader(bytes.NewReader(b)); err == nil {
			out, _ := io.ReadAll(dec)
			all.Write(out)
			dec.Close()
		}
		return nil
	})
	return all.Bytes()
}

func TestRun_RootRequired(t *testing.T) {
	prov := localProvider(t.TempDir())
	for _, args := range [][]string{
		{"artifact", "upload", "--run", "r1", "--name", "a", "--path", t.TempDir()},
		{"artifact", "download", "--run", "r1", "--name", "a", "--dest", t.TempDir()},
		{"cache", "save", "--key", "k", "--path", t.TempDir(), "--job", "j"},
		{"cache", "restore", "--key", "k", "--path", t.TempDir(), "--job", "j"},
	} {
		var stderr bytes.Buffer
		if code := run(context.Background(), prov, args, &stderr); code != 2 {
			t.Errorf("%v: exit=%d, want 2 (stderr %q)", args[:2], code, stderr.String())
		}
	}
}

func TestRun_UploadThroughSymlinkOutOfRootIsRefused(t *testing.T) {
	creds := t.TempDir() // stands in for /etc/unified-cd
	if err := os.WriteFile(filepath.Join(creds, "s3-credentials"), []byte("TOPSECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	ws := t.TempDir()
	mustSymlink(t, creds, filepath.Join(ws, "l"))
	storeDir := t.TempDir()
	prov := localProvider(storeDir)
	ctx := context.Background()

	var stderr bytes.Buffer
	code := run(ctx, prov, []string{"artifact", "upload", "--run", "r1", "--name", "a", "--root", ws,
		"--path", filepath.Join(ws, "l", "s3-credentials")}, &stderr)
	if code == 0 {
		t.Fatal("artifact upload through a link out of the workspace succeeded")
	}
	if bytes.Contains(storedBytes(t, storeDir), []byte("TOPSECRET")) {
		t.Fatal("the credential reached the object store")
	}

	code = run(ctx, prov, []string{"cache", "save", "--key", "k", "--job", "j", "--root", ws,
		"--path", filepath.Join(ws, "l", "s3-credentials")}, &stderr)
	if code != 0 {
		t.Fatalf("cache save is best-effort and exits 0, got %d", code)
	}
	if bytes.Contains(storedBytes(t, storeDir), []byte("TOPSECRET")) {
		t.Fatal("the credential reached the object store through cache save")
	}
}

func TestRun_DownloadAndRestoreThroughSymlinkOutOfRootAreRefused(t *testing.T) {
	outside := t.TempDir() // stands in for the sidecar's trust store directory
	ws := t.TempDir()
	src := filepath.Join(ws, "src")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "ca.pem"), []byte("PWNED"), 0o600); err != nil {
		t.Fatal(err)
	}
	prov := localProvider(t.TempDir())
	ctx := context.Background()
	if code := run(ctx, prov, []string{"artifact", "upload", "--run", "r1", "--name", "a", "--root", ws, "--path", src}, io.Discard); code != 0 {
		t.Fatalf("upload exit=%d", code)
	}
	if code := run(ctx, prov, []string{"cache", "save", "--key", "k", "--job", "j", "--root", ws, "--path", src}, io.Discard); code != 0 {
		t.Fatalf("cache save exit=%d", code)
	}
	mustSymlink(t, outside, filepath.Join(ws, "l"))

	if code := run(ctx, prov, []string{"artifact", "download", "--run", "r1", "--name", "a", "--root", ws,
		"--dest", filepath.Join(ws, "l")}, io.Discard); code == 0 {
		t.Error("artifact download into a link out of the workspace succeeded")
	}
	if code := run(ctx, prov, []string{"artifact", "download", "--run", "r1", "--name", "a", "--root", ws,
		"--dest", filepath.Join(ws, "l", "sub")}, io.Discard); code == 0 {
		t.Error("artifact download below a link out of the workspace succeeded")
	}
	var stdout bytes.Buffer
	_ = runCache(ctx, mustStore(t, prov), "restore",
		[]string{"--key", "k", "--job", "j", "--root", ws, "--path", filepath.Join(ws, "l")}, &stdout, io.Discard)
	if bytes.Contains(stdout.Bytes(), []byte("UCD_CACHE_RESULT=hit")) {
		t.Error("cache restore through a link out of the workspace reported a hit")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatalf("something was written outside the root: %v", entries)
	}
}

// A link that stays inside the workspace keeps working in both directions.
func TestRun_InRootSymlinkStillWorks(t *testing.T) {
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "real", "a.txt"), []byte("A"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, "real", filepath.Join(ws, "alias"))
	prov := localProvider(t.TempDir())
	ctx := context.Background()
	if code := run(ctx, prov, []string{"artifact", "upload", "--run", "r1", "--name", "a", "--root", ws,
		"--path", filepath.Join(ws, "alias", "a.txt")}, io.Discard); code != 0 {
		t.Fatalf("upload via in-root link exit=%d", code)
	}
	if code := run(ctx, prov, []string{"artifact", "download", "--run", "r1", "--name", "a", "--root", ws,
		"--dest", filepath.Join(ws, "alias", "out")}, io.Discard); code != 0 {
		t.Fatalf("download via in-root link exit=%d", code)
	}
	if b, err := os.ReadFile(filepath.Join(ws, "real", "out", "a.txt")); err != nil || string(b) != "A" {
		t.Fatalf("a.txt = %q, %v", b, err)
	}
}

func mustStore(t *testing.T, prov storeProvider) objectstore.ObjectStore {
	t.Helper()
	st, err := prov(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	return st
}
