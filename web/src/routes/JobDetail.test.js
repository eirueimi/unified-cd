import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import { token, serverURL } from '../lib/api.js';
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
