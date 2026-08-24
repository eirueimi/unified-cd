# Argo-Style Manifest Distribution Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop committing generated Kubernetes bundles, build them at release time with the release tag injected, keep credentials out of the production bundle, and make the controller report which capabilities switched themselves off.

**Architecture:** The kustomize sources stay in the repository; the three generated bundles leave it. Both Secrets move out of `manifests/base/controller/` and into the `install` overlay, so the `core-install` overlay inherits none and an operator supplies them with `kubectl create secret`. A release job injects the release tag and attaches the built bundles to the GitHub Release. Separately, the controller gains a startup summary naming every optional capability and what is lost when one is off.

**Tech Stack:** Go 1.x with `log/slog` and testify, kustomize (via `kubectl kustomize` and the standalone `kustomize` CLI), GitHub Actions.

**Spec:** [`docs/superpowers/specs/2026-08-24-argo-style-manifest-distribution-design.md`](../specs/2026-08-24-argo-style-manifest-distribution-design.md)

## Global Constraints

- Secret resource names never change: `unified-cd-controller` and `unified-cd-controller-kek`. Consumption mechanisms never change: `envFrom` for the first, a volume mount with `defaultMode: 0400` for the second.
- The KEK must never enter the Secret referenced by `envFrom` — that Secret's every key is projected into the process environment, which is exactly what the file mount avoids.
- `install.yaml`, the development bundle, keeps its Secrets and their development-default values.
- Startup never fails because a capability is off. The goal is visibility, not prohibition.
- Third-party images (`postgres:16-alpine`, `dxflrs/garage:v2.3.0`) are already pinned and are not touched.
- Commit messages follow Conventional Commits.
- `go build ./...` and `go test ./... -short -count=1` pass at the end of every task.

---

## Task 1: Controller startup summary

Independent of the manifest work. Ships first so the visibility exists before the flow that needs it.

**Files:**
- Create: `cmd/controller/startup_summary.go`
- Create: `cmd/controller/startup_summary_test.go`
- Modify: `internal/config/keysource.go:89-97` (add one field, set it in one branch)
- Modify: `internal/config/keysource_test.go` (assert the new field)
- Modify: `cmd/controller/main.go` — object-store branches at `:318-338`, the OIDC block ending near `:380`, and the call site immediately before `slog.Info("controller listening", "addr", *addr)` at `:483`

**Interfaces:**
- Consumes: nothing.
- Produces: `summarizeStartup(startupInputs) []capabilityState` and `logStartupSummary(startupInputs)` in package `main`; `config.Resolved.Ephemeral bool`. No later task depends on these.

- [ ] **Step 1: Write the failing test for the pure summary function**

Create `cmd/controller/startup_summary_test.go`:

