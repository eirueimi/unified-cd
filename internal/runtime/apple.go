package runtime

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// appleContainer drives Apple's native `container` CLI on macOS. Its surface
// is docker-like; verify flags against the installed CLI and adjust runArgs
// if they diverge.
type appleContainer struct{}

func (a *appleContainer) Name() string { return "container" }

func (a *appleContainer) Available() bool {
	_, err := lookPath("container")
	return err == nil
}

func (a *appleContainer) runArgs(spec RunSpec) []string {
	args := []string{"run", "--rm"}
	for _, e := range spec.Env {
		args = append(args, "-e", e)
	}
	args = append(args, spec.Image)
	shell := spec.Shell
	if len(shell) == 0 {
		shell = []string{"sh", "-c"}
	}
	args = append(args, shell...)
	args = append(args, spec.Script)
	return args
}

func (a *appleContainer) Pull(ctx context.Context, image string) error {
	return execCommand(ctx, "container", "pull", image).Run()
}

func (a *appleContainer) Run(ctx context.Context, spec RunSpec, stdout, stderr io.Writer) (int, error) {
	cmd := execCommand(ctx, "container", a.runArgs(spec)...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	return -1, err
}

// Create, Exec, CopyIn, CopyOut, and Remove mirror ociCLI's argv shape:
// Apple's `container` CLI is docker-compatible for run/exec/cp/rm (see
// runArgs/Pull/Run above, which already used the same grammar before this
// change).
// createArgs builds the argv for `run -d`; extracted from Create so tests can
// assert on the argv (notably -w for spec.WorkDir) without depending on
// exec.Cmd.Output()'s stdout plumbing. Mirrors ociCLI.createArgs.
func (a *appleContainer) createArgs(spec CreateSpec) []string {
	args := []string{"run", "-d"}
	if spec.CPULimit != "" {
		args = append(args, "--cpus", spec.CPULimit)
	}
	if spec.MemLimit != "" {
		args = append(args, "--memory", spec.MemLimit)
	}
	if spec.WorkDir != "" {
		args = append(args, "-w", spec.WorkDir)
	}
	for _, m := range spec.Mounts {
		args = append(args, "-v", m.HostPath+":"+m.ContainerPath)
	}
	for _, e := range spec.Env {
		args = append(args, "-e", e)
	}
	args = append(args, spec.Image, "sleep", "infinity")
	return args
}

func (a *appleContainer) Create(ctx context.Context, spec CreateSpec) (ContainerHandle, error) {
	if spec.NetworkContainer != "" {
		return ContainerHandle{}, fmt.Errorf("apple container: joining another container's network namespace is not supported (claim pod requires docker/podman/nerdctl)")
	}
	args := a.createArgs(spec)
	out, err := execCommand(ctx, "container", args...).Output()
	if err != nil {
		return ContainerHandle{}, fmt.Errorf("container run -d: %w", err)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return ContainerHandle{}, fmt.Errorf("container run -d: empty container id")
	}
	return ContainerHandle{ID: id}, nil
}

func (a *appleContainer) Exec(ctx context.Context, h ContainerHandle, spec ExecSpec, stdout, stderr io.Writer) (int, error) {
	args := []string{"exec"}
	for _, e := range spec.Env {
		args = append(args, "-e", e)
	}
	shell := spec.Shell
	if len(shell) == 0 {
		shell = []string{"sh", "-c"}
	}
	args = append(args, h.ID)
	args = append(args, shell...)
	args = append(args, spec.Script)
	cmd := execCommand(ctx, "container", args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	return -1, err
}

func (a *appleContainer) CopyIn(ctx context.Context, h ContainerHandle, hostPath, containerPath string) error {
	return execCommand(ctx, "container", "cp", hostPath, h.ID+":"+containerPath).Run()
}

func (a *appleContainer) CopyOut(ctx context.Context, h ContainerHandle, containerPath, hostPath string) error {
	return execCommand(ctx, "container", "cp", h.ID+":"+containerPath, hostPath).Run()
}

func (a *appleContainer) Remove(ctx context.Context, h ContainerHandle) error {
	return execCommand(ctx, "container", "rm", "-f", h.ID).Run()
}
