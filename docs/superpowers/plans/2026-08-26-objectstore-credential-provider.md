# Object-Store Credential Provider Seam Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the object-store client take a *credentials provider* instead of a fixed key pair, and let the Kubernetes sidecar read its credentials from a file that can be refreshed — so that short-lived or rotated credentials become possible at all.

**What this does NOT do.** The operator still creates the sidecar's Secret, still puts it in the job Pod's namespace, and still names it in the agent's config. This plan changes how that Secret reaches the sidecar, not whether one is needed. Removing it is spec §5.2 (the agent projects a credential the controller supplies) or §5.4 (the cloud issues a short-lived one), and **neither is built here** — §5.2 costs the agent write access to Secrets in the job namespace, §5.4 costs portability, and the spec deliberately leaves that choice open. The seam's whole point is that it is not wasted whichever way that decision goes.

**Architecture:** `minio-go` already accepts a `*credentials.Credentials`, which re-fetches when its provider reports expiry. Today the code hardcodes `credentials.NewStaticV4`, the one provider that can never refresh. This adds an optional provider to `S3Config`, teaches the sidecar to select one, and mounts the sidecar's Secret as a volume so the kubelet can update it. Static environment credentials stay last in precedence, so no existing deployment changes behaviour.

**Tech Stack:** Go, `github.com/minio/minio-go/v7 v7.2.0` (already pinned; ships `static.go`, `file_aws_credentials.go`, `sts_web_identity.go`, `assume_role.go`, `chain.go`), Kubernetes `corev1`.

**Spec:** `docs/superpowers/specs/2026-08-26-sidecar-credential-delivery-design.md` — §5.5 is what this plan implements.

## Global Constraints

