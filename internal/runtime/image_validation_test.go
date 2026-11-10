package runtime

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// A leading '-' would be parsed by the container CLI as an option, so every
// entry point must refuse it before building argv or spawning a process.
func TestRuntimes_RejectOptionLikeImage(t *testing.T) {
	ctx := context.Background()
	for _, rt := range []interface {
		Pull(context.Context, string) error
	}{&ociCLI{bin: "docker"}, &appleContainer{}} {
		require.Error(t, rt.Pull(ctx, "--privileged"))
	}

	oci := &ociCLI{bin: "docker"}
	_, err := oci.Run(ctx, RunSpec{Image: "-v", Script: "true"}, &bytes.Buffer{}, &bytes.Buffer{})
	require.ErrorContains(t, err, "must not start with '-'")
	_, err = oci.createArgs(CreateSpec{Image: "--rm"})
	require.Error(t, err)
	_, err = oci.createArgs(CreateSpec{Image: "a b"})
	require.Error(t, err)
	_, err = oci.ImageUser(ctx, "-x")
	require.Error(t, err)

	apple := &appleContainer{}
	_, err = apple.Run(ctx, RunSpec{Image: "-v"}, &bytes.Buffer{}, &bytes.Buffer{})
	require.Error(t, err)
	_, err = apple.createArgs(CreateSpec{Image: "--rm"})
	require.Error(t, err)
	_, err = apple.createArgs(CreateSpec{Image: "localhost:5000/a/b:1@sha256:abc"})
	require.NoError(t, err)
}
