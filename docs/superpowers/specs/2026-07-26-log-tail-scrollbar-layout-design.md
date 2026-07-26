# Log Tail Scrollbar Layout Design

## Problem

The unwrapped log view can stop one horizontal-scrollbar height above the
actual bottom when its rendered tail contains a very long line. The log data
is complete and the final row is present, but the browser adds the horizontal
scrollbar after Svelte's DOM update. That reduces `clientHeight` after the
component has already assigned `scrollTop = scrollHeight`, leaving the scroll
position short of the new maximum.

The same timing-sensitive assignment is used when switching log views and
when following live SSE batches, so both paths can exhibit the problem.

## Requirements

- Preserve unwrapped log lines and horizontal scrolling.
- Keep following new SSE log batches only while the user is already near the
  tail.
- Place the log viewport at the true bottom after initial backfill, a log-view
  switch, or a followed SSE batch, including when a horizontal scrollbar
  appears during layout.
- Do not change log fetching, filtering, virtualization, search, or wrapping
  behavior.

## Design

Use two tail-scroll paths that share the final assignment:

1. Wait for Svelte's pending DOM update with `tick()`.
2. Wait for the browser's next animation frame so layout and scrollbar
   geometry are current.
3. Assign `scrollTop = scrollHeight` and mirror the resulting value into
   `logScrollTop`.
4. Repeat the render/frame/assignment sequence once. The first assignment can
   move the virtual window to the tail, materializing a long final row that
   adds a horizontal scrollbar only after that first assignment.

Log-view switches await this sequence because their tail placement is
unconditional for the current view. SSE batches must not await it: they mark
one coalesced pending post-layout scroll and immediately continue reading the
stream. The scheduled callback re-checks `logStick` and the captured run/view
generation before assigning the tail, so a user scroll away, a view switch,
or a run change invalidates stale work. Teardown, SSE restart, and every view
switch also cancel or invalidate any pending callback. The scheduler retains
its pending sentinel across both correction stages and clears it on every
completion or early-return path.

No observer or persistent event listener is needed. A `ResizeObserver` would
cover unrelated future resizes but would add lifecycle management and could
compete with intentional user scrolling. The animation-frame boundary
directly addresses the observed DOM-to-layout timing gap.

## Failure Handling

If the log element is no longer mounted, the generation is stale, or the user
has left the tail when the deferred frame runs, the scheduled callback exits
without changing state. This covers route changes and component teardown
without introducing a new error path. When animation frames are unavailable,
the scheduled path applies after `tick()` without a frame so non-visual
environments remain usable.

## Testing

Extend the existing `RunDetail` tail-view tests with browser geometry stubs
that model the observed sequence:

- the first tail assignment occurs against the pre-scrollbar viewport;
- that assignment materializes the virtualized long tail, then a horizontal
  scrollbar reduces `clientHeight`;
- only the second post-render/frame correction can reach the new maximum
  scroll position;
- suspended controlled frames do not block subsequent SSE chunks or terminal
  status handling, and redundant batches coalesce into one callback;
- a user who scrolls away before the controlled callback runs is not pulled
  back to the tail.

The regression test must fail against the current immediate-scroll
implementation and pass after the helper is used. Existing tests continue to
cover initial backfill, filtered view switching, live tailing, and respecting
manual scrolling.

Run the focused `RunDetail` test file, the full UI test suite, and the
production UI build before completion.
