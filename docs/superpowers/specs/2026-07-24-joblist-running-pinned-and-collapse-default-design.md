# Jobs list: pinned Running section + folders collapsed by default — Design

**Date:** 2026-07-24
**Scope:** Web UI only — `web/src/routes/JobList.svelte` and one helper in `web/src/lib/utils.js`. No backend/API change.

## Problem

On the jobs list (`JobList.svelte`), jobs are shown as a folder tree (grouped by the `/`-separated job name). Two ergonomics asks:

1. Bring active (Running/Queued) jobs to the top so they're immediately visible.
2. Collapse folders by default (the tree currently starts fully expanded).

These two conflict directly: if folders are collapsed by default, an active job inside a collapsed folder is hidden. Chosen resolution (**Option A**): a pinned "Running" section at the very top that mirrors every active job flat across folders (always visible), with the full folder tree below it collapsed by default.

Name sorting already exists (folders and jobs are alphabetical within each level, via `flattenJobTree`) and is kept as-is. No sort control is added.

## Design (Option A)

### 1. Pinned "Running" section

- Rendered between the filter input and the tree, only when at least one job has an active run.
- Contents: every job with a Running or Queued run (from the already-polled `activeRunsByJob`), flat (across folders), sorted **Running-first, then Queued-only, then by name**.
- Each row shows the job's full name (a link to its detail), its Running/Queued badge(s) (same markup as the tree rows), the updated time, and a "Runs →" link.
- The section is a mirror: the same jobs still appear in the tree below (with their badges). This duplication is intentional and matches the approved mockup — it guarantees active jobs are visible regardless of folder collapse.
- Rows link to the job detail; they do NOT host the inline "recent runs" expander (that stays only on the tree rows, to avoid a job appearing expanded in two places).

### 2. Folders collapsed by default, persisted

- Persist the set of folder paths the user has **expanded** (opened) in `localStorage` (key `ucd.joblist.expanded`, a JSON array), mirroring the `theme.js` load/save pattern (try/catch, ignore failures).
- A folder is open iff it is in the persisted expanded set. Any folder not in the set — including brand-new folders that appear later — defaults to **collapsed**. So "collapsed by default" and "remember what I opened" fall out of the same rule, with no separate first-run flag.
- Derivation: `collapsed = folderPaths(tree) \ expanded`. `flattenJobTree` keeps its existing `collapsed`-set contract (unchanged, so its tests stay valid). A new `folderPaths(root)` helper in `utils.js` lists every folder path in the tree.
- Toggling a folder adds/removes its path from the expanded set and re-saves. Filtering (a non-empty query) still force-expands everything, unchanged.

### 3. Name sort

Unchanged — alphabetical folders then alphabetical jobs within each level (`flattenJobTree`). No sort dropdown.

## Non-goals / tradeoffs

- No backend change; the Running set comes from the existing 5s poll of `/api/v1/runs/active`.
- The pinned Running section intentionally duplicates active jobs that also appear in the tree.
- "Running" here includes Queued jobs (the active set), matching the approved mockup, since a Queued job in a collapsed folder is hidden by the same problem.
- No per-user server-side persistence — collapse state is per-browser (localStorage), like the theme preference.

## Acceptance

- With folders present and no stored state, the tree renders with every folder collapsed.
- Expanding a folder, reloading the page, and the folder stays open (localStorage); a folder never opened stays collapsed, including folders that first appear after the state was saved.
- When a job has a Running or Queued run, it appears in a pinned section above the tree (Running-first ordering) as well as in the tree; when none are active, no pinned section renders.
- Filtering by text still expands all matching folders (unchanged).
