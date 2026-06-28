import { describe, it, expect, beforeEach, vi } from 'vitest';
import { get } from 'svelte/store';
import { browserSSOEnabled, currentUser, serverURL, initAuth } from './api.js';

beforeEach(() => {
  browserSSOEnabled.set(false);
  currentUser.set(null);
  serverURL.set('http://localhost');
  vi.resetAllMocks();
});

describe('initAuth — browserSSOEnabled', () => {
  it('サーバーが browserSSOEnabled:true を返したとき store が true になる', async () => {
    global.fetch = vi.fn()
      .mockResolvedValueOnce({ ok: true, json: async () => ({ browserSSOEnabled: true, clientId: 'test', issuer: 'http://localhost' }) })
      .mockResolvedValueOnce({ ok: false });

    await initAuth();

    expect(get(browserSSOEnabled)).toBe(true);
  });

  it('サーバーが browserSSOEnabled:false を返したとき store が false のまま', async () => {
    global.fetch = vi.fn()
      .mockResolvedValueOnce({ ok: true, json: async () => ({ browserSSOEnabled: false }) })
      .mockResolvedValueOnce({ ok: false });

    await initAuth();

    expect(get(browserSSOEnabled)).toBe(false);
  });

  it('fetch が失敗したとき store が false のまま', async () => {
    global.fetch = vi.fn().mockRejectedValue(new Error('network error'));

    await initAuth();

    expect(get(browserSSOEnabled)).toBe(false);
  });

  it('oidc-config レスポンスが ok でないとき store が false のまま', async () => {
    global.fetch = vi.fn()
      .mockResolvedValueOnce({ ok: false })
      .mockResolvedValueOnce({ ok: false });

    await initAuth();

    expect(get(browserSSOEnabled)).toBe(false);
  });
});

describe('initAuth — currentUser', () => {
  it('/auth/me が成功したとき currentUser が設定される', async () => {
    const user = { email: 'test@example.com', name: 'Test User' };
    global.fetch = vi.fn()
      .mockResolvedValueOnce({ ok: true, json: async () => ({ browserSSOEnabled: true }) })
      .mockResolvedValueOnce({ ok: true, json: async () => user });

    await initAuth();

    expect(get(currentUser)).toEqual(user);
  });

  it('/auth/me が失敗したとき currentUser が null のまま', async () => {
    global.fetch = vi.fn()
      .mockResolvedValueOnce({ ok: true, json: async () => ({ browserSSOEnabled: true }) })
      .mockResolvedValueOnce({ ok: false });

    await initAuth();

    expect(get(currentUser)).toBeNull();
  });

  it('/auth/me の fetch が失敗しても currentUser が null のまま', async () => {
    global.fetch = vi.fn()
      .mockResolvedValueOnce({ ok: true, json: async () => ({ browserSSOEnabled: true }) })
      .mockRejectedValueOnce(new Error('network error'));

    await initAuth();

    expect(get(currentUser)).toBeNull();
  });

  it('browserSSOEnabled と currentUser を同時に設定できる', async () => {
    const user = { email: 'sso@example.com' };
    global.fetch = vi.fn()
      .mockResolvedValueOnce({ ok: true, json: async () => ({ browserSSOEnabled: true }) })
      .mockResolvedValueOnce({ ok: true, json: async () => user });

    await initAuth();

    expect(get(browserSSOEnabled)).toBe(true);
    expect(get(currentUser)).toEqual(user);
  });
});
