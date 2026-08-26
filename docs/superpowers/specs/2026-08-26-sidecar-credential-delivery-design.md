# Delivering object-store credentials to the Kubernetes sidecar — Design

Date: 2026-08-26
Status: Draft for review

## 1. Purpose

An operator deployed unified-cd on Kubernetes, put S3 credentials in the
controller's Secret, watched log archiving work, and then had artifact steps
fail mid-run with `artifact requires S3 configuration (UNIFIED_S3_*)`.

Two more rounds of trial and error followed: discovering that the agent needs
its own `sidecarS3SecretName`, and then that the Secret it names must live in
the **job Pod's** namespace rather than the agent's.

Nothing was broken. Every component behaved as designed. The operator's
inference — *log archiving works, so the object store is configured* — was
reasonable and wrong, and the system offered no way to find that out except by
running a job and reading a failure at the end of it.

This design is about the credential path that produced that inference. It is
**not** about the detection fixes, which are being made separately and are
described in §4 only so this document's scope is clear.

## 2. Who needs credentials today

| Component | Uses object storage for | Credentials from | Constraint |
|---|---|---|---|
| Controller | Log archives, the artifact HTTP API, cache TTL cleanup, run retention, git-template cache | `UNIFIED_S3_*` in its own Secret | Controller's namespace |
| Standard agent | **Cache only**, direct to the store | `UNIFIED_CACHE_*` — a different variable family | Agent process |
| Standard agent, artifacts | — | **None.** Proxied through the controller over HTTP | — |
| Kubernetes agent process | Nothing. It never opens a store client | — | — |
| **`unified-artifact` sidecar** | **Artifacts and cache**, direct to the store | `UNIFIED_S3_*`, injected by `envFrom.secretRef` | The Secret must exist in the **job Pod's** namespace. `LocalObjectReference` has no namespace field. |

One coupling nothing documents: the sidecar writes to
`artifacts/{runID}/{name}.tar.gz` and the controller's list/download API reads
the same prefix. The sidecar's bucket must therefore be **the same bucket** as
the controller's, or a Kubernetes run's artifacts exist but are invisible to
`unified-cli artifact list` and to the UI.

## 3. Why the trap exists

Logs and artifacts take different routes, and only one of them passes through
the component the operator configured.

Logs go agent → controller → store. The controller's credentials suffice, and
they are the ones the install manifests set up. Artifacts on Kubernetes go
sidecar → store, directly, using credentials the controller never sees and
cannot supply.

So a working log archive proves nothing about artifacts. Worse, three things
actively encourage the wrong conclusion:

- The controller's own startup summary says *"log archival and artifacts are
  disabled; set `UNIFIED_S3_ENDPOINT` and `UNIFIED_S3_BUCKET`"*. That is true
  for the standard agent and false for Kubernetes.
- The shipped `install.yaml` produces exactly this state: log archiving works
  out of the box, artifacts fail out of the box.
- `manifests/README.md` documents the two controller Secrets and never mentions
  a third.

The standard agent has the mirror-image trap for **cache**: its credentials are
`UNIFIED_CACHE_*`, a family that appears in no S3 documentation section, so an
operator who configured `UNIFIED_S3_*` has a silently disabled cache there.

## 4. What is being fixed separately, and why that is not enough

A separate change makes the failure honest and early: a cache step that could
not reach the store stops reporting a hit, a claim carrying an artifact step
fails immediately when no sidecar Secret is configured, and a Pod stuck on a
missing Secret reports `secret "…" not found` instead of a five-minute
`context deadline exceeded`.

That work is worth doing on its own and should land regardless of what this
document concludes. But it makes a misconfiguration **findable**, not
**impossible**. The operator still has to know that a third Secret exists, put
it in the right namespace, and keep it in step with the controller's.

## 5. Options

### 5.1 Keep the operator-created Secret (today)

The sidecar reads `UNIFIED_S3_*` from a Secret the operator creates in each job
namespace and names in the agent's config.

**For.** The agent never touches Secrets — it has no such RBAC and cannot read
or write them. The sidecar's credentials can be scoped independently of the
controller's: a different key, a different bucket policy, a different rotation
schedule. Container-boundary isolation keeps them out of the job container.

**Against.** Every job namespace needs its own copy, kept in step by hand. The
credential is long-lived, static and bucket-scoped. And the setup is
undiscoverable in the way §3 describes.

### 5.2 The controller ships credentials to the agent

The mechanism already exists: `FetchSecrets(ctx, agentID, runID, names)`
delivers a run's secrets from the controller to the agent, encrypted at rest
under the KEK. Adding the store credentials to that path is a small change on
its face.

The difficulty is the last hop. The sidecar reads its configuration from its own
process environment, so the agent must get the values into the Pod. Two ways:

- **Literal `env:` entries in the Pod spec.** Rejected. The values become
  readable by anyone with `get pod` in the job namespace — *worse* than a
  Secret, which at least requires a separate permission.
- **A per-run Secret the agent creates**, owner-referenced to the Pod so it is
  garbage-collected. This works, and it removes the operator's manual
  duplication entirely.

**The cost is a trust-boundary change, and it is the real decision here.** The
agent's Role in the job namespace today grants `pods`, `pods/exec`, `pods/log`
and PVCs — **no access to Secrets at all**. This option requires `create` and
`delete` on Secrets there. The project's posture elsewhere is deliberately the
opposite: job secrets are encrypted under a KEK the agent never holds, the
agent's own credentials are denylisted from ever reaching a step, and agent
enrollment uses a projected, audience-bound token specifically so that no shared
secret exists to steal.

