import { describe, it, expect } from 'vitest';
import { matchesFilter, buildJobTree, flattenJobTree } from './utils.js';

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

const jobs = [
  { name: 'team-a/build', path: 'team-a', leaf: 'build' },
  { name: 'team-a/deploy', path: 'team-a', leaf: 'deploy' },
  { name: 'team-b/edge/test', path: 'team-b/edge', leaf: 'test' },
  { name: 'hello', path: '', leaf: 'hello' },
];

describe('job tree', () => {
  it('flattens folders and root jobs (all expanded)', () => {
    const rows = flattenJobTree(buildJobTree(jobs), new Set(), '');
    const shape = rows.map((r) => r.kind === 'folder' ? `D${r.depth}:${r.name}` : `J${r.depth}:${r.job.leaf}`);
    expect(shape).toEqual([
      'D0:team-a', 'J1:build', 'J1:deploy',
      'D0:team-b', 'D1:edge', 'J2:test',
      'J0:hello',
    ]);
  });

  it('hides collapsed folder children', () => {
    const rows = flattenJobTree(buildJobTree(jobs), new Set(['team-a']), '');
    expect(rows.some((r) => r.kind === 'job' && r.job.leaf === 'build')).toBe(false);
    expect(rows.some((r) => r.kind === 'folder' && r.name === 'team-a')).toBe(true);
  });

  it('filter keeps matches and their ancestor folders, ignoring collapse', () => {
    const rows = flattenJobTree(buildJobTree(jobs), new Set(['team-b', 'team-b/edge']), 'test');
    const shape = rows.map((r) => r.kind === 'folder' ? `D:${r.name}` : `J:${r.job.leaf}`);
    expect(shape).toEqual(['D:team-b', 'D:edge', 'J:test']);
  });
});
