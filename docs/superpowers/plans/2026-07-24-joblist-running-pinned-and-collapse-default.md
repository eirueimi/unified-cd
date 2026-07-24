# Jobs list: pinned Running section + collapse-by-default — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Add a pinned "Running" section atop the jobs list and make folders collapse by default (persisted per-browser), keeping the existing alphabetical name sort.

**Architecture:** Frontend only. `web/src/lib/utils.js` gains a `folderPaths(root)` helper; `web/src/routes/JobList.svelte` persists the user-expanded folder set in localStorage, derives the collapsed set as "all folders minus expanded" (default collapsed), and renders a pinned Running section from the already-polled active runs. `flattenJobTree`'s `collapsed`-set contract is unchanged.

**Tech Stack:** Svelte, Vite, Vitest (`npm test` = `vitest run`), @testing-library/svelte. Run from `web/`.

## Global Constraints

- Web workspace lives in `web/`; run web tests with `cd /c/Users/arimax/unified-cd-project/unified-cd/web && npm test` (a specific file: `npm test -- src/lib/utils.test.js`).
- Do NOT change `flattenJobTree`'s signature or its `collapsed`-set meaning (a set of collapsed folder paths) — `web/src/lib/utils.test.js` relies on it.
- localStorage access must be wrapped in try/catch and degrade gracefully (mirror `web/src/lib/theme.js`).
- Keep the existing alphabetical sort; add no sort control.
- Commit trailer (exact): `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`

---

### Task 1: `folderPaths(root)` helper in utils.js

**Files:**
- Modify: `web/src/lib/utils.js`
- Test: `web/src/lib/utils.test.js`

**Interfaces:**
- Consumes: the tree shape from `buildJobTree` (`{ folders: Map<name, node>, path, jobs }`).
- Produces: `export function folderPaths(root) -> string[]` — every folder path in the tree (depth-first), e.g. for jobs `team-a/build`, `team-a/edge/x`, `top` → `['team-a', 'team-a/edge']`.

- [ ] **Step 1: Write the failing test (RED)**

Add to `web/src/lib/utils.test.js` (it already imports from `./utils.js`; add `folderPaths` to the import):
```js
describe('folderPaths', () => {
  it('lists every folder path depth-first, excluding root and leaf jobs', () => {
    const jobs = [
      { name: 'team-a/build', path: 'team-a' },
      { name: 'team-a/edge/x', path: 'team-a/edge' },
      { name: 'top', path: '' },
    ];
    const paths = folderPaths(buildJobTree(jobs));
    expect(paths.sort()).toEqual(['team-a', 'team-a/edge']);
  });

  it('returns an empty array when there are no folders', () => {
    expect(folderPaths(buildJobTree([{ name: 'a', path: '' }]))).toEqual([]);
  });
});
```

- [ ] **Step 2: Run it — expect FAIL (RED)**

Run: `cd /c/Users/arimax/unified-cd-project/unified-cd/web && npm test -- src/lib/utils.test.js`
Expected: FAIL (`folderPaths` is not exported).

- [ ] **Step 3: Implement (GREEN)**

Add to `web/src/lib/utils.js`:
```js
export function folderPaths(root) {
  const paths = [];
  function walk(node) {
    for (const f of node.folders.values()) {
      paths.push(f.path);
      walk(f);
    }
  }
  walk(root);
  return paths;
}
```

- [ ] **Step 4: Run it — expect PASS (GREEN)**

Run: `cd /c/Users/arimax/unified-cd-project/unified-cd/web && npm test -- src/lib/utils.test.js`
Expected: PASS (new cases + existing `flattenJobTree`/`buildJobTree` tests unchanged).

- [ ] **Step 5: Commit** (`feat(web): add folderPaths helper for the jobs tree`).

---

### Task 2: JobList — collapse-by-default (persisted) + pinned Running section

**Files:**
- Modify: `web/src/routes/JobList.svelte`
- Test: `web/src/routes/JobList.test.js`