A second cost: the controller's credentials become every agent's credentials,
unless the controller holds a *separate* sidecar credential to hand out — which
reintroduces the two-credential setup this option exists to remove, just moved
into the controller's configuration.

### 5.3 Route Kubernetes artifacts through the controller

The standard agent proves this works: its artifacts need no agent-side store
configuration at all, because they are proxied over HTTP.

**This does not close the failure class.** Cache is direct-to-store on *both*
agent types by design — the standard agent uses its own client, the Kubernetes
agent uses the sidecar — so routing artifacts through the controller leaves
cache still needing the sidecar's credentials, and cache is the half that fails
silently. It converts one trap into a smaller one rather than removing it.

It also reopens a deliberate, documented trade: the sidecar exists to move
artifact bytes directly to the store rather than through the controller. One
misconfiguration is not sufficient reason to revisit that, and doing so would
need throughput measurement this document does not have.

### 5.4 Short-lived, per-pod credentials

The documentation already names this as the destination: *"a planned hardening
is to move to short-lived, per-pod credentials via IAM Roles for Service
Accounts (IRSA) on EKS or an equivalent Workload Identity / STS-assumed-role
mechanism on other clouds, so the sidecar authenticates via a projected
service-account token instead of a static Secret."* The same section calls the
current shape **"not least-privilege per run"**.

This removes the operator's per-namespace Secret, removes the long-lived
credential, and does not require the agent to touch Secrets — the token is
projected by the kubelet, not created by the agent.

Its cost is that it is **not portable**. IRSA is EKS; Workload Identity is GKE;
self-hosted MinIO and Garage need STS support that may not exist in a given
deployment. So it cannot be the only path — a static-credential fallback has to
remain for clusters that cannot do it, which means the setup this design set out
to simplify stays, as a second supported configuration.

## 6. Recommendation

**Do not redesign the credential path yet. Ship the detection work, make the
setup discoverable, and treat §5.4 as the destination with §5.2 as its fallback
shape.**

The reasoning:

- The operator's failure was one of **discoverability**, not of mechanism. §4's
  work plus the documentation and manifest changes in §7 address that directly,
  at a fraction of the cost and with no trust-boundary change.
- §5.2's real price is granting the agent write access to Secrets in the job
  namespace. That is a considered posture the project has defended in three
  separate places, and it should be given up for a stronger reason than saving
  an operator one `kubectl create secret`.
- §5.3 does not close the failure class, because cache stays direct.
- §5.4 is right and is already the stated plan, but it is per-cloud work that
  cannot remove the static path.

If §5.2 is nonetheless wanted, the version worth building is the one that
**also** serves §5.4: the agent projects a credential it obtained from the
controller, and the controller is free to mint that credential per-run and
short-lived where the backing store supports it. Building §5.2 with static
credentials alone buys convenience and pays for it in blast radius.

## 7. What should change regardless

These are cheap, and each removes one step of the operator's trial and error.

1. **`docs/operator-manual/kubernetes-integration.md` says to create the Secret
   "in the agent's namespace".** In the shipped manifests the agent Deployment
   is in `unified-cd` and job Pods run in `ci`. That one word is the whole of
   the operator's second failure.
2. **The same page claims a nonexistent named Secret behaves like an unset
   one.** It does not: unset degrades, missing breaks *every* job with
   `CreateContainerConfigError`.
3. **The artifacts user guide links out for cache credentials but not for
   artifact credentials** — backwards from the failure mode.
4. **`manifests/` never mentions `sidecarS3SecretName`.** The `ci` namespace is
   already created by the base overlay, so the Secret has a home; there is just
   nothing to put in it and nothing telling the operator to.
5. **`install.yaml` demonstrates the trap.** The evaluation bundle should either
   work end to end or say plainly which parts do not.
6. **The controller's startup summary claims setting `UNIFIED_S3_*` enables
   artifacts.** True for the standard agent, false for Kubernetes.
7. **No troubleshooting entry exists for `requires S3 configuration`** — a grep
   over `docs/` for the string an operator actually sees returns nothing.

## 8. Out of scope

- The sidecar's direct-to-store design. Reopening it needs throughput evidence,
  not one misconfiguration.
- The standard agent's `UNIFIED_CACHE_*` naming. It is the mirror-image trap and
  deserves its own decision; renaming an environment variable family is a
  breaking change for every existing deployment.
- Widening the agent's RBAC. Nothing here recommends it; §5.2 records what it
  would cost so the decision can be made deliberately rather than as a side
  effect.

## 9. Open questions

- **Is a second supported configuration acceptable?** §5.4 cannot cover
  self-hosted stores without STS, so adopting it means maintaining both paths.
  If that is unacceptable, §5.4 is not the destination and §5.2 becomes the
  candidate on its own merits — with the RBAC cost paid explicitly.
- **Should the controller hold a separate sidecar credential** even under §5.2,
  so the sidecar's blast radius stays smaller than the controller's? That
  restores a two-credential setup, but in one place instead of every namespace.
- **What does the sidecar do when a short-lived credential expires mid-transfer?**
  §5.4 needs an answer before it is a plan; a large artifact upload can outlive
  a token.
