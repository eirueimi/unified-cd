# Controller-Brokered Store Credentials Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the `unified-artifact` sidecar obtain object-store credentials from the controller, authenticated by a projected ServiceAccount token the kubelet gives it — so an operator no longer creates an S3 Secret in every job namespace, and the agent never gains access to Secrets.

**Architecture:** The agent adds a projected SA token volume to the job Pod, mounted into the sidecar container only. The sidecar presents that token to a new controller endpoint and receives store credentials. The controller verifies the token with a `TokenReview` — the same call `agent_enrollment_kubernetes.go` already makes for agent enrollment — against a **different audience**. The credentials feed the provider seam so they refresh. **Artifact bytes still go direct to the store**; only credential acquisition passes through the controller.

**Tech Stack:** Go, `k8s.io/api/authentication/v1` `TokenReview`, `k8s.io/api/core/v1` projected volumes, `github.com/minio/minio-go/v7` credential providers.

**Spec:** `docs/superpowers/specs/2026-08-26-sidecar-credential-delivery-design.md` — §5.6 is what this plan implements. §5.5's provider seam is a prerequisite; if it has not landed, build it first from `2026-08-26-objectstore-credential-provider.md`.

## Global Constraints

- **The audience MUST differ from `KubernetesEnrollmentAudience` (`"unified-cd-agent-enrollment"`).** If a job Pod's token were accepted for enrollment, **any job could register itself as an agent**. This is the single detail that makes the design safe or unsafe. It gets its own test, and that test asserts the rejection in **both** directions.
- **The agent must not gain any new Kubernetes permission.** It adds a volume to a Pod spec it already writes. If any step needs an RBAC change, stop — the design has drifted into §5.2 and the whole reason for choosing §5.6 is gone.
- **The data path must not change.** The sidecar talks to the controller once, to get credentials. Artifact and cache bytes continue to go straight to the store.
- **The existing Secret path keeps working**, unchanged and as the default. This lands alongside it, not instead of it. Flipping the default is a separate decision with its own migration note.
- **The token is mounted into the sidecar container only**, never the job container — the same container boundary that keeps today's credentials away from user code.
- Do not implement scoped credentials in this plan. See Task 2 Step 4 for why, and for the shape that keeps them a later swap rather than a rewrite.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/controller/kubernetes_token.go` | **new** — the token-review-and-claims verification, extracted so two audiences can use it |
| `internal/controller/agent_enrollment_kubernetes.go` | uses the extracted verifier; behaviour unchanged |
| `internal/controller/api_store_credentials.go` | **new** — the broker endpoint |
| `internal/controller/server.go` | route registration |
| `internal/api/types.go` | the request and response types |
| `internal/k8sagent/podbuilder.go` | the projected token volume and its mount |
| `internal/k8sagent/config.go` | the opt-in field |
| `cmd/unified-sidecar/` | fetch credentials from the controller when configured |
| `docs/operator-manual/kubernetes-integration.md` | how to enable it, and what it replaces |

---

### Task 1: Extract the token verification

A refactor with **no behaviour change**, so the broker can reuse verification that is already correct rather than writing a second one that is subtly weaker.

**Files:**
- Create: `internal/controller/kubernetes_token.go`, `internal/controller/kubernetes_token_test.go`
- Modify: `internal/controller/agent_enrollment_kubernetes.go`

**Interfaces:**
- Produces: a verifier that takes an audience and a token and returns the bound-pod identity. Task 2 consumes it.

- [ ] **Step 1: Read what is being extracted, and why each check exists**

`kubernetesEnrollmentVerifier.Verify` does five things after the `TokenReview`, and **every one of them is load-bearing**:

1. `review.Status.Authenticated` **and** `contains(review.Status.Audiences, …)` — the audience must be confirmed by the API server, not merely requested.
2. `parseBoundPodClaims(token)` — the projected claims naming the namespace, ServiceAccount, Pod and UIDs.
3. `review.Status.User.Username == "system:serviceaccount:"+ns+":"+sa` — the reviewed subject must match the claims.
4. The ServiceAccount UID comparison, whose comment explains it stops a token minted for a **deleted-and-recreated ServiceAccount of the same name** from being accepted.
5. The policy's namespace and ServiceAccount constraints.

Items 1–4 are about the token. Item 5 is about enrollment policy. **Extract 1–4; leave 5 where it is.**

- [ ] **Step 2: Write the failing test**

Create `internal/controller/kubernetes_token_test.go`. The audience behaviour is the security core, so test it first and in both directions:

```go
// A token minted for one audience must not verify against another. This is
// what stops a job Pod's store-credential token from enrolling an agent, and
// an agent's enrollment token from fetching store credentials.
func TestVerifyProjectedToken_RejectsAWrongAudience(t *testing.T) {}

// The API server's answer is what counts, not what we asked for: a review that
// authenticates but does not echo the audience must be rejected.
func TestVerifyProjectedToken_RejectsUnconfirmedAudience(t *testing.T) {}

// The reviewed subject must match the token's own claims.
func TestVerifyProjectedToken_RejectsSubjectMismatch(t *testing.T) {}

