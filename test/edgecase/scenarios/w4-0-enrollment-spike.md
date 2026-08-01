# W4-0 — Kubernetes agent enrollment spike (go/no-go)

> **VERDICT: NO. A k8s agent cannot enroll against the compose controllers —
> and it cannot enroll against *anything*.** The blocker is not networking,
> not config plumbing, and not the host-run shortcut. Every one of those was
> built and proven working in this spike. The blocker is a product defect:
> `kubernetesEnrollmentVerifier.Verify` reads the enrolling ServiceAccount's
> UID from a TokenReview response field **that the Kubernetes API server never
> populates**, so the check is unsatisfiable and every Kubernetes enrollment
> is rejected 403. Since PR #75 (`4e8f315`) deleted static token auth for the
> k8s agent, **the k8s agent has no working authentication path at all.**
> See FINDINGS W4-0 entry 1. Wave W4 must be re-planned around this.

This is a **spike record**, not a scenario runbook: it records what was tried,
what worked, what did not, and the verdict. It produces no invariant-probe
arms of its own.

---

## Verdict summary

| Step | Result |
| --- | --- |
| 1. Kubernetes cluster available | **YES** — already present, and it *is* kind |
| 2. Controller given kubeconfig + config file; policy seeded | **YES** — verified in DB |
| 3. Bidirectional network bridge (agent↔controller, controller↔API) | **YES** — both directions, full TLS verification |
| 4. Agent obtains a pod-bound token and enrolls | **NO** — 403, product defect |
| 5. Route usable for W4-1 / W4-2 | **NO** — nothing can enroll |

Steps 1-3 all succeeded. Step 4 is where it dies, and it dies for a reason
that makes the entire enrollment surface unreachable rather than merely
awkward on this rig.

---

## Environment

Captured 2026-08-01. Raw captures in the session scratchpad under `w4-1/`
(archived out-of-tree by the checkpoint task).

```
docker            29.6.1
kubectl client    v1.36.2
Kubernetes server v1.35.1
go                go1.26.2 windows/amd64
kind CLI          NOT INSTALLED  (and not needed — see step 1)
```

---

## Step 1 — Stand up kind

**Nothing to stand up. The cluster already existed, and Docker Desktop's
Kubernetes *is* kind.** The brief assumed a `kind create cluster` was needed
and that `kind` would be on PATH. Neither holds:

```
$ kind version
bash: kind: command not found

$ kubectl config get-contexts
CURRENT   NAME             CLUSTER          AUTHINFO         NAMESPACE
*         docker-desktop   docker-desktop   docker-desktop

$ kubectl get nodes -o wide
NAME                    STATUS   ROLES           AGE   VERSION   INTERNAL-IP
desktop-control-plane   Ready    control-plane   22d   v1.35.1   172.18.0.4

$ docker ps --format '{{.Names}}\t{{.Image}}'
desktop-control-plane   kindest/node:v1.35.1
kind-cloud-provider     docker/desktop-cloud-provider-kind:v0.6.0
kind-registry-mirror    docker/desktop-containerd-registry-mirror:v0.0.4
```

The node is a `kindest/node` container on a Docker bridge network literally
named `kind`. So the campaign gets kind's topology (node-as-container,
attachable bridge, `docker network connect` idiom) without installing the
kind CLI, and without a `kind create cluster` step that other W4 tasks would
have had to repeat.

**Namespaces — the brief left this open, and it is already settled:**

```
$ kubectl get ns
NAME                 STATUS   AGE
ci                   Active   21d
default              Active   22d
kube-node-lease      Active   22d
kube-public          Active   22d
local-path-storage   Active   22d
pvp                  Active   15d
unified-cd           Active   21d
```

**Both `unified-cd` and `ci` already exist.** `ci` (the namespace
`manifests/base/.../config-configmap.yaml` references) held only its `default`
ServiceAccount, so it is free for spike use. `manifests/` was not modified and
was not applied.

### Unexpected: a k8s agent is already deployed here, and has been crash-looping for 14 days

The brief says "There is no deployed k8s agent anywhere in this repo's
automation." That is true *of the repo*, but **not of this machine**:

