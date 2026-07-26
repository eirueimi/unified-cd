# Log Search Focus Design

## Context

The run detail page searches the complete server-side log and returns absolute
row numbers. Its current `gotoMatch` implementation only assigns
`logBox.scrollTop` and relies on the browser to emit a later `scroll` event.
That event is expected to trigger the virtual window fetch. In wrap mode, the
target window's cumulative line heights are not known yet, so `row * 20px`
can map back into the currently loaded window. The target range is then never
requested, leaving the match counter changed without the matching row being
rendered, centered, and highlighted.

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
3. Move the virtual coordinate to the target and explicitly request a window
   that covers its absolute row through the existing range-loading path.
   Suppress that request's ordinary viewport settle re-check so the previous
   wrapped window cannot replace the requested match window.
4. Wait for Svelte to render the fetched window and calculate its cumulative
   wrap offsets.
5. Compute the target pixel position from those offsets and apply the centered
   scroll position so the highlighted current row is stable.

The implementation will not call `focus()` on the log row or container.
Because navigation starts from the search input and does not move DOM focus,
the input remains focused for repeated Enter and Shift+Enter navigation.

Existing request tokens and view-switch guards remain authoritative. A match
jump that races with a step-filter change or reconnect must not install a
stale range.

## Testing

The UI regression test will use a match outside the initial log window with
long preceding lines in wrap mode. It will assert that:

- the required `/logs/range` request is made;
- the matching row is rendered and marked as current;
- the log viewport is centered using the loaded rows' cumulative wrap
  offsets; and
- the search input retains `document.activeElement`.

Existing search debounce, result-cap, step-filter, virtualization, reconnect,
and view-switch tests must continue to pass.

## Delivery

The `unified-cd` repository will receive one focused implementation commit
containing the run-detail search regression test and deterministic navigation
fix.
