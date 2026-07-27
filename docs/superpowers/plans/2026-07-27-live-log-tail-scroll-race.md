# Live Log Tail Scroll Race Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep the run detail log viewer pinned to live SSE output when a delayed programmatic `scroll` event arrives after the log extent has grown.

**Architecture:** Record the browser-clamped `scrollTop` produced by the viewer's own tail correction. The scroll handler will ignore only events observed at that recorded position; any different position continues through the existing distance-from-tail user-intent calculation.

**Tech Stack:** Svelte 5, JavaScript, Vitest 4, Testing Library, Vite 8

## Global Constraints

- Keep all repository text and commit messages in English.
- Work only in the isolated `fix/live-log-tail-race` worktree.
- Do not modify the unrelated `web/package-lock.json` change in the main checkout.
- Preserve the two-row tail threshold, two-stage layout correction, lifecycle invalidation, virtualization, SSE batching, filtering, and range-fetch behavior.
- Do not add dependencies or change user-facing APIs, DSL, configuration, examples, or templates.
- Prove the race with a failing test before changing `RunDetail.svelte`.

---

## File Structure

- Modify `web/src/routes/RunDetail.test.js`: model the real browser ordering in which a programmatic tail assignment queues a scroll event, another SSE batch grows the log, and the delayed event then runs.
- Modify `web/src/routes/RunDetail.svelte`: track the latest programmatic scroll position and prevent only that position from disabling `logStick`.

### Task 1: Reproduce the Delayed Programmatic Scroll Event

**Files:**
- Test: `web/src/routes/RunDetail.test.js`

**Interfaces:**
- Consumes: the existing `RunDetail` SSE reader, `requestAnimationFrame` scheduler, `.log-box` element, and `fireEvent.scroll`.
- Produces: regression test `keeps tailing when a programmatic scroll event arrives after the log grows`.

- [ ] **Step 1: Add the regression test inside `RunDetail — log tail view (auto-scroll after backfill)`**

Add a test that gates three SSE batches and controls both animation frames and
`scrollHeight`:

```js
it('keeps tailing when a programmatic scroll event arrives after the log grows', async () => {
  const originalRAF = globalThis.requestAnimationFrame;
  const frames = [];
  const enc = new TextEncoder();
  let scrollHeight = 1000;
  let readCount = 0;
  let releaseSecondBatch;
  let releaseThirdBatch;
  const secondBatchGate = new Promise((resolve) => {
    releaseSecondBatch = resolve;
  });
  const thirdBatchGate = new Promise((resolve) => {
    releaseThirdBatch = resolve;
  });

  Object.defineProperty(Element.prototype, 'scrollHeight', {
    configurable: true,
    get() {
      return this.classList?.contains('log-box') ? scrollHeight : 0;
    },
  });
  globalThis.requestAnimationFrame = (callback) => {
    frames.push(callback);
    return frames.length;
  };

  try {
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/events')) {
        return Promise.resolve({
          ok: true,
          status: 200,
          body: {
            getReader() {
              return {
                read: async () => {
                  readCount++;
                  if (readCount === 1) {
                    return {
                      done: false,
                      value: enc.encode(`data: ${JSON.stringify({
                        type: 'log',
                        seq: 1,
                        stepIndex: 0,
                        stream: 'stdout',
                        line: 'first',
                      })}\n\n`),
                    };
                  }
                  if (readCount === 2) {
                    await secondBatchGate;
                    return {
                      done: false,
                      value: enc.encode(`data: ${JSON.stringify({
                        type: 'log',
                        seq: 2,
                        stepIndex: 0,
                        stream: 'stdout',
                        line: 'second',
                      })}\n\n`),
                    };
                  }
                  if (readCount === 3) {
                    await thirdBatchGate;
                    return {
                      done: false,
                      value: enc.encode(`data: ${JSON.stringify({
                        type: 'log',
                        seq: 3,
                        stepIndex: 0,
                        stream: 'stdout',
                        line: 'third',
                      })}\n\n`),
                    };
                  }
                  return { done: true, value: undefined };
                },
              };
            },
          },
        });
      }
      if (u.includes('/steps')) return jsonResponse([]);
      if (u.includes('/approvals')) return jsonResponse([]);
      if (u.includes('/artifacts')) return jsonResponse([]);
      return jsonResponse({
        id: 'run-delayed-programmatic-scroll',
        status: 'Running',
        jobName: 'j',
        triggeredBy: 'x',
        createdAt: null,
        params: {},
      });
    });
    global.fetch = fetchMock;

    const { container } = render(RunDetail, {
      props: { params: { id: 'run-delayed-programmatic-scroll' } },
    });
    await vi.waitFor(() => expect(frames).toHaveLength(1));

    const box = container.querySelector('.log-box');
    frames.shift()(performance.now());
    await vi.waitFor(() => expect(frames).toHaveLength(1));
    expect(box.scrollTop).toBe(1000);

    scrollHeight = 2000;
    releaseSecondBatch();
    await vi.waitFor(() => expect(readCount).toBe(3));
    await fireEvent.scroll(box);

    frames.shift()(performance.now());
    await vi.waitFor(() => expect(box.scrollTop).toBe(2000));
    expect(frames).toHaveLength(0);

    scrollHeight = 3000;
    releaseThirdBatch();
    await vi.waitFor(() => expect(frames).toHaveLength(1));
    frames.shift()(performance.now());
    await vi.waitFor(() => expect(frames).toHaveLength(1));
    frames.shift()(performance.now());
    await vi.waitFor(() => expect(box.scrollTop).toBe(3000));
  } finally {
    restore();
    if (originalRAF) globalThis.requestAnimationFrame = originalRAF;
    else delete globalThis.requestAnimationFrame;
  }
});
```

