# W4 rig — a working Kubernetes agent, built around a broken enrollment path

> **DISCLOSURE, STATED ONCE HERE AND REFERENCED BY EVERY W4 SCENARIO.**
> Kubernetes agent enrollment does not work and cannot be made to work without
> a product-code change (W4-0). Every W4 scenario therefore runs against a
> controller whose **enrollment is bypassed by test infrastructure**: an
> interposer answers `POST /api/v1/agents/enroll` with a credential minted
> through the product's ordinary `"enrollment"` method instead.
> **Consequence:** no W4 finding says anything about the Kubernetes enrollment
> path beyond what `w4-0-enrollment-spike.md` already records. Everything
> downstream of authentication — claiming, pod lifecycle, execution, logging,
> reporting, pooling, GC — is the **real** product path, unmodified, and W4's
> findings about it stand on their own. What is bypassed is exactly one
> endpoint, and what replaces it is a real controller-issued credential, not a
> synthetic one.

This is a **rig record**, like `w4-0-enrollment-spike.md`: it records what was
built, what was measured, and what the next task may assume. It probes no
invariant of its own.

> **TIMESTAMPS: two clocks appear below and they are the same instant.**
> The agent and the interposer run on the host and log **local time,
> `+09:00`** (`15:26:27` etc.); the controller, the database and every
> `date -u` in a capture script log **UTC** (`06:26:27Z` etc.). **Local =
> UTC + 9 h.** Steps 5-7 alternate between them within a few lines, so
> subtract 9 before comparing a host log line to a database row.

---

## Verdict summary

| Step | Result |
| --- | --- |
| 1. Route decision recorded | **host-run** for W4-1/W4-3; in-cluster **deferred** to W4-2 |
| 2. `uca_` from the `"enrollment"` method accepted for k8s-agent traffic | **YES** — all six request paths in the table below, verified live |
| 3. Interposer built and proven by effect | **YES** — agent enrolled, registered, claiming |
| 4. Fixtures built, verified through the real `dsl.Parse` | **YES** — 4 fixtures, both source and payload-extracted YAML |
| 5. k8s fault-injection verbs, each with an effect measurement | **YES**, all of them — but `block hang` shipped unmeasured and was measured only at review (Step 5). One verb was found inert and fixed |
| 6. End-to-end baseline | **PASS** — pod created, run `Succeeded`, pod cleaned up |
| 7. Pod deleted mid-run → terminal status | **`Failed`, ~1 s, exit code 137** — not the reaper |

---

## Step 1 — Route decision

**Host-run for everything in W4 except W4-2's RBAC-denial arm, which is
deferred.**

The agent runs as a host process (`test/edgecase/tools/w4/bin/k8s-agent`)
against Docker Desktop's Kubernetes, using the developer's own
`~/.kube/config` (`kubeconfig:` is left unset in
`test/edgecase/k8s/w4-agent-config.yaml`, so `buildRestConfig` falls back to
`clientcmd.RecommendedHomeFile`). Reasons, in order of weight:

1. **`internal/k8sagent/` emits zero Prometheus metrics.** Every W4 signal is a
   log line, so the agent's stdout is the instrument. A host process writes it
   to a file this rig already tails; an in-cluster Deployment puts it behind
   `kubectl logs` and a pod that restarts on crash.
2. **No image build, no `kind load`, no Deployment rollout.** W4-0 established
   there is no `kind` CLI on this machine, so an image would have to be pushed
   into `desktop-control-plane`'s containerd store by hand for every code
   change.
3. It is the route W4-0 already proved works end to end, minus the enrollment.

**W4-2's RBAC-denial arm needs an in-cluster agent and is DEFERRED to the W4-2
task, not dropped.** A host-run agent authenticates to the API server with a
kubeconfig this rig controls; it cannot be denied a verb the way a Pod's
ServiceAccount can, because there is no ServiceAccount in the picture at all.
Reproducing the denial requires a Pod bound to a Role that lacks the verb —
i.e. an image build plus a Deployment. That cost is charged to the task that
needs it, and W4-2 must budget for it. **Deferring is a decision; silently
running W4-2 without that arm is not acceptable.**

One consequence to carry forward: because the host agent holds the developer's
own cluster rights, **no W4 host-run capture is evidence about the RBAC the
shipped `manifests/` grant.** `podTemplate.reuse` works on this rig **because
the kubeconfig is privileged**; that observation is silent about the manifests
in both directions, and W4-2 depends on the distinction.

**Withdrawn claim, recorded because it was published in three documents.** An
earlier draft of this record asserted that "the shipped `manifests/` Roles lack
`pods update/patch`, so reuse is broken there", citing W4-0 and a cross-session
note. **That is false at HEAD, and W4-0 records nothing of the kind.**
`manifests/base/k8s-agent/rbac.yaml:7-13` grants
`create, get, list, delete, watch, update, patch` on `pods`, with a comment
naming pod reuse as the reason. The gap was real once and was fixed in **PR #50
(`6b0bf8f`, 2026-07-15)**; the note the claim came from carries a `RESOLVED`
header that did not travel with the claim. Stale-premise reuse of a
cross-session note is what forced W4-2 to be re-chartered; this is the same
failure, caught in review.

---

## Step 2 — The credential path. **It works, and nothing cross-checks it.**

This was the go/no-go: if anything on the request path compared
`enrollment_method`, the whole approach would have died.

