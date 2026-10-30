//go:build integration

package runtime

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestOCICLI_EnvReachesContainerWithoutArgv runs against a real docker/podman:
// values supplied through Create and Exec (including a key that collides with
// a CLI variable and so takes the --env-file path) must arrive in the
// container.
func TestOCICLI_EnvReachesContainerWithoutArgv(t *testing.T) {
	rt, err := Detect("")
	if err != nil {
		t.Skipf("no container runtime available: %v", err)
	}
	cr, ok := rt.(scopeRuntime)
	if !ok {
		t.Skip("runtime has no Create/Exec")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	const image = "alpine:3"
	require.NoError(t, rt.Pull(ctx, image))
	h, err := cr.Create(ctx, CreateSpec{Image: image, Env: []string{"CREATE_SECRET=from-create"}, Entrypoint: []string{"sleep"}, Args: []string{"300"}})
	require.NoError(t, err)
	defer func() { _ = cr.Remove(context.Background(), h) }()

	var out, errb bytes.Buffer
	code, err := cr.Exec(ctx, h, ExecSpec{
		Script: `printf '%s|%s|%s' "$CREATE_SECRET" "$EXEC_SECRET" "$HOME"`,
		Env:    []string{"EXEC_SECRET=from-exec", "HOME=/custom-home"},
	}, &out, &errb)
	require.NoError(t, err, errb.String())
	require.Equal(t, 0, code, errb.String())
	require.Equal(t, "from-create|from-exec|/custom-home", out.String())
}
