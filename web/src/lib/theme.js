import { writable } from 'svelte/store';

const STORAGE_KEY = 'theme';
const VALID = ['light', 'dark'];

export const themePref = writable('dark');

// Determine the theme from the OS color scheme setting (for the first render).
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
    // localStorage unavailable (private mode, etc.). Give up on persisting and continue.
  }
}

function apply(pref) {
  document.documentElement.setAttribute('data-theme', pref);
}

export function initTheme() {
  let pref = readStored();
  if (pref === null) {
    // Only on the first run with no stored value, decide from the OS setting and persist it (the OS is not consulted again after this).
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
