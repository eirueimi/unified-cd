# Job

## Job

The primary unit of work. See [Job Reference](../writing-jobs/index.md) for the full feature guide.

```yaml
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: <string>                  # required
  labels:                         # optional
    <key>: <value>
  annotations:                    # optional
    <key>: <value>
spec:
  params:
    inputs:
      - name: <string>                    # required
        type: string | bool | int | array # required
        required: <bool>
        default: <any>
        description: <string>
    outputs:
      - name: <string>
        type: string | bool | int | artifact
  agentSelector:
    - <label>                     # e.g. "kind:linux"
  concurrency:
    mutex: <string>
    semaphores:
      - pool: <string>
        capacity: <int>
    orLocks:
      - name: <string>
        in:                       # list of candidate values, or a $param expression
          - <string>
  timeoutMinutes: <number>
  native: <bool>                  # true = host-process job, no containers, host-agent only
                                   # (mutually exclusive with podTemplate and step container:)
  podTemplate:                    # sidecar containers for an isolated job (default when native
                                   # is unset/false); full pod config is k8s-agent only, the
                                   # standard agent reads only spec.containers to build its claim
                                   # pod (see Kubernetes Integration Guide)
    name: <string>
    spec: <PodSpec map>
    workspace:
      mountPath: <string>
      pvc:
        claimName: <string>
        storageClassName: <string>
        storageRequest: <string>
        accessMode: ReadWriteOnce | ReadOnlyMany | ReadWriteMany
    reuse: <bool>
    cleanWorkspace: <bool>
    override:
      containers: [<map>]
      volumes: [<map>]
  steps:
    - name: <string>              # required, unique within the job
      if: <CEL expression>        # e.g. params.env == "production"; see user-guide/writing-jobs/expressions.md
      env:
        <KEY>: <value>            # supports {{ secrets.NAME }} and {{ .Params.X }}
      run: <shell script>
      outputs:
        <key>: <template expression>
      call:
        job: <job-name>
        with:
          <key>: <value>
      uses:
        job: git://<host>/<owner>/<repo>/<path>@<ref>   # target must be kind: JobTemplate (see below)
        with:
          <key>: <value>
      cache:
        path: <string>
        key: <string>
        restoreKeys: [<string>, ...]
        ttlDays: <int>            # default: 30
      uploadArtifact:
        name: <string>
        path: <string>
      downloadArtifact:
        name: <string>
        destDir: <string>         # default: current directory
      post:
        run: <shell script>
        env:
          <KEY>: <value>
      container: <string>          # exec into a named podTemplate container instead of the primary
      continueOnError: <bool>     # default: false
      timeoutMinutes: <number>
    - parallel:                   # OR: a group of steps that run concurrently; see user-guide/writing-jobs/steps.md
        - name: <string>          # ("Concurrent Steps (parallel)")
          run: <shell script>
  finally:                        # optional — same structure as steps; see user-guide/writing-jobs/approval-and-finally.md#finally-block-finally
    - name: <string>
      run: <shell script>
```

### Job isolation: `native` and `container:`

