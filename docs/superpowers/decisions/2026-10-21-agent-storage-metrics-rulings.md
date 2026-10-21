# Agent storage metrics — decisions taken during implementation

Plan: `docs/superpowers/plans/2026-10-21-agent-storage-metrics-plan.md`
Spec: `docs/superpowers/specs/2026-10-21-agent-storage-metrics-design.md`

The plan was executed task by task, each task implemented by one agent and
reviewed by another, then reviewed as a whole branch. Where a review, an
implementer or the plan itself raised a question the spec did not settle, it
was ruled on rather than parked, and every ruling is recorded here with what
it costs if it turns out to be wrong.

This file exists because the execution ledger is git-ignored scratch: without
it these decisions would die with the workspace, and a maintainer who disagrees
with one would have no way to find out it was a decision at all.

## Rulings

### 1. `api.AgentStorage` carries `AgentName`

Spec §4.5 labels the series with both `agent` (the ID) and `agent_name`, but
nothing in the plan's interfaces carried a name, and the `agents` table has
only `id` and `hostname`. The field is set by the agent, persisted with the
reading and returned by `ListAgentStorage`. A host agent sets it to its
hostname — `internal/config/agent.go` has `id`, not `name` — and a Kubernetes
agent to its configured `agentName`, which is the PVC prefix.

**Cost if wrong:** two host agents sharing a hostname merge under
`min by (agent_name, volume)`. The minimum is the conservative figure, so the
alert errs loud, and the `agent` label still separates them.

### 2. The spec is not amended for the `freeBytes` → `diskStats` rename

Spec §3 records the state of the code at design time and §6 names this choice
as the builder's. A design document is a historical record, not a reference
page.

**Cost if wrong:** a reader greps the spec for a function that no longer
exists. The plan and the code both name `diskStats`.

### 3. The shim's `statfs` uses a three-file build-tag split

`linux || darwin || freebsd || dragonfly`, `windows`, and the complement.
`x/sys/unix` has no `Statfs` on netbsd, solaris or illumos, and openbsd's
`Statfs_t` has no `Bavail`, so a plain `unix` tag does not compile everywhere.
Cross-built clean for 16 targets.

**Cost if wrong:** a GOOS lands in two tag sets or in none. Either is a build
failure, not a silent defect.

### 4. The `statfs` tests accept a zero free value

A full disk is a legitimate state, and asserting `free > 0` would make the test
flaky. `total > 0` and `free <= total` still pin the contract.

**Cost if wrong:** a bug returning zero free passes this test. The error path
is tested separately and a zero total is rejected outright.

### 5. `MinFreeDiskBytes` is `*uint64` and `ClaimBlockedByDisk` is `*bool`

Spec §4.2 requires a Kubernetes agent to omit both series; spec §4.5 requires a
host agent with the check disabled to publish `0`, because the disabled case is
its own warning. Plain value types collapse those two cases, and the only
remaining discriminator — `Workspace != nil` — is an undocumented coupling that
breaks precisely when a host agent's workspace measurement fails, which is when
its blocked flag matters most. nil means "this agent has no such thing"; a
non-nil zero means "configured as 0".

**Cost if wrong:** three later tasks dereference these pointers, so a missed
nil check is a panic in the collector rather than a wrong number. The
alternative publishes "disk check disabled, not blocked" for every Kubernetes
agent, which is the §2 false reassurance.

### 6. `SetAgentStorage` takes a pointer, and nil writes SQL NULL

`ListAgentStorage` keys freshness on `last_seen_at` alone, so an agent that is
alive and heartbeating but no longer sends a reading — a downgrade, or a
measurement path that gives up — would keep exporting its last one. The
handler calls `SetAgentStorage` on every beat, so "this beat carried nothing"
becomes "no series".

**Cost if wrong:** an agent whose measurement fails transiently loses its
series for a beat instead of showing a 15-second-old number. Absence is this
design's preferred answer.

### 7. The workspace figure carries no `MeasuredAt`

Spec §4.5 gives a timestamp only to the per-volume series. A host agent
remeasures the workspace on every beat and `last_seen_at` bounds its age; the
timestamp exists for the Kubernetes case, where a figure is only as fresh as
the last run that mounted the volume.

**Cost if wrong:** an operator cannot tell how old a workspace figure is except
through agent liveness. The whole-branch review noted this leaves the collector
asymmetric — a volume with no stamp is dropped outright while a workspace
figure is published with no age at all — and judged it acceptable.

### 8. The host agent measures in a sampler, not on the heartbeat goroutine

`statfs` on a hard-mounted NFS export whose server has gone away blocks in the
kernel and no context interrupts it, and `shared`-mode volumes on NFS are a
supported configuration. Measuring inline meant the agent stopped heartbeating
entirely, the reaper failed its running jobs — including jobs that never
touched the dead volume — and shutdown hung, since `Run` joins the heartbeat
goroutine. `heartbeatTimeout` does not help: it bounds only the HTTP request,
and its clock starts before the provider runs, so a slow measurement poisons
the following beat too.

The sampler keeps one measurement in flight per path, and a reading older than
two sample intervals is omitted rather than sent, the workspace included.

