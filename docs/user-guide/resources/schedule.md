# Schedule

## Schedule

Triggers a job on a cron schedule.

```yaml
apiVersion: unified-cd/v1
kind: Schedule
metadata:
  name: <string>                  # required
  annotations:                    # optional
    <key>: <value>
spec:
  cron: <string>                  # required — 5-field cron expression
  job: <string>                   # required — name of the Job to trigger
  params:                         # optional — parameters passed to the triggered run
    <key>: <value>
```

### Fields

| Field | Type | Required | Description |
|---|---|---|---|
| `metadata.name` | string | Yes | Unique schedule name |
| `metadata.annotations` | map[string]string | No | Arbitrary key/value metadata |
| `spec.cron` | string | Yes | 5-field cron expression: `min hour day month weekday` |
| `spec.job` | string | Yes | Name of the registered Job to trigger |
| `spec.params` | map[string]string | No | Input parameters to pass to the triggered run |

### Cron expression format

```
┌─ minute        (0-59)
│  ┌─ hour       (0-23)
│  │  ┌─ day     (1-31)
│  │  │  ┌─ month (1-12)
│  │  │  │  ┌─ weekday (0-6, 0=Sunday)
│  │  │  │  │
*  *  *  *  *
```

| Example | Meaning |
|---|---|
| `0 2 * * *` | Every day at 02:00 UTC |
| `*/15 * * * *` | Every 15 minutes |
| `0 9 * * 1-5` | Weekdays at 09:00 UTC |
| `0 0 1 * *` | First day of every month |

If the controller is down during a scheduled fire time, the fire is caught up within 1 hour after restart.

Apply a Schedule the same way as any other resource:

```bash
unified-cli apply -f schedule.yaml
```

Runs triggered by a Schedule show up with `triggeredBy: schedule:<name>`.

### Example

```yaml
apiVersion: unified-cd/v1
kind: Schedule
metadata:
  name: nightly-build
spec:
  cron: "0 2 * * *"
  job: build
  params:
    tag: nightly
    deploy_env: staging
```

---

