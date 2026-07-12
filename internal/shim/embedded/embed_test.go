package embedded

import "testing"

// TestBytes exercises Bytes() without assuming whether the two-stage build
// has run in this checkout, or which of the arch-tagged files
// (embed_amd64.go / embed_arm64.go) supplied payload for the GOARCH this
// test binary was compiled with. Two states are both valid here:
//
//   - Fresh clone / plain `go test` in CI before `make embed-shim`/`make
//     build` runs: the committed placeholder (ucd-sh-amd64 or ucd-sh-arm64,
//     whichever matches GOARCH) is empty, so len(Bytes()) == 0. That is
//     documented, expected behavior — see the package doc comment — not a
//     test failure.
//   - After the two-stage build has overwritten the host-arch placeholder
//     with a real linux ucd-sh binary: len(Bytes()) > 0.
//
// This test asserts Bytes() never panics, is non-nil, and is stable across
// calls, and logs which state it observed. The "real bytes are actually a
// working ucd-sh binary" property is covered separately by `go build
// ./cmd/ucd-sh` (exercised directly in cmd/ucd-sh, and indirectly by the
// Makefile/air/Dockerfile/scripts/build-shims.sh targets that produce this
// embed in the first place) — re-deriving that here would mean
// re-implementing the two-stage build inside a unit test, which is exactly
// the heavyweight approach the brief calls out to avoid.
func TestBytes(t *testing.T) {
	b := Bytes()
	if b == nil {
		t.Fatalf("Bytes() returned nil; want a (possibly zero-length) []byte from go:embed")
	}

	b2 := Bytes()
	if len(b) != len(b2) {
		t.Fatalf("Bytes() not stable across calls: len %d then %d", len(b), len(b2))
	}

	if len(b) == 0 {
		t.Log("embedded ucd-sh is the empty placeholder for this GOARCH (two-stage build has not run in this checkout) — expected on a fresh clone or plain `go build`/`go test`")
		return
	}

	t.Logf("embedded ucd-sh is %d bytes (two-stage build has run)", len(b))
}
