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
//
// The replacement values are deliberately the SAME LENGTH as the originals,
// and the rewrite happens immediately with no sleep. That is not incidental —
// it is the shape of a real rotation (an S3 access key ID is always 20
// characters, a secret always 40) and it is the case a size-and-mtime
// detector gets wrong: same size, and an mtime that a coarse filesystem
// clock cannot distinguish from the previous write. This test failed on
// Windows against exactly such a detector, which is why the provider now
// compares content.
//
// Do not "fix" a future failure here by sleeping between the writes. A sleep
// would only hide a detector that misses same-tick rotations in production,
// where nothing sleeps on the agent's behalf.
func TestFileCredentials_PicksUpARewrite(t *testing.T) {
	dir := t.TempDir()
	p := writeCredFile(t, dir, "AKIA0000000000000001", "s3cr3t00000000000000000000000000000000001")
	c := NewFileCredentials(p)

	first, err := c.Get()
	require.NoError(t, err)
	require.Equal(t, "AKIA0000000000000001", first.AccessKeyID)

	writeCredFile(t, dir, "AKIA0000000000000002", "s3cr3t00000000000000000000000000000000002")

	second, err := c.Get()
	require.NoError(t, err)
	assert.Equal(t, "AKIA0000000000000002", second.AccessKeyID,
		"a same-length rewrite must be re-read; the kubelet updates a mounted Secret in place and a rotated key is the same length as the one it replaces")
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