```go
package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func lost(t *testing.T, caps []capabilityState, name string) string {
	t.Helper()
	for _, c := range caps {
		if c.Name == name {
			return c.Lost
		}
	}
	t.Fatalf("capability %q not present in summary", name)
	return ""
}

func TestSummarizeStartupReportsNothingLostWhenFullyConfigured(t *testing.T) {
	caps := summarizeStartup(startupInputs{
		ObjectStore: "s3",
		KeyDesc:     "key file /etc/unified-cd/kek",
		OIDC:        true,
		WebUI:       true,
		LogTrimDays: 30,
	})

	for _, c := range caps {
		assert.Empty(t, c.Lost, "capability %q should not be degraded", c.Name)
	}
}

func TestSummarizeStartupNamesWhatEachDegradedCapabilityCosts(t *testing.T) {
	caps := summarizeStartup(startupInputs{
		ObjectStore:  "none",
		KeyDesc:      "ephemeral development key",
		KeyEphemeral: true,
		OIDC:         false,
		WebUI:        false,
		LogTrimDays:  0,
	})

	assert.Contains(t, lost(t, caps, "objectStore"), "log archival and artifacts")
	assert.Contains(t, lost(t, caps, "objectStore"), "UNIFIED_S3_ENDPOINT")
	assert.Contains(t, lost(t, caps, "secretKey"), "unreadable after a restart")
	assert.Contains(t, lost(t, caps, "secretKey"), "UNIFIED_CONTROLLER_KEY_FILE")
	assert.Contains(t, lost(t, caps, "sso"), "device flow")
	assert.Contains(t, lost(t, caps, "webUI"), "404")
}

// The controller logs "log trim enabled" whenever the setting is positive, but
// RunLogTrim returns immediately without an object store
// (internal/controller/log_trim.go:29-31), so the sweeper never runs. The
// summary is where that contradiction has to surface.
func TestSummarizeStartupFlagsLogTrimAsInertWithoutAnObjectStore(t *testing.T) {
	caps := summarizeStartup(startupInputs{
		ObjectStore: "none",
		KeyDesc:     "key file /etc/unified-cd/kek",
		OIDC:        true,
		WebUI:       true,
		LogTrimDays: 30,
	})

	assert.Equal(t, "inert", func() string {
		for _, c := range caps {
			if c.Name == "logTrim" {
				return c.State
			}
		}
		return ""
	}())
	assert.Contains(t, lost(t, caps, "logTrim"), "never runs without an object store")
}

func TestSummarizeStartupOmitsLogTrimWhenItWouldActuallyRun(t *testing.T) {
	caps := summarizeStartup(startupInputs{
		ObjectStore: "s3",
		KeyDesc:     "key file /etc/unified-cd/kek",
		OIDC:        true,
		WebUI:       true,
		LogTrimDays: 30,
	})

	for _, c := range caps {
		assert.NotEqual(t, "logTrim", c.Name, "logTrim should only appear when it is inert")
	}
}

func TestLogStartupSummaryEmitsOneInfoAndOneWarnPerDegradedCapability(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	logStartupSummary(startupInputs{
		ObjectStore:  "none",
		KeyDesc:      "ephemeral development key",
		KeyEphemeral: true,
		OIDC:         false,
		WebUI:        true,
		LogTrimDays:  30,
	})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	require.Len(t, lines, 5, "one summary record plus one per degraded capability")

	assert.Contains(t, lines[0], `"msg":"startup summary"`)
	assert.Contains(t, lines[0], `"objectStore":"none"`)
	assert.Contains(t, lines[0], `"webUI":"served"`)

	warned := strings.Join(lines[1:], "\n")
	assert.Contains(t, warned, `"capability":"objectStore"`)
	assert.Contains(t, warned, `"capability":"secretKey"`)
	assert.Contains(t, warned, `"capability":"sso"`)
	assert.Contains(t, warned, `"capability":"logTrim"`)
	assert.NotContains(t, warned, `"capability":"webUI"`)
}
```

- [ ] **Step 2: Run the tests and watch them fail**

Run: `go test ./cmd/controller/ -run 'Startup' -count=1`

Expected: FAIL to compile — `undefined: capabilityState`, `undefined: startupInputs`, `undefined: summarizeStartup`, `undefined: logStartupSummary`.

- [ ] **Step 3: Write the implementation**

Create `cmd/controller/startup_summary.go`:

