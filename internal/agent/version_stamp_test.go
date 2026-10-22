package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stampFlag is the linker flag prefix that sets agent.Version. Version-derived
// default images (VersionedImage) only work if every released agent binary is
// built with it: rename the variable in one place and the binary silently
// reports "dev", so its default images fall back to :latest and the "an agent
// pulls its own release's images" guarantee is gone with nothing failing.
//
// This is a tripwire over the release build files, not a parser. It reads them
// from disk and looks for the flag as a plain string.
const stampFlag = "-X github.com/eirueimi/unified-cd/internal/agent.Version="

// repoFile reads a file relative to the repository root (this package is two
// directories below it).
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(b)
}

// TestGoreleaserStampsAgentVersion pins that the goreleaser build of the host
// agent (the binary built from ./cmd/unified-cd-agent) still sets
// internal/agent.Version. Other builds in the file stamp their own variables
// and are not the concern of this test.
func TestGoreleaserStampsAgentVersion(t *testing.T) {
	src := repoFile(t, ".goreleaser.yaml")

	found := false
	// Each build entry starts with "  - id:"; split on it so the stamp must sit
	// in the same entry as the agent's main package, not merely somewhere in
	// the file.
	for _, block := range strings.Split(src, "\n  - id:") {
		if !strings.Contains(block, "main: ./cmd/unified-cd-agent") {
			continue
		}
		found = true
		if !strings.Contains(block, stampFlag) {
			t.Errorf(".goreleaser.yaml: the build of ./cmd/unified-cd-agent no longer passes %q in its ldflags; "+
				"release host agents would report \"dev\" and default to :latest images", stampFlag)
		}
	}
	if !found {
		t.Error(".goreleaser.yaml: no build of ./cmd/unified-cd-agent found; update this test if the agent build moved")
	}
}

// TestDockerfilesStampAgentVersion pins the same coupling for the images.
// Every Dockerfile under docker/ must be listed, so a new one cannot be added
// without deciding whether it ships an agent binary.
func TestDockerfilesStampAgentVersion(t *testing.T) {
	// true: builds an agent binary and must stamp agent.Version.
	// false: must not be required to, and the reason is the map value below.
	mustStamp := map[string]bool{
		"agent.Dockerfile":     true,
		"k8s-agent.Dockerfile": true,
	}
	exempt := map[string]string{
		"runner.Dockerfile":           "builds no Go binary (a toolbox of third-party CLIs), so there is nothing to stamp; agents find it by deriving its tag from their own version",
		"controller.Dockerfile":       "builds the controller, which stamps internal/controller.Version, not the agent's",
		"artifact-sidecar.Dockerfile": "builds the sidecar, which stamps main.version, not the agent's",
		"dev.Dockerfile":              "a development image, not a released one",
		"vite.Dockerfile":             "builds the web UI with node; no Go binary",
	}

	files, err := filepath.Glob(filepath.Join("..", "..", "docker", "*.Dockerfile"))
	if err != nil || len(files) == 0 {
		t.Fatalf("finding docker/*.Dockerfile: %v (found %d)", err, len(files))
	}
	for _, f := range files {
		name := filepath.Base(f)
		if _, ok := exempt[name]; ok {
			t.Logf("%s not checked: %s", name, exempt[name])
			continue
		}
		if !mustStamp[name] {
			t.Errorf("docker/%s is not classified in this test: add it to mustStamp if it builds an agent binary, "+
				"or to exempt with the reason if it does not", name)
			continue
		}
		if !strings.Contains(repoFile(t, "docker/"+name), stampFlag) {
			t.Errorf("docker/%s no longer passes %q to go build; the image's agent would report \"dev\" and default to :latest images",
				name, stampFlag)
		}
	}
	for name := range mustStamp {
		if _, err := os.Stat(filepath.Join("..", "..", "docker", name)); err != nil {
			t.Errorf("docker/%s is listed in mustStamp but cannot be read: %v", name, err)
		}
	}
}
