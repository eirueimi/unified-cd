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

Add one small asynchronous helper that owns tail scrolling:

1. Wait for Svelte's pending DOM update with `tick()`.
2. Wait for the browser's next animation frame so layout and scrollbar
   geometry are current.
3. Assign `scrollTop = scrollHeight` and mirror the resulting value into
   `logScrollTop`.

Replace the duplicate immediate tail-scroll assignments in the log-view
switch and SSE follow paths with this helper. The existing `logStick` check
continues to decide whether SSE batches may trigger the helper, so manual
scrolling away from the tail remains respected.

No observer or persistent event listener is needed. A `ResizeObserver` would
cover unrelated future resizes but would add lifecycle management and could
compete with intentional user scrolling. The animation-frame boundary
directly addresses the observed DOM-to-layout timing gap.

## Failure Handling

If the log element is no longer mounted when the deferred frame runs, the
helper exits without changing state. This covers route changes and component
teardown without introducing a new error path.

## Testing

Extend the existing `RunDetail` tail-view tests with browser geometry stubs
that model the observed sequence:

- the first tail assignment occurs against the pre-scrollbar viewport;
- a horizontal scrollbar then reduces `clientHeight`;
- the animation-frame follow-up must reach the new maximum scroll position.

The regression test must fail against the current immediate-scroll
implementation and pass after the helper is used. Existing tests continue to
cover initial backfill, filtered view switching, live tailing, and respecting
manual scrolling.

Run the focused `RunDetail` test file, the full UI test suite, and the
production UI build before completion.
