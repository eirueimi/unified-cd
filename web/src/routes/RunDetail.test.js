import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import { token, serverURL, stderrPlain } from '../lib/api.js';
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

// Builds a fetch-mock handler pair for the windowed-log HTTP contract
// (`/logs/stats` and `/logs/range`) so Task 3+ tests can simulate a run whose
// total log size is much larger than the SSE backfill window. `totalCount` is
// what `/logs/stats` reports; `makeLine(seq)` builds one api.LogLine-shaped
// object for a given absolute row/seq number, used to answer `/logs/range`
// requests by slicing `[offset, offset+limit)`.
function statsAndRange(totalCount, makeLine) {
  return (url) => {
    const u = new URL(String(url), 'http://localhost');
    if (u.pathname.endsWith('/logs/stats')) {
      return jsonResponse({ count: totalCount, minSeq: 1, maxSeq: totalCount });
    }
    if (u.pathname.endsWith('/logs/range')) {
      const offset = Number(u.searchParams.get('offset') || '0');
      const limit = Number(u.searchParams.get('limit') || '1000');
      const end = Math.min(totalCount, offset + limit);
      const lines = [];
      for (let row = offset; row < end; row++) lines.push(makeLine(row));
      return jsonResponse(lines);
    }
    return null;
  };
}

// A `/events` response that streams `n` log lines in a single chunk, then ends.
// When `truncated` is true it leads with a "truncated" event, mimicking the
// server dropping older lines from a capped backfill.
function eventsResponseWithLogs(n, truncated = false) {
  const enc = new TextEncoder();
  let payload = truncated ? `data: ${JSON.stringify({ type: 'truncated' })}\n\n` : '';
  for (let i = 0; i < n; i++) {
    payload += `data: ${JSON.stringify({ type: 'log', seq: i + 1, stepIndex: 0, stream: 'stdout', line: 'line ' + i })}\n\n`;
  }
  let sent = false;
  return Promise.resolve({
    ok: true,
    status: 200,
    body: {
      getReader() {
        return {
          read: async () => {
            if (sent) return { done: true, value: undefined };
            sent = true;
            return { done: false, value: enc.encode(payload) };
          },
        };
      },
    },
  });
}

beforeEach(() => {
  token.set('');
  serverURL.set('http://localhost:8080');
  stderrPlain.set(false);
});

// A `/events` response streaming one stdout line and one stderr line, then ends.
function stdoutStderrEventsResponse() {
  const enc = new TextEncoder();
  const payload =
    `data: ${JSON.stringify({ type: 'log', seq: 1, stepIndex: 0, stream: 'stdout', line: 'out line' })}\n\n` +
    `data: ${JSON.stringify({ type: 'log', seq: 2, stepIndex: 0, stream: 'stderr', line: 'err line' })}\n\n`;
  let sent = false;
  return Promise.resolve({
    ok: true,
    status: 200,
    body: {
      getReader() {
        return {
          read: async () => {
            if (sent) return { done: true, value: undefined };
            sent = true;
            return { done: false, value: enc.encode(payload) };
          },
        };
      },
    },
  });
}

function runWithStderrLog() {
  return vi.fn((url) => {
    const u = String(url);
    if (u.includes('/events')) return stdoutStderrEventsResponse();
    if (u.includes('/steps')) return jsonResponse([]);
    if (u.includes('/approvals')) return jsonResponse([]);
    return jsonResponse({ id: 'run-1', status: 'Succeeded', jobName: 'j', triggeredBy: 'x', createdAt: null, params: {} });
  });
}

