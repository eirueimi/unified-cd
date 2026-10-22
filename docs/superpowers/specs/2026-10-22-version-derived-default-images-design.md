# Version-derived default images — Design

Date: 2026-10-22
Status: Approved (direction chosen by the maintainer)

## 1. Purpose

The fleet-wide default images are pinned by digest today
(`repo:tag@sha256:<digest>`). The pin has to be rotated by hand on every
image release, nothing checks that it is still current, and the three tests
that guard it assert only that the string is a well-formed digest — so a
stale-but-pinned digest passes.

The cost of that is not hypothetical. The named-volumes work added an
`--exclude` flag to the artifact sidecar, and the pinned
`defaultSidecarImage` digest predates it, so a stock Kubernetes agent breaks
every job that archives an ancestor of a volume until somebody remembers to
rotate. Worse, the rotation can never be done in the same release that
publishes the image: the digest is only knowable after the image is pushed,
so a digest pin can only ever name the *previous* release.

**The decision taken here: the defaults name a tag, and digest pinning becomes
the operator's choice rather than the project's default.**

## 2. Goals, non-goals, anti-goals

**Goals**

- A given release's agent pulls that same release's images, by construction.
- No manual rotation step on any release.
- An operator who needs immutability can still have it, by configuration.

**Non-goals**

- Immutable defaults. That is what is being given up, deliberately.
- Automating digest rotation. It was considered and rejected in favour of
  removing the need for it.
- Changing `busybox` handling. It is not a unified-cd image and has no
  release identity in this tree, so it keeps its digest pin. An earlier draft
  of this document dropped that digest in §4.1 while this non-goal said it was
  unchanged; the digest stays. The maintainer's decision was about the
  rotation chore that unified-cd's own releases create, and `busybox` creates
  none — its pin only changes when somebody deliberately bumps the `busybox`
  version, which is a normal edit with no circularity.

**Anti-goals** — outcomes that make this a failure even if it ships:

- A silent posture change. Someone upgrading must be told that the default is
  no longer immutable, and told how to get immutability back.
- A default that pulls an image from a *different* release than the binary.
  That is the version skew the existing lockstep requirement forbids, and it
  is the thing this change exists to make structurally impossible.
- Replacing a hand-maintained digest with a hand-maintained tag. That is the
  same defect with a smaller blast radius, and this repository's most
  repeated defect class.

## 3. Current state

| Constant | Where | Today |
|---|---|---|
| `defaultRunnerImage` | `cmd/unified-cd-agent/main.go:26` | `unified-cd-runner:v0.0.3@sha256:…` |
| `defaultPauseImage` | `cmd/unified-cd-agent/main.go:27` | `busybox:1.36@sha256:…` |
| `defaultPodImage` | `internal/k8sagent/config.go:156` | `unified-cd-runner:v0.0.3@sha256:…` |
| `defaultSidecarImage` | `internal/k8sagent/config.go:173` | `artifact-sidecar:latest@sha256:…` |
| `defaultShimImage` | `internal/k8sagent/config.go:146` | `unified-cd-k8s-agent:latest` — already not pinned |

`defaultShimImage`'s comment already works this through at length. It explains
that pinning *it* would be strictly worse because of circularity — the digest
of the image a commit produces cannot be written into that commit — and then
names the resolution:

> The resolution that satisfies both lockstep and immutability is a
> version-derived default (":" + the agent's build version, falling back to
> ":latest" for dev builds), which needs no digest and so has no circularity.

It records that resolution as blocked, because "no Dockerfile passes
-ldflags today, so every containerised agent reports dev".

**That blocker no longer exists.** Every Go-building Dockerfile passes
`-ldflags "-X …Version=${VERSION}"` with `ARG VERSION=dev`
(`docker/agent.Dockerfile`, `k8s-agent.Dockerfile`, `controller.Dockerfile`,
`artifact-sidecar.Dockerfile`), and `.github/workflows/release-docker.yml`
supplies `VERSION=${{ github.ref_name }}`. `docker/runner.Dockerfile` builds
no Go binary and says so. The comment's premise is stale, and correcting it is
part of this change.

