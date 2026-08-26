package objectstore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeCredFile(t *testing.T, dir, key, secret string) string {
	t.Helper()
	p := filepath.Join(dir, "creds")
	body := "UNIFIED_S3_KEY=" + key + "\nUNIFIED_S3_SECRET=" + secret + "\n"
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

func TestFileCredentials_ReadsTheFile(t *testing.T) {
	p := writeCredFile(t, t.TempDir(), "AKIA1", "s3cr3t1")
	v, err := NewFileCredentials(p).Get()
	require.NoError(t, err)
	assert.Equal(t, "AKIA1", v.AccessKeyID)
	assert.Equal(t, "s3cr3t1", v.SecretAccessKey)
}

// The point of the whole seam: a rewritten file is picked up. Without this,
// a rotated or refreshed credential never reaches the client and the provider
// is no better than a static key pair.
func TestFileCredentials_PicksUpARewrite(t *testing.T) {
	dir := t.TempDir()
	p := writeCredFile(t, dir, "AKIA1", "s3cr3t1")
	c := NewFileCredentials(p)

	first, err := c.Get()
	require.NoError(t, err)
	require.Equal(t, "AKIA1", first.AccessKeyID)

	writeCredFile(t, dir, "AKIA2", "s3cr3t2")

	second, err := c.Get()
	require.NoError(t, err)
	assert.Equal(t, "AKIA2", second.AccessKeyID,
		"a rewritten credential file must be re-read; the kubelet updates a mounted Secret in place")
}

// A missing or unreadable file is an error, not empty credentials. Empty
// credentials produce a signature failure at transfer time, which is a much
// worse message than "cannot read the credential file".
func TestFileCredentials_MissingFileErrors(t *testing.T) {
	_, err := NewFileCredentials(filepath.Join(t.TempDir(), "absent")).Get()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absent")
}

func TestFileCredentials_IncompleteFileErrors(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "creds")
	require.NoError(t, os.WriteFile(p, []byte("UNIFIED_S3_KEY=AKIA1\n"), 0o600))
	_, err := NewFileCredentials(p).Get()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UNIFIED_S3_SECRET")
}