```go
package main

import "log/slog"

// capabilityState is one optional capability's resolved state at startup,
// together with what an operator loses when it is off.
//
// The controller initializes each subsystem with its own log line, so a
// capability that quietly switched itself off is visible only as the absence
// of a line among twenty. Worse, some settings report themselves as enabled
// while being inert — see the logTrim case below. The summary exists so a
// misconfiguration is legible in one record instead of inferred from a feature
// that never fires.
type capabilityState struct {
	// Name is the log attribute key, e.g. "objectStore".
	Name string
	// State is the resolved value, e.g. "s3", "local", "none".
	State string
	// Lost is empty when the capability is fully available. Otherwise it names
	// what does not work and the setting that restores it.
	Lost string
}

// startupInputs is the resolved configuration the summary reports on.
type startupInputs struct {
	// ObjectStore is "s3", "local", or "none", matching the selection order in
	// main (S3, then UNIFIED_DATA_DIR, then nothing).
	ObjectStore string
	// KeyDesc is config.Resolved.Description — the key's origin.
	KeyDesc string
	// KeyEphemeral is config.Resolved.Ephemeral: the key does not survive a
	// restart, so neither do the secrets encrypted with it.
	KeyEphemeral bool
	OIDC         bool
	WebUI        bool
	LogTrimDays  int
}

func summarizeStartup(in startupInputs) []capabilityState {
	caps := []capabilityState{
		{Name: "objectStore", State: in.ObjectStore},
		{Name: "secretKey", State: in.KeyDesc},
		{Name: "sso", State: onOff(in.OIDC, "oidc")},
		{Name: "webUI", State: onOff(in.WebUI, "served")},
	}

	if in.ObjectStore == "none" {
		caps[0].Lost = "log archival and artifacts are disabled; set UNIFIED_S3_ENDPOINT and UNIFIED_S3_BUCKET, or UNIFIED_DATA_DIR for development"
	}
	if in.KeyEphemeral {
		caps[1].Lost = "the encryption key is ephemeral: every stored secret is unreadable after a restart; set UNIFIED_CONTROLLER_KEY_FILE or UNIFIED_KMS_URI"
	}
	if !in.OIDC {
		caps[2].Lost = "SSO and the CLI device flow are unavailable; set UNIFIED_OIDC_ISSUER"
	}
	if !in.WebUI {
		caps[3].Lost = "/ui/* returns 404; set UNIFIED_WEB_DIR"
	}

	// Only reported when it contradicts itself. RunLogTrim returns immediately
	// with a nil object store, so no logs are lost — but startup otherwise
	// prints "log trim enabled" for a sweeper that will never run.
	if in.LogTrimDays > 0 && in.ObjectStore == "none" {
		caps = append(caps, capabilityState{
			Name:  "logTrim",
			State: "inert",
			Lost:  "log trim is configured but never runs without an object store; configure one, or unset UNIFIED_LOG_TRIM_DAYS",
		})
	}

	return caps
}

func onOff(on bool, whenOn string) string {
	if on {
		return whenOn
	}
	return "off"
}

// logStartupSummary emits one info record carrying every capability's state,
// then one warn record per degraded capability.
func logStartupSummary(in startupInputs) {
	caps := summarizeStartup(in)

	attrs := make([]any, 0, len(caps)*2)
	for _, c := range caps {
		attrs = append(attrs, c.Name, c.State)
	}
	slog.Info("startup summary", attrs...)

	for _, c := range caps {
		if c.Lost != "" {
			slog.Warn("degraded capability", "capability", c.Name, "state", c.State, "impact", c.Lost)
		}
	}
}
```

- [ ] **Step 4: Run the tests and watch them pass**

Run: `go test ./cmd/controller/ -run 'Startup' -count=1 -v`

Expected: PASS, all five tests.

- [ ] **Step 5: Write the failing test for the new keysource field**

`internal/config/keysource_test.go` already asserts on `Description` for the ephemeral branch (search for `"ephemeral"`). Add to that same test function, immediately after the existing `Description` assertion:

```go
	assert.True(t, got.Ephemeral, "the development key does not survive a restart")
```

And to the key-file test (the one asserting `assert.Contains(t, got.Description, "key file")`), add:

```go
	assert.False(t, got.Ephemeral, "a key read from a file survives a restart")
```

- [ ] **Step 6: Run it and watch it fail**

Run: `go test ./internal/config/ -run KeySource -count=1`

Expected: FAIL to compile — `got.Ephemeral undefined`.

- [ ] **Step 7: Add the field**

In `internal/config/keysource.go`, add to the `Resolved` struct, directly beneath the `Description` field and its comment:

```go
	// Ephemeral is true when the key is generated per-process and lost on
	// restart, taking every secret encrypted with it. Only development mode
	// resolves this way; the startup summary warns on it.
	Ephemeral bool
```

Then set it in the ephemeral branch — the `Resolved` literal at `keysource.go:97` whose `Description` is `"ephemeral development key"`:

```go
			Ephemeral:   true,
```

Leave every other `Resolved` literal alone; `false` is the correct zero value for a key file and for Vault transit.

- [ ] **Step 8: Run it and watch it pass**

Run: `go test ./internal/config/ -count=1`

Expected: PASS.

- [ ] **Step 9: Wire the summary into main**

Three edits in `cmd/controller/main.go`.

First, capture the object-store outcome. Change the declaration at the top of the selection block from

```go
	var obj objectstore.ObjectStore
```

to

```go
	var obj objectstore.ObjectStore
	objectStoreState := "none"
```

and add one assignment in each of the two configured branches — `objectStoreState = "s3"` immediately after `obj = s3`, and `objectStoreState = "local"` immediately after `obj = objectstore.NewLocalObjectStore(*dataDir)`. Leave the existing log lines exactly as they are; the summary supplements them, it does not replace them.