describe('RunDetail — single SSE/events connection per run (TODO #10)', () => {
  it("renders an Agent link when run.claimedBy is present", async () => {
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/events')) return emptyEventsResponse();
      if (u.includes('/steps')) return jsonResponse([]);
      if (u.includes('/approvals')) return jsonResponse([]);
      return jsonResponse({
        id: 'run-3', status: 'Running', jobName: 'job-a', triggeredBy: 'x',
        createdAt: null, params: {}, claimedBy: 'k8s-agent-1',
      });
    });
    global.fetch = fetchMock;

    const { container } = render(RunDetail, { props: { params: { id: 'run-3' } } });

    await vi.waitFor(() => {
      expect(container.querySelector('.run-agent')).toBeTruthy();
    });
    const link = container.querySelector('.run-agent a');
    expect(link).toBeTruthy();
    expect(link.getAttribute('href')).toBe('#/agents/k8s-agent-1');
    expect(link.textContent).toContain('k8s-agent-1');
  });

  it("omits the Agent row when run.claimedBy is absent", async () => {
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/events')) return emptyEventsResponse();
      if (u.includes('/steps')) return jsonResponse([]);
      if (u.includes('/approvals')) return jsonResponse([]);
      return jsonResponse({
        id: 'run-4', status: 'Queued', jobName: 'job-a', triggeredBy: 'x',
        createdAt: null, params: {},
      });
    });
    global.fetch = fetchMock;

    const { container } = render(RunDetail, { props: { params: { id: 'run-4' } } });

    await vi.waitFor(() => {
      expect(container.textContent).toContain('job-a');
    });
    expect(container.querySelector('.run-agent')).toBeFalsy();
  });


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

// Task 4: call step <-> child run link. Forward link on a step that has
// childRunId/callJobName, and a reverse "Called by" breadcrumb when the run
// itself was invoked by a `call` step in another run (run.calledBy).
describe('RunDetail — call step / child run link (Task 4)', () => {
  it('renders a link to the child run on a call step', async () => {
    const steps = [
      { index: 0, stageIndex: 0, name: 'call-child', status: 'Succeeded', childRunId: 'c1', callJobName: 'child-job' },
    ];
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/events')) return emptyEventsResponse();
      if (u.includes('/steps')) return jsonResponse(steps);
      if (u.includes('/approvals')) return jsonResponse([]);
      return jsonResponse({ id: 'run-1', status: 'Succeeded', jobName: 'job-a', triggeredBy: 'x', createdAt: null, params: {} });
    });
    global.fetch = fetchMock;

    const { container } = render(RunDetail, { props: { params: { id: 'run-1' } } });

    await vi.waitFor(() => {
      expect(container.querySelector('.step-name')).toBeTruthy();
    });

    const link = container.querySelector('a.call-link');
    expect(link).toBeTruthy();
    expect(link.getAttribute('href')).toBe('#/runs/c1');
    expect(link.textContent).toContain('child-job');
  });

  it("renders a 'Called by' breadcrumb when run.calledBy is present", async () => {
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/events')) return emptyEventsResponse();
      if (u.includes('/steps')) return jsonResponse([]);
      if (u.includes('/approvals')) return jsonResponse([]);
      return jsonResponse({
        id: 'run-2',
        status: 'Succeeded',
        jobName: 'child-job',
        triggeredBy: 'x',
        createdAt: null,
        params: {},
        calledBy: { parentRunId: 'p1', parentJobName: 'parent-job', stepName: 'call-child' },
      });
    });
    global.fetch = fetchMock;

    const { container } = render(RunDetail, { props: { params: { id: 'run-2' } } });

    await vi.waitFor(() => {
      expect(container.querySelector('.called-by')).toBeTruthy();
    });

    const link = container.querySelector('.called-by a');
    expect(link).toBeTruthy();
    expect(link.getAttribute('href')).toBe('#/runs/p1');
    expect(link.textContent).toContain('parent-job');
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

