# Disk-headroom gating — decisions taken during implementation

Plan: `docs/superpowers/plans/2026-10-23-disk-gating.md`
Spec: `docs/superpowers/specs/2026-10-23-disk-gating-design.md`

The plan was executed task by task, each task implemented by one agent and
reviewed by another. Where the spec left a question open, or a review or an
implementer found the plan's premise wrong, it was ruled on rather than
parked, and every ruling is recorded here with what it costs if it turns out
to be wrong.

This file exists because the execution ledger is git-ignored scratch: without
it these decisions would die with the workspace, and a maintainer who disagrees
with one would have no way to find out it was a decision at all.

## Rulings

### 1. Defaults and startup behaviour (spec §7, questions 1 to 9)

The default for `minFreeDisk` is `1%` and the check is on. The user chose this
("1%とか"); the remaining open questions were answered with the spec's
recommendations on the user's instruction ("おすすめで進めて"). In particular a
malformed `UNIFIED_AGENT_MIN_FREE_DISK` (or `UNIFIED_K8S_MIN_FREE_DISK`) value
fails startup instead of being ignored, and the Kubernetes `workspaceSizeLimit`
defaults to unset.

**Cost if wrong:** the default and startup behaviour change in the agent
config tasks (T2, T5), and every deployment that never set `minFreeDisk` begins
gating on upgrade; the migration note lists what to check first.

### 2. Plan's worktree and branch names are superseded

Work happened in `.claude/worktrees/disk-gating` on branch `disk-gating`, not
the names the plan gives.

**Cost if wrong:** cosmetic.

### 3. The manual `kind` check is replaced by CI

The plan's manual Kubernetes check (task 9) needs `kind`, which is not
installed on the build machine. The repository's Kubernetes integration tests
run in CI on the pushed feature branch instead, and the manual host check ran
locally on Windows.

**Cost if wrong:** the eviction-message path is verified only by unit tests with
pod fixtures; no real kubelet eviction was observed before merge.

### 4. The Kubernetes agent config loader stays lenient about unknown keys

The plan assumed `k8sagent.loadYAML` rejects unknown keys. It does not (it uses
`yaml.Unmarshal`, and `rejectRemovedWorkspacePVC` exists precisely because of
that). It was left lenient: making it strict is an unrelated breaking change
for existing configs with stray keys.

**Cost if wrong:** a misspelled `minFreeDisk` or `workspaceSizeLimit` is
silently ignored (the 1% default applies, or the workspace stays uncapped). The
user documentation shows the exact key names and says so.

## Deferred, not fixed

Minor review findings left for follow-up, none affecting correctness of the
shipped behaviour:

- `DiskThreshold.String()` formats with `'g'`, so a tiny percentage such as
  `0.00001%` renders as `1e-05%` and does not round-trip.
- A percentage `Resolve` goes through `float64`, so it can be one byte under
  exact for some values.
- The `--min-free-disk=""` flag silently means unset (1%), and its help shows
  `(default "")`.
- The host regression test for "volumes are read from the sampler" guards
  against an inline `statfs` but not against a stalled sampler.
- Whitespace-only `UNIFIED_K8S_MIN_FREE_DISK` silently falls back to the default.
- The Kubernetes "workspaceSizeLimit unset" startup INFO also fires when every
  template sets its own limit.
- The Kubernetes probe-run warning prints an absolute threshold as raw bytes
  next to an IEC free-space figure.
- The Kubernetes `clearReusedWorkspace` exec is not wrapped in the eviction
  explanation, and scope pods use only the global workspace limit.
- No `kind` integration case covers a real eviction.