// A recreated ServiceAccount of the same name has a new UID; a token minted
// for the old one must not verify.
func TestVerifyProjectedToken_RejectsStaleServiceAccountUID(t *testing.T) {}
```

Use the fake `kubernetes.Interface` the existing enrollment tests use — find it rather than building a second one.

- [ ] **Step 3: Run it to verify it fails**

Run: `go test ./internal/controller/ -run VerifyProjectedToken -count=1`
Expected: FAIL — the function does not exist.

- [ ] **Step 4: Extract**

Move items 1–4 into `kubernetes_token.go` as something like:

```go
// BoundPodIdentity is the identity a projected ServiceAccount token proves.
type BoundPodIdentity struct{ Cluster, Namespace, ServiceAccount, PodName, PodUID string }

// VerifyProjectedToken confirms a projected ServiceAccount token against one
// audience and returns the Pod identity it proves.
//
// The audience is a parameter and not a constant because two callers use this
// with DIFFERENT audiences, and that difference is a security boundary: agent
// enrollment and store-credential brokering must never accept each other's
// tokens. A shared audience would let any job Pod register itself as an agent.
func VerifyProjectedToken(ctx context.Context, client kubernetes.Interface, cluster, audience, token string, timeout time.Duration) (BoundPodIdentity, error)
```

Rewrite `kubernetesEnrollmentVerifier.Verify` to call it and then apply item 5. **Its existing tests must pass untouched** — that is the evidence the refactor changed nothing.

- [ ] **Step 5: Verify**

Run: `go test ./internal/controller/ -run 'VerifyProjectedToken|Enrollment' -count=1`
Expected: PASS, with the enrollment tests unmodified.

- [ ] **Step 6: Commit**

```bash
git add internal/controller/
git commit -m "refactor(controller): extract projected-token verification so a second audience can use it"
```

---

### Task 2: The broker endpoint

**Files:**
- Create: `internal/controller/api_store_credentials.go`, and its test
- Modify: `internal/controller/server.go`, `internal/api/types.go`

**Interfaces:**
- Consumes: `VerifyProjectedToken` from Task 1.
- Produces: `POST /api/v1/store-credentials`, and `api.StoreCredentialsRequest` / `api.StoreCredentialsResponse`. Task 3 consumes them.

- [ ] **Step 1: Write the failing tests**

The audience separation deserves an end-to-end test at this layer too, not only at Task 1's:

```go
// The security property, at the endpoint: an enrollment-audience token must
// not buy store credentials.
func TestStoreCredentials_RejectsAnEnrollmentToken(t *testing.T) {}

// And the converse, asserted against the enrollment endpoint.
func TestAgentEnrollment_RejectsAStoreCredentialToken(t *testing.T) {}

// A valid token from a Pod in a permitted namespace gets credentials.
func TestStoreCredentials_ReturnsCredentialsForAValidToken(t *testing.T) {}

// The controller has no store configured: a clear error naming what to set,
// not an empty credential the sidecar will fail to sign with later.
func TestStoreCredentials_ErrorsWhenNoStoreIsConfigured(t *testing.T) {}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/controller/ -run StoreCredentials -count=1`
Expected: FAIL — the route does not exist, so the request 404s.

- [ ] **Step 3: Add the types**

In `internal/api/types.go`:

```go
type StoreCredentialsRequest struct {
	Token string `json:"token"`
	RunID string `json:"runId"`
}

// StoreCredentialsResponse carries what the sidecar needs to build a client.
//
// ExpiresAt is present even when the credential is static and does not expire —
// it is zero then. The field exists now so that a future scoped or STS-minted
// credential is a change of what the controller returns rather than a change of
// shape, and the sidecar's refresh logic does not have to be added later.
type StoreCredentialsResponse struct {
	Endpoint  string    `json:"endpoint"`
	Bucket    string    `json:"bucket"`
	Region    string    `json:"region,omitempty"`
	UseSSL    bool      `json:"useSsl"`
	AccessKey string    `json:"accessKey"`
	SecretKey string    `json:"secretKey"`
	Token     string    `json:"sessionToken,omitempty"`
	ExpiresAt time.Time `json:"expiresAt,omitempty"`
}
```

- [ ] **Step 4: Write the handler, and stop at passthrough**

Verify the token with Task 1's function against the **store-credential audience**, then return the controller's own store configuration.

**Return the controller's credential as-is. Do not scope it.** Reasons, and they should be a comment at the handler:

- Scoping to a run's prefix needs store support that is not universal — the shipped evaluation bundle uses Garage, whose `AssumeRoleWithWebIdentity` support is unconfirmed. Passthrough works on every store.
- Passthrough leaves the blast radius exactly where it is today, while removing the per-namespace Secret. That is the operator-facing win, and it is complete on its own.
- `ExpiresAt` and `sessionToken` in the response shape mean adding scoping later changes what the controller returns, not what anything parses.

**Authorization, and what it does and does not need today.** The token proves the Pod's namespace, ServiceAccount and UID. It does **not** prove which run the Pod is executing. With a passthrough credential that gap costs nothing — every caller would receive the identical credential, so binding the request to a run buys no isolation. **When scoping arrives it becomes necessary**, and the agent would have to tell the controller which Pod runs which run. Say that in the comment so the next person does not have to rediscover why `RunID` is accepted but not yet enforced.

Restrict *which* Pods may ask. Follow how enrollment policy constrains namespaces and ServiceAccounts rather than inventing a second mechanism, and decide whether the constraint is configuration or is derived from the agent's own enrollment policy. Say which and why.

- [ ] **Step 5: Register the route and verify**

Register beside the agent endpoints in `server.go`. This endpoint is **not** authenticated by an agent credential — the token is the credential — so check carefully which middleware applies.

Run: `go test ./internal/controller/ -count=1`

- [ ] **Step 6: Commit**

```bash
git add internal/controller/ internal/api/
git commit -m "feat(controller): broker store credentials against a projected pod token"
```

---

### Task 3: The Pod volume and the sidecar fetch

**Files:**
- Modify: `internal/k8sagent/podbuilder.go`, `internal/k8sagent/config.go`, and `cmd/unified-sidecar/`
- Test: `internal/k8sagent/podbuilder_test.go`

- [ ] **Step 1: Write the failing tests**

```go
// The token is mounted into the sidecar container ONLY. The job container runs
// user code; a store credential reachable from it defeats the container
// boundary that protects today's Secret.
func TestBuildPod_BrokerTokenIsSidecarOnly(t *testing.T) {}