**Interfaces:**
- Consumes: `folderPaths` (Task 1), existing `buildJobTree`/`flattenJobTree`, `activeRunsByJob`, `jobs`.
- Produces: folders default-collapsed with an expanded-set persisted at localStorage key `ucd.joblist.expanded`; a pinned Running section above the tree.

- [ ] **Step 1: Write failing tests (RED)**

Add to `web/src/routes/JobList.test.js` (mirror the existing fetch-mock harness in that file — mock `/api/v1/jobs` and `/api/v1/runs/active`, `render(JobList)`, `vi.waitFor`). Between tests, reset storage: add `localStorage.clear()` to `beforeEach`. Three cases:

```js
// (a) folders collapse by default: a job inside a folder is NOT in the DOM until its folder is expanded.
// Mock /api/v1/jobs with [{name:'team-a/build', leaf:'build', path:'team-a', updatedAt:'2026-07-01T00:00:00Z'}]
// and /api/v1/runs/active => []. After render, assert the folder label 'team-a' is present but the job link
// text 'build' is absent (folder collapsed). (Query by the job's anchor / leaf text.)

// (b) expanded state persists: seed localStorage.setItem('ucd.joblist.expanded', JSON.stringify(['team-a']))
// BEFORE render; assert 'build' IS shown (folder starts expanded from stored state).

// (c) pinned Running section: mock /api/v1/runs/active => [{ id:'r1', jobName:'team-a/build', status:'Running', createdAt:'...' }].
// Assert that a running indicator for the job appears ABOVE the 'all jobs' tree region — e.g. the container's
// textContent shows the job name and a 'Running' badge even though 'team-a' is collapsed (proving the pinned
// section mirrors it). Assert that with active => [], no 'Running' pinned row is rendered.
```
Keep assertions resilient (query by visible text / roles, not brittle DOM structure). If a precise "above the tree" assertion is awkward in jsdom, assert the running job's name+badge is present while its folder is collapsed (that alone proves the pinned mirror, since the collapsed tree wouldn't show it).

- [ ] **Step 2: Run — expect FAIL (RED)**

Run: `cd /c/Users/arimax/unified-cd-project/unified-cd/web && npm test -- src/routes/JobList.test.js`
Expected: FAIL (folders currently start expanded; no pinned section).

- [ ] **Step 3: Collapse-by-default with persisted expanded set (GREEN part 1)**

In `web/src/routes/JobList.svelte` `<script>`, replace the `collapsed`/`rows`/`toggleFolder` block:
```js
  import { fmtTime, fmtRelative, statusBadge, buildJobTree, flattenJobTree, folderPaths } from '../lib/utils.js';
```
```js
  const EXPANDED_KEY = 'ucd.joblist.expanded';
  function loadExpanded() {
    try { return new Set(JSON.parse(localStorage.getItem(EXPANDED_KEY) || '[]')); }
    catch { return new Set(); }
  }
  function saveExpanded(s) {
    try { localStorage.setItem(EXPANDED_KEY, JSON.stringify([...s])); } catch { /* ignore */ }
  }
  // Folders default to collapsed: a folder is open only if the user has
  // expanded it (persisted). collapsed = every folder minus the expanded set,
  // so folders that appear later also default collapsed.
  let expanded = loadExpanded();
  $: tree = buildJobTree(jobs);
  $: collapsed = new Set(folderPaths(tree).filter((p) => !expanded.has(p)));
  $: rows = flattenJobTree(tree, collapsed, filterQuery);

  function toggleFolder(path) {
    if (expanded.has(path)) expanded.delete(path); else expanded.add(path);
    expanded = expanded;
    saveExpanded(expanded);
  }
```
The template already reads `collapsed.has(row.path)` for the folder caret and calls `toggleFolder(row.path)` — both keep working (`collapsed` is now derived).

- [ ] **Step 4: Pinned Running section (GREEN part 2)**

