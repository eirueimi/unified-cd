import { describe, it, expect, beforeEach, vi } from 'vitest';
import { get } from 'svelte/store';
import { browserSSOEnabled, currentUser, serverURL, authReady, initAuth, jobPath } from './api.js';

beforeEach(() => {
  browserSSOEnabled.set(false);
  currentUser.set(null);
  serverURL.set('http://localhost');
  authReady.set(false);
  vi.resetAllMocks();
});

describe('initAuth — browserSSOEnabled', () => {
  it('store becomes true when the server returns browserSSOEnabled:true', async () => {
    global.fetch = vi.fn()
      .mockResolvedValueOnce({ ok: true, json: async () => ({ browserSSOEnabled: true, clientId: 'test', issuer: 'http://localhost' }) })
      .mockResolvedValueOnce({ ok: false });

    await initAuth();

    expect(get(browserSSOEnabled)).toBe(true);
  });

  it('store stays false when the server returns browserSSOEnabled:false', async () => {
    global.fetch = vi.fn()
      .mockResolvedValueOnce({ ok: true, json: async () => ({ browserSSOEnabled: false }) })
      .mockResolvedValueOnce({ ok: false });

    await initAuth();

    expect(get(browserSSOEnabled)).toBe(false);
  });

  it('store stays false when fetch fails', async () => {
    global.fetch = vi.fn().mockRejectedValue(new Error('network error'));

    await initAuth();

    expect(get(browserSSOEnabled)).toBe(false);
  });

  it('store stays false when the oidc-config response is not ok', async () => {
    global.fetch = vi.fn()
      .mockResolvedValueOnce({ ok: false })
      .mockResolvedValueOnce({ ok: false });

    await initAuth();

    expect(get(browserSSOEnabled)).toBe(false);
  });
});

describe('initAuth — currentUser', () => {
  it('currentUser is set when /auth/me succeeds', async () => {
    const user = { email: 'test@example.com', name: 'Test User' };
    global.fetch = vi.fn()
      .mockResolvedValueOnce({ ok: true, json: async () => ({ browserSSOEnabled: true }) })
      .mockResolvedValueOnce({ ok: true, json: async () => user });

    await initAuth();

    expect(get(currentUser)).toEqual(user);
  });

  it('currentUser stays null when /auth/me fails', async () => {
    global.fetch = vi.fn()
      .mockResolvedValueOnce({ ok: true, json: async () => ({ browserSSOEnabled: true }) })
      .mockResolvedValueOnce({ ok: false });

    await initAuth();

    expect(get(currentUser)).toBeNull();
  });

  it('currentUser stays null even when the /auth/me fetch fails', async () => {
    global.fetch = vi.fn()
      .mockResolvedValueOnce({ ok: true, json: async () => ({ browserSSOEnabled: true }) })
      .mockRejectedValueOnce(new Error('network error'));

    await initAuth();

    expect(get(currentUser)).toBeNull();
  });

  it('can set browserSSOEnabled and currentUser at the same time', async () => {
    const user = { email: 'sso@example.com' };
    global.fetch = vi.fn()
      .mockResolvedValueOnce({ ok: true, json: async () => ({ browserSSOEnabled: true }) })
      .mockResolvedValueOnce({ ok: true, json: async () => user });

    await initAuth();

    expect(get(browserSSOEnabled)).toBe(true);
    expect(get(currentUser)).toEqual(user);
  });
});

describe('initAuth — authReady', () => {
  it('authReady is false before starting', () => {
    expect(get(authReady)).toBe(false);
  });

  it('authReady becomes true after /auth/me succeeds and currentUser is set (ordering guarantee)', async () => {
    const user = { email: 'sso@example.com' };
    let resolveMe;
    const mePromise = new Promise((resolve) => { resolveMe = resolve; });
    global.fetch = vi.fn()
      .mockResolvedValueOnce({ ok: true, json: async () => ({ browserSSOEnabled: true }) })
      .mockReturnValueOnce(mePromise);

    const p = initAuth();

    // authReady stays false until the session check completes
    await Promise.resolve();
    await Promise.resolve();
    expect(get(authReady)).toBe(false);
    expect(get(currentUser)).toBeNull();

    resolveMe({ ok: true, json: async () => user });
    await p;

    // authReady becomes true after currentUser is set
    expect(get(currentUser)).toEqual(user);
    expect(get(authReady)).toBe(true);
  });

  it('authReady becomes true even when /auth/me fails (unauthenticated)', async () => {
    global.fetch = vi.fn()
      .mockResolvedValueOnce({ ok: true, json: async () => ({ browserSSOEnabled: false }) })
      .mockResolvedValueOnce({ ok: false });

    await initAuth();

    expect(get(currentUser)).toBeNull();
    expect(get(authReady)).toBe(true);
  });

  it('authReady becomes true even on a network error (the router is never permanently blocked)', async () => {
    global.fetch = vi.fn().mockRejectedValue(new Error('network error'));

    await initAuth();

    expect(get(authReady)).toBe(true);
  });
});

describe('jobPath', () => {
  it('encodes segments but keeps slashes', () => {
    expect(jobPath('team-a/build')).toBe('team-a/build');
    expect(jobPath('hello')).toBe('hello');
    expect(jobPath('a b/c')).toBe('a%20b/c');
  });
});
