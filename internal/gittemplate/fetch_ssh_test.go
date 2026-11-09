package gittemplate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestBuildSSHCommand(t *testing.T) {
	strict, err := buildSSHCommand("/k/id", "/k/known_hosts", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`IdentityFile="/k/id"`, "StrictHostKeyChecking=yes", `UserKnownHostsFile="/k/known_hosts"`, "GlobalKnownHostsFile=/dev/null", "BatchMode=yes"} {
		if !strings.Contains(strict, want) {
			t.Errorf("strict command %q missing %q", strict, want)
		}
	}
	tofu, err := buildSSHCommand("/k/id", "/data/known_hosts", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tofu, "StrictHostKeyChecking=accept-new") || strings.Contains(tofu, "StrictHostKeyChecking=no") {
		t.Errorf("TOFU command must use accept-new, got %q", tofu)
	}
}

func TestSSHHostFromURL(t *testing.T) {
	cases := map[string]string{
		"ssh://git@example.com:2222/o/r.git": "example.com",
		"ssh://example.com/o/r.git":          "example.com",
		"git@github.com:o/r.git":             "github.com",
		"github.com:o/r.git":                 "github.com",
		"/local/path":                        "",
	}
	for in, want := range cases {
		if got := sshHostFromURL(in); got != want {
			t.Errorf("sshHostFromURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSSHSetup_Modes(t *testing.T) {
	ctx := context.Background()
	f := &Fetcher{KnownHosts: func(_ context.Context, host string) (string, error) {
		if host == "pinned.example.com" {
			return "pinned.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample\n", nil
		}
		return "", nil
	}, KnownHostsFile: filepath.Join(t.TempDir(), "state", "known_hosts")}

	cmd, cleanup, err := f.sshSetup(ctx, "pinned.example.com", "KEY")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, "StrictHostKeyChecking=yes") || strings.Contains(cmd, "state") {
		t.Errorf("pinned host must use strict checking against a private file, got %q", cmd)
	}
	cleanup()

	cmd, cleanup, err = f.sshSetup(ctx, "other.example.com", "KEY")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if !strings.Contains(cmd, "StrictHostKeyChecking=accept-new") || !strings.Contains(cmd, "state/known_hosts") {
		t.Errorf("unpinned host must use accept-new with the persistent file, got %q", cmd)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.KnownHostsFile))); err != nil {
		t.Errorf("known_hosts directory must be created: %v", err)
	}
}

// fakeSSHServer accepts SSH handshakes with the given host key and rejects all
// authentication. A client therefore fails with a host-key error when it does not
// trust the key, and with "Permission denied" when it does.
func fakeSSHServer(t *testing.T, addr string, hostKey ssh.Signer) (net.Listener, int) {
	t.Helper()
	cfg := &ssh.ServerConfig{}
	cfg.AddHostKey(hostKey)
	cfg.PublicKeyCallback = func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
		return nil, fmt.Errorf("denied")
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(10 * time.Second))
				if sc, _, _, err := ssh.NewServerConn(c, cfg); err == nil {
					sc.Close()
				}
			}()
		}
	}()
	return l, l.Addr().(*net.TCPAddr).Port
}

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func clientKeyPEM(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(blk))
}

func requireSSH(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("ssh binary not available")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
}

func TestHostKeyVerification_PinnedKeys(t *testing.T) {
	requireSSH(t)
	serverKey := newSigner(t)
	l, port := fakeSSHServer(t, "127.0.0.1:0", serverKey)
	defer l.Close()
	repoURL := fmt.Sprintf("ssh://git@127.0.0.1:%d/o/r.git", port)
	line := func(s ssh.Signer) string {
		return fmt.Sprintf("[127.0.0.1]:%d %s", port, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey()))))
	}
	key := clientKeyPEM(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Mismatched pinned key: must fail on host key verification.
	bad := &Fetcher{KnownHosts: func(context.Context, string) (string, error) { return line(newSigner(t)), nil }}
	_, err := bad.FetchDir(ctx, repoURL, "main", "", "", key)
	if err == nil || !strings.Contains(err.Error(), "host key verification failed") {
		t.Fatalf("mismatched pinned key must fail host key verification, got %v", err)
	}

	// Matching pinned key: verification passes; the fake server then denies auth.
	good := &Fetcher{KnownHosts: func(context.Context, string) (string, error) { return line(serverKey), nil }}
	_, err = good.FetchDir(ctx, repoURL, "main", "", "", key)
	if err == nil || strings.Contains(err.Error(), "host key verification failed") {
		t.Fatalf("matching pinned key must pass host verification (and then fail auth), got %v", err)
	}
}

