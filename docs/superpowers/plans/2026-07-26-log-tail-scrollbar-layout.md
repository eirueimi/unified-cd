# Log Tail Scrollbar Layout Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep the unwrapped Web UI log view at the true tail when a long line adds a horizontal scrollbar during browser layout.

**Architecture:** Add private tail-scroll helpers in `RunDetail.svelte`. Log-view switching awaits two Svelte DOM update and browser animation-frame boundaries, assigning the final position after each: the first can materialize the virtualized long tail, and the second corrects its resulting horizontal scrollbar geometry. SSE stick-scrolling schedules and coalesces the same two-stage correction without awaiting an animation frame in the reader; callbacks re-check `logStick` and the captured run/view generation. Pending callbacks are cancelled or invalidated on teardown, run changes, and superseding view switches, and the sentinel is cleared on every completion or early return.

**Final race ordering:** SSE corrections use only `tailScrollGeneration` for
applicability; `windowFetchToken` also changes for ordinary same-view range
loads and must not cancel valid tail work. View switches perform both
unconditional corrections before re-running an active search, so an immediate
search match jump cannot be overwritten by tail placement. The caller
re-checks the captured lifecycle generation after the awaited correction and
does not start a stale search when a newer switch, run change, or teardown
invalidated that correction.

**Branch history through final re-review:** This work is a multi-commit branch,
not a two-commit design/implementation pair:

- `17cf744 docs: design stable log tail scrolling`
- `f3f8d12 docs: plan stable log tail scrolling`
- `2f7919c fix(web): keep long log views at tail`
- `970a05b fix(web): schedule log tail scrolling`
- `d602e27 fix(web): correct virtualized log tail layout`
- `1b2641d test(web): model deferred log scrollbar layout`
- `e340781 fix(web): preserve log tail correction races`

The final lifecycle regression and documentation commit follows these commits.

**Tech Stack:** Svelte 5, JavaScript, Vitest 4, Testing Library, jsdom, Vite 8

## Global Constraints

- Preserve unwrapped log lines and horizontal scrolling.
- Keep following SSE batches only while `logStick` is true.
- Do not change fetching, filtering, virtualization, search, or wrapping behavior.
- Follow strict TDD: observe the regression test fail before editing production code.
- Keep all repository text and commit messages in English.
- Preserve unrelated changes in the main working tree.

---

## File Structure

- `web/src/routes/RunDetail.svelte`: Owns the private unconditional view-switch helper and the non-blocking, coalesced SSE tail-scroll scheduler.
- `web/src/routes/RunDetail.test.js`: Models delayed horizontal-scrollbar layout, non-blocking SSE consumption, callback coalescing, manual scroll-away, same-view range-token movement, lifecycle invalidation, scheduler reuse, and view-search ordering.
- `docs/superpowers/specs/2026-07-26-log-tail-scrollbar-layout-design.md`: Records the final two-stage scheduler, lifecycle, and search-ordering contract.
- `docs/superpowers/plans/2026-07-26-log-tail-scrollbar-layout.md`: Records the final regression scope, inspection gates, and multi-commit branch evidence.

### Task 1: Stabilize Tail Scrolling After Layout

**Files:**
- Modify: `web/src/routes/RunDetail.test.js`
- Modify: `web/src/routes/RunDetail.svelte`

**Interfaces:**
- Consumes: Svelte `tick(): Promise<void>`, browser `requestAnimationFrame(callback): number`, existing `logBox`, `logScrollTop`, and `logStick` state.
- Produces: private `async function scrollLogToBottom(): Promise<void>` for view switches and a private non-blocking SSE tail-scroll scheduler.

- [ ] **Step 1: Add the failing browser-geometry regression test**

Add this test to the existing
`RunDetail — log tail view (auto-scroll after backfill)` suite. Preserve and
restore all prototype descriptors and the original animation-frame function
inside `finally`.

```javascript
it('reapplies tail scrolling after a horizontal scrollbar changes the viewport height', async () => {
  const descCH = Object.getOwnPropertyDescriptor(Element.prototype, 'clientHeight');
  const originalRAF = globalThis.requestAnimationFrame;
  let layoutSettled = false;
  let frameID = 0;

  Object.defineProperty(Element.prototype, 'clientHeight', {
    configurable: true,
    get() {
      if (!this.classList?.contains('log-box')) return 0;
      return layoutSettled ? 384 : 400;
    },
  });
  Object.defineProperty(Element.prototype, 'scrollTop', {
    configurable: true,
    get() { return this.__stubScrollTop || 0; },
    set(value) {
      if (!this.classList?.contains('log-box')) {
        this.__stubScrollTop = value;
        return;
      }
      this.__stubScrollTop = Math.min(
        value,
        this.scrollHeight - this.clientHeight,
      );
      layoutSettled = true;
    },
  });
  globalThis.requestAnimationFrame = (callback) => {
    const id = ++frameID;
    queueMicrotask(() => {
      layoutSettled = true;
      callback(performance.now());
    });
    return id;
  };

  try {
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/events')) return eventsResponseWithLogs(200, true);
      if (u.includes('/steps')) return jsonResponse([]);
      if (u.includes('/approvals')) return jsonResponse([]);
      if (u.includes('/artifacts')) return jsonResponse([]);
      return jsonResponse({
        id: 'run-scrollbar-tail',
        status: 'Succeeded',
        jobName: 'j',
        triggeredBy: 'x',
        createdAt: null,
        params: {},
      });
    });
    global.fetch = fetchMock;

    const { container } = render(RunDetail, {
      props: { params: { id: 'run-scrollbar-tail' } },
    });
    await vi.waitFor(() => {
      expect(container.querySelectorAll('.log-row').length).toBeGreaterThan(0);
    });

    const box = container.querySelector('.log-box');
    await vi.waitFor(() => expect(box.scrollTop).toBeGreaterThan(0));
    expect(box.scrollHeight - box.scrollTop - box.clientHeight).toBe(0);
  } finally {
    restore();
    if (descCH) Object.defineProperty(Element.prototype, 'clientHeight', descCH);
    else delete Element.prototype.clientHeight;
    if (originalRAF) globalThis.requestAnimationFrame = originalRAF;
    else delete globalThis.requestAnimationFrame;
  }
});
```