**Code read.** `internal/controller/agent_auth.go:38-116` is the only
authenticator for `uca_` bearers. It checks, in order: the `uca_` prefix,
`agentauth.Parse`, `GetAgentCredentialForAuth(parsed.ID)`, then
`Kind == "access"`, `Status == "active"`, `RevokedAt == nil`, not expired, and
the token hash. **`enrollment_method` is never read.** The store query behind it
(`internal/store/postgres_agent_auth.go:272-276`) joins `agent_credentials` to
`agent_identities` and selects `status, authorized_labels,
authorized_capabilities` — not `enrollment_method`.

Across the tree the column is **compared** in exactly two places, both of them
credential *issuance*: `postgres_agent_auth.go:193` (the re-issue conflict
check) and `:526` (`WHERE enrollment_method = $1 AND external_subject = $2`,
the Kubernetes path's identity key, reached from `:237`). It is additionally
**selected** — and then only carried, never tested — by the three identity
getters (`:507`, `:516`, `:525`), by `insertExternalAgentIdentity`'s
`RETURNING` (`:544-557`), and read out for display by
`internal/controller/api_agent_enrollment.go:171` and
`internal/cli/agent_enrollment.go:302`. **Nothing on the request path compares
it, and the request path's own query does not even select it.**

**Measured live**, on the running HA stack (`step2-credential-path.txt`). One
`uce_` created with `--label kind:kubernetes`, exchanged for a `uca_`, whose
identity row reads:

```
$ psql -At -c "select agent_id, status, enrollment_method, authorized_labels, authorized_capabilities from agent_identities where agent_id='k8s-agent-w4';"
k8s-agent-w4|active|enrollment|{kind:kubernetes}|{}
```

That credential then drove every request path a k8s agent uses:

| Path | Result |
| --- | --- |
| `POST /api/v1/agents/register` with `capabilities:["pod","container"]` | **204** |
| `POST /api/v1/agents/{id}/heartbeat` | **204** |
| `POST /api/v1/agents/{id}/claim?timeout=2s` | **200** (empty claim) |
| `POST /api/v1/agents/{id}/runs/{zero-uuid}/steps/0/logs/bulk` | **400** `json: cannot unmarshal object into Go value of type []api.LogAppendRequest` — a *body-shape* error, i.e. authenticated and inside the handler |
| `POST /api/v1/agents/{id}/runs/{zero-uuid}/finish` | **404** `run not found` — likewise authenticated |
| `POST /api/v1/agents/token/refresh` | **200**, rotated pair returned |

Not one 401 or 403 anywhere. **Verdict: GO.**

### Capabilities — the thing the brief told us not to assume

**Capabilities cannot be, and are not, set on the enrollment path.** The
identity's `authorized_capabilities` stays `{}` and the enroll response carries
`"capabilities": null`. They arrive from the agent's **own self-report** at
register: `handleAgentRegister` (`internal/controller/api_agent.go:39-55`)
takes `req.Capabilities` verbatim after validating each against
`dsl.ValidCapability`, with the comment "capabilities are the agent's own
runtime auto-detection … not an authorization boundary". The k8s agent always
advertises `["pod","container"]` (`internal/k8sagent/agent.go:57-59`,
`k8sAgentCapabilities()`), so the right set arrives the moment it registers —
confirmed in the `agents` table:

```
      id      |      labels       |    capabilities    |     os
 agent1       | {kind:linux}      | {native,container} | linux
 agent2       | {kind:linux}      | {native,container} | linux
 k8s-agent-w4 | {kind:kubernetes} | {pod,container}    | windows/k8s
```

**Labels are the opposite and this asymmetry is load-bearing for every W4
fixture.** `handleAgentRegister` **ignores** the agent's requested labels and
uses `principal.AuthorizedLabels` — so the label the job selector must match is
the one put on the *enrollment token*, not the one in
`w4-agent-config.yaml`. The agent asks for `["kind:kubernetes","kubernetes"]`
(it appends a bare `kubernetes` at `agent.go:71`) and is registered with
`{kind:kubernetes}` alone.

---

## Step 3 — The interposer

`test/edgecase/tools/w4/enrollproxy/` — 427 lines of Go (`wc -l main.go`), no
dependencies outside the standard library. It:

- **forwards every path unchanged** via `httputil.ReverseProxy` with
  `FlushInterval: -1` (so SSE and any streamed response are not buffered) and
  no `ReadHeaderTimeout` (so the 30-60 s claim long poll is not cut short);
- **answers `POST /api/v1/agents/enroll`** with the canned
  `api.AgentTokenResponse` shape the k8s agent's `Token()` requires (`agentId`,
  `accessToken`, an `accessExpiresAt` in the future, `labels`, `capabilities`);
- **re-mints through the product's own `POST /api/v1/agents/token/refresh`**
  when the cached access token is inside `-min-remaining` (default 5 m),
  persisting the rotated pair back to the credentials file at 0600;
- **logs one `INTERCEPT #n` line per interception**, carrying the requested
  provider/policy/labels/capabilities and the served credential's *id prefix*
  (`uca_142df0c5…`) — never secret material.

### Proven by effect, not by inspection

**(a) The agent is up and registered through it** (`step3-w4-up.txt`,
`step3-bypass-by-effect.txt`):

