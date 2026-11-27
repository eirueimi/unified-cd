import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import { token, serverURL, currentUser } from '../lib/api.js';
import AccountList from './AccountList.svelte';

const accounts = [
  { id: 'h1', kind: 'human', name: 'alice@example.com', email: 'alice@example.com', disabled: false, createdAt: '2026-01-01T00:00:00Z', patCount: 2 },
  { id: 'h2', kind: 'human', name: 'bob@example.com', email: 'bob@example.com', disabled: true, createdAt: '2026-01-01T00:00:00Z', patCount: 0 },
  { id: 's1', kind: 'service', name: 'ci', disabled: false, createdAt: '2026-01-01T00:00:00Z', patCount: 1 },
];

describe('AccountList', () => {
  const origFetch = global.fetch;
  let calls;
  beforeEach(() => {
    token.set('');
    serverURL.set('http://localhost:8080');
    currentUser.set({ email: 'a@example.com', role: 'admin', permissions: { global: ['account.manage'], folders: {} } });
    calls = [];
    global.fetch = vi.fn((url, opts = {}) => {
      calls.push({ url, method: opts.method || 'GET' });
      const body = opts.method === 'POST' ? null : accounts;
      return Promise.resolve({
        ok: true,
        status: opts.method === 'POST' ? 204 : 200,
        json: async () => body,
        text: async () => JSON.stringify(body),
      });
    });
  });
  afterEach(() => {
    currentUser.set(null);
    global.fetch = origFetch;
  });

  it('lists accounts with kind and name', async () => {
    const { findByText } = render(AccountList);
    expect(await findByText('alice@example.com')).toBeTruthy();
    expect(await findByText('ci')).toBeTruthy();
  });

  it('offers Disable/Enable for humans only', async () => {
    const { findByText, getAllByText, queryAllByText } = render(AccountList);
    await findByText('alice@example.com');
    expect(getAllByText('Disable')).toHaveLength(1);
    expect(getAllByText('Enable')).toHaveLength(1);
    expect(queryAllByText('Disable').length + queryAllByText('Enable').length).toBe(2);
  });

  it('the Disable button POSTs to /api/v1/accounts/<id>/disable', async () => {
    const { findByText } = render(AccountList);
    await fireEvent.click(await findByText('Disable'));
    await waitFor(() => {
      expect(calls.some((c) => c.method === 'POST' && c.url === 'http://localhost:8080/api/v1/accounts/h1/disable')).toBe(true);
    });
  });

  it('the Enable button POSTs to /api/v1/accounts/<id>/enable', async () => {
    const { findByText } = render(AccountList);
    await fireEvent.click(await findByText('Enable'));
    await waitFor(() => {
      expect(calls.some((c) => c.method === 'POST' && c.url === 'http://localhost:8080/api/v1/accounts/h2/enable')).toBe(true);
    });
  });
});