// Task 3: pre-execution planned steps display. GetRunSteps now returns the
// full planned flow, including not-yet-run steps with status "Pending" plus
// `kind`/`section`/`matrix`. The step list must show a waiting badge for
// Pending steps, display each step's kind, and split into "Steps" (section
// "main") and "Finally" (section "finally") headings — grouping stageIndex
// within each section so finally's stageIndex 0 doesn't collide with main's.
describe('RunDetail — planned steps display (Task 3)', () => {
  it('shows planned steps as Pending with kind, split into Steps/Finally sections', async () => {
    const steps = [
      { index: 0, stageIndex: 0, name: 'build', status: 'Succeeded', kind: 'run', section: 'main' },
      { index: 1, stageIndex: 1, name: 'restore-cache', status: 'Pending', kind: 'cache', section: 'main' },
      { index: 2, stageIndex: 0, name: 'notify', status: 'Pending', kind: 'run', section: 'finally' },
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
      expect(container.querySelectorAll('.step-name').length).toBe(3);
    });

    // Section headings: "Steps" for main, "Finally" for finally.
    const headings = [...container.querySelectorAll('h2')].map((h) => h.textContent);
    expect(headings).toContain('Steps');
    expect(headings).toContain('Finally');

    // restore-cache is Pending with a waiting badge and shows its kind.
    const rows = [...container.querySelectorAll('.step-row, .step-row-indented')];
    const cacheRow = rows.find((r) => r.querySelector('.step-name')?.textContent === 'restore-cache');
    expect(cacheRow).toBeTruthy();
    expect(cacheRow.querySelector('.badge-pending')).toBeTruthy();
    expect(cacheRow.textContent).toContain('Pending');
    expect(cacheRow.querySelector('.step-kind')?.textContent).toContain('cache');

    // notify is under the Finally heading.
    const notifyRow = rows.find((r) => r.querySelector('.step-name')?.textContent === 'notify');
    expect(notifyRow).toBeTruthy();
    expect(notifyRow.querySelector('.step-kind')?.textContent).toContain('run');

    // Finally heading appears after notify's row in document order.
    const finallyHeading = [...container.querySelectorAll('h2')].find((h) => h.textContent === 'Finally');
    expect(finallyHeading.compareDocumentPosition(notifyRow) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });
});

