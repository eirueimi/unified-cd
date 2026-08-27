package srchash

import (
	"os"
	"path/filepath"
	"testing"
)

// TestHashFile_NormalizesLineEndings is the regression test for the exact
// failure mode PR #153 hit: two files with identical logical content but
// different literal line endings (as a Windows checkout with
// core.autocrlf=true vs. a Linux checkout of the same commit would produce)
// must hash identically. Without normalisation this test fails immediately.
func TestHashFile_NormalizesLineEndings(t *testing.T) {
	dir := t.TempDir()

	crlf := filepath.Join(dir, "crlf.go")
	lf := filepath.Join(dir, "lf.go")
	content := "package foo\n\nfunc bar() int {\n\treturn 1\n}\n"
	crlfContent := ""
	for _, r := range content {
		if r == '\n' {
			crlfContent += "\r\n"
		} else {
			crlfContent += string(r)
		}
	}

	if err := os.WriteFile(crlf, []byte(crlfContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lf, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	hCRLF, err := hashFile(crlf)
	if err != nil {
		t.Fatalf("hashFile(crlf): %v", err)
	}
	hLF, err := hashFile(lf)
	if err != nil {
		t.Fatalf("hashFile(lf): %v", err)
	}
	if hCRLF != hLF {
		t.Fatalf("hashFile is not line-ending independent: CRLF file hashed to %s, LF file (same logical content) hashed to %s", hCRLF, hLF)
	}
}

// TestHashFile_DetectsRealContentChange is the flip side of the line-ending
// test above: normalisation must not become so aggressive that it masks an
// actual content change. A guard that always reports "match" is worse than
// no guard.
func TestHashFile_DetectsRealContentChange(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go")
	b := filepath.Join(dir, "b.go")
	if err := os.WriteFile(a, []byte("package foo\n\nfunc bar() int { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("package foo\n\nfunc bar() int { return 2 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hA, err := hashFile(a)
	if err != nil {
		t.Fatal(err)
	}
	hB, err := hashFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if hA == hB {
		t.Fatalf("hashFile did not detect a real content change: both files hashed to %s", hA)
	}
}

// TestCompute_DeterministicAndStable runs Compute twice against this repo's
// own module root and asserts both a stable, well-formed result and that
// repeated calls agree — the freshness test in internal/shim/embedded
// depends on Compute being exactly reproducible run to run on one checkout.
func TestCompute_DeterministicAndStable(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := FindModuleRoot(wd)
	if err != nil {
		t.Fatalf("FindModuleRoot: %v", err)
	}

	h1, err := Compute(root)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if len(h1) != 64 {
		t.Fatalf("Compute returned %q, want a 64-char hex SHA-256 digest", h1)
	}

	h2, err := Compute(root)
	if err != nil {
		t.Fatalf("Compute (2nd call): %v", err)
	}
	if h1 != h2 {
		t.Fatalf("Compute is not deterministic: %s vs %s", h1, h2)
	}
}

// TestFindModuleRoot_WalksUpToGoMod asserts the walk-up-to-go.mod behaviour
// listDeps and Compute rely on to work correctly when invoked from a
// subdirectory (which is exactly how `go test` runs the freshness test in
// internal/shim/embedded: its working directory is its own package dir, not
// the module root).
func TestFindModuleRoot_WalksUpToGoMod(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := FindModuleRoot(wd)
	if err != nil {
		t.Fatalf("FindModuleRoot(%s): %v", wd, err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("FindModuleRoot returned %s, which has no go.mod: %v", root, err)
	}
}