Second, capture whether OIDC was configured. Declare `oidcConfigured := false` immediately before the `if` that guards the OIDC block, and set `oidcConfigured = true` inside it, next to the existing `slog.Info("OIDC configured", ...)` call.

Third, call the summary. Immediately before

```go
	slog.Info("controller listening", "addr", *addr)
```

insert:

```go
	logStartupSummary(startupInputs{
		ObjectStore:  objectStoreState,
		KeyDesc:      resolved.Description,
		KeyEphemeral: resolved.Ephemeral,
		OIDC:         oidcConfigured,
		WebUI:        *webDir != "" || *uiProxyTarget != "",
		LogTrimDays:  *logTrimDays,
	})
```

`webDir` and `uiProxyTarget` are the flag variables declared at `main.go:208-209`; either one being set means `/ui/*` is served.

- [ ] **Step 10: Verify the whole package still builds and passes**

Run: `go build ./... && go test ./cmd/controller/ ./internal/config/ -count=1`

Expected: PASS.

- [ ] **Step 11: Confirm the summary actually appears at runtime**

The unit tests prove the records; this proves the wiring. Run the controller with nothing configured and no database — it will exit on the database connection, but the flag parsing and summary wiring compile-check as a unit:

Run: `go vet ./cmd/controller/`

Expected: clean. If a live database is available, start the controller with `UNIFIED_DEV_MODE=1` and no `UNIFIED_S3_*`, and confirm a `startup summary` record and `degraded capability` records appear immediately before `controller listening`. If no database is available, say so in the report rather than claiming the runtime check.

- [ ] **Step 12: Commit**

```bash
git add cmd/controller/startup_summary.go cmd/controller/startup_summary_test.go cmd/controller/main.go internal/config/keysource.go internal/config/keysource_test.go
git commit -m "feat(controller): report degraded capabilities in a startup summary

Each subsystem logged its own state, so a capability that switched itself
off was visible only as a missing line among twenty. Log trim was worse: it
printed \"log trim enabled\" while RunLogTrim returns immediately without an
object store, so the sweeper never ran.

One info record now carries every capability's resolved state, and each
degraded one gets a warn naming what is unavailable and the setting that
restores it."
```

---

## Task 2: Move both Secrets out of the base into the install overlay

**Files:**
- Modify: `manifests/base/controller/kustomization.yaml` (drop two resources)
- Delete: `manifests/base/controller/secret.yaml`, `manifests/base/controller/kek-secret.yaml`
- Create: `manifests/install/secret.yaml`
- Delete: `manifests/install/secret-patch.yaml`, `manifests/install/kek-secret-patch.yaml`
- Modify: `manifests/install/kustomization.yaml`
- Modify: `manifests/README.md:9`, `:20-22`, `:30-38`, `:41-65`, `:89-131`
- Create: `docs/operator-manual/migrations/kubernetes-secrets-out-of-the-bundle.md`
- Modify: `mkdocs.yml` (nav entry for the new migration guide)
- Regenerate: `manifests/core-install.yaml`, `manifests/install.yaml`, `manifests/agent-only.yaml`

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces: a `core-install` overlay with no Secret resources, which Task 3 generates from and Task 4 publishes.

- [ ] **Step 1: Move the Secrets out of the base**

The base currently owns both Secrets and the `install` overlay patches them — one with development values, one with `$patch: delete`. Inverting that is what makes `core-install` inherit nothing.

Remove these two lines from `manifests/base/controller/kustomization.yaml`'s `resources:` list:

```yaml
  - secret.yaml
  - kek-secret.yaml
```

```bash
git rm manifests/base/controller/secret.yaml manifests/base/controller/kek-secret.yaml
```

- [ ] **Step 2: Turn the install patch into a resource**

`manifests/install/secret-patch.yaml` is a strategic-merge patch over a base resource that no longer exists. Its content becomes a plain resource.

Create `manifests/install/secret.yaml` with the full Secret: `apiVersion: v1`, `kind: Secret`, `metadata.name: unified-cd-controller`, `metadata.namespace: unified-cd`, `type: Opaque`, the `unified-cd.io/kek-provenance` annotation copied verbatim from `secret-patch.yaml`, and the six development-default `stringData` values copied verbatim — including the comment block explaining why no fixed KEK is baked into this bundle.