- [ ] **Step 2: Run the focused test and verify the RED state**

Run:

```powershell
npm.cmd test -- --run src/routes/RunDetail.test.js -t "keeps tailing when a programmatic scroll event arrives after the log grows"
```

Expected: FAIL because the delayed `fireEvent.scroll(box)` computes the gap
using `scrollHeight = 2000`, sets `logStick = false`, and the third batch does
not add a new animation frame.

- [ ] **Step 3: Confirm the failure is the intended race**

Check that the assertion waiting for `frames` to contain the third-batch tail
correction fails. If the test fails earlier, correct the test fixture without
changing `RunDetail.svelte`, then rerun until the failure isolates the missing
programmatic-scroll distinction.

### Task 2: Preserve Tail State for the Recorded Programmatic Position

**Files:**
- Modify: `web/src/routes/RunDetail.svelte:170-175`
- Modify: `web/src/routes/RunDetail.svelte:277-297`
- Test: `web/src/routes/RunDetail.test.js`

**Interfaces:**
- Consumes: `logBox.scrollTop`, `logBox.scrollHeight`, `logScrollTop`, `logStick`, and `invalidateLogTailScroll()`.
- Produces: nullable component state `programmaticLogScrollTop: number | null`; `applyLogTailScroll()` records it and `onLogScroll()` distinguishes it from a different user position.

- [ ] **Step 1: Add the nullable programmatic position state**

Place the state beside the existing tail scheduler fields:

```js
let logStick = true; // keep auto-scrolling to the bottom while the user is there
let programmaticLogScrollTop = null;
let tailScrollFrame = null;
let tailScrollGeneration = 0;
```

- [ ] **Step 2: Teach the scroll handler to preserve `logStick` at that exact position**

Replace the handler with:

```js
function onLogScroll() {
  if (!logBox) return;
  const currentScrollTop = logBox.scrollTop;
  logScrollTop = currentScrollTop;
  if (currentScrollTop === programmaticLogScrollTop) return;
  programmaticLogScrollTop = null;
  // Stick to the bottom only while the user is within ~2 rows of the end.
  logStick =
    logBox.scrollHeight - currentScrollTop - logBox.clientHeight <
    LOG_ROW_H * 2;
}
```

The strict equality is intentional: the stored value is read back from the
same browser element immediately after assignment. A real user move to any
different position clears the marker and follows the existing calculation.

- [ ] **Step 3: Clear the marker during lifecycle invalidation**