```
2026/08/01 15:20:51 INTERCEPT #1 POST /api/v1/agents/enroll from=127.0.0.1:63060
  provider="kubernetes" policy="w4-spike-policy" labels=[kind:kubernetes]
  caps=[pod container] -> 200 agentId=k8s-agent-w4 cred=uca_142df0c5...
{"level":"INFO","msg":"k8s agent registered","agentId":"k8s-agent-w4","labels":["kind:kubernetes","kubernetes"]}

$ unified-cli agent list
agent2         55d43218e4fa    linux         kind:linux
agent1         3737888bc9c7    linux         kind:linux
k8s-agent-w4   DESKTOP-EMUF6H6 windows/k8s   kind:kubernetes
```

**(b) The bypass is necessary — the control, taken on the same stack at the
same moment** (`step3-control-403.txt`). One pod-bound token, one request body,
two ports:

```
-- direct to the LB :18080 (no interposer) --
http_code=403
enrollment policy rejected
-- through the interposer :18099 (byte-identical request) --
http_code=200
```

The 403 is W4-0's defect reproducing live on this very stack, which is what
makes the interposer's necessity a measurement rather than a citation.

**(c) It is transparent on every other path** — `GET /healthz` → 200 and
`GET /api/v1/agents` with an admin PAT → 200, both through :18099.

**(d) It answers repeatedly, and re-mints when asked to**
(`step3-repeat-and-remint.txt`). Three consecutive enroll calls against a
throwaway identity with `-min-remaining 24h`, which forces the refresh path
every time:

```
INTERCEPT #1 ... -> 200 agentId=k8s-agent-w4-probe cred=uca_1736c175... remounted=true
INTERCEPT #2 ... -> 200 agentId=k8s-agent-w4-probe cred=uca_34cf2d91... remounted=true
INTERCEPT #3 ... -> 200 agentId=k8s-agent-w4-probe cred=uca_0353528c... remounted=true
```

Three distinct credential ids, all issued by the real controller.

### Correction to the brief: "the k8s agent re-enrolls on 401" is only half true

`internal/agent/client.go:96-100` calls `source.Invalidate()` **only when
`method == http.MethodGet`**. The k8s agent's traffic is overwhelmingly POST
(claim, heartbeat, steps, logs, finish), and those surface a 401 without
invalidating the cached credential. The **dominant** driver of repeated
enrollment is not the 401 path at all: it is the proactive refresh in
`KubernetesCredentialSource.Token()` (`internal/k8sagent/credentials.go:83-84`),
which re-enters the exchange once the cached token is within
`kubernetesTokenRefreshLeadTime` (15 min) plus up to 5 min of jitter of
expiry. With the controller's 1 h `agentAccessTTL`, that is roughly every
40-45 min of agent uptime. The interposer must answer repeatedly either way,
so the requirement is unchanged — but a scenario expecting a re-enrollment to
follow a 401 on a POST will wait forever.

---

## Step 4 — Fixtures

All four verified through the real `dsl.Parse` (`KnownFields(true)` +
`Job.Validate`) via `tools/w3/fixcheck`, **twice**: once on the `.yaml` source
and once on the YAML re-extracted from the `.payload.json`, per the README rule.

> **Provenance, because the first version of this section got it wrong.** The
> original capture (`w4-2/step4-fixcheck.txt`) covers **three** fixtures on each
> side — `w4-longpod` was validated live but never captured, and this record
> pasted its output under "Output, verbatim" anyway. That was a fabricated
> citation, caught in review. The block below is the **re-run**, covering all
> four on both sides, captured in full at
> **`w4-2-fixes/f1-fixcheck.txt`**. The longpod lines are unchanged from what
> the earlier draft claimed — the claim was true; it was the evidence that did
> not exist.

Output, verbatim (`w4-2-fixes/f1-fixcheck.txt`):

```
=== fixcheck on the .yaml sources ===
test/edgecase/workloads/w4-tick.yaml: OK
  name="edge-w4-tick" native=false agentSelector=[kind:kubernetes] requiredCaps=[container]
  step[0] name="probe" kind=run
------------------------------------------------------------
test/edgecase/workloads/w4-pending.yaml: OK
  name="edge-w4-pending" native=false agentSelector=[kind:kubernetes] requiredCaps=[pod]
  step[0] name="probe" kind=run
------------------------------------------------------------
test/edgecase/workloads/w4-reuse.yaml: OK
  name="edge-w4-reuse" native=false agentSelector=[kind:kubernetes] requiredCaps=[container]
  step[0] name="identity" kind=run
  step[1] name="marker" kind=run
------------------------------------------------------------
test/edgecase/workloads/w4-longpod.yaml: OK
  name="edge-w4-longpod" native=false agentSelector=[kind:kubernetes] requiredCaps=[container]
  step[0] name="tick" kind=run
------------------------------------------------------------
=== fixcheck on YAML RE-EXTRACTED from the .payload.json (README rule) ===
.rt2/w4-tick.yaml: OK
  name="edge-w4-tick" native=false agentSelector=[kind:kubernetes] requiredCaps=[container]
  step[0] name="probe" kind=run
------------------------------------------------------------
.rt2/w4-pending.yaml: OK
  name="edge-w4-pending" native=false agentSelector=[kind:kubernetes] requiredCaps=[pod]
  step[0] name="probe" kind=run
------------------------------------------------------------
.rt2/w4-reuse.yaml: OK
  name="edge-w4-reuse" native=false agentSelector=[kind:kubernetes] requiredCaps=[container]
  step[0] name="identity" kind=run
  step[1] name="marker" kind=run
------------------------------------------------------------
.rt2/w4-longpod.yaml: OK
  name="edge-w4-longpod" native=false agentSelector=[kind:kubernetes] requiredCaps=[container]
  step[0] name="tick" kind=run
------------------------------------------------------------
```

