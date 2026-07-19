package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureShimIntact_RepairsTamperedShim(t *testing.T) {
	ws := t.TempDir()
	// Fake real (non-empty) shim bytes so this test exercises the actual
	// repair path regardless of whether internal/shim/embedded holds the
	// real two-stage-build binary or CI's committed zero-byte placeholder
	// (see withFakeShimBytes's doc comment in install_shim_test.go) — the
	// same convention that package's InstallShim tests already use, rather
	// than skipping and losing this coverage in every CI run.
	withFakeShimBytes(t, []byte("#!/bin/sh\necho real-shim-content\n"))
	toolsDir, err := InstallShim(ws)
	require.NoError(t, err)
	shimPath := filepath.Join(toolsDir, "ucd-sh")
	pristine, err := os.ReadFile(shimPath)
	require.NoError(t, err)

	// Simulate a native step overwriting the shim.
	require.NoError(t, os.WriteFile(shimPath, []byte("#!/bin/sh\nexfiltrate\n"), 0o755))

	repaired, err := EnsureShimIntact(toolsDir)
	require.NoError(t, err)
	assert.True(t, repaired, "tampering must be detected and repaired")

	got, err := os.ReadFile(shimPath)
	require.NoError(t, err)
	assert.Equal(t, pristine, got, "shim must be restored to the embedded bytes")
}

func TestEnsureShimIntact_NoopWhenIntact(t *testing.T) {
	ws := t.TempDir()
	// See TestEnsureShimIntact_RepairsTamperedShim's comment: fake non-empty
	// bytes so this runs the same regardless of CI's placeholder embed.
	withFakeShimBytes(t, []byte("#!/bin/sh\necho real-shim-content\n"))
	toolsDir, err := InstallShim(ws)
	require.NoError(t, err)

	repaired, err := EnsureShimIntact(toolsDir)
	require.NoError(t, err)
	assert.False(t, repaired, "an intact shim must not be rewritten")
}

// TestEnsureShimIntact_RepeatedTamperingIsRepairedEachTime guards against a
// specific self-lock bug: EnsureShimIntact tightens the shim to 0o555 (no
// write bit) after every check. Since a native step and the agent run as
// the same OS user, file-mode enforcement can't tell them apart — if the
// repair path didn't restore the write bit before its own os.WriteFile, the
// SECOND tampering-then-repair cycle would fail with a permission error
// instead of repairing, because the file would still be 0o555 from the
// first call's tightening.
//
// Tampering is simulated via remove-then-recreate rather than an in-place
// os.WriteFile: once EnsureShimIntact has tightened the file to 0o555, a
// plain overwrite is exactly what that's meant to block (and does — an
// in-place os.WriteFile onto a 0o555 file correctly fails on both POSIX and
// Windows). remove+recreate only needs write permission on the directory,
// which is how a determined attacker (or `cp -f`, or `mv`) would actually
// get around a read-only target, so it's the realistic way to exercise
// repeated repair cycles here.
func TestEnsureShimIntact_RepeatedTamperingIsRepairedEachTime(t *testing.T) {
	ws := t.TempDir()
	// See TestEnsureShimIntact_RepairsTamperedShim's comment: fake non-empty
	// bytes so this runs the same regardless of CI's placeholder embed.
	withFakeShimBytes(t, []byte("#!/bin/sh\necho real-shim-content\n"))
	toolsDir, err := InstallShim(ws)
	require.NoError(t, err)
	shimPath := filepath.Join(toolsDir, "ucd-sh")
	pristine, err := os.ReadFile(shimPath)
	require.NoError(t, err)

	for i := 0; i < 2; i++ {
		require.NoError(t, os.Remove(shimPath))
		require.NoError(t, os.WriteFile(shimPath, []byte("#!/bin/sh\nexfiltrate again\n"), 0o755))

		repaired, err := EnsureShimIntact(toolsDir)
		require.NoErrorf(t, err, "iteration %d", i)
		assert.Truef(t, repaired, "iteration %d: tampering must be detected and repaired", i)

		got, err := os.ReadFile(shimPath)
		require.NoError(t, err)
		assert.Equalf(t, pristine, got, "iteration %d: shim must be restored to the embedded bytes", i)
	}
}

