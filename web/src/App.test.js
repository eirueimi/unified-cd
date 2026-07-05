import { describe, it, expect, vi, afterEach, beforeEach } from 'vitest';
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

// Regression test for #34: AuthSetup.test.js used to assert the logged-in
// header (email + logout button) against AuthSetup.svelte, but that markup
// actually lives in App.svelte's <nav> — AuthSetup renders nothing once
// $currentUser is set. These assertions belong here instead, and check the
// actual English strings App.svelte renders after the SSO header copy was
// unified to English (previously Japanese).
describe('App — logged-in header', () => {
  const originalFetch = global.fetch;

  beforeEach(() => {
    global.fetch = vi.fn((url) => {
      if (url.includes('/api/v1/auth/oidc-config')) {
        return Promise.resolve({ ok: true, json: async () => ({ browserSSOEnabled: false }) });
      }
      if (url.includes('/api/v1/auth/me')) {
        return Promise.resolve({ ok: false, status: 401, json: async () => ({}) });
      }
      return Promise.resolve({ ok: true, json: async () => ([]) });
    });
  });

  afterEach(() => {
    authReady.set(false);
    currentUser.set(null);
    browserSSOEnabled.set(false);
    global.fetch = originalFetch;
    window.location.hash = '';
  });

  it('shows the user email and a "Log out" button when currentUser is set', async () => {
    currentUser.set({ email: 'user@example.com' });
    authReady.set(true);

    const { unmount } = render(App);

    await waitFor(() => {
      expect(screen.getByText('user@example.com')).toBeInTheDocument();
      expect(screen.getByText('Log out')).toBeInTheDocument();
    });

    unmount();
  });

  it('shows the SSO login button (not the logged-in header) when logged out and SSO is enabled', async () => {
    browserSSOEnabled.set(true);
    currentUser.set(null);
    authReady.set(true);

    const { unmount } = render(App);

    await waitFor(() => {
      expect(screen.getByText(/Log in with SSO/)).toBeInTheDocument();
    });
    expect(screen.queryByText('Log out')).not.toBeInTheDocument();

    unmount();
  });
});

// Regression test for #33: hash routing loses the intended deep-linked route
// (e.g. #/runs/xyz) across the full-page navigation to the IdP and back
// during an SSO login. App.svelte's ssoLogin() persists the current hash to
// localStorage before redirecting; once the app remounts on /ui/ and the
// session check resolves, the saved hash should be restored and cleared.
describe('App — deep-link restoration after SSO (#33)', () => {
  const originalFetch = global.fetch;

  beforeEach(() => {
    global.fetch = vi.fn((url) => {
      if (url.includes('/api/v1/auth/oidc-config')) {
        return Promise.resolve({ ok: true, json: async () => ({ browserSSOEnabled: true }) });
      }
      if (url.includes('/api/v1/auth/me')) {
        return Promise.resolve({ ok: true, json: async () => ({ email: 'sso@example.com' }) });
      }
      return Promise.resolve({ ok: true, json: async () => ([]) });
    });
  });

  afterEach(() => {
    authReady.set(false);
    currentUser.set(null);
    browserSSOEnabled.set(false);
    global.fetch = originalFetch;
    localStorage.removeItem('ecd_post_login_hash');
    window.location.hash = '';
  });

  it('restores a hash saved before the SSO redirect once the session resolves, then clears it', async () => {
    // Simulate the state left behind by ssoLogin() before the redirect to the
    // IdP: the app lands back on /ui/ with no hash, but a saved route.
    localStorage.setItem('ecd_post_login_hash', '#/runs/xyz');
    window.location.hash = '';

    const { unmount } = render(App);

    await waitFor(() => {
      expect(window.location.hash).toBe('#/runs/xyz');
    });
    expect(localStorage.getItem('ecd_post_login_hash')).toBeNull();

    unmount();
  });

  it('does not touch the hash when nothing was saved', async () => {
    localStorage.removeItem('ecd_post_login_hash');
    window.location.hash = '';

    const { unmount } = render(App);

    await waitFor(() => {
      expect(get(authReady)).toBe(true);
    });
    expect(window.location.hash).toBe('');

    unmount();
  });
});
