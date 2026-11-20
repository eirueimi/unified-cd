package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eirueimi/unified-cd/internal/imageref"
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
	args = append(args, envFlags(spec.Env)...)
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
	if err := imageref.Validate(image); err != nil {
		return err
	}
	return execCommand(ctx, r.bin, "pull", image).Run()
}

func (r *ociCLI) Run(ctx context.Context, spec RunSpec, stdout, stderr io.Writer) (int, error) {
	if err := imageref.Validate(spec.Image); err != nil {
		return -1, err
	}
	prep, cleanup, perr := prepareCLIEnv(spec.Env)
	defer cleanup()
	if perr != nil {
		return -1, perr
	}
	cmd := execCommand(ctx, r.bin, prep.withEnvFile(r.runArgs(spec))...)
	cmd.Env = prep.childEnv
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
	if err := imageref.Validate(spec.Image); err != nil {
		return nil, err
	}
	args := []string{"run", "-d"}
	for _, k := range sortedKeys(spec.Labels) {
		args = append(args, "--label", k+"="+spec.Labels[k])
	}
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
	args = append(args, envFlags(spec.Env)...)
	if spec.User != "" {
		args = append(args, "--user", spec.User)
	}
	for _, g := range spec.GroupAdd {
		args = append(args, "--group-add", g)
	}
	for _, c := range spec.CapDrop {
		args = append(args, "--cap-drop", c)
	}
	for _, c := range spec.CapAdd {
		args = append(args, "--cap-add", c)
	}
	if spec.NoNewPrivileges {
		args = append(args, "--security-opt", "no-new-privileges")
	}
	if spec.ReadOnlyRootfs {
		args = append(args, "--read-only")
		if r.bin == "podman" {
			// podman mounts tmpfs on /tmp, /run and /var/tmp under --read-only
			// unless told otherwise (design section 11 V7).
			args = append(args, "--read-only-tmpfs=false")
		}
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
	prep, cleanup, err := prepareCLIEnv(spec.Env)
	defer cleanup()
	if err != nil {
		return ContainerHandle{}, err
	}
	cmd := execCommand(ctx, r.bin, prep.withEnvFile(args)...)
	cmd.Env = prep.childEnv
	out, err := cmd.Output()
	if err != nil {
		return ContainerHandle{}, fmt.Errorf("%s run -d: %w", r.bin, err)
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return ContainerHandle{}, fmt.Errorf("%s run -d: empty container id", r.bin)
	}
	return ContainerHandle{ID: id}, nil
}

// execArgs builds the argv for `exec`. Env values are never included; see
// ocienv.go.
func (r *ociCLI) execArgs(h ContainerHandle, spec ExecSpec) []string {
	args := []string{"exec"}
	args = append(args, envFlags(spec.Env)...)
	shell := spec.Shell
	if len(shell) == 0 {
		shell = []string{"sh", "-c"}
	}
	args = append(args, h.ID)
	args = append(args, shell...)
	return append(args, spec.Script)
}

func (r *ociCLI) Exec(ctx context.Context, h ContainerHandle, spec ExecSpec, stdout, stderr io.Writer) (int, error) {
	prep, cleanup, perr := prepareCLIEnv(spec.Env)
	defer cleanup()
	if perr != nil {
		return -1, perr
	}
	cmd := execCommand(ctx, r.bin, prep.withEnvFile(r.execArgs(h, spec))...)
	cmd.Env = prep.childEnv
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

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ImageID returns the local image ID; a missing image (non-zero exit) is
// present=false with a nil error.
func (r *ociCLI) ImageID(ctx context.Context, image string) (string, bool, error) {
	if err := imageref.Validate(image); err != nil {
		return "", false, err
	}
	out, err := execCommand(ctx, r.bin, "image", "inspect", "--format", "{{.Id}}", image).Output()
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return "", false, nil
		}
		return "", false, fmt.Errorf("%s image inspect %s: %w", r.bin, image, err)
	}
	id := strings.TrimSpace(string(out))
	return id, id != "", nil
}

