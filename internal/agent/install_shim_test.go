package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withFakeShimBytes overrides the package-level shimBytes indirection (see
// agent.go) so InstallShim's tests don't depend on the two-stage build
// (`make embed-shim`) having populated internal/shim/embedded with a real
// binary — the committed placeholders are intentionally zero bytes (see that
// package's doc comment).
func withFakeShimBytes(t *testing.T, payload []byte) {
	t.Helper()
	orig := shimBytes
	shimBytes = func() []byte { return payload }
	t.Cleanup(func() { shimBytes = orig })
}

// TestInstallShim_EmptyBytesIsHardError is the regression test for the
// "EMPTY Bytes() = hard startup error" requirement (step-shell-shim spec,
// Component 3 "/.ucd injection"): a zero-length embedded payload — the
// committed placeholder, not overwritten by the two-stage build — must fail
// loudly with an actionable message, not silently start without the shim.
func TestInstallShim_EmptyBytesIsHardError(t *testing.T) {
	withFakeShimBytes(t, nil)

	_, err := InstallShim(filepath.Join(t.TempDir(), "workspace"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not embedded")
	assert.Contains(t, err.Error(), "embed-shim", "error must name the two-stage build fix")
}

// TestInstallShim_WritesExecutableFileNextToWorkspaceDir verifies InstallShim
// writes the payload to <dirname(workspaceDir)>/tools/ucd-sh, mode 0755, and
// returns that tools directory.
func TestInstallShim_WritesExecutableFileNextToWorkspaceDir(t *testing.T) {
	payload := []byte("#!/bin/sh\necho fake-shim\n")
	withFakeShimBytes(t, payload)

	base := t.TempDir()
	wsDir := filepath.Join(base, "workspace")

	toolsDir, err := InstallShim(wsDir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(base, "tools"), toolsDir)

	shimPath := filepath.Join(toolsDir, "ucd-sh")
	got, err := os.ReadFile(shimPath)
	require.NoError(t, err)
	assert.Equal(t, payload, got)

	// The exec bit is only meaningful on POSIX filesystems; Windows/NTFS has
	// no equivalent permission bit (os.WriteFile's mode argument is not
	// honored the same way there), so this check is skipped on windows —
	// the real target of 0o755 here is the containers the shim later gets
	// bind-mounted into, which are always Linux.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(shimPath)
		require.NoError(t, err)
		if info.Mode().Perm()&0o100 == 0 {
			t.Fatalf("expected the shim file to be executable (0755), got mode %v", info.Mode())
		}
	}
}

// TestInstallShim_DefaultsEmptyWorkspaceDir verifies InstallShim applies the
// same "~/workspace" default Agent.Run applies to an unset WorkspaceDir, so
// cmd/agent's InstallShim(*workspaceDir) call agrees with Run's own wsBase
// computation even when the flag is left empty. Points HOME/USERPROFILE at a
// throwaway temp dir first so this never touches the real home directory.
func TestInstallShim_DefaultsEmptyWorkspaceDir(t *testing.T) {
	payload := []byte("fake-shim")
	withFakeShimBytes(t, payload)

	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome) // os.UserHomeDir() on Windows

	toolsDir, err := InstallShim("")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(fakeHome, "tools"), toolsDir)
}
