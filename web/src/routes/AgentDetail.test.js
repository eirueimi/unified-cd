import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import { token, serverURL, currentUser } from '../lib/api.js';
import AgentDetail from './AgentDetail.svelte';

function jsonResponse(body) {
  return Promise.resolve({ ok: true, status: 200, json: async () => body, text: async () => JSON.stringify(body) });
}

const GiB = 2 ** 30;
const TiB = 2 ** 40;

function makeAgent(over = {}) {
  return {
    id: 'a1', hostname: 'h', os: 'linux', version: '1', labels: [],
    lastSeenAt: new Date().toISOString(),
    volumeStatus: [
      { name: 'cache', ready: true },
      { name: 'scratch', ready: false, reason: 'claim pending' },
    ],
    storage: {
      volumes: {
        cache: { freeBytes: 41 * GiB, totalBytes: TiB, measuredAt: new Date().toISOString(), usedBytes: 250 * GiB },
        scratch: { freeBytes: 1 * GiB, totalBytes: 2 * GiB, measuredAt: new Date().toISOString(), claimBlocked: true },
      },
    },
    ...over,
  };
}

const decls = [
  { name: 'cache', spec: { mode: 'shared', size: '200Gi' }, updatedAt: '' },
  { name: 'scratch', spec: { mode: 'perJob', size: '10Gi' }, updatedAt: '' },
];

function mockFetch(agent, declResponse) {
  global.fetch = vi.fn((url) => {
    if (url.endsWith('/api/v1/agent-volumes')) {
      return declResponse ? declResponse() : jsonResponse(decls);
    }
    if (url.endsWith('/runs')) return jsonResponse([]);
    return jsonResponse(agent);
  });
}

function setPerms(perms) {
  currentUser.set({ email: 'a@example.com', permissions: { global: perms, folders: {} } });
}

describe('AgentDetail Volumes card', () => {
  const origFetch = global.fetch;
  beforeEach(() => {
    token.set('');
    serverURL.set('http://localhost:8080');
  });
  afterEach(() => {
    currentUser.set(null);
    global.fetch = origFetch;
  });

  it('lists volumes with state, declaration and readings', async () => {
    setPerms(['agent.read', 'agentvolume.read']);
    mockFetch(makeAgent());
    const { findByText, getByText } = render(AgentDetail, { params: { id: 'a1' } });
    await findByText('Volumes');
    expect(getByText('cache')).toBeTruthy();
    expect(getByText('scratch')).toBeTruthy();
    expect(getByText('Ready')).toBeTruthy();
    expect(getByText('Not ready')).toBeTruthy();
    expect(getByText(/claim pending/)).toBeTruthy();
    expect(getByText('shared')).toBeTruthy();
    expect(getByText('200Gi')).toBeTruthy();
    expect(getByText('41.0 GiB / 1.0 TiB')).toBeTruthy();
  });

  it('shows a dash for used on a host agent', async () => {
    setPerms(['agent.read', 'agentvolume.read']);
    const agent = makeAgent();
    delete agent.storage.volumes.cache.usedBytes;
    mockFetch(agent);
    const { findByText, container } = render(AgentDetail, { params: { id: 'a1' } });
    await findByText('Volumes');
    const cell = container.querySelector('td[data-label="Used"]');
    expect(cell.textContent.trim()).toBe('—');
  });

  it('shows used and the over-size marker on a k8s agent', async () => {
    setPerms(['agent.read', 'agentvolume.read']);
    mockFetch(makeAgent({ os: 'linux/k8s' }));
    const { findByText, container } = render(AgentDetail, { params: { id: 'a1' } });
    await findByText('Volumes');
    const cell = container.querySelector('td[data-label="Used"]');
    expect(cell.textContent).toContain('250.0 GiB');
    expect(cell.textContent).toContain('⚠');
  });

  it('does not fetch declarations without agentvolume.read', async () => {
    setPerms(['agent.read']);
    mockFetch(makeAgent());
    const { findByText, container } = render(AgentDetail, { params: { id: 'a1' } });
    await findByText('Volumes');
    expect(global.fetch.mock.calls.some(([u]) => u.includes('/agent-volumes'))).toBe(false);
    expect(container.querySelector('td[data-label="Mode"]').textContent.trim()).toBe('—');
    expect(container.querySelector('td[data-label="Size"]').textContent.trim()).toBe('—');
  });

  it('keeps the page when the declarations fetch fails', async () => {
    setPerms(['agent.read', 'agentvolume.read']);
    mockFetch(makeAgent(), () => Promise.resolve({ ok: false, status: 500, json: async () => ({}), text: async () => 'boom' }));
    const { findByText, container } = render(AgentDetail, { params: { id: 'a1' } });
    await findByText('Volumes');
    expect(container.querySelector('td[data-label="Mode"]').textContent.trim()).toBe('—');
  });

  it('renders no card without volumeStatus', async () => {
    setPerms(['agent.read', 'agentvolume.read']);
    mockFetch(makeAgent({ volumeStatus: undefined }));
    const { findByText, queryByText } = render(AgentDetail, { params: { id: 'a1' } });
    await findByText('Recent Runs');
    expect(queryByText('Volumes')).toBeNull();
  });

  it('renders rows with dashes when storage is absent', async () => {
    setPerms(['agent.read', 'agentvolume.read']);
    mockFetch(makeAgent({ storage: undefined }));
    const { findByText, container } = render(AgentDetail, { params: { id: 'a1' } });
    await findByText('Volumes');
    expect(container.querySelector('td[data-label="Disk free"]').textContent.trim()).toBe('—');
  });

  it('marks a claim-blocked free cell with the failure class', async () => {
    setPerms(['agent.read', 'agentvolume.read']);
    mockFetch(makeAgent());
    const { findByText, container } = render(AgentDetail, { params: { id: 'a1' } });
    await findByText('Volumes');
    const cells = container.querySelectorAll('td[data-label="Disk free"]');
    expect(cells[1].classList.contains('vol-blocked')).toBe(true);
    expect(cells[0].classList.contains('vol-blocked')).toBe(false);
  });
});
