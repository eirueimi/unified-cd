package main

import (
	"context"
	"io"
	"testing"
)

func TestIsVersionCommand(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}, {"-version"}} {
		if !isVersionCommand(args) {
			t.Errorf("isVersionCommand(%q) = false, want true", args)
		}
	}
	for _, args := range [][]string{nil, {"idle"}, {"cache", "save"}, {"version", "extra"}} {
		if isVersionCommand(args) {
			t.Errorf("isVersionCommand(%q) = true, want false", args)
		}
	}
}

func TestBuildVersion_FallsBackToDev(t *testing.T) {
	// version is the -ldflags override (see version.go); unset, as in a plain
	// `go test` run, buildVersion falls back to "dev".
	old := version
	version = ""
	defer func() { version = old }()

	if got := buildVersion(); got != "dev" {
		t.Errorf("expected %q, got %q", "dev", got)
	}
}

func TestBuildVersion_UsesStampedValue(t *testing.T) {
	old := version
	version = "v1.2.3"
	defer func() { version = old }()

	if got := buildVersion(); got != "v1.2.3" {
		t.Errorf("expected %q, got %q", "v1.2.3", got)
	}
}

// The version subcommand must not need the object store: it has to answer in
// a degraded pod with no S3 configuration, which is exactly when an operator
// is trying to work out which build is running.
func TestRunVersion_NeedsNoStore(t *testing.T) {
	if code := run(context.Background(), erroringProvider, []string{"version"}, io.Discard); code != 0 {
		t.Fatalf("exit=%d, want 0", code)
	}
}
