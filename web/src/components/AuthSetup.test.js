import { describe, it, expect, beforeEach, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/svelte';
import { browserSSOEnabled, currentUser, token, serverURL } from '../lib/api.js';
import AuthSetup from './AuthSetup.svelte';

// AuthSetup only ever renders the token-entry / SSO card — it shows nothing
// once a user is logged in (`{#if !$currentUser}` guards the whole markup).
// The logged-in header (email + "Log out") lives in App.svelte instead; see
// App.test.js for those assertions.

beforeEach(() => {
  browserSSOEnabled.set(false);
  currentUser.set(null);
  token.set('');
  serverURL.set('http://localhost:8080');
});

describe('AuthSetup — SSO button', () => {
  it('does not show the SSO button when browserSSOEnabled is false', () => {
    browserSSOEnabled.set(false);
    render(AuthSetup);

    expect(screen.queryByText(/SSO Login/)).not.toBeInTheDocument();
  });

  it('shows the SSO button when browserSSOEnabled is true', () => {
    browserSSOEnabled.set(true);
    render(AuthSetup);

    expect(screen.getByText('SSO Login')).toBeInTheDocument();
  });

  it('clicking the SSO button redirects to /api/v1/auth/oidc-login', () => {
    browserSSOEnabled.set(true);
    // Capture assignments to window.location.href.
    let redirectTarget = '';
    Object.defineProperty(window, 'location', {
      value: { ...window.location, get href() { return redirectTarget; }, set href(v) { redirectTarget = v; } },
      writable: true,
      configurable: true,
    });

    render(AuthSetup);
    fireEvent.click(screen.getByText('SSO Login'));

    expect(redirectTarget).toBe('http://localhost:8080/api/v1/auth/oidc-login');
  });
});

describe('AuthSetup — hidden once logged in', () => {
  it('renders nothing when currentUser is set, even if SSO is enabled', () => {
    currentUser.set({ email: 'user@example.com' });
    browserSSOEnabled.set(true);
    const { container } = render(AuthSetup);

    expect(screen.queryByText(/SSO Login/)).not.toBeInTheDocument();
    expect(container.textContent.trim()).toBe('');
  });
});

describe('AuthSetup — token entry form', () => {
  it('shows the token entry form when logged out', () => {
    render(AuthSetup);

    expect(screen.getByPlaceholderText('Bearer token or PAT')).toBeInTheDocument();
    expect(screen.getByPlaceholderText('Server URL')).toBeInTheDocument();
    expect(screen.getByText('Save')).toBeInTheDocument();
  });

  it('shows both the SSO button and the token entry form when browserSSOEnabled is true', () => {
    browserSSOEnabled.set(true);
    render(AuthSetup);

    expect(screen.getByText('SSO Login')).toBeInTheDocument();
    expect(screen.getByPlaceholderText('Bearer token or PAT')).toBeInTheDocument();
  });
});
