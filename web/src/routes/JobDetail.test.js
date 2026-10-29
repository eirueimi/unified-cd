import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import { token, serverURL, currentUser } from '../lib/api.js';
import JobDetail from './JobDetail.svelte';

function jsonResponse(body) {
  return Promise.resolve({
    ok: true,
    status: 200,
    json: async () => body,
    text: async () => JSON.stringify(body),
  });
}

beforeEach(() => {
  token.set('');
  serverURL.set('http://localhost:8080');
});

// Task 6: JobDetail shows a warning banner when the job can't currently be
// scheduled (Task 5's GET /api/v1/jobs/{name}/schedulability reports
// satisfiable:false), and shows nothing when it's satisfiable or the fetch
// fails.
describe('JobDetail — unschedulable warning banner (Task 6)', () => {
  it("renders a warning banner with the reason when schedulability is unsatisfiable", async () => {
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/schedulability')) {
        return jsonResponse({
          requiredCaps: ['native'],
          selector: [],
          satisfiable: false,
          reason: 'no registered agent provides capability [native]',
          selectorDependsOnParams: false,
        });
      }
      if (u.includes('/runs')) return jsonResponse([]);
      return jsonResponse({});
    });
    global.fetch = fetchMock;

    const { container } = render(JobDetail, { props: { params: { name: 'job-a' } } });

    await vi.waitFor(() => {
      expect(container.querySelector('[role="alert"]')).toBeTruthy();
    });
    const banner = container.querySelector('[role="alert"]');
    expect(banner.textContent).toContain('no registered agent provides capability [native]');
  });

  it('renders no warning banner when schedulability is satisfiable', async () => {
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/schedulability')) {
        return jsonResponse({
          requiredCaps: [],
          selector: [],
          satisfiable: true,
          reason: '',
          selectorDependsOnParams: false,
        });
      }
      if (u.includes('/runs')) return jsonResponse([]);
      return jsonResponse({});
    });
    global.fetch = fetchMock;

    const { container } = render(JobDetail, { props: { params: { name: 'job-b' } } });

    await vi.waitFor(() => {
      expect(container.textContent).toContain('No runs yet.');
    });
    expect(container.querySelector('[role="alert"]')).toBeFalsy();
  });

  it('renders no warning banner when the schedulability fetch fails', async () => {
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/schedulability')) {
        return Promise.resolve({ ok: false, status: 500, text: async () => 'boom' });
      }
      if (u.includes('/runs')) return jsonResponse([]);
      return jsonResponse({});
    });
    global.fetch = fetchMock;

    const { container } = render(JobDetail, { props: { params: { name: 'job-c' } } });

    await vi.waitFor(() => {
      expect(container.textContent).toContain('No runs yet.');
    });
    expect(container.querySelector('[role="alert"]')).toBeFalsy();
  });
});

// Task 2: JobDetail shows the job's description under the heading when the
// job fetch (GET /api/v1/jobs/{name}) returns a non-empty `description`, and
// shows nothing (without crashing the runs list) when it's absent or the
// fetch fails.
describe('JobDetail — description display (Task 2)', () => {
  it('renders the description when the job fetch returns one', async () => {
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/schedulability')) return jsonResponse({ satisfiable: true });
      if (u.includes('/runs')) return jsonResponse([]);
      if (u.includes('/api/v1/jobs/')) return jsonResponse({ name: 'deploy', description: 'Deploys the app' });
      return jsonResponse({});
    });
    global.fetch = fetchMock;

    const { container } = render(JobDetail, { props: { params: { name: 'deploy' } } });

    await vi.waitFor(() => {
      expect(container.textContent).toContain('Deploys the app');
    });
  });

  it('renders no description when the job has none, and the runs area still renders', async () => {
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/schedulability')) return jsonResponse({ satisfiable: true });
      if (u.includes('/runs')) return jsonResponse([]);
      if (u.includes('/api/v1/jobs/')) return jsonResponse({ name: 'deploy', description: '' });
      return jsonResponse({});
    });
    global.fetch = fetchMock;

    const { container } = render(JobDetail, { props: { params: { name: 'deploy' } } });

    await vi.waitFor(() => {
      expect(container.textContent).toContain('No runs yet.');
    });
    expect(container.querySelector('.meta')).toBeFalsy();
  });

  it('renders no description and does not crash when the job fetch fails', async () => {
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/schedulability')) return jsonResponse({ satisfiable: true });
      if (u.includes('/runs')) return jsonResponse([]);
      if (u.includes('/api/v1/jobs/')) return Promise.resolve({ ok: false, status: 500, text: async () => 'boom' });
      return jsonResponse({});
    });
    global.fetch = fetchMock;

    const { container } = render(JobDetail, { props: { params: { name: 'deploy' } } });

    await vi.waitFor(() => {
      expect(container.textContent).toContain('No runs yet.');
    });
    expect(container.querySelector('[role="alert"]')).toBeFalsy();
  });
});

