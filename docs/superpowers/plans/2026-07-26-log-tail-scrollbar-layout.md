# Log Tail Scrollbar Layout Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep the unwrapped Web UI log view at the true tail when a long line adds a horizontal scrollbar during browser layout.

**Architecture:** Add a private `scrollLogToBottom()` helper in `RunDetail.svelte` that waits for Svelte DOM updates and the next browser animation frame before assigning the final scroll position. Use it from both log-view switching and SSE stick-scroll paths, while leaving the existing `logStick` decision and virtualization model unchanged.

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

- `web/src/routes/RunDetail.svelte`: Owns the private tail-scroll helper and calls it from the existing view-switch and SSE follow paths.
- `web/src/routes/RunDetail.test.js`: Models delayed horizontal-scrollbar layout and verifies the resulting consumer-visible bottom distance.

### Task 1: Stabilize Tail Scrolling After Layout

**Files:**
- Modify: `web/src/routes/RunDetail.test.js`
- Modify: `web/src/routes/RunDetail.svelte`

**Interfaces:**
- Consumes: Svelte `tick(): Promise<void>`, browser `requestAnimationFrame(callback): number`, existing `logBox`, `logScrollTop`, and `logStick` state.
- Produces: private `async function scrollLogToBottom(): Promise<void>`.

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
    layoutSettled = true;
    const id = ++frameID;
    queueMicrotask(() => callback(performance.now()));
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

- [ ] **Step 3: Add the minimal deferred tail-scroll helper**

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

The non-browser fallback keeps jsdom and any non-visual environment usable;
real browsers always take the animation-frame path.

- [ ] **Step 4: Route both existing tail-scroll paths through the helper**

In `switchLogView()`, keep search behavior and replace the immediate
scroll block with:

```javascript
if (logQuery) runSearch();
await scrollLogToBottom();
```

In the SSE read loop, replace the `await tick()` and direct assignment with:

```javascript
if (logStick) {
  await scrollLogToBottom();
}
```

Do not change when `logStick` is calculated or reset.

- [ ] **Step 5: Run the regression test and focused component suite**

Run:

```powershell
npm.cmd test -- RunDetail.test.js -t "reapplies tail scrolling after a horizontal scrollbar changes the viewport height"
npm.cmd test -- RunDetail.test.js
```

Expected: the regression test passes, followed by all `RunDetail.test.js`
tests passing with zero failures.

- [ ] **Step 6: Inspect the implementation diff**

Run:

```powershell
git diff -- web/src/routes/RunDetail.svelte web/src/routes/RunDetail.test.js
git diff --check
```

Confirm that the diff contains only the geometry regression test, the helper,
and replacement of the two duplicate tail-scroll blocks.

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
npm.cmd test -- RunDetail.test.js -t "reapplies tail scrolling after a horizontal scrollbar changes the viewport height"
```

Expected: one matching test passes with zero failures.

- [ ] **Step 9: Commit the tested implementation**

```powershell
git add web/src/routes/RunDetail.svelte web/src/routes/RunDetail.test.js
git commit -m "fix(web): keep long log views at tail"
```

- [ ] **Step 10: Record final repository evidence**

Run:

```powershell
git status --short --branch
git log -2 --oneline
```

Expected: a clean `fix/log-tail-scrollbar-layout` worktree containing the
design commit and the implementation commit.
