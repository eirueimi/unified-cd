# Concurrency and Agent Selection

## Concurrency Control

Prevent multiple runs from executing simultaneously when they share a resource.

### Mutex

A named mutual exclusion lock — only one run holding the mutex runs at a time.

```yaml
spec:
  concurrency:
    mutex: deploy-production
```

Runs that cannot acquire the mutex wait in the queue until it is released.

### Named Lock Pool

A semaphore — allows up to `capacity` runs to proceed simultaneously.

```yaml
spec:
  concurrency:
    semaphores:
      - pool: test-environments    # pool name
        capacity: 3                # max concurrent holders
```

Useful for limiting usage of a shared resource (e.g. 3 test environments available).

### OR Lock

Acquire exactly one of several named resources. The acquired value is injected as a parameter.

```yaml
spec:
  concurrency:
    orLocks:
      - name: env                          # parameter name prefix
        in:                                # candidate resources (list, or a $param expression)
          - staging-a
          - staging-b
          - staging-c
  steps:
    - name: deploy
      run: |
        echo "Deploying to {{ .Params.ENV_LOCK_VALUE }}"
        ./deploy.sh --target {{ .Params.ENV_LOCK_VALUE }}
```

The acquired candidate is available as `{{ .Params.<NAME>_LOCK_VALUE }}` (uppercased name + `_LOCK_VALUE`).

---

## Agent Selection (`agentSelector`)

A list of labels that a qualifying agent must have. All labels must match (AND logic).

```yaml
spec:
  agentSelector:
    - kind:linux
    - env:prod
```

Labels can include parameter expansion:

```yaml
spec:
  params:
    inputs:
      - name: pool
        type: string
        required: true
  agentSelector:
    - "pool:{{ .Params.pool }}"
```

```bash
unified-cli run trigger build --param pool=gpu-workers
# → only agents with label "pool:gpu-workers" can claim this run
```

If `agentSelector` is omitted, any available agent can claim the run.

---

