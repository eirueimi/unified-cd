# Live Log Tail Scroll Race Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Keep the run detail log viewer pinned to live SSE output when a delayed programmatic `scroll` event arrives after the log extent has grown.

**Architecture:** Record the browser-clamped `scrollTop` produced by the viewer's own tail correction per log-box element. Lifecycle invalidation cancels future animation-frame writes but retains acknowledgment for writes that already ran; the scroll handler ignores only events observed at that element's recorded position, while any different position continues through the existing distance-from-tail user-intent calculation.

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

- Modify `web/src/routes/RunDetail.test.js`: model the real browser ordering in which a programmatic tail assignment queues a scroll event, later geometry grows, and the delayed event then runs, both within one view and across lifecycle invalidation during a view switch.
- Modify `web/src/routes/RunDetail.svelte`: track the latest programmatic scroll position per element and prevent only that position from disabling `logStick`, including across lifecycle invalidation.

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

Recorded RED: FAIL at `RunDetail.test.js:1518`. The delayed
`fireEvent.scroll(box)` computes the gap using `scrollHeight = 2000`, sets
`logStick = false`, and the already queued second-batch correction leaves
`scrollTop` at `1000` instead of advancing it to `2000`.

- [ ] **Step 3: Confirm the failure is the intended race**

The focused run received `1000` where line 1518 expected `2000`. This earlier
failure is the intended race: the scheduler's `canApply()` guard observes the
incorrectly cleared `logStick` before the pending second-batch frame can write.

### Task 2: Preserve Tail State for the Element-Scoped Programmatic Position

**Files:**
- Modify: `web/src/routes/RunDetail.svelte:170-175`
- Modify: `web/src/routes/RunDetail.svelte:277-297`
- Test: `web/src/routes/RunDetail.test.js`

**Interfaces:**
- Consumes: `logBox.scrollTop`, `logBox.scrollHeight`, `logScrollTop`, `logStick`, and `invalidateLogTailScroll()`.
- Produces: component-local `WeakMap<Element, number>` state; `applyLogTailScroll()` records the browser-read value per element and `onLogScroll()` distinguishes it from a different user position.

- [ ] **Step 1: Add the element-scoped programmatic position state**

Place the state beside the existing tail scheduler fields:

```js
let logStick = true; // keep auto-scrolling to the bottom while the user is there
const programmaticLogScrollTops = new WeakMap();
let tailScrollFrame = null;
let tailScrollGeneration = 0;
```

- [ ] **Step 2: Teach the scroll handler to preserve `logStick` at that exact position**

Replace the handler with:

```js
function onLogScroll(event) {
  const box = event.currentTarget;
  if (!box) return;
  const currentScrollTop = box.scrollTop;
  if (box === logBox) logScrollTop = currentScrollTop;
  if (currentScrollTop === programmaticLogScrollTops.get(box)) return;
  programmaticLogScrollTops.delete(box);
  if (box !== logBox) return;
  // Stick to the bottom only while the user is within ~2 rows of the end.
  logStick =
    box.scrollHeight - currentScrollTop - box.clientHeight <
    LOG_ROW_H * 2;
}
```

The strict equality is intentional: the stored value is read back from the
same browser element immediately after assignment. Matching does not consume
the marker, so queued or coalesced programmatic events remain acknowledged. A
real user move to any different position clears the marker and follows the
existing calculation.

- [ ] **Step 3: Retain applied markers during lifecycle invalidation**

Do not delete the element-scoped marker from `invalidateLogTailScroll()`.
Invalidation still advances the scheduler generation and cancels its pending
animation frame:

```js
tailScrollGeneration++;
```

An already-applied assignment has already queued an uncancellable browser
event. Element scoping prevents its position from affecting a replacement log
box, while retaining it allows the old event to be acknowledged after a
same-element view switch.

- [ ] **Step 4: Record the browser-clamped position in the tail assignment**

Update `applyLogTailScroll()`:

```js
function applyLogTailScroll() {
  if (!logBox) return;
  const box = logBox;
  box.scrollTop = box.scrollHeight;
  const currentScrollTop = box.scrollTop;
  programmaticLogScrollTops.set(box, currentScrollTop);
  logScrollTop = currentScrollTop;
}
```

- [ ] **Step 5: Run the regression test and verify the GREEN state**

Run:

```powershell
npm.cmd test -- --run src/routes/RunDetail.test.js -t "keeps tailing when a programmatic scroll event arrives after the log grows"
```

Expected: PASS. The delayed event matches the current element's recorded
position, leaves `logStick` enabled, and the third SSE batch schedules another
correction.

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

Actual TDD history used separate commits:

- `4c08e2e test: reproduce delayed log tail scroll event`
- `36942ad test: restore delayed scroll race timing`
- `2aca07c fix(web): preserve live log tail across delayed scroll events`

The RED regression and its timing correction were committed before the
production implementation rather than combined with it.

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

---

## Final Review Fix Wave

The whole-branch review found that lifecycle invalidation erased the marker
for a write that had already run, even though only its future animation frame
could be cancelled. It also found that the original regression's zero-height,
unclamped jsdom geometry did not enforce browser readback.

### Task 4: Reproduce the Lifecycle-Crossing Race

**Files:**
- Test: `web/src/routes/RunDetail.test.js`

- [x] Apply an old-view tail write and leave its next scheduled frame pending.
- [x] Start a step-view switch, which invalidates and cancels that future frame.
- [x] Let the new view's stats enlarge `scrollHeight`.
- [x] Dispatch the old queued `scroll` event twice to cover repeated or
  coalesced delivery.
- [x] Complete the switch's two-stage bottom correction.
- [x] Deliver the next live SSE batch and require a new tail frame.

Focused RED command:

```powershell
npm.cmd test -- --run src/routes/RunDetail.test.js -t "keeps tailing after an applied scroll event crosses a log view switch"
```

Recorded RED: exit code 1 at `RunDetail.test.js:1748`; the next live batch
left `frames` at length `0` instead of `1`. All earlier switch-completion and
browser-bottom assertions passed, isolating the stale event's incorrect
`logStick = false` transition.

### Task 5: Harden Browser-Clamped Readback

**Files:**
- Test: `web/src/routes/RunDetail.test.js`

The original same-view regression now exposes `clientHeight = 200`, clamps the
log box's `scrollTop` setter to `scrollHeight - clientHeight`, and asserts both
the clamped position and a zero tail gap after every correction. A mutation
that records the assigned `scrollHeight` instead of reading back `scrollTop`
therefore fails when the delayed event compares `800` with `1000`.

### Task 6: Separate Future Cancellation from Applied-Write Acknowledgment

**Files:**
- Modify: `web/src/routes/RunDetail.svelte`
- Modify: `docs/superpowers/specs/2026-07-27-live-log-tail-scroll-race-design.md`

The final implementation stores browser-read positions in a
`WeakMap<Element, number>`. Lifecycle invalidation still advances the
generation and cancels pending frames, but retains markers for applied writes.
Matching events do not consume the marker, so repeated/coalesced events remain
acknowledged. A different position deletes the marker and executes the
existing two-row user-intent calculation. Events from detached elements cannot
change current-view stickiness.

Focused GREEN result: the lifecycle regression passed (1 passed, 48 skipped).
The hardened same-view regression passed (1 passed, 48 skipped). The complete
`RunDetail` file passed 49 tests, the full Web UI suite passed 109 tests across
8 files, and the production build transformed 133 modules successfully.