In the `<script>`, derive the active jobs (Running first, then Queued-only, then name):
```js
  $: runningJobs = jobs
    .filter((j) => activeRunsByJob[j.name]?.length)
    .sort((a, b) => {
      const ar = activeRunsByJob[a.name].some((r) => r.status === 'Running') ? 0 : 1;
      const br = activeRunsByJob[b.name].some((r) => r.status === 'Running') ? 0 : 1;
      return ar - br || a.name.localeCompare(b.name);
    });
```
In the template, immediately AFTER the `<input class="filter-input" …>` and BEFORE the `{#if !rows.length}` block, add the pinned section (reuse the existing `.badge`/`.badge-running`/`.badge-queued` classes and `fmtTime`):
```svelte
  {#if runningJobs.length}
    <div class="meta" style="margin:0.75rem 0 0.25rem;font-size:0.8rem">Running</div>
    <table>
      <tbody>
        {#each runningJobs as j (j.name)}
          {@const _runs = activeRunsByJob[j.name]}
          {@const _running = _runs.filter((r) => r.status === 'Running').length}
          {@const _queued = _runs.filter((r) => r.status === 'Queued').length}
          <tr>
            <td style="padding-left:0.75rem">
              <a href="#/jobs/{encodeURIComponent(j.name)}">{j.name}</a>
              {#if _running}
                <span class="badge badge-running" style="margin-left:0.5rem">● Running{_running > 1 ? ` (${_running})` : ''}</span>
              {/if}
              {#if _queued}
                <span class="badge badge-queued" style="margin-left:0.5rem" title="Waiting for an available agent">◷ Queued{_queued > 1 ? ` (${_queued})` : ''}</span>
              {/if}
            </td>
            <td class="meta">{fmtTime(j.updatedAt)}</td>
            <td><a href="#/jobs/{encodeURIComponent(j.name)}" class="btn">Runs →</a></td>
          </tr>
        {/each}
      </tbody>
    </table>
    <div class="meta" style="margin:0.75rem 0 0.25rem;font-size:0.8rem">All jobs</div>
  {/if}
```
(When `runningJobs.length` is 0, neither the "Running" nor the "All jobs" label renders — the tree shows on its own exactly as before.)

- [ ] **Step 5: Run — expect PASS (GREEN)**

Run: `cd /c/Users/arimax/unified-cd-project/unified-cd/web && npm test -- src/routes/JobList.test.js`
Expected: PASS. Then the whole web suite:
Run: `cd /c/Users/arimax/unified-cd-project/unified-cd/web && npm test`
Expected: PASS (existing JobList description/filter tests still green — the filter still force-expands folders, so any filter test that asserts a nested job is visible while filtering keeps working; if a pre-existing test relied on folders being expanded by default WITHOUT a filter, update it to seed `ucd.joblist.expanded` or apply a filter, and note why).

- [ ] **Step 6: Build + commit**

Run: `cd /c/Users/arimax/unified-cd-project/unified-cd/web && npm run build`
Expected: builds clean.
Commit (`feat(web): collapse job folders by default (persisted) + pin a Running section`).

---

## Self-Review

**Spec coverage:** pinned Running section (Running-first, mirror, no inline expander) → Task 2 Step 4; collapse-by-default + persisted expanded set + later folders default collapsed → Task 2 Step 3 + `folderPaths` Task 1; name sort unchanged (flattenJobTree untouched) → Global Constraints; filter still force-expands → unchanged `flattenJobTree`. ✓

**Placeholder scan:** concrete code for the helper, the script block, and the template; test cases enumerated with concrete mock shapes.

**Type consistency:** `folderPaths(root) -> string[]` (Task 1) consumed in Task 2's `collapsed` derivation. `flattenJobTree(tree, collapsed, filterQuery)` keeps its 3-arg `collapsed`-set contract. `activeRunsByJob` shape (`jobName -> Run[]`) reused from the existing `refreshActive`.

**Interaction risks:** the folder caret expression `collapsed.has(row.path) && !filterQuery ? '▸' : '▾'` and `open = q ? true : !collapsed.has(f.path)` inside `flattenJobTree` both continue to work because `collapsed` is still a set of collapsed paths — only its source changed (derived vs. mutated). The pinned rows deliberately omit `on:click={toggleExpand}` so a job never shows its recent-runs expander in two places.
