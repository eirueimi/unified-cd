package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eirueimi/unified-cd/internal/agent"
	"github.com/eirueimi/unified-cd/internal/config"
)

// setAgentVersion stamps agent.Version for one test. A test binary always
// reports "dev", which maps to ":latest" -- a test reading the default image
// without stamping a version could not tell "derived from the version" from
// "hard-coded :latest".
func setAgentVersion(t *testing.T, v string) {
	t.Helper()
	old := agent.Version
	agent.Version = v
	t.Cleanup(func() { agent.Version = old })
}

// TestDefaultRunnerImageCarriesNoDigest guards the invariant that replaced
// digest pinning: a unified-cd default image is named by a tag derived from the
// agent's own version, never by a digest.
//
// A digest of an image published from this repository can only be recorded in
// a commit AFTER the image is pushed, so a digest pin can only ever name the
// PREVIOUS release -- the agent of release N would permanently pull release
// N-1's runner. A reader who sees a digest-less default next to the busybox pin
// and "fixes" the inconsistency reintroduces that skew and the manual rotation
// chore. See docs/superpowers/specs/2026-10-22-version-derived-default-images-design.md.
func TestDefaultRunnerImageCarriesNoDigest(t *testing.T) {
	assert.NotContains(t, defaultRunnerImage(), "@sha256:",
		"the default runner image must be version-derived, not digest-pinned: a digest "+
			"for an image built from this tree can only name the previous release")
}

// TestDefaultRunnerImageResolvesThroughVersionedImage pins that the runner
// default is computed from the stamped version rather than holding a literal
// tag. Without this a literal ":v0.0.3" would satisfy the no-digest test above
// and silently reintroduce the skew.
func TestDefaultRunnerImageResolvesThroughVersionedImage(t *testing.T) {
	setAgentVersion(t, "v9.9.9")
	assert.Equal(t, agent.VersionedImage("ghcr.io/eirueimi/unified-cd-runner", "v9.9.9"), defaultRunnerImage())
	assert.Equal(t, "ghcr.io/eirueimi/unified-cd-runner:v9.9.9", defaultRunnerImage())

	setAgentVersion(t, "dev")
	assert.Equal(t, "ghcr.io/eirueimi/unified-cd-runner:latest", defaultRunnerImage(),
		"a build that is not a release must fall back to :latest")
}

// TestDefaultPauseImageStaysDigestPinned guards the one default that is
// DELIBERATELY not version-derived. busybox is a third-party image with no
// unified-cd release identity, so there is no release it could be in lockstep
// with and no rotation chore tied to one; dropping its digest would only weaken
// the supply-chain posture. If this test fails because someone made the pause
// image consistent with its siblings, the change is wrong, not the test.
func TestDefaultPauseImageStaysDigestPinned(t *testing.T) {
	setAgentVersion(t, "v9.9.9")
	assert.Regexp(t, `^busybox:[^@]+@sha256:[0-9a-f]{64}$`, defaultPauseImage,
		"the pause image must stay a busybox image pinned to a complete sha256 digest")
	assert.False(t, strings.Contains(defaultPauseImage, "9.9.9") || strings.Contains(defaultPauseImage, "unified-cd"),
		"the pause image must not be derived from the agent version: busybox has no unified-cd release")
}