`release-docker.yml` publishes all five images on a `v*` tag, applying both
`:latest` and `:<tag>`, and a `verify` job asserts the tags actually resolved.

## 4. Design

### 4.1 The rule

A default image that names a **unified-cd-owned** image is
`<repo>:<tag>`, where the tag is the agent binary's own build version when it
has one, and `latest` otherwise:

- `defaultRunnerImage` → `ghcr.io/eirueimi/unified-cd-runner:<version>`
- `defaultPodImage` → the same image, same rule
- `defaultSidecarImage` → `ghcr.io/eirueimi/unified-cd-artifact-sidecar:<version>`
- `defaultShimImage` → `ghcr.io/eirueimi/unified-cd-k8s-agent:<version>`

A default that names a **third-party** image is left exactly as it is, digest
and all, because it has no release identity in this tree and therefore no
rotation chore tied to a unified-cd release:

- `defaultPauseImage` → unchanged, `busybox:1.36@sha256:…`

Because all five unified-cd images are built and tagged by the same workflow
from the same git tag, a `v0.7.0` agent asking for `:v0.7.0` is asking for an
image that exists and was built from the same commit. Lockstep holds by
construction rather than by a procedure someone has to remember.

### 4.2 Which versions count, and the fallback

The version a binary reports is **not** uniformly `dev` or `vX.Y.Z`, and the
differences decide what tag gets requested:

| Build path | What `Version` holds |
|---|---|
| `release-docker.yml` (images) | `v0.7.0` — `github.ref_name`, with the `v` |
| goreleaser (host binaries) | `0.7.0` — `.goreleaser.yaml` stamps `{{.Version}}`, which **strips the `v`** |
| `make` | `git describe --tags --always --dirty` → `v0.6.0-3-gabc123-dirty`, or a bare SHA |
| plain `go build`, Docker without `--build-arg` | `dev` |

So the predicate is:

- Accept `^v?\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$` and **normalise to a leading
  `v`**, because the image tags carry it and goreleaser's value does not.
  Requiring a leading `v` instead — as an earlier draft of §7 suggested —
  would send every release host-agent binary down the fallback path, which is
  the opposite of the intent.
- **That accept pattern alone is not enough, and an earlier draft of this
  section was wrong to imply it was.** It matches
  `v0.6.0-3-gabc123-dirty`, because a semver pre-release identifier may
  contain hyphens, digits and letters — the `git describe` form is not a case
  of extra dot-separated segments. So a second check rejects what
  `git describe --tags --always --dirty` appends:
  `(-dirty|-\d+-g[0-9a-f]+(-dirty)?)$`. This was found by running the pattern
  rather than reading it.

  The cost, stated because it is a real one: a genuine pre-release tag shaped
  like `v0.7.0-5-gdeadbeef` would be read as a dev build and fall back to
  `latest`. Nothing would tag a release that way, so this is accepted.

  The deeper point is that provenance is being inferred from the *shape* of a
  human-facing string, and no purely syntactic rule can separate a describe
  output from a pre-release tag that happens to look like one. The clean
  alternative is to stamp a second variable that only the release path sets
  (goreleaser's `{{.Tag}}`, which keeps the `v`, and `github.ref_name` for
  the images), making the helper "empty means dev" with no parsing and no
  normalisation at all. That was not taken here because it touches four
  Dockerfiles, the goreleaser config and the release workflow to fix a case
  that cannot realistically occur — but it is the right shape if this
  heuristic ever bites.
- A pre-release like `v0.7.0-rc1` is accepted. `release-docker.yml` triggers
  on `v*`, so that tag really is published, and accepting it keeps an rc
  self-consistent.
- Everything else — `dev`, `v0.6.0-3-gabc123-dirty`, a bare SHA — falls back
  to `:latest`, because none of them names a published tag.

