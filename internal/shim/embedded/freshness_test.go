package embedded

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eirueimi/unified-cd/internal/shim/srchash"
)

// TestShimSourceMatchesRecordedHash is the guard this package's doc comment
// describes as replacing the byte-exact CI diff that was tried and reverted
// in PR #157 (see embed.go). It does not compare compiled bytes at all —
// build reproducibility for these binaries is not reliable enough to gate on
// (embed.go's package doc has the evidence) — so instead it answers the one
// question a byte-exact diff could never answer portably: has the shim's
// SOURCE changed since someone last regenerated the committed binaries?
//
// cmd/shimgen recomputes and rewrites internal/shim/embedded/ucd-sh-source.sha256
// every time `go generate ./internal/shim/embedded/...` runs, so a developer
// who edits cmd/ucd-sh or internal/shim and regenerates has nothing extra to
// remember; this test just recomputes the same hash from the CURRENT source
// tree and fails if it disagrees with what's recorded, which can only happen
// when the source moved and regeneration didn't.
//
// srchash.Compute is deliberately platform-independent (it normalises line
// endings and never touches the compiled binaries), so — unlike the reverted
// byte-exact guard, which could never pass from a Windows checkout because of
// core.filemode and could never agree with CI because of non-reproducible
// compiler output — this test is expected to pass identically on Windows,
// macOS and Linux, from this repo's own checkout, always. If it ever doesn't,
// that is itself a bug in srchash, not a reason to weaken this test.
func TestShimSourceMatchesRecordedHash(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := srchash.FindModuleRoot(wd)
	if err != nil {
		t.Fatalf("locate module root: %v", err)
	}

	got, err := srchash.Compute(root)
	if err != nil {
		t.Fatalf("compute current shim source hash: %v", err)
	}

	recordedPath := filepath.Join(root, srchash.RecordedHashPath)
	recordedRaw, err := os.ReadFile(recordedPath)
	if err != nil {
		t.Fatalf("read recorded shim source hash %s: %v (run `go generate ./internal/shim/embedded/...` and commit)", recordedPath, err)
	}
	want := strings.TrimSpace(string(recordedRaw))

	if got != want {
		t.Fatalf(`the source of cmd/ucd-sh / internal/shim no longer matches the committed shim binaries.

recorded hash (%s): %s
current source hash:            %s

This means cmd/ucd-sh or internal/shim (or a dependency pinned in go.mod)
changed since internal/shim/embedded/ucd-sh-amd64 and ucd-sh-arm64 were last
regenerated. To fix: regenerate the shim binaries and commit BOTH the updated
binaries and the updated hash file (cmd/shimgen writes the hash file for you,
so there is no separate manual step):

    go generate ./internal/shim/embedded/...

Prefer running this on LINUX, not Windows or macOS: the committed binaries'
exact bytes are known to differ (though still validly) depending on the host
OS that built them (see embed.go's package doc), so a non-Linux regeneration
churns the git diff more than necessary even though it is not wrong. Via
Docker from the repo root (match the image tag to go.mod's `+"`go`"+` directive):

    docker run --rm -v "$PWD:/work" -w /work golang:1.26.2 go generate ./internal/shim/embedded/...
`, srchash.RecordedHashPath, want, got)
	}
}
