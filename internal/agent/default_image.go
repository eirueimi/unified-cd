package agent

import "regexp"

// releasedVersion matches the versions that can name a published image tag: a
// semantic version with an optional pre-release, with or without the leading
// "v". The "v" is optional because the two release paths disagree about it
// (see VersionedImage). Go's "$" without the (?m) flag matches only at the end
// of the text, so a trailing newline is rejected rather than ignored.
var releasedVersion = regexp.MustCompile(`^v?\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`)

// gitDescribeSuffix matches what `git describe --tags --always --dirty` appends
// to a tag when the build is not exactly that tag: "-<commits>-g<sha>", "-dirty",
// or both. No release workflow publishes an image under such a name.
//
// releasedVersion cannot reject these on its own, and this is the non-obvious
// case: "v0.6.0-3-gabc123-dirty" is a perfectly well-formed pre-release as far
// as the character class goes, because a pre-release identifier may contain
// hyphens, digits and letters. Without this second check every `make` build
// would request a tag that does not exist and fail with ImagePullBackOff
// instead of falling back to latest.
var gitDescribeSuffix = regexp.MustCompile(`(-dirty|-\d+-g[0-9a-f]+(-dirty)?)$`)

// VersionedImage returns repo:tag, where the tag is derived from version so
// that an agent pulls the images published alongside its own release. This is
// what keeps the default images in lockstep with the binary by construction
// instead of by a digest someone has to rotate on every release.
//
// A version that names a release maps to "v<X.Y.Z[-pre]>"; anything else (a
// plain `go build`'s "dev", a `make` build's git-describe string, a bare SHA,
// the empty string) maps to "latest", because none of those names a published
// tag.
//
// The leading "v" is added when missing, not required. The images are tagged
// from the git tag (release-docker.yml stamps "v0.7.0"), but .goreleaser.yaml
// stamps {{.Version}}, which strips the "v", so a release host-agent binary
// reports "0.7.0" while its image is ":v0.7.0". Requiring the "v" would send
// every release host agent to ":latest" -- silently, because ":latest"
// resolves.
//
// version is a parameter rather than a read of Version because a test binary
// always has Version == "dev"; a helper reading the variable could only ever be
// tested against "latest".
func VersionedImage(repo, version string) string {
	if !releasedVersion.MatchString(version) || gitDescribeSuffix.MatchString(version) {
		return repo + ":latest"
	}
	if version[0] != 'v' {
		version = "v" + version
	}
	return repo + ":" + version
}

// runnerImageRepo is the repository of the runner image: the default primary
// container for isolated jobs without a podTemplate job container. It exists
// once, here, because the host agent and the Kubernetes agent must default to
// the SAME image; two copies of this string would let a rename in one leave the
// other quietly pulling a different (or missing) image.
const runnerImageRepo = "ghcr.io/eirueimi/unified-cd-runner"

// DefaultRunnerImage is the fleet-wide default runner image for the running
// agent: the runner published with this agent's own release, or :latest for a
// build that carries no release version. Both the host agent's runnerImage
// default and the Kubernetes agent's podImage default delegate to it.
func DefaultRunnerImage() string {
	return VersionedImage(runnerImageRepo, Version)
}