// A huge log (e.g. Unity's `-logFile -`) used to render every line as a DOM
// node, freezing the tab. RunDetail now virtualizes the log: it ingests every
// line (the "N lines" counter reflects the full total) but only keeps a small
// window of rows in the DOM.
describe('RunDetail — log virtualization', () => {
  it('renders only a window of rows for a large log, not every line', async () => {
    const N = 500;
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/events')) return eventsResponseWithLogs(N);
      if (u.includes('/steps')) return jsonResponse([]);
      if (u.includes('/approvals')) return jsonResponse([]);
      return jsonResponse({ id: 'run-1', status: 'Succeeded', jobName: 'job-a', triggeredBy: 'x', createdAt: null, params: {} });
    });
    global.fetch = fetchMock;

    const { container } = render(RunDetail, { props: { params: { id: 'run-1' } } });

    // All N lines are ingested (the counter shows the full total)...
    await vi.waitFor(() => {
      expect(container.textContent).toContain(`${N} lines`);
    });

    // ...but only a small window of rows is materialized in the DOM.
    const rows = container.querySelectorAll('.log-row');
    expect(rows.length).toBeGreaterThan(0);
    expect(rows.length).toBeLessThan(N);
    expect(rows.length).toBeLessThanOrEqual(60);
  });

  it('searches the full log (including off-screen lines) and highlights the match', async () => {
    const N = 500;
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/events')) return eventsResponseWithLogs(N);
      if (u.includes('/steps')) return jsonResponse([]);
      if (u.includes('/approvals')) return jsonResponse([]);
      return jsonResponse({ id: 'run-1', status: 'Succeeded', jobName: 'job-a', triggeredBy: 'x', createdAt: null, params: {} });
    });
    global.fetch = fetchMock;

    const { container } = render(RunDetail, { props: { params: { id: 'run-1' } } });
    await vi.waitFor(() => {
      expect(container.textContent).toContain(`${N} lines`);
    });

    // "line 123" is off-screen (row 123 is far below the initial window) yet the
    // in-app search finds it — proving search covers the whole log, not just the
    // rendered rows — and highlights it.
    const input = container.querySelector('.log-search-input');
    await fireEvent.input(input, { target: { value: 'line 123' } });

    await vi.waitFor(() => {
      expect(container.textContent).toContain('1 / 1');
      expect(container.querySelector('mark.log-hit')).toBeTruthy();
    });
  });

  // Task 3: truncation is no longer surfaced as a banner — the windowed log
  // model means the scrollbar itself spans the FULL server-side log (via
  // logWindow.totalCount) and older lines are reachable by scrolling up
  // (ensureRowsLoaded fetches them), so there's nothing to warn about. The
  // SSE "truncated" event is still consumed (it just no longer renders a
  // banner); the backfilled lines still show up normally.
  it('renders no truncation banner when the server drops older backfill lines (windowed viewer supersedes it)', async () => {
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/events')) return eventsResponseWithLogs(10, true);
      if (u.includes('/steps')) return jsonResponse([]);
      if (u.includes('/approvals')) return jsonResponse([]);
      return jsonResponse({ id: 'run-1', status: 'Succeeded', jobName: 'job-a', triggeredBy: 'x', createdAt: null, params: {} });
    });
    global.fetch = fetchMock;

    const { container } = render(RunDetail, { props: { params: { id: 'run-1' } } });

    await vi.waitFor(() => {
      expect(container.querySelector('.log-row')).toBeTruthy();
    });
    expect(container.querySelector('.log-truncated')).toBeFalsy();
  });

  it('ingests a full chunk of logs even when it ends with a terminal status', async () => {
    // Logs and the terminal status arrive in ONE chunk. The batched ingestion
    // must flush every log line before acting on the status, so the tail of a
    // completed run's log is not dropped.
    const N = 40;
    const enc = new TextEncoder();
    let payload = '';
    for (let i = 0; i < N; i++) {
      payload += `data: ${JSON.stringify({ type: 'log', seq: i + 1, stepIndex: 0, stream: 'stdout', line: 'line ' + i })}\n\n`;
    }
    payload += `data: ${JSON.stringify({ type: 'status', status: 'Succeeded' })}\n\n`;
    let sent = false;
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
                  if (sent) return { done: true, value: undefined };
                  sent = true;
                  return { done: false, value: enc.encode(payload) };
                },
              };
            },
          },
        });
      }
      if (u.includes('/steps')) return jsonResponse([]);
      if (u.includes('/approvals')) return jsonResponse([]);
      return jsonResponse({ id: 'run-1', status: 'Running', jobName: 'job-a', triggeredBy: 'x', createdAt: null, params: {} });
    });
    global.fetch = fetchMock;

    const { container } = render(RunDetail, { props: { params: { id: 'run-1' } } });

    await vi.waitFor(() => {
      expect(container.textContent).toContain(`${N} lines`);
    });
  });

  it('toggles line wrapping (and persists the choice)', async () => {
    localStorage.removeItem('ecd_log_wrap');
    const N = 30;
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/events')) return eventsResponseWithLogs(N);
      if (u.includes('/steps')) return jsonResponse([]);
      if (u.includes('/approvals')) return jsonResponse([]);
      return jsonResponse({ id: 'run-1', status: 'Succeeded', jobName: 'job-a', triggeredBy: 'x', createdAt: null, params: {} });
    });
    global.fetch = fetchMock;

    const { container } = render(RunDetail, { props: { params: { id: 'run-1' } } });
    await vi.waitFor(() => {
      expect(container.querySelector('.log-row')).toBeTruthy();
    });

    // Default: no wrapping.
    expect(container.querySelector('.log-box.wrap')).toBeFalsy();
    expect(container.querySelector('.log-row-wrap')).toBeFalsy();

    await fireEvent.click(container.querySelector('.log-wrap-btn'));

    await vi.waitFor(() => {
      expect(container.querySelector('.log-box.wrap')).toBeTruthy();
      expect(container.querySelector('.log-row-wrap')).toBeTruthy();
    });
    expect(localStorage.getItem('ecd_log_wrap')).toBe('1');
  });
});

