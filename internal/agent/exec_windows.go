//go:build windows

package agent

import (
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// setupProcAttrs configures cmd on Windows. Job-object assignment happens
// after Start (see assignJob), so no SysProcAttr changes are required here;
// this exists to keep the call site symmetric with Unix.
func setupProcAttrs(cmd *exec.Cmd) {
	// No pre-Start configuration needed.
}

// jobMu guards jobHandles, which maps a running *exec.Cmd to the Job Object
// it was assigned to, so killTree can find it without extra plumbing.
var (
	jobMu      sync.Mutex
	jobHandles = map[*exec.Cmd]windows.Handle{}
)

// assignJob creates a Job Object with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE and
// assigns cmd's process to it. On Windows, killing only the direct child
// (bash.exe) leaves grandchildren (e.g. sleep.exe launched by `bash -c`)
// running — there is no process-group equivalent. A job object configured
// with KILL_ON_JOB_CLOSE guarantees that closing the job handle terminates
// every process assigned to it, including grandchildren, giving us a
// process-tree kill. Must be called after cmd.Start() succeeds.
func assignJob(cmd *exec.Cmd) error {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(job) //nolint:errcheck
		return err
	}

	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_SET_QUOTA, false, uint32(cmd.Process.Pid))
	if err != nil {
		windows.CloseHandle(job) //nolint:errcheck
		return err
	}
	defer windows.CloseHandle(h) //nolint:errcheck

	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		windows.CloseHandle(job) //nolint:errcheck
		return err
	}

	jobMu.Lock()
	jobHandles[cmd] = job
	jobMu.Unlock()
	return nil
}

// killTree terminates the Job Object associated with cmd (if any), which
// kills cmd's process and every descendant assigned to the same job —
// including grandchildren such as sleep.exe spawned by `bash -c`. Falls back
// to killing just the direct process if no job was ever assigned (e.g.
// assignJob failed after Start, which should be rare).
func killTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	job, ok := takeJobHandle(cmd)

	if ok {
		// Closing a job object created with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		// terminates every process still assigned to it.
		_ = windows.CloseHandle(job)
		return
	}
	_ = cmd.Process.Kill()
}

// takeJobHandle removes and returns the Job Object handle associated with
// cmd, if any. It is the single place that mutates jobHandles so that
// killTree (cancel path) and cleanupTree (normal-completion path) can never
// both act on the same handle: whichever runs first wins the entry, and the
// other observes ok == false and does nothing. This makes cleanup idempotent
// regardless of which exit path runs, or if both run.
func takeJobHandle(cmd *exec.Cmd) (windows.Handle, bool) {
	jobMu.Lock()
	defer jobMu.Unlock()
	job, ok := jobHandles[cmd]
	if ok {
		delete(jobHandles, cmd)
	}
	return job, ok
}

// jobHandleCount reports the number of live entries in jobHandles. It exists
// so tests can assert on leak/no-leak behavior without depending on
// Windows-only types (windows.Handle) in cross-platform test files.
func jobHandleCount() int {
	jobMu.Lock()
	defer jobMu.Unlock()
	return len(jobHandles)
}

// cleanupTree releases any Job Object handle still associated with cmd. It
// must be called on every exit path from runTreeKilled — not just the
// cancel/killTree path — otherwise the handle assigned by assignJob leaks:
// each normally-completed step would permanently hold one kernel Job Object
// handle plus one jobHandles map entry (which also pins cmd, preventing GC
// of its output buffers). Safe to call after killTree already removed the
// entry (takeJobHandle then reports ok == false and this is a no-op), so
// callers can invoke it unconditionally via defer.
func cleanupTree(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if job, ok := takeJobHandle(cmd); ok {
		_ = windows.CloseHandle(job)
	}
}
