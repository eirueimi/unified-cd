import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import { token, serverURL, currentUser } from '../lib/api.js';
import AgentVolumeList from './AgentVolumeList.svelte';

function jsonResponse(body) {
  return Promise.resolve({ ok: true, status: 200, json: async () => body, text: async () => JSON.stringify(body) });
}

const GiB = 2 ** 30;

const decls = [
  { name: 'cache', spec: { mode: 'shared', size: '200Gi', agentSelector: ['role:unity', 'os:linux'], reclaimPolicy: 'Delete' }, updatedAt: '' },
  { name: 'orphan', spec: { mode: 'exclusive', size: '10Gi' }, updatedAt: '' },
];

function agent(id, over = {}) {
  return {
    id, hostname: id, os: 'linux', version: '1', labels: [],
    lastSeenAt: new Date().toISOString(),
    volumeStatus: [{ name: 'cache', ready: true }],
    ...over,
  };
}

const agents = [
  agent('k8s-1', {
    os: 'linux/k8s',
    storage: { agentName: 'builders', volumes: { cache: { freeBytes: GiB, totalBytes: 2 * GiB, measuredAt: new Date().toISOString(), usedBytes: 12 * GiB } } },
  }),
  agent('host-1', {
    lastSeenAt: new Date(Date.now() - 10 * 60 * 1000).toISOString(),
    volumeStatus: [{ name: 'cache', ready: false, reason: 'claim pending' }],
  }),
];

let calls;
function mockFetch() {
  calls = [];
  global.fetch = vi.fn((url) => {
    calls.push(url);
    if (url.endsWith('/api/v1/agent-volumes')) return jsonResponse(decls);
    if (url.endsWith('/api/v1/agents')) return jsonResponse(agents);
    return jsonResponse([]);
  });
}

function setPerms(perms) {
  currentUser.set({ email: 'a@example.com', permissions: { global: perms, folders: {} } });
}

describe('AgentVolumeList', () => {
  const origFetch = global.fetch;
  beforeEach(() => {
    token.set('');
    serverURL.set('http://localhost:8080');
    mockFetch();
  });
  afterEach(() => {
    currentUser.set(null);
    global.fetch = origFetch;
  });

  it('shows declaration columns and aggregated usage with both permissions', async () => {
    setPerms(['agentvolume.read', 'agent.read']);
    const { findByText, getByText, container } = render(AgentVolumeList);
    await findByText('cache');
    const row = getByText('cache').closest('tr');
    expect(row.textContent).toContain('shared');
    expect(row.textContent).toContain('role:unity, os:linux');
    expect(row.textContent).toContain('Delete');
    expect(row.textContent).toContain('1 / 2');
    expect(row.textContent).toContain('12.0 GiB');
    expect(row.textContent).toContain('(1 of 2 measured)');
    const link = row.querySelector('a[href="#/agents/k8s-1"]');
    expect(link).toBeTruthy();
    expect(container.querySelector('table.cards')).toBeTruthy();
    for (const td of container.querySelectorAll('tbody tr:not(.vol-detail) > td')) {
      expect(td.getAttribute('data-label')).toBeTruthy();
    }
  });

  it('defaults the reclaim policy to Retain', async () => {
    setPerms(['agentvolume.read', 'agent.read']);
    const { findByText } = render(AgentVolumeList);
    const row = (await findByText('orphan')).closest('tr');
    expect(row.textContent).toContain('Retain');
  });

  it('expands a row to the per-agent breakdown and shows Offline', async () => {
    setPerms(['agentvolume.read', 'agent.read']);
    const { findByText, getByText, queryByText } = render(AgentVolumeList);
    await findByText('cache');
    expect(queryByText('Offline')).toBeNull();
    await fireEvent.click(getByText('cache'));
    await waitFor(() => expect(getByText('Offline')).toBeTruthy());
    expect(getByText('claim pending')).toBeTruthy();
    expect(getByText('host-1').closest('a').getAttribute('href')).toBe('#/agents/host-1');
  });

  it('shows 0 / 0 and a dash for a volume no agent was asked to provision', async () => {
    setPerms(['agentvolume.read', 'agent.read']);
    const { findByText, getByText } = render(AgentVolumeList);
    const row = (await findByText('orphan')).closest('tr');
    expect(row.textContent).toContain('0 / 0');
    expect(row.textContent).toContain('—');
    await fireEvent.click(getByText('orphan'));
    expect(await findByText('No agent has been asked to provision this volume.')).toBeTruthy();
  });

  it('without agent.read, never fetches agents, hides agent columns and does not expand', async () => {
    setPerms(['agentvolume.read']);
    const { findByText, getByText, queryByText } = render(AgentVolumeList);
    await findByText('cache');
    expect(calls.some((u) => u.endsWith('/api/v1/agents'))).toBe(false);
    for (const h of ['Agents', 'Total used', 'Largest']) {
      expect(queryByText(h)).toBeNull();
    }
    await fireEvent.click(getByText('cache'));
    expect(queryByText('No agent has been asked to provision this volume.')).toBeNull();
    expect(queryByText('Offline')).toBeNull();
  });
});