// Task 3: the log data layer is now a single contiguous WINDOW over the full
// server-side log, not a full in-memory array. Scrolling outside the current
// window must fetch a fresh range from the server; live lines while scrolled
// away from the tail must only grow the total (scrollbar length), not fetch.
describe('RunDetail — windowed log data model (Task 3)', () => {
  let descST, descSH;
  beforeEach(() => {
    descST = Object.getOwnPropertyDescriptor(Element.prototype, 'scrollTop');
    descSH = Object.getOwnPropertyDescriptor(Element.prototype, 'scrollHeight');
    Object.defineProperty(Element.prototype, 'scrollTop', {
      configurable: true,
      get() { return this.__stubScrollTop || 0; },
      set(v) { this.__stubScrollTop = v; },
    });
    Object.defineProperty(Element.prototype, 'scrollHeight', {
      configurable: true,
      get() { return this.classList && this.classList.contains('log-box') ? 4000 : 0; },
    });
  });
  const restore = () => {
    if (descST) Object.defineProperty(Element.prototype, 'scrollTop', descST);
    if (descSH) Object.defineProperty(Element.prototype, 'scrollHeight', descSH);
  };

  it('scrolling above the window fetches an earlier range', async () => {
    try {
      const TOTAL = 50000;
      const BACKFILL = 200;
      const makeLine = (row) => ({
        seq: row + 1, stepIndex: 0, stream: 'stdout', line: 'row ' + row,
      });
      const statsRange = statsAndRange(TOTAL, makeLine);
      // SSE backfill: tail 200 lines, absolute rows 49800..49999.
      const enc = new TextEncoder();
      let payload = '';
      for (let row = TOTAL - BACKFILL; row < TOTAL; row++) {
        payload += `data: ${JSON.stringify({ type: 'log', ...makeLine(row) })}\n\n`;
      }
      let sent = false;
      const eventsResp = Promise.resolve({
        ok: true, status: 200,
        body: { getReader() { return { read: async () => sent ? { done: true, value: undefined } : (sent = true, { done: false, value: enc.encode(payload) }) } } },
      });
      const fetchMock = vi.fn((url) => {
        const u = String(url);
        if (u.includes('/events')) return eventsResp;
        const sr = statsRange(url);
        if (sr) return sr;
        if (u.includes('/steps')) return jsonResponse([]);
        if (u.includes('/approvals')) return jsonResponse([]);
        if (u.includes('/artifacts')) return jsonResponse([]);
        return jsonResponse({ id: 'run-scroll', status: 'Running', jobName: 'j', triggeredBy: 'x', createdAt: null, params: {} });
      });
      global.fetch = fetchMock;

      const { container } = render(RunDetail, { props: { params: { id: 'run-scroll' } } });
      await vi.waitFor(() => {
        expect(container.querySelectorAll('.log-row').length).toBeGreaterThan(0);
      });

      const box = container.querySelector('.log-box');
      box.scrollTop = 0;
      await fireEvent.scroll(box);

      await vi.waitFor(() => {
        expect(fetchMock.mock.calls.some((c) => {
          const cu = String(c[0]);
          return cu.includes('/logs/range') && cu.includes('offset=0');
        })).toBe(true);
      });

      await vi.waitFor(() => {
        const texts = [...container.querySelectorAll('.log-row')].map((r) => r.textContent);
        expect(texts.some((t) => t.includes('row 0'))).toBe(true);
      });
    } finally {
      restore();
    }
  });

  it('live lines while scrolled up only grow the total, without fetching a range', async () => {
    try {
      const TOTAL = 300;
      const makeLine = (row) => ({
        seq: row + 1, stepIndex: 0, stream: 'stdout', line: 'row ' + row,
      });
      const statsRange = statsAndRange(TOTAL, makeLine);
      // SSE: backfill the tail, then (after the initial read) stream one more
      // live line — but only once the test has scrolled away from the tail.
      const enc = new TextEncoder();
      let backfillPayload = '';
      for (let row = 0; row < TOTAL; row++) {
        backfillPayload += `data: ${JSON.stringify({ type: 'log', ...makeLine(row) })}\n\n`;
      }
      let readCount = 0;
      let releaseLive = null;
      const liveGate = new Promise((res) => { releaseLive = res; });
      const eventsResp = Promise.resolve({
        ok: true, status: 200,
        body: {
          getReader() {
            return {
              read: async () => {
                readCount++;
                if (readCount === 1) {
                  return { done: false, value: enc.encode(backfillPayload) };
                }
                if (readCount === 2) {
                  await liveGate;
                  const liveLine = JSON.stringify({ type: 'log', seq: TOTAL + 1, stepIndex: 0, stream: 'stdout', line: 'live line' });
                  return { done: false, value: enc.encode(`data: ${liveLine}\n\n`) };
                }
                return { done: true, value: undefined };
              },
            };
          },
        },
      });
      const fetchMock = vi.fn((url) => {
        const u = String(url);
        if (u.includes('/events')) return eventsResp;
        const sr = statsRange(url);
        if (sr) return sr;
        if (u.includes('/steps')) return jsonResponse([]);
        if (u.includes('/approvals')) return jsonResponse([]);
        if (u.includes('/artifacts')) return jsonResponse([]);
        return jsonResponse({ id: 'run-live', status: 'Running', jobName: 'j', triggeredBy: 'x', createdAt: null, params: {} });
      });
      global.fetch = fetchMock;

      const { container } = render(RunDetail, { props: { params: { id: 'run-live' } } });
      await vi.waitFor(() => {
        expect(container.textContent).toContain(`${TOTAL} lines`);
      });

      // Scroll away from the tail (releases stick) before the live line lands.
      const box = container.querySelector('.log-box');
      box.scrollTop = 0;
      await fireEvent.scroll(box);
      await vi.waitFor(() => {
        expect(fetchMock.mock.calls.some((c) => String(c[0]).includes('/logs/range'))).toBe(true);
      });
      const rangeCallsBefore = fetchMock.mock.calls.filter((c) => String(c[0]).includes('/logs/range')).length;

      // Now let the live line arrive while scrolled away from the tail.
      releaseLive();
      await vi.waitFor(() => {
        expect(container.textContent).toContain(`${TOTAL + 1} lines`);
      });

      // No additional range fetch was triggered by the live line itself.
      const rangeCallsAfter = fetchMock.mock.calls.filter((c) => String(c[0]).includes('/logs/range')).length;
      expect(rangeCallsAfter).toBe(rangeCallsBefore);
    } finally {
      restore();
    }
  });
});