The production mutation this test catches is removal of the post-layout
tail-scroll step: the first assignment is clamped using `clientHeight = 400`,
then layout reduces it to `384`, leaving a literal 16-pixel bottom distance.

- [ ] **Step 2: Run the focused test and verify RED**

Run:

```powershell
npm.cmd test -- RunDetail.test.js -t "reapplies tail scrolling after a horizontal scrollbar changes the viewport height"
```

Expected: FAIL with a bottom-distance comparison equivalent to
`expected 16 to be 0`. A syntax error, timeout, or missing fixture is not the
required RED result and must be corrected before proceeding.

- [ ] **Step 3: Add deferred view-switch scrolling and non-blocking SSE scheduling**

Add the helper next to `onLogScroll()` in `RunDetail.svelte`:

```javascript
async function scrollLogToBottom() {
  await tick();
  await new Promise((resolve) => {
    if (typeof requestAnimationFrame === "function") {
      requestAnimationFrame(resolve);
    } else {
      resolve();
    }
  });
  if (!logBox) return;
  logBox.scrollTop = logBox.scrollHeight;
  logScrollTop = logBox.scrollTop;
}
```

Keep this awaited helper for unconditional current-view placement, and repeat
the render/frame/assignment sequence once after the first assignment so a
virtualized long tail can materialize and change the viewport geometry. Add a
separate scheduler for SSE batches: mark a pending callback before `tick()`,
coalesce redundant batches, then request each of the two correction frames
without awaiting either from the reader. Each callback must re-check
`logStick` and its captured run/view generation before assigning the tail,
and every stale or completed path must clear the pending sentinel. Invalidate
or cancel pending work during teardown, run changes, SSE restarts, and
superseding view switches. The non-browser fallback applies after `tick()`
without an animation frame.

- [ ] **Step 4: Route view switching and SSE following through their appropriate paths**

In `switchLogView()`, finish the unconditional correction first, then start
search only if the correction still belongs to the current lifecycle:

```javascript
const tailGeneration = tailScrollGeneration;
await scrollLogToBottom();
if (tailGeneration !== tailScrollGeneration) return;
if (logQuery) runSearch();
```

In the SSE read loop, schedule rather than await the post-layout scroll:

```javascript
if (logStick) {
  void scheduleLogTailScroll();
}
```

Do not change when `logStick` is calculated or reset; re-check it at deferred
callback time to avoid overriding a user scroll that occurs before the frame.

- [ ] **Step 5: Run the regression test and focused component suite**

Run:

```powershell
npm.cmd test -- RunDetail.test.js -t "reapplies tail scrolling after a horizontal scrollbar changes the viewport height"
npm.cmd test -- RunDetail.test.js -t "same-view range load advances|scrollbar-adjusted geometry|changes runs|component is destroyed|scheduler after an early-return|stale view search"
npm.cmd test -- RunDetail.test.js
```

Expected: the initial-backfill geometry test and all six final controlled
regressions pass, followed by the complete `RunDetail.test.js` suite with zero
failures.

- [ ] **Step 6: Inspect the implementation diff**

Run:

```powershell
git diff -- web/src/routes/RunDetail.svelte web/src/routes/RunDetail.test.js docs/superpowers/specs/2026-07-26-log-tail-scrollbar-layout-design.md docs/superpowers/plans/2026-07-26-log-tail-scrollbar-layout.md
git diff --check
```

Confirm that the diff contains the stale-search lifecycle re-check, the six
controlled final regressions, and documentation of scheduler applicability,
invalidation, cleanup/reuse, view geometry, search ordering, and the actual
multi-commit branch. Confirm no fetching, filtering, virtualization, search
payload, or wrapping behavior changed.

- [ ] **Step 7: Run the complete UI verification**

Run:

```powershell
npm.cmd test
npm.cmd run build
```

Expected: all UI test files pass and Vite completes the production build with
exit code 0.

- [ ] **Step 8: Re-run the focused regression test immediately before commit**

Run:

```powershell
npm.cmd test -- RunDetail.test.js -t "reapplies tail scrolling|does not block SSE chunks|does not pull the user back|starts an active-query|same-view range load advances|scrollbar-adjusted geometry|changes runs|component is destroyed|scheduler after an early-return|stale view search"
```

Expected: all ten tail lifecycle, geometry, non-blocking, coalescing, range,
invalidation, reuse, and search-ordering regressions pass with zero failures.

- [ ] **Step 9: Commit the tested implementation**

```powershell
git add web/src/routes/RunDetail.svelte web/src/routes/RunDetail.test.js docs/superpowers/specs/2026-07-26-log-tail-scrollbar-layout-design.md docs/superpowers/plans/2026-07-26-log-tail-scrollbar-layout.md
git commit -m "fix(web): close log tail lifecycle races"
```

- [ ] **Step 10: Record final repository evidence**

Run:

```powershell
git status --short --branch
git log --oneline b0ba3b4..HEAD
```

Expected: a clean `fix/log-tail-scrollbar-layout` worktree containing the
design and plan commits, each scheduler and geometry fix wave, the fidelity
test commit, the final race-ordering commit, and the final lifecycle regression
commit.
