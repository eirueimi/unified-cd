package config

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testKeyHex = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"

func writeKeyFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "kek")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestKeySource_BothFileAndKMSIsAnError(t *testing.T) {
	ks := KeySource{KeyFile: "/tmp/kek", KMSURI: "hashivault://kek"}
	err := ks.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exactly one")
}

func TestKeySource_NothingConfiguredIsAnError(t *testing.T) {
	_, err := KeySource{}.Resolve(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unified-cli keygen",
		"the error must tell the operator how to produce a key")
	assert.Contains(t, err.Error(), "UNIFIED_CONTROLLER_KEY_FILE")
}

func TestKeySource_ReadsKeyFile(t *testing.T) {
	got, err := KeySource{KeyFile: writeKeyFile(t, testKeyHex)}.Resolve(context.Background())
	require.NoError(t, err)
	require.NotNil(t, got.KeyManager)
	assert.Contains(t, got.Description, "key file")
	assert.Empty(t, got.Warnings, "a 0600 key file must not warn")
}

// Editors and `echo` append newlines; a trailing newline must not break startup.
func TestKeySource_TrimsWhitespaceInKeyFile(t *testing.T) {
	_, err := KeySource{KeyFile: writeKeyFile(t, "  "+testKeyHex+"\n")}.Resolve(context.Background())
	require.NoError(t, err)
}

func TestKeySource_RejectsShortKey(t *testing.T) {
	_, err := KeySource{KeyFile: writeKeyFile(t, "0102030405")}.Resolve(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "64 hex")
}

func TestKeySource_MissingKeyFileReportsPath(t *testing.T) {
	_, err := KeySource{KeyFile: filepath.Join(t.TempDir(), "absent")}.Resolve(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absent")
}

// The warning is returned, not printed: this package does no logging, and
// main.go emits it via slog once the logger exists.
func TestKeySource_WarnsOnWorldReadableKeyFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file permission bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "kek")
	require.NoError(t, os.WriteFile(path, []byte(testKeyHex), 0o644))

	got, err := KeySource{KeyFile: path}.Resolve(context.Background())
	require.NoError(t, err, "a loose mode is a warning, not a failure")
	require.Len(t, got.Warnings, 1)
	assert.Contains(t, got.Warnings[0], "chmod 600")
}

func TestKeySource_DevModeProducesEphemeralKeyAndWarns(t *testing.T) {
	got, err := KeySource{DevMode: true}.Resolve(context.Background())
	require.NoError(t, err)
	require.NotNil(t, got.KeyManager)
	assert.Contains(t, strings.ToLower(got.Description), "ephemeral")
	require.Len(t, got.Warnings, 1)
	assert.Contains(t, got.Warnings[0], "after a restart")
}

// The URI names the key, and optionally the mount it lives on.
func TestParseKMSURI(t *testing.T) {
	mount, key, err := parseHashiVaultURI("hashivault://unified-cd-kek")
	require.NoError(t, err)
	assert.Equal(t, "transit", mount, "the default mount is transit")
	assert.Equal(t, "unified-cd-kek", key)

	mount, key, err = parseHashiVaultURI("hashivault://kms-transit/unified-cd-kek")
	require.NoError(t, err)
	assert.Equal(t, "kms-transit", mount)
	assert.Equal(t, "unified-cd-kek", key)
}

// More than two segments is a configuration error rather than a guess.
func TestParseKMSURI_RejectsTooManySegments(t *testing.T) {
	_, _, err := parseHashiVaultURI("hashivault://a/b/c")
	require.Error(t, err)
}

func TestParseKMSURI_RejectsEmptyKey(t *testing.T) {
	_, _, err := parseHashiVaultURI("hashivault://")
	require.Error(t, err)
}

// A KMS URI with no address cannot work, and must say which variable is missing.
func TestKeySource_KMSRequiresAddress(t *testing.T) {
	_, err := KeySource{KMSURI: "hashivault://kek"}.Resolve(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UNIFIED_VAULT_ADDR")
}

func TestParseAuthParams(t *testing.T) {
	got, err := parseAuthParams("role=unified-cd,mount=kubernetes")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"role": "unified-cd", "mount": "kubernetes"}, got)

	empty, err := parseAuthParams("")
	require.NoError(t, err)
	assert.Empty(t, empty)
}

// A malformed parameter must not be silently dropped: a typo in a
// security-relevant setting must not fail open.
func TestParseAuthParams_RejectsMalformed(t *testing.T) {
	_, err := parseAuthParams("role")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "key=value")
}

func TestKeySource_UnknownKMSSchemeListsSupported(t *testing.T) {
	_, err := KeySource{KMSURI: "wat://nope"}.Resolve(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "hashivault")
}

// Resolve acquires resources for some key sources (a KMS client with a
// background renewal loop), so every Resolved must be closable, and closing a
// source that acquired nothing must be safe.
func TestResolved_CloseIsAlwaysSafe(t *testing.T) {
	got, err := KeySource{KeyFile: writeKeyFile(t, testKeyHex)}.Resolve(context.Background())
	require.NoError(t, err)
	require.NoError(t, got.Close())
	require.NoError(t, got.Close(), "Close must be idempotent")

	dev, err := KeySource{DevMode: true}.Resolve(context.Background())
	require.NoError(t, err)
	require.NoError(t, dev.Close())
}