// The controller's UNIFIED_LOG_STDERR_PLAIN setting reaches the UI via the
// `stderrPlain` store (loaded from /api/v1/ui-config at startup). Default:
// stderr is red (.log-stderr). When the controller enables it, stderr renders
// the same color as stdout (.log-stdout), with no per-user toggle in the UI.
describe('RunDetail — stderr color (controller stderrPlain)', () => {
  it('renders stderr red by default (.log-stderr)', async () => {
    global.fetch = runWithStderrLog();
    const { container } = render(RunDetail, { props: { params: { id: 'run-1' } } });
    await vi.waitFor(() => {
      expect(container.querySelector('.log-row')).toBeTruthy();
    });
    expect(container.querySelector('.log-stderr')).toBeTruthy();
  });

  it('renders stderr the same as stdout when stderrPlain is enabled (no .log-stderr)', async () => {
    stderrPlain.set(true);
    global.fetch = runWithStderrLog();
    const { container } = render(RunDetail, { props: { params: { id: 'run-1' } } });
    await vi.waitFor(() => {
      expect(container.querySelector('.log-row')).toBeTruthy();
    });
    // No line is styled as stderr; both lines use the stdout class.
    expect(container.querySelector('.log-stderr')).toBeFalsy();
    expect(container.querySelectorAll('.log-stdout').length).toBeGreaterThanOrEqual(2);
  });
});

describe('RunDetail — artifacts', () => {
  it('lists run artifacts and downloads one on click', async () => {
    // jsdom lacks blob-URL plumbing; stub it.
    const origCreate = URL.createObjectURL;
    const origRevoke = URL.revokeObjectURL;
    URL.createObjectURL = vi.fn(() => 'blob:x');
    URL.revokeObjectURL = vi.fn();

    let downloadUrl = null;
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/events')) return emptyEventsResponse();
      if (u.includes('/artifacts/')) {
        downloadUrl = u;
        return Promise.resolve({ ok: true, status: 200, blob: async () => new Blob(['data']) });
      }
      if (u.endsWith('/artifacts')) return jsonResponse([{ name: 'build' }, { name: 'report' }]);
      if (u.includes('/steps')) return jsonResponse([]);
      if (u.includes('/approvals')) return jsonResponse([]);
      return jsonResponse({ id: 'run-1', status: 'Succeeded', jobName: 'job-a', triggeredBy: 'x', createdAt: null, params: {} });
    });
    global.fetch = fetchMock;

    const { container } = render(RunDetail, { props: { params: { id: 'run-1' } } });
    await vi.waitFor(() => {
      expect(container.querySelectorAll('.artifact-item').length).toBe(2);
    });
    const first = container.querySelector('.artifact-item');
    expect(first.textContent).toContain('build');

    await fireEvent.click(first);
    await vi.waitFor(() => {
      expect(downloadUrl).toContain('/runs/run-1/artifacts/build');
    });

    URL.createObjectURL = origCreate;
    URL.revokeObjectURL = origRevoke;
  });
});

