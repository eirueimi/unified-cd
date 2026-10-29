import { writable, get, derived } from 'svelte/store';

export const token = writable(localStorage.getItem('ecd_token') || '');
export const serverURL = writable(localStorage.getItem('ecd_server') || window.location.origin);
export const browserSSOEnabled = writable(false);
export const currentUser = writable(null);
// The caller's grants (the `permissions` object of /auth/me), or null while not
// yet known. Feed it to allows()/can() from authz.js.
export const grants = derived(currentUser, (u) => (u ? u.permissions ?? null : null));

// ApiError is thrown for non-OK API responses; message is the response body.
export class ApiError extends Error {
  constructor(message, status) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}
// Server-set display preference (from /api/v1/ui-config): when true, the run log
// renders step stderr the same color as stdout instead of red. Default: red.
export const stderrPlain = writable(false);
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

let meInflight = null;

// refreshMe re-reads /auth/me into currentUser. Concurrent callers share one
// in-flight request. It uses raw fetch (not apiFetch) so a 403 here cannot
// trigger another refresh, and it never rejects.
export function refreshMe() {
  if (meInflight) return meInflight;
  meInflight = (async () => {
    try {
      const t = get(token);
      const headers = t ? { Authorization: 'Bearer ' + t } : {};
      const resp = await fetch(get(serverURL) + '/api/v1/auth/me', { headers, credentials: 'include' });
      if (resp.ok) currentUser.set(await resp.json());
    } catch { }
  })().finally(() => { meInflight = null; });
  return meInflight;
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
  await refreshMe();
  try {
    const resp = await fetch(url + '/api/v1/ui-config');
    if (resp.ok) {
      const cfg = await resp.json();
      stderrPlain.set(cfg.stderrPlain === true);
    }
  } catch { }
  authReady.set(true);
}

// failResponse throws an ApiError for a non-OK response. On a 403 it first
// re-reads /auth/me once (shared promise) so the UI picks up changed grants.
async function failResponse(resp) {
  const body = await resp.text();
  if (resp.status === 403) await refreshMe();
  throw new ApiError(body, resp.status);
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
    throw new ApiError('Unauthorized — check your token', 401);
  }
  if (!resp.ok) await failResponse(resp);
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
    throw new ApiError('Unauthorized — check your token', 401);
  }
  if (!resp.ok) await failResponse(resp);
  return resp.text();
}

// jobPath encodes a qualified job name for use as a URL path under
// /api/v1/jobs/ — each segment is percent-encoded but the slashes are kept
// literal so the controller's catch-all route captures the full name.
export function jobPath(name) {
  return String(name).split('/').map(encodeURIComponent).join('/');
}