All four `POST /api/v1/jobs` → **200** live, also re-run and captured
(`w4-2-fixes/f1-jobs.txt`); the original Step 6 capture
(`w4-2/step6-baseline.txt:2-4`) shows only three, because `edge-w4-longpod` was
created later, for Step 7:

```
POST /api/v1/jobs w4-tick.payload.json    -> 200
POST /api/v1/jobs w4-pending.payload.json -> 200
POST /api/v1/jobs w4-reuse.payload.json   -> 200
POST /api/v1/jobs w4-longpod.payload.json -> 200
--- the four job rows the controller now holds ---
  edge-w4-longpod
  edge-w4-pending
  edge-w4-reuse
  edge-w4-tick
```

### Two things about the fixtures the brief did not anticipate

**1. `requiredCaps` is `container`, not `pod`, for three of the four.**
`dsl.RequiredCaps` (`internal/dsl/capabilities.go:24-33`) returns `pod` only
when `PodTemplateNeedsKubernetes(spec.PodTemplate)` — a bare `containers:` list
does not qualify; `w4-pending`'s `nodeSelector` does. So `edge-w4-tick`,
`edge-w4-reuse` and `edge-w4-longpod` are capability-schedulable on the Linux
agents too, and are kept off them **only by the `kind:kubernetes` label**.
Do not treat a `podTemplate` as implying the `pod` capability.

**2. `edge-w4-pending` is a copy under a NEW `metadata.name`, deliberately.**
The brief said to copy the file rather than edit `podcap-job.payload.json` in
place, because `edge-podcap-job` must stay claimable for W2-4 Part D
(`w2-4-queued-reaper.md:207`). Copying the *file* is not sufficient: a payload
that kept `metadata.name: edge-podcap-job` would **overwrite the same job row**
on `POST /api/v1/jobs` and break that premise just as thoroughly. The fixture
is therefore `edge-w4-pending`. `podcap-job.payload.json` is untouched.

**3. The primary container must be named `job`, and nothing tells you so until
runtime.** The first baseline attempt used `name: main` (copied from
`podcap-job.payload.json`) and the run **Failed** with

```
unified-cd: step "probe" failed to execute: unable to upgrade connection:
container job is not valid for pod ucd-run-9c2e41c0-cc65-45
```

`dsl.PrimaryContainerName` is `"job"` (`internal/dsl/container.go:23`) and the
executor falls back to it for any step with no `container:`. A podTemplate that
supplies its own `containers:` and does not include one named `job` parses,
validates, schedules, and builds a Pod — and then cannot execute a single step.
Filed as a FINDINGS observation; `podcap-job.payload.json` has exactly this
shape and has never been executed.

**`sidecarS3SecretName` is unset** in `w4-agent-config.yaml`, so cache steps are
silent no-ops reported `Succeeded` and artifact steps **fail**. No W4 fixture
depends on artifacts, and none may.

---

## Step 5 — The k8s fault-injection verbs

`test/edgecase/tools/w4/w4-k8s-inject.sh`. This is the first k8s fault tooling
in the repo. `tools/inject.sh` is useless here by construction: its verbs take
compose **service names** and hardcode `unified-cd-ha_default` /
`unified-cd-ha-$svc-1`, and the k8s agent is neither a compose service nor a
container on that network.

| Verb | Effect measured in |
| --- | --- |
| `pods` | `step5-annotations-reuse.txt` |
| `delete-pod <runId\|latest>` | `step7-delete-pod.txt` |
| `annotations [pod]` | `step5-annotations-reuse.txt` |
| `block reset` / `unblock` | `step5-block-recheck.txt` (post-fix), `step5-block-verb.txt` (pre-fix) |
| `block <status>` | `step5-block-verb.txt`, `block 503` section |
| `block hang` | **`w4-2-fixes/f5-hang.txt`** — shipped unmeasured; measured at review |
| `show` / `probe` | `step5-block-verb.txt` |
| the arm assertion itself (all three modes + a negative control) | `w4-2-fixes/f5-hang-assert.txt` |

### `block` — the agent→controller partition, and why it is not `nginx-block`

`inject.sh nginx-block` resolves the target agent's **source IP** with
`docker inspect` on its compose container and denies it at the LB. A host-run
agent has no compose container, and its traffic reaches nginx from the same
Docker-host address as every `curl` the scenario itself makes — an IP deny
would cut the instrument along with the subject. The blunt substitute (SIGKILL
all three controllers) fails in the other direction: it stops the controller's
own timers — reapers, archiver, scheduler — which are precisely what W4-1 needs
still running while the agent is isolated.

The interposer already sits alone in front of this one agent, so arming it is
both **surgical** and **one-way**: the controller keeps running and keeps
serving `agent1`/`agent2` and the admin API. The switch is a file
(`.w4run/block.arm`), the same idiom as `inject.sh`'s `$S3FAULT_DIR` arm files;
its content selects `reset` (hijack and close — a transport failure with no
status code, the closest analogue to a partition), `hang`, or a 3-digit status.

