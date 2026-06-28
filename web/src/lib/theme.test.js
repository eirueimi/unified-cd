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

  it('toggleTheme は dark ↔ light を切り替える', () => {
    themePref.set('dark');
    toggleTheme();
    expect(get(themePref)).toBe('light');
    toggleTheme();
    expect(get(themePref)).toBe('dark');
  });

  it('toggleTheme は preference を localStorage に保存する', () => {
    themePref.set('dark');
    toggleTheme();
    expect(localStorage.getItem('theme')).toBe('light');
  });

  it('initTheme は localStorage の preference を読み戻す', () => {
    localStorage.setItem('theme', 'light');
    initTheme();
    expect(get(themePref)).toBe('light');
  });

  it('initTheme は data-theme 属性を設定する', () => {
    localStorage.setItem('theme', 'light');
    initTheme();
    expect(document.documentElement.getAttribute('data-theme')).toBe('light');
  });

  it('保存値がないとき初回は OS 設定（dark）から決定する', () => {
    mockMatchMedia(true);
    initTheme();
    expect(get(themePref)).toBe('dark');
    expect(document.documentElement.getAttribute('data-theme')).toBe('dark');
  });

  it('保存値がないとき初回は OS 設定（light）から決定する', () => {
    mockMatchMedia(false);
    initTheme();
    expect(get(themePref)).toBe('light');
    expect(document.documentElement.getAttribute('data-theme')).toBe('light');
  });

  it('初回に OS から決定した値を localStorage に永続化する（以後は OS を参照しない）', () => {
    mockMatchMedia(false);
    initTheme();
    expect(localStorage.getItem('theme')).toBe('light');
  });

  it('不正な localStorage 値は OS 設定から決定する', () => {
    localStorage.setItem('theme', 'garbage');
    mockMatchMedia(true);
    initTheme();
    expect(get(themePref)).toBe('dark');
  });

  it('matchMedia 非対応環境では dark にフォールバックする', () => {
    delete window.matchMedia;
    initTheme();
    expect(get(themePref)).toBe('dark');
  });
});
