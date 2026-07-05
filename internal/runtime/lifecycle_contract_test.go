package runtime

import (
	"context"
	"io"
	"testing"
)

// scopeRuntime is the subset of ContainerRuntime the agent scope manager needs.
type scopeRuntime interface {
	Create(ctx context.Context, spec CreateSpec) (ContainerHandle, error)
	Exec(ctx context.Context, h ContainerHandle, spec ExecSpec, stdout, stderr io.Writer) (int, error)
	CopyIn(ctx context.Context, h ContainerHandle, hostPath, containerPath string) error
	CopyOut(ctx context.Context, h ContainerHandle, containerPath, hostPath string) error
	Remove(ctx context.Context, h ContainerHandle) error
}

func TestOCICLISatisfiesScopeRuntime(t *testing.T) {
	var _ scopeRuntime = &ociCLI{bin: "docker"}
	var _ ContainerRuntime = &ociCLI{bin: "docker"}
}
