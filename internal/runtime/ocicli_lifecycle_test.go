// internal/runtime/ocicli_lifecycle_test.go
package runtime

import (
	"context"
	"os/exec"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// noopCmd returns a command that is guaranteed to exist and exit 0 on every
// platform we test on, so fakes don't depend on a `true` binary being on
// PATH (absent by default on Windows).
func noopCmd(ctx context.Context) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.CommandContext(ctx, "cmd", "/c", "exit 0")
	}
	return exec.CommandContext(ctx, "true")
}

func withFakeExec(t *testing.T, record *[][]string) {
	t.Helper()
	orig := execCommand
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		*record = append(*record, append([]string{name}, args...))
		// Use a cross-platform no-op in place of the real argv so the fake
		// doesn't depend on a `true` binary being present on PATH.
		return noopCmd(ctx)
	}
	t.Cleanup(func() { execCommand = orig })
}

func TestOCICLICopyOutArgv(t *testing.T) {
	var rec [][]string
	withFakeExec(t, &rec)
	r := &ociCLI{bin: "docker"}
	if err := r.CopyOut(context.Background(), ContainerHandle{ID: "abc"}, "/out/app", "/tmp/app"); err != nil {
		t.Fatalf("CopyOut: %v", err)
	}
	got := rec[0]
	want := []string{"docker", "cp", "abc:/out/app", "/tmp/app"}
	if len(got) != len(want) {
		t.Fatalf("argv = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv = %v, want %v", got, want)
		}
	}
}

func TestOCICLICopyInArgv(t *testing.T) {
	var rec [][]string
	withFakeExec(t, &rec)
	r := &ociCLI{bin: "podman"}
	if err := r.CopyIn(context.Background(), ContainerHandle{ID: "xyz"}, "/tmp/deps", "/work/deps"); err != nil {
		t.Fatalf("CopyIn: %v", err)
	}
	want := []string{"podman", "cp", "/tmp/deps", "xyz:/work/deps"}
	for i := range want {
		if rec[0][i] != want[i] {
			t.Fatalf("argv = %v, want %v", rec[0], want)
		}
	}
}

// TestOCICLICreateArgv_WorkDir is the regression test for Finding 1: the host
// scope container must be launched with a defined working directory (`-w`),
// mirroring the k8s scope pod's WorkingDir. Without this, scoped `run:` steps
// and `docker exec` have no cwd, and relative container-side paths passed to
// `docker cp` are rejected ("destination path must be absolute").
//
// Uses createArgs directly (not Create+withFakeExec): Create shells out via
// exec.Cmd.Output() to read the container id from stdout, and the fake `true`
// command used elsewhere in this file produces no stdout, so it can't stand
// in for a real container-id-producing `run -d`. createArgs isolates the pure
// argv-building logic that Create dispatches through exec.
func TestOCICLICreateArgv_WorkDir(t *testing.T) {
	r := &ociCLI{bin: "docker"}
	got := r.createArgs(CreateSpec{Image: "golang:1.22", WorkDir: "/workspace"})
	foundFlag := false
	for i, a := range got {
		if a == "-w" {
			foundFlag = true
			if i+1 >= len(got) || got[i+1] != "/workspace" {
				t.Fatalf("expected -w to be followed by /workspace, argv = %v", got)
			}
		}
	}
	if !foundFlag {
		t.Fatalf("expected -w /workspace in argv, got %v", got)
	}
}

// TestOCICLICreateArgv_NoWorkDirWhenEmpty confirms empty WorkDir omits -w
// entirely (preserves prior behavior / driver default) rather than passing
// -w "".
func TestOCICLICreateArgv_NoWorkDirWhenEmpty(t *testing.T) {
	r := &ociCLI{bin: "docker"}
	got := r.createArgs(CreateSpec{Image: "golang:1.22"})
	for _, a := range got {
		if a == "-w" {
			t.Fatalf("expected no -w flag when WorkDir is empty, argv = %v", got)
		}
	}
}

