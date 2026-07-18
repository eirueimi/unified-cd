package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEnsureShimIntact_RepairsTamperedShim(t *testing.T) {
	ws := t.TempDir()
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