**The first version of this verb was a silent partial no-op, and only an effect
measurement caught it — the W3-1 `s3-slow` lesson repeating.**
Armed at 15:26:27 with every state check passing (`curl_exit=52`, control
`:18080` still 200, `BLOCK #n` lines appearing), the agent nonetheless
**claimed and ran `edge-w4-tick` at 15:26:44**, 17 s into the armed window
(`step5-block-verb.txt`, EFFECT 3). Its claim long poll (`?timeout=30s`) had
entered the handler *before* the arm and was already proxying, so the
per-request check never saw it. Fixed by tracking live connections via
`http.Server.ConnState` and severing them on the arm transition:

```
2026/08/01 15:28:24 BLOCK-ARM severed 3 in-flight connection(s)
```

Re-measured (`step5-block-recheck.txt`): a run triggered inside the armed
window stayed **`Queued` for the full 40 s** with **no pod created**, against
56 `BLOCK #n` lines; `unblock` → claimed and `Succeeded` within 6 s.

Two supporting measurements, **both taken in the PRE-fix window**
(`step5-block-verb.txt`), not in the recheck. Both are unaffected by the defect
that window contains — it spared only requests already inside the handler, and
neither of these is one — but the citation must name the window it came from:

- **The partition is one agent wide.** `step5-block-verb.txt` EFFECT 4: with
  the k8s agent's `lastSeenAt` frozen at `06:26:21Z`, `agent1`/`agent2` kept
  heartbeating (`06:26:42Z`). The k8s agent's own heartbeat is visible being
  refused in the same file (`BLOCK #2 POST .../heartbeat`).
- **`block 503` behaves as documented** — `step5-block-verb.txt`, the
  `block 503` section: `http_code=503` at the probe, while the direct control
  against `:18080` stayed 200.

`step5-block-recheck.txt` contains neither of these (`grep 06:26` does not
match it); it holds only the 40 s `Queued` re-measurement above.

### The `hang` arm — shipped unmeasured, measured at review

**This arm originally shipped with a comment describing what it does and no
capture measuring it** — the exact pattern the house rule exists for, from the
same author whose `block` arm had already shipped inert once in this session.
It has now been measured. Capture `w4-2-fixes/f5-hang.txt`, on a freshly
rebuilt rig (`w4-2-fixes/stack-up.txt`, `rig-up.txt`).

**It works, and it is a different failure shape from `reset`, not a synonym.**

| | `reset` | `hang` |
| --- | --- | --- |
| probe result | `http_code=000 curl_exit=52` in **1.5 ms** | `http_code=000 curl_exit=28` after the **full 5 s** deadline |
| what the agent sees | `wsarecv: connection forcibly closed` | `context deadline exceeded (Client.Timeout exceeded while awaiting headers)` |
| run triggered while armed | `Queued` 40 s, no pod | `Queued` 40 s, no pod |
| recovery after `unblock` | claimed and `Succeeded` **within 6 s** | claimed only on the **24th** 2 s sample |

The 40 s armed window (`f5-hang.txt`): `Queued` on all 40 samples, `pods=[]`
throughout, 11 `BLOCK … mode=hang` lines covering both `heartbeat` and `claim`,
and the one-agent-wide control holding — `k8s-agent-w4` frozen at
`lastSeen=07:18:51Z` while `agent1`/`agent2` reported `07:20:08Z`, with the
direct `:18080` control still 200. After `unblock` the run reached
**`Succeeded`** and its pod was cleaned up (read back at `07:21:47Z`).

**Two properties a scenario author must budget for, both measured here:**

1. **`hang` costs a scenario its recovery latency.** `unblock` does *not* sever
   the hanging requests — `watchArm` severs only on the transition *into* an
   armed state — so the agent stays stuck on its own client timeout after the
   arm is cleared. Measured: `reset` recovered in ~6 s, `hang` took ~24 samples
   at a 2 s poll. Do not use `hang` for an arm whose window must close sharply.
2. **The first probe after arming `hang` reports `curl_exit=52`, not 28.** The
   arm transition severs every live connection, and a probe issued inside that
   ~200 ms window is severed along with them (`f5-hang.txt`, first ARM). The
   verb now settles past one `watchArm` tick before probing, so what it asserts
   on is the steady-state arm rather than the transition.

**The arm is now asserted, not merely printed.** `probe_proxy` previously ended
in a no-op `if … then : fi` — dead code shaped like a verification — so an
interposer started without `-block-file` would have printed `ARMED` and exited
0. `block` now exits non-zero unless the probe fails in the shape the mode
promises (`28` for `hang`, the exact status for `<status>`, any transport
failure for `reset`), and `unblock` requires a 200. Verified in all three modes
**and against a negative control** — a second interposer started on `:18098`
with no `-block-file`, where the arm file is ignored
(`w4-2-fixes/f5-hang-assert.txt`):

```
w4-k8s-inject: agent->controller partition ARMED (mode=reset)
  probe GET /healthz via 127.0.0.1:18098: http_code=200 time=0.002401s curl_exit=0
w4-k8s-inject: FAILED to arm 'reset' — probe still answered http_code=200. ...
   exit=1
```

That is the check that would have caught the original inert `block` at arm
time, instead of 17 s into a scenario.

### `delete-pod` and `annotations`

