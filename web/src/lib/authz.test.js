import { describe, it, expect } from 'vitest';
import cases from './authz-cases.json';
import { can, canAnywhere, allows, requiresText } from './authz.js';

describe('can() fixture (shared with the Go test)', () => {
  it.each(cases)('$name', (c) => {
    expect(can(c.grants, c.permission, c.job)).toBe(c.want);
  });
});

describe('canAnywhere', () => {
  it('sees folder-only grants', () => {
    const g = { global: [], folders: { 'team-a': ['run.trigger'] } };
    expect(canAnywhere(g, 'run.trigger')).toBe(true);
    expect(canAnywhere(g, 'secret.write')).toBe(false);
  });
});

it('allows() is permissive only while grants are unknown', () => {
  expect(allows(null, 'secret.write')).toBe(true);
  expect(allows(undefined, 'secret.write', 'a/b')).toBe(true);
  expect(allows({ global: [], folders: {} }, 'secret.write')).toBe(false);
  const g = { global: [], folders: { 'team-a': ['run.trigger'] } };
  expect(allows(g, 'run.trigger')).toBe(true);
  expect(allows(g, 'run.trigger', 'team-a/x')).toBe(true);
  expect(allows(g, 'run.trigger', 'team-b/x')).toBe(false);
});

it('requiresText', () => {
  expect(requiresText('run.approve', 'team-a/prod/deploy')).toBe('requires run.approve on team-a/prod/deploy');
  expect(requiresText('secret.write')).toBe('requires secret.write');
});
