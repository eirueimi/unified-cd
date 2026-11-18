# Examples

Runnable, self-contained examples for unified-cd. Each file applies cleanly on its own and its
trigger works without applying any other file first (`jobs/volumes.yaml` also needs volumes
declared with `resources/agentvolume.yaml`; see Notes).

## Layout

- `jobs/` — one feature per file (parameters, parallel steps, matrix, concurrency, `call:`
  composition, `uses:` git templates, artifacts, cache, secrets, approvals, webhooks, …).
  - `jobs/k8s/` — jobs that run on a k8s-agent (real Pods).
  - `jobs/team-a/` — a job carrying `annotations.path: team-a`, showing the path-namespaced
    layout an AppSource derives from directory structure.
- `config/` — sample controller / agent / k8s-agent configuration files.
- `resources/` — non-job resources (`AppSource`, `GitCredential`, `AgentVolume`).

## Applying

Apply a single file:

```bash
unified-cli apply -f examples/jobs/hello.yaml
unified-cli run trigger hello-docker
```

Or apply every job at once — the resource names in `examples/jobs/*.yaml` are unique, so a bulk
apply registers them all without one clobbering another:

```bash
unified-cli apply -f examples/jobs/            # applies every file in the directory
```

(The `build` job in `jobs/team-a/` does not collide with the top-level examples: it is
path-namespaced via `annotations.path: team-a`. The canonical parameterized build lives in
`jobs/params.yaml`; the `call:` and webhook examples use their own `build-service` and
`image-build` jobs so all names stay distinct.)

## Notes

- Several examples run steps in the default runner container (isolated). Add `native: true` to a
  job to run its steps directly on the agent host instead (needed for `native-build.yaml`, which
  targets a host toolchain).
- A job's workspace is empty at the start of every run and discarded at the end, on host agents
  and k8s-agents alike. `jobs/volumes.yaml` shows the only way to keep files between runs: a
  named volume declared on the controller (`kind: AgentVolume`, see
  `resources/agentvolume.yaml`) and mounted with `spec.volumes`. It runs only on an agent that
  receives those volumes; with none declared, the run stays Queued.
- `call:` examples (`call-job.yaml`) hold an agent slot while waiting on the jobs they call, so a
  single-agent fleet must allow ≥2 concurrent runs (`--max-concurrent 2`) or provide a second
  agent — otherwise the child runs stay Queued and the parent waits. `agent-routing.yaml` pins its
  child to a specific host by hostname, which needs the same headroom.
