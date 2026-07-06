# Availability Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** k8s-agent runs active-active by default (replicas: 2), the queued-run reaper grace becomes operator-configurable, and alerting ships as Prometheus rules plus a self-contained self-monitoring job example.

**Architecture:** No new runtime subsystems. One env-var knob in `cmd/controller/main.go` (mirroring the existing audit-retention pattern), manifest edits, one static alert-rules file, and example Job/Schedule YAML that uses only existing DSL features (`outputs` from `.Stdout`, CEL `if:`, `call:` into the registered `slack-notify` template job).

**Tech Stack:** Go, Kubernetes manifests, Prometheus alerting rules, unified-cd Job DSL.

Spec: `docs/superpowers/specs/2026-07-06-availability-hardening-design.md`

## Global Constraints

- English only (code, comments, docs, commit messages).
- No leader election; no built-in notification code (notifications go through `uses:`/`call:` templates only).
- `UNIFIED_QUEUED_RUN_GRACE`: Go duration string, default `5m`; unset/invalid/non-positive → warn + default, never fatal.
- Only the k8s-agent Deployments change replica counts; controller/postgres/garage stay as they are.
- The example job must NOT use `needs:` (removed from the DSL; docs mentioning it are stale) and must not add new template files — it calls the existing `templates/slack-notify.yaml` job.
- `if:` is CEL (lowercase `steps.NAME.outputs.KEY`, no `{{ }}`); `outputs:`/`run:` are Go templates.
- `gofmt -w` touched Go files; `go build ./...` and the named test commands must pass.

---

### Task 1: configurable queued-run reaper grace

**Files:**
- Modify: `cmd/controller/main.go` (add helper near `auditRetentionDaysDefault`, ~line 24; change the `RunQueuedRunReaper` call, ~line 246)
- Test: `cmd/controller/main_test.go`

**Interfaces:**
- Consumes: existing `RunQueuedRunReaper(ctx, st, interval, minAge, staleAfter)`.
- Produces: `queuedRunGraceDefault() time.Duration` (package main; no other task depends on it).

- [ ] **Step 1: Write the failing test**

Read `cmd/controller/main_test.go` first and match its existing style for the audit-retention test if one exists. Add:

```go
func TestQueuedRunGraceDefault(t *testing.T) {
	t.Setenv("UNIFIED_QUEUED_RUN_GRACE", "")
	assert.Equal(t, 5*time.Minute, queuedRunGraceDefault())

	t.Setenv("UNIFIED_QUEUED_RUN_GRACE", "20m")
	assert.Equal(t, 20*time.Minute, queuedRunGraceDefault())

	t.Setenv("UNIFIED_QUEUED_RUN_GRACE", "bogus")
	assert.Equal(t, 5*time.Minute, queuedRunGraceDefault())

	t.Setenv("UNIFIED_QUEUED_RUN_GRACE", "-1m")
	assert.Equal(t, 5*time.Minute, queuedRunGraceDefault())
}
```