**Cost if wrong:** more moving parts in the agent for a failure mode an
operator may never hit, and readings up to two intervals old — which the age
cutoff exists to bound.

### 9. The Kubernetes agent measures both before and after a run

Spec §6 left "at mount time, at teardown, or both" open. A pre-run figure
describes the volume as the *previous* run left it and only when a later run
starts, so a run that fills the volume and then fails is never seen. On a root
volume with `cleanWorkspace` it is worse: the measurement lands just after the
wipe, so every reading shows near-full headroom however much each run consumes
— a figure that can never show peak use. The post-run reading is the one an
alert needs; the pre-run one still shows the headroom a run starts with.

**Cost if wrong:** one extra exec per run, delaying an exclusive volume's lock
release by up to the 15s bound. The run's own reported result is unaffected.

### 10. A volume whose `MeasuredAt` is zero is dropped entirely

Not merely denied its timestamp series. Exporting free and size with no age is
the "figure so stale it describes last week" case §2 forbids, and a query
joining free against the timestamp would drop that volume anyway. Every real
producer stamps the field, so this is a defensive path.

**Cost if wrong:** a volume with a malformed timestamp vanishes from the
metrics instead of showing its sizes. It also turns a future producer bug into
a silent absence, which reads as "not applicable".

### 11. `diskStats` rejects a filesystem reporting a zero total

`internal/shim/statfs.go` already did; the agent's own measurement did not, and
two measurement paths disagreeing about what a valid reading is was the only
cross-task inconsistency the whole-branch review found. Such a filesystem made
a host agent publish `free 0, size 0`, which reads as "completely full", fires
a permanent low-disk alert, and divides by zero in the `1 - free/size` query
this branch's own documentation recommends.

This changed the claim gate's behaviour: a zero-total workspace with
`minFreeDisk > 0` previously reported `free = 0` and declined every claim
forever; the gate now logs the error and claims. That is documented in
`docs/reference/configuration.md`.

**Cost if wrong:** a filesystem that legitimately reports a zero total is
omitted rather than reported as 0/0. Omission is this design's preferred
answer.

### 12. Four documentation findings were fixed rather than deferred

Two of them misled in the way §2 forbids. The stated freshness bound
understated the age an operator would see, so an alert tuned on it would
false-positive; and because a Kubernetes agent's ID carries the pod UID, a
restart retires the old ID's series while the new ID has none until a run
mounts the volume — so a single-replica rollout silently resolves a firing
low-disk alert. The documentation covered each half separately and never
joined them.

**Cost if wrong:** small prose edits on pages that were otherwise accurate.

### 13. The two unguarded fixes got tests, against the "one fix wave" rule

The final fix wave shipped two fixes without tests, the branch's only Critical
among them — a log write under the sampler mutex, which would have blocked the
heartbeat through a smaller door than the one the sampler was built to close.
A reviewer then showed both were testable cheaply: a blocking `slog.Handler`
guards the first, and on Linux `unix.Statfs("/proc")` reports `Blocks == 0`,
giving the second a real seam. No production code changed.

**Cost if wrong:** two more tests to maintain, one touching the global `slog`
default (so it must not run in parallel) and one Linux-only and skipped
elsewhere.

## Findings accepted without a fix

Each was raised by a reviewer, judged Minor, and triaged ship-as-is by the
whole-branch review. They are listed so a maintainer who trips over one knows
it was seen.

- `uint64(stat.Bavail)` is unchecked on FreeBSD and dragonfly, where the field
  is signed and can go negative past the root reserve. Pre-existing, and the
  project does not target those platforms.
- `TestDiskStats_ReportsBothAndTotalExceedsFree` asserts only `free <= total`,
  so a `(free, free)` return would pass. An honest fix needs an independent
  capacity source that is not portable.
- That test's non-zero assertion fails on a legitimately full temp volume.
- `diskfree_unix.go` shadows its named `err` result. Harmless; every return is
  explicit.
- The shim multiplies block counts by `Bsize` where POSIX `statvfs` uses
  `Frsize`. They are equal on every filesystem Linux containers run on, and the
  free/total ratio is right regardless.
- The `total == 0` guard in `internal/shim/statfs.go` has no test.
- A persistently failing path logs a warning every sample interval, on both
  agents.
- No test resamples after a failed `statfs` to pin that the in-flight flag is
  released on the error path. The code is correct by inspection.
- The first heartbeat can carry no reading, if the first sample has not landed.
  Absence, not a lie, and it self-heals within an interval.
- The Kubernetes measurement's 15s bound covers a whole pass, so one hung
  volume skips the rest for that run. The others keep their stamped older
  readings.
- `SetAgentStorage` is a second `UPDATE` on a row `TouchAgent` just updated, so
  the heartbeat path doubles its row-update rate. It could have ridden on
  `TouchAgent`.
- The post-run Kubernetes measurement uses `context.WithoutCancel`, so on
  SIGTERM it can hold up to 15s past drain before the pod delete and the volume
  release. Worth knowing when tuning `terminationGracePeriodSeconds`.
