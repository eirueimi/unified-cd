# Log Search Focus Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make server-side log-search navigation load, center, and highlight an off-window match while preserving focus in the search input.

**Architecture:** Keep the existing server search and virtual-window model. Change `gotoMatch` so it explicitly asks `ensureRowsLoaded` for rows around the absolute match, waits for Svelte to calculate the loaded window's wrap offsets, and reapplies the centered position using those offsets.

**Tech Stack:** Svelte, JavaScript, Vitest, Testing Library, jsdom

## Global Constraints

- Do not change the `/logs/search` endpoint or matching semantics.
- Preserve the existing request-token and view-switch race guards.
- Do not move DOM focus away from `.log-search-input`.
- Use fixed-height positioning only before an off-window target's wrap offsets
  are available.

---

### Task 1: Deterministic Off-Window Match Navigation

**Files:**
- Modify: `web/src/routes/RunDetail.test.js:700-780`
- Modify: `web/src/routes/RunDetail.svelte:358-440`

**Interfaces:**
- Consumes: `ensureRowsLoaded(firstRow, lastRow, recheckViewport = true): Promise<void>`, `logWindow.totalCount`, `LOG_ROW_H`, `LOG_OVERSCAN`, and `tick()`.
- Produces: `centeredMatchScrollTop(rowIdx): number` and `gotoMatch(pos): Promise<void>`, which navigate to a wrapped match index and load its visible window.

- [x] **Step 1: Write the failing regression test**

Update the existing Enter-navigation test to enable wrap mode with 100
preceding rows that each occupy 50 visual lines. Focus the search input, press
Enter, and assert the range fetch, rendered current row, wrap-aware centered
scroll position, and active element:

```javascript
input.focus();
expect(document.activeElement).toBe(input);
await fireEvent.keyDown(input, { key: 'Enter' });

await vi.waitFor(() => {
  const rangeCallsAfter = fetchMock.mock.calls.filter((c) =>
    String(c[0]).includes('/logs/range'),
  ).length;
  expect(rangeCallsAfter).toBeGreaterThan(rangeCallsBefore);
});
await vi.waitFor(() => {
  const current = container.querySelector('.log-row-current');
  expect(current?.textContent).toContain('target row 100');
});
expect(box.scrollTop).toBe(Math.max(0, 100 * 50 * 20 - box.clientHeight / 2));
expect(document.activeElement).toBe(input);
```

- [x] **Step 2: Run the focused test to verify it fails**

Run:

```powershell
npm.cmd test -- src/routes/RunDetail.test.js -t "loads and centers a wrapped off-window match without moving input focus"
```

Expected: FAIL because the fixed-height jump is interpreted through the
currently loaded wrapped window, so no range request for row 100 occurs.

- [x] **Step 3: Implement deterministic match loading**

Add `centeredMatchScrollTop` to use exact cumulative offsets when the match is
in the loaded window. Make `gotoMatch` asynchronous, explicitly request rows
around the absolute match, await rendering, and re-center using those offsets:

```javascript
function centeredMatchScrollTop(rowIdx) {
  const windowIdx = rowIdx - logWindow.startRow;
  const targetY =
    logWrap && logOffsets && windowIdx >= 0 && windowIdx < logOffsets.length - 1
      ? logWindow.startRow * LOG_ROW_H + logOffsets[windowIdx]
      : rowIdx * LOG_ROW_H;
  return Math.max(0, targetY - logBox.clientHeight / 2);
}
async function gotoMatch(pos) {
  if (!logMatches.length) return;
  const n = logMatches.length;
  logMatchPos = ((pos % n) + n) % n;
  const rowIdx = logMatches[logMatchPos];
  logStick = false;
  await tick();
  if (!logBox) return;
  logScrollTop = rowIdx * LOG_ROW_H;
  await ensureRowsLoaded(
    Math.max(0, rowIdx - LOG_OVERSCAN),
    Math.min(logWindow.totalCount, rowIdx + LOG_OVERSCAN + 1),
    false,
  );
  await tick();
  if (!logBox) return;
  logBox.scrollTop = centeredMatchScrollTop(rowIdx);
  logScrollTop = logBox.scrollTop;
  await tick();
}
```

Update the adjacent comment to say that match navigation explicitly loads the
target virtual window through `ensureRowsLoaded`.

- [x] **Step 4: Run the focused and complete route tests**

Run:

```powershell
npm.cmd test -- src/routes/RunDetail.test.js -t "loads and centers a wrapped off-window match without moving input focus"
npm.cmd test -- src/routes/RunDetail.test.js
```

Expected: the focused test passes and all `RunDetail` tests pass.

- [x] **Step 5: Run UI build and whitespace checks**

Run:

```powershell
npm.cmd run build
git diff --check
```

Expected: both commands exit successfully.

- [ ] **Step 6: Commit the UI fix**

```powershell
git add web/src/routes/RunDetail.svelte web/src/routes/RunDetail.test.js docs/superpowers/plans/2026-07-26-log-search-focus.md
git commit -m "fix: load focused log search matches"
```
