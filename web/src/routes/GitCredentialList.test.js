import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import { token, serverURL } from '../lib/api.js';
import GitCredentialList from './GitCredentialList.svelte';

function jsonResponse(body) {
  return Promise.resolve({ ok: true, status: 200, json: async () => body, text: async () => JSON.stringify(body) });
}

const creds = [
  {
    id: '1', name: 'gh-app-release', host: 'github.com', credType: 'githubApp', secretRef: 'github-app-key',
    controllerUse: false, createdAt: '2026-11-12T00:00:00Z',
    githubApp: { appId: 123456 },
    jobAccess: { folders: ['team-a'], repositories: ['acme/app'], permissions: { contents: 'write' } },
  },
  { id: '2', name: 'plain', host: 'github.com', credType: 'token', secretRef: 'gh-token', controllerUse: true, createdAt: '2026-11-12T00:00:00Z' },
];

describe('GitCredentialList', () => {
  const origFetch = global.fetch;
  beforeEach(() => {
    token.set('');
    serverURL.set('http://localhost:8080');
    global.fetch = vi.fn(() => jsonResponse(creds));
  });
  afterEach(() => {
    global.fetch = origFetch;
  });

  it('shows App ID, job access and controller use for a githubApp credential', async () => {
    const { findByText, getAllByText } = render(GitCredentialList);
    await findByText('gh-app-release');
    expect(getAllByText(/123456/).length).toBeGreaterThan(0);
    expect(getAllByText(/team-a/).length).toBeGreaterThan(0);
    expect(getAllByText(/acme\/app/).length).toBeGreaterThan(0);
    expect(getAllByText('no').length).toBe(1);
    expect(getAllByText('yes').length).toBe(1);
    expect(getAllByText('none').length).toBe(1);
  });
});