`delete-pod` deletes by the `unified-cd/runId` label
(`internal/k8sagent/podbuilder.go:78-81`), not by name, so a scenario can act on
a run id it already has without re-deriving the `ucd-run-<first 16 chars>`
truncation. It prints the pod list before and after; the before/after pair in
`step7-delete-pod.txt` shows the target going `Running` → `Terminating` while
a second, unrelated pooled pod is untouched.

`annotations` reads `unified-cd/pool-status`, `pool-key`, `pool-run-id` and
`pool-template` (`internal/k8sagent/pool.go:20-31`). Measured on a live pooled
pod after a `podTemplate.reuse` run released it:

```
== ucd-run-763f7138-cdcb-45 ==
  pool-status  = idle
  pool-key     = 3c1d2612f14e2dd35233f878a28578b5
  pool-run-id  =
  pool-template=
  label runId  = 763f7138-cdcb-45c1-a1f0-ad41671a2f32
  phase        = Running
```

### The reuse fixture works, and it exposes a naming trap

`edge-w4-reuse` was run three times. All three executed in the **same pod**,
and the marker survived across them:

```
run A:  W4-REUSE-HOSTNAME=ucd-run-763f7138-cdcb-45   MARKER=MISS  (first)
run B:  W4-REUSE-HOSTNAME=ucd-run-763f7138-cdcb-45   MARKER=HIT planted=06:29:47Z
run C:  W4-REUSE-HOSTNAME=ucd-run-763f7138-cdcb-45   MARKER=HIT planted=06:29:59Z
```

Both observation channels the brief asked for agree: **pod-name identity** and
**marker persistence**. Note the trap it exposes — the pod's name **and** its
`unified-cd/runId` label both carry the **first** run's id forever. Under
`reuse`, neither identifies the run currently executing; only
`unified-cd/pool-run-id` does, and it is cleared on release. A scenario that
locates "the pod for run X" by label will find nothing for runs B and C.

---

## Step 6 — End-to-end baseline

`step6-baseline.txt`. Every limb passed:

```
POST /api/v1/jobs w4-tick -> 200 ; w4-pending -> 200 ; w4-reuse -> 200
--- pods in ci BEFORE trigger --- (only the W4-0 spike pod)
runId=37926cdc-7107-411f-83bf-9970dd274fd7
06:23:19 t=01s status=Queued     pods=<none>
06:23:20 t=02s status=Running    pods=ucd-run-37926cdc-7107-41  0/2  Init:0/1
06:23:22 t=03s status=Running    pods=ucd-run-37926cdc-7107-41  2/2  Running
06:23:23 t=04s status=Succeeded  pods=ucd-run-37926cdc-7107-41  2/2  Terminating
--- cleanup: 3 samples over 6s ---
sample 1: ucd-run-37926cdc-7107-41 2/2 Terminating
sample 2: <none>
sample 3: <none>
--- run logs ---
"w4-tick-ran"
"W4-POD-HOSTNAME=ucd-run-37926cdc-7107-41"
```

Agent enrolled via the interposer and claiming ✓ · trivial job `Succeeded` on
the k8s agent ✓ · pod created ✓ · pod cleaned up ✓. Total wall time from
trigger to terminal: **4 s**; pod gone within **6 s** of that.

**No limb failed, so no downstream task is blocked by this rig** — except
W4-2's RBAC arm, which is blocked by the deferred route decision above, not by
a failure here.

---

## Step 7 — What happens when a run's pod is deleted mid-run

**Answer: the run reaches `Failed`, essentially immediately (~1 s), through the
ordinary step-error path — NOT by sitting `Running` until the controller's
90 s stuck-run reaper.** Of the brief's two plausible readings, the first is
correct.

Capture `step7-delete-pod.txt` / `step7-detail.txt`. `edge-w4-longpod` (120 s of
1 Hz ticks) was triggered, allowed to produce four log lines, then had its pod
deleted at `06:30:45Z`:

```
-- before --
ucd-run-898c342e-339a-45   2/2   Running   0   7s
pod "ucd-run-898c342e-339a-45" deleted from ci namespace
-- after --
ucd-run-898c342e-339a-45   2/2   Terminating   0   7s

  *** t=+1s STATUS -> Failed
TERMINAL STATUS = Failed  after 1s
```

The mechanism is visible in the step row:

```
 run_id                               | step_index | status | exit_code | started_at            | ended_at              | step_name
 898c342e-339a-45d3-b456-cf297c988750 |          0 | Failed |       137 | 06:30:42.463647+00    | 06:30:45.681615+00    | tick
```

`exit_code = 137` is `128 + SIGKILL`: deleting the Pod killed the container and
`agentlib.RunClaim` treated the result as an ordinary non-zero step exit and
finished the run `Failed`. The run's `updated_at` is `06:30:45.779503Z` —
**98 ms** after the step ended. The controller's stuck-run reaper never entered
the picture.

*(Labelled: "the exec stream returned that status" is **inferred**, from the
recorded `exit_code` plus the shape of `internal/k8sagent/executor.go` — the
exec stream is the only thing that supplies a step exit code on this path.
**No agent log line records it**; this entry's own second finding is that the
agent logged nothing at all for the event. What is measured is the `137` in
the `steps` row, not the mechanism that carried it.)*

**Three things this settles for W4-1, all of them measured:**

1. A deleted pod produces a *fast, terminal* `Failed`, so a W4-1 arm that
   expects a run to hang after pod loss is wrong.
