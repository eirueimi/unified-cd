# Migrating to concurrent Kubernetes step execution

`matrix:`/`foreach:` combinations and `parallel:` groups on the Kubernetes
agent now run concurrently inside the run Pod, sharing its workspace — the
same as they already do on the standard agent. Previously the Kubernetes
agent ran them one member at a time; that was an implementation concession
in the backend (`ConcurrencyMode` returning `Sequential` because the
scope-pod map wasn't concurrency-safe), not a documented guarantee. Now that
the map is concurrency-safe, the concession is gone and the two backends
match.

| Before | After |
|---|---|
| `matrix:`/`foreach:` combinations and `parallel:` group members ran one at a time inside the Kubernetes agent's run Pod. | Combinations and group members run concurrently, exactly as on the standard agent. |
| A job whose members wrote the same workspace path was accidentally race-free on the Kubernetes agent. | The same job races on both backends. |
| `ConcurrencyMode` on the Kubernetes backend reported `Sequential`. | Both backends report `Concurrent`. The field stays on the `ExecBackend` interface as the documented seam for a future backend to declare otherwise — it isn't going away just because both current backends agree. |

## What can break

The DSL's contract has always been that parallel steps share a workspace;
nothing in the spec ever promised the Kubernetes agent would serialize
matrix or `parallel:` members for you. Aligning Kubernetes to that contract
is the direction the whole `ExecBackend` seam has been moving in — the
Kubernetes ordering was an implementation artefact of an unguarded internal
map, not a feature. This guide exists because that artefact was load-bearing
for some jobs without anyone deciding it should be.

**The job shape at risk:** two or more `matrix:`/`foreach:` combinations or
`parallel:` group members that write the same path in the shared workspace —
several combinations appending to one file, or two members each writing a
file with the same name. On the standard agent, a job shaped like this was
**already racy**; nothing here changes the standard agent. On the
Kubernetes agent, the same job was accidentally safe, because members ran
one at a time and never actually overlapped on disk. That accident is what
this change removes.

No flag restores the old serialization, and none is planned — adding one
would carve out DSL surface to protect a contract this project never
documented as a guarantee. If two writes must not overlap, order them with
the DSL's existing sequencing primitive: declaration order (see below).
`needs:` is not that primitive — it was removed from the DSL entirely (`apply`
rejects it outright, at any nesting level, with "`needs: is no longer
supported`"); do not add it to a job definition expecting it to do anything.

## The symptom

This is not a run that fails cleanly with a named error. It's a data race
in the job's own script, so it shows up the way data races always do:

- Output that's truncated, interleaved, or missing lines a member was
  supposed to append.
- A step that fails intermittently — passing on some runs and failing on
  others with no change to the job definition.
- A downstream step reading a file that one matrix combination half-wrote
  before another combination's write (or its own container's buffering)
  landed on top of it.

If you're expecting a distinct error message naming a conflict, you won't
get one. Treat "flaky in a way that correlates with concurrency" and
"corrupted file in the shared workspace" as the signals to look for.

## Who is affected, and how to find out before upgrading

There is no single greppable token for this: "two matrix members write the
same path" is a fact about what a step's script *does*, not about how it's
declared, and no keyword in the job YAML marks it. A search can only narrow
the list of jobs worth reading by eye — it cannot confirm or clear one on
its own.

Start from jobs that have both a `matrix:`/`foreach:`/`parallel:` block and
a shell redirect or append inside it:

```bash
grep -rlE '(matrix|foreach)\s*:|parallel\s*:' <your job definitions> \
  | xargs grep -lE '>>|>\s*[^&]|\btee\b|\bcp\b|\bmv\b'
```

Every hit needs a human read: does the write target a path that's the same
across combinations/members (a fixed filename, or a filename built only from
job-level values), or does it already vary per combination (built from
`{{ .Matrix.* }}`/`{{ .Foreach.* }}`, or a per-member subdirectory)? Only the
former is affected. The search will also surface writes that are already
per-combination and therefore fine — false positives to dismiss by eye, not
a defect in the search. It will miss a collision written some other way
entirely (a compiled program the step invokes, an append done in a language
other than shell, two *different* step names that happen to target the same
file without either being an obvious redirect) — a clean result narrows the
list, it does not clear a job.

## The fix for an affected job

The DSL's only step-ordering primitive is declaration order: steps under
`steps:` run one at a time, in the order listed, by default; `parallel:` is
what opts a group of them into running concurrently instead (see [Concurrent
Steps (`parallel`)](../../user-guide/writing-jobs/steps.md#concurrent-steps-parallel)).
There is no separate dependency keyword — ordering two writes that must not
overlap means taking them out of concurrency, not annotating a dependency
between them.

**For `parallel:` group members**, pull the member that must go second out of
the block. It becomes a plain step placed after the block, which — by
ordinary declaration order — only starts once every member of the block
above it has finished.

Before — two members of one `parallel:` group both append to
`/workspace/report.txt`:

```yaml
steps:
  - parallel:
      - name: summarize-frontend
        run: echo "frontend done" >> /workspace/report.txt

      - name: summarize-backend
        run: echo "backend done" >> /workspace/report.txt
```

After — `summarize-backend` moved out of the block, so it now runs after
`summarize-frontend` (and anything else left in the block) instead of
alongside it:

```yaml
steps:
  - parallel:
      - name: summarize-frontend
        run: echo "frontend done" >> /workspace/report.txt

  - name: summarize-backend        # outside parallel: — runs after the
    run: echo "backend done" >> /workspace/report.txt   # block above completes, per declaration order
```

**For `matrix:`/`foreach:` combinations**, there's no equivalent move — the
combinations are all expansions of the same step and run as one concurrent
set, so pulling one out isn't possible the way it is for a named `parallel:`
member. Give each combination its own path instead, parameterized by the
combination key, and aggregate afterward if a single combined file is
actually needed:

```yaml
steps:
  - name: summarize
    matrix:
      part: [frontend, backend]
    run: |
      echo "{{ .Matrix.part }} done" >> /workspace/report-{{ .Matrix.part }}.txt

  - name: combine-reports
    run: cat /workspace/report-*.txt > /workspace/report.txt
```

`combine-reports` is a plain step after the matrix step, so it already waits
for every combination to finish before it runs — no extra ordering needed.