```
$ kubectl get deploy,pod -n unified-cd
NAME                                    READY   UP-TO-DATE   AVAILABLE   AGE
deployment.apps/unified-cd-k8s-agent    0/1     1            0           21d

NAME                                        READY   STATUS             RESTARTS         AGE
pod/unified-cd-k8s-agent-59f75b6d74-2lfb7   0/1     CrashLoopBackOff   2983 (75s ago)   14d
```

Its pod spec still sets `UNIFIED_K8S_SECRET: /etc/unified-cd/secret.yaml` and
mounts a `unified-cd-k8s-agent-secret` — i.e. it is a **pre-#75 static-token
deployment**, left over from before static k8s auth was removed. It fails with:

```
{"level":"ERROR","msg":"k8s agent run","error":"http 401: unauthorized"}
```

This is not the enrollment path and is not evidence about it — the binary in
that image predates the current one. It is recorded because (a) it explains
2983 restarts an implementer will otherwise trip over, and (b) it is a
standing, independent demonstration that the k8s agent has been non-functional
on this machine for two weeks. **It was left untouched by this spike.**

### Spike identity created

`test/edgecase/k8s/w4-spike-identity.yaml` (committed) creates the ServiceAccount
the agent enrolls as, plus a live Pod bound to it — the Pod is mandatory because
the controller re-reads it (`agent_enrollment_kubernetes.go:93`,`:100-102`).

```
$ kubectl apply -f test/edgecase/k8s/w4-spike-identity.yaml
serviceaccount/w4-spike-agent created
pod/w4-spike-agent-pod created

$ kubectl -n ci get pod w4-spike-agent-pod -o wide
NAME                 READY   STATUS    RESTARTS   AGE   IP            NODE
w4-spike-agent-pod   1/1     Running   0          6s    10.244.0.14   desktop-control-plane

SA  UID = a7031b08-1c99-4348-994f-13b82ab66eb4
Pod UID = 8b221199-0bb8-4f1d-a7b0-ba3f50b7e710
```

---

## Step 2 — Give the controller a kubeconfig and a config file

The brief's premise here **holds exactly as written** and was re-confirmed:
`agentAuth` has no environment-variable path
(`internal/config/controller.go:277`, "Cluster credentials remain YAML-only"),
and `test/ha/docker-compose.ha.yaml` passes no `-f`. Without a config file the
verifier map is empty and enrollment answers 503.

Three committed assets, none of them touching `test/ha/` or `manifests/`:

- `test/edgecase/compose/controller-k8senroll.yaml` — the controller config.
- `test/edgecase/compose/k8senroll.override.yaml` — the compose overlay.
- `test/edgecase/k8s/make-spike-kubeconfig.sh` — generates the kubeconfig.

**The controller is given a least-privilege ServiceAccount, not the admin
kubeconfig.** `test/edgecase/k8s/w4-spike-controller-rbac.yaml` grants exactly
`create tokenreviews` + `get pods` — the two calls the verifier makes. This is
better than the admin kubeconfig the brief anticipated, and it documents the
minimum RBAC a real deployment needs:

```
$ kubectl auth can-i create tokenreviews --as=system:serviceaccount:unified-cd:w4-spike-controller
yes
$ kubectl auth can-i get pods -n ci --as=system:serviceaccount:unified-cd:w4-spike-controller
yes
```

The generated kubeconfig carries a live bearer token and is **gitignored**
(`.gitignore` entry added); only the generator script is committed.

Bring-up:

```
docker compose -f test/ha/docker-compose.ha.yaml \
               -f test/edgecase/compose/k8senroll.override.yaml up -d
```

**Policy seeding confirmed** — `bootstrapKubernetesEnrollmentPolicies`
(`cmd/controller/main.go:156-169`) wrote the row:

```
$ docker exec unified-cd-ha-postgres-1 psql -U unified -d unified \
    -c "select name, provider, subject_constraints, agent_id_template, allowed_labels, enabled from agent_enrollment_policies;"
      name       |  provider  |                   subject_constraints                    |         agent_id_template          |    allowed_labels     | enabled
-----------------+------------+---------------------------------------------------------+------------------------------------+-----------------------+---------
 w4-spike-policy | kubernetes | {"namespaces": ["ci"], "serviceAccounts": ["w4-spike-agent"]} | k8s:{cluster}:{namespace}:{podUID} | {kind:linux,w4:spike} | t
```