- **No existing deployment may change behaviour.** Static `UNIFIED_S3_KEY`/`UNIFIED_S3_SECRET` stays the last-resort provider and keeps working exactly as it does today. This is the property that makes the change safe to ship ahead of any decision between §5.2 and §5.4.
- **Do not choose between §5.2 and §5.4.** This plan builds the seam only. Do not add agent-side Secret creation, do not widen the agent's RBAC, and do not wire an STS endpoint.
- **The controller and the standard agent must not need changes.** Both construct `objectstore.S3Config` directly with key and secret. If either has to change, the seam is in the wrong place — stop and say so.
- **`envFrom` stays the default** for the sidecar. The volume path is opt-in for this change. Flipping the default is a separate decision with its own migration note.
- A Secret consumed via `envFrom` is snapshotted at container creation and never updates. That is the reason the file path exists; say so in comments rather than leaving the next reader to rediscover it.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/objectstore/s3.go` | `S3Config` gains an optional `Creds`; `NewS3ObjectStore` uses it or falls back to static |
| `internal/objectstore/credfile.go` | **new** — a `credentials.Provider` that reads a credentials file and re-reads when it changes |
| `internal/objectstore/credfile_test.go` | **new** |
| `internal/objectstore/env.go` | `S3ConfigFromEnv` selects a provider; the required-field check becomes provider-dependent |
| `internal/objectstore/env_test.go` | provider selection and precedence |
| `internal/k8sagent/podbuilder.go` | mount the Secret as an optional volume and point the sidecar at the file, when opted in |
| `internal/k8sagent/config.go` | the opt-in field |
| `docs/operator-manual/kubernetes-integration.md` | how to use the file path, and why it exists |

---

### Task 1: The provider seam

Pure Go, no Kubernetes. Nothing changes behaviour until Task 2 opts in.

**Files:**
- Modify: `internal/objectstore/s3.go:15-39`
- Create: `internal/objectstore/credfile.go`, `internal/objectstore/credfile_test.go`
- Modify: `internal/objectstore/env.go`, and its test file

**Interfaces:**
- Consumes: nothing.
- Produces: `S3Config.Creds *credentials.Credentials`, and `objectstore.NewFileCredentials(path string) *credentials.Credentials`. Task 2 consumes the env-variable names, not these directly.

- [ ] **Step 1: Add the optional provider to `S3Config`**

In `internal/objectstore/s3.go`:

```go
type S3Config struct {
	Endpoint        string // e.g. "s3.amazonaws.com" or "localhost:9000" for MinIO
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	UseSSL          bool
	Region          string

	// Creds, when non-nil, supplies credentials instead of AccessKeyID and
	// SecretAccessKey. minio-go re-fetches from the provider when it reports
	// the credential expired and re-signs per request, so a provider that can
	// refresh makes short-lived credentials work end to end — including across
	// a transfer that outlives the credential it started with.
	//
	// A static key pair is just one provider. It is the only one that can
	// never refresh, which is why it is the fallback rather than the shape.
	Creds *credentials.Credentials
}
```

and in `NewS3ObjectStore`:

```go
	creds := cfg.Creds
	if creds == nil {
		creds = credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, "")
	}
	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  creds,
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
```

The controller (`cmd/controller/main.go:322`) and the standard agent (`cmd/unified-cd-agent/main.go:217`) construct `S3Config` without `Creds` and are unaffected. Confirm that by building, not by assuming.

- [ ] **Step 2: Write the failing tests for the file provider**

Create `internal/objectstore/credfile_test.go`. The behaviour that matters is that the provider **re-reads** — a provider that caches forever is the bug this seam exists to remove.

```go
package objectstore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeCredFile(t *testing.T, dir, key, secret string) string {
	t.Helper()
	p := filepath.Join(dir, "creds")
	body := "UNIFIED_S3_KEY=" + key + "\nUNIFIED_S3_SECRET=" + secret + "\n"
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

func TestFileCredentials_ReadsTheFile(t *testing.T) {
	p := writeCredFile(t, t.TempDir(), "AKIA1", "s3cr3t1")
	v, err := NewFileCredentials(p).Get()
	require.NoError(t, err)
	assert.Equal(t, "AKIA1", v.AccessKeyID)
	assert.Equal(t, "s3cr3t1", v.SecretAccessKey)
}

// The point of the whole seam: a rewritten file is picked up. Without this,
// a rotated or refreshed credential never reaches the client and the provider
// is no better than a static key pair.
func TestFileCredentials_PicksUpARewrite(t *testing.T) {
	dir := t.TempDir()
	p := writeCredFile(t, dir, "AKIA1", "s3cr3t1")
	c := NewFileCredentials(p)

	first, err := c.Get()
	require.NoError(t, err)
	require.Equal(t, "AKIA1", first.AccessKeyID)

	writeCredFile(t, dir, "AKIA2", "s3cr3t2")

	second, err := c.Get()
	require.NoError(t, err)
	assert.Equal(t, "AKIA2", second.AccessKeyID,
		"a rewritten credential file must be re-read; the kubelet updates a mounted Secret in place")
}

// A missing or unreadable file is an error, not empty credentials. Empty
// credentials produce a signature failure at transfer time, which is a much
// worse message than "cannot read the credential file".
func TestFileCredentials_MissingFileErrors(t *testing.T) {
	_, err := NewFileCredentials(filepath.Join(t.TempDir(), "absent")).Get()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absent")
}

func TestFileCredentials_IncompleteFileErrors(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "creds")
	require.NoError(t, os.WriteFile(p, []byte("UNIFIED_S3_KEY=AKIA1\n"), 0o600))
	_, err := NewFileCredentials(p).Get()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "UNIFIED_S3_SECRET")
}
```

- [ ] **Step 3: Run them to verify they fail**

Run: `go test ./internal/objectstore/ -run FileCredentials -count=1`
Expected: FAIL — `undefined: NewFileCredentials`.

- [ ] **Step 4: Decide the mechanism, then implement**

**Before writing it, check whether `minio-go`'s own `credentials.FileAWSCredentials` already does this.** It reads the AWS shared-credentials INI format. Two things decide it:

1. Does it **re-read on expiry**, or cache after the first read? Read the vendored source at `$(go env GOMODCACHE)/github.com/minio/minio-go/v7@v7.2.0/pkg/credentials/file_aws_credentials.go`.
2. Is the AWS INI format the right thing for a file the agent may later write?

If it re-reads and the format is acceptable, **use it** and delete the tests that only make sense for a bespoke provider — do not write a second implementation of something the dependency ships. Say which you chose and why.

If you write your own, `credentials.Provider` is a two-method interface — `Retrieve()` and `IsExpired()`. The shape that works with a kubelet-updated mount is to stat the file and report expired when its modification time or size has changed since the last read:

```go
// NewFileCredentials returns credentials read from path, re-read whenever the
// file changes.
//
// The file is the seam that makes refreshable credentials possible. A Secret
// consumed through envFrom is snapshotted into the container's environment when
// the container is created and never updates, so rewriting it leaves a running
// Pod holding the old value. A Secret mounted as a volume IS updated by the
// kubelet, which is why this provider watches the file rather than reading it
// once at startup.
func NewFileCredentials(path string) *credentials.Credentials {
	return credentials.New(&fileProvider{path: path})
}
```

Guard the read and the cached value with a mutex: `minio-go` calls the provider from whichever goroutine is signing a request, and the sidecar can have an upload and a download in flight.

- [ ] **Step 5: Run them to verify they pass**

Run: `go test ./internal/objectstore/ -run FileCredentials -count=1`
Expected: PASS.

- [ ] **Step 6: Teach `S3ConfigFromEnv` to select a provider**

`internal/objectstore/env.go` currently requires `UNIFIED_S3_KEY` and `UNIFIED_S3_SECRET` unconditionally. That has to become provider-dependent, or the file path can never be used.

Precedence, most specific first — and **static stays last**, which is what keeps existing deployments working:

1. `UNIFIED_S3_CREDENTIAL_FILE` set → `NewFileCredentials(path)`.
2. Otherwise → static from `UNIFIED_S3_KEY` / `UNIFIED_S3_SECRET`, exactly as today.

`UNIFIED_S3_ENDPOINT` and `UNIFIED_S3_BUCKET` stay required in every case — they are not credentials.

Leave a comment naming `UNIFIED_S3_WEB_IDENTITY_TOKEN_FILE` as the slot a future STS provider takes, and pointing at spec §5.4. Do **not** implement it here.

The error when nothing is configured must name **both** ways to configure it. An operator who mounted a file and got "missing `UNIFIED_S3_KEY`" would reasonably conclude the file path does not exist.

- [ ] **Step 7: Test the precedence**

Add to `internal/objectstore/env_test.go`:

```go
// The file wins when both are set: an operator who mounted a credential file
// meant to use it, and silently preferring the stale env pair would be the
// hardest possible thing to debug.
func TestS3ConfigFromEnv_FileWinsOverStatic(t *testing.T) {}