// ContainerRunning reports the container's State.Running.
func (r *ociCLI) ContainerRunning(ctx context.Context, h ContainerHandle) (bool, error) {
	out, err := execCommand(ctx, r.bin, "inspect", "-f", "{{.State.Running}}", h.ID).Output()
	if err != nil {
		return false, fmt.Errorf("%s inspect running: %w", r.bin, err)
	}
	return strings.TrimSpace(string(out)) == "true", nil
}

// ListContainers lists every container (any state) carrying all the given
// labels. It uses `ps -a -q --no-trunc --filter label=k=v` plus a per-ID
// `inspect -f {{json .Config.Labels}}` rather than `ps --format {{json .Labels}}`:
// the ps Labels column is rendered differently by docker (a "k=v,k=v" string),
// podman and nerdctl, whereas `inspect .Config.Labels` is a JSON map on all
// three. A container removed between the list and the inspect is skipped.
func (r *ociCLI) ListContainers(ctx context.Context, labels map[string]string) ([]ContainerInfo, error) {
	args := []string{"ps", "-a", "-q", "--no-trunc"}
	for _, k := range sortedKeys(labels) {
		args = append(args, "--filter", "label="+k+"="+labels[k])
	}
	out, err := execCommand(ctx, r.bin, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("%s ps: %w", r.bin, err)
	}
	var infos []ContainerInfo
	for _, id := range strings.Fields(string(out)) {
		lo, err := execCommand(ctx, r.bin, "inspect", "-f", "{{json .Config.Labels}}", id).Output()
		if err != nil {
			continue
		}
		var lm map[string]string
		if err := json.Unmarshal(bytes.TrimSpace(lo), &lm); err != nil {
			continue
		}
		infos = append(infos, ContainerInfo{Handle: ContainerHandle{ID: id}, Labels: lm})
	}
	return infos, nil
}

// LogsSince follows the container's logs from since (RFC3339Nano). Like Logs,
// a stream that ends or a ctx cancellation is not an error.
func (r *ociCLI) LogsSince(ctx context.Context, h ContainerHandle, since time.Time, stdout, stderr io.Writer) error {
	cmd := execCommand(ctx, r.bin, "logs", "-f", "--since", since.UTC().Format(time.RFC3339Nano), h.ID)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if err == nil || ctx.Err() != nil {
		return nil
	}
	if _, ok := err.(*exec.ExitError); ok {
		return nil
	}
	return fmt.Errorf("%s logs -f --since: %w", r.bin, err)
}

// LastLogTime returns the runtime-stamped time of the container's last log
// line (`logs --timestamps --tail 1`), or the zero time for a container with
// no output. Both streams are read: stdout lines arrive on stdout and stderr
// lines on stderr, but --tail 1 yields exactly one line in total.
func (r *ociCLI) LastLogTime(ctx context.Context, h ContainerHandle) (time.Time, error) {
	out, err := execCommand(ctx, r.bin, "logs", "--timestamps", "--tail", "1", h.ID).CombinedOutput()
	if err != nil {
		return time.Time{}, fmt.Errorf("%s logs --tail 1: %w", r.bin, err)
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return time.Time{}, nil
	}
	ts, _, _ := strings.Cut(line, " ")
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s logs --timestamps: unparseable timestamp %q: %w", r.bin, ts, err)
	}
	return t, nil
}

// ImageUser returns the image's Config.User. If the image is not present
// locally (any inspect failure) it is pulled and the inspect retried once.
func (r *ociCLI) ImageUser(ctx context.Context, image string) (string, error) {
	if err := imageref.Validate(image); err != nil {
		return "", err
	}
	inspect := func() (string, error) {
		var stderr bytes.Buffer
		cmd := execCommand(ctx, r.bin, "image", "inspect", "--format", "{{.Config.User}}", image)
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("%s image inspect %s: %w: %s", r.bin, image, err, strings.TrimSpace(stderr.String()))
		}
		return strings.TrimSpace(string(out)), nil
	}
	u, err := inspect()
	if err == nil {
		return u, nil
	}
	// The "image missing" wording differs across docker, podman 4.x/5.x and
	// nerdctl, so any inspect failure triggers one bounded pull-and-retry. A
	// pull failure surfaces real daemon/auth problems.
	if perr := r.Pull(ctx, image); perr != nil {
		return "", fmt.Errorf("%w; pull %s: %v", err, image, perr)
	}
	return inspect()
}