### Unexpected: one controller died on cold start and stayed dead

On the first `up`, `controller1` exited(1) immediately:

```
controller1-1 | {"level":"ERROR","msg":"sync UNIFIED_TOKEN as bootstrap PAT",
                 "error":"create bootstrap pat: ERROR: duplicate key value violates
                          unique constraint \"pats_token_hash_key\" (SQLSTATE 23505)"}

$ docker compose -p unified-cd-ha ps -a
controller1   Exited (1)
controller2   Up
controller3   Up
```

Three controllers race to insert the same `UNIFIED_TOKEN`-derived PAT on a cold
database; the loser hits the `token_hash` unique constraint and
`cmd/controller/main.go:277-281` calls `os.Exit(1)`. The rig sets no `restart:`
policy, so it stays down. A manual `start` succeeds once the row exists, which
confirms it is **cold-start-only**:

```
$ docker compose -p unified-cd-ha start controller1 && sleep 8 && docker compose -p unified-cd-ha ps -a
controller1   Up 8 seconds
...
$ docker exec unified-cd-ha-postgres-1 psql -U unified -d unified -c "select name from pats;"
       name
-------------------
 env:UNIFIED_TOKEN
(1 row)
```

This is unrelated to the overlay (which only adds `-f`, two mounts, and a
network) and unrelated to Kubernetes. Recorded as FINDINGS W4-0 entry 3.

---

## Step 3 — Bridge the networks

**Both directions verified. No TLS workaround was needed** — this contradicts
the brief's expectation that `insecure-skip-tls-verify` might be required.

The brief reasoned about `host.docker.internal`, which is indeed not in the
apiserver certificate. But the node's own **container name is**, and Docker's
embedded DNS resolves it on the `kind` bridge:

```
$ docker run --rm --network kind alpine/openssl s_client -connect 172.18.0.4:6443 \
    </dev/null | openssl x509 -noout -text | grep -A1 'Alternative Name'
X509v3 Subject Alternative Name:
    DNS:desktop-control-plane, DNS:kubernetes, DNS:kubernetes.default,
    DNS:kubernetes.default.svc, DNS:kubernetes.default.svc.cluster.local,
    DNS:localhost, IP Address:10.96.0.1, IP Address:172.18.0.4, IP Address:127.0.0.1
```

So the kubeconfig uses `https://desktop-control-plane:6443` with the cluster CA
embedded, and **verifies normally**. The overlay attaches the controllers to the
external `kind` network *in addition to* the rig's default network — listing
`default` explicitly is load-bearing, since naming any network replaces implicit
default membership and would cut the controllers off from postgres/garage/nginx.

**(a) agent (host) → controller**, via the rig's published nginx port:

```
$ curl -s -o /dev/null -w 'http_code=%{http_code}\n' http://localhost:18080/healthz
http_code=200
$ curl -s http://localhost:18080/healthz
ok
```

**(b) controller → Kubernetes API:**

```
$ docker inspect unified-cd-ha-controller1-1 --format '{{range $k,$v := .NetworkSettings.Networks}}{{$k}}={{$v.IPAddress}} {{end}}'
kind=172.18.0.6 unified-cd-ha_default=172.20.0.10

$ docker exec unified-cd-ha-controller1-1 getent hosts desktop-control-plane
fc00:f853:ccd:e793::4  desktop-control-plane  desktop-control-plane

$ docker exec unified-cd-ha-controller1-1 wget -q -O- --no-check-certificate --timeout=5 \
    https://desktop-control-plane:6443/version
{ "major": "1", "minor": "35", ... "gitVersion": "v1.35.1", ... }
```

`--no-check-certificate` here proves *transport* reachability only. Certificate
validity is established separately by the SAN capture above, and conclusively
by step 4: the controller's Go client — which verifies against the embedded CA
with no insecure flag anywhere — completed a real TokenReview and a real
Pods.Get. **The enrollment reached the policy-evaluation stage, which is only
possible if both API calls succeeded.**

