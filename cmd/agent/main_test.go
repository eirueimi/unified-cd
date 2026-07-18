package main

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
)

// digestPinPattern requires a well-formed, complete 64-hex-character sha256
// digest anchored at the end of the string. A plain `strings.Contains(img,
// "@sha256:")` check would still pass for a truncated or malformed digest
// (e.g. "@sha256:" with nothing after it, or a short-copy-pasted prefix) —
// which is exactly the kind of pin that looks correct at a glance but
// silently resolves to the wrong (or no) image at pull time.
var digestPinPattern = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

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
		assert.Regexp(t, digestPinPattern, img,
			"default %s image must be pinned to a well-formed, complete sha256 digest — "+
				"a mutable tag (or a truncated/malformed digest) lets whoever controls the "+
				"registry repository force-push it and execute code in every isolated job's "+
				"primary container fleet-wide", name)
	}
}
