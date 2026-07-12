package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun_DashCEchoExitZero(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"-c", "echo hi"}, strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if got := strings.TrimRight(out.String(), "\n"); got != "hi" {
		t.Fatalf("stdout = %q, want %q", got, "hi")
	}
}

func TestRun_DashCExitCode(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"-c", "exit 5"}, strings.NewReader(""), &out, &errOut)
	if code != 5 {
		t.Fatalf("exit code = %d, want 5", code)
	}
}

func TestRun_NoArgsUsageExitTwo(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run(nil, strings.NewReader(""), &out, &errOut)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "usage:") {
		t.Fatalf("stderr = %q, want a usage message", errOut.String())
	}
}

func TestRun_UnknownCommandUsageExitTwo(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"bogus"}, strings.NewReader(""), &out, &errOut)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestRun_DashCWrongArgCountUsageExitTwo(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"-c"}, strings.NewReader(""), &out, &errOut)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestRun_InstallDelegatesToShim(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "copy.bin")

	var out, errOut bytes.Buffer
	code := run([]string{"--install", dest}, strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat installed copy: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("installed copy is empty")
	}
}

// TestCLI_EndToEnd builds the real ucd-sh binary and drives it as a
// subprocess through its full CLI contract: -c executes a script with the
// right exit code, --install produces a working copy of itself, and that
// installed copy is itself a fully functional ucd-sh (it can run -c too).
func TestCLI_EndToEnd(t *testing.T) {
	bin := buildUcdSh(t)

	t.Run("dash_c_stdout_and_exit_code", func(t *testing.T) {
		out, err := exec.Command(bin, "-c", "echo hello; exit 7").CombinedOutput()
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("expected *exec.ExitError, got %v (output: %s)", err, out)
		}
		if exitErr.ExitCode() != 7 {
			t.Fatalf("exit code = %d, want 7", exitErr.ExitCode())
		}
		if got := strings.TrimSpace(string(out)); got != "hello" {
			t.Fatalf("output = %q, want %q", got, "hello")
		}
	})

	t.Run("dash_c_sanitizer_warning_on_stderr", func(t *testing.T) {
		cmd := exec.Command(bin, "-c", "trap 'echo cleaned' TERM EXIT")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("run: %v (stderr: %s)", err, stderr.String())
		}
		if got := strings.TrimSpace(stdout.String()); got != "cleaned" {
			t.Fatalf("stdout = %q, want %q", got, "cleaned")
		}
		if !strings.HasPrefix(stderr.String(), "[ucd-sh] ") {
			t.Fatalf("stderr = %q, want it prefixed with %q", stderr.String(), "[ucd-sh] ")
		}
	})

	t.Run("install_produces_a_working_copy", func(t *testing.T) {
		// Deliberately avoid "install" in the filename: Windows' UAC
		// installer-detection heuristic auto-requests elevation for
		// unsigned executables whose name matches *install*/*setup*/etc,
		// which has nothing to do with our copy logic.
		dest := filepath.Join(t.TempDir(), "ucd-sh-copy.exe")
		out, err := exec.Command(bin, "--install", dest).CombinedOutput()
		if err != nil {
			t.Fatalf("--install: %v (output: %s)", err, out)
		}

		out, err = exec.Command(dest, "-c", "echo works-from-installed-copy").CombinedOutput()
		if err != nil {
			t.Fatalf("exec installed copy: %v (output: %s)", err, out)
		}
		if got := strings.TrimSpace(string(out)); got != "works-from-installed-copy" {
			t.Fatalf("output = %q, want %q", got, "works-from-installed-copy")
		}
	})
}

// buildUcdSh compiles cmd/ucd-sh into a temp directory once per test binary
// run and returns the resulting executable's path.
func buildUcdSh(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "ucd-sh-under-test.exe")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build ./cmd/ucd-sh: %v\n%s", err, out)
	}
	return bin
}