Addresses used:
- agent → controller: `http://localhost:18080` (loopback; note
  `allowInsecureHTTP` is not even strictly required here, because
  `isLoopbackHost` in `internal/k8sagent/config.go:242-248` already exempts
  `localhost`. It is set anyway, explicitly.)
- controller → API: `https://desktop-control-plane:6443` over the `kind` bridge.

---

## Step 4 — Run the agent on the host. **Enrollment fails.**

```
$ go build -buildvcs=false -o k8s-agent.exe ./cmd/k8s-agent
BUILD OK
```

(`-buildvcs=false` was required: plain `go build` fails in this worktree with
`error obtaining VCS status: exit status 128`.)

Token minted exactly as the brief prescribes:

```
$ kubectl create token w4-spike-agent -n ci \
    --audience=unified-cd-agent-enrollment \
    --bound-object-kind Pod --bound-object-name w4-spike-agent-pod --duration=2h
```

**The brief's open question — whether this satisfies `parseBoundPodClaims`
(`agent_enrollment_kubernetes.go:128-130`) — is settled: YES, it does.** Every
field the parser demands is present:

```json
{
  "aud": ["unified-cd-agent-enrollment"],
  "sub": "system:serviceaccount:ci:w4-spike-agent",
  "kubernetes.io": {
    "namespace": "ci",
    "node": {"name": "desktop-control-plane", "uid": "fa803348-..."},
    "pod": {"name": "w4-spike-agent-pod", "uid": "8b221199-0bb8-4f1d-a7b0-ba3f50b7e710"},
    "serviceaccount": {"name": "w4-spike-agent", "uid": "a7031b08-1c99-4348-994f-13b82ab66eb4"}
  }
}
```

Agent run:

```
$ ./k8s-agent.exe --config agent-config.yaml --log-level debug
{"level":"WARN","msg":"sidecarS3SecretName is not set: ..."}
{"level":"ERROR","msg":"bootstrap agent credentials","error":"kubernetes enrollment request failed"}
exit=1
```

Controller side — **403, not 503**:

```
controller2-1 | {"level":"INFO","msg":"http request","method":"POST",
                 "path":"/api/v1/agents/enroll","status":403,"duration_ms":18,
                 "remoteAddr":"172.20.0.8:44808"}
```

**Which side produced it: the controller.** And the distinction between 403 and
503 is the single most informative bit in this spike. Per
`internal/controller/api_agent_enrollment.go:346-351`, a missing verifier
yields **503** "kubernetes identity unavailable". We got **403** "enrollment
policy rejected", which means the verifier *was* registered, the kubeconfig
*was* loaded, and `Verify` ran to completion against the live cluster —
i.e. **steps 1-3 all worked, and the rig is correct.**

### Root cause: the SA-UID check reads a field Kubernetes does not populate

`Verify` requires the reviewed ServiceAccount UID at
`agent_enrollment_kubernetes.go:84-87`:

```go
reviewedUID, hasReviewedUID := review.Status.User.Extra["authentication.kubernetes.io/serviceaccount.uid"]
if claims.ServiceAccount.UID == "" || !hasReviewedUID || len(reviewedUID) != 1 ||
   reviewedUID[0] == "" || reviewedUID[0] != claims.ServiceAccount.UID {
    return ..., fmt.Errorf("%w: token review service account UID", ErrKubernetesEnrollmentRejected)
}
```

Issuing that exact TokenReview by hand against the live cluster:

```
$ kubectl create -o json -f - <<< '{"apiVersion":"authentication.k8s.io/v1","kind":"TokenReview",
    "spec":{"token":"<redacted>","audiences":["unified-cd-agent-enrollment"]}}'

authenticated : True
audiences     : ['unified-cd-agent-enrollment']
user.username : system:serviceaccount:ci:w4-spike-agent
user.uid      : 'a7031b08-1c99-4348-994f-13b82ab66eb4'
extra keys    : ['authentication.kubernetes.io/credential-id',
                 'authentication.kubernetes.io/node-name',
                 'authentication.kubernetes.io/node-uid',
                 'authentication.kubernetes.io/pod-name',
                 'authentication.kubernetes.io/pod-uid']
serviceaccount.uid extra present? -> False
```

