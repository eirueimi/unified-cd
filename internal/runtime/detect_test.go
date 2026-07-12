package runtime

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDetect_DoesNotAutoDetectAppleContainer pins the soft-unsupported policy for
// Apple's `container` runtime: auto-detection must never fall back to it (its
// netns model can't back a claim pod — see appleContainer.Create), but an
// explicit --container-runtime container still resolves the driver.
func TestDetect_DoesNotAutoDetectAppleContainer(t *testing.T) {
	// Only Apple's `container` CLI is on PATH; no docker/podman/nerdctl/wslc.
	orig := lookPath
	t.Cleanup(func() { lookPath = orig })
	lookPath = func(file string) (string, error) {
		if file == "container" {
			return "/usr/bin/container", nil
		}
		return "", exec.ErrNotFound
	}

	// Auto-detect (empty preference) must NOT pick Apple container.
	_, err := Detect("")
	assert.Error(t, err, "auto-detect must not fall back to Apple container")

	// Explicit selection still works — the driver is de-listed from auto-detect,
	// not removed.
	r, err := Detect("container")
	require.NoError(t, err)
	assert.Equal(t, "container", r.Name())
}
