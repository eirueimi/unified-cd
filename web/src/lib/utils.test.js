import { describe, it, expect } from 'vitest';
import { matchesFilter, buildJobTree, flattenJobTree, collapseCarriageReturns } from './utils.js';

describe('matchesFilter', () => {
  it('matches on partial match', () => {
    expect(matchesFilter('hello-docker', 'docker')).toBe(true);
  });

  it('matches case-insensitively', () => {
    expect(matchesFilter('Hello-Docker', 'docker')).toBe(true);
    expect(matchesFilter('hello-docker', 'DOCKER')).toBe(true);
  });

  it('an empty query always matches', () => {
    expect(matchesFilter('hello-docker', '')).toBe(true);
  });

  it('returns false when there is no match', () => {
    expect(matchesFilter('hello-docker', 'xyz')).toBe(false);
  });

  it('a null/undefined query always matches', () => {
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

describe('collapseCarriageReturns', () => {
  it('keeps the final redraw of \r progress output (git clone style)', () => {
    expect(collapseCarriageReturns('Updating files:  90% (700/773)\rUpdating files: 100% (773/773)\rUpdating files: 100% (773/773), done.'))
      .toBe('Updating files: 100% (773/773), done.');
  });

  it('returns plain lines unchanged', () => {
    expect(collapseCarriageReturns('Cloning into \'src\'...')).toBe('Cloning into \'src\'...');
  });

  it('ignores trailing carriage returns', () => {
    expect(collapseCarriageReturns('50%\r100%\r')).toBe('100%');
  });

  it('passes through empty and null-ish values', () => {
    expect(collapseCarriageReturns('')).toBe('');
    expect(collapseCarriageReturns(undefined)).toBe(undefined);
  });
});
