package gittemplate

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestMain lets the test binary double as a fake "git": when
// GITTEMPLATE_FAKE_GIT is set, the process behaves as the named fake instead of
// running tests. Tests install a copy of the binary named "git" first on PATH.
func TestMain(m *testing.M) {
	switch os.Getenv("GITTEMPLATE_FAKE_GIT") {
	case "":
		os.Exit(m.Run())
	case "hang":
		// Like a git that never returns, with a helper child (as ssh would be)
		// that inherits stdout/stderr and so keeps the output pipes open.
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), "GITTEMPLATE_FAKE_GIT=sleep")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		_ = child.Start()
		time.Sleep(time.Minute)
	case "sleep":
		time.Sleep(time.Minute)
	case "grow":
		// git init succeeds; git fetch writes a large pack into the scratch repo
		// and then stalls.
		if len(os.Args) > 1 && os.Args[1] == "fetch" {
			_ = os.MkdirAll(".git/objects/pack", 0o755)
			_ = os.WriteFile(".git/objects/pack/tmp_pack_x", make([]byte, 4<<20), 0o644)
			time.Sleep(time.Minute)
		}
	}
	os.Exit(0)
}

// installFakeGit puts a copy of the test binary named git first on PATH and
// selects its behaviour.
func installFakeGit(t *testing.T, mode string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	name := "git"
	if runtime.GOOS == "windows" {
		name = "git.exe"
	}
	dir := t.TempDir()
	src, err := os.Open(exe)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	dst.Close()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GITTEMPLATE_FAKE_GIT", mode)
}

func TestFetchDir_TimeoutKillsProcessTree(t *testing.T) {
	installFakeGit(t, "hang")
	f := &Fetcher{Timeout: time.Second}
	start := time.Now()
	_, err := f.FetchDir(context.Background(), "file:///nonexistent", "main", "", Credential{})
	elapsed := time.Since(start)
	if err == nil || !errors.Is(err, errGitTimeout) || !strings.Contains(err.Error(), "timed out after 1s") {
		t.Fatalf("want a timeout error, got %v", err)
	}
	// The child holds the output pipes open; returning well before WaitDelay
	// (5s) means the whole process tree was killed, not just git.
	if elapsed > 4*time.Second {
		t.Fatalf("operation returned after %s; process tree was not killed promptly", elapsed)
	}
}

func TestResolveCommitSHA_Timeout(t *testing.T) {
	installFakeGit(t, "sleep")
	f := &Fetcher{Timeout: 500 * time.Millisecond}
	start := time.Now()
	_, err := f.ResolveCommitSHA(context.Background(), "file:///nonexistent", "main", Credential{})
	if err == nil || !errors.Is(err, errGitTimeout) {
		t.Fatalf("want a timeout error, got %v", err)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatalf("ls-remote did not stop at the deadline")
	}
}

func TestFetch_TimeoutDefaultAndParentCancel(t *testing.T) {
	installFakeGit(t, "sleep")
	f := &Fetcher{}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := f.Fetch(ctx, URI{Host: "example.com", Owner: "o", Repo: "r", Ref: "main", Path: "a.yaml"}, Credential{})
	if err == nil {
		t.Fatal("want an error")
	}
	// The caller's own deadline fired first, so this is not reported as the git timeout.
	if errors.Is(err, errGitTimeout) {
		t.Fatalf("caller cancellation misreported as git timeout: %v", err)
	}
}

func TestFetchDir_SizeLimit(t *testing.T) {
	installFakeGit(t, "grow")
	old := sizePollInterval
	sizePollInterval = 20 * time.Millisecond
	defer func() { sizePollInterval = old }()
	f := &Fetcher{Timeout: 30 * time.Second, MaxRepoBytes: 1 << 20}
	start := time.Now()
	_, err := f.FetchDir(context.Background(), "file:///nonexistent", "main", "", Credential{})
	if err == nil || !errors.Is(err, errRepoTooLarge) {
		t.Fatalf("want a size-limit error, got %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("size limit did not abort the fetch promptly")
	}
}

func TestGitShow_RejectsOversizedBlob(t *testing.T) {
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}} {
		c := exec.Command("git", args...)
		c.Dir = repo
		if out, err := c.CombinedOutput(); err != nil {
			t.Skipf("git unavailable: %v %s", err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "big.yaml"), make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "-m", "x"}} {
		c := exec.Command("git", args...)
		c.Dir = repo
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	f := &Fetcher{MaxRepoBytes: 1024}
	if _, err := f.gitShow(context.Background(), repo, os.Environ(), "HEAD:big.yaml"); !errors.Is(err, errRepoTooLarge) {
		t.Fatalf("oversized blob must be rejected, got %v", err)
	}
	f.MaxRepoBytes = 4096
	if data, err := f.gitShow(context.Background(), repo, os.Environ(), "HEAD:big.yaml"); err != nil || len(data) != 2048 {
		t.Fatalf("blob under the limit must be read, got %d bytes err=%v", len(data), err)
	}
}