// TestEnsureShimIntact_MissingFileIsRepaired covers the file being deleted
// entirely (not just overwritten), which EnsureShimIntact must treat the
// same as tampered content: recreate it from the embedded payload.
func TestEnsureShimIntact_MissingFileIsRepaired(t *testing.T) {
	ws := t.TempDir()
	// See TestEnsureShimIntact_RepairsTamperedShim's comment: fake non-empty
	// bytes so this runs the same regardless of CI's placeholder embed.
	withFakeShimBytes(t, []byte("#!/bin/sh\necho real-shim-content\n"))
	toolsDir, err := InstallShim(ws)
	require.NoError(t, err)
	shimPath := filepath.Join(toolsDir, "ucd-sh")
	pristine, err := os.ReadFile(shimPath)
	require.NoError(t, err)

	require.NoError(t, os.Remove(shimPath))

	repaired, err := EnsureShimIntact(toolsDir)
	require.NoError(t, err)
	assert.True(t, repaired, "a missing shim must be detected and recreated")

	got, err := os.ReadFile(shimPath)
	require.NoError(t, err)
	assert.Equal(t, pristine, got)
}

// TestEnsureShimIntact_EmptyPayloadIsHardError proves EnsureShimIntact never
// treats a zero-length embedded payload as "the real shim, just replace it":
// InstallShim already hard-errors on len(payload)==0 (see that function), but
// EnsureShimIntact reads shimPayload() independently on every claim. Without
// its own guard, a WORKING shim on disk would be compared against an empty
// "want", found "not intact" (bytes never equal unless both empty), and
// repaired into a 0-byte file — turning a functioning agent into one that
// destroys its own shim on the very next claim.
func TestEnsureShimIntact_EmptyPayloadIsHardError(t *testing.T) {
	ws := t.TempDir()
	// This test must exercise the empty-payload guard regardless of whether
	// internal/shim/embedded holds the real two-stage-build binary or CI's
	// committed zero-byte placeholder — it is the one test in this file that
	// specifically covers the empty-payload path, so it must never be
	// skipped. Install a real (fake but non-empty) shim first via
	// withFakeShimBytes, exactly like the other tests in this file, so the
	// "pristine" baseline below never depends on the ambient embed state;
	// only THEN does it flip to a nil payload to drive EnsureShimIntact's
	// hard-error path.
	withFakeShimBytes(t, []byte("#!/bin/sh\necho real-shim-content\n"))
	toolsDir, err := InstallShim(ws)
	require.NoError(t, err)
	shimPath := filepath.Join(toolsDir, "ucd-sh")
	pristine, err := os.ReadFile(shimPath)
	require.NoError(t, err)

	withFakeShimBytes(t, nil)

	repaired, err := EnsureShimIntact(toolsDir)
	require.Error(t, err, "an empty embedded payload must be a hard error, not a trigger to zero out a working shim")
	assert.False(t, repaired)
	assert.Contains(t, err.Error(), "empty")

	got, err := os.ReadFile(shimPath)
	require.NoError(t, err)
	assert.Equal(t, pristine, got, "the on-disk shim must be untouched when the embedded payload is empty")
}