The helper takes the version **as a parameter** and is exported from
`internal/agent` (which both `cmd/unified-cd-agent` and `internal/k8sagent`
already import, so no new dependency edge). That is not a style preference: a
`const` cannot be derived from a `var`, so these become functions, and a
helper that reads the package variable directly cannot be tested at all —
a test binary always has `Version == "dev"`, so every "the tag equals the
version" assertion would be vacuous.

### 4.3 What an operator who needs immutability does

Set the image explicitly to a digest. Every one of these is already a
configuration field — `runnerImage` and `pauseImage` on the host agent,
`podImage`, `sidecarImage` and `shimImage` on the Kubernetes agent — so the
mechanism exists and needs no new surface. This is exactly how
`defaultShimImage`'s comment already tells operators to close its exposure,
so this change generalises an established pattern rather than inventing one.

### 4.4 The exposure being accepted

A mutable tag means whoever can write to the registry repository can change
what every agent pulls. For the runner and pod images that is code execution
in the primary container of every isolated job; for the artifact sidecar it is
additionally a credential-exfiltration path, because that sidecar holds
long-lived bucket-scoped static S3 credentials. This is the posture the
digest pins were protecting, and it is being traded for lockstep correctness
and the removal of a rotation step that has already failed once in a way that
shipped broken behaviour.

The trade is the maintainer's call and is recorded here as theirs. The
documentation obligation in §6 is not optional: an operator must meet this
fact where they would act on it.

### 4.5 Operational consequences, all of them

These follow from the change and must be documented, not discovered:

- **Pull policy flips with the tag.** Nothing in this tree sets
  `imagePullPolicy`, so Kubernetes defaults it: `Always` for `:latest`,
  `IfNotPresent` for a versioned tag. A release agent therefore caches its
  images and a dev agent re-pulls on every pod. That is a behaviour
  difference between build paths, not just a naming one.
- **A failed release now fails loudly instead of running stale.** If the
  image build fails, `verify` keeps every tag unmoved, but the git tag still
  exists and the binary built from it still asks for `:v0.7.0` — which does
  not resolve, so pods land in `ImagePullBackOff`. Previously the old digest
  kept working, silently one release behind. Loud is the better failure, and
  the recovery is to re-push the tag; both belong in the docs.
- **A mirrored or air-gapped registry must carry the new tag.** The pinned
  digest used to stay valid across restarts and upgrades; a version-derived
  tag changes every release, so an operator mirroring images gains an upgrade
  step. This needs an upgrade-checklist entry, not a footnote.
- **A locally built agent still skews.** A `make` or `go build` agent asks
  for `:latest` and gets whichever release is newest, which is exactly the
  skew §2 calls an anti-goal. The change makes *released* agents
  self-consistent; it does not fix locally built ones, and claiming otherwise
  would be false.

### 4.6 The tests change meaning

`TestDefaultImagesAreDigestPinned` (`cmd/unified-cd-agent/main_test.go`),
`TestDefaultPodImageIsDigestPinned` and
`TestDefaultSidecarImageIsDigestPinned` (`internal/k8sagent/config_test.go`)
exist to stop precisely this change. They are not obstacles to route around:
they are the guard for the old invariant, and they must be *replaced* by a
guard for the new one, not deleted. The new invariant worth pinning:

- a unified-cd default image carries no `@sha256:` digest (this one fails
  against the current code, so it is a real guard);
- the helper maps `v0.7.0`, `0.7.0` and `v0.7.0-rc1` to `:v0.7.0`,
  `:v0.7.0` and `:v0.7.0-rc1` — a table test, with the normalisation of the
  missing `v` as its own case, since that is the goreleaser path;
- it maps `dev`, a bare SHA and `v0.6.0-3-gabc123-dirty` to `:latest`;
- `defaultPauseImage` still names `busybox` **with its digest** and is not
  version-derived. Assert the digest is present and that no substitution
  happened: a test that would pass if someone "made it consistent" with the
  others is not a test, and this is the sibling-consistency trap that the
  `defaultShimImage` comment was written to prevent in the other direction.

## 5. Decisions taken in this design

1. **Tags, not digests, for unified-cd-owned defaults.** The maintainer's
   call, made after the digest rationale was laid out.
