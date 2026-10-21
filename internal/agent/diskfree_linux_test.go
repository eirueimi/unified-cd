//go:build linux

package agent

import (
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// A filesystem that reports a total of 0 has no capacity to be a fraction of;
// diskStats must return an error so the caller omits the reading instead of
// publishing "free 0, size 0". procfs reports Blocks == 0 on Linux, which
// makes it a real filesystem to ask. This covers the unix implementation only.
func TestDiskStats_RejectsAFilesystemReportingZeroTotal(t *testing.T) {
	var st unix.Statfs_t
	if err := unix.Statfs("/proc", &st); err != nil {
		t.Skipf("/proc is not available: %v", err)
	}
	if uint64(st.Blocks)*uint64(st.Bsize) != 0 {
		t.Skip("/proc reports a non-zero total on this kernel")
	}

	_, _, err := diskStats("/proc")
	require.Error(t, err)
	require.Contains(t, err.Error(), "total size of 0")
}