Add missing imports (`time`, testify `assert`) if the file lacks them.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/controller/ -run TestQueuedRunGraceDefault -v`
Expected: FAIL — `undefined: queuedRunGraceDefault`.

- [ ] **Step 3: Implement**

In `cmd/controller/main.go`, directly below `auditRetentionDaysDefault`:

```go
// queuedRunGraceDefault resolves the queued-run reaper grace period from
// UNIFIED_QUEUED_RUN_GRACE (a Go duration string such as "5m" or "20m"),
// falling back to 5 minutes when unset, invalid, or non-positive. This is
// how long a run may sit Queued with no eligible live agent before being
// failed - raise it in environments where a full agent outage (e.g. a
// node-pool upgrade) can exceed the default.
func queuedRunGraceDefault() time.Duration {
	const def = 5 * time.Minute
	v := os.Getenv("UNIFIED_QUEUED_RUN_GRACE")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		slog.Warn("invalid UNIFIED_QUEUED_RUN_GRACE, using default", "value", v, "default", def)
		return def
	}
	return d
}
```

Change the reaper call (keep the surrounding comment but update "5m" wording to mention the env var):

```go
	go controller.RunQueuedRunReaper(ctx, st, 30*time.Second, queuedRunGraceDefault(), 90*time.Second)
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./cmd/controller/ -run TestQueuedRunGraceDefault -v && go build ./...`
Expected: PASS, clean build.

- [ ] **Step 5: Commit**

```bash
git add cmd/controller/main.go cmd/controller/main_test.go
git commit -m "feat(controller): make queued-run reaper grace configurable via UNIFIED_QUEUED_RUN_GRACE"
```

---

### Task 2: k8s-agent replicas: 2 + HA docs

**Files:**
- Modify: `manifests/base/k8s-agent/deployment.yaml` (replicas)
- Modify: `manifests/install.yaml`, `manifests/core-install.yaml`, `manifests/agent-only.yaml` (ONLY the k8s-agent Deployment's replicas)
- Modify: `docs/high-availability.md` (Agent Redundancy section), `docs/kubernetes-integration.md`

**Interfaces:** none.

- [ ] **Step 1: Bump replicas in the four manifests**

In each file, locate the Deployment named `unified-cd-k8s-agent` and change `replicas: 1` → `replicas: 2`. The flat manifests contain several Deployments (controller, postgres, garage) whose `replicas: 1` MUST stay untouched — edit by locating the `name: unified-cd-k8s-agent` Deployment block, not by global replace.

- [ ] **Step 2: Verify structurally**

```bash
python - <<'EOF'
import yaml
for f in ["manifests/base/k8s-agent/deployment.yaml","manifests/install.yaml","manifests/core-install.yaml","manifests/agent-only.yaml"]:
    docs = [d for d in yaml.safe_load_all(open(f, encoding="utf-8-sig")) if d]
    for d in docs:
        if d.get("kind") == "Deployment":
            name = d["metadata"]["name"]; r = d["spec"].get("replicas")
            expect = 2 if name == "unified-cd-k8s-agent" else 1
            assert r == expect, (f, name, r)
print("manifests ok")
EOF
```

Expected: `manifests ok`. (Note `utf-8-sig`: `base/k8s-agent/deployment.yaml` starts with a BOM.)

- [ ] **Step 3: Update the docs**

`docs/high-availability.md`, at the end of the "Agent Redundancy" section, add:

```markdown
### k8s-agent replicas

