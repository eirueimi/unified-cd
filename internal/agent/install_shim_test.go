package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withFakeShimBytes overrides the package-level shimBytes indirection (see
// agent.go) so InstallShim's tests don't depend on the committed
// internal/shim/embedded/ucd-sh-<arch> bytes; a zero-length payload means
// that file is missing or empty.
func withFakeShimBytes(t *testing.T, payload []byte) {
	t.Helper()
	orig := shimBytes
	shimBytes = func() []byte { return payload }
	t.Cleanup(func() { shimBytes = orig })
}

// TestInstallShim_EmptyBytesIsHardError is the regression test for the
// "EMPTY Bytes() = hard startup error" requirement (step-shell-shim spec,
// Component 3 "/.ucd injection"): a zero-length embedded payload — meaning
// the committed internal/shim/embedded/ucd-sh-<arch> bytes; a zero-length
// payload means that file is missing or empty — must fail loudly with an
// actionable message, not silently start without the shim.
func TestInstallShim_EmptyBytesIsHardError(t *testing.T) {
	withFakeShimBytes(t, nil)

	_, err := InstallShim(filepath.Join(t.TempDir(), "workspace"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not embedded")
	assert.Contains(t, err.Error(), "go generate", "error must name the regeneration fix")
}

// TestInstallShim_WritesExecutableFileUnderWorkspaceDir verifies InstallShim
// writes the payload to <workspaceDir>/.ucd-tools/ucd-sh, mode 0755, and
// returns that tools directory. toolsDir must live UNDER workspaceDir (not
// beside it) so it shares whatever mount makes workspaceDir visible to a
// possibly-remote container runtime — see InstallShim's doc comment.
func TestInstallShim_WritesExecutableFileUnderWorkspaceDir(t *testing.T) {
	payload := []byte("#!/bin/sh\necho fake-shim\n")
	withFakeShimBytes(t, payload)

	base := t.TempDir()
	wsDir := filepath.Join(base, "workspace")

	toolsDir, err := InstallShim(wsDir)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(wsDir, ".ucd-tools"), toolsDir)
	assert.True(t, strings.HasPrefix(toolsDir, filepath.Clean(wsDir)+string(filepath.Separator)),
		"toolsDir %q must be nested under workspaceDir %q so a remote container runtime sharing workspaceDir's mount can also see toolsDir", toolsDir, wsDir)

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
// cmd/unified-cd-agent's InstallShim(*workspaceDir) call agrees with Run's own wsBase
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
	assert.Equal(t, filepath.Join(fakeHome, "workspace", ".ucd-tools"), toolsDir)
}
