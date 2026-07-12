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
	// WorkDir sets the container's working directory (docker/podman/Apple
	// container's `run -w`), so scoped `run:` steps and `exec` default there
	// instead of an undefined working directory. Empty means "no -w flag"
	// (driver default). See scope.go's scopeWorkDir for the host agent's value.
	WorkDir string
	// Mounts are host-path bind mounts (docker `run -v`). Empty means no bind
	// mounts (an isolated uses-scope container); a named runsIn.container
	// container sets one mount to share the host workspace.
	Mounts []Mount
	// NetworkContainer joins the created container into another container's
	// network namespace (docker/podman/nerdctl `--network container:<id>`).
	// Used by the host agent's claim pod: every claim container joins the
	// pause container's netns so sidecars are reachable on localhost,
	// mirroring a k8s pod. Empty = default network.
	NetworkContainer string
	// Command is the container's argv, appended after the image in `run -d`
	// (docker/podman/Apple container semantics: overrides the image's CMD,
	// keeping ENTRYPOINT intact). Nil/empty means the image's default
	// entrypoint/CMD runs unmodified — required for podTemplate sidecars
	// (e.g. mysql, redis) whose own image entrypoint IS their service.
	// Callers that need a long-lived exec target (the uses-scope container,
	// the claim pod's primary "job" container, the claim pod's pause
	// container) must set this explicitly, e.g. []string{"sleep", "infinity"}.
	Command []string
}

// Mount is a host-path bind mount for a long-lived container: the host
// directory HostPath is made available inside the container at ContainerPath
// (docker/podman/Apple container's `run -v host:container`). Used to share the
// host workspace with a named runsIn.container container, and to inject the
// ucd-sh shim read-only at /.ucd (see ReadOnly).
type Mount struct {
	HostPath      string
	ContainerPath string
	// ReadOnly emits the `:ro` suffix on the `-v host:container` mount arg
	// (docker/podman/Apple container semantics). Used for the /.ucd shim
	// mount, which must never be writable inside the container: the shim
	// binary is shared read-only from the agent's tools dir across every
	// container of a claim, and a writable mount would let a step
	// overwrite or delete it out from under sibling containers.
	ReadOnly bool
}

// ExecSpec describes one script execution inside a running container.
type ExecSpec struct {
	Script string
	Env    []string // KEY=VALUE, injected as -e on exec
	// Shell is the interpreter argv the script is appended to (exec'd as
	// Shell + [Script], verbatim, never re-parsed or quoted). The agent is
	// the layer that decides the effective shell (step.shell resolved by
	// the controller onto api.ClaimStep.Shell, defaulting to the injected
	// shim's ["/.ucd/ucd-sh", "-c"] when unset) and always sets this field
	// explicitly on every exec it issues — see internal/agent/claim_pod.go
	// and scope.go — so this runtime package stays free of any shim/DSL
	// knowledge. Empty/nil is a fallback for callers outside the agent
	// (direct package users, tests) that don't set it explicitly; ociCLI
	// and appleContainer both fall back to {"sh","-c"} in that case, NOT
	// the shim default, since this package has no notion of /.ucd.
	Shell []string
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

// detectOrder is the auto-detection preference order. Apple's `container` is
// deliberately excluded: its runtime can't join another container's network
// namespace, so it cannot back a claim pod (see appleContainer.Create), and
// silently auto-selecting it would strand isolated jobs. It remains selectable
// explicitly via --container-runtime container (driverFor still knows it).
var detectOrder = []string{"docker", "podman", "nerdctl", "wslc"}

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
