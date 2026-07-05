package runtime

import (
	"context"
	"io"
	"os/exec"
)

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
	return exec.CommandContext(ctx, r.bin, "pull", image).Run()
}

func (r *ociCLI) Run(ctx context.Context, spec RunSpec, stdout, stderr io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, r.bin, r.runArgs(spec)...)
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
