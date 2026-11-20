package artifact

import "testing"

func TestParseKey(t *testing.T) {
	key, err := ArtifactKey("run-1", "my.out")
	if err != nil {
		t.Fatal(err)
	}
	runID, name, err := ParseKey(key)
	if err != nil || runID != "run-1" || name != "my.out" {
		t.Fatalf("ParseKey(%q) = %q, %q, %v", key, runID, name, err)
	}
	for _, bad := range []string{
		"", "artifacts/", "artifacts/run-1", "artifacts/run-1/x", "artifacts/run-1/.tar.gz",
		"artifacts//x.tar.gz", "artifacts/run-1/a/b.tar.gz", "artifacts/../x.tar.gz",
		"artifacts/run-1/..tar.gz", "caches/run-1/x.tar.gz", "artifacts/run-1/a\\b.tar.gz",
	} {
		if r, n, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) = %q, %q, nil; want an error", bad, r, n)
		}
	}
}
