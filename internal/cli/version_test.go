package cli

import (
	"strings"
	"testing"
)

func TestVersionCmd_PrintsDefaultDev(t *testing.T) {
	// version is the package-level build-time override (see version.go); when
	// unset (as in a plain `go test` run, with no -ldflags -X), buildVersion()
	// falls back to "dev".
	old := version
	version = ""
	defer func() { version = old }()

	cmd := newVersionCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "dev" {
		t.Errorf("expected %q, got %q", "dev", got)
	}
}

func TestVersionCmd_PrintsOverriddenVersion(t *testing.T) {
	old := version
	version = "v1.2.3"
	defer func() { version = old }()

	cmd := newVersionCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "v1.2.3" {
		t.Errorf("expected %q, got %q", "v1.2.3", got)
	}
}

func TestRoot_HasVersionSubcommand(t *testing.T) {
	root := NewRoot()
	cmd, _, err := root.Find([]string{"version"})
	if err != nil {
		t.Fatalf("find version command: %v", err)
	}
	if cmd.Use != "version" {
		t.Errorf("expected the version command, got %q", cmd.Use)
	}
}