// The audience on the projected volume must be the store-credential one. A
// volume minted with the enrollment audience would hand every job a token that
// can register an agent.
func TestBuildPod_BrokerTokenUsesTheStoreAudience(t *testing.T) {}

// Default off: an existing deployment's Pod spec is byte-identical.
func TestBuildPod_BrokerTokenAbsentByDefault(t *testing.T) {}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/k8sagent/ -run BrokerToken -count=1`

- [ ] **Step 3: Build the volume**

A `corev1.Volume` with a `Projected` source containing a `ServiceAccountToken` with the store-credential audience and a short `ExpirationSeconds`, mounted read-only into the sidecar container only.

**Check `internal/k8sagent/pool.go`.** Its reuse key is built from the sidecar's image and Secret name. A Pod built without the broker volume must not be reused for a claim wanting it — add the mode to that key, as the provider-seam plan does for its own mode.

- [ ] **Step 4: Teach the sidecar to fetch**

When pointed at a controller URL and a token file, the sidecar reads the token, calls the endpoint, and builds its store client from the response — through the §5.5 provider, so a credential carrying `ExpiresAt` refreshes by re-fetching.

Precedence stays as §5.5 defined it, with the broker ahead of the file and static paths. **Static env credentials remain last and unchanged**, so nothing existing moves.

The failure message when the fetch fails must name the controller and the reason. `artifact requires S3 configuration (UNIFIED_S3_*)` was the message that started all of this; do not produce its equivalent.

- [ ] **Step 5: Verify**

Run: `go build ./... && go vet ./... && go test ./internal/k8sagent/ ./internal/objectstore/ -count=1`

- [ ] **Step 6: Commit**

```bash
git add internal/k8sagent/ cmd/unified-sidecar/
git commit -m "feat(k8sagent): project a store-credential token into the sidecar, and fetch through it"
```

---

### Task 4: Documentation and the evaluation bundle

- [ ] **Step 1: Document it**

In `docs/operator-manual/kubernetes-integration.md`: what the broker replaces, how to enable it, that the existing Secret path is unchanged and still the default, and — plainly — that **the credential returned today is the controller's own and is not scoped per run**, with the reason.

Say what an operator gains: no Secret in any job namespace, and no agent access to Secrets. Do not claim short-lived or least-privilege credentials; neither is true yet.

- [ ] **Step 2: Decide whether `install.yaml` should use it**

The evaluation bundle currently demonstrates the trap — log archiving works out of the box, artifacts fail. With the broker it could work end to end with no extra Secret.

Judge it: is switching the bundle's default a good first impression, or does shipping an evaluation bundle on a non-default path mislead about what a production install looks like? Decide, say why, and if you switch it, make sure the bundle's own documentation says which path it is on.

- [ ] **Step 3: Verify**

Run: `python -m mkdocs build --strict` and `python scripts/check_redirect_collisions.py`

- [ ] **Step 4: Commit**

---

## Self-review notes

**The one thing that must not be got wrong.** The two audiences. Task 1 Step 2 and Task 2 Step 1 both test it, from different layers and in both directions, because a single shared audience turns every job Pod into an agent-enrollment credential. If a reviewer reads one thing in this branch, it should be those tests.

**What this plan deliberately does not deliver.** Scoped or short-lived credentials. The response shape accommodates them and the handler comment says what would have to change, but building them needs store support this project cannot assume. Shipping passthrough removes the operator's per-namespace Secret — which is the reported problem — and leaves the blast radius unchanged rather than worse.

**The stop condition.** If any task needs a new Kubernetes permission for the agent, stop and report. §5.6 was chosen over §5.2 precisely because it does not, and an implementation that quietly needs one has become §5.2 without the decision being made.
