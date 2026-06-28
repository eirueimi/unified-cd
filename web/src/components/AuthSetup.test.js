import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/svelte';
import { browserSSOEnabled, currentUser, token, serverURL } from '../lib/api.js';
import AuthSetup from './AuthSetup.svelte';

beforeEach(() => {
  browserSSOEnabled.set(false);
  currentUser.set(null);
  token.set('');
  serverURL.set('http://localhost:8080');
});

describe('AuthSetup — SSO ボタン', () => {
  it('browserSSOEnabled が false のとき SSO ボタンが表示されない', () => {
    browserSSOEnabled.set(false);
    render(AuthSetup);

    expect(screen.queryByText(/SSOでログイン/)).not.toBeInTheDocument();
  });

  it('browserSSOEnabled が true のとき SSO ボタンが表示される', () => {
    browserSSOEnabled.set(true);
    render(AuthSetup);

    expect(screen.getByText('🔒 SSOでログイン')).toBeInTheDocument();
  });

  it('SSO ボタンをクリックすると /api/v1/auth/oidc-login へリダイレクトする', () => {
    browserSSOEnabled.set(true);
    const assignSpy = vi.spyOn(window, 'location', 'get').mockReturnValue({
      ...window.location,
      assign: vi.fn(),
      href: '',
    });
    // window.location.href への代入をキャプチャ
    let redirectTarget = '';
    Object.defineProperty(window, 'location', {
      value: { ...window.location, get href() { return redirectTarget; }, set href(v) { redirectTarget = v; } },
      writable: true,
      configurable: true,
    });

    render(AuthSetup);
    fireEvent.click(screen.getByText('🔒 SSOでログイン'));

    expect(redirectTarget).toBe('http://localhost:8080/api/v1/auth/oidc-login');
    assignSpy.mockRestore();
  });
});

describe('AuthSetup — ログイン済み表示', () => {
  it('currentUser が設定されているとき SSO ボタンが表示されない', () => {
    currentUser.set({ email: 'user@example.com' });
    browserSSOEnabled.set(true);
    render(AuthSetup);

    expect(screen.queryByText(/SSOでログイン/)).not.toBeInTheDocument();
  });

  it('currentUser が設定されているときメールアドレスが表示される', () => {
    currentUser.set({ email: 'user@example.com' });
    render(AuthSetup);

    expect(screen.getByText(/user@example\.com/)).toBeInTheDocument();
  });

  it('currentUser が設定されているときログアウトボタンが表示される', () => {
    currentUser.set({ email: 'user@example.com' });
    render(AuthSetup);

    expect(screen.getByText('ログアウト')).toBeInTheDocument();
  });
});

describe('AuthSetup — トークン入力フォーム', () => {
  it('未ログイン時はトークン入力フォームが表示される', () => {
    render(AuthSetup);

    expect(screen.getByPlaceholderText('Bearer token or PAT')).toBeInTheDocument();
    expect(screen.getByPlaceholderText('Server URL')).toBeInTheDocument();
    expect(screen.getByText('Save')).toBeInTheDocument();
  });

  it('browserSSOEnabled が true のとき SSO ボタンとトークン入力の両方が表示される', () => {
    browserSSOEnabled.set(true);
    render(AuthSetup);

    expect(screen.getByText('🔒 SSOでログイン')).toBeInTheDocument();
    expect(screen.getByPlaceholderText('Bearer token or PAT')).toBeInTheDocument();
  });
});
