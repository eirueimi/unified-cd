import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import { token, serverURL, currentUser } from '../lib/api.js';
import TokenList from './TokenList.svelte';

function jsonResponse(body) {
  return Promise.resolve({ ok: true, status: 200, json: async () => body, text: async () => JSON.stringify(body) });
}

const tokens = [{ id: 't1', name: 'ci', role: 'viewer', owner: 'o@example.com', createdAt: '2026-01-01T00:00:00Z' }];

describe('TokenList — permission-aware form and listing', () => {
  const origFetch = global.fetch;
  beforeEach(() => {
    token.set('');
    serverURL.set('http://localhost:8080');
    global.fetch = vi.fn(() => jsonResponse(tokens));
  });
  afterEach(() => {
    currentUser.set(null);
    global.fetch = origFetch;
  });

  it('offers only roles not above the caller role', async () => {
    currentUser.set({ email: 'v@example.com', role: 'viewer', permissions: { global: ['token.create'], folders: {} } });
    const { container } = render(TokenList);
    const options = [...container.querySelectorAll('select option')].map((o) => o.textContent.trim());
    expect(options).toEqual(['Role (default: yourself)', 'viewer']);
  });

  it('lists the permissions held anywhere as a checklist', async () => {
    currentUser.set({ email: 'v@example.com', role: 'viewer', permissions: { global: ['token.create'], folders: { 'team-a': ['run.trigger'] } } });
    const { container } = render(TokenList);
    const boxes = [...container.querySelectorAll('input[type="checkbox"]')].map((c) => c.value);
    expect(boxes).toEqual(['run.trigger', 'token.create']);
  });

  it('shows "Your tokens" and no Owner column without token.manage', async () => {
    currentUser.set({ email: 'v@example.com', role: 'viewer', permissions: { global: ['token.create'], folders: {} } });
    const { findByText, queryByText } = render(TokenList);
    expect(await findByText('Your tokens')).toBeTruthy();
    expect(queryByText('Owner')).toBeNull();
  });

  it('renders the Account and Expires columns', async () => {
    global.fetch = vi.fn(() => jsonResponse([{ id: 't2', name: 'deploy', role: 'viewer', account: 'sa:ci', createdAt: '2026-01-01T00:00:00Z', expiresAt: '2026-04-01T00:00:00Z' }]));
    currentUser.set({ email: 'v@example.com', role: 'viewer', permissions: { global: ['token.create'], folders: {} } });
    const { findByText } = render(TokenList);
    expect(await findByText('Account')).toBeTruthy();
    expect(await findByText('Expires')).toBeTruthy();
    expect(await findByText('sa:ci')).toBeTruthy();
  });

  it('shows the Account field only with token.manage or serviceaccount.token', async () => {
    currentUser.set({ email: 'v@example.com', role: 'viewer', permissions: { global: ['token.create'], folders: {} } });
    const plain = render(TokenList);
    expect(plain.container.querySelector('input[placeholder^="Account"]')).toBeNull();
    plain.unmount();

    currentUser.set({ email: 'a@example.com', role: 'admin', permissions: { global: ['serviceaccount.token'], folders: {} } });
    const sa = render(TokenList);
    expect(sa.container.querySelector('input[placeholder^="Account"]')).not.toBeNull();
    sa.unmount();

    currentUser.set({ email: 'a@example.com', role: 'admin', permissions: { global: ['token.manage'], folders: {} } });
    const mgr = render(TokenList);
    expect(mgr.container.querySelector('input[placeholder^="Account"]')).not.toBeNull();
  });

  it('enables Issue with serviceaccount.token alone', async () => {
    currentUser.set({ email: 'a@example.com', role: 'viewer', permissions: { global: ['serviceaccount.token'], folders: {} } });
    const { findByText } = render(TokenList);
    const issue = await findByText('Issue');
    expect(issue.disabled).toBe(false);
  });

  it('shows "All tokens" and an Owner column with token.manage', async () => {
    currentUser.set({ email: 'a@example.com', role: 'admin', permissions: { global: ['token.manage'], folders: {} } });
    const { findByText } = render(TokenList);
    expect(await findByText('All tokens')).toBeTruthy();
    expect(await findByText('Owner')).toBeTruthy();
    expect(await findByText('o@example.com')).toBeTruthy();
  });
});
