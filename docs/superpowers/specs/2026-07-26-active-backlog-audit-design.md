# Active Backlog Audit and Reorganization

**Date:** 2026-07-26
**Status:** Approved (brainstorming)

## Motivation

`TODO.md` currently mixes active defects, completed work, historical
investigation notes, test-environment observations, accepted behavior, and
feature requests. Several entries are internally inconsistent: a summary may
say that a range is complete while an individual entry still reads like an
open defect, and some completed entries retain long implementation proposals.
As a result, the file cannot answer the basic backlog question: what work is
still active?

The file should become a concise, English-only active backlog. Completed
history remains available in Git and does not need a second archive inside the
repository.

## Scope

Audit every current issue candidate in `TODO.md` against the repository at the
latest `main` revision. This includes:

- all numbered and lettered headings,
- the unnumbered Low-priority bullets,
- unresolved follow-ups embedded inside entries otherwise marked complete,
- environment and validation notes that may describe either a product issue
  or a one-time test setup problem.

The audit is documentation-only. It does not implement any backlog item or
change production behavior.

## Audit Method

For each candidate:

1. Identify the behavior claimed by the existing entry.
2. Inspect the current implementation, tests, user documentation, and relevant
   recent commits.
3. Record concrete current evidence, preferably an exact source or test path.
4. Assign one outcome:

   - **Resolved:** the current implementation and tests cover the claimed
     problem. Remove the entry.
   - **Accepted behavior:** the behavior is intentional and documented.
     Remove the entry.
   - **Environment-only:** the observation belongs to a historical validation
     environment rather than the product backlog. Remove the entry.
   - **Open:** the problematic behavior is still present. Retain a rewritten
     active entry.
   - **Partial:** some of the original issue was fixed, but a concrete
     remaining defect exists. Retain only the remaining defect.
   - **Needs verification:** static inspection cannot establish whether the
     current product still exhibits the behavior. Retain a bounded verification
     task that states the exact experiment and success condition.

Do not mark an item resolved solely because the old TODO text says
`IMPLEMENTED`, `implemented`, or `resolved`. Verify the current tree. Likewise,
do not retain an item solely because its old description sounds severe.

## Target File Structure

`TODO.md` will contain:

```markdown
# Active Backlog

This file contains active work only. Completed history is available in Git.
Last audited: 2026-07-26 against `1e46459`.

## Critical
...

## High
...

## Medium
...

## Low
...
```

Each entry uses one compact format:

```markdown
### <ID>. <Action-oriented title>

- **Status:** Open | Partial | Needs verification
- **Impact:** What fails and who or what is affected.
- **Evidence:** Current source/test/documentation paths that demonstrate the
  gap.
- **Done when:** An observable completion condition, including the required
  regression or integration coverage.
```

The title describes the remaining problem, not the historical state in which
it was discovered. Do not include a proposed implementation unless a specific
constraint is essential to the completion condition.

## Identity, Ordering, and Consolidation

- Preserve existing numeric or lettered IDs for traceability; gaps are
  expected after resolved entries are removed.
- Assign `L1`, `L2`, and so on to active Low-priority bullets that previously
  had no identifier.
- When two entries describe the same remaining root cause, consolidate them
  under the clearest existing ID and add a short `Legacy IDs` line.
- Keep distinct symptoms separate when they require independently testable
  fixes, even if they share a subsystem.
- Order entries first by severity and then by existing ID. Do not renumber the
  whole file.

## Content Removed from the Active Backlog

The rewrite removes:

- completed entries and implementation summaries,
- historical reproduction logs and commit-by-commit narratives,
- old fix proposals,
- validation-environment setup notes,
- lists of features verified as healthy,
- duplicated symptoms,
- Japanese text,
- branch names and stale source line numbers that no longer describe the
  current tree.

If an old investigation contains a constraint still necessary to avoid a
regression, preserve only that constraint in `Evidence` or `Done when`.

## Evidence Quality

Every retained entry must point to current evidence. Acceptable evidence
includes:

- a current function or type whose behavior directly exposes the gap,
- a current test that documents the missing or incorrect behavior,
- an explicit absence confirmed by a repository-wide symbol or route search,
- current documentation that contradicts implementation,
- a narrowly described runtime verification task when static evidence is not
  sufficient.

Avoid unqualified claims such as "probably", "suspected", or "may be broken".
Use `Needs verification` when runtime evidence is required.

## Verification

After the rewrite:

- review every original candidate in an audit matrix and account for its
  outcome before deleting the matrix from the working tree or keeping it only
  as ignored review scratch,
- confirm every retained entry has `Status`, `Impact`, `Evidence`, and
  `Done when`,
- confirm there are no completed entries in the active file,
- confirm there are no Japanese characters or completion markers such as
  `IMPLEMENTED`,
- confirm historical environment and healthy-feature sections are gone,
- run `git diff --check`,
- run a repository-wide reference scan for legacy TODO IDs that were merged
  or removed, ensuring no live documentation link is broken,
- run the repository's documentation-relevant and full CI checks before
  integration.

## Out of Scope

- Implementing any retained backlog item.
- Creating a separate completed-work archive.
- Moving the backlog into GitHub Issues or another tracker.
- Adding backlog-generation automation.
- Editing application code, examples, templates, or user documentation unless
  the audit uncovers a direct live reference that would otherwise become
  broken by the TODO reorganization.