The k8s agent runs active-active: the install manifests default to
`replicas: 2`. This is safe without leader election because run claiming is
atomic (`FOR UPDATE SKIP LOCKED`), each pod registers under its own agent ID
(`UNIFIED_K8S_AGENT_ID` from the Downward API), scope pods use
`generateName`, and pod GC only touches pods whose runs are terminal or
absent. During a *full* k8s-agent outage (for example a node-pool upgrade
taking every replica down), queued runs are failed once they have waited
longer than the queued-run reaper grace — configurable on the controller via
`UNIFIED_QUEUED_RUN_GRACE` (default `5m`). Raise it if such outages can
exceed the default in your environment.
```

`docs/kubernetes-integration.md`: where the k8s-agent Deployment/install is described, add one sentence noting the default is now 2 replicas and pointing at the HA guide section above.

- [ ] **Step 4: Commit**

```bash
git add manifests/ docs/high-availability.md docs/kubernetes-integration.md
git commit -m "feat(manifests): default k8s-agent to two active-active replicas"
```

---

### Task 3: Prometheus alert rules

**Files:**
- Create: `deployments/observability/prometheus-alerts.yaml`
- Modify: `docs/operations.md` (Metrics section: link the file)

**Interfaces:** consumes metric names exposed by `internal/metrics` (verify against `docs/operations.md` table).

- [ ] **Step 1: Create the rules file**

```yaml
# Prometheus alerting rules for unified-cd controller metrics.
# Load via your Prometheus rule_files (or a managed-monitoring equivalent).
# Aggregations use max()/sum() so they stay correct with multiple
# controller replicas (gauges are identical per replica; counters are
# per-replica partial counts).
groups:
  - name: unified-cd
    rules:
      - alert: UnifiedCDNoAliveAgents
        expr: max(unifiedcd_agents{state="alive"}) == 0
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "No alive unified-cd agents"
          description: "No agent has sent a heartbeat within the liveness window for 5 minutes. Queued runs cannot start and will be failed once the queued-run reaper grace expires."

      - alert: UnifiedCDQueueBacklog
        expr: >
          max(unifiedcd_runs_current{status="Pending"})
            + max(unifiedcd_runs_current{status="Queued"}) > 20
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "unified-cd run queue backlog"
          description: "More than 20 runs have been waiting (Pending + Queued) for 10 minutes. Agents may be under-provisioned, mislabeled, or down."

      - alert: UnifiedCDHighFailureRate
        expr: >
          (
            sum(rate(unifiedcd_runs_finished_total{status="Failed"}[30m]))
              /
            clamp_min(sum(rate(unifiedcd_runs_finished_total[30m])), 1e-10)
          ) > 0.5
          and sum(rate(unifiedcd_runs_finished_total[30m])) > 0
        for: 15m
        labels:
          severity: warning
        annotations:
          summary: "unified-cd run failure rate above 50%"
          description: "More than half of finished runs failed over the last 30 minutes. Check recent job changes, agent health, and external dependencies."

      - alert: UnifiedCDScrapeCollectorErrors
        expr: sum(rate(unifiedcd_scrape_collector_errors_total[10m])) > 0
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "unified-cd metrics collector cannot read its database"
          description: "The controller's scrape-time DB collector has been erroring for 10 minutes; runs/agents gauges are missing from scrapes. Check controller-to-Postgres connectivity."
```

- [ ] **Step 2: Validate**

```bash
python -c "import yaml; yaml.safe_load(open('deployments/observability/prometheus-alerts.yaml')); print('yaml ok')"
promtool check rules deployments/observability/prometheus-alerts.yaml || echo "promtool not installed - YAML check only"
```

Cross-check every metric name in the file against the "Key metrics" table in `docs/operations.md` — all four must exist there (add `unifiedcd_scrape_collector_errors_total` to that table if absent, since the rule references it).

- [ ] **Step 3: Link from docs/operations.md**

In the Metrics section, after the example queries, add:

```markdown
Ready-made Prometheus alerting rules for these metrics live in
[`deployments/observability/prometheus-alerts.yaml`](../deployments/observability/prometheus-alerts.yaml)
(no alive agents, queue backlog, high failure rate, collector errors).
```

- [ ] **Step 4: Commit**

```bash
git add deployments/observability/prometheus-alerts.yaml docs/operations.md
git commit -m "feat(observability): Prometheus alert rules for controller metrics"
```

---

### Task 4: self-monitoring job example

**Files:**
- Create: `examples/self-monitoring/job.yaml`
- Create: `examples/self-monitoring/schedule.yaml`
- Create: `examples/self-monitoring/README.md`

**Interfaces:** consumes the registered `slack-notify` job from `templates/slack-notify.yaml` via `call:`. BEFORE writing `job.yaml`, read `templates/slack-notify.yaml` and list its actual input params; the `with:` block below must be adjusted to that template's real inputs (the alert text goes into whichever param carries the message/status text; the Slack webhook comes from the secret name that template expects).

- [ ] **Step 1: Write job.yaml**

```yaml
apiVersion: unified-cd/v1
kind: Job
metadata:
  name: self-monitor