// Existing deployments set only the key pair. This is the test that says the
// change is safe to ship.
func TestS3ConfigFromEnv_StaticStillWorksAlone(t *testing.T) {}

// Neither configured: the error must name both routes.
func TestS3ConfigFromEnv_ErrorNamesBothRoutes(t *testing.T) {}
```

- [ ] **Step 8: Verify nothing else moved**

Run: `go build ./... && go vet ./... && go test ./internal/objectstore/ ./internal/artifact/ -count=1`

Then confirm by reading that `cmd/controller/main.go:322` and `cmd/unified-cd-agent/main.go:217` are untouched. If either needed a change, stop — the seam is in the wrong place.

- [ ] **Step 9: Commit**

```bash
git add internal/objectstore/
git commit -m "feat(objectstore): take a credentials provider, and add a refreshable file provider"
```

---

### Task 2: Mount the Secret as a volume, opt-in

**Files:**
- Modify: `internal/k8sagent/config.go` (the opt-in field), `internal/k8sagent/podbuilder.go:44-66`
- Test: `internal/k8sagent/podbuilder_test.go`
- Modify: `docs/operator-manual/kubernetes-integration.md`

**Interfaces:**
- Consumes: `UNIFIED_S3_CREDENTIAL_FILE` from Task 1.
- Produces: nothing later tasks depend on.

- [ ] **Step 1: Write the failing test**

The three properties that matter, in `internal/k8sagent/podbuilder_test.go`:

```go
// Default is unchanged: envFrom, no volume. Existing deployments must see
// byte-identical Pod specs.
func TestBuildPod_SidecarSecretDefaultsToEnvFrom(t *testing.T) {}

