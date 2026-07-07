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

  // Task 5: search is no longer client-side (scanning only the in-memory
  // window) — it's a server call over the FULL view via
  // `/logs/search?q=...`, which can find matches far outside the currently
  // loaded window. This test's row (123) is deliberately off-screen from the
  // initial (tail) window to prove the match still resolves: the server
  // returns the absolute row, the component jumps the scroller there, and
  // the scroll-driven range fetch (ensureRowsLoaded) loads it in so it can
  // be highlighted.
  it('searches the full log via the server (including off-screen lines) and highlights the match', async () => {
    const N = 500;
    const makeLine = (row) => ({ seq: row + 1, stepIndex: 0, stream: 'stdout', line: 'line ' + row });
    const statsRange = statsAndRange(N, makeLine);
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/events')) return eventsResponseWithLogs(N);
      if (u.includes('/logs/search')) {
        const uu = new URL(u, 'http://localhost');
        expect(uu.searchParams.get('q')).toBe('line 123');
        return jsonResponse({ total: 1, matches: [{ row: 123, seq: 124, stepIndex: 0 }] });
      }
      const sr = statsRange(url);
      if (sr) return sr;
      if (u.includes('/steps')) return jsonResponse([]);
      if (u.includes('/approvals')) return jsonResponse([]);
      return jsonResponse({ id: 'run-1', status: 'Succeeded', jobName: 'job-a', triggeredBy: 'x', createdAt: null, params: {} });
    });
    global.fetch = fetchMock;

    const { container } = render(RunDetail, { props: { params: { id: 'run-1' } } });
    await vi.waitFor(() => {
      expect(container.textContent).toContain(`${N} lines`);
    });

    // "line 123" is off-screen (row 123 is far below the initial tail window)
    // yet the server-backed search finds it — proving search covers the
    // whole log, not just the rendered rows — and highlights it once the
    // jump's range fetch brings it into the window.
    const input = container.querySelector('.log-search-input');
    await fireEvent.input(input, { target: { value: 'line 123' } });

    await vi.waitFor(() => {
      expect(fetchMock.mock.calls.some((c) => String(c[0]).includes('/logs/search'))).toBe(true);
    });

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

