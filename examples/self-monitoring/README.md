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

   ```bash
   unified-cd apply -f templates/slack-notify.yaml
   unified-cd secret set slack-webhook-url "https://hooks.slack.com/services/..."
   ```

2. Apply the job and schedule, pointing at your controller URL:

   ```bash
   unified-cd apply -f examples/self-monitoring/job.yaml
   unified-cd apply -f examples/self-monitoring/schedule.yaml
   ```

3. Trigger once manually to verify the wiring:

   ```bash
   unified-cd run trigger self-monitor --param controller_url=http://controller:8080
   ```