// TestEnsureShimIntact_RepairDoesNotLeaveTempFileBehind proves the temp file
// repairShim creates in toolsDir (ucd-sh.tmp*, renamed over shimPath on
// success) never lingers: after a successful repair — including several in a
// row — toolsDir must contain exactly the shim itself, nothing else.
func TestEnsureShimIntact_RepairDoesNotLeaveTempFileBehind(t *testing.T) {
	ws := t.TempDir()
	// See TestEnsureShimIntact_RepairsTamperedShim's comment: fake non-empty
	// bytes so this runs the same regardless of CI's placeholder embed.
	withFakeShimBytes(t, []byte("#!/bin/sh\necho real-shim-content\n"))
	toolsDir, err := InstallShim(ws)
	require.NoError(t, err)
	shimPath := filepath.Join(toolsDir, "ucd-sh")

	for i := 0; i < 3; i++ {
		// remove+recreate, not an in-place os.WriteFile: after the first
		// repair, EnsureShimIntact has tightened shimPath to 0o555, and an
		// in-place O_TRUNC write onto a 0o555 file correctly fails on both
		// POSIX and Windows (see
		// TestEnsureShimIntact_RepeatedTamperingIsRepairedEachTime's doc
		// comment) — remove+recreate is the realistic way an attacker (or
		// `cp -f`/`mv`) actually gets around a read-only target.
		require.NoError(t, os.Remove(shimPath))
		require.NoError(t, os.WriteFile(shimPath, []byte("#!/bin/sh\ntamper\n"), 0o755))
		repaired, err := EnsureShimIntact(toolsDir)
		require.NoErrorf(t, err, "iteration %d", i)
		require.Truef(t, repaired, "iteration %d", i)
	}

	entries, err := os.ReadDir(toolsDir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Equal(t, []string{"ucd-sh"}, names,
		"toolsDir must contain only the shim after repair — no ucd-sh.tmp* debris left behind on any error or success path")
}

// TestEnsureShimIntact_RepairSucceedsWhileShimIsExecuting is the direct
// regression test for the ETXTBSY finding: repair must succeed via rename
// even while another process currently has shimPath open as its own
// executing text — the exact situation an in-place os.WriteFile(shimPath,
// ...) (the pre-fix implementation) cannot handle on Linux, because opening
// a file for writing while it is the text image of a running process fails
// with ETXTBSY ("text file busy"). A rename instead swaps the directory
// entry without needing write access to the busy inode, so it is unaffected.
//
// This copies a real ELF binary (the `sleep` binary already on PATH) to
// shimPath and execve()s it directly via exec.Command(shimPath, ...) — NOT
// via an interpreter (e.g. `sh script.sh`), and not a shebang script either:
// the kernel only holds a text-busy reference on the exact inode it loaded as
// a process's own executable image, so a shebang script's OWN inode is not
// reliably kept busy for the child's lifetime (the shell interpreter is what
// actually stays mapped executable; the script itself is just read as data
// once the interpreter takes over). Using a real native binary as the
// directly-invoked shim, exactly like production `/.ucd/ucd-sh pause`, is
// what makes ETXTBSY actually reproduce here.
//
// This only reproduces on a real POSIX kernel that enforces ETXTBSY — it is
// skipped on Windows, which has no equivalent restriction (and, not
// coincidentally, no need for this workaround: Windows' own file-locking
// behavior around executing images is different again, which is why
// renameOverExisting has its own Windows-specific fallback rather than
// relying on POSIX rename semantics). It is also skipped if no `sleep`
// binary is found on PATH.
func TestEnsureShimIntact_RepairSucceedsWhileShimIsExecuting(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ETXTBSY is a Linux/POSIX exec-time restriction with no Windows equivalent; see doc comment")
	}
	sleepBinPath, lookErr := exec.LookPath("sleep")
	if lookErr != nil {
		t.Skip("no `sleep` binary found on PATH to use as a real, directly-executable ELF binary for this reproduction")
	}
	sleepBin, err := os.ReadFile(sleepBinPath)
	require.NoError(t, err)

	ws := t.TempDir()
	withFakeShimBytes(t, sleepBin)

	toolsDir, err := InstallShim(ws)
	require.NoError(t, err)
	shimPath := filepath.Join(toolsDir, "ucd-sh")

	// Execute shimPath itself directly (like production's `/.ucd/ucd-sh
	// pause`), not `sh shimPath` — this is what makes the kernel execve()
	// this exact inode and hold it text-busy for the child's lifetime.
	cmd := exec.Command(shimPath, "5")
	require.NoError(t, cmd.Start())
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// Give the kernel a moment to have actually entered execve() on
	// shimPath before we race a repair against it.
	time.Sleep(200 * time.Millisecond)

	// A payload distinct from what's on disk forces EnsureShimIntact to take
	// the repair path while shimPath is mid-execution above. It is never
	// executed itself, so it need not be a valid ELF — only distinct bytes.
	newPayload := append(append([]byte{}, sleepBin...), 0x00)
	withFakeShimBytes(t, newPayload)

	repaired, err := EnsureShimIntact(toolsDir)
	require.NoError(t, err, "repair via rename must succeed even while the shim is currently executing (ETXTBSY would block an in-place O_TRUNC write here)")
	assert.True(t, repaired)

	got, err := os.ReadFile(shimPath)
	require.NoError(t, err)
	assert.Equal(t, newPayload, got)
}