2. The evidence recorded is **indistinguishable from a step that exited 137 on
   its own** (an OOM-kill, say). There is no `error` column populated, no
   `stderr` line in the run's log — the run log's five rows are the step's own
   `W4-LONGPOD-BEGIN` and `TICK 0..3` and nothing else — and **the agent logged
   nothing at all**: the last two lines in `k8s-agent.log` for that run are the
   routine `executing Run` and `running`. Filed as a FINDINGS observation.
3. `agent.go:340-349` having no re-create path and `podgc.go:51-52`'s
   "pod-per-run does not resume" are both consistent with this — but neither is
   what produces the status; the step exit code is.

**The limits of this measurement, stated because the answer is load-bearing
for W4-1:**

- **n = 1.** One deletion, one run. Nothing here establishes a rate, a
  distribution, or that the timing holds under a busier agent.
- **The delete marker has 1 s resolution.** It is a
  `date -u +%Y-%m-%dT%H:%M:%SZ` stamp printed by the capture script
  immediately before `kubectl delete` — `06:30:45Z` means "somewhere in that
  second", while the step's `ended_at` is `06:30:45.681615Z`. The two are
  within the same second and cannot be ordered to sub-second precision from
  this capture.
- **Causality rests on the fixture, not on the timestamps.** `edge-w4-longpod`
  is a 120 × 1 s loop, so it cannot reach a terminal exit 3.2 s in without an
  external cause; that, plus the `137`, is what makes this a deletion effect
  rather than a coincidence. It is not bare correlation, and it is also not a
  sub-second ordering proof.
- **On the "~1 s":** the status poll interval was 2 s and the transition was
  seen on the first sample after the delete. The precise figure that *is*
  measured is the 98 ms from step end to run `updated_at`.

---

## Bring-up

Two commands. The compose stack first:

```bash
docker compose -f test/ha/docker-compose.ha.yaml \
               -f test/edgecase/compose/k8senroll.override.yaml up -d --build
```

The `k8senroll` overlay is **not** required by the interposer — it exists so the
403 control above can be taken on the same stack. A scenario that does not need
that control can bring up plain `test/ha/docker-compose.ha.yaml` and skip the
kubeconfig entirely. If you do use it, regenerate the (gitignored) kubeconfig
first with `test/edgecase/k8s/make-spike-kubeconfig.sh`; its SA token lasts 24 h.

> **Gotcha, hit live while re-running these measurements.** If the kubeconfig
> is absent, the overlay's bind mount makes Docker **create a directory** at
> `test/edgecase/compose/kubeconfig-k8senroll.yaml`, and **all three
> controllers exit(1)** with
> `error loading config file ".../kubeconfig.yaml": read ...: is a directory`.
> That looks like the bootstrap-PAT race and is not: the race kills one
> replica, this kills all three, and the empty directory persists to poison the
> next `up` until it is `rmdir`ed. Either regenerate the kubeconfig first, or
> — if you do not need the 403 control — use the plain `test/ha` compose file,
> which is what these re-run measurements used.

Then the agent rig:

```bash
test/edgecase/tools/w4/w4-up.sh
```

which builds `k8s-agent` and `enrollproxy` into a gitignored `bin/`, mints a
credential, writes the dummy SA-token file, starts the interposer on
`127.0.0.1:18099`, starts the agent, and waits for `k8s agent registered`.
Logs and pidfiles land in `.w4run/`.

**Check all three controllers are `Up`.** W4-0 recorded a bootstrap-PAT race
that kills one replica on a cold `up`. **It did not fire on either of this
rig's cold bring-ups** — 3/3 both times, on the first `up -d --build` after a
`down -v` (`w4-2/stack-up.txt`, `w4-2-fixes/stack-up.txt`) — so it is a
**race, not a certainty**. `w4-0-enrollment-spike.md` item 6 has been corrected
at source to say so. Verify, do not assume in either direction.

## Teardown

```bash
test/edgecase/tools/w4/w4-down.sh [capture-dir]
docker compose -f test/ha/docker-compose.ha.yaml \
               -f test/edgecase/compose/k8senroll.override.yaml down -v
```

`w4-down.sh` SIGTERMs the agent first (so its graceful-drain path runs),
escalates to SIGKILL after 10 s, prints each process's final 25 log lines, and
reports how many enrollment exchanges the interposer answered.

