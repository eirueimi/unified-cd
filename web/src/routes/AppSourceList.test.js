import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import { token, serverURL, currentUser } from '../lib/api.js';
import AppSourceList from './AppSourceList.svelte';

function jsonResponse(body) {
  return Promise.resolve({ ok: true, status: 200, json: async () => body, text: async () => JSON.stringify(body) });
}

const sources = [{ name: 'apps', repoURL: 'https://example.com/r.git', targetRevision: 'main', path: '' }];

describe('AppSourceList — Sync is global-only', () => {
  const origFetch = global.fetch;
  beforeEach(() => {
    token.set('');
    serverURL.set('http://localhost:8080');
    global.fetch = vi.fn(() => jsonResponse(sources));
  });
  afterEach(() => {
    currentUser.set(null);
    global.fetch = origFetch;
  });

  it('hides Sync without appsource.write', async () => {
    currentUser.set({ email: 'a@example.com', permissions: { global: ['appsource.read'], folders: {} } });
    const { findByText, queryByText } = render(AppSourceList);
    await findByText('apps');
    expect(queryByText(/Sync/)).toBeNull();
  });

  it('shows Sync with appsource.write', async () => {
    currentUser.set({ email: 'a@example.com', permissions: { global: ['appsource.write'], folders: {} } });
    const { findByText } = render(AppSourceList);
    expect(await findByText(/Sync/)).toBeTruthy();
  });
});

describe('AppSourceList — never renders repository credentials', () => {
  const origFetch = global.fetch;
  beforeEach(() => {
    token.set('');
    serverURL.set('http://localhost:8080');
    global.fetch = vi.fn(() => jsonResponse([{
      name: 'leaky',
      repoURL: 'https://user:s3cr3tpw@example.com/r.git',
      targetRevision: 'main',
      path: '',
      syncStatus: 'Failed',
      lastError: 'fetch https://user:s3cr3tpw@example.com/r.git: denied',
    }]));
  });
  afterEach(() => {
    currentUser.set(null);
    global.fetch = origFetch;
  });

  it('strips userinfo from the link, its text, and the error tooltip', async () => {
    const { findByText, container } = render(AppSourceList);
    await findByText('leaky');
    expect(container.innerHTML).not.toContain('s3cr3t');
    const a = container.querySelector('a[target="_blank"]');
    expect(a.getAttribute('href')).toBe('https://example.com/r.git');
    expect(a.textContent).toBe('example.com/r.git');
  });
});