spec:
  params:
    inputs:
      - name: controller_url
        type: string
        default: "http://localhost:8080"
      - name: backlog_threshold
        type: int
        default: "20"
  steps:
    - name: check
      timeoutMinutes: 2
      run: |
        set -e
        m=$(curl -fsS --max-time 10 "{{ .Params.controller_url }}/metrics")
        alive=$(printf '%s\n' "$m" | awk '$1 == "unifiedcd_agents{state=\"alive\"}" {print int($2)}')
        backlog=$(printf '%s\n' "$m" | awk '$1 == "unifiedcd_runs_current{status=\"Pending\"}" || $1 == "unifiedcd_runs_current{status=\"Queued\"}" {s += $2} END {print int(s)}')
        msg=""
        if [ "${alive:-0}" -eq 0 ]; then
          msg="no alive agents"
        fi
        if [ "${backlog:-0}" -gt "{{ .Params.backlog_threshold }}" ]; then
          msg="${msg:+$msg; }queue backlog ${backlog} exceeds {{ .Params.backlog_threshold }}"
        fi
        echo "ALIVE=${alive:-0}"
        echo "BACKLOG=${backlog:-0}"
        echo "ALERT=$msg"
      outputs:
        alert_message: '{{ .Stdout | grep "ALERT=" | cut "=" 2 | trim }}'
    - name: notify
      if: steps.check.outputs.alert_message != ""
      call:
        job: slack-notify
        with:
          # Adjust keys to templates/slack-notify.yaml's actual inputs
          # (read that file first); the alert text must flow through.
          status: "failure"
          job_name: "self-monitor"
          message: "{{ .Steps.check.Outputs.alert_message }}"
```

Adapt the `with:` keys and the exact steps-output template reference style to what `templates/slack-notify.yaml` and `docs/jobs.md` actually define — then remove the adjustment comment. The `check` step's shell and outputs are fixed as written.

- [ ] **Step 2: Write schedule.yaml**

```yaml
apiVersion: unified-cd/v1
kind: Schedule
metadata:
  name: self-monitor-every-5m
spec:
  cron: "*/5 * * * *"
  job: self-monitor
```

- [ ] **Step 3: Write README.md**

````markdown
# Self-monitoring example

Alerts on unified-cd's own `/metrics` using only unified-cd primitives —
no external monitoring required.

**What it checks every 5 minutes:** alive agents == 0, and run queue
backlog (Pending + Queued) above a threshold. Failure *rate* alerting
needs rate() over time and is intentionally left to the Prometheus rules
in `deployments/observability/prometheus-alerts.yaml`.

**Known limitation:** a job scheduled by the controller cannot alert on
the controller itself being down. Cover full-outage detection with the
Prometheus rules or any external uptime check on `GET /healthz`.

## Setup

1. Register the Slack template and its webhook secret (see
   `templates/README.md`):

   ```
   unified-cd apply -f templates/slack-notify.yaml
   unified-cd secret set slack-webhook-url https://hooks.slack.com/...
   ```

2. Apply the job and schedule, pointing at your controller URL:

   ```
   unified-cd apply -f examples/self-monitoring/job.yaml
   unified-cd apply -f examples/self-monitoring/schedule.yaml
   ```

3. Trigger once manually to verify the wiring:

   ```
   unified-cd run self-monitor --param controller_url=http://controller:8080
   ```
````

Adjust the secret-name/CLI syntax in the README to match `templates/slack-notify.yaml` and the real CLI (`unified-cd secret --help` style found in docs/cli.md) — the structure above is fixed.

- [ ] **Step 4: Validate the YAML parses as a Job/Schedule**

Check whether a parse test pattern exists for template/example YAML (`internal/dsl/parse_test.go` parses template files). Add the two files to that pattern if a directory-walking test exists; otherwise validate manually:

```bash
python -c "import yaml; yaml.safe_load(open('examples/self-monitoring/job.yaml')); yaml.safe_load(open('examples/self-monitoring/schedule.yaml')); print('yaml ok')"
```

Also verify against a running dev stack if available: `docker compose` stack is running on this machine; `unified-cli` can apply the job with `--server http://localhost:8080` and the dev token. If the stack is not reachable, note it in the report instead.

- [ ] **Step 5: Commit**

```bash
git add examples/self-monitoring/
git commit -m "docs(examples): self-monitoring job alerting on /metrics via slack-notify"
```
