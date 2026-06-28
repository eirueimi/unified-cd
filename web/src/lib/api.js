import { writable, get } from 'svelte/store';

export const token = writable(localStorage.getItem('ecd_token') || '');
export const serverURL = writable(localStorage.getItem('ecd_server') || window.location.origin);
export const browserSSOEnabled = writable(false);
export const currentUser = writable(null);

export function saveAuth(t, s) {
  localStorage.setItem('ecd_token', t);
  localStorage.setItem('ecd_server', s);
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
    const resp = await fetch(url + '/api/v1/auth/me', { credentials: 'include' });
    if (resp.ok) currentUser.set(await resp.json());
  } catch { }
}

export async function apiFetch(path, options = {}) {
  const url = get(serverURL);
  const t = get(token);
  const headers = { 'Content-Type': 'application/json', ...(options.headers || {}) };
  if (t) headers['Authorization'] = 'Bearer ' + t;
  const resp = await fetch(url + path, { ...options, credentials: 'include', headers });
  if (resp.status === 401) {
    if (get(browserSSOEnabled)) {
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
      window.location.href = url + '/api/v1/auth/oidc-login';
      throw new Error('sso-redirect');
    }
    throw new Error('Unauthorized — check your token');
  }
  if (!resp.ok) throw new Error(await resp.text());
  return resp.text();
}