// Task 5: the in-app search block now delegates to the server's
// `/logs/search` endpoint (Task 2's contract: `{total, matches: [{row, seq,
// stepIndex}]}`, capped at 1000, `total` reflecting the uncapped count) over
// the CURRENT view (all-steps or a step/group filter, same `viewStepsQuery()`
// used by range/stats fetches), instead of scanning only the in-memory
// window. Typing debounces 300ms before firing the request; jumping to a
// match sets `scrollTop` and lets the existing scroll handler's
// `ensureRowsLoaded` bring the row into the window (no direct fetch call
// from the jump itself).
describe('RunDetail — server-side log search (Task 5)', () => {
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

  it('debounces input and calls /logs/search with the query and current view steps', async () => {
    try {
      const N = 300;
      const makeLine = (row) => ({ seq: row + 1, stepIndex: 1, stream: 'stdout', line: 'build ' + row });
      const statsRange = statsAndRange(N, makeLine);
      const searchCalls = [];
      const fetchMock = vi.fn((url) => {
        const u = String(url);
        if (u.includes('/events')) return eventsResponseWithLogs(N);
        if (u.includes('/logs/search')) {
          searchCalls.push(u);
          return jsonResponse({ total: 0, matches: [] });
        }
        if (u.includes('steps=1')) {
          const sr = statsRange(url);
          if (sr) return sr;
        }
        if (u.includes('/steps')) return jsonResponse([
          { index: 0, stageIndex: 0, name: 'checkout', status: 'Succeeded', kind: 'run', section: 'main' },
          { index: 1, stageIndex: 1, name: 'build', status: 'Succeeded', kind: 'run', section: 'main' },
        ]);
        if (u.includes('/approvals')) return jsonResponse([]);
        if (u.includes('/artifacts')) return jsonResponse([]);
        return jsonResponse({ id: 'run-search-debounce', status: 'Succeeded', jobName: 'j', triggeredBy: 'x', createdAt: null, params: {} });
      });
      global.fetch = fetchMock;

      const { container } = render(RunDetail, { props: { params: { id: 'run-search-debounce' } } });
      await vi.waitFor(() => {
        expect(container.querySelectorAll('.step-row').length).toBeGreaterThan(0);
      });

      // Switch to the "build" step view (steps=1) so the search request must
      // carry that view's steps filter, not the all-steps view.
      await fireEvent.click(container.querySelectorAll('.step-row')[1]);
      await vi.waitFor(() => {
        expect(fetchMock.mock.calls.some((c) => String(c[0]).includes('/logs/range') && String(c[0]).includes('steps=1'))).toBe(true);
      });

      const input = container.querySelector('.log-search-input');
      await fireEvent.input(input, { target: { value: 'err' } });

      // Not called immediately — debounced.
      expect(searchCalls.length).toBe(0);

      await vi.waitFor(() => {
        expect(searchCalls.length).toBeGreaterThan(0);
      }, { timeout: 1000 });

      const u = new URL(searchCalls[searchCalls.length - 1], 'http://localhost');
      expect(u.searchParams.get('q')).toBe('err');
      expect(u.searchParams.get('steps')).toBe('1');
    } finally {
      restore();
    }
  });

  it('jumps to a match on Enter by setting scrollTop, and the scroll handler fetches the range', async () => {
    try {
      // TOTAL is deliberately bigger than FETCH_CHUNK (5000) so the initial
      // tail window does NOT cover every row. Two matches: #0 sits inside the
      // tail backfill (already loaded — the initial auto-jump-to-first-match
      // is a no-op fetch-wise), #1 (row 10) is far outside it, so pressing
      // Enter to advance 0 -> 1 must trigger a genuinely new range fetch via
      // the scroll handler, not just render from what's already loaded.
      const TOTAL = 50000;
      const BACKFILL = 200;
      const makeLine = (row) => ({ seq: row + 1, stepIndex: 0, stream: 'stdout', line: 'row ' + row });
      const statsRange = statsAndRange(TOTAL, makeLine);
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
        if (u.includes('/logs/search')) {
          return jsonResponse({
            total: 2,
            matches: [
              { row: TOTAL - 5, seq: TOTAL - 4, stepIndex: 0 },
              { row: 10, seq: 11, stepIndex: 0 },
            ],
          });
        }
        const sr = statsRange(url);
        if (sr) return sr;
        if (u.includes('/steps')) return jsonResponse([]);
        if (u.includes('/approvals')) return jsonResponse([]);
        if (u.includes('/artifacts')) return jsonResponse([]);
        return jsonResponse({ id: 'run-search-jump', status: 'Running', jobName: 'j', triggeredBy: 'x', createdAt: null, params: {} });
      });
      global.fetch = fetchMock;

      const { container } = render(RunDetail, { props: { params: { id: 'run-search-jump' } } });
      await vi.waitFor(() => {
        expect(container.querySelectorAll('.log-row').length).toBeGreaterThan(0);
      });

      const input = container.querySelector('.log-search-input');
      await fireEvent.input(input, { target: { value: 'row' } });
      await vi.waitFor(() => {
        expect(container.textContent).toContain('1 / 2');
      }, { timeout: 1000 });

      const box = container.querySelector('.log-box');
      const rangeCallsBefore = fetchMock.mock.calls.filter((c) => String(c[0]).includes('/logs/range')).length;

      await fireEvent.keyDown(input, { key: 'Enter' });

      await vi.waitFor(() => {
        expect(container.textContent).toContain('2 / 2');
      });

      // scrollTop lands near row * LOG_ROW_H (centered in the viewport), not
      // at some other position — the jump itself must not call ensureRowsLoaded
      // directly; it just moves the scrollbar and lets the scroll handler do it.
      await vi.waitFor(() => {
        expect(box.scrollTop).toBe(Math.max(0, 10 * 20 - box.clientHeight / 2));
      });

      await fireEvent.scroll(box);
      await vi.waitFor(() => {
        const rangeCallsAfter = fetchMock.mock.calls.filter((c) => String(c[0]).includes('/logs/range')).length;
        expect(rangeCallsAfter).toBeGreaterThan(rangeCallsBefore);
      });

      await vi.waitFor(() => {
        const texts = [...container.querySelectorAll('.log-row')].map((r) => r.textContent);
        expect(texts.some((t) => t.includes('row 10'))).toBe(true);
      });
    } finally {
      restore();
    }
  });

  it('shows a capped-results note when total exceeds the returned matches', async () => {
    try {
      const N = 300;
      const makeLine = (row) => ({ seq: row + 1, stepIndex: 0, stream: 'stdout', line: 'x ' + row });
      const statsRange = statsAndRange(N, makeLine);
      const matches = Array.from({ length: 1000 }, (_, i) => ({ row: i, seq: i + 1, stepIndex: 0 }));
      const fetchMock = vi.fn((url) => {
        const u = String(url);
        if (u.includes('/events')) return eventsResponseWithLogs(N);
        if (u.includes('/logs/search')) {
          return jsonResponse({ total: 1500, matches });
        }
        const sr = statsRange(url);
        if (sr) return sr;
        if (u.includes('/steps')) return jsonResponse([]);
        if (u.includes('/approvals')) return jsonResponse([]);
        if (u.includes('/artifacts')) return jsonResponse([]);
        return jsonResponse({ id: 'run-search-cap', status: 'Succeeded', jobName: 'j', triggeredBy: 'x', createdAt: null, params: {} });
      });
      global.fetch = fetchMock;

      const { container } = render(RunDetail, { props: { params: { id: 'run-search-cap' } } });
      await vi.waitFor(() => {
        expect(container.querySelectorAll('.log-row').length).toBeGreaterThan(0);
      });

      const input = container.querySelector('.log-search-input');
      await fireEvent.input(input, { target: { value: 'x' } });

      await vi.waitFor(() => {
        expect(container.textContent).toContain('1 / 1,000+');
      }, { timeout: 1000 });
    } finally {
      restore();
    }
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

// Review finding (round 2): `windowLoading` was shared by BOTH switchLogView
// AND the ordinary same-view scroll fetch in ensureRowsLoaded, so the SSE
// reader's `else if (windowLoading)` drop branch (added to fix the Task 4
// atomicity findings) also dropped live batches during a plain scroll-driven
// range fetch — permanently losing their contribution to totalCount, since
// ensureRowsLoaded never touches totalCount and refreshStats() is only called
// from startSSE/switchLogView. Scrolling back while a job is actively logging
// is core usage, so live totals must keep growing during a same-view fetch.
describe('RunDetail — SSE totals keep growing during a same-view scroll fetch (review finding round 2)', () => {
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

  it('an SSE line arriving while a same-view ensureRowsLoaded fetch is in flight still grows the total', async () => {
    try {
      const TOTAL = 50000;
      const BACKFILL = 300;
      const makeLine = (row) => ({
        seq: row + 1, stepIndex: 0, stream: 'stdout', line: 'row ' + row,
      });
      const statsRange = statsAndRange(TOTAL, makeLine);

      // SSE: backfill the tail, then (after the initial read, gated) stream
      // one more live line — released only once the test has fired a
      // scroll-driven range fetch and confirmed it's still pending.
      const enc = new TextEncoder();
      let backfillPayload = '';
      for (let row = TOTAL - BACKFILL; row < TOTAL; row++) {
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

      // Gate the scroll-driven /logs/range fetch (offset=0, the top of the
      // log) so it's still in flight when the live SSE line arrives — the
      // deferred-promise pattern used elsewhere in this file to make an SSE
      // event genuinely overlap an in-flight fetch.
      let releaseRangeFetch = null;
      const rangeFetchGate = new Promise((res) => { releaseRangeFetch = res; });

      const fetchMock = vi.fn((url) => {
        const u = String(url);
        if (u.includes('/events')) return eventsResp;
        if (u.includes('/logs/range') && u.includes('offset=0')) {
          return rangeFetchGate.then(() => {
            const sr = statsRange(url);
            return sr;
          });
        }
        const sr = statsRange(url);
        if (sr) return sr;
        if (u.includes('/steps')) return jsonResponse([]);
        if (u.includes('/approvals')) return jsonResponse([]);
        if (u.includes('/artifacts')) return jsonResponse([]);
        return jsonResponse({ id: 'run-samefetch', status: 'Running', jobName: 'j', triggeredBy: 'x', createdAt: null, params: {} });
      });
      global.fetch = fetchMock;

      const { container } = render(RunDetail, { props: { params: { id: 'run-samefetch' } } });
      await vi.waitFor(() => {
        expect(container.textContent).toContain(`${TOTAL.toLocaleString()} lines`);
      });

      // Scroll away from the tail to a range outside the loaded window — a
      // real ensureRowsLoaded range fetch (NOT a view switch) fires and gets
      // gated on rangeFetchGate.
      const box = container.querySelector('.log-box');
      box.scrollTop = 0;
      await fireEvent.scroll(box);
      await vi.waitFor(() => {
        expect(fetchMock.mock.calls.some((c) => String(c[0]).includes('/logs/range') && String(c[0]).includes('offset=0'))).toBe(true);
      });

      // While that same-view range fetch is still pending, let the live SSE
      // line land.
      releaseLive();
      await vi.waitFor(() => expect(readCount).toBeGreaterThanOrEqual(2));

      // The total must grow even though a same-view ensureRowsLoaded fetch is
      // in flight — unlike a view switch, this is NOT a view-switch and must
      // not suppress the SSE contribution to totalCount.
      await vi.waitFor(() => {
        expect(container.textContent).toContain(`${(TOTAL + 1).toLocaleString()} lines`);
      });

      // Let the in-flight range fetch complete too, and confirm the window
      // is left in a consistent state (no crash, some rows still render).
      releaseRangeFetch();
      await vi.waitFor(() => {
        expect(container.querySelectorAll('.log-row').length).toBeGreaterThan(0);
      });
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


   it('selecting a step switches to a server-side filtered view', async () => {
    try {
      // SSE buffer: 200 lines, ALL for step 1 (the huge build), truncated —
      // step 0 (checkout) has zero buffered lines, mirroring the real case
      // where an early quiet step falls outside the tail window. Selecting
      // step 0 must now re-query the server for a STEPS-FILTERED view
      // (/logs/stats?steps=0 + /logs/range?...&steps=0) rather than merging
      // a one-off per-step backfill into the existing window.
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
      // checkout's lines (older seqs, own view). 250 so the virtual window
      // (offset ~185 under the fixed-4000 stub) has rows.
      const checkoutLines = Array.from({ length: 250 }, (_, i) => (
        { seq: 100 + i, stepIndex: 0, stream: 'stdout', line: 'checkout ' + i, timestamp: '2026-01-01T00:00:00Z' }
      ));
      const statsRange = statsAndRange(checkoutLines.length, (row) => checkoutLines[row]);
      const fetchMock = vi.fn((url) => {
        const u = String(url);
        if (u.includes('/events')) return eventsResp;
        if (u.includes('steps=0')) {
          const sr = statsRange(url);
          if (sr) return sr;
        }
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
        // The steps-filtered stats + range endpoints were hit...
        expect(fetchMock.mock.calls.some(c => String(c[0]).includes('/logs/stats') && String(c[0]).includes('steps=0'))).toBe(true);
        expect(fetchMock.mock.calls.some(c => String(c[0]).includes('/logs/range') && String(c[0]).includes('steps=0'))).toBe(true);
        // ...and the server-filtered checkout lines render in the view.
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
      // Step 1's own view (queried via /logs/stats?steps=1 + /logs/range?...
      // steps=1 on selection): 200 lines so the virtual window (offset ~185
      // under the fixed-4000 scrollHeight stub) has rows to render.
      const stepLines = Array.from({ length: 200 }, (_, i) => (
        { seq: i + 1, stepIndex: 1, stream: 'stdout', line: 'test line ' + i }
      ));
      const statsRange = statsAndRange(stepLines.length, (row) => stepLines[row]);
      const fetchMock = vi.fn((url) => {
        const u = String(url);
        // 200 lines, not a handful: the scrollHeight stub is a fixed 4000,
        // and the component now jumps to the bottom on mount, so the virtual
        // scroller's window sits at row ~185 — fewer lines than that would
        // render zero rows under the stub (a stub artifact; real browsers
        // clamp scrollTop). Keep the fixture bigger than the window offset.
        if (u.includes('/events')) return eventsResponseWithLogs(200, false);
        if (u.includes('steps=1')) {
          const sr = statsRange(url);
          if (sr) return sr;
        }
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

// Review findings on Task 4's switchLogView: it awaits refreshStats() and then
// a tail range fetch while `logWindow` still holds the PREVIOUS view's lines.
// Two windows of vulnerability during those awaits:
//   1. The SSE reader's live-append path is not switch-aware, so a batch that
//      arrives mid-switch gets concatenated onto the OLD window/totalCount
//      even though it was filtered for the NEW view — a transient (and, if
//      the switch itself is superseded, permanent) corruption of the window.
//   2. `windowLoading` is still false during the `await refreshStats()` leg,
//      so a scroll-driven `ensureRowsLoaded` can bump `windowFetchToken` out
//      from under the switch, causing it to silently abort (no tail fetch, no
//      jump to bottom) once its post-refreshStats token check fails.
describe('RunDetail — log view switch atomicity (review findings)', () => {
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

  it('an SSE batch arriving between switch start and its stats resolution does not mix into the old window', async () => {
    try {
      // All-steps SSE backfill: 200 lines for step 0 only (kept >= the
      // virtual window's offset under the fixed-4000 scrollHeight stub — see
      // the "jumps to the bottom" test above for why fewer lines render
      // zero rows under jsdom). Selecting step 1 (which has NO buffered
      // lines, like the existing "truncated-away step" scenario) drives
      // switchLogView([1]) into its server round-trips.
      const enc = new TextEncoder();
      let backfillPayload = '';
      for (let i = 0; i < 200; i++) {
        backfillPayload += `data: ${JSON.stringify({ type: 'log', seq: i + 1, stepIndex: 0, stream: 'stdout', line: 'step0 ' + i })}\n\n`;
      }
      // A live batch for step 1 (the NEW view being switched to), encoded so
      // it can be delivered on a later reader.read() call, i.e. AFTER the
      // user clicks step 1 but potentially before switchLogView's stats/range
      // awaits resolve.
      const liveBatchPayload = `data: ${JSON.stringify({ type: 'log', seq: 9001, stepIndex: 1, stream: 'stdout', line: 'LIVE-INTRUDER' })}\n\n`;

      let readCount = 0;
      let releaseLiveBatch = null;
      const liveBatchGate = new Promise((res) => { releaseLiveBatch = res; });
      const eventsResp = Promise.resolve({
        ok: true, status: 200,
        body: {
          getReader() {
            return {
              read: async () => {
                readCount++;
                if (readCount === 1) return { done: false, value: enc.encode(backfillPayload) };
                if (readCount === 2) {
                  await liveBatchGate;
                  return { done: false, value: enc.encode(liveBatchPayload) };
                }
                return { done: true, value: undefined };
              },
            };
          },
        },
      });

      // step 1's server-side view: 200 lines (>= the virtual window's offset
      // under the fixed-4000 scrollHeight stub, same reasoning as the
      // 200-line all-steps backfill above), its own totalCount (unrelated to
      // the live-intruder line, which must NOT be folded into it).
      const step1Lines = Array.from({ length: 200 }, (_, i) => (
        { seq: 5000 + i, stepIndex: 1, stream: 'stdout', line: 'step1 ' + i }
      ));
      const step1StatsRange = statsAndRange(step1Lines.length, (row) => step1Lines[row]);

      // Gate the steps=1 /logs/stats response so the test can deliver the SSE
      // live batch WHILE switchLogView's `await refreshStats()` is pending.
      let releaseStep1Stats = null;
      const step1StatsGate = new Promise((res) => { releaseStep1Stats = res; });

      const fetchMock = vi.fn((url) => {
        const u = String(url);
        if (u.includes('/events')) return eventsResp;
        if (u.includes('/logs/stats') && u.includes('steps=1')) {
          return step1StatsGate.then(() => jsonResponse({ count: step1Lines.length, minSeq: 1, maxSeq: step1Lines.length }));
        }
        if (u.includes('steps=1')) {
          const sr = step1StatsRange(u);
          if (sr) return sr;
        }
        if (u.includes('/steps')) return jsonResponse([
          { index: 0, stageIndex: 0, name: 'checkout', status: 'Succeeded', kind: 'run', section: 'main' },
          { index: 1, stageIndex: 1, name: 'build', status: 'Succeeded', kind: 'run', section: 'main' },
        ]);
        if (u.includes('/approvals')) return jsonResponse([]);
        if (u.includes('/artifacts')) return jsonResponse([]);
        return jsonResponse({ id: 'run-atomic', status: 'Running', jobName: 'j', triggeredBy: 'x', createdAt: null, params: {} });
      });
      global.fetch = fetchMock;

      const { container } = render(RunDetail, { props: { params: { id: 'run-atomic' } } });
      await vi.waitFor(() => {
        expect(container.querySelectorAll('.log-row').length).toBeGreaterThan(0);
        expect(container.querySelectorAll('.step-row').length).toBeGreaterThan(0);
      });
      expect(container.textContent).toContain('200 lines');

      // Click step 1 (build): kicks off switchLogView([1]), which awaits the
      // (gated) /logs/stats?steps=1 call.
      await fireEvent.click(container.querySelectorAll('.step-row')[1]);

      // While that stats fetch is still pending, let the SSE reader deliver
      // the live batch for step 1 — this used to get concat'd straight onto
      // the OLD (step-0) window and its totalCount, since the live-append
      // path didn't know a switch was in flight.
      releaseLiveBatch();
      await vi.waitFor(() => expect(readCount).toBeGreaterThanOrEqual(2));
      // Give the SSE .then()/tick microtasks a chance to run before the
      // switch's stats call resolves — this is the transient window where
      // the corruption is observable: the switch has started (logView.steps
      // is already [1]) but refreshStats()/the tail range fetch for the new
      // view haven't landed yet, so `logWindow` is still the OLD (step-0)
      // window. A switch-aware SSE append must not touch it at all.
      await new Promise((r) => setTimeout(r, 20));

      const midTexts = [...container.querySelectorAll('.log-row')].map((r) => r.textContent);
      expect(midTexts.some((t) => t.includes('LIVE-INTRUDER'))).toBe(false);
      // totalCount (rendered as "N lines") must not have been bumped by the
      // dropped live batch while still showing the stale step-0 total.
      expect(container.textContent).toContain('200 lines');

      // Now let the switch's stats fetch resolve, and let the range fetch
      // (unguarded) complete the switch.
      releaseStep1Stats();

      await vi.waitFor(() => {
        // The switch must complete: step1's own lines land in the view.
        const texts = [...container.querySelectorAll('.log-row')].map((r) => r.textContent);
        expect(texts.some((t) => t.includes('step1 '))).toBe(true);
      });

      // Final state: the live-intruder line must never appear anywhere in
      // the rendered window (it should have been dropped, not merged into
      // either the old or the new window), and totalCount must be exactly
      // step1Lines.length — not inflated by the dropped/misrouted live batch.
      const texts = [...container.querySelectorAll('.log-row')].map((r) => r.textContent);
      expect(texts.some((t) => t.includes('LIVE-INTRUDER'))).toBe(false);
      expect(texts.some((t) => t.includes('step0'))).toBe(false);
      expect(container.textContent).toContain(`${step1Lines.length} lines`);
    } finally {
      restore();
    }
  });

  it('a scroll-driven fetch during switchLogView\'s refreshStats does not abort the switch', async () => {
    try {
      // All-steps view: a huge total (50000) with only the tail 300 lines
      // backfilled via SSE — mirroring the Task 3 "scrolling above the
      // window fetches an earlier range" test. This matters here because
      // scrolling to the top must be OUTSIDE the currently-loaded window (so
      // it actually triggers a real ensureRowsLoaded range fetch that bumps
      // windowFetchToken) rather than a same-window no-op.
      const ALL_TOTAL = 50000;
      const BACKFILL = 300;
      const enc = new TextEncoder();
      let backfillPayload = '';
      for (let row = ALL_TOTAL - BACKFILL; row < ALL_TOTAL; row++) {
        backfillPayload += `data: ${JSON.stringify({ type: 'log', seq: row + 1, stepIndex: 0, stream: 'stdout', line: 'all ' + row })}\n\n`;
      }
      let sent = false;
      const eventsResp = Promise.resolve({
        ok: true, status: 200,
        body: { getReader() { return { read: async () => sent ? { done: true, value: undefined } : (sent = true, { done: false, value: enc.encode(backfillPayload) }) } } },
      });

      const step1Lines = Array.from({ length: 200 }, (_, i) => (
        { seq: 9000 + i, stepIndex: 1, stream: 'stdout', line: 'step1 ' + i }
      ));
      const step1StatsRange = statsAndRange(step1Lines.length, (row) => step1Lines[row]);
      const allStatsRange = statsAndRange(ALL_TOTAL, (row) => ({ seq: row + 1, stepIndex: 0, stream: 'stdout', line: 'all ' + row }));

      // Gate the steps=1 stats fetch so a scroll (against the OLD, all-steps
      // window that's still installed) can be fired while it's pending —
      // that scroll's ensureRowsLoaded must not be able to steal the token
      // and silently no-op the switch.
      let releaseStep1Stats = null;
      const step1StatsGate = new Promise((res) => { releaseStep1Stats = res; });

      const fetchMock = vi.fn((url) => {
        const u = String(url);
        if (u.includes('/events')) return eventsResp;
        if (u.includes('/logs/stats') && u.includes('steps=1')) {
          return step1StatsGate.then(() => jsonResponse({ count: step1Lines.length, minSeq: 1, maxSeq: step1Lines.length }));
        }
        if (u.includes('steps=1')) {
          const sr = step1StatsRange(u);
          if (sr) return sr;
        }
        // All-steps stats/range (used by startSSE's initial refreshStats and
        // by the scroll-driven ensureRowsLoaded against the OLD view before
        // the switch installs the new one).
        if (!u.includes('steps=')) {
          const sr = allStatsRange(u);
          if (sr) return sr;
        }
        if (u.includes('/steps')) return jsonResponse([
          { index: 0, stageIndex: 0, name: 'checkout', status: 'Succeeded', kind: 'run', section: 'main' },
          { index: 1, stageIndex: 1, name: 'build', status: 'Succeeded', kind: 'run', section: 'main' },
        ]);
        if (u.includes('/approvals')) return jsonResponse([]);
        if (u.includes('/artifacts')) return jsonResponse([]);
        return jsonResponse({ id: 'run-scrollrace', status: 'Running', jobName: 'j', triggeredBy: 'x', createdAt: null, params: {} });
      });
      global.fetch = fetchMock;

      const { container } = render(RunDetail, { props: { params: { id: 'run-scrollrace' } } });
      await vi.waitFor(() => {
        expect(container.querySelectorAll('.log-row').length).toBeGreaterThan(0);
        expect(container.querySelectorAll('.step-row').length).toBeGreaterThan(0);
      });

      const box = container.querySelector('.log-box');

      // Click step 1 (build): switchLogView([1]) starts, awaiting the gated
      // steps=1 stats fetch.
      await fireEvent.click(container.querySelectorAll('.step-row')[1]);

      // While that's pending, scroll the box to the MIDDLE of the log (row
      // ~25000 under the fixed 20px row height) — well outside both the
      // SSE-backfilled tail window [ALL_TOTAL-300, ALL_TOTAL) AND whatever
      // the mount-time ensureRowsLoaded may have already fetched around row
      // 0. Before the fix, this fired a genuinely new, real
      // ensureRowsLoaded range fetch that could steal windowFetchToken out
      // from under the in-flight switch (windowLoading was still false
      // during switchLogView's `await refreshStats()`), silently aborting
      // the switch. With the fix, `windowLoading` is already true for the
      // whole switch, so ensureRowsLoaded's own guard suppresses this
      // scroll-driven fetch entirely — no race, no extra request.
      const rangeCallsBeforeScroll = fetchMock.mock.calls.filter((c) => String(c[0]).includes('/logs/range')).length;
      box.scrollTop = 25000 * 20;
      await fireEvent.scroll(box);
      await new Promise((r) => setTimeout(r, 20));
      const rangeCallsAfterScroll = fetchMock.mock.calls.filter((c) => String(c[0]).includes('/logs/range')).length;
      expect(rangeCallsAfterScroll).toBe(rangeCallsBeforeScroll);

      // Now let the switch's stats fetch resolve.
      releaseStep1Stats();

      // The switch must still complete: step1's own lines land in the view,
      // and the box jumps back to the bottom (tail) as switchLogView always
      // does on success — NOT silently no-op due to a stolen token.
      await vi.waitFor(() => {
        const texts = [...container.querySelectorAll('.log-row')].map((r) => r.textContent);
        expect(texts.some((t) => t.includes('step1 '))).toBe(true);
      });
      await vi.waitFor(() => {
        expect(box.scrollTop).toBe(4000);
      });
    } finally {
      restore();
    }
  });
});

// Review finding (round 3): startSSE() bumps windowFetchToken to invalidate
// any in-flight switchLogView from the OLD run, but never resets
// windowLoading/viewSwitching. Those flags are only reset in switchLogView's
// `finally` block, gated on `token === windowFetchToken` — and since startSSE
// already bumped the token past that switch's own token, the gate never
// passes for the superseded switch. With nothing else to reset them, they get
// stuck `true` forever once the user navigates to a different run while a
// switchLogView is in flight: `viewSwitching` stuck true makes the SSE reader
// drop every future batch for the NEW run (live updates silently dead), and
// `windowLoading` stuck true permanently blocks ensureRowsLoaded.
describe('RunDetail — window flags reset when startSSE supersedes an in-flight view switch (review finding round 3)', () => {
  it('navigating to a new run while a switchLogView is in flight still lets the new run\'s SSE backfill render', async () => {
    const steps1 = [
      { index: 0, stageIndex: 0, name: 'checkout', status: 'Succeeded', kind: 'run', section: 'main' },
      { index: 1, stageIndex: 1, name: 'build', status: 'Succeeded', kind: 'run', section: 'main' },
    ];

    // run-1's all-steps SSE backfill (small; only needs to render enough to
    // click a step and kick off switchLogView).
    const enc = new TextEncoder();
    const run1Backfill =
      `data: ${JSON.stringify({ type: 'log', seq: 1, stepIndex: 0, stream: 'stdout', line: 'run1 line0' })}\n\n`;
    let run1Sent = false;
    const run1EventsResp = Promise.resolve({
      ok: true, status: 200,
      body: {
        getReader() {
          return {
            read: async () => run1Sent
              ? { done: true, value: undefined }
              : (run1Sent = true, { done: false, value: enc.encode(run1Backfill) }),
          };
        },
      },
    });

    // run-2's SSE stream: a first batch (the initial backfill — accepted
    // unconditionally by the `!backfilled` branch regardless of
    // viewSwitching/windowLoading) followed by a SECOND, genuinely live
    // batch. That second batch is the one that actually exercises the bug:
    // the SSE reader's live-append path drops batches outright while
    // `viewSwitching` is true, so if startSSE left it stuck true this line
    // never renders no matter how long the test waits.
    const run2Backfill =
      `data: ${JSON.stringify({ type: 'log', seq: 1, stepIndex: 1, stream: 'stdout', line: 'run2 backfill' })}\n\n`;
    const run2Live =
      `data: ${JSON.stringify({ type: 'log', seq: 2, stepIndex: 1, stream: 'stdout', line: 'RUN2-LIVE' })}\n\n`;
    let run2ReadCount = 0;
    const run2EventsResp = Promise.resolve({
      ok: true, status: 200,
      body: {
        getReader() {
          return {
            read: async () => {
              run2ReadCount++;
              if (run2ReadCount === 1) return { done: false, value: enc.encode(run2Backfill) };
              if (run2ReadCount === 2) return { done: false, value: enc.encode(run2Live) };
              return { done: true, value: undefined };
            },
          };
        },
      },
    });

    // Gate run-1's steps=1 /logs/stats fetch so switchLogView([1]) is still
    // in flight (windowLoading/viewSwitching already true, windowFetchToken
    // already bumped) when we navigate away to run-2.
    let releaseRun1Step1Stats = null;
    const run1Step1StatsGate = new Promise((res) => { releaseRun1Step1Stats = res; });

    const fetchMock = vi.fn((url) => {
      const u = String(url);
      const runID = u.match(/\/runs\/([^/]+)/)?.[1];
      if (u.includes('/events')) return runID === 'run-2' ? run2EventsResp : run1EventsResp;
      if (runID === 'run-1' && u.includes('/logs/stats') && u.includes('steps=1')) {
        // Never actually resolves within this test — the switch is left
        // permanently in flight, exactly like a switch superseded by
        // navigation before its own round-trip completes.
        return run1Step1StatsGate.then(() => jsonResponse({ count: 0, minSeq: 0, maxSeq: 0 }));
      }
      if (u.includes('/logs/stats') || u.includes('/logs/range')) {
        return jsonResponse({ count: 0, minSeq: 0, maxSeq: 0 });
      }
      if (u.includes('/steps')) return jsonResponse(runID === 'run-1' ? steps1 : []);
      if (u.includes('/approvals')) return jsonResponse([]);
      if (u.includes('/artifacts')) return jsonResponse([]);
      return jsonResponse({ id: runID, status: 'Running', jobName: 'j', triggeredBy: 'x', createdAt: null, params: {} });
    });
    global.fetch = fetchMock;

    const { container, rerender } = render(RunDetail, { props: { params: { id: 'run-1' } } });

    await vi.waitFor(() => {
      expect(container.querySelectorAll('.step-row').length).toBeGreaterThan(0);
    });

    // Click step 1 (build): kicks off switchLogView([1]), gated forever on
    // the steps=1 stats fetch above. windowLoading/viewSwitching are now
    // true and windowFetchToken has been bumped past its previous value.
    await fireEvent.click(container.querySelectorAll('.step-row')[1]);

    // Give switchLogView's synchronous pre-await block a tick to run.
    await new Promise((r) => setTimeout(r, 0));

    // Navigate to a different run while that switch is still stuck in
    // flight — this is the reactive `$: runID, init()` path real navigation
    // takes, reusing the same component instance. init() calls startSSE(),
    // which bumps windowFetchToken again (superseding the stuck switch) but,
    // before the fix, never resets windowLoading/viewSwitching.
    await rerender({ params: { id: 'run-2' } });

    // run-2's SSE backfill must render: if the flags were left stuck true by
    // startSSE, the SSE reader's `viewSwitching` branch drops every batch
    // for run-2 forever, and windowLoading stuck true means ensureRowsLoaded
    // is permanently blocked too. With the fix, startSSE resets both flags
    // as the new token owner, so the backfill installs normally.
    await vi.waitFor(() => {
      const texts = [...container.querySelectorAll('.log-row')].map((r) => r.textContent);
      expect(texts.some((t) => t.includes('RUN2-LIVE'))).toBe(true);
    });

    // Cleanup: release the gate so the never-resolving promise doesn't leak
    // across tests (its `finally` will see a stale token and correctly
    // no-op — that's fine, the assertion above already covers the fix).
    releaseRun1Step1Stats();
  });
});

// Final review finding 1: ensureRowsLoaded's `if (windowLoading) return` guard
// dropped a scroll request while a range fetch was in flight, but nothing
// re-checked the viewport once that fetch settled. If the user scrolled to a
// new, uncovered position while a fetch was pending (its own request
// early-returned), the settling fetch installed a window centered on the OLD
// scroll target and cleared windowLoading WITHOUT re-invoking itself — the new
// viewport was left uncovered ("Loading…") with no fetch in flight and nothing
// that would ever fire one (no SSE appends on a finished run to rescue it).
// The fix re-checks the CURRENT logStart/logEnd in ensureRowsLoaded's finally
// and refetches if still uncovered, terminating via the same in-window
// early-return once the viewport is covered (so a view smaller than
// FETCH_CHUNK does not loop forever).
describe('RunDetail — ensureRowsLoaded re-checks the viewport after an in-flight fetch settles (final review finding 1)', () => {
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

  it('a scroll to a new uncovered position that landed while a fetch was in flight is served once that fetch settles', async () => {
    try {
      const TOTAL = 50000;
      const BACKFILL = 200;
      const makeLine = (row) => ({ seq: row + 1, stepIndex: 0, stream: 'stdout', line: 'row ' + row });
      const statsRange = statsAndRange(TOTAL, makeLine);
      // SSE backfill: tail 200 lines (rows 49800..49999).
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

      // The mount installs a window around the tail-anchored center; scrolling
      // to row 40000 (deep in the log, uncovered) fires a range fetch we GATE
      // so it stays in flight. Any /logs/range with a high offset (>= 30000)
      // is that gated first-target fetch; the later (low-offset) fetch for the
      // second scroll target is left ungated. The mount fetch has a low offset
      // and is never gated, so the initial window renders.
      let releaseFarFetch = null;
      const farFetchGate = new Promise((res) => { releaseFarFetch = res; });
      const isFarRange = (u) => {
        if (!u.includes('/logs/range')) return false;
        const off = Number(new URL(u, 'http://localhost').searchParams.get('offset') || '0');
        return off >= 30000;
      };

      const fetchMock = vi.fn((url) => {
        const u = String(url);
        if (u.includes('/events')) return eventsResp;
        if (isFarRange(u)) {
          return farFetchGate.then(() => statsRange(url));
        }
        const sr = statsRange(url);
        if (sr) return sr;
        if (u.includes('/steps')) return jsonResponse([]);
        if (u.includes('/approvals')) return jsonResponse([]);
        if (u.includes('/artifacts')) return jsonResponse([]);
        return jsonResponse({ id: 'run-recheck', status: 'Succeeded', jobName: 'j', triggeredBy: 'x', createdAt: null, params: {} });
      });
      global.fetch = fetchMock;

      const { container } = render(RunDetail, { props: { params: { id: 'run-recheck' } } });
      await vi.waitFor(() => {
        expect(container.querySelectorAll('.log-row').length).toBeGreaterThan(0);
      });

      const box = container.querySelector('.log-box');

      // Scroll DEEP into the log (row ~40000): fires ensureRowsLoaded for a
      // high offset, which is gated and stays in flight (windowLoading = true).
      box.scrollTop = 40000 * 20;
      await fireEvent.scroll(box);
      await vi.waitFor(() => {
        expect(fetchMock.mock.calls.some((c) => isFarRange(String(c[0])))).toBe(true);
      });

      // While the far fetch is still in flight, scroll to row ~10000 (a LOW
      // offset, uncovered by any installed window). This request is
      // early-returned by the `windowLoading` guard — nothing fetches it yet.
      const lowCallsBefore = fetchMock.mock.calls.filter((c) => {
        const cu = String(c[0]);
        return cu.includes('/logs/range') && !isFarRange(cu);
      }).length;
      box.scrollTop = 10000 * 20;
      await fireEvent.scroll(box);
      await new Promise((r) => setTimeout(r, 20));
      // Confirm the row-10000 range was NOT fetched while the far fetch was gated.
      const lowCallsDuring = fetchMock.mock.calls.filter((c) => {
        const cu = String(c[0]);
        return cu.includes('/logs/range') && !isFarRange(cu);
      }).length;
      expect(lowCallsDuring).toBe(lowCallsBefore);

      // Release the far fetch. Its finally must re-check the CURRENT viewport
      // (now row ~10000, uncovered by the far window it just installed) and
      // fire a fresh range fetch for it.
      releaseFarFetch();

      await vi.waitFor(() => {
        const texts = [...container.querySelectorAll('.log-row')].map((r) => r.textContent);
        expect(texts.some((t) => t.includes('row 10000'))).toBe(true);
      });
    } finally {
      restore();
    }
  });

  it('a view smaller than FETCH_CHUNK does not loop forever re-fetching after settle', async () => {
    try {
      // Total is far smaller than FETCH_CHUNK (5000): the server legitimately
      // returns fewer rows than requested, and every row fits in a single
      // window. The re-check must compare against what the window NOW covers
      // and early-return, NOT keep firing because the fetch returned < limit.
      // (250, not a handful: under the fixed-4000 scrollHeight stub the
      // virtual window sits around row ~185, so fewer rows would render zero
      // — a stub artifact, see the step-filtered tests above.)
      const TOTAL = 250;
      const makeLine = (row) => ({ seq: row + 1, stepIndex: 0, stream: 'stdout', line: 'small ' + row });
      const statsRange = statsAndRange(TOTAL, makeLine);
      const fetchMock = vi.fn((url) => {
        const u = String(url);
        if (u.includes('/events')) return eventsResponseWithLogs(TOTAL);
        const sr = statsRange(url);
        if (sr) return sr;
        if (u.includes('/steps')) return jsonResponse([]);
        if (u.includes('/approvals')) return jsonResponse([]);
        if (u.includes('/artifacts')) return jsonResponse([]);
        return jsonResponse({ id: 'run-small', status: 'Succeeded', jobName: 'j', triggeredBy: 'x', createdAt: null, params: {} });
      });
      global.fetch = fetchMock;

      const { container } = render(RunDetail, { props: { params: { id: 'run-small' } } });
      await vi.waitFor(() => {
        expect(container.querySelectorAll('.log-row').length).toBeGreaterThan(0);
      });

      const box = container.querySelector('.log-box');
      box.scrollTop = 0;
      await fireEvent.scroll(box);
      // Let any re-check settle and confirm it stabilizes (no runaway fetch
      // loop) — the range-fetch count must plateau.
      await new Promise((r) => setTimeout(r, 60));
      const c1 = fetchMock.mock.calls.filter((c) => String(c[0]).includes('/logs/range')).length;
      await new Promise((r) => setTimeout(r, 60));
      const c2 = fetchMock.mock.calls.filter((c) => String(c[0]).includes('/logs/range')).length;
      expect(c2).toBe(c1);
    } finally {
      restore();
    }
  });
});

// Final review finding 2: selectedStep/selectedParallelGroup (and thus
// logView) were never reset when runID changed. Navigating from run A with a
// step selected to run B reused the component instance with logView.steps
// still set, so run B rendered a step-filtered chrome (labels hidden, "All
// Steps" reset button shown) while its SSE backfill was the ALL-steps tail —
// a filtered view showing every step's lines, with an internally inconsistent
// totalCount and no self-correction until the user toggled the selection.
// Step indices are per-run, so a carried-over selection is meaningless; init()
// now resets it when runID changes (suppressing the view-switch reactive the
// same way the initial mount does, since startSSE installs the all-view
// window anyway).
describe('RunDetail — cross-run navigation resets the step selection (final review finding 2)', () => {
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

  it('navigating to a new run clears a step selection and renders the all-steps view', async () => {
    try {
      const enc = new TextEncoder();
      // run-1: 2 steps, all-steps SSE backfill of 200 step-0 lines.
      let run1Backfill = '';
      for (let i = 0; i < 200; i++) {
        run1Backfill += `data: ${JSON.stringify({ type: 'log', seq: i + 1, stepIndex: 0, stream: 'stdout', line: 'run1 ' + i })}\n\n`;
      }
      let run1Sent = false;
      const run1EventsResp = Promise.resolve({
        ok: true, status: 200,
        body: { getReader() { return { read: async () => run1Sent ? { done: true, value: undefined } : (run1Sent = true, { done: false, value: enc.encode(run1Backfill) }) } } },
      });
      // run-2: all-steps SSE backfill of 200 step-0 lines (its own run).
      let run2Backfill = '';
      for (let i = 0; i < 200; i++) {
        run2Backfill += `data: ${JSON.stringify({ type: 'log', seq: i + 1, stepIndex: 0, stream: 'stdout', line: 'run2 ' + i })}\n\n`;
      }
      let run2Sent = false;
      const run2EventsResp = Promise.resolve({
        ok: true, status: 200,
        body: { getReader() { return { read: async () => run2Sent ? { done: true, value: undefined } : (run2Sent = true, { done: false, value: enc.encode(run2Backfill) }) } } },
      });

      // run-1's step-0 filtered view (queried on selection).
      const step0Lines = Array.from({ length: 200 }, (_, i) => (
        { seq: 500 + i, stepIndex: 0, stream: 'stdout', line: 'run1-step0 ' + i }
      ));
      const step0StatsRange = statsAndRange(step0Lines.length, (row) => step0Lines[row]);

      const run2StatsUrls = [];
      const fetchMock = vi.fn((url) => {
        const u = String(url);
        const runID = u.match(/\/runs\/([^/]+)/)?.[1];
        if (u.includes('/events')) return runID === 'run-2' ? run2EventsResp : run1EventsResp;
        if (runID === 'run-2' && u.includes('/logs/stats')) run2StatsUrls.push(u);
        if (runID === 'run-1' && u.includes('steps=0')) {
          const sr = step0StatsRange(u);
          if (sr) return sr;
        }
        if (u.includes('/logs/stats')) return jsonResponse({ count: 200, minSeq: 1, maxSeq: 200 });
        if (u.includes('/logs/range')) return jsonResponse([]);
        if (u.includes('/steps')) return jsonResponse(runID === 'run-1' ? [
          { index: 0, stageIndex: 0, name: 'checkout', status: 'Succeeded', kind: 'run', section: 'main' },
          { index: 1, stageIndex: 1, name: 'build', status: 'Succeeded', kind: 'run', section: 'main' },
        ] : []);
        if (u.includes('/approvals')) return jsonResponse([]);
        if (u.includes('/artifacts')) return jsonResponse([]);
        return jsonResponse({ id: runID, status: 'Succeeded', jobName: 'j', triggeredBy: 'x', createdAt: null, params: {} });
      });
      global.fetch = fetchMock;

      const { container, rerender } = render(RunDetail, { props: { params: { id: 'run-1' } } });
      await vi.waitFor(() => {
        expect(container.querySelectorAll('.step-row').length).toBeGreaterThan(0);
        expect(container.querySelectorAll('.log-row').length).toBeGreaterThan(0);
      });

      // Select step 0 (checkout) on run-1 → step-filtered view: the "All Steps"
      // reset button appears and per-line step labels disappear.
      await fireEvent.click(container.querySelectorAll('.step-row')[0]);
      await vi.waitFor(() => {
        const texts = [...container.querySelectorAll('.log-row')].map((r) => r.textContent);
        expect(texts.some((t) => t.includes('run1-step0'))).toBe(true);
      });
      const allStepsBtn = [...container.querySelectorAll('.log-header .btn')].find((b) => b.textContent.includes('All Steps'));
      expect(allStepsBtn).toBeTruthy();
      expect(container.querySelector('.log-step-label')).toBeFalsy();

      const run2StatsBefore = run2StatsUrls.length;

      // Navigate to run-2 while step 0 is selected — the reactive
      // `$: runID, init()` path, reusing this component instance.
      await rerender({ params: { id: 'run-2' } });

      // run-2 renders its all-steps view: per-line step labels are back and
      // the "All Steps" reset button is gone (selection cleared).
      await vi.waitFor(() => {
        const texts = [...container.querySelectorAll('.log-row')].map((r) => r.textContent);
        expect(texts.some((t) => t.includes('run2 '))).toBe(true);
      });
      expect(container.querySelector('.log-step-label')).toBeTruthy();
      const allStepsBtnAfter = [...container.querySelectorAll('.log-header .btn')].find((b) => b.textContent.includes('All Steps'));
      expect(allStepsBtnAfter).toBeFalsy();

      // run-2's stats fetch(es) must NOT carry a stale steps= filter from run-1.
      const run2StatsAfter = run2StatsUrls.slice(run2StatsBefore);
      expect(run2StatsAfter.length).toBeGreaterThan(0);
      expect(run2StatsAfter.every((s) => !s.includes('steps='))).toBe(true);
    } finally {
      restore();
    }
  });
});

// Final review finding 3: switchLogView set logView (the new view) immediately
// but kept the OLD window's lines until the new range fetch landed. If that
// range fetch THREW, the catch only reset flags — logWindow.lines still held
// the OLD view's rows while totalCount had already been replaced by the NEW
// view's stats. The result: the wrong step's logs rendered under the new
// selection as if they were its rows, and because the window "covered" rows
// 0..newCount, ensureRowsLoaded never refetched — a coherent-looking WRONG
// view with no recovery except re-toggling. The fix installs an empty window
// in the catch so the view degrades to empty (matching the pre-branch failure
// mode) and a later scroll can recover it.
describe('RunDetail — switchLogView range-fetch failure degrades to empty, not stale content (final review finding 3)', () => {
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

  it('a failed switch does not show the old view\'s lines as the new view, and a later scroll recovers', async () => {
    try {
      // All-steps SSE backfill: 200 step-0 lines (rows 0..199). Selecting
      // step 1 switches to a view whose stats succeed (count 3000) but whose
      // tail range fetch FAILS the first time — the exact catch path.
      const enc = new TextEncoder();
      let backfill = '';
      for (let i = 0; i < 200; i++) {
        backfill += `data: ${JSON.stringify({ type: 'log', seq: i + 1, stepIndex: 0, stream: 'stdout', line: 'STEP0-LINE ' + i })}\n\n`;
      }
      let sent = false;
      const eventsResp = Promise.resolve({
        ok: true, status: 200,
        body: { getReader() { return { read: async () => sent ? { done: true, value: undefined } : (sent = true, { done: false, value: enc.encode(backfill) }) } } },
      });

      // step 1's server view: 3000 lines. The FIRST steps=1 range fetch
      // (switchLogView's tail fetch) fails; later ones (a recovery scroll)
      // succeed, so the fix's "later scroll recovers" claim is exercised.
      const STEP1_TOTAL = 3000;
      const step1Line = (row) => ({ seq: 10000 + row, stepIndex: 1, stream: 'stdout', line: 'STEP1-LINE ' + row });
      let step1RangeFailed = false;
      const fetchMock = vi.fn((url) => {
        const u = String(url);
        if (u.includes('/events')) return eventsResp;
        if (u.includes('/logs/stats') && u.includes('steps=1')) {
          return jsonResponse({ count: STEP1_TOTAL, minSeq: 1, maxSeq: STEP1_TOTAL });
        }
        if (u.includes('/logs/range') && u.includes('steps=1')) {
          if (!step1RangeFailed) {
            step1RangeFailed = true;
            // First steps=1 range fetch (the switch's tail fetch) fails.
            return Promise.resolve({ ok: false, status: 500, text: async () => 'boom' });
          }
          // Recovery fetch succeeds.
          const uu = new URL(u, 'http://localhost');
          const offset = Number(uu.searchParams.get('offset') || '0');
          const limit = Number(uu.searchParams.get('limit') || '1000');
          const end = Math.min(STEP1_TOTAL, offset + limit);
          const lines = [];
          for (let row = offset; row < end; row++) lines.push(step1Line(row));
          return jsonResponse(lines);
        }
        if (u.includes('/logs/stats')) return jsonResponse({ count: 200, minSeq: 1, maxSeq: 200 });
        if (u.includes('/logs/range')) return jsonResponse([]);
        if (u.includes('/steps')) return jsonResponse([
          { index: 0, stageIndex: 0, name: 'checkout', status: 'Succeeded', kind: 'run', section: 'main' },
          { index: 1, stageIndex: 1, name: 'build', status: 'Succeeded', kind: 'run', section: 'main' },
        ]);
        if (u.includes('/approvals')) return jsonResponse([]);
        if (u.includes('/artifacts')) return jsonResponse([]);
        return jsonResponse({ id: 'run-switchfail', status: 'Succeeded', jobName: 'j', triggeredBy: 'x', createdAt: null, params: {} });
      });
      global.fetch = fetchMock;

      const { container } = render(RunDetail, { props: { params: { id: 'run-switchfail' } } });
      await vi.waitFor(() => {
        expect(container.querySelectorAll('.step-row').length).toBeGreaterThan(0);
        expect(container.querySelectorAll('.log-row').length).toBeGreaterThan(0);
      });
      // Sanity: the all-steps view shows step-0 lines before the switch.
      expect([...container.querySelectorAll('.log-row')].some((r) => r.textContent.includes('STEP0-LINE'))).toBe(true);

      // Select step 1 (build): switchLogView([1]) — stats succeed, range fails.
      await fireEvent.click(container.querySelectorAll('.step-row')[1]);

      // Wait for the failed range fetch to have been attempted.
      await vi.waitFor(() => {
        expect(step1RangeFailed).toBe(true);
      });

      // After the failure the window must NOT still be showing the old
      // all-steps (step-0) lines addressed as step-1 rows.
      await vi.waitFor(() => {
        const texts = [...container.querySelectorAll('.log-row')].map((r) => r.textContent);
        expect(texts.some((t) => t.includes('STEP0-LINE'))).toBe(false);
      });

      // The view is recoverable: scroll to the top of the (now empty) step-1
      // view — the scroll-driven ensureRowsLoaded fires a fresh range fetch,
      // which succeeds and brings step-1 lines in.
      const box = container.querySelector('.log-box');
      box.scrollTop = 0;
      await fireEvent.scroll(box);
      await vi.waitFor(() => {
        const texts = [...container.querySelectorAll('.log-row')].map((r) => r.textContent);
        expect(texts.some((t) => t.includes('STEP1-LINE'))).toBe(true);
      });
    } finally {
      restore();
    }
  });
});

// Round-2 review finding 1: the settle re-check added to ensureRowsLoaded's
// finally could loop forever with zero backoff. When a range fetch keeps
// FAILING against an offline/failing server, the catch leaves the window
// unchanged, the finally (token-matched) re-checks the still-uncovered
// viewport and immediately refires the same fetch → throws → re-checks… a
// tight, unbounded request storm. The fix skips the re-check on the catch
// path (a failure no longer auto-retries; a user scroll re-triggers via the
// reactive), while the SUCCESS-path re-check (the wedge-recovery case) stays.
describe('RunDetail — a persistently failing range fetch does not storm-refire after settle (round-2 finding 1)', () => {
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

  it('a range fetch that always rejects is not refired in a tight loop after settle, and a later user scroll still retriggers exactly one retry', async () => {
    try {
      const TOTAL = 50000;
      const BACKFILL = 200;
      const makeLine = (row) => ({ seq: row + 1, stepIndex: 0, stream: 'stdout', line: 'row ' + row });
      // SSE backfill: tail 200 lines (rows 49800..49999) — the initial window.
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

      // Every /logs/range REJECTS (offline/failing server). /logs/stats still
      // succeeds so the scrollbar spans the full log and the viewport can be
      // uncovered by a scroll into the middle.
      const rangeCalls = () => fetchMock.mock.calls.filter((c) => String(c[0]).includes('/logs/range')).length;
      const fetchMock = vi.fn((url) => {
        const u = String(url);
        if (u.includes('/events')) return eventsResp;
        if (u.includes('/logs/range')) return Promise.reject(new Error('offline'));
        if (u.includes('/logs/stats')) return jsonResponse({ count: TOTAL, minSeq: 1, maxSeq: TOTAL });
        if (u.includes('/steps')) return jsonResponse([]);
        if (u.includes('/approvals')) return jsonResponse([]);
        if (u.includes('/artifacts')) return jsonResponse([]);
        return jsonResponse({ id: 'run-storm', status: 'Succeeded', jobName: 'j', triggeredBy: 'x', createdAt: null, params: {} });
      });
      global.fetch = fetchMock;

      const { container } = render(RunDetail, { props: { params: { id: 'run-storm' } } });
      // The tail backfill installs (scrollbar spans the full 50000) even though
      // the top-of-log viewport is uncovered and its offset-0 range fetch
      // rejects — wait on the "N lines" counter, not on rendered rows.
      await vi.waitFor(() => {
        expect(container.textContent).toContain(`${TOTAL.toLocaleString()} lines`);
      });

      const box = container.querySelector('.log-box');

      // Scroll into the middle (row ~25000): uncovered by the tail window, so
      // ensureRowsLoaded fires a range fetch — which rejects.
      box.scrollTop = 25000 * 20;
      await fireEvent.scroll(box);
      await vi.waitFor(() => {
        expect(rangeCalls()).toBeGreaterThan(0);
      });

      // Let the rejection settle and give any (buggy) re-check loop time to
      // storm. The range-fetch count must PLATEAU — no auto-retry after a
      // failure (the finally re-check is skipped on the catch path).
      await new Promise((r) => setTimeout(r, 80));
      const afterSettle = rangeCalls();
      await new Promise((r) => setTimeout(r, 80));
      expect(rangeCalls()).toBe(afterSettle);

      // A subsequent user scroll DOES retrigger one retry (recovery semantics
      // preserved via the reactive), not a storm.
      box.scrollTop = 24000 * 20;
      await fireEvent.scroll(box);
      await vi.waitFor(() => {
        expect(rangeCalls()).toBeGreaterThan(afterSettle);
      });
      const afterScrollRetry = rangeCalls();
      await new Promise((r) => setTimeout(r, 80));
      // Still no storm after that one retry settles.
      expect(rangeCalls()).toBe(afterScrollRetry);
    } finally {
      restore();
    }
  });
});

// Round-2 review finding 2: the `!backfilled` branch's token bump (added to
// fix the initial head-fetch race) also cancels a user's IN-FLIGHT
// switchLogView when the SSE backfill arrives mid-switch. The user clicks a
// step after steps render but before the backfill's first batch lands;
// switchLogView([idx]) is in flight; the bump invalidates it, then installs
// the ALL-steps tail window while logView.steps already points at the step
// view — filtered chrome over unfiltered content, no self-correction. The fix:
// when `viewSwitching` is true at the `!backfilled` branch, do NOT bump/install
// the backfill; set backfilled = true (so later batches take the live-append
// path) and let the in-flight switch's install stand.
describe('RunDetail — a deferred SSE backfill does not clobber an in-flight step switch (round-2 finding 2)', () => {
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

  it('clicking a step before the SSE backfill batch arrives keeps the step view (backfill does not install all-steps rows under the filtered view)', async () => {
    try {
      // Steps render immediately (from /steps). The SSE backfill's first batch
      // (all-steps: step-0 lines) is GATED so the user can click a step first,
      // driving switchLogView([1]) into flight. When the gate releases, the
      // `!backfilled` branch runs while viewSwitching is true.
      const enc = new TextEncoder();
      let allStepsBackfill = '';
      for (let i = 0; i < 200; i++) {
        allStepsBackfill += `data: ${JSON.stringify({ type: 'log', seq: i + 1, stepIndex: 0, stream: 'stdout', line: 'ALLSTEP-LINE ' + i })}\n\n`;
      }
      let releaseBackfill = null;
      const backfillGate = new Promise((res) => { releaseBackfill = res; });
      let readCount = 0;
      const eventsResp = Promise.resolve({
        ok: true, status: 200,
        body: {
          getReader() {
            return {
              read: async () => {
                readCount++;
                if (readCount === 1) {
                  await backfillGate;
                  return { done: false, value: enc.encode(allStepsBackfill) };
                }
                return { done: true, value: undefined };
              },
            };
          },
        },
      });

      // step 1's server-side view: 200 lines, its own totalCount.
      const step1Lines = Array.from({ length: 200 }, (_, i) => (
        { seq: 5000 + i, stepIndex: 1, stream: 'stdout', line: 'STEP1-LINE ' + i }
      ));
      const step1StatsRange = statsAndRange(step1Lines.length, (row) => step1Lines[row]);

      const fetchMock = vi.fn((url) => {
        const u = String(url);
        if (u.includes('/events')) return eventsResp;
        if (u.includes('steps=1')) {
          const sr = step1StatsRange(u);
          if (sr) return sr;
        }
        // All-steps stats: report 200 (the backfill total) so the initial
        // reactive HEAD fetch path is the same as production.
        if (u.includes('/logs/stats')) return jsonResponse({ count: 200, minSeq: 1, maxSeq: 200 });
        if (u.includes('/logs/range')) return jsonResponse([]);
        if (u.includes('/steps')) return jsonResponse([
          { index: 0, stageIndex: 0, name: 'checkout', status: 'Succeeded', kind: 'run', section: 'main' },
          { index: 1, stageIndex: 1, name: 'build', status: 'Succeeded', kind: 'run', section: 'main' },
        ]);
        if (u.includes('/approvals')) return jsonResponse([]);
        if (u.includes('/artifacts')) return jsonResponse([]);
        return jsonResponse({ id: 'run-deferbackfill', status: 'Running', jobName: 'j', triggeredBy: 'x', createdAt: null, params: {} });
      });
      global.fetch = fetchMock;

      const { container } = render(RunDetail, { props: { params: { id: 'run-deferbackfill' } } });

      // Steps render before any log batch (backfill is gated).
      await vi.waitFor(() => {
        expect(container.querySelectorAll('.step-row').length).toBeGreaterThan(0);
      });

      // Click step 1 (build) BEFORE the backfill batch arrives: switchLogView([1])
      // goes in flight (viewSwitching true, its own token).
      await fireEvent.click(container.querySelectorAll('.step-row')[1]);

      // Now release the gated all-steps backfill: it hits the `!backfilled`
      // branch while viewSwitching is true. It must NOT cancel the switch or
      // install its all-steps rows under the step-1 filter.
      releaseBackfill();

      // The switch completes: step-1 lines render.
      await vi.waitFor(() => {
        const texts = [...container.querySelectorAll('.log-row')].map((r) => r.textContent);
        expect(texts.some((t) => t.includes('STEP1-LINE'))).toBe(true);
      });

      // Give any lingering microtasks a chance, then assert the all-steps
      // backfill rows never render under the step view, and the step chrome
      // (filtered: no per-line labels, "All Steps" reset button present) stands.
      await new Promise((r) => setTimeout(r, 20));
      const texts = [...container.querySelectorAll('.log-row')].map((r) => r.textContent);
      expect(texts.some((t) => t.includes('ALLSTEP-LINE'))).toBe(false);
      expect(texts.some((t) => t.includes('STEP1-LINE'))).toBe(true);
      // Filtered view: per-line step labels are hidden and the reset button shows.
      expect(container.querySelector('.log-step-label')).toBeFalsy();
      const allStepsBtn = [...container.querySelectorAll('.log-header .btn')].find((b) => b.textContent.includes('All Steps'));
      expect(allStepsBtn).toBeTruthy();
      // totalCount reflects the step view, not the all-steps backfill.
      expect(container.textContent).toContain(`${step1Lines.length} lines`);
    } finally {
      restore();
    }
  });
});
