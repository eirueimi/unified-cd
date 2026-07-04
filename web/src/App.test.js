import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/svelte';
import { get } from 'svelte/store';
import App from './App.svelte';
import { authReady, currentUser, browserSSOEnabled } from './lib/api.js';

// Regression test for TODO #20: hard-navigating to a deep-linked hash route
// (e.g. reload on /ui/#/agents) with only an SSO session cookie (no token in
// localStorage) must not render an empty body while the async session check
// (/api/v1/auth/me) is still in flight. The route should render once the
// session check resolves, whether or not the user turns out to be
// authenticated.
describe('App — deep link render ordering (TODO #20)', () => {
  const originalFetch = global.fetch;

  afterEach(() => {
    authReady.set(false);
    currentUser.set(null);
    browserSSOEnabled.set(false);
    global.fetch = originalFetch;
  });

  it('shows a loading state (not an empty body) while the session check is pending, then renders the route once it resolves', async () => {
    window.location.hash = '#/agents';

    let resolveMe;
    const mePromise = new Promise((resolve) => { resolveMe = resolve; });

    global.fetch = vi.fn((url) => {
      if (url.includes('/api/v1/auth/oidc-config')) {
        return Promise.resolve({ ok: true, json: async () => ({ browserSSOEnabled: true }) });
      }
      if (url.includes('/api/v1/auth/me')) {
        return mePromise;
      }
      if (url.includes('/api/v1/agents')) {
        return Promise.resolve({ ok: true, json: async () => ([]) });
      }
      // Defensive fallback: svelte-spa-router's singleton location store is
      // shared across the test file, so other routes (e.g. JobList on "/")
      // may briefly mount too. Return an empty array-shaped response for
      // anything list-like so a stray mount doesn't throw.
      return Promise.resolve({ ok: true, json: async () => ([]) });
    });

    const { container, unmount } = render(App);

    // Before the session check resolves: the body must show a visible loading
    // state rather than nothing, and must NOT have already rendered the route.
    await waitFor(() => {
      expect(container.querySelector('.loading')).toBeInTheDocument();
    });
    expect(screen.queryByText(/Agent Monitor/)).not.toBeInTheDocument();

    // Session check resolves (SSO cookie was valid).
    resolveMe({ ok: true, json: async () => ({ email: 'sso@example.com' }) });

    // The deep-linked route now renders (AgentMonitor's own data fetch
    // resolves quickly since it isn't gated on anything here).
    await waitFor(() => {
      expect(screen.getByText(/Agent Monitor/)).toBeInTheDocument();
      expect(screen.getByText(/No agents registered yet/)).toBeInTheDocument();
    });
    expect(get(authReady)).toBe(true);

    // Tear down deterministically (stop the router's effect) before restoring
    // the hash, so the singleton router's hashchange listener doesn't react
    // against an unmounted component tree.
    unmount();
    window.location.hash = '';
  });
});
