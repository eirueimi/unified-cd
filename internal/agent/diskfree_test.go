package agent

import (
	"os"
	"testing"
)

func TestBelowMinFreeDisk(t *testing.T) {
	cases := []struct {
		name string
		free uint64
		min  uint64
		want bool
	}{
		{"min disabled (zero)", 0, 0, false},
		{"free below min", 100, 1000, true},
		{"free equal to min", 1000, 1000, false},
		{"free above min", 2000, 1000, false},
		{"min disabled even if free is zero", 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := belowMinFreeDisk(tc.free, tc.min); got != tc.want {
				t.Errorf("belowMinFreeDisk(%d, %d) = %v, want %v", tc.free, tc.min, got, tc.want)
			}
		})
	}
}

// TestFreeBytes_RealFilesystem is a smoke test that the platform-specific
// freeBytes implementation returns a plausible non-zero value for a real,
// existing directory (the OS temp dir).
func TestFreeBytes_RealFilesystem(t *testing.T) {
	dir := os.TempDir()
	free, err := freeBytes(dir)
	if err != nil {
		t.Fatalf("freeBytes(%q) error: %v", dir, err)
	}
	if free == 0 {
		t.Errorf("freeBytes(%q) = 0, want a plausible non-zero value", dir)
	}
}
