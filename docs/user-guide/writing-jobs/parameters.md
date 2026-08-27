# Parameters

## Parameters (inputs / outputs)

Declare typed inputs that callers must or may supply, and outputs that the job produces.

```yaml
spec:
  params:
    inputs:
      - name: image
        type: string        # "string" | "bool" | "int" | "array"
        required: true
        description: "Docker image name"
      - name: tag
        type: string
        default: latest
      - name: run_tests
        type: bool
        default: true
      - name: ref
        type: string
        pattern: '^[A-Za-z0-9._/-]+$'
      - name: env
        type: string
        choices: [dev, stg, prod]  # renders as a dropdown in the Web UI
        default: dev
    outputs:
      - name: image_ref
        type: string        # "string" | "bool" | "int" | "artifact"
      - name: test_report
        type: artifact
```

### Input fields

| Field | Type | Required | Description |
|---|---|---|---|
| `name` | string | Yes | Parameter name. Referenced as `{{ .Params.name }}` in steps. |
| `type` | string | Yes | `string`, `bool`, `int`, or `array` (backs `$param`-style references used by `matrix`/`foreach`/`orLocks`) |
| `required` | bool | No | If true, the run fails immediately when the value is not supplied. |
| `default` | any | No | Value used when the caller does not supply this parameter. Checked against `pattern` just like a caller-supplied value, so a bad default cannot slip through. |
| `description` | string | No | Human-readable description shown in the Web UI trigger form. |
| `pattern` | string | No | A regular expression the resolved value must match (checked in `resolveParams`, the choke point every param source flows through: webhook mapping, CLI `--param`, `call:`/`uses:` `with:`, schedule params). **Why this exists:** param values are interpolated directly into step shell text (`sh -lc`), so a param sourced from outside the job author's control is a command-injection vector — the same class of bug as GitHub Actions script injection. This matters most for webhook `paramsMapping`: a valid HMAC/token signature only proves who sent the request, not that its content is benign, and an outside contributor who can open a PR or push a branch controls fields like `.Payload.pull_request.head.ref`. A malformed pattern is itself a rejected apply/trigger, never a silent allow-all. Suggested starting point: `^[A-Za-z0-9._/-]+$`. A rejected value is never echoed back in the error, since it may itself carry the injection payload into operator-read logs. |
| `choices` | []string | No | A Jenkins-style fixed dropdown of allowed values: instead of a free-text box, the Web UI trigger form renders a `<select>` (with a blank `-- select --` placeholder) populated from this list. Only `string` and `int` inputs may declare it — `bool` already has exactly two values, and `array` is rejected because it's ambiguous whether choices would constrain each element or the array as a whole, a materially different (multi-select) feature this can't express. Mutually exclusive with `pattern`. If `default` is set, it must be one of the listed choices — checked at **parse time**, stricter than `pattern`, whose "defaults are checked" claim only actually holds at run time in `resolveParams`. At run creation, a resolved value that isn't one of the choices is rejected with an error naming the *allowed* values (never the rejected one, for the same log-injection reason as `pattern`); an explicit empty string on an optional param with no default is treated as "no selection", not an error, matching the `<select>`'s blank placeholder. **Security:** a `choices:` allow-list is a strict enumeration of exact values, strictly stronger than any `pattern:` regex (a regex only constrains syntax; choices constrains membership) — so a `choices:`-only input, with no `pattern:` at all, satisfies the webhook `paramsMapping` security gate on its own (see [Payload-mapped params must be validated](../resources/webhook-receiver.md#payload-mapped-params-must-be-validated)). |
| `unvalidated` | bool | No | Explicit opt-out of the `pattern`/`choices` requirement for a webhook payload-mapped param (see [WebhookReceiver](../resources/webhook-receiver.md#webhookreceiver)). Use only when the value is genuinely free-form and never reaches a shell. |

### Output fields

| Field | Type | Required | Description |
|---|---|---|---|
| `name` | string | Yes | Output name. Accessible in calling jobs as `{{ .Steps.stepName.Outputs.name }}`. |
| `type` | string | Yes | `string`, `bool`, `int`, or `artifact` |

### Trigger with parameters

```bash
unified-cli run trigger build --param image=myapp --param tag=v2.0 --param run_tests=false
```

---

