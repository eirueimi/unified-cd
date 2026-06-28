import { writable } from 'svelte/store';

const STORAGE_KEY = 'theme';
const VALID = ['light', 'dark'];

export const themePref = writable('dark');

// OS の配色設定からテーマを決定する（初回表示用）。
function systemTheme() {
  if (typeof window === 'undefined' || !window.matchMedia) return 'dark';
  return window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}

function readStored() {
  try {
    const v = localStorage.getItem(STORAGE_KEY);
    return VALID.includes(v) ? v : null;
  } catch {
    return null;
  }
}

function savePref(pref) {
  try {
    localStorage.setItem(STORAGE_KEY, pref);
  } catch {
    // localStorage 不可（プライベートモード等）。永続化を諦めて続行。
  }
}

function apply(pref) {
  document.documentElement.setAttribute('data-theme', pref);
}

export function initTheme() {
  let pref = readStored();
  if (pref === null) {
    // 保存値がない初回のみ OS 設定から決定し、永続化する（以後は OS を参照しない）。
    pref = systemTheme();
    savePref(pref);
  }
  themePref.set(pref);
  apply(pref);
}

export function toggleTheme() {
  let next;
  themePref.update((p) => {
    next = p === 'dark' ? 'light' : 'dark';
    return next;
  });
  savePref(next);
  apply(next);
}
