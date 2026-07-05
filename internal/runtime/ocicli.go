package runtime

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// execCommand is indirected for testability.
var execCommand = exec.CommandContext

// ociCLI drives any runtime whose CLI is docker-compatible:
// docker, podman, nerdctl, and Microsoft's wslc.
type ociCLI struct {
	bin string
}

func (r *ociCLI) Name() string { return r.bin }

func (r *ociCLI) Available() bool {
	_, err := lookPath(r.bin)
	return err == nil
}

func (r *ociCLI) runArgs(spec RunSpec) []string {
	args := []string{"run", "--rm"}
	if spec.CPULimit != "" {
		args = append(args, "--cpus", spec.CPULimit)
	}
	if spec.MemLimit != "" {
		args = append(args, "--memory", spec.MemLimit)
	}
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

func (r *ociCLI) Pull(ctx context.Context, image string) error {
	return execCommand(ctx, r.bin, "pull", image).Run()
}

func (r *ociCLI) Run(ctx context.Context, spec RunSpec, stdout, stderr io.Writer) (int, error) {
	cmd := execCommand(ctx, r.bin, r.runArgs(spec)...)
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

func (r *ociCLI) Create(ctx context.Context, spec CreateSpec) (ContainerHandle, error) {
	args := []string{"run", "-d"}
	if spec.CPULimit != "" {
		args = append(args, "--cpus", spec.CPULimit)
	}
	if spec.MemLimit != "" {
		args = append(args, "--memory", spec.MemLimit)
	}
	for _, e := range spec.Env {
		args = append(args, "-e", e)
	}
	args = append(args, spec.Image, "sleep", "infinity")
	out, err := execCommand(ctx, r.bin, args...).Output()
	if err != nil {
		return ContainerHandle{}, fmt.Errorf("%s run -d: %w", r.bin, err)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return ContainerHandle{}, fmt.Errorf("%s run -d: empty container id", r.bin)
	}
	return ContainerHandle{ID: id}, nil
}

func (r *ociCLI) Exec(ctx context.Context, h ContainerHandle, spec ExecSpec, stdout, stderr io.Writer) (int, error) {
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
	cmd := execCommand(ctx, r.bin, args...)
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

func (r *ociCLI) CopyIn(ctx context.Context, h ContainerHandle, hostPath, containerPath string) error {
	return execCommand(ctx, r.bin, "cp", hostPath, h.ID+":"+containerPath).Run()
}

func (r *ociCLI) CopyOut(ctx context.Context, h ContainerHandle, containerPath, hostPath string) error {
	return execCommand(ctx, r.bin, "cp", h.ID+":"+containerPath, hostPath).Run()
}

func (r *ociCLI) Remove(ctx context.Context, h ContainerHandle) error {
	return execCommand(ctx, r.bin, "rm", "-f", h.ID).Run()
}
