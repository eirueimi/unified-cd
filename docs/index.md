# unified-cd

An open-source CI/CD tool (Jenkins alternative) written in Go.

Jobs are defined in YAML, executed as a DAG of steps, and dispatched to agents
running on Linux, macOS, Windows, or Kubernetes. The controller is stateless;
all durable state lives in PostgreSQL and an S3-compatible object store.

## Where to start

- **New here?** [Installation](getting-started/quickstart.md) walks from a running stack to
  your first job.
- **Writing a job?** [Jobs](user-guide/writing-jobs/index.md) is the complete Job YAML reference, and
  [Resources](resources.md) covers every other resource kind.
- **Running a deployment?** [Configuration](reference/configuration.md),
  [High Availability](operator-manual/high-availability.md), and [Operations](operator-manual/operations.md)
  cover the operator side.
- **Something broke?** [Troubleshooting](troubleshooting/index.md) is indexed by the
  symptom you saw.

## Key features

- YAML-defined jobs with DAG step execution
- Multi-platform agents (Linux, macOS, Windows, Kubernetes)
- Per-agent enrollment with independently revocable credentials
- Secrets management with automatic log masking
- Webhook and cron triggers
- High availability with leader election
- Web UI and OIDC SSO
