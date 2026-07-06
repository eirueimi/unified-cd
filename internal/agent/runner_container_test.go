package agent

import (
	"bytes"
	"context"
	"io"
	"testing"

	crt "github.com/eirueimi/unified-cd/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeRuntime struct {
	gotSpec crt.RunSpec
	stdout  string
	exit    int
}

func (f *fakeRuntime) Name() string                       { return "fake" }
func (f *fakeRuntime) Available() bool                    { return true }
func (f *fakeRuntime) Pull(context.Context, string) error { return nil }
func (f *fakeRuntime) Run(_ context.Context, spec crt.RunSpec, stdout, _ io.Writer) (int, error) {
	f.gotSpec = spec
	_, _ = stdout.Write([]byte(f.stdout))
	return f.exit, nil
}

func (f *fakeRuntime) Create(context.Context, crt.CreateSpec) (crt.ContainerHandle, error) {
	return crt.ContainerHandle{}, nil
}
func (f *fakeRuntime) Exec(context.Context, crt.ContainerHandle, crt.ExecSpec, io.Writer, io.Writer) (int, error) {
	return 0, nil
}
func (f *fakeRuntime) CopyIn(context.Context, crt.ContainerHandle, string, string) error  { return nil }
func (f *fakeRuntime) CopyOut(context.Context, crt.ContainerHandle, string, string) error { return nil }
func (f *fakeRuntime) Remove(context.Context, crt.ContainerHandle) error                  { return nil }

func TestRunStepContainer_StreamsStdoutAndPassesEnv(t *testing.T) {
	f := &fakeRuntime{stdout: "built\n", exit: 0}
	var stdout, stderr bytes.Buffer
	code, err := RunStepContainer(context.Background(), f, "golang:1.22", "go build",
		&stdout, &stderr, []string{"FOO=bar"}, "", "")
	require.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Equal(t, "built\n", stdout.String())
	assert.Equal(t, "golang:1.22", f.gotSpec.Image)
	assert.Equal(t, "go build", f.gotSpec.Script)
	assert.Contains(t, f.gotSpec.Env, "FOO=bar")
}
