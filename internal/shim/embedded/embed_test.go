package embedded

import "testing"

// TestBytes asserts the committed, generated ucd-sh binary for this GOARCH
// is embedded and stable across calls. The bytes are produced by
// `go generate ./internal/shim/embedded/` (cmd/shimgen) and committed to
// git, so a zero length here means the committed file was truncated or the
// wrong file was committed — a regression, not an expected fresh-clone state.
func TestBytes(t *testing.T) {
	b := Bytes()
	if len(b) == 0 {
		t.Fatalf("Bytes() is empty; the committed ucd-sh-<arch> shim is missing or truncated — run `go generate ./internal/shim/embedded/` and commit")
	}
	if len(Bytes()) != len(b) {
		t.Fatalf("Bytes() not stable across calls")
	}
	t.Logf("embedded ucd-sh is %d bytes", len(b))
}