Then:

```bash
git rm manifests/install/secret-patch.yaml manifests/install/kek-secret-patch.yaml
```

`kek-secret-patch.yaml` deleted a base resource that no longer exists, so it has nothing left to do. The KEK Secret for this bundle is still created at install time by `kek-job.yaml`, which is unchanged.

In `manifests/install/kustomization.yaml`, add `secret.yaml` to `resources:` and remove `secret-patch.yaml` and `kek-secret-patch.yaml` from `patches:`. Leave `agent-config-patch.yaml` alone.

- [ ] **Step 3: Regenerate and inspect**

```bash
kubectl kustomize manifests/core-install > manifests/core-install.yaml
kubectl kustomize manifests/install > manifests/install.yaml
kubectl kustomize manifests/agent-only > manifests/agent-only.yaml
```

Then check the invariants that matter:

```bash
grep -c "^kind: Secret" manifests/core-install.yaml   # expect 0
grep -c "^kind: Secret" manifests/install.yaml        # expect the same count as before this task
grep -c "REPLACE_WITH" manifests/core-install.yaml    # expect 0
grep -c "kek-provenance" manifests/install.yaml       # expect 1
```

Expected: `core-install.yaml` has no Secret and no placeholder; `install.yaml` still carries its development Secret with the provenance annotation. If `install.yaml` changed in any way other than Secret ordering, find out why before continuing — its content is supposed to be equivalent to what the patch produced.

- [ ] **Step 4: Confirm the Deployment still references the Secrets it no longer ships**

```bash
grep -n -A2 "secretRef" manifests/core-install.yaml
grep -n -B2 -A6 "controller-kek" manifests/core-install.yaml
```

Expected: `envFrom.secretRef.name: unified-cd-controller` and the volume with `secretName: unified-cd-controller-kek` are both still present. That is the point — the bundle references Secrets the operator creates.

- [ ] **Step 5: Rewrite the manifests README**

`manifests/README.md` tells operators to edit placeholders in five places (`:9`, `:20-22`, `:30-38`, and the Vault section at `:41-65` and SSO section at `:89-131`). Replace the editing instructions with the creation flow. The canonical block, which the README and the migration guide both use verbatim:

```bash
kubectl create namespace unified-cd

kubectl create secret generic unified-cd-controller -n unified-cd \
  --from-literal=UNIFIED_DB_DSN='postgres://...' \
  --from-literal=UNIFIED_TOKEN='...' \
  --from-literal=UNIFIED_S3_ENDPOINT='...' \
  --from-literal=UNIFIED_S3_BUCKET='...' \
  --from-literal=UNIFIED_S3_KEY='...' \
  --from-literal=UNIFIED_S3_SECRET='...'

unified-cli keygen --out ./kek
kubectl create secret generic unified-cd-controller-kek -n unified-cd \
  --from-file=kek=./kek
```

Directly beneath it, three points that are not optional:

1. The KEK uses `--from-file` deliberately: `keygen --out` already writes a file, and `--from-literal` would put the key into `argv` and the shell history. `--from-env-file` is offered for the other six for the same reason.
2. Omitting the four `UNIFIED_S3_*` keys does not stop the controller. The pod starts and object storage is simply absent; the log says `no object store configured — log archival disabled`, and log archival and artifacts are off. Quote that log line exactly so it is greppable.
3. Fill the KEK once and never re-apply a different value — every secret encrypted under the old key becomes permanently unreadable. This is the text that used to live in the `unified-cd.io/kek-warning` annotation, which disappears with the Secret.

Keep the Vault and SSO sections; they configure the same Secret, so rewrite them to add keys to the `kubectl create secret` command rather than to edit a file.

- [ ] **Step 6: Write the migration guide**

Create `docs/operator-manual/migrations/kubernetes-secrets-out-of-the-bundle.md`, following the shape of `agent-id-scoped-credentials.md` in the same directory — read it first and match its structure: a short statement of what changed, a before/after table, the exact commands, and the exact error strings.

It must say plainly that **an existing installation needs no action**: `kubectl apply -f` does not prune, the Secrets already in the cluster survive a bundle that no longer contains them, and the names are unchanged, so the Deployment's references still resolve.

For a fresh install, give the creation block from Step 5, and the error an operator sees if they skip it — the pod stays in `CreateContainerConfigError` with an event reading `secret "unified-cd-controller" not found`.

