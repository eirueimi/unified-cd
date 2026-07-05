import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import { token, serverURL } from '../lib/api.js';
import RunDetail from './RunDetail.svelte';

// Regression test for TODO #10: RunDetail used to call init() twice on load
// (once via onMount(init), once via the reactive `$: runID, init()`), which
// opened two concurrent fetch()es to the run's `/events` endpoint. The second
// connection's `logLines = []` reset raced with / clobbered the first, so a
// terminal run's log panel got stuck at "Waiting for logs…". The fix keeps
// only the reactive re-init, so exactly one `/events` connection is opened
// per runID, and navigating to a new run opens exactly one more (not two).

function jsonResponse(body) {
  return Promise.resolve({
    ok: true,
    status: 200,
    json: async () => body,
    text: async () => JSON.stringify(body),
  });
}

// A `/events` response whose body stream ends immediately, so the SSE read
// loop in startSSE() exits on its own without needing to be aborted.
function emptyEventsResponse() {
  return Promise.resolve({
    ok: true,
    status: 200,
    body: {
      getReader() {
        return {
          read: async () => ({ done: true, value: undefined }),
        };
      },
    },
  });
}

function countEventsCalls(fetchMock) {
  return fetchMock.mock.calls.filter(([url]) => String(url).includes('/events')).length;
}

beforeEach(() => {
  token.set('');
  serverURL.set('http://localhost:8080');
});

describe('RunDetail — single SSE/events connection per run (TODO #10)', () => {
  it('opens exactly one connection to /events when the view loads', async () => {
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/events')) return emptyEventsResponse();
      if (u.includes('/steps')) return jsonResponse([]);
      if (u.includes('/approvals')) return jsonResponse([]);
      return jsonResponse({ id: 'run-1', status: 'Running', jobName: 'job-a', triggeredBy: 'x', createdAt: null, params: {} });
    });
    global.fetch = fetchMock;

    render(RunDetail, { props: { params: { id: 'run-1' } } });

    // Let the async init() chain (Promise.all + startSSE's fetch) settle.
    await vi.waitFor(() => {
      expect(countEventsCalls(fetchMock)).toBeGreaterThan(0);
    });

    expect(countEventsCalls(fetchMock)).toBe(1);
  });

  it('navigating from one run to another opens exactly one new /events connection, not two', async () => {
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/events')) return emptyEventsResponse();
      if (u.includes('/steps')) return jsonResponse([]);
      if (u.includes('/approvals')) return jsonResponse([]);
      const id = u.match(/\/runs\/([^/]+)/)?.[1];
      return jsonResponse({ id, status: 'Running', jobName: 'job-a', triggeredBy: 'x', createdAt: null, params: {} });
    });
    global.fetch = fetchMock;

    const { rerender } = render(RunDetail, { props: { params: { id: 'run-1' } } });

    await vi.waitFor(() => {
      expect(countEventsCalls(fetchMock)).toBe(1);
    });
    expect(fetchMock.mock.calls.some(([url]) => String(url).includes('/runs/run-1/events'))).toBe(true);

    await rerender({ params: { id: 'run-2' } });

    await vi.waitFor(() => {
      expect(countEventsCalls(fetchMock)).toBe(2);
    });

    // Exactly one of the two /events calls is for run-1, one for run-2 — the
    // old double-init bug would have produced two calls for run-1 alone
    // before ever navigating anywhere.
    const eventsUrls = fetchMock.mock.calls
      .map(([url]) => String(url))
      .filter((u) => u.includes('/events'));
    expect(eventsUrls.filter((u) => u.includes('/runs/run-1/events')).length).toBe(1);
    expect(eventsUrls.filter((u) => u.includes('/runs/run-2/events')).length).toBe(1);
  });
});

// Regression test for matrix-steps review finding C1: GetRunSteps now returns
// one row per (stepIndex, variant) for matrix/foreach steps, all sharing the
// same `index`. The step list used to key `{#each ...}` by `s.index` alone,
// which is a duplicate-key Svelte 5 runtime error whenever a step expands
// into more than one variant. Keying by `${index}/${variant}` fixes it.
describe('RunDetail — matrix/foreach steps with duplicate step index (C1)', () => {
  it('renders multiple variant rows sharing the same step index without throwing', async () => {
    const steps = [
      { index: 0, stageIndex: 0, name: 'build (linux, amd64)', variant: 'linux/amd64', status: 'Succeeded' },
      { index: 0, stageIndex: 0, name: 'build (linux, arm64)', variant: 'linux/arm64', status: 'Running' },
    ];
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/events')) return emptyEventsResponse();
      if (u.includes('/steps')) return jsonResponse(steps);
      if (u.includes('/approvals')) return jsonResponse([]);
      return jsonResponse({ id: 'run-1', status: 'Running', jobName: 'job-a', triggeredBy: 'x', createdAt: null, params: {} });
    });
    global.fetch = fetchMock;

    const { container } = render(RunDetail, { props: { params: { id: 'run-1' } } });

    await vi.waitFor(() => {
      const rows = container.querySelectorAll('.step-name');
      expect(rows.length).toBe(2);
    });
  });
});