// The SSE backfill for a large log keeps the TAIL (server: sseBackfillLimit +
// "truncated" event). That only reads as "tail" in the UI if the log box is
// scrolled to the bottom once the backfill lands — otherwise the user is left
// looking at the OLDEST buffered lines. jsdom has no layout, so scroll
// geometry is stubbed on Element.prototype: the test asserts the component
// ASSIGNS scrollTop = scrollHeight after the backfill batch.
describe('RunDetail — log tail view (auto-scroll after backfill)', () => {
  let descST, descSH;
  beforeEach(() => {
    descST = Object.getOwnPropertyDescriptor(Element.prototype, 'scrollTop');
    descSH = Object.getOwnPropertyDescriptor(Element.prototype, 'scrollHeight');
    Object.defineProperty(Element.prototype, 'scrollTop', {
      configurable: true,
      get() { return this.__stubScrollTop || 0; },
      set(v) { this.__stubScrollTop = v; },
    });
    Object.defineProperty(Element.prototype, 'scrollHeight', {
      configurable: true,
      get() { return this.classList && this.classList.contains('log-box') ? 4000 : 0; },
    });
  });
  const restore = () => {
    if (descST) Object.defineProperty(Element.prototype, 'scrollTop', descST);
    if (descSH) Object.defineProperty(Element.prototype, 'scrollHeight', descSH);
  };

  it('scrolls the log box to the bottom after a truncated backfill on a finished run', async () => {
    try {
      const fetchMock = vi.fn((url) => {
        const u = String(url);
        if (u.includes('/events')) return eventsResponseWithLogs(200, true);
        if (u.includes('/steps')) return jsonResponse([]);
        if (u.includes('/approvals')) return jsonResponse([]);
        if (u.includes('/artifacts')) return jsonResponse([]);
        return jsonResponse({ id: 'run-tail', status: 'Succeeded', jobName: 'j', triggeredBy: 'x', createdAt: null, params: {} });
      });
      global.fetch = fetchMock;
      const { container } = render(RunDetail, { props: { params: { id: 'run-tail' } } });
      await vi.waitFor(() => {
        expect(container.querySelectorAll('.log-row').length).toBeGreaterThan(0);
      });
      const box = container.querySelector('.log-box');
      expect(box).toBeTruthy();
      await vi.waitFor(() => {
        expect(box.scrollTop).toBe(4000);
      });
    } finally {
      restore();
    }
  });
});