// Opted in: a volume, a mount, UNIFIED_S3_CREDENTIAL_FILE pointing at it, and
// NO envFrom for that Secret — carrying both would leave the snapshotted env
// values as a silent fallback that masks a broken mount.
func TestBuildPod_SidecarSecretAsVolume(t *testing.T) {}

// The volume is optional, so a missing or misplaced Secret does not make the
// kubelet fail the whole Pod with CreateContainerConfigError. That failure
// breaks every job, not just artifacts, and reports itself as a five-minute
// timeout with the real cause never surfaced.
func TestBuildPod_SidecarSecretVolumeIsOptional(t *testing.T) {}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/k8sagent/ -run SidecarSecret -count=1`
Expected: FAIL — the opt-in field does not exist.

- [ ] **Step 3: Add the opt-in**

In `internal/k8sagent/config.go`, beside `SidecarS3SecretName`:

```go
	// SidecarS3SecretMode selects how the Secret named by SidecarS3SecretName
	// reaches the sidecar.
	//
	//   "env"  (default) envFrom.secretRef, as today.
	//   "file" mounted as a volume, with UNIFIED_S3_CREDENTIAL_FILE pointing at it.
	//
	// "file" is what makes a rotated or short-lived credential possible: an
	// envFrom Secret is snapshotted into the container's environment at creation
	// and never updates, while a mounted Secret is updated by the kubelet.
	// It also lets the volume be optional, so a missing Secret degrades instead
	// of failing the whole Pod. Env: UNIFIED_K8S_SIDECAR_S3_SECRET_MODE.
	SidecarS3SecretMode string `yaml:"sidecarS3SecretMode,omitempty"`
```

Validate it to `env` or `file`, defaulting to `env`. Follow whatever `Config.Validate` does for the other enum-ish fields — an unrecognised value must be an error at startup, not a silent fallback to the default.

- [ ] **Step 4: Build the volume in `podbuilder.go`**

`SidecarSpec` gains the mode. In file mode: a `corev1.Volume` with `Secret` source, `Optional: ptr.To(true)`, `DefaultMode: ptr.To(int32(0o400))`; a read-only `VolumeMount`; and a `UNIFIED_S3_CREDENTIAL_FILE` env entry pointing at the mounted path. **No `EnvFrom` for that Secret in this mode.**

`0400` and the deliberate absence of `envFrom` both mirror the controller's KEK mount, which is the established pattern here — say so in a comment.

**Check `internal/k8sagent/pool.go`.** Its reuse key is built from `sidecar.Image` and `sidecar.S3SecretName`. A pod built in `env` mode must not be reused for a claim wanting `file` mode; add the mode to that key. Getting this wrong means a pooled pod silently serves the wrong credential shape.

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/k8sagent/ -count=1`
Expected: PASS, with the existing pod-builder tests unchanged — that is the evidence the default did not move.

- [ ] **Step 6: Document it**

In `docs/operator-manual/kubernetes-integration.md`, next to `sidecarS3SecretName`: what `file` mode does, that it is the mode a rotated credential needs, that the volume is optional so a missing Secret degrades rather than breaking every job, and that `env` remains the default.

Do **not** claim it enables short-lived cloud credentials — that is §5.4 and is not built. Say the seam exists and name the spec.

Run: `python -m mkdocs build --strict`

- [ ] **Step 7: Commit**

```bash
git add internal/k8sagent/ docs/
git commit -m "feat(k8sagent): mount the sidecar's S3 Secret as an optional volume, opt-in"
```

---

## Self-review notes

**Spec coverage.** §5.5's table has three rows; this plan builds two of them — the file provider and the static fallback. The STS row is deliberately left as a named slot in Task 1 Step 6, because implementing it needs an STS endpoint and a per-cloud decision that §9 has not settled.

**What this plan does not do, on purpose.** It does not create Secrets, widen RBAC, change the default, or touch the controller and the standard agent. Every one of those is a decision the spec says should be made deliberately, and none is needed to make refreshable credentials possible.

**The one thing to stop on.** If Task 1 turns out to need changes in `cmd/controller/main.go` or `cmd/unified-cd-agent/main.go`, the seam is in the wrong place — an optional field on `S3Config` should be invisible to callers that do not set it. Stop and report rather than editing those files.
