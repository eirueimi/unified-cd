package agent

import (
	"path/filepath"
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

// TestDiskStats_ReportsBothAndTotalExceedsFree is a smoke test that the
// platform-specific diskStats returns plausible, mutually consistent values
// for a real, existing directory.
func TestDiskStats_ReportsBothAndTotalExceedsFree(t *testing.T) {
	dir := t.TempDir()
	free, total, err := diskStats(dir)
	if err != nil {
		t.Fatalf("diskStats(%q) error: %v", dir, err)
	}
	if total == 0 {
		t.Errorf("diskStats(%q) total = 0, want a plausible non-zero value", dir)
	}
	if free == 0 {
		t.Errorf("diskStats(%q) free = 0, want a plausible non-zero value", dir)
	}
	if free > total {
		t.Errorf("diskStats(%q) free = %d exceeds total = %d", dir, free, total)
	}
}

// TestDiskStats_MissingPathIsAnError pins that an unmeasurable path surfaces
// as an error rather than as zeros, which a caller would read as "disk full".
func TestDiskStats_MissingPathIsAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does", "not", "exist")
	free, total, err := diskStats(missing)
	if err == nil {
		t.Fatalf("diskStats(%q) = (%d, %d, nil), want an error", missing, free, total)
	}
}
