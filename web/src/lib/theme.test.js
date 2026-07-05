import { describe, it, expect, beforeEach, vi } from 'vitest';
import { get } from 'svelte/store';
import { themePref, initTheme, toggleTheme } from './theme.js';

let currentDarkMode = true;

function mockMatchMedia(dark) {
  currentDarkMode = dark;
  window.matchMedia = vi.fn().mockImplementation((query) => ({
    get matches() {
      return currentDarkMode;
    },
    media: query,
    addEventListener: vi.fn(),
    removeEventListener: vi.fn(),
  }));
}

describe('theme', () => {
  beforeEach(() => {
    localStorage.clear();
    document.documentElement.removeAttribute('data-theme');
    mockMatchMedia(true);
    themePref.set('dark');
  });

  it('toggleTheme switches between dark and light', () => {
    themePref.set('dark');
    toggleTheme();
    expect(get(themePref)).toBe('light');
    toggleTheme();
    expect(get(themePref)).toBe('dark');
  });

  it('toggleTheme saves the preference to localStorage', () => {
    themePref.set('dark');
    toggleTheme();
    expect(localStorage.getItem('theme')).toBe('light');
  });

  it('initTheme reads the preference back from localStorage', () => {
    localStorage.setItem('theme', 'light');
    initTheme();
    expect(get(themePref)).toBe('light');
  });

  it('initTheme sets the data-theme attribute', () => {
    localStorage.setItem('theme', 'light');
    initTheme();
    expect(document.documentElement.getAttribute('data-theme')).toBe('light');
  });

  it('decides from the OS setting (dark) on first run when there is no stored value', () => {
    mockMatchMedia(true);
    initTheme();
    expect(get(themePref)).toBe('dark');
    expect(document.documentElement.getAttribute('data-theme')).toBe('dark');
  });

  it('decides from the OS setting (light) on first run when there is no stored value', () => {
    mockMatchMedia(false);
    initTheme();
    expect(get(themePref)).toBe('light');
    expect(document.documentElement.getAttribute('data-theme')).toBe('light');
  });

  it('persists the OS-derived value to localStorage on first run (the OS is not consulted again after this)', () => {
    mockMatchMedia(false);
    initTheme();
    expect(localStorage.getItem('theme')).toBe('light');
  });

  it('falls back to the OS setting when the localStorage value is invalid', () => {
    localStorage.setItem('theme', 'garbage');
    mockMatchMedia(true);
    initTheme();
    expect(get(themePref)).toBe('dark');
  });

  it('falls back to dark in environments without matchMedia support', () => {
    delete window.matchMedia;
    initTheme();
    expect(get(themePref)).toBe('dark');
  });
});