func TestOCICLICreateArgv_Mounts(t *testing.T) {
	r := &ociCLI{bin: "docker"}
	got := r.createArgs(CreateSpec{
		Image:   "alpine",
		WorkDir: "/workspace",
		Mounts:  []Mount{{HostPath: "/host/ws", ContainerPath: "/workspace"}},
	})
	found := false
	for i, a := range got {
		if a == "-v" {
			found = true
			if i+1 >= len(got) || got[i+1] != "/host/ws:/workspace" {
				t.Fatalf("expected -v /host/ws:/workspace, argv = %v", got)
			}
		}
	}
	if !found {
		t.Fatalf("expected -v in argv, got %v", got)
	}
}

func TestOCICLICreateArgv_NoMountsWhenEmpty(t *testing.T) {
	r := &ociCLI{bin: "docker"}
	got := r.createArgs(CreateSpec{Image: "alpine"})
	for _, a := range got {
		if a == "-v" {
			t.Fatalf("expected no -v flag when Mounts is empty, argv = %v", got)
		}
	}
}

// TestCreateArgs_NetworkContainer is the regression test for job isolation:
// the claim pod's per-job containers must join the pause container's netns
// (docker/podman/nerdctl `--network container:<id>`) so sidecars are
// reachable on localhost, mirroring a k8s pod's shared network namespace.
func TestCreateArgs_NetworkContainer(t *testing.T) {
	r := &ociCLI{bin: "docker"}
	args := r.createArgs(CreateSpec{Image: "busybox", NetworkContainer: "abc123"})
	assert.Contains(t, strings.Join(args, " "), "--network container:abc123")
}

// TestCreateArgs_NoNetworkByDefault confirms empty NetworkContainer omits
// --network entirely (preserves prior behavior / driver default).
func TestCreateArgs_NoNetworkByDefault(t *testing.T) {
	r := &ociCLI{bin: "docker"}
	args := r.createArgs(CreateSpec{Image: "busybox"})
	assert.NotContains(t, strings.Join(args, " "), "--network")
}

// TestOCICLICreateArgv_CommandEmitsSleepInfinity is the regression test for
// the sidecar-sleep-infinity bug: a caller that explicitly asks for the
// keep-alive command (uses-scope containers, the claim pod's primary "job"
// container) must still get "sleep infinity" appended after the image.
func TestOCICLICreateArgv_CommandEmitsSleepInfinity(t *testing.T) {
	r := &ociCLI{bin: "docker"}
	got := r.createArgs(CreateSpec{Image: "golang:1.22", Command: []string{"sleep", "infinity"}})
	want := []string{"run", "-d", "golang:1.22", "sleep", "infinity"}
	assert.Equal(t, want, got)
}

// TestOCICLICreateArgv_NilCommandRunsImageEntrypoint is the core fix under
// test: a claim-pod sidecar (mysql, redis, ...) created with no Command must
// NOT have "sleep infinity" appended — the image's own entrypoint/CMD must
// run unmodified, or the sidecar's service (e.g. mysqld) never starts.
func TestOCICLICreateArgv_NilCommandRunsImageEntrypoint(t *testing.T) {
	r := &ociCLI{bin: "docker"}
	got := r.createArgs(CreateSpec{Image: "mysql:8"})
	want := []string{"run", "-d", "mysql:8"}
	assert.Equal(t, want, got)
	assert.NotContains(t, strings.Join(got, " "), "sleep",
		"a sidecar with no explicit Command must run its image's default entrypoint, not sleep infinity")
}

// TestOCICLICreateArgv_CommandHonorsCustomArgv confirms a sidecar's own
// podTemplate command/args (carried through as CreateSpec.Command) is
// emitted verbatim, not silently replaced by sleep infinity.
func TestOCICLICreateArgv_CommandHonorsCustomArgv(t *testing.T) {
	r := &ociCLI{bin: "docker"}
	got := r.createArgs(CreateSpec{Image: "redis:7", Command: []string{"redis-server", "--port", "6380"}})
	want := []string{"run", "-d", "redis:7", "redis-server", "--port", "6380"}
	assert.Equal(t, want, got)
}
