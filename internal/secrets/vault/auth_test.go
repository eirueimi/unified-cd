package vault

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTokenFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestStaticTokenAuth_ReadsFile(t *testing.T) {
	a, err := newStaticTokenAuth("", writeTokenFile(t, "s.abc123"))
	require.NoError(t, err)
	got, err := a.login(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "s.abc123", got.Token)
}

// Editors and `echo` append newlines; a trailing newline must not break startup.
func TestStaticTokenAuth_TrimsWhitespace(t *testing.T) {
	a, err := newStaticTokenAuth("", writeTokenFile(t, "  s.abc123\n"))
	require.NoError(t, err)
	got, err := a.login(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "s.abc123", got.Token)
}

// The file is re-read on every login, so an operator can replace a rotated
// token without restarting the controller.
func TestStaticTokenAuth_RereadsFileOnEachLogin(t *testing.T) {
	path := writeTokenFile(t, "s.first")
	a, err := newStaticTokenAuth("", path)
	require.NoError(t, err)

	first, err := a.login(context.Background())
	require.NoError(t, err)
	require.Equal(t, "s.first", first.Token)

	require.NoError(t, os.WriteFile(path, []byte("s.second"), 0o600))
	second, err := a.login(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "s.second", second.Token, "a replaced token file must take effect without a restart")
}

func TestStaticTokenAuth_FilePreferredOverLiteral(t *testing.T) {
	a, err := newStaticTokenAuth("s.from-env", writeTokenFile(t, "s.from-file"))
	require.NoError(t, err)
	got, err := a.login(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "s.from-file", got.Token,
		"a file is preferred: it does not leak into docker inspect or child processes")
}

func TestStaticTokenAuth_LiteralUsedWhenNoFile(t *testing.T) {
	a, err := newStaticTokenAuth("s.from-env", "")
	require.NoError(t, err)
	got, err := a.login(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "s.from-env", got.Token)
}

func TestStaticTokenAuth_NeitherIsAnError(t *testing.T) {
	_, err := newStaticTokenAuth("", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UNIFIED_VAULT_TOKEN_FILE")
	assert.Contains(t, err.Error(), "VAULT_TOKEN")
}

func TestStaticTokenAuth_MissingFileReportsPath(t *testing.T) {
	a, err := newStaticTokenAuth("", filepath.Join(t.TempDir(), "absent"))
	require.NoError(t, err, "a missing file is a login-time failure, not a construction failure")
	_, err = a.login(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absent")
}
