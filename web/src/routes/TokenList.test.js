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

  it('shows "All tokens" and an Owner column with token.manage', async () => {
    currentUser.set({ email: 'a@example.com', role: 'admin', permissions: { global: ['token.manage'], folders: {} } });
    const { findByText } = render(TokenList);
    expect(await findByText('All tokens')).toBeTruthy();
    expect(await findByText('Owner')).toBeTruthy();
    expect(await findByText('o@example.com')).toBeTruthy();
  });
});
