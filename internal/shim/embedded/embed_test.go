package embedded

import (
	"bytes"
	"debug/elf"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// committedShims describes the two arch-specific shim binaries committed under
// internal/shim/embedded and go:embedded by embed_amd64.go / embed_arm64.go.
// Both are always GOOS=linux (job containers share the host arch, not the host
// OS); only the CPU architecture differs.
var committedShims = []struct {
	file    string
	arch    string
	machine elf.Machine
}{
	{"ucd-sh-amd64", "amd64", elf.EM_X86_64},
	{"ucd-sh-arm64", "arm64", elf.EM_AARCH64},
}

// minShimSize is a sanity floor: a real static Go ucd-sh binary is multiple MB,
// so anything under 1 MiB is a truncated/placeholder/corrupt commit.
const minShimSize = 1 << 20

// TestBytes asserts the committed ucd-sh binary for THIS GOARCH is embedded and
// stable across calls. The bytes are produced by
// `go generate ./internal/shim/embedded/` (cmd/shimgen) and committed to git,
// so a zero length here means the committed file was truncated or the wrong
// file was committed — a regression, not an expected fresh-clone state.
func TestBytes(t *testing.T) {
	b := Bytes()
	if len(b) == 0 {
		t.Fatalf("Bytes() is empty; the committed ucd-sh-<arch> shim is missing or truncated — run `go generate ./internal/shim/embedded/` and commit")
	}
	if len(Bytes()) != len(b) {
		t.Fatalf("Bytes() not stable across calls")
	}
	t.Logf("embedded ucd-sh is %d bytes", len(b))
}

// TestCommittedShimsAreValidLinuxELF validates BOTH committed shim binaries
// (independent of which arch this test binary was compiled for) are real,
// statically-linked linux ELF executables of the expected CPU architecture.
//
// This is the robustness guarantee that replaced the former byte-exact CI drift
// guard: Go builds are not byte-reproducible across build machines (BuildID and
// other environment-derived bytes differ even for the same GOOS/GOARCH and Go
// version), so no committed bytes can satisfy a `git diff --exit-code` against a
// fresh rebuild. Instead of demanding byte identity, we verify the committed
// artifact is a genuine, well-formed linux shim — which is all `go install` and
// the release build actually need.
func TestCommittedShimsAreValidLinuxELF(t *testing.T) {
	for _, s := range committedShims {
		t.Run(s.arch, func(t *testing.T) {
			data, err := os.ReadFile(s.file)
			if err != nil {
				t.Fatalf("read committed shim %s: %v (run `go generate ./internal/shim/embedded/` and commit)", s.file, err)
			}
			if len(data) < minShimSize {
				t.Fatalf("%s is %d bytes, want >= %d — looks truncated/placeholder, not a real shim", s.file, len(data), minShimSize)
			}

			f, err := elf.NewFile(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("%s is not a valid ELF binary: %v", s.file, err)
			}
			defer f.Close()

			if f.Class != elf.ELFCLASS64 {
				t.Errorf("%s: ELF class = %v, want ELFCLASS64", s.file, f.Class)
			}
			// Go emits ELFOSABI_NONE (SYSV) for linux binaries.
			if f.OSABI != elf.ELFOSABI_NONE && f.OSABI != elf.ELFOSABI_LINUX {
				t.Errorf("%s: OSABI = %v, want NONE/LINUX (a linux binary)", s.file, f.OSABI)
			}
			if f.Machine != s.machine {
				t.Errorf("%s: ELF machine = %v, want %v — wrong-architecture binary committed for this file", s.file, f.Machine, s.machine)
			}
			if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
				t.Errorf("%s: ELF type = %v, want executable (ET_EXEC or PIE ET_DYN)", s.file, f.Type)
			}
			// CGO_ENABLED=0 static build: there must be no dynamic-loader
			// (PT_INTERP) program header. A dynamically linked shim would fail
			// to exec inside a minimal/scratch job container.
			for _, p := range f.Progs {
				if p.Type == elf.PT_INTERP {
					t.Errorf("%s: has PT_INTERP (dynamically linked); a static build (CGO_ENABLED=0) is required", s.file)
				}
			}
		})
	}
}

// TestEmbeddedShimRunsAsUcdSh executes the embedded shim for the current GOARCH
// when the test host can run it (linux + the matching arch, e.g. CI's
// ubuntu-latest amd64 runner) and asserts it behaves as a real ucd-sh, not just
// a valid-looking ELF: no-args prints the usage line and exits 2, and
// `-c "exit N"` actually interprets the script and propagates its exit status.
// On non-linux hosts (a linux binary can't exec there) the test skips; the
// static ELF validation above still covers those platforms.
func TestEmbeddedShimRunsAsUcdSh(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("embedded ucd-sh is a linux/%s binary; cannot execute it on %s", runtime.GOARCH, runtime.GOOS)
	}

	payload := Bytes()
	if len(payload) == 0 {
		t.Fatal("Bytes() is empty; nothing to execute")
	}
	path := filepath.Join(t.TempDir(), "ucd-sh")
	if err := os.WriteFile(path, payload, 0o755); err != nil {
		t.Fatalf("write shim to disk: %v", err)
	}

	// No args: usage to stderr, exit code 2 (see cmd/ucd-sh/main.go).
	var stderr bytes.Buffer
	noArgs := exec.Command(path)
	noArgs.Stderr = &stderr
	err := noArgs.Run()
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 2 {
		t.Fatalf("ucd-sh with no args: want exit code 2, got err=%v", err)
	}
	if !strings.Contains(stderr.String(), "usage: ucd-sh") {
		t.Errorf("ucd-sh with no args: stderr = %q, want it to contain the ucd-sh usage line", stderr.String())
	}

	// `-c "exit 7"`: the shim must interpret the script and exit with 7,
	// proving it is a functioning shell interpreter, not just an ELF that
	// happens to exit 2.
	exit7 := exec.Command(path, "-c", "exit 7")
	err = exit7.Run()
	ee, ok = err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 7 {
		t.Fatalf("ucd-sh -c 'exit 7': want exit code 7, got err=%v", err)
	}
}
