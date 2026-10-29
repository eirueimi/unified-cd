import { describe, it, expect, beforeEach, vi } from 'vitest';
import { get } from 'svelte/store';
import { browserSSOEnabled, currentUser, serverURL, authReady, initAuth, jobPath, apiFetch, apiFetchText, ApiError, grants, refreshMe } from './api.js';

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

describe('ApiError and 403 refresh', () => {
  const resp = (status, body = '') => ({ ok: status < 400, status, text: async () => body, json: async () => JSON.parse(body || 'null') });
  const meCalls = () => global.fetch.mock.calls.filter((c) => String(c[0]).endsWith('/api/v1/auth/me'));

  it('apiFetch throws ApiError with status on 404', async () => {
    global.fetch = vi.fn().mockResolvedValue(resp(404, 'not found'));
    const e = await apiFetch('/api/v1/jobs/x').catch((err) => err);
    expect(e).toBeInstanceOf(ApiError);
    expect(e.status).toBe(404);
    expect(e.message).toBe('not found');
    expect(meCalls()).toHaveLength(0);
  });

  it('apiFetchText throws ApiError with status', async () => {
    global.fetch = vi.fn().mockResolvedValue(resp(500, 'boom'));
    const e = await apiFetchText('/x').catch((err) => err);
    expect(e).toBeInstanceOf(ApiError);
    expect(e.status).toBe(500);
  });

  it('apiFetch: a 403 refetches /auth/me once and updates grants', async () => {
    const me = { name: 'x', permissions: { global: ['job.read'], folders: {} } };
    global.fetch = vi.fn(async (u) => (String(u).endsWith('/auth/me') ? resp(200, JSON.stringify(me)) : resp(403, 'forbidden')));
    const e = await apiFetch('/api/v1/x', { method: 'POST' }).catch((err) => err);
    expect(e).toBeInstanceOf(ApiError);
    expect(e.status).toBe(403);
    expect(meCalls()).toHaveLength(1);
    expect(get(grants)).toEqual(me.permissions);
  });

  it('apiFetch: concurrent 403s trigger a single /auth/me refetch', async () => {
    global.fetch = vi.fn(async (u) => (String(u).endsWith('/auth/me') ? resp(200, '{"permissions":null}') : resp(403, 'forbidden')));
    const rs = await Promise.allSettled([apiFetch('/a'), apiFetch('/b'), apiFetch('/c')]);
    expect(rs.every((r) => r.status === 'rejected' && r.reason.status === 403)).toBe(true);
    expect(meCalls()).toHaveLength(1);
  });

  it('apiFetch: a failing /auth/me refetch does not loop', async () => {
    global.fetch = vi.fn(async (u) => resp(403, String(u).endsWith('/auth/me') ? 'me forbidden' : 'forbidden'));
    const e = await apiFetch('/a').catch((err) => err);
    expect(e).toBeInstanceOf(ApiError);
    expect(e.message).toBe('forbidden');
    expect(meCalls()).toHaveLength(1);
  });

  it('refreshMe: concurrent callers share one request', async () => {
    global.fetch = vi.fn(async () => resp(200, '{"permissions":{"global":[],"folders":{}}}'));
    await Promise.all([refreshMe(), refreshMe()]);
    expect(meCalls()).toHaveLength(1);
  });
});
