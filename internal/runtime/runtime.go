// Package runtime abstracts container runtimes behind a small, CRI-inspired
// lifecycle interface (image pull + run). Implementations shell out to a CLI
// (docker/podman/nerdctl/wslc/Apple container) — CRI/gRPC is intentionally
// NOT used; the target runtimes are CLI tools, not CRI endpoints.
package runtime

import (
	"context"
	"fmt"
	"io"
	"os/exec"
)

// RunSpec describes a one-shot containerized step execution.
type RunSpec struct {
	Image    string   // OCI image reference
	Script   string   // shell script to run (the step's run:)
	Env      []string // KEY=VALUE, injected as -e
	Shell    []string // entrypoint; defaults to {"sh","-c"}
	CPULimit string   // container CPU limit in cores (e.g. "0.5"); empty = no limit
	MemLimit string   // container memory limit in bytes (e.g. "536870912"); empty = no limit
}

// ContainerHandle identifies a running long-lived container (scope environment).
type ContainerHandle struct{ ID string }

// CreateSpec describes a detached long-lived container for a uses scope.
type CreateSpec struct {
	Image    string
	Env      []string // KEY=VALUE, injected as -e
	CPULimit string
	MemLimit string
}

// ExecSpec describes one script execution inside a running container.
type ExecSpec struct {
	Script string
	Env    []string // KEY=VALUE, injected as -e on exec
	Shell  []string // defaults to {"sh","-c"}
}

// ContainerRuntime runs a step in a fresh, isolated container. No host
// workspace is mounted — inputs arrive via Env, outputs via stdout.
type ContainerRuntime interface {
	Name() string
	Available() bool
	Pull(ctx context.Context, image string) error
	Run(ctx context.Context, spec RunSpec, stdout, stderr io.Writer) (int, error)

	// Long-lived scope lifecycle (uses-level runsIn.image).
	Create(ctx context.Context, spec CreateSpec) (ContainerHandle, error)
	Exec(ctx context.Context, h ContainerHandle, spec ExecSpec, stdout, stderr io.Writer) (int, error)
	CopyIn(ctx context.Context, h ContainerHandle, hostPath, containerPath string) error
	CopyOut(ctx context.Context, h ContainerHandle, containerPath, hostPath string) error
	Remove(ctx context.Context, h ContainerHandle) error
}

// detectOrder is the auto-detection preference order.
var detectOrder = []string{"docker", "podman", "nerdctl", "wslc", "container"}

// Detect returns the first available runtime. If preferred is non-empty, only
// that runtime is considered (and it must be a known driver).
func Detect(preferred string) (ContainerRuntime, error) {
	order := detectOrder
	if preferred != "" {
		order = []string{preferred}
	}
	for _, name := range order {
		r := driverFor(name)
		if r == nil {
			if preferred != "" {
				return nil, fmt.Errorf("unknown container runtime %q", preferred)
			}
			continue
		}
		if r.Available() {
			return r, nil
		}
	}
	return nil, fmt.Errorf("no container runtime available (looked for %v)", order)
}

// driverFor maps a runtime name to a driver, or nil if unknown.
func driverFor(name string) ContainerRuntime {
	switch name {
	case "docker", "podman", "nerdctl", "wslc":
		return &ociCLI{bin: name}
	case "container":
		return &appleContainer{}
	default:
		return nil
	}
}

// lookPath is indirected for testability.
var lookPath = exec.LookPath