Every job is isolated by default on both agents — see [Job Isolation:
`native` and the claim pod](../writing-jobs/isolation-and-containers.md#job-isolation-native-and-the-claim-pod)
in the Job Reference for the full model (claim pod construction, supported
runtimes, sidecar behavior). The schema-level surface is small:

| Field | Behavior |
|---|---|
| `spec.native` | `true` opts the whole job out of isolation: every step runs as a host process, exactly like pre-isolation behavior. Host-agent only (a k8s-agent fails a `native: true` claim fast). Mutually exclusive with `podTemplate` and any step `container:` (apply-time errors). |
| `podTemplate` | Sidecar container definitions for an isolated job. Full PodSpec is k8s-agent only; the standard agent reads `spec.containers` (name/image/env/`resources.limits`) to build its claim pod — see [Kubernetes Pod Template (`podTemplate`)](../writing-jobs/isolation-and-containers.md#kubernetes-pod-template-podtemplate) in the Job Reference. |
| `step.container` | Exec into a named `podTemplate` container instead of the job's primary container. Requires a `podTemplate` defining that container name (checked at apply time for isolated jobs). This is the **canonical** field for targeting a container — the old step-level `runsIn: { image / container }` is **removed**. |

Resource limits for a `podTemplate` container (previously `runsIn.resources`)
now live directly on the container definition, matching Kubernetes:
`podTemplate.spec.containers[].resources.limits`. The standard agent applies
CPU/memory limits from there the same way k8s does.

The **uses-level** `runsIn.image` (an isolated "scope" spanning an entire
inlined `uses:` template) is a separate, unaffected code path — see the next
section.

#### Uses-level `runsIn.image` (scope)

- **Uses-level** `runsIn.image` (on a `uses:` step): **scope mode**. The whole
  inlined template runs inside **one** isolated environment (a "scope") that
  stays alive for all of the template's steps, instead of each inlined step
  running against the outer job's environment.
- **Uses-level** `runsIn.container` is rejected (a parse error) — target a
  named container from the template's own steps with `container:` instead.
- A `uses` step without `runsIn`: unchanged current inlining behavior
  (inlined steps run in the outer job's environment, isolated or native
  depending on the job).

#### Uses-level scope: artifacts & cache in the isolated environment

When a `uses:` step declares `runsIn.image`, every step inlined from that
template shares one isolated scope environment (one container on the standard
agent, one dedicated pod on k8s). `cache`, `uploadArtifact`, and
`downloadArtifact` steps inside that scope operate on **the scope's own
filesystem**, not the outer job workspace — so a template that builds
something in its isolated environment can save/restore that output as an
artifact or cache entry without ever touching the outer workspace.

The scope does not share the outer job workspace; it starts from a fresh,
empty filesystem:

- **Inputs** enter the scope via `with:` (environment variables, as with any
  `uses`) and `downloadArtifact` (which writes into the scope's filesystem).
- **Outputs** leave the scope via `uploadArtifact` (pushed to the run's
  artifact store) and `outputs:`/stdout.

Because artifacts are keyed by run, not by workspace path, they cross the
isolation boundary naturally — on Kubernetes this means no shared
`ReadWriteMany` volume is required for a scoped `uses`.

```yaml
steps:
  - name: build-in-container
    uses:
      job: git://github.com/my-org/ci-templates/jobs/build.yaml@v1
      with:
        target: ./cmd/server
      runsIn:
        image: golang:1.22
```

If `build.yaml` contains a `download-deps` step with `cache:`, a `compile`
step with `run:`, and a `save-binary` step with `uploadArtifact:`, all three
run inside the same `golang:1.22` scope: the cache is restored into and saved
from the scope's filesystem, the compile step writes into that same
filesystem, and the artifact upload reads the compiled binary from it — none
of it touches the outer job's workspace.

Under `matrix`/`foreach`, each variant of a scoped `uses` step gets its own,
independent scope instance (its own container/pod), so matrix variants never
share isolated state.

**Validation — the following are parse errors inside a scoped `uses` (a `uses`
whose own `runsIn.image` is set), because they are incompatible with holding
one isolated environment open for the template's steps:**

| Inside a scoped `uses` | Why it's a parse error |
|---|---|
| A nested `runsIn:` (any form) on an inlined step | A scope must be homogeneous — one environment for the whole template, not a per-step override. |
| An `approval:` step | An approval pause would hold the scope's container/pod alive across a human wait (up to the approval timeout), wasting resources and risking a k8s pod deadline killing it mid-wait. |
| A `call:` step | `call:` spawns a separate child run on another agent/workspace that cannot see the scope's isolated filesystem — undefined semantics inside a scope. |

These checks apply to both concrete steps and members of a `parallel:` block,
and are inert outside scope mode — a plain `uses` (with no `runsIn`) still
allows `approval:`/`call:` unchanged. `parallel:` sub-steps inside a scoped
`uses` execute concurrently, but all still target the same shared scope
environment.

---

