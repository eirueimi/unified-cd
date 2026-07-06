package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

// TestExport_RoundTripQualifiedNames verifies that parsing exported files with
// the same rules the AppSource reconciler uses (dir-based qualification)
// reproduces the original qualified job names.
func TestExport_RoundTripQualifiedNames(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	cmd, _ := newTestExportCmd(exportFixtures(false))
	cmd.SetArgs([]string{"-o", dir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}

	wantJobs := map[string]bool{"team-a/build": false, "hello": false}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(b), "kind: Job") {
			return nil
		}
		job, err := dsl.Parse(bytes.NewReader(b))
		if err != nil {
			t.Errorf("%s: exported job must re-parse: %v", path, err)
			return nil
		}
		rel, _ := filepath.Rel(dir, filepath.Dir(path))
		reldir := filepath.ToSlash(rel)
		if reldir == "." {
			reldir = ""
		}
		qualified := dsl.QualifyName(reldir, job.Metadata.Name)
		if _, ok := wantJobs[qualified]; !ok {
			t.Errorf("unexpected qualified name %q from %s", qualified, path)
			return nil
		}
		wantJobs[qualified] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	for name, seen := range wantJobs {
		if !seen {
			t.Errorf("job %q not reproduced by round-trip", name)
		}
	}
}