Add it to `mkdocs.yml`'s `nav` under `Operator Manual → Migrations`, beside the existing entry.

- [ ] **Step 7: Verify the site still builds**

Run: `python -m mkdocs build --strict`

Expected: PASS with zero warnings. A new page missing from `nav`, or a broken link in the new guide, fails here.

- [ ] **Step 8: Commit**

```bash
git add -A
git commit -m "fix(manifests): keep credentials out of the production bundle

core-install.yaml shipped two Secrets full of REPLACE_WITH_ placeholders and
told operators to edit them, which invites live credentials into git. Both
Secrets move into the install overlay, so the production bundle inherits
none and an operator supplies them with kubectl create secret.

The development bundle is unchanged in content: its patch becomes a plain
resource now that there is no base Secret to patch."
```

---

## Task 3: Generate the bundles in the release workflow

The bundles stay committed through this task, so the generated output can be diffed against them and proven identical apart from the image tags.

**Files:**
- Modify: `.github/workflows/release-docker.yml`
- Create: `scripts/build-manifests.sh`
- Modify: `Makefile` (point `manifests` at the script)

**Interfaces:**
- Consumes: the overlays as left by Task 2.
- Produces: `scripts/build-manifests.sh <output-dir> [image-tag]`, which Task 4 keeps as the only way bundles are built.

- [ ] **Step 1: Write the build script**

One script, used by both the Makefile and the release job, so local output and released output cannot diverge.

Create `scripts/build-manifests.sh`:

```bash
#!/usr/bin/env bash
# Builds the three install bundles from the kustomize overlays.
#
# Usage: scripts/build-manifests.sh <output-dir> [image-tag]
#
# With an image tag, the two first-party images are pinned to it — this is what
# the release workflow does, so a released bundle names the images that release
# published. Without one, the overlays' own values are used, which is what a
# local build wants. Third-party images (postgres, garage) are already pinned
# in the base and are never rewritten here.
set -euo pipefail

out_dir=${1:?usage: build-manifests.sh <output-dir> [image-tag]}
image_tag=${2:-}

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
mkdir -p "$out_dir"

overlays=(core-install install agent-only)

if [ -n "$image_tag" ]; then
  # kubectl embeds kustomize for `kubectl kustomize` but does not expose
  # `kustomize edit`, so the standalone CLI is required for this branch.
  command -v kustomize >/dev/null || {
    echo "kustomize CLI not found; it is required to pin an image tag" >&2
    exit 1
  }
  for overlay in "${overlays[@]}"; do
    (
      cd "$repo_root/manifests/$overlay"
      kustomize edit set image \
        "ghcr.io/eirueimi/unified-cd-controller=ghcr.io/eirueimi/unified-cd-controller:$image_tag" \
        "ghcr.io/eirueimi/unified-cd-k8s-agent=ghcr.io/eirueimi/unified-cd-k8s-agent:$image_tag"
    )
  done
fi

for overlay in "${overlays[@]}"; do
  kubectl kustomize "$repo_root/manifests/$overlay" > "$out_dir/$overlay.yaml"
  echo "wrote $out_dir/$overlay.yaml"
done
```

Make it executable: `git update-index --chmod=+x scripts/build-manifests.sh` after adding it (or `chmod +x` first on a filesystem that carries the bit).

- [ ] **Step 2: Prove the script reproduces the committed bundles**

This is the test for this task: with no tag argument, the script must produce byte-identical output to what is committed.

```bash
scripts/build-manifests.sh /tmp/mf-check
for f in core-install install agent-only; do
  diff -u "manifests/$f.yaml" "/tmp/mf-check/$f.yaml" && echo "$f identical"
done
```

Expected: all three report identical. Any difference means the script builds something other than what ships — resolve it before continuing.

- [ ] **Step 3: Prove tag injection works and touches only the two first-party images**

```bash
git stash list > /tmp/stash-before.txt
scripts/build-manifests.sh /tmp/mf-tagged v0.5.0
grep -n "image:" /tmp/mf-tagged/core-install.yaml
grep -n "image:" /tmp/mf-tagged/install.yaml
git checkout -- manifests/core-install/kustomization.yaml manifests/install/kustomization.yaml manifests/agent-only/kustomization.yaml
```

