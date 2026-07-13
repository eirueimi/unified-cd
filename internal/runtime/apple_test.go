package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDriverFor_AppleContainer(t *testing.T) {
	r := driverFor("container")
	require.NotNil(t, r)
	assert.Equal(t, "container", r.Name())
}

func TestAppleContainer_RunArgs(t *testing.T) {
	r := &appleContainer{}
	args := r.runArgs(RunSpec{Image: "alpine", Script: "echo hi", Env: []string{"A=b"}})
	assert.Equal(t, []string{"run", "--rm", "-e", "A=b", "alpine", "sh", "-c", "echo hi"}, args)
}

// TestAppleCreateArgs_ArgsOnly_Positional mirrors
// TestOCICLICreateArgs_ArgsOnly_Positional: no --entrypoint anywhere, tail
// after the image is exactly the args.
func TestAppleCreateArgs_ArgsOnly_Positional(t *testing.T) {
	a := &appleContainer{}
	got := a.createArgs(CreateSpec{Image: "img", Args: []string{"serve", "--port", "80"}})
	assert.NotContains(t, got, "--entrypoint")
	assert.Equal(t, []string{"img", "serve", "--port", "80"}, got[len(got)-4:])
}

// TestAppleCreateArgs_EntrypointOverride_ClearsAndPositions mirrors
// TestOCICLICreateArgs_EntrypointOverride_ClearsAndPositions: --entrypoint ""
// precedes the image, entrypoint+args ride positionally after it.
func TestAppleCreateArgs_EntrypointOverride_ClearsAndPositions(t *testing.T) {
	a := &appleContainer{}
	got := a.createArgs(CreateSpec{Image: "img", Entrypoint: []string{"kubectl"}, Args: []string{"get", "pods"}})
	assert.Equal(t, []string{"--entrypoint", "", "img", "kubectl", "get", "pods"}, got[len(got)-6:])
}

// TestAppleCreateArgs_NoEntrypointNoArgs_Bare mirrors
// TestOCICLICreateArgs_NoEntrypointNoArgs_Bare: no tail, image is the last
// token.
func TestAppleCreateArgs_NoEntrypointNoArgs_Bare(t *testing.T) {
	a := &appleContainer{}
	got := a.createArgs(CreateSpec{Image: "img"})
	assert.Equal(t, "img", got[len(got)-1])
	assert.NotContains(t, got, "--entrypoint")
}

// TestAppleCreateArgs_EntrypointOverride_DegradesOnNoClearRuntime mirrors
// TestOCICLICreateArgs_EntrypointOverride_DegradesOnNoClearRuntime: Apple's
// `container` participates in the same noEmptyEntrypointClear set (its
// support for the empty-clear form is unverified — see the host-entrypoint
// parity design doc).
func TestAppleCreateArgs_EntrypointOverride_DegradesOnNoClearRuntime(t *testing.T) {
	a := &appleContainer{}
	noEmptyEntrypointClear["container"] = true
	defer delete(noEmptyEntrypointClear, "container")
	got := a.createArgs(CreateSpec{Image: "img", Entrypoint: []string{"kubectl"}, Args: []string{"get"}})
	assert.NotContains(t, got, "--entrypoint")
	assert.Equal(t, []string{"img", "kubectl", "get"}, got[len(got)-3:])
}
