package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDefaultImagesAreDigestPinned guards against the fleet-wide default
// images regressing to mutable tags. A mutable tag lets whoever controls the
// registry repository force-push it and execute code in the primary
// container of every isolated job on every agent lacking a podTemplate job
// container (see claimNeedsRunnerImage in internal/agent/claim_pod.go).
func TestDefaultImagesAreDigestPinned(t *testing.T) {
	for name, img := range map[string]string{
		"runner": defaultRunnerImage,
		"pause":  defaultPauseImage,
	} {
		assert.Contains(t, img, "@sha256:", "default %s image must be digest-pinned", name)
	}
}
