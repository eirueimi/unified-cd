package shim

import (
	"path/filepath"
	"testing"
)

func TestStatfs_ReportsFreeAndTotalForExistingDir(t *testing.T) {
	free, total, err := Statfs(t.TempDir())
	if err != nil {
		t.Fatalf("Statfs: %v", err)
	}
	// free may legitimately be 0 on a full disk; total never is.
	if total == 0 {
		t.Fatal("total = 0, want a positive size")
	}
	if free > total {
		t.Fatalf("free = %d exceeds total = %d", free, total)
	}
}

func TestStatfs_MissingPathIsAnErrorNeverZeros(t *testing.T) {
	free, total, err := Statfs(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("Statfs of a missing path returned no error")
	}
	if free != 0 || total != 0 {
		t.Fatalf("got (%d, %d) alongside an error, want (0, 0)", free, total)
	}
}
