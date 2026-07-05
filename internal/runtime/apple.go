package runtime

import (
	"context"
	"io"
	"os/exec"
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
	return exec.CommandContext(ctx, "container", "pull", image).Run()
}

func (a *appleContainer) Run(ctx context.Context, spec RunSpec, stdout, stderr io.Writer) (int, error) {
	cmd := exec.CommandContext(ctx, "container", a.runArgs(spec)...)
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