func TestHostKeyVerification_TOFUDetectsChangedKey(t *testing.T) {
	requireSSH(t)
	l, port := fakeSSHServer(t, "127.0.0.1:0", newSigner(t))
	repoURL := fmt.Sprintf("ssh://git@127.0.0.1:%d/o/r.git", port)
	khFile := filepath.Join(t.TempDir(), "state", "known_hosts")
	f := &Fetcher{KnownHostsFile: khFile}
	key := clientKeyPEM(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// First contact: the key is trusted and recorded; auth is then denied.
	_, err := f.FetchDir(ctx, repoURL, "main", "", "", key)
	if err == nil || strings.Contains(err.Error(), "host key verification failed") {
		t.Fatalf("first contact must be accepted (then fail auth), got %v", err)
	}
	if b, rerr := os.ReadFile(khFile); rerr != nil || !strings.Contains(string(b), "ssh-ed25519") {
		t.Fatalf("TOFU must persist the host key to %s, got %q err=%v", khFile, b, rerr)
	}

	// Same host, different key: must be rejected.
	l.Close()
	l2, _ := fakeSSHServer(t, fmt.Sprintf("127.0.0.1:%d", port), newSigner(t))
	defer l2.Close()
	_, err = f.FetchDir(ctx, repoURL, "main", "", "", key)
	if err == nil || !strings.Contains(err.Error(), "host key verification failed") {
		t.Fatalf("changed host key must fail the fetch, got %v", err)
	}
}

func TestBuildSSHCommand_QuotesPaths(t *testing.T) {
	cmd, err := buildSSHCommand("/tmp/a b/id", "/tmp/a b/100%/known_hosts", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`'IdentityFile="/tmp/a b/id"'`, `'UserKnownHostsFile="/tmp/a b/100%%/known_hosts"'`} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command %q missing %q", cmd, want)
		}
	}
	if _, err := buildSSHCommand("/k/id", `/k/bad"path/known_hosts`, false); err == nil {
		t.Error("a path with a double quote must be rejected")
	}
	if err := ValidateKnownHostsFile(filepath.Join(t.TempDir(), "x\"y", "known_hosts")); err == nil {
		t.Error("ValidateKnownHostsFile must reject a double quote")
	}
}

func TestValidateKnownHostsFile_RefusesWritableDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ownership and mode checks are Unix-only")
	}
	dir := filepath.Join(t.TempDir(), "kh")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := ValidateKnownHostsFile(filepath.Join(dir, "known_hosts")); err == nil {
		t.Error("a world-writable known_hosts directory must be refused")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ValidateKnownHostsFile(filepath.Join(dir, "known_hosts")); err != nil {
		t.Errorf("a private directory must be accepted: %v", err)
	}
}

// TestHostKeyVerification_PathsWithSpaces guards against ssh splitting a path on
// whitespace: the TOFU file and the pinned known_hosts must be honoured even when
// their directories contain a space.
func TestHostKeyVerification_PathsWithSpaces(t *testing.T) {
	requireSSH(t)
	spaced := filepath.Join(t.TempDir(), "with space")
	if err := os.MkdirAll(spaced, 0o700); err != nil {
		t.Fatal(err)
	}
	// Temp dirs (key + pinned known_hosts) now live under a spaced path too.
	t.Setenv("TMPDIR", spaced)
	t.Setenv("TMP", spaced)
	t.Setenv("TEMP", spaced)

	serverKey := newSigner(t)
	l, port := fakeSSHServer(t, "127.0.0.1:0", serverKey)
	defer l.Close()
	repoURL := fmt.Sprintf("ssh://git@127.0.0.1:%d/o/r.git", port)
	key := clientKeyPEM(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Pinned, wrong key: must be rejected (not silently accepted).
	wrong := fmt.Sprintf("[127.0.0.1]:%d %s", port, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(newSigner(t).PublicKey()))))
	bad := &Fetcher{KnownHosts: func(context.Context, string) (string, error) { return wrong, nil }}
	if _, err := bad.FetchDir(ctx, repoURL, "main", "", "", key); err == nil || !strings.Contains(err.Error(), "host key verification failed") {
		t.Fatalf("pinned mismatch under a spaced path must fail verification, got %v", err)
	}

	// TOFU file under a spaced directory: the key must actually be recorded there.
	khFile := filepath.Join(spaced, "state", "known_hosts")
	f := &Fetcher{KnownHostsFile: khFile}
	if _, err := f.FetchDir(ctx, repoURL, "main", "", "", key); err == nil || strings.Contains(err.Error(), "host key verification failed") {
		t.Fatalf("first contact must be accepted then fail auth, got %v", err)
	}
	if b, err := os.ReadFile(khFile); err != nil || !strings.Contains(string(b), "ssh-ed25519") {
		t.Fatalf("TOFU key must be written to the spaced path, got %q err=%v", b, err)
	}
}
