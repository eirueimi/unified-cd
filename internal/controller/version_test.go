package controller

import "testing"

func TestBuildVersion_FallsBackToDev(t *testing.T) {
	// Version is the -ldflags override (see version.go); unset, as in a plain
	// `go test` run, BuildVersion falls back to "dev".
	old := Version
	Version = ""
	defer func() { Version = old }()

	if got := BuildVersion(); got != "dev" {
		t.Errorf("expected %q, got %q", "dev", got)
	}
}

func TestBuildVersion_UsesStampedValue(t *testing.T) {
	old := Version
	Version = "v1.2.3"
	defer func() { Version = old }()

	if got := BuildVersion(); got != "v1.2.3" {
		t.Errorf("expected %q, got %q", "v1.2.3", got)
	}
}