Expected: both `ghcr.io/eirueimi/unified-cd-*` images carry `:v0.5.0`; `postgres:16-alpine` and `dxflrs/garage:v2.3.0` are untouched. `kustomize edit` rewrites the overlay `kustomization.yaml` files in place, which is why they are restored afterwards — the release job runs on a throwaway checkout and does not care, but a local run must not leave the tag behind. Confirm `git status --short` is clean after the checkout.

- [ ] **Step 4: Point the Makefile at the script**

Replace the `manifests` target's three `kubectl kustomize` lines with:

```makefile
manifests:
	scripts/build-manifests.sh manifests
```

Keeping the output directory as `manifests/` for now means this task changes nothing about where bundles live; Task 4 moves it.

Run `make manifests` if `make` is available, or `scripts/build-manifests.sh manifests` if it is not, then `git status --short` — expected: clean, because the script reproduces what is committed.

- [ ] **Step 5: Add the release job**

`.github/workflows/release-docker.yml` has three jobs: `build` (line 12), `merge` (line 77, `needs: build`, applies the release tag to the images), and `verify` (line 136, `needs: [build, merge]`). Add a fourth after `verify`. A bundle attached before its images exist and are verified would advertise references that cannot be pulled, so this ordering is load-bearing, not cosmetic.

```yaml
  manifests:
    name: Publish install bundles
    # After verify, not merely after merge: the bundles name the images, so
    # they should not be published until those images are confirmed good.
    needs: [verify]
    runs-on: ubuntu-latest
    permissions:
      contents: write   # required to attach assets to the release
    steps:
      - uses: actions/checkout@v4
      - name: Install kustomize
        run: |
          curl -sL "https://raw.githubusercontent.com/kubernetes-sigs/kustomize/master/hack/install_kustomize.sh" | bash
          sudo mv kustomize /usr/local/bin/
      # The bundles name the images this release just published, so they are
      # built from the tag rather than from whatever the overlays carry.
      - name: Build bundles pinned to ${{ github.ref_name }}
        run: scripts/build-manifests.sh dist/manifests "${{ github.ref_name }}"
      - name: Attach to the release
        env:
          GH_TOKEN: ${{ github.token }}
        run: |
          gh release upload "${{ github.ref_name }}" \
            dist/manifests/core-install.yaml \
            dist/manifests/install.yaml \
            dist/manifests/agent-only.yaml \
            --clobber
```

Confirm the job ids still match what is above before committing — a `needs` naming a job that does not exist makes the whole workflow fail to parse.

- [ ] **Step 6: Validate the workflow parses**

```bash
python -c "import yaml; yaml.safe_load(open('.github/workflows/release-docker.yml')); print('yaml ok')"
```

Expected: `yaml ok`.

- [ ] **Step 7: Commit**

```bash
git add scripts/build-manifests.sh Makefile .github/workflows/release-docker.yml
git commit -m "ci(manifests): build the install bundles at release time

One script builds the bundles for both a local run and the release job, so
the two cannot diverge. The release job pins both first-party images to the
tag being released and attaches the three bundles to the GitHub Release.

The bundles stay committed for now; the next change removes them, once this
one has shown the generated output matches."
```

---

## Task 4: Delete the committed bundles and move the documented URLs

**Files:**
- Delete: `manifests/core-install.yaml`, `manifests/install.yaml`, `manifests/agent-only.yaml`
- Modify: `Makefile` (output to `dist/manifests`)
- Modify: `.gitignore`
- Modify: `manifests/README.md` (the apply instructions and the "Regenerating manifests" section)
- Modify: `docs/getting-started/installation.md:38-43`
- Modify: `docs/operator-manual/kubernetes-integration.md:195`
- Modify: `AGENTS.md` (the generated-artifacts rule, if it names the manifests)
- Modify: `docs/operator-manual/migrations/kubernetes-secrets-out-of-the-bundle.md` (add the URL change)

**Interfaces:**
- Consumes: `scripts/build-manifests.sh` from Task 3, proven to reproduce the committed output.
- Produces: nothing later depends on.

- [ ] **Step 1: Delete the bundles and redirect the build output**

```bash
git rm manifests/core-install.yaml manifests/install.yaml manifests/agent-only.yaml
```

Change the Makefile target to write elsewhere:

