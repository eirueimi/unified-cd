import { writable, get } from 'svelte/store';

export const token = writable(localStorage.getItem('ecd_token') || '');
export const serverURL = writable(localStorage.getItem('ecd_server') || window.location.origin);
export const browserSSOEnabled = writable(false);
export const currentUser = writable(null);
// Resolves once the initial session check (initAuth) has completed, whether it
// found a session or not. The router waits on this before rendering the
// current route so a hard-navigated / deep-linked hash route (e.g. reloading
// on /ui/#/jobs with only an SSO cookie and no localStorage token) doesn't
// render against a still-unresolved auth state.
export const authReady = writable(false);

export function saveAuth(t, s) {
  localStorage.setItem('ecd_token', t);
  localStorage.setItem('ecd_server', s);
}

// Deep-link restoration across the SSO redirect (hash routing loses the
// intended route across the full-page navigation to the IdP and back).
// Only in-app hash routes ("#/...") are ever persisted/restored, so this
// can't be used as an open redirect.
const DEEP_LINK_KEY = 'ecd_post_login_hash';

export function savePostLoginHash() {
  const hash = window.location.hash;
  if (hash && hash.startsWith('#/')) {
    localStorage.setItem(DEEP_LINK_KEY, hash);
  }
}

export function restorePostLoginHash() {
  const hash = localStorage.getItem(DEEP_LINK_KEY);
  localStorage.removeItem(DEEP_LINK_KEY);
  if (hash && hash.startsWith('#/')) {
    window.location.hash = hash;
  }
}

export async function initAuth() {
  const url = get(serverURL);
  try {
    const resp = await fetch(url + '/api/v1/auth/oidc-config');
    if (resp.ok) {
      const cfg = await resp.json();
      browserSSOEnabled.set(cfg.browserSSOEnabled === true);
    }
  } catch { }
  try {
    const t = get(token);
    const headers = t ? { Authorization: 'Bearer ' + t } : {};
    const resp = await fetch(url + '/api/v1/auth/me', { headers, credentials: 'include' });
    if (resp.ok) currentUser.set(await resp.json());
  } catch { }
  authReady.set(true);
}

export async function apiFetch(path, options = {}) {
  const url = get(serverURL);
  const t = get(token);
  const headers = { 'Content-Type': 'application/json', ...(options.headers || {}) };
  if (t) headers['Authorization'] = 'Bearer ' + t;
  const resp = await fetch(url + path, { ...options, credentials: 'include', headers });
  if (resp.status === 401) {
    if (get(browserSSOEnabled)) {
      savePostLoginHash();
      window.location.href = url + '/api/v1/auth/oidc-login';
      throw new Error('sso-redirect');
    }
    throw new Error('Unauthorized — check your token');
  }
  if (!resp.ok) throw new Error(await resp.text());
  if (resp.status === 204) return null;
  return resp.json();
}

export async function apiFetchText(path, options = {}) {
  const url = get(serverURL);
  const t = get(token);
  const headers = { ...(options.headers || {}) };
  if (t) headers['Authorization'] = 'Bearer ' + t;
  const resp = await fetch(url + path, { ...options, credentials: 'include', headers });
  if (resp.status === 401) {
    if (get(browserSSOEnabled)) {
      savePostLoginHash();
      window.location.href = url + '/api/v1/auth/oidc-login';
      throw new Error('sso-redirect');
    }
    throw new Error('Unauthorized — check your token');
  }
  if (!resp.ok) throw new Error(await resp.text());
  return resp.text();
}

// jobPath encodes a qualified job name for use as a URL path under
// /api/v1/jobs/ — each segment is percent-encoded but the slashes are kept
// literal so the controller's catch-all route captures the full name.
export function jobPath(name) {
  return String(name).split('/').map(encodeURIComponent).join('/');
}