2. **Version-derived rather than a hand-written tag constant.** A
   hand-maintained tag is the same drift defect as a hand-maintained digest.
   This is also the resolution `defaultShimImage`'s own comment prescribes.
3. **`busybox` is left untouched, digest included.** It has no release
   identity here, so there is nothing to derive from and no rotation chore
   tied to a unified-cd release. Dropping its digest would be a posture change
   with nothing bought.
4. **Digest immutability moves to configuration**, reusing fields that already
   exist.
5. **The three digest-pin tests are replaced, not removed.**
6. **The stale "no Dockerfile passes -ldflags" premise is corrected** in the
   same change, since it is the reason this was deferred.
7. **A pre-release tag counts as a version, and `release-docker.yml` is left
   alone.** Pushing `v0.7.0-rc1` publishes both `:v0.7.0-rc1` and `:latest`,
   so an rc moves `:latest` for every default-configured agent that falls back
   to it. The maintainer accepted that: the rc's own agent stays
   self-consistent because `:v0.7.0-rc1` exists, and suppressing `:latest`
   for pre-releases is a separate change to the release workflow, out of
   scope here.

## 6. Documentation obligations

Per `AGENTS.md`, not done until these agree with the code:

- `docs/operator-manual/operations.md` — the "Rotating the default
  runner/pause image digests" section largely ceases to apply and must be
  rewritten rather than left describing a procedure that no longer exists.
  What remains is `busybox`. The upgrade checklist item that calls rotation
  "a required step of every runner-image release" becomes wrong.
  This section also currently contradicts itself and must be fixed while
  being rewritten: step 1 resolves
  `unified-cd-artifact-sidecar:vX.Y.Z` while step 2 claims "there is no
  `vX.Y.Z` release tag to bump for this image" — `release-docker.yml` tags
  `artifact-sidecar` with `:$TAG` like every other image, so step 2 is false.
- `internal/k8sagent/config.go` — `defaultShimImage`'s long comment is the
  single best explanation of this problem in the tree. Its reasoning survives;
  its stale blocker and its "NOTE ON WHAT :latest MEANS TODAY" paragraph do
  not. Rewrite, keep the circularity argument, and keep warning the next
  reader off re-pinning.
- `docs/operator-manual/kubernetes-integration.md` — its "Shim image" section
  tells operators how to pin; that guidance now applies to every default
  image, not just the shim.
- `docs/reference/configuration.md` — the image fields' descriptions.
- `examples/config/agent.yaml` and `examples/config/k8s-agent.yaml` both pin
  `unified-cd-runner:v0.0.3`, which is the old default spelled out; they are
  the first thing a user copies.
- `manifests/*/agent-config-patch.yaml` sets `podImage: golang:1.24-alpine`,
  which overrides the default entirely — check it still makes sense and says
  why it overrides.
- A migration note under `docs/operator-manual/migrations/` — the default is
  no longer immutable. State it plainly, say who is affected, give the exact
  configuration that restores a digest pin, and cover every consequence in
  §4.5: the pull-policy flip, the loud failure when a release's images did not
  publish, and the mirrored-registry upgrade step.

## 7. Left to the builder

- The helper's exact name and signature. §4.2 fixes where it lives, that it
  takes the version as a parameter, and the predicate it implements.
- Whether `defaultPodImage` and `defaultRunnerImage` collapse into one
  exported helper now that both produce the same string by the same rule.
  They sit in different packages (`cmd/unified-cd-agent` and
  `internal/k8sagent`), so collapsing means exporting from `internal/agent`.
- The three call sites that read these constants today —
  `internal/k8sagent/podbuilder.go`'s `defaultPodSpec`, and `DefaultConfig`
  and `Validate` in `internal/k8sagent/config.go` — must all go through the
  one helper rather than each deriving a tag; existing tests compare against
  the constants and will need to compare against the helper.
- Whether the k8s-agent should gain image flags. It has none today, so an
  operator pinning a digest edits the ConfigMap, while the host agent has
  `--runner-image` / `--pause-image`. Not required by this design; worth a
  line in the report if it looks cheap.
