//go:build windows

package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

const helperModeEnv = "UCD_TEST_PROC_HELPER"
const helperPidFileEnv = "UCD_TEST_PROC_PIDFILE"

// TestHelperProcess is not a real test: the tests below re-exec the test
// binary into it. Mode "parent" spawns a "sleeper" grandchild immediately at
// startup, publishes the grandchild's PID, then blocks; "sleeper" just blocks.
func TestHelperProcess(t *testing.T) {
	switch os.Getenv(helperModeEnv) {
	case "parent":
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
		cmd.Env = append(os.Environ(), helperModeEnv+"=sleeper")
		if err := cmd.Start(); err != nil {
			os.Exit(3)
		}
		pidFile := os.Getenv(helperPidFileEnv)
		tmp := pidFile + ".tmp"
		if err := os.WriteFile(tmp, []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
			os.Exit(4)
		}
		if err := os.Rename(tmp, pidFile); err != nil {
			os.Exit(5)
		}
		time.Sleep(120 * time.Second)
		os.Exit(0)
	case "sleeper":
		time.Sleep(120 * time.Second)
		os.Exit(0)
	}
}

func helperShell() []string {
	return []string{os.Args[0], "-test.run=^TestHelperProcess$", "--"}
}

func helperEnv(pidFile string) []string {
	return []string{helperModeEnv + "=parent", helperPidFileEnv + "=" + pidFile}
}

// pidAlive reports whether a process with pid is still running.
func pidAlive(pid uint32) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h) //nolint:errcheck
	ev, _ := windows.WaitForSingleObject(h, 0)
	return ev == uint32(windows.WAIT_TIMEOUT)
}

func readPid(t *testing.T, path string) (uint32, bool) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 32)
	require.NoError(t, err)
	return uint32(n), true
}

func requireDead(t *testing.T, pid uint32, what string) {
	t.Helper()
	require.Eventually(t, func() bool { return !pidAlive(pid) },
		10*time.Second, 50*time.Millisecond, "%s (pid %d) survived", what, pid)
}

// TestRunStep_Windows_CancelAtStartup_NoDescendantSurvives: the child spawns a
// grandchild as the very first thing it does and the step is cancelled as soon
// as the grandchild exists. Because the child starts suspended and is resumed
// only after it is in the Job Object, the grandchild is always in the job.
func TestRunStep_Windows_CancelAtStartup_NoDescendantSurvives(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		code, err := RunStepWithShell(ctx, helperShell(), "unused", nil, nil, helperEnv(pidFile), nil, dir)
		done <- result{code, err}
	}()

	var gc uint32
	require.Eventually(t, func() bool {
		var ok bool
		gc, ok = readPid(t, pidFile)
		return ok
	}, 15*time.Second, 5*time.Millisecond, "grandchild pid was never published")
	cancel()

	select {
	case r := <-done:
		require.Error(t, r.err)
		assert.ErrorIs(t, r.err, context.Canceled, "cancelled step must report cancellation")
	case <-time.After(20 * time.Second):
		t.Fatal("step did not return after cancellation")
	}
	requireDead(t, gc, "grandchild")
	assert.Equal(t, 0, jobHandleCount())
}

// TestRunTreeKilled_Windows_AssignFailure_KillsChildAndFailsStart: when Job
// Object assignment fails the child must be killed before it ever runs (no
// single-process fallback) and the step must fail to start.
func TestRunTreeKilled_Windows_AssignFailure_KillsChildAndFailsStart(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")

	var childPid uint32
	injected := errors.New("injected assignment failure")
	orig := assignJobFn
	assignJobFn = func(cmd *exec.Cmd) error {
		childPid = uint32(cmd.Process.Pid)
		return injected
	}
	t.Cleanup(func() { assignJobFn = orig })

	before := jobHandleCount()
	code, err := RunStepWithShell(context.Background(), helperShell(), "unused", nil, nil, helperEnv(pidFile), nil, dir)
	require.Error(t, err)
	assert.ErrorIs(t, err, injected)
	assert.Contains(t, err.Error(), "step not started")
	assert.Equal(t, -1, code)

	require.NotZero(t, childPid)
	requireDead(t, childPid, "child")
	time.Sleep(500 * time.Millisecond)
	_, published := readPid(t, pidFile)
	assert.False(t, published, "child ran user code despite failed assignment")
	assert.Equal(t, before, jobHandleCount())
}

// TestRunTreeKilled_Windows_FailureAfterResume_KillsWholeTree covers a failure
// reported after the process was already resumed inside its job: everything in
// the job, including any grandchild, must still die.
func TestRunTreeKilled_Windows_FailureAfterResume_KillsWholeTree(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")

	var childPid uint32
	injected := errors.New("injected late failure")
	orig := assignJobFn
	assignJobFn = func(cmd *exec.Cmd) error {
		childPid = uint32(cmd.Process.Pid)
		if err := orig(cmd); err != nil {
			return err
		}
		// Let the child spawn its grandchild before reporting failure.
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if _, ok := readPid(t, pidFile); ok {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		return injected
	}
	t.Cleanup(func() { assignJobFn = orig })

	_, err := RunStepWithShell(context.Background(), helperShell(), "unused", nil, nil, helperEnv(pidFile), nil, dir)
	require.ErrorIs(t, err, injected)

	requireDead(t, childPid, "child")
	gc, ok := readPid(t, pidFile)
	require.True(t, ok, "grandchild was expected to have started")
	requireDead(t, gc, "grandchild")
	assert.Equal(t, 0, jobHandleCount())
}
