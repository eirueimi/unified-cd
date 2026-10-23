package agent

import (
	"path/filepath"
	"testing"
)

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