```makefile
manifests:
	scripts/build-manifests.sh dist/manifests
```

Append to `.gitignore`:

```
# Install bundles, built from manifests/*/ by scripts/build-manifests.sh and
# published as release assets. Nothing generated here is committed.
/dist/
```

- [ ] **Step 2: Confirm a local build still works and leaves the tree clean**

```bash
scripts/build-manifests.sh dist/manifests
ls dist/manifests
git status --short
```

Expected: three files in `dist/manifests`, and `git status --short` shows only the deletions and edits from this task — no untracked `dist/`.

- [ ] **Step 3: Move the documented URLs**

`docs/getting-started/installation.md:38-43` currently gives:

```
kubectl apply -f https://raw.githubusercontent.com/eirueimi/unified-cd/main/manifests/install.yaml
kubectl apply -f https://raw.githubusercontent.com/eirueimi/unified-cd/main/manifests/agent-only.yaml
```

Replace with the release-asset form, pinned as the primary and `latest` as the convenience:

```
# Pinned to a release
kubectl apply -f https://github.com/eirueimi/unified-cd/releases/download/v0.5.0/install.yaml

# Or always the newest release
kubectl apply -f https://github.com/eirueimi/unified-cd/releases/latest/download/install.yaml
```

Search the whole repository for any other `raw.githubusercontent` URL naming a manifest and replace it the same way:

```bash
grep -rn "raw.githubusercontent.com/eirueimi/unified-cd" --include=*.md --include=*.yaml --include=*.yml . | grep -v node_modules
```

Every hit must be resolved. `manifests/README.md`'s apply instructions become `kubectl apply -k manifests/install` for local development, and the release URL for everything else.

- [ ] **Step 4: Rewrite the "Regenerating manifests" section**

`manifests/README.md`'s section on regenerating (near `:166-173`) says the three bundles must be regenerated and committed together. That rule is gone — there is nothing committed to keep in sync. Replace it with what is now true: the overlays are the source, `scripts/build-manifests.sh dist/manifests` builds them locally for inspection, and the release workflow builds and publishes the versions operators actually use.

Check `AGENTS.md` for the same claim (`grep -n "manifests" AGENTS.md`) and update it if it names the bundles as generated artifacts that must be committed.

- [ ] **Step 5: Add the URL change to the migration guide**

The guide created in Task 2 covers the Secrets. Add a section for the distribution change: the old `raw.githubusercontent.com` URLs stop working, the replacement is the release asset URL, and automation pointing at the old ones will get a 404 rather than a wrong result. Give the before/after URLs in a table.

- [ ] **Step 6: Verify**

```bash
python -m mkdocs build --strict
grep -rn "raw.githubusercontent.com/eirueimi/unified-cd" --include=*.md . | grep -v node_modules
go build ./... && go test ./... -short -count=1
```

Expected: the site builds with zero warnings; the grep returns nothing; Go builds and tests pass.

- [ ] **Step 7: Commit**

```bash
git add -A
git commit -m "docs(manifests): publish install bundles as release assets

The committed bundles are gone; scripts/build-manifests.sh builds them into
a gitignored directory for local inspection, and the release workflow
publishes the ones operators use.

Install instructions now point at a release asset rather than main, so
following them no longer installs unreleased changes with :latest images.
Automation pointing at the old raw.githubusercontent URLs will 404 — the
migration guide gives the replacement."
```

---

## Final acceptance

Against spec section 9:

- [ ] A `v*` tag produces a release carrying all three bundles, each pinning both first-party images to that tag. (Verifiable only at the next release — confirm the workflow logic by reading, and check after the first tag.)
- [ ] The three bundles no longer exist in the repository, and the build script writes to a gitignored directory.
- [ ] The released `core-install.yaml` contains no Secret resource; `install.yaml` still contains its development-default Secrets.
- [ ] Applying `core-install.yaml` with the two Secrets pre-created brings the controller up; applying it without them leaves the pod in `CreateContainerConfigError` and nothing worse.
- [ ] A controller with no object store emits the summary record plus a warn naming log archival and artifacts; with `logTrimDays > 0` and no object store it additionally warns that the setting is inert.
- [ ] No documentation tells an operator to edit placeholders, and no `raw.githubusercontent` manifest URL remains.
- [ ] `go build ./...` and `go test ./... -short -count=1` pass.
