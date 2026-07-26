# Log Search Focus Design

## Context

The run detail page searches the complete server-side log and returns absolute
row numbers. Its current `gotoMatch` implementation only assigns
`logBox.scrollTop` and relies on the browser to emit a later `scroll` event.
That event is expected to trigger the virtual window fetch. This indirect
dependency is unreliable: an off-window match can remain unloaded, so the
current match counter changes without the matching row being rendered,
centered, and highlighted.

## Goals

- Make every log-search match jump deterministic, including matches outside
  the currently loaded virtual window.
- Center and highlight the current matching row inside the log viewport.
- Preserve keyboard focus in the search input while navigating matches.

## Non-Goals

- Change the log search endpoint or its substring-matching semantics.
- Add regular expressions, whole-word search, or case-sensitive search.
- Alter the existing log virtualization model beyond deterministic match
  navigation.

## UI Design

`gotoMatch` will own the complete match-navigation sequence:

1. Normalize and store the requested match position.
2. Disable tail sticking.
3. Compute the centered scroll position from the match's absolute row.
4. Update the virtual scroll coordinates immediately.
5. Explicitly request a window that covers the target row through the existing
   range-loading path instead of waiting for a browser `scroll` event.
6. Wait for Svelte to render the fetched window.
7. Reapply the centered scroll position so the highlighted current row is
   stable after rendering.

The implementation will not call `focus()` on the log row or container.
Because navigation starts from the search input and does not move DOM focus,
the input remains focused for repeated Enter and Shift+Enter navigation.

Existing request tokens and view-switch guards remain authoritative. A match
jump that races with a step-filter change or reconnect must not install a
stale range.

## Testing

The UI regression test will use a match outside the initial log window. It
will assert that, without manually dispatching a `scroll` event:

- the required `/logs/range` request is made;
- the matching row is rendered and marked as current;
- the log viewport is centered on the matching row; and
- the search input retains `document.activeElement`.

Existing search debounce, result-cap, step-filter, virtualization, reconnect,
and view-switch tests must continue to pass.

## Delivery

The `unified-cd` repository will receive one focused implementation commit
containing the run-detail search regression test and deterministic navigation
fix.
