package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/eirueimi/unified-cd/internal/agent"
)

// setAgentVersion stamps agent.Version for one test. A test binary always
// reports "dev", which maps to ":latest" -- a test reading the default image
// without stamping a version could not tell "derived from the version" from
// "hard-coded :latest".
func setAgentVersion(t *testing.T, v string) {
	t.Helper()
	old := agent.Version
	agent.Version = v
	t.Cleanup(func() { agent.Version = old })
}

// TestDefaultRunnerImageCarriesNoDigest guards the invariant that replaced
// digest pinning: a unified-cd default image is named by a tag derived from the
// agent's own version, never by a digest.
//
// A digest of an image published from this repository can only be recorded in
// a commit AFTER the image is pushed, so a digest pin can only ever name the
// PREVIOUS release -- the agent of release N would permanently pull release
// N-1's runner. A reader who sees a digest-less default next to the busybox pin
// and "fixes" the inconsistency reintroduces that skew and the manual rotation
// chore. See docs/superpowers/specs/2026-10-22-version-derived-default-images-design.md.
func TestDefaultRunnerImageCarriesNoDigest(t *testing.T) {
	assert.NotContains(t, defaultRunnerImage(), "@sha256:",
		"the default runner image must be version-derived, not digest-pinned: a digest "+
			"for an image built from this tree can only name the previous release")
}

// TestDefaultRunnerImageResolvesThroughVersionedImage pins that the runner
// default is computed from the stamped version rather than holding a literal
// tag. Without this a literal ":v0.0.3" would satisfy the no-digest test above
// and silently reintroduce the skew.
func TestDefaultRunnerImageResolvesThroughVersionedImage(t *testing.T) {
	setAgentVersion(t, "v9.9.9")
	assert.Equal(t, agent.VersionedImage("ghcr.io/eirueimi/unified-cd-runner", "v9.9.9"), defaultRunnerImage())
	assert.Equal(t, "ghcr.io/eirueimi/unified-cd-runner:v9.9.9", defaultRunnerImage())

	setAgentVersion(t, "dev")
	assert.Equal(t, "ghcr.io/eirueimi/unified-cd-runner:latest", defaultRunnerImage(),
		"a build that is not a release must fall back to :latest")
}

// TestDefaultPauseImageStaysDigestPinned guards the one default that is
// DELIBERATELY not version-derived. busybox is a third-party image with no
// unified-cd release identity, so there is no release it could be in lockstep
// with and no rotation chore tied to one; dropping its digest would only weaken
// the supply-chain posture. If this test fails because someone made the pause
// image consistent with its siblings, the change is wrong, not the test.
func TestDefaultPauseImageStaysDigestPinned(t *testing.T) {
	setAgentVersion(t, "v9.9.9")
	assert.Regexp(t, `^busybox:[^@]+@sha256:[0-9a-f]{64}$`, defaultPauseImage,
		"the pause image must stay a busybox image pinned to a complete sha256 digest")
	assert.False(t, strings.Contains(defaultPauseImage, "9.9.9") || strings.Contains(defaultPauseImage, "unified-cd"),
		"the pause image must not be derived from the agent version: busybox has no unified-cd release")
}