**Read that count the way the script does.** `0` means **no enrollment was
intercepted while these logs were being written** — it does *not* mean the
bypass was not in effect. The agent enrolls once at startup and then not again
for roughly 40-45 min (`internal/k8sagent/credentials.go:83-84`), so a short
session legitimately reports 0. The supporting evidence for any claim resting
on the rig is the **`INTERCEPT` line at the agent's own startup**, wherever it
landed. It fired exactly this way on this task: `step8-teardown.txt:56` reports
`0 enrollment exchange(s)` for a session whose bypass demonstrably *was* in
effect — the startup `INTERCEPT #1` was in a proxy log the restart had already
truncated (see `w4-up.sh`'s log rotation, added for this reason). The count is
unsupported only if **no** log in the directory carries an `INTERCEPT` line at
all.

`down -v` is mandatory between scenarios (Garage volume).

The W4-0 spike objects (`w4-spike-agent` SA, `w4-spike-agent-pod`, controller
RBAC) are left in place, as W4-0 left them.

**If a scenario clears the `ci` namespace it destroys `w4-spike-agent-pod`, and
recreating it mints a NEW Pod UID.** W4-2 did exactly this and recreated the Pod
from `test/edgecase/k8s/w4-spike-identity.yaml` (`w4-2b/teardown.txt`: SA
`unchanged`, Pod `created`, `1/1 Running`). Nothing is broken by that — spike
tokens are re-minted per rig-up, and the controller's Kubernetes enrollment
verifier binds a token to the Pod UID it was issued for
(`internal/controller/agent_enrollment_kubernetes.go:100` compares
`pod.UID` against the token's `claims.Pod.UID`, and `:103` carries `PodUID` into
the resulting identity). But **any downstream artifact that pinned the old UID —
a captured token, a recorded identity, a hand-written policy fixture — is now
stale and will be rejected as a `pod UID mismatch`.** Re-mint rather than reuse.
This is a non-defect note; no finding rests on it.

## Credential hygiene

`uca_`/`ucr_`/`uce_` material, ServiceAccount tokens and kubeconfigs are all
gitignored and never printed in full: the mint script and the interposer log
only a 4-character kind prefix or the credential's UUID prefix. The
`serviceAccountTokenFile` this rig writes is a dummy string, not a token — the
interposer never inspects the Bearer. The whole session's captures were swept
for `uca_`/`ucr_`/`uce_` followed by a UUID before archival.

---

## Review corrections applied to this record

A task-scoped review found the rig itself sound and its verdict intact, and
found the **evidence discipline** wanting: three cited captures did not contain
what they were cited for, one "verbatim" paste existed in no capture, and one
claim was false at HEAD. Everything below was applied on top of `2d353eb`.
Read this before citing any number in this record.

| # | What was wrong | What was done |
| --- | --- | --- |
| F1 | The Step 4 "Output, verbatim" block pasted a `w4-longpod` fixcheck result that appeared in **no capture**, and claimed "all four `POST /api/v1/jobs` → 200" against a capture showing three | Both **re-run and captured** on a rebuilt rig: `w4-2-fixes/f1-fixcheck.txt` (4 fixtures × 2 forms), `w4-2-fixes/f1-jobs.txt` (4 × 200). The original claims were true; the evidence did not exist |
| F2 | Both reader-facing documents still carried the retracted "a count of 0 means the bypass was never in effect" | Replaced with the script's own semantics, in `w4-rig.md` §Teardown and `README.md` |
| F3 | `w4-up.sh` truncated `enrollproxy.log`, destroying the startup `INTERCEPT` line that `w4-down.sh` counts — the cause of the misleading `0` in `step8-teardown.txt` | `w4-up.sh` now **rotates**; `w4-down.sh` prints a per-file breakdown. Proven by effect this session: `3 intercepted — 1 in enrollproxy.log, 2 in enrollproxy.log.1785568655` |
| F4 | Two supporting measurements were attributed to `step5-block-recheck.txt`; both live in the **pre-fix** `step5-block-verb.txt` | Citations split and each window named, in `w4-rig.md` §Step 5 and the FINDINGS entry |
| F5 | The `hang` arm shipped with a comment and **no effect measurement** | **Measured**: `w4-2-fixes/f5-hang.txt`. It works, and it is not a synonym for `reset` — `unblock` does not sever hanging requests, so recovery costs ~24 × 2 s samples vs `reset`'s 6 s. The verb now **asserts** its arm, verified in all three modes plus a negative control (`f5-hang-assert.txt`) |
| F6 | The `container job is not valid for pod` line and a pod phase history were cited to a capture since overwritten | Re-cited to `w4-2/final-logs/k8s-agent.log:5`; the phase history was never captured and is **dropped** |
| F7 | "the shipped `manifests/` Roles lack `pods update/patch`" — unevidenced **and false at HEAD** | **Deleted and replaced with the verified fact** (`manifests/base/k8s-agent/rbac.yaml:7-13`; fixed in PR #50, `6b0bf8f`). The surrounding point — no host-run capture is evidence about shipped RBAC — is kept, because W4-2 depends on it |
| F8 | "the column is read in exactly three places … and nowhere else" — a false enumeration in three documents | Corrected to *compared* in two (`:193`, `:526`), *selected* by the getters and the enrollment API/CLI, never used to gate a request. The GO decision is unchanged |
| — | The W4-0 over-claim correction was filed where nothing was wrong | The over-claiming sentence is `w4-0-enrollment-spike.md` item 6 and has been **fixed at source**; the FINDINGS entry is reduced to a frequency datapoint |
| — | Step 7's causality held but its limits were unstated | **n = 1** and the delete marker's **1 s resolution** are now stated, in both documents |

Seven minor corrections also applied: the interposer's line count (~250 → 427),
"all five request paths" → six, an explicit `+09:00` vs UTC note (F4 was hard to
spot without it), the mint script's actual client (raw `curl`, not
`unified-cli`), `probe_proxy`'s no-op `if … then : fi` replaced by a real
assertion, the block-inert entry relabelled `n/a (campaign asset)` per the
precedent at `FINDINGS.md:1110`/`:1270`, and "the exec stream returned that
status" labelled **inferred**.

Captures for this pass are under `w4-2-fixes/`. The rig was brought up on the
plain `test/ha` compose file, measured, and torn down: `stack-up.txt`,
`rig-up.txt`, `teardown.txt`, `stack-down.txt`, `final-logs/`. No stray
process or container was left behind, and the captures were swept for
credential material.
