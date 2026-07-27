# Live Log Tail Scroll Race Design

## Problem

The run detail log viewer can stop following the tail while a job is actively
emitting logs, even when the user has not scrolled away.

The SSE reader appends a batch and schedules a programmatic scroll to the
bottom. Assigning `logBox.scrollTop` causes the browser to queue a `scroll`
event. If another SSE batch increases `scrollHeight` before that event runs,
`onLogScroll` observes the old programmatic `scrollTop` against the new height.
It then incorrectly concludes that the user moved away from the tail and sets
`logStick` to `false`. Later batches keep arriving, but no further tail
correction is scheduled.

This race was reproduced against a running `unity-build-android` job: the log
count grew from 3 to more than 1,000 lines while `scrollTop` remained fixed and
the gap from the tail grew beyond 20,000 pixels.

## Goals

- Keep following live SSE logs when a delayed `scroll` event was caused by the
  viewer's own tail correction.
- Preserve the current behavior that a real user scroll away from the tail
  immediately disables automatic following.
- Preserve the current behavior that returning to the tail re-enables
  automatic following.
- Preserve run/view lifecycle invalidation and the two-stage scrollbar layout
  correction.
- Cover the browser event ordering with a regression test.

## Non-Goals

- Changing the two-row threshold used to decide whether the user is at the
  tail.
- Changing log virtualization, SSE batching, filtering, or range fetching.
- Adding new user-facing controls or preferences.

## Considered Approaches

### Record the last programmatic scroll position per element

When the viewer applies a tail correction, record the resulting `scrollTop`.
When `onLogScroll` later observes that exact position, treat the event as a
delayed consequence of the programmatic correction and keep `logStick`
unchanged. A different position is treated as user movement and uses the
existing distance-from-tail calculation.

This is the selected approach because it directly identifies the state
produced by the viewer, changes only the affected data flow, and does not hide
real user movement during a pending animation frame. The record is scoped to
the DOM element and survives lifecycle invalidation because invalidation can
cancel a future animation-frame write, but cannot cancel a `scroll` event
already queued by an applied write.

### Infer user intent from input events

Track wheel, pointer, touch, and keyboard input separately from `scroll`.
Although explicit, this expands the event surface and risks missing an input
method or accessibility interaction.

### Ignore all scroll events during tail correction

This is small but would also ignore a user who deliberately scrolls away while
a correction is pending, which would regress existing behavior.

## Design

Add a component-local `WeakMap` from log-box elements to the latest
browser-clamped `scrollTop` produced by `applyLogTailScroll`. Element scoping
prevents a position from a detached run's log box from acknowledging an event
on a replacement element.

`applyLogTailScroll` will:

1. Assign `logBox.scrollTop = logBox.scrollHeight`.
2. Read back the browser-clamped `scrollTop`.
3. Store that value for the current log-box element.
4. Copy it to `logScrollTop` as today.

`onLogScroll` will use `event.currentTarget`, updating `logScrollTop` only for
the currently bound log box. It will then compare that element's current
position with its recorded programmatic position:

- If they match, the handler leaves `logStick` unchanged because a delayed
  programmatic event must not be reclassified using newer log geometry. The
  marker remains recorded so multiple queued events, or one coalesced event
  observed after multiple writes, receive the same acknowledgment.
- If they differ, it clears the recorded position and performs the existing
  distance-from-tail calculation. This preserves immediate user opt-out and
  re-entry at the tail.

Lifecycle invalidation will continue to advance the generation and cancel
future animation-frame writes, but it will not clear markers for writes that
already ran. Old elements are weakly held, and events from an element that is
no longer the current `logBox` cannot change current-view stickiness.

The recorded value does not need an event counter. Browser scroll handlers
read the element's current position, not a historical position embedded in the
event. Keeping each element's latest viewer-produced position until a
different scroll position is observed covers coalesced, repeated, and delayed
programmatic events.

## Testing

Add a regression test that models the observed browser order:

1. Start at the tail with `logStick` enabled.
2. Deliver a live SSE batch and run the scheduled tail assignment.
3. Increase the modeled `scrollHeight` with another live batch before
   dispatching the delayed `scroll` event from the assignment.
4. Dispatch that event while `scrollTop` still equals the recorded
   programmatic position.
5. Deliver another live batch and assert that a new tail correction is still
   scheduled and reaches the new bottom.

Add a lifecycle regression that applies an old-view tail write, starts a log
view switch (invalidating future writes), enlarges the new view's geometry,
delivers the old queued event more than once, completes the switch, and then
proves the next SSE batch still schedules and reaches a browser-clamped bottom.

The modeled log box must expose a nonzero `clientHeight` and clamp its
`scrollTop` setter to `scrollHeight - clientHeight`. Bottom assertions use a
zero tail gap so recording the assigned `scrollHeight` instead of the
browser-read `scrollTop` fails.

The regression test must fail against the current implementation before the
fix is applied.

Existing tests must continue to prove that:

- a user scroll away before a pending frame runs is not pulled back;
- returning to the tail resumes following;
- rapid SSE chunks remain non-blocking and coalesced;
- run/view changes invalidate stale corrections;
- two-stage layout correction still reaches the real bottom.

Run the focused `RunDetail` suite, the full Web UI suite, and the production UI
build.

## Documentation Impact

This is an internal correctness fix with no user-facing contract, DSL,
configuration, example, or template change. No documentation outside this
design and implementation plan is required.