// run-display-name feature: JobDetail's run list shows a run's displayName
// (interpolated at run-creation time from the job's `displayName:` DSL field)
// as the primary link text in place of the truncated run ID when present, and
// renders byte-for-byte identically to today (`{r.id.slice(0, 8)}…`) when
// absent.
describe('JobDetail — run display name (run-display-name feature)', () => {
  it('shows the display name instead of the truncated run ID when present', async () => {
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/schedulability')) return jsonResponse({ satisfiable: true });
      if (u.includes('/runs')) return jsonResponse([{
        id: 'abcdef1234567890',
        status: 'Succeeded',
        triggeredBy: 'api',
        createdAt: new Date().toISOString(),
        params: {},
        displayName: 'deploy prod',
      }]);
      if (u.includes('/api/v1/jobs/')) return jsonResponse({ name: 'deploy', description: '' });
      return jsonResponse({});
    });
    global.fetch = fetchMock;

    const { container } = render(JobDetail, { props: { params: { name: 'deploy' } } });

    await vi.waitFor(() => {
      expect(container.textContent).toContain('deploy prod');
    });
  });

  it('falls back to the truncated run ID when displayName is absent, unchanged from today', async () => {
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/schedulability')) return jsonResponse({ satisfiable: true });
      if (u.includes('/runs')) return jsonResponse([{
        id: 'abcdef1234567890',
        status: 'Succeeded',
        triggeredBy: 'api',
        createdAt: new Date().toISOString(),
        params: {},
      }]);
      if (u.includes('/api/v1/jobs/')) return jsonResponse({ name: 'deploy', description: '' });
      return jsonResponse({});
    });
    global.fetch = fetchMock;

    const { container } = render(JobDetail, { props: { params: { name: 'deploy' } } });

    await vi.waitFor(() => {
      expect(container.textContent).toContain('abcdef12…');
    });
  });
});

describe('JobDetail — permission-aware controls', () => {
  const origFetch = global.fetch;
  afterEach(() => {
    currentUser.set(null);
    global.fetch = origFetch;
  });

  it('renders the Run tab as a disabled span without run.trigger on the job', async () => {
    currentUser.set({ email: 'a@example.com', permissions: { global: [], folders: { 'team-a': ['job.read'] } } });
    global.fetch = vi.fn((url) => (String(url).includes('/runs') ? jsonResponse([]) : jsonResponse({})));
    const { findByText } = render(JobDetail, { props: { params: { name: 'team-a/deploy' } } });
    const tab = await findByText('▶ Run');
    expect(tab.tagName).toBe('SPAN');
    expect(tab.title).toBe('requires run.trigger on team-a/deploy');
  });

  it('keeps the Run tab a link with run.trigger', async () => {
    currentUser.set({ email: 'a@example.com', permissions: { global: [], folders: { 'team-a': ['run.trigger'] } } });
    global.fetch = vi.fn((url) => (String(url).includes('/runs') ? jsonResponse([]) : jsonResponse({})));
    const { findByText } = render(JobDetail, { props: { params: { name: 'team-a/deploy' } } });
    expect((await findByText('▶ Run')).tagName).toBe('A');
  });

  it('shows the server message, not the not-found view, for a 403', async () => {
    global.fetch = vi.fn(() => Promise.resolve({ ok: false, status: 403, text: async () => 'forbidden: missing permission "job.read"' }));
    const { findByText, queryByText } = render(JobDetail, { props: { params: { name: 'nope' } } });
    expect(await findByText('forbidden: missing permission "job.read"')).toBeTruthy();
    expect(queryByText('Job not found')).toBeNull();
  });

  it('renders the not-found view for a 404', async () => {
    global.fetch = vi.fn(() => Promise.resolve({ ok: false, status: 404, text: async () => 'job not found' }));
    const { findByText } = render(JobDetail, { props: { params: { name: 'nope' } } });
    expect(await findByText('Job not found')).toBeTruthy();
  });
});
