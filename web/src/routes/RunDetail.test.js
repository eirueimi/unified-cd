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

  it('shows a truncation banner when the server drops older backfill lines', async () => {
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
      const banner = container.querySelector('.log-truncated');
      expect(banner).toBeTruthy();
      expect(banner.textContent).toContain('truncated');
    });
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
