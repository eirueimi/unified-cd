package shim

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestMain lets the test binary double as the child command: when
// UCD_ENVEXEC_HELPER is set the binary prints its environment / argv instead
// of running tests, so RunEnv is exercised portably without any external tool.
func TestMain(m *testing.M) {
	switch os.Getenv("UCD_ENVEXEC_HELPER") {
	case "dump":
		for _, a := range os.Args[1:] {
			fmt.Printf("ARG=%s\n", a)
		}
		for _, e := range os.Environ() {
			fmt.Printf("ENV=%s\n", e)
		}
		os.Exit(0)
	case "exit7":
		os.Exit(7)
	}
	os.Exit(m.Run())
}

func runEnv(t *testing.T, base []string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := RunEnv(args, base, strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

func self(t *testing.T) string {
	t.Helper()
	p, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunEnv_SetsAndOverrides(t *testing.T) {
	code, out, errOut := runEnv(t, []string{"KEEP=1", "FOO=old"},
		"FOO=new", "BAR=baz", "UCD_ENVEXEC_HELPER=dump", "--", self(t), "x")
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	for _, want := range []string{"ENV=FOO=new", "ENV=BAR=baz", "ENV=KEEP=1", "ARG=x"} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	if strings.Contains(out, "FOO=old") {
		t.Errorf("old value leaked: %q", out)
	}
}

func TestRunEnv_ArgvWithSpacesAndEmptyValue(t *testing.T) {
	code, out, _ := runEnv(t, nil, "EMPTY=", "SP=a b  c", "UCD_ENVEXEC_HELPER=dump", "--", self(t), "one two", "")
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
	for _, want := range []string{"ARG=one two\n", "ARG=\n", "ENV=EMPTY=\n", "ENV=SP=a b  c\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
}

func TestRunEnv_ExitCodePassthrough(t *testing.T) {
	code, _, _ := runEnv(t, nil, "UCD_ENVEXEC_HELPER=exit7", "--", self(t))
	if code != 7 {
		t.Fatalf("code=%d want 7", code)
	}
}

func TestRunEnv_MissingCommand127(t *testing.T) {
	code, _, errOut := runEnv(t, nil, "A=1", "--", "definitely-not-a-command-xyz")
	if code != 127 {
		t.Fatalf("code=%d want 127", code)
	}
	if !strings.Contains(errOut, "definitely-not-a-command-xyz") {
		t.Errorf("stderr should name the command: %q", errOut)
	}
}

func TestRunEnv_ResolvesViaNewPATH(t *testing.T) {
	dir := t.TempDir()
	name := "ucd-envexec-tool"
	dst := filepath.Join(dir, name)
	if runtime.GOOS == "windows" {
		name += ".exe"
		dst += ".exe"
	}
	data, err := os.ReadFile(self(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		t.Fatal(err)
	}
	// The base env has an unusable PATH; only the PATH set on the command line
	// can locate the tool (as coreutils env does).
	code, out, errOut := runEnv(t, []string{"PATH=/nonexistent"},
		"PATH="+dir, "UCD_ENVEXEC_HELPER=dump", "--", name)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut)
	}
	if !strings.Contains(out, "ENV=PATH="+dir+"\n") {
		t.Errorf("PATH not applied: %q", out)
	}
}

func TestRunEnv_UsageErrors(t *testing.T) {
	for name, args := range map[string][]string{
		"no separator":  {"A=1"},
		"no command":    {"A=1", "--"},
		"not an assign": {"A", "--", "x"},
		"empty name":    {"=v", "--", "x"},
	} {
		code, _, errOut := runEnv(t, nil, args...)
		if code != 2 {
			t.Errorf("%s: code=%d want 2 (stderr %q)", name, code, errOut)
		}
	}
}