// Selecting a step used to reset the log view to the TOP and disable stick-
// scroll entirely (the old resetLogScrollOnFilter + the `selectedStep ===
// null` gate) — the exact opposite of the most common use: clicking the
// running step to follow its output. The filtered view must now jump to the
// END on selection and keep tailing.
describe('RunDetail — step-filtered log view tails (jump to end on select)', () => {
  let descST, descSH;
  beforeEach(() => {
    descST = Object.getOwnPropertyDescriptor(Element.prototype, 'scrollTop');
    descSH = Object.getOwnPropertyDescriptor(Element.prototype, 'scrollHeight');
    Object.defineProperty(Element.prototype, 'scrollTop', {
      configurable: true,
      get() { return this.__stubScrollTop || 0; },
      set(v) { this.__stubScrollTop = v; },
    });
    Object.defineProperty(Element.prototype, 'scrollHeight', {
      configurable: true,
      get() { return this.classList && this.classList.contains('log-box') ? 4000 : 0; },
    });
  });
  const restore = () => {
    if (descST) Object.defineProperty(Element.prototype, 'scrollTop', descST);
    if (descSH) Object.defineProperty(Element.prototype, 'scrollHeight', descSH);
  };


   it('backfills a truncated-away step from the server when selected', async () => {
    try {
      // SSE buffer: 200 lines, ALL for step 1 (the huge build), truncated —
      // step 0 (checkout) has zero buffered lines, mirroring the real case
      // where an early quiet step falls outside the tail window.
      const enc = new TextEncoder();
      let payload = `data: ${JSON.stringify({ type: 'truncated' })}

`;
      for (let i = 0; i < 200; i++) {
        payload += `data: ${JSON.stringify({ type: 'log', seq: 1000 + i, stepIndex: 1, stream: 'stdout', line: 'build ' + i })}

`;
      }
      let sent = false;
      const eventsResp = Promise.resolve({
        ok: true, status: 200,
        body: { getReader() { return { read: async () => sent ? { done: true, value: undefined } : (sent = true, { done: false, value: enc.encode(payload) }) } } },
      });
      // The on-demand endpoint returns checkout's lines (older seqs). 250 so
      // the virtual window (offset ~185 under the fixed-4000 stub) has rows.
      const checkoutLines = Array.from({ length: 250 }, (_, i) => (
        { seq: 100 + i, stepIndex: 0, stream: 'stdout', line: 'checkout ' + i, timestamp: '2026-01-01T00:00:00Z' }
      ));
      const fetchMock = vi.fn((url) => {
        const u = String(url);
        if (u.includes('/events')) return eventsResp;
        if (u.includes('/steps/0/logs')) return jsonResponse(checkoutLines);
        if (u.includes('/steps')) return jsonResponse([
          { index: 0, stageIndex: 0, name: 'checkout', status: 'Succeeded', kind: 'run', section: 'main' },
          { index: 1, stageIndex: 1, name: 'build', status: 'Succeeded', kind: 'run', section: 'main' },
        ]);
        if (u.includes('/approvals')) return jsonResponse([]);
        if (u.includes('/artifacts')) return jsonResponse([]);
        return jsonResponse({ id: 'run-stepfill', status: 'Succeeded', jobName: 'j', triggeredBy: 'x', createdAt: null, params: {} });
      });
      global.fetch = fetchMock;
      const { container } = render(RunDetail, { props: { params: { id: 'run-stepfill' } } });
      await vi.waitFor(() => {
        expect(container.querySelectorAll('.log-row').length).toBeGreaterThan(0);
        expect(container.querySelectorAll('.step-row').length).toBeGreaterThan(0);
      });

      await fireEvent.click(container.querySelectorAll('.step-row')[0]); // checkout
      await vi.waitFor(() => {
        // The on-demand endpoint was hit...
        expect(fetchMock.mock.calls.some(c => String(c[0]).includes('/steps/0/logs'))).toBe(true);
        // ...and the merged checkout lines render in the filtered view.
        const texts = [...container.querySelectorAll('.log-row')].map(r => r.textContent);
        expect(texts.some(t => t.includes('checkout'))).toBe(true);
      });
      const box = container.querySelector('.log-box');
      expect(box.scrollTop).toBe(4000);
    } finally {
      restore();
    }
  });

  it('jumps to the bottom when a step filter is applied', async () => {
    try {
      const fetchMock = vi.fn((url) => {
        const u = String(url);
        // 200 lines, not a handful: the scrollHeight stub is a fixed 4000,
        // and the component now jumps to the bottom on mount, so the virtual
        // scroller's window sits at row ~185 — fewer lines than that would
        // render zero rows under the stub (a stub artifact; real browsers
        // clamp scrollTop). Keep the fixture bigger than the window offset.
        if (u.includes('/events')) return eventsResponseWithLogs(200, false);
        if (u.includes('/steps')) return jsonResponse([
          { index: 0, stageIndex: 0, name: 'build', status: 'Succeeded', kind: 'run', section: 'main' },
          { index: 1, stageIndex: 1, name: 'test', status: 'Succeeded', kind: 'run', section: 'main' },
        ]);
        if (u.includes('/approvals')) return jsonResponse([]);
        if (u.includes('/artifacts')) return jsonResponse([]);
        return jsonResponse({ id: 'run-filter-tail', status: 'Succeeded', jobName: 'j', triggeredBy: 'x', createdAt: null, params: {} });
      });
      global.fetch = fetchMock;
      const { container } = render(RunDetail, { props: { params: { id: 'run-filter-tail' } } });
      await vi.waitFor(() => {
        expect(container.querySelectorAll('.log-row').length).toBeGreaterThan(0);
        expect(container.querySelectorAll('.step-row').length).toBeGreaterThan(0);
      });
      const box = container.querySelector('.log-box');
      // Simulate the user having scrolled away from the end.
      box.scrollTop = 0;

      await fireEvent.click(container.querySelectorAll('.step-row')[1]);
      await vi.waitFor(() => {
        expect(box.scrollTop).toBe(4000);
      });
    } finally {
      restore();
    }
  });
});