**There is no `authentication.kubernetes.io/serviceaccount.uid` extra.** The
ServiceAccount UID is returned in **`review.Status.User.UID`** — and its value,
`a7031b08-...`, is *exactly* the value the check wants to compare against.
The code looks in the wrong place. `hasReviewedUID` is therefore always
`false`, and the branch rejects **unconditionally**.

This is fatal on its own, independent of everything downstream. Every other
condition in `Verify` was independently confirmed satisfied (audience ✓,
authenticated ✓, username matches claims ✓, all claim fields present ✓,
namespace/SA within policy constraints ✓, pod live with matching UID and
`serviceAccountName` ✓, requested labels ⊆ `allowedLabels` ✓), so the UID
branch is the only reachable cause of the 403.

### It is not an artifact of running the agent on the host

The obvious objection is that `kubectl create token` differs from the projected
volume token a real in-cluster Deployment would use. **It does not.** A pod was
created with a `serviceAccountToken` projected volume at the enrollment
audience, its token read from inside the container, and reviewed:

```
=== PROJECTED-VOLUME token (what an in-cluster Deployment gets) ===
authenticated : True
audiences     : ['unified-cd-agent-enrollment']
user.username : system:serviceaccount:ci:w4-spike-agent
user.uid      : 'a7031b08-1c99-4348-994f-13b82ab66eb4'
extra keys    : ['authentication.kubernetes.io/credential-id',
                 'authentication.kubernetes.io/node-name',
                 'authentication.kubernetes.io/node-uid',
                 'authentication.kubernetes.io/pod-name',
                 'authentication.kubernetes.io/pod-uid']
serviceaccount.uid extra present? -> False
```

Byte-for-byte the same shape. **Building an image and deploying in-cluster
would fail identically.** That is precisely why this spike stops here instead
of escalating, as the brief instructs.

### Why the unit tests do not catch it

`internal/controller/agent_enrollment_kubernetes_test.go:175-181` fabricates
the key with a fake clientset:

```go
func tokenReviewReactor(authenticated bool, audiences []string, username, serviceAccountUID string) k8stesting.ReactionFunc {
	return func(action k8stesting.Action) (bool, runtime.Object, error) {
		extra := map[string]authv1.ExtraValue{}
		if serviceAccountUID != "" {
			extra["authentication.kubernetes.io/serviceaccount.uid"] = authv1.ExtraValue{serviceAccountUID}
		}
		return true, &authv1.TokenReview{Status: authv1.TokenReviewStatus{..., User: authv1.UserInfo{Username: username, Extra: extra}}}, nil
	}
}
```

It never sets `UserInfo.UID`. The fake mirrors the implementation rather than
the API server, so the suite is green against a contract that does not exist:

```
$ go test ./internal/controller/ -run TestKubernetesEnrollment -v
--- PASS: TestKubernetesEnrollmentVerifier_RejectsMissingOrAmbiguousServiceAccountUID/missing_service_account_UID
...
PASS
ok  github.com/eirueimi/unified-cd/internal/controller  1.523s
```

There is even a passing test asserting rejection when the extra is missing —
which, in reality, is *always*. The happy-path test passes only because it
injects a field the API server never sends.

---

## Step 5 — Verdict and consequences

### Does enrollment work?

**No.** Not host-side, and not in-cluster either. The failure is not a property
of this rig, this cluster, or the host-run shortcut — it is a defect in
`internal/controller/agent_enrollment_kubernetes.go:84`. Combined with PR #75's
removal of static token auth (`internal/k8sagent/config.go` has no `Token`
field and `enrollmentPolicy` is mandatory), **the Kubernetes agent currently
has no usable authentication path whatsoever.**

Per campaign Phase-1 rules, **no product code was changed.** The fix is
one-line-shaped (compare against `review.Status.User.UID`), but proposing and
landing it is not this campaign's job.

### What the next task should assume

1. **W4-1 and W4-2 as chartered are blocked.** Both need a running, enrolled
   k8s agent. Nothing can enroll. Re-plan the wave; do not try to route around
   this with an image build or a Deployment — that path was disproven above
   with a projected-volume token.
