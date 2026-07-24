import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import { token, serverURL } from '../lib/api.js';
import JobList from './JobList.svelte';

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
  localStorage.clear();
});

// Task 2: JobList shows each job's description (from GET /api/v1/jobs) under
// its name, and shows nothing for jobs without one.
describe('JobList — description display (Task 2)', () => {
  it('shows the description when a job has one', async () => {
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/api/v1/runs/active')) return jsonResponse([]);
      if (u.includes('/api/v1/jobs')) {
        return jsonResponse([
          { name: 'nightly-build', leaf: 'nightly-build', path: '', updatedAt: '2026-07-01T00:00:00Z', description: 'Nightly build' },
        ]);
      }
      return jsonResponse({});
    });
    global.fetch = fetchMock;

    const { container } = render(JobList);

    await vi.waitFor(() => {
      expect(container.textContent).toContain('Nightly build');
    });
  });

  it('shows no description node for a job without one', async () => {
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/api/v1/runs/active')) return jsonResponse([]);
      if (u.includes('/api/v1/jobs')) {
        return jsonResponse([
          { name: 'plain-job', leaf: 'plain-job', path: '', updatedAt: '2026-07-01T00:00:00Z', description: '' },
        ]);
      }
      return jsonResponse({});
    });
    global.fetch = fetchMock;

    const { container } = render(JobList);

    await vi.waitFor(() => {
      expect(container.textContent).toContain('plain-job');
    });
    // The "Updated" column also uses class="meta" on a <td>; the description
    // node this feature adds is a <div class="meta"> inside the name cell —
    // check specifically for that, not the always-present date cell.
    expect(container.querySelector('div.meta')).toBeFalsy();
  });
});

// Task 2: folders collapse by default (persisted expanded set) + pinned
// Running section above the tree.
describe('JobList — collapse-by-default and pinned Running section (Task 2)', () => {
  it('folders collapse by default: a job inside a folder is not shown until expanded', async () => {
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/api/v1/runs/active')) return jsonResponse([]);
      if (u.includes('/api/v1/jobs')) {
        return jsonResponse([
          { name: 'team-a/build', leaf: 'build', path: 'team-a', updatedAt: '2026-07-01T00:00:00Z' },
        ]);
      }
      return jsonResponse({});
    });
    global.fetch = fetchMock;

    const { container, queryByText } = render(JobList);

    await vi.waitFor(() => {
      expect(container.textContent).toContain('team-a');
    });
    expect(queryByText('build')).toBeFalsy();
  });

  it('expanded state persists via localStorage: a previously expanded folder starts open', async () => {
    localStorage.setItem('ucd.joblist.expanded', JSON.stringify(['team-a']));
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/api/v1/runs/active')) return jsonResponse([]);
      if (u.includes('/api/v1/jobs')) {
        return jsonResponse([
          { name: 'team-a/build', leaf: 'build', path: 'team-a', updatedAt: '2026-07-01T00:00:00Z' },
        ]);
      }
      return jsonResponse({});
    });
    global.fetch = fetchMock;

    const { getByText } = render(JobList);

    await vi.waitFor(() => {
      expect(getByText('build')).toBeTruthy();
    });
  });

  it('pins a Running section that mirrors an active job even while its folder is collapsed', async () => {
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/api/v1/runs/active')) {
        return jsonResponse([
          { id: 'r1', jobName: 'team-a/build', status: 'Running', createdAt: '2026-07-01T00:00:00Z' },
        ]);
      }
      if (u.includes('/api/v1/jobs')) {
        return jsonResponse([
          { name: 'team-a/build', leaf: 'build', path: 'team-a', updatedAt: '2026-07-01T00:00:00Z' },
        ]);
      }
      return jsonResponse({});
    });
    global.fetch = fetchMock;

    const { container, queryByText } = render(JobList);

    // Folder stays collapsed (no stored expanded state), so the only way
    // 'team-a/build' and a Running badge can show up is via the pinned section.
    await vi.waitFor(() => {
      expect(container.textContent).toContain('team-a/build');
      expect(container.textContent).toContain('Running');
    });
    expect(queryByText('build')).toBeFalsy();
  });

  it('renders no pinned Running section when there are no active runs', async () => {
    const fetchMock = vi.fn((url) => {
      const u = String(url);
      if (u.includes('/api/v1/runs/active')) return jsonResponse([]);
      if (u.includes('/api/v1/jobs')) {
        return jsonResponse([
          { name: 'team-a/build', leaf: 'build', path: 'team-a', updatedAt: '2026-07-01T00:00:00Z' },
        ]);
      }
      return jsonResponse({});
    });
    global.fetch = fetchMock;

    const { container } = render(JobList);

    await vi.waitFor(() => {
      expect(container.textContent).toContain('team-a');
    });
    expect(container.textContent).not.toContain('Running');
  });
});
