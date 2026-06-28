import { describe, it, expect } from 'vitest';
import { matchesFilter } from './utils.js';

describe('matchesFilter', () => {
  it('部分一致でマッチする', () => {
    expect(matchesFilter('hello-docker', 'docker')).toBe(true);
  });

  it('大小文字を無視してマッチする', () => {
    expect(matchesFilter('Hello-Docker', 'docker')).toBe(true);
    expect(matchesFilter('hello-docker', 'DOCKER')).toBe(true);
  });

  it('空文字クエリは常にマッチする', () => {
    expect(matchesFilter('hello-docker', '')).toBe(true);
  });

  it('マッチしない場合は false を返す', () => {
    expect(matchesFilter('hello-docker', 'xyz')).toBe(false);
  });

  it('null/undefined クエリは常にマッチする', () => {
    expect(matchesFilter('hello-docker', null)).toBe(true);
    expect(matchesFilter('hello-docker', undefined)).toBe(true);
  });
});