2. **The rig itself is built, correct, and committed.** Steps 1-3 do not need
   redoing. Reuse:
   - `test/edgecase/k8s/w4-spike-identity.yaml` (SA + bound Pod)
   - `test/edgecase/k8s/w4-spike-controller-rbac.yaml` (minimum controller RBAC)
   - `test/edgecase/k8s/make-spike-kubeconfig.sh` (regenerates the gitignored kubeconfig)
   - `test/edgecase/compose/controller-k8senroll.yaml` + `k8senroll.override.yaml`

   The moment the UID check is fixed, this rig should enroll on the first try —
   though that is a **prediction, not a measurement**, since it could not be
   tested without changing product code.
3. **No kind CLI, no `kind create cluster`, no `kind load`.** Docker Desktop's
   Kubernetes is `kindest/node` on the `kind` bridge. Use `desktop-control-plane`
   as the API host from containers; TLS verifies cleanly.
4. **Do not budget for a TLS workaround.** There is none in this rig.
5. **The admin-kubeconfig concern in the brief does not apply.** The controller
   runs on a least-privilege SA. Separately, W4-2's RBAC-denial arm would still
   need an in-cluster agent bound to a restricted Role — but that is moot until
   enrollment works at all.
6. **Expect one controller to die on every cold `up` of the HA rig** until the
   bootstrap-PAT race is fixed. Either `docker compose start <dead controller>`
   afterwards, or check all three are `Up` before trusting a 3-replica premise.

### Corrections to the brief's premises

| Brief claim | Outcome |
| --- | --- |
| k8s agent cannot use a static token; `enrollmentPolicy` mandatory | **Confirmed** (`internal/k8sagent/config.go:16-48`,`:190-192`) |
| Enrollment is bidirectional; controller needs cluster access | **Confirmed** — both API calls observed to run |
| Compose controllers can't do this (env-only, no `-f`) → 503 | **Confirmed**, and fixed by the overlay |
| HTTPS enforced agent-side unless `allowInsecureHTTP`/loopback | **Confirmed** (`config.go:228-248`); loopback alone suffices here |
| Agent binary can run on host against the cluster | **Confirmed** — it ran; enrollment failed for unrelated reasons |
| Need to create a kind cluster; `kind` on PATH | **FALSE** — no kind CLI; cluster already exists and is kind-based |
| Whether a `ci` namespace exists is an open question | **RESOLVED** — `ci` and `unified-cd` both already exist |
| `insecure-skip-tls-verify` may be required | **FALSE** — node container name is in the SANs; verifies cleanly |
| Whether `kubectl create token --bound-object-kind Pod` satisfies `parseBoundPodClaims` | **RESOLVED — YES** |
| No deployed k8s agent anywhere | **True of the repo; FALSE of this machine** — a pre-#75 deployment has been crash-looping 14 days |
| Controller would run with an admin kubeconfig | **Avoided** — least-privilege SA used instead |

The W3 pattern held again: **every `file:line` claim in the brief was accurate;
the mechanism-level and environment-level claims are where it went wrong.**

---

## Cleanup

- Agent process exited on its own (exit 1); no background process left running.
  Verified: no `k8s-agent` in `tasklist`.
- Ad-hoc projected-token probe pod deleted (`kubectl -n ci delete pod w4-spike-projected`).
- HA stack torn down with `down -v` (both volumes removed), restoring the state
  found at start — the rig was **not** running when this spike began.
- External `kind` network intact; the separate dev stack (`unified-cd_default`,
  7 containers) was never touched.
- The pre-existing crash-looping `unified-cd-k8s-agent` Deployment was left
  exactly as found.
- Manifest-backed spike objects (`w4-spike-agent` SA, `w4-spike-agent-pod`,
  controller RBAC) were **left in place** so the next implementer can reproduce
  the 403 immediately. Remove with
  `kubectl delete -f test/edgecase/k8s/w4-spike-identity.yaml -f test/edgecase/k8s/w4-spike-controller-rbac.yaml`.
- The generated kubeconfig is gitignored and its SA token expires in 24h;
  regenerate with `test/edgecase/k8s/make-spike-kubeconfig.sh`.
- No ServiceAccount token, kubeconfig, or agent credential appears in any
  committed file or in any capture retained in the scratchpad.
