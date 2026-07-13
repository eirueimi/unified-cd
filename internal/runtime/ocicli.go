package runtime

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
)

// execCommand is indirected for testability.
var execCommand = exec.CommandContext

// noEmptyEntrypointClear names runtimes whose CLI does NOT support the
// `--entrypoint ""` empty-clear form. For those, an Entrypoint override
// degrades to positional args (the image ENTRYPOINT still runs) plus a WARN.
// Seeded empty; add a runtime only when real-binary verification proves it
// necessary (see the host-entrypoint-parity design doc).
//
// WARNING when populating this set: the degrade is only "diagnosed-wrong" for
// SIDECARS (command becomes CMD, image ENTRYPOINT still runs — the pre-parity
// behavior plus a WARN). For a KEEP-ALIVE container (Entrypoint=ucd-sh pause on
// the primary "job"/pause/scope/cleanup containers) whose IMAGE declares its own
// ENTRYPOINT, degrading reintroduces the exact latent bug this parity work fixed:
// the container would run `<image-entrypoint> /.ucd/ucd-sh pause` and never
// become an exec-able keep-alive. This is harmless today only because the set is
// empty AND the default runner/pause images are ENTRYPOINT-less. Before adding a
// runtime here, gate keep-alive/primary containers on the runtime supporting the
// clear (or require an ENTRYPOINT-less keep-alive image on that runtime).
var noEmptyEntrypointClear = map[string]bool{}

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

// createArgs builds the argv for `run -d` (a long-lived container: a
// uses-level runsIn.image scope, or one of a claim pod's containers).
// spec.Entrypoint/spec.Args (if set) are appended after the image; nil/empty
// runs the image's default entrypoint/CMD — see the CreateSpec.Entrypoint
// and CreateSpec.Args doc comments. Extracted from Create so tests can
// assert on the argv (notably -w for spec.WorkDir) without depending on
// exec.Cmd.Output()'s stdout plumbing.
func (r *ociCLI) createArgs(spec CreateSpec) []string {
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
		v := m.HostPath + ":" + m.ContainerPath
		if m.ReadOnly {
			v += ":ro"
		}
		args = append(args, "-v", v)
	}
	if spec.NetworkContainer != "" {
		args = append(args, "--network", "container:"+spec.NetworkContainer)
	}
	for _, e := range spec.Env {
		args = append(args, "-e", e)
	}
	// An Entrypoint override clears the image ENTRYPOINT (docker
	// `--entrypoint ""`, which must precede the image) and runs
	// Entrypoint+Args as positional argv. Args-only leaves the image
	// ENTRYPOINT in place. See CreateSpec.Entrypoint/Args.
	if len(spec.Entrypoint) > 0 {
		if noEmptyEntrypointClear[r.bin] {
			slog.Warn("runtime does not support clearing the image ENTRYPOINT (--entrypoint \"\"); "+
				"running command as positional args — the image's own ENTRYPOINT still applies", "runtime", r.bin)
		} else {
			args = append(args, "--entrypoint", "")
		}
	}
	args = append(args, spec.Image)
	args = append(args, spec.Entrypoint...)
	args = append(args, spec.Args...)
	return args
}

func (r *ociCLI) Create(ctx context.Context, spec CreateSpec) (ContainerHandle, error) {
	args := r.createArgs(spec)
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
