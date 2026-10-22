package agent

import "testing"

// The version is a parameter precisely so this table can feed it real values:
// a test binary always has Version == "dev", so a helper that read the package
// variable could only ever be asserted against "latest" and every
// "the tag equals the version" check would pass vacuously.
func TestVersionedImage_Tag(t *testing.T) {
	const repo = "ghcr.io/example/unified-cd-runner"

	tests := []struct {
		name    string
		version string
		wantTag string
	}{
		// The cases from the plan's Task 1 table, verbatim.
		{"release image build, leading v", "v0.7.0", "v0.7.0"},
		{"goreleaser host binary, v stripped", "0.7.0", "v0.7.0"},
		{"pre-release", "v0.7.0-rc1", "v0.7.0-rc1"},
		{"plain go build", "dev", "latest"},
		{"make build, git describe past a tag", "v0.6.0-3-gabc123-dirty", "latest"},
		{"make build, bare SHA", "abc1234", "latest"},
		{"empty", "", "latest"},

		// Neighbours of the above that pin down the edges of the predicate.
		{"goreleaser pre-release", "0.7.0-rc1", "v0.7.0-rc1"},
		{"dotted pre-release", "v1.2.3-rc.1", "v1.2.3-rc.1"},
		{"multi-digit components", "v10.20.30", "v10.20.30"},
		{"git describe past a tag, clean tree", "v0.6.0-3-gabc123", "latest"},
		{"git describe on the tag itself, dirty tree", "v0.6.0-dirty", "latest"},
		{"pre-release tag, dirty tree", "v0.7.0-rc1-dirty", "latest"},
		{"git describe past a pre-release tag", "v0.7.0-rc1-12-g0123abc", "latest"},
		{"two components", "v1.2", "latest"},
		{"four components", "v1.2.3.4", "latest"},
		{"build metadata", "v0.7.0+build5", "latest"},
		{"dangling pre-release hyphen", "v0.7.0-", "latest"},
		{"uppercase V", "V0.7.0", "latest"},
		{"leading whitespace", " v0.7.0", "latest"},
		{"trailing newline", "v0.7.0\n", "latest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := repo + ":" + tt.wantTag
			if got := VersionedImage(repo, tt.version); got != want {
				t.Errorf("VersionedImage(%q, %q) = %q, want %q", repo, tt.version, got, want)
			}
		})
	}
}

// The repository is returned verbatim, including a registry host with a port,
// so a mirrored registry keeps working.
func TestVersionedImage_RepositoryIsPreserved(t *testing.T) {
	got := VersionedImage("registry.internal:5000/ci/unified-cd-runner", "0.7.0")
	want := "registry.internal:5000/ci/unified-cd-runner:v0.7.0"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// DefaultRunnerImage is the single source of the runner repository for both the
// host agent and the Kubernetes agent, so it must follow the stamped Version.
func TestDefaultRunnerImage_FollowsVersion(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })

	Version = "v9.9.9"
	if got, want := DefaultRunnerImage(), "ghcr.io/eirueimi/unified-cd-runner:v9.9.9"; got != want {
		t.Errorf("DefaultRunnerImage() = %q, want %q", got, want)
	}
	Version = "dev"
	if got, want := DefaultRunnerImage(), "ghcr.io/eirueimi/unified-cd-runner:latest"; got != want {
		t.Errorf("DefaultRunnerImage() = %q, want %q", got, want)
	}
}
