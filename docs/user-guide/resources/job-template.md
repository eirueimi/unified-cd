# JobTemplate

## JobTemplate

The resource a `uses:` step points at. **Not applied to the controller** — a
JobTemplate lives in a git repository and is fetched at run creation via the
`uses:` step's `git://` URI. Its schema deliberately contains only what
inlining into the caller's run can honor; any other field is rejected at run
creation (strict decode). Pointing `uses:` at a `kind: Job` fails with a
conversion hint. See [Job Reference — uses:](../writing-jobs/index.md) for the full contract
and [templates/README.md](https://github.com/eirueimi/unified-cd/blob/main/templates/README.md) for a ready-made collection.

```yaml
apiVersion: unified-cd/v1
kind: JobTemplate
metadata:
  name: <string>                  # required
  labels: {}                      # optional
  annotations: {}                 # optional
spec:
  description: <string>           # optional, documentation only
  params:                         # same schema as Job (inputs/outputs)
    inputs:
      - name: <string>
        type: string | bool | int | array
        required: <bool>
        default: <any>
        description: <string>
    outputs:
      - name: <string>
        type: string | bool | int | artifact
        value: <template expression>
  shell: [<string>, ...]          # optional default interpreter for the template's steps
  podTemplate:                    # optional pod-shape subset — merged into the CALLER's pod
    spec:
      containers: [...]           # gap-fill merged; reserved names job/unified-artifact/ucd-shim rejected;
                                   # every container/volume name must be a valid DNS-1123 label (apply-time error)
      volumes: [...]              # gap-fill merged; reserved names workspace/ucd-tools rejected
  steps: [...]                    # same StepEntry schema as Job (nested uses: allowed)
  finally: [...]                  # optional — spliced into the CALLER's finally phase, not run standalone
```

Fields that do NOT exist on a JobTemplate (and error if present):
`agentSelector`, `concurrency`, `timeoutMinutes`, `native`, and any
`podTemplate` field other than `spec.containers`/`spec.volumes` — a template
inlines into the caller's run, so it cannot shape a different pod, agent, or
run. Use `call:` (a registered `kind: Job`, its own child run) when you need
those semantics.

### `finally`

`spec.finally` uses the same `StepEntry` schema as `steps` (including nested
`uses:` and `parallel:`), but its steps do **not** run in a phase of their
own — a `uses:` step that targets this template splices the (renamed,
ref-rewritten) finally steps into the **caller's** `spec.finally`, appended
after the caller's own hand-written finally steps, prefixed with the
`uses:` step's name like any other inlined step (`usesName__stepName`).
Nested `uses:` inside a template's body or its own `finally:` bubble their
finally steps up with the full prefix chain applied at each level. A
scope-mode `uses:` step (`runsIn.image`) rejects a target template that
declares `finally:` — the scope pod's lifetime ends with the template body,
so there is nothing left to run the finally steps in. See [Job Reference —
Template `finally:`](../writing-jobs/templates-and-reuse.md#template-finally-splice-into-the-caller) for
the full contract, ordering guarantee, and examples.

---

