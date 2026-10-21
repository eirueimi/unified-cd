# Core Concepts

```
CLI / Browser / Webhook
        │
        ▼
┌─────────────────┐     ┌───────────────┐
│   Controller    │────►│  PostgreSQL   │  jobs, runs, queue, secrets, sessions
│   (stateless)   │     └───────────────┘
│   N replicas    │     ┌───────────────┐
│   behind LB     │────►│  S3 / Garage  │  log archives, artifacts, git template cache
└────────┬────────┘     └───────────────┘
         │ HTTP long-poll
         ▼
┌────────────────────────────────────────┐
│  Agents                                │
│  ┌──────────┐  ┌──────────┐  ┌──────┐ │
│  │  Linux   │  │  Windows │  │  k8s │ │  execute job steps
│  └──────────┘  └──────────┘  └──────┘ │
└────────────────────────────────────────┘
```

- **Controller** — stateless HTTP server; schedules and dispatches jobs; manages all resources
- **Agent** — connects to controller via long-polling; executes job steps in a per-run workspace that is empty when the run starts and discarded when it ends (data that must survive between runs lives on a named volume the agent's own config declares and the job mounts with `spec.volumes`; see [Volumes](../user-guide/writing-jobs/volumes.md)). Jobs are isolated by default: each claim runs inside a container ("claim pod": a pause container + `podTemplate` sidecars sharing one network namespace), the same model as the k8s-agent's real Pod. Jobs that need the host itself opt out with `spec.native: true` — see [Job Isolation](../user-guide/writing-jobs/isolation-and-containers.md#job-isolation-native-and-the-claim-pod).
- **k8s-agent** — Kubernetes-native agent; creates a Pod per job and exec's steps inside it
- **CLI** — `unified-cli` — apply YAML, trigger runs, stream logs, manage secrets and tokens
