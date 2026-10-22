# Version-derived default images Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A release's agent pulls that release's own images by construction, with no digest to rotate by hand.

**Architecture:** The unified-cd-owned default images become functions of the agent binary's stamped `Version`, through one exported helper in `internal/agent` that takes the version as a parameter. The third-party `busybox` default is left exactly as it is, digest included.

**Tech Stack:** Go. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-10-22-version-derived-default-images-design.md` — read it; this plan argues from it and does not repeat its reasoning.

## Global Constraints

- Everything in this repository is written in English: code, comments, commit messages, docs, error strings. No Japanese.
- No personally identifying information. Placeholders are `/path/to/unified-cd` and `your-username`.
- `docs/reference/field-reference.md` and `schemas/unified-cd.schema.json` are generated; never hand-edit. This change touches no DSL field, so neither should move.
- The accepted predicate is `^v?\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`, **normalised to a leading `v`**. Everything else maps to `latest`. Requiring a leading `v` is wrong: goreleaser stamps `0.7.0` without it.
- `defaultPauseImage` (busybox) keeps its digest and is not version-derived.
- `release-docker.yml` is not modified.

## Review Focus

Failure modes the spec implies that a happy path would not exercise.

1. **The goreleaser path.** A release host-agent binary reports `0.7.0`, not `v0.7.0`. If normalisation is missed it falls back to `:latest` and the whole point is lost — silently, because `:latest` resolves. — Task 1.
2. **A test binary always has `Version == "dev"`.** Any assertion that reads the package variable instead of a parameter is vacuous and will pass whatever the helper does. — Task 1.
3. **Sibling consistency pressure on busybox.** The next reader will want to "finish the job" and make `defaultPauseImage` version-derived too. — Task 1's guard test, and the comment Task 2 writes.
4. **The three call sites.** `defaultPodSpec`, `DefaultConfig` and `Validate` each read a constant today; one left behind keeps the old string and nothing fails. — Task 2.
5. **Docs that describe a procedure that no longer exists.** The rotation section is not merely stale after this change, it instructs an operator to do something impossible. — Task 3.

---

### Task 1: The helper and its guards

**Files:**
- Modify: `internal/agent/` — new file for the helper (name it for what it does, not `util.go`)
- Test: alongside it

**Interfaces:**
- Produces: an exported function in `internal/agent` taking a version string and an image repository, returning `repo:tag`. Both `cmd/unified-cd-agent` and `internal/k8sagent` already import this package, so no new dependency edge.

- [ ] **Step 1: Write the failing table test**

Cases, with the expected tag:

| version in | tag out |
|---|---|
| `v0.7.0` | `v0.7.0` |
| `0.7.0` | `v0.7.0` (the goreleaser path — normalisation) |
| `v0.7.0-rc1` | `v0.7.0-rc1` |
| `dev` | `latest` |
| `v0.6.0-3-gabc123-dirty` | `latest` |
| `abc1234` (bare SHA) | `latest` |
| `` (empty) | `latest` |

- [ ] **Step 2: Run it and watch it fail** — the helper does not exist.

- [ ] **Step 3: Implement the helper** per the predicate in Global Constraints.

- [ ] **Step 4: Run it and watch it pass.**

- [ ] **Step 5: Commit.**

---

### Task 2: The four defaults become version-derived

**Files:**
- Modify: `cmd/unified-cd-agent/main.go` (`defaultRunnerImage`; leave `defaultPauseImage` alone)
- Modify: `internal/k8sagent/config.go` (`defaultPodImage`, `defaultSidecarImage`, `defaultShimImage`, and `DefaultConfig` / `Validate`)
- Modify: `internal/k8sagent/podbuilder.go` (`defaultPodSpec`)
- Test: `cmd/unified-cd-agent/main_test.go`, `internal/k8sagent/config_test.go`

**Interfaces:**
- Consumes: Task 1's helper.

- [ ] **Step 1: Replace the three digest-pin tests**

`TestDefaultImagesAreDigestPinned`, `TestDefaultPodImageIsDigestPinned` and `TestDefaultSidecarImageIsDigestPinned` are the guard for the invariant being retired. Replace them — do not delete them — with:
- a unified-cd default image carries no `@sha256:`;
- `defaultPauseImage` still carries one, still names `busybox`, and contains no substituted version;
- each default resolves through Task 1's helper rather than holding a literal tag.

Run them and watch the first fail against the current code. If it passes before the change, it is not a guard — say so and fix the test.

- [ ] **Step 2: Convert the constants**

They become functions, because a `const` cannot be derived from a `var`. Keep each one's doc comment and its reasoning; `defaultShimImage`'s comment is the best explanation of this problem in the tree — keep the circularity argument, delete the stale "no Dockerfile passes -ldflags" blocker and the "NOTE ON WHAT :latest MEANS TODAY" paragraph, and keep warning the next reader off re-pinning. Add, to `defaultPauseImage`, why it is deliberately the odd one out.

- [ ] **Step 3: Update all three call sites** — `defaultPodSpec`, `DefaultConfig`, `Validate` — plus any test comparing against a constant.

- [ ] **Step 4: Verify**

Run: `go build ./...`, `go vet ./...`, `go test ./cmd/... ./internal/agent/ ./internal/k8sagent/ -count=1`
Expected: PASS, and `grep -rn "@sha256:" cmd/ internal/ --include=*.go` shows only the busybox pin.

- [ ] **Step 5: Commit.**

---

### Task 3: Documentation

**Files:**
- Modify: `docs/operator-manual/operations.md`, `docs/operator-manual/kubernetes-integration.md`, `docs/reference/configuration.md`, `examples/config/agent.yaml`, `examples/config/k8s-agent.yaml`, `manifests/*/agent-config-patch.yaml`
- Create: `docs/operator-manual/migrations/2026-10-22-version-derived-default-images.md`

- [ ] **Step 1: Rewrite the rotation section in `operations.md`**

"Rotating the default runner/pause image digests" now applies to `busybox` alone. Everything about the unified-cd images goes, including the upgrade-checklist item calling rotation "a required step of every runner-image release". While rewriting, fix the self-contradiction already there: step 1 resolves `unified-cd-artifact-sidecar:vX.Y.Z` while step 2 claims no such tag exists — `release-docker.yml` tags it like every other image, so step 2 is false.

- [ ] **Step 2: Write the migration note**

The default is no longer immutable. Who is affected, the exact configuration that restores a digest pin, and every consequence from spec §4.5: the pull-policy flip (`:latest` → `Always`, versioned → `IfNotPresent`), the loud `ImagePullBackOff` when a release's images did not publish and how to recover, the mirrored-registry upgrade step, and that a locally built agent still pulls `:latest` and so still skews.

- [ ] **Step 3: The remaining pages**

`kubernetes-integration.md`'s "Shim image" section tells operators how to pin; that guidance now covers every default image. `configuration.md`'s image rows show the old defaults and a sidecar digest, and its `shimImage` row says "Unlike sidecarImage/podImage" — which is now backwards. `examples/config/*.yaml` pin `unified-cd-runner:v0.0.3`. `manifests/*/agent-config-patch.yaml` sets `podImage: golang:1.24-alpine` — confirm it still makes sense and says why it overrides.

- [ ] **Step 4: Sweep**

`grep -rn "v0.0.3\|@sha256:" docs/ examples/ templates/ manifests/ README.md` — every remaining hit must be busybox or a historical `docs/superpowers/` file. Report the list.

- [ ] **Step 5: Verify**

Run: `python -m mkdocs build --strict`, then `go test ./... -count=1 -timeout 60m`.

- [ ] **Step 6: Commit.**
