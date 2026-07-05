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

describe('initAuth — authReady', () => {
  it('開始前は authReady が false', () => {
    expect(get(authReady)).toBe(false);
  });

  it('/auth/me が成功して currentUser が設定された後に authReady が true になる (順序保証)', async () => {
    const user = { email: 'sso@example.com' };
    let resolveMe;
    const mePromise = new Promise((resolve) => { resolveMe = resolve; });
    global.fetch = vi.fn()
      .mockResolvedValueOnce({ ok: true, json: async () => ({ browserSSOEnabled: true }) })
      .mockReturnValueOnce(mePromise);

    const p = initAuth();

    // セッション確認が完了するまで authReady は false のまま
    await Promise.resolve();
    await Promise.resolve();
    expect(get(authReady)).toBe(false);
    expect(get(currentUser)).toBeNull();

    resolveMe({ ok: true, json: async () => user });
    await p;

    // currentUser が設定された後で authReady が true になる
    expect(get(currentUser)).toEqual(user);
    expect(get(authReady)).toBe(true);
  });

  it('/auth/me が失敗しても(unauthenticated)authReady は true になる', async () => {
    global.fetch = vi.fn()
      .mockResolvedValueOnce({ ok: true, json: async () => ({ browserSSOEnabled: false }) })
      .mockResolvedValueOnce({ ok: false });

    await initAuth();

    expect(get(currentUser)).toBeNull();
    expect(get(authReady)).toBe(true);
  });

  it('ネットワークエラーでも authReady は true になる(ルーターが永久にブロックされない)', async () => {
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
