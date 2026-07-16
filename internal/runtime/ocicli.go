package runtime

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
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
//
// Bind mounts are emitted in the `--mount type=bind,source=...,target=...`
// form rather than `-v host:ctr[:ro]` (G6): on Windows, a host path is
// itself of the form `C:\ws`, so the colon-joined `-v` grammar can't tell a
// drive-letter colon apart from the host:container separator. The
// `--mount` key=value grammar sidesteps that, but it's comma/equals
// delimited, so a path containing either character returns an error instead
// of producing a mount Docker/podman would parse wrong.
func (r *ociCLI) createArgs(spec CreateSpec) ([]string, error) {
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
		if strings.ContainsAny(m.HostPath, ",=") || strings.ContainsAny(m.ContainerPath, ",=") {
			return nil, fmt.Errorf("mount path %q -> %q contains ',' or '=', which cannot be expressed in --mount syntax", m.HostPath, m.ContainerPath)
		}
		v := "type=bind,source=" + m.HostPath + ",target=" + m.ContainerPath
		if m.ReadOnly {
			v += ",readonly"
		}
		args = append(args, "--mount", v)
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
	return args, nil
}

func (r *ociCLI) Create(ctx context.Context, spec CreateSpec) (ContainerHandle, error) {
	args, err := r.createArgs(spec)
	if err != nil {
		return ContainerHandle{}, err
	}
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

func (r *ociCLI) Logs(ctx context.Context, h ContainerHandle, stdout, stderr io.Writer) error {
	cmd := execCommand(ctx, r.bin, "logs", "-f", h.ID)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	// `logs -f` exits 0 when the container stops; ctx cancellation kills it.
	// Neither is an error for the caller (the stream simply ended).
	if err == nil || ctx.Err() != nil {
		return nil
	}
	if _, ok := err.(*exec.ExitError); ok {
		return nil // container gone / logs ended non-zero — not our failure
	}
	return fmt.Errorf("%s logs -f: %w", r.bin, err)
}

func (r *ociCLI) ExitCode(ctx context.Context, h ContainerHandle) (int, error) {
	out, err := execCommand(ctx, r.bin, "inspect", "-f", "{{.State.ExitCode}}", h.ID).Output()
	if err != nil {
		return 0, fmt.Errorf("%s inspect exitcode: %w", r.bin, err)
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}