// TestEveryDefaultImageIsClassified covers the SET of default images in this
// package rather than trusting the two tests above to be exhaustive. They name
// the runner and pause images by hand, so a third default image added to
// main.go would be covered by neither. This scans the package's non-test source
// for every top-level identifier named default<Something>Image and requires it
// to appear in the table below, so adding one fails here until its policy
// (version-derived or deliberately pinned) is decided and a test exists for it.
//
// When it fails: add the new identifier to the table, and add or extend a test
// for its policy (TestDefaultRunnerImageResolvesThroughVersionedImage for a
// version-derived default, TestDefaultPauseImageStaysDigestPinned for a pin).
func TestEveryDefaultImageIsClassified(t *testing.T) {
	classified := map[string]string{
		"defaultRunnerImage": "version-derived (TestDefaultRunnerImageResolvesThroughVersionedImage)",
		"defaultPauseImage":  "digest-pinned third-party (TestDefaultPauseImageStaysDigestPinned)",
	}
	isDefaultImage := regexp.MustCompile(`^default\w*Image$`)

	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing package source: %v", err)
	}
	found := map[string]bool{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					if d.Recv == nil && isDefaultImage.MatchString(d.Name.Name) {
						found[d.Name.Name] = true
					}
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						if vs, ok := spec.(*ast.ValueSpec); ok {
							for _, n := range vs.Names {
								if isDefaultImage.MatchString(n.Name) {
									found[n.Name] = true
								}
							}
						}
					}
				}
			}
		}
	}
	require.NotEmpty(t, found, "found no default*Image identifier at all; has the naming convention changed?")
	for name := range found {
		assert.Contains(t, classified, name,
			"%s is a default image with no recorded policy: classify it in this test and cover it", name)
	}
	for name := range classified {
		assert.True(t, found[name], "%s is classified here but no longer exists in main.go", name)
	}
}

// TestRunnerImageFlagDefaultIsVersionDerived covers the one place the runner
// default is consumed, the --runner-image flag registered in main(). Testing
// defaultRunnerImage() alone cannot see that call site: reverting the flag to a
// literal tag would leave every other test green while host agents pull a stale
// runner. So this runs the real main() in a child process, with a stamped
// version, and reads the default out of its -h output.
func TestRunnerImageFlagDefaultIsVersionDerived(t *testing.T) {
	if os.Getenv("UCD_TEST_RUN_MAIN_HELP") == "1" {
		agent.Version = "v9.9.9"
		os.Args = []string{"unified-cd-agent", "-h"}
		main() // -h prints the usage with every flag default and exits
		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunnerImageFlagDefaultIsVersionDerived$")
	// A scratch working directory so no unified-agent.yaml is picked up, and no
	// inherited UNIFIED_AGENT_* variable can change the effective default.
	cmd.Dir = t.TempDir()
	env := []string{"UCD_TEST_RUN_MAIN_HELP=1"}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "UNIFIED_AGENT_") {
			env = append(env, kv)
		}
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running main -h: %v: %s", err, out)
	}
	assert.Contains(t, string(out), `(default "ghcr.io/eirueimi/unified-cd-runner:v9.9.9")`,
		"--runner-image must default to the runner derived from the agent's version")
}

func TestParseMinFreeDiskFlag(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want config.DiskThreshold
	}{
		{"", config.DiskThreshold{}},
		{"5368709120", config.DiskThreshold{Bytes: 5368709120, Set: true}},
		{"1%", config.DiskThreshold{Percent: 1, Set: true}},
		{"10GiB", config.DiskThreshold{Bytes: 10 << 30, Set: true}},
		{"0", config.DiskThreshold{Set: true, Off: true}},
	} {
		got, err := parseMinFreeDiskFlag(tc.in)
		require.NoError(t, err, tc.in)
		assert.Equal(t, tc.want, got, tc.in)
	}
	_, err := parseMinFreeDiskFlag("lots")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--min-free-disk")
}

func TestMinFreeDiskLogValue(t *testing.T) {
	assert.Equal(t, "minFreeDisk=1% (default)", minFreeDiskLogValue(config.DiskThreshold{}))
	assert.Equal(t, "minFreeDisk=5368709120 bytes", minFreeDiskLogValue(config.DiskThreshold{Bytes: 5368709120, Set: true}))
	assert.Equal(t, "minFreeDisk=2%", minFreeDiskLogValue(config.DiskThreshold{Percent: 2, Set: true}))
	assert.Equal(t, "disabled", minFreeDiskLogValue(config.DiskThreshold{Set: true, Off: true}))
}