At the start of `invalidateLogTailScroll()` add:

```js
programmaticLogScrollTop = null;
```

This prevents a position recorded for an old run or view from affecting a new
one.

- [ ] **Step 4: Record the browser-clamped position in the tail assignment**

Update `applyLogTailScroll()`:

```js
function applyLogTailScroll() {
  if (!logBox) return;
  logBox.scrollTop = logBox.scrollHeight;
  programmaticLogScrollTop = logBox.scrollTop;
  logScrollTop = programmaticLogScrollTop;
}
```

- [ ] **Step 5: Run the regression test and verify the GREEN state**

Run:

```powershell
npm.cmd test -- --run src/routes/RunDetail.test.js -t "keeps tailing when a programmatic scroll event arrives after the log grows"
```

Expected: PASS. The delayed event matches `programmaticLogScrollTop`, leaves
`logStick` enabled, and the third SSE batch schedules another correction.

- [ ] **Step 6: Run the complete RunDetail test file**

Run:

```powershell
npm.cmd test -- --run src/routes/RunDetail.test.js
```

Expected: all RunDetail tests pass, including user scroll opt-out, scheduler
reuse, rapid SSE coalescing, lifecycle invalidation, view switching, and
two-stage layout correction.

- [ ] **Step 7: Review the focused diff and commit the fix**

Run:

```powershell
git diff --check
git diff -- web/src/routes/RunDetail.svelte web/src/routes/RunDetail.test.js
git add web/src/routes/RunDetail.svelte web/src/routes/RunDetail.test.js
git commit -m "fix(web): preserve live log tail across delayed scroll events"
```

Expected: one implementation commit containing only the regression test and
the minimal component state changes.

### Task 3: Full and Live Verification

**Files:**
- Verify: `web/src/routes/RunDetail.svelte`
- Verify: `web/src/routes/RunDetail.test.js`

**Interfaces:**
- Consumes: the committed fix from Task 2 and the local controller at `http://localhost:8080`.
- Produces: test, build, and live browser evidence that the fix works without changing repository files.

- [ ] **Step 1: Run the full Web UI test suite**

Run:

```powershell
npm.cmd test
```

Expected: all Web UI test files pass.

- [ ] **Step 2: Run the production Web UI build**

Run:

```powershell
npm.cmd run build
```

Expected: Vite completes a production build without errors.

- [ ] **Step 3: Start the worktree UI on an isolated port**

Run from `web/`:

```powershell
npm.cmd run dev -- --host 127.0.0.1 --port 5174
```

Keep this process limited to the verification session. The Vite proxy forwards
`/api` to the controller at `http://localhost:8080`.

- [ ] **Step 4: Run `unity-build-android` through the worktree UI**

Open:

```text
http://localhost:5174/ui/#/jobs/unity-build-android/run
```

Authenticate with the development token already supplied for this local
environment. Keep the default `git_ref = main` and `development = true`, then
start the run.

- [ ] **Step 5: Measure live tail geometry while logs grow**

During the run, sample the unique `.log-box`:

```js
const box = document.querySelector('.log-box');
({
  scrollTop: box.scrollTop,
  scrollHeight: box.scrollHeight,
  clientHeight: box.clientHeight,
  tailGap: box.scrollHeight - box.scrollTop - box.clientHeight,
});
```

Expected: the log count and `scrollHeight` grow while `tailGap` remains zero
or returns to zero after each scheduled animation-frame correction. It must
not grow continuously as it did before the fix.

- [ ] **Step 6: Verify user opt-out and re-entry**

While the run is logging:

1. Scroll the log box upward by more than two rows.
2. Confirm new logs do not pull the viewport back to the bottom.
3. Return the log box to the bottom.
4. Confirm subsequent logs resume tail following.

Expected: the fix distinguishes the recorded programmatic position without
regressing deliberate user scrolling.

- [ ] **Step 7: Stop the temporary Vite process and inspect repository state**

Run:

```powershell
git status --short
git log --oneline -3
```

Expected: only the committed design, plan, regression test, and implementation
changes are present; `web/dist` remains ignored and no dependency or
package-lock changes were introduced.
