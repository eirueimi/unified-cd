import { describe, it, expect } from 'vitest';
import { detectRefTarget, completionsForLine } from '../src/refCompletion';
import { ResourceIndex } from '../src/resourceIndex';

describe('detectRefTarget', () => {
  it('detects a Schedule job field', () => {
    expect(detectRefTarget('  job: ')).toBe('Job');
  });

  it('detects a nested trigger.job field (key on its own line)', () => {
    expect(detectRefTarget('    job: my')).toBe('Job');
  });

  it('detects a gitCredentialRef field', () => {
    expect(detectRefTarget('  gitCredentialRef: ')).toBe('GitCredential');
  });

  it('returns undefined for unrelated keys', () => {
    expect(detectRefTarget('  name: hello')).toBeUndefined();
    expect(detectRefTarget('  cron: "* * * * *"')).toBeUndefined();
    expect(detectRefTarget('')).toBeUndefined();
  });
});

describe('completionsForLine', () => {
  it('returns Job names on a job line', () => {
    const index = new ResourceIndex();
    index.update('u1', 'kind: Job\nmetadata:\n  name: build\n');
    index.update('u2', 'kind: GitCredential\nmetadata:\n  name: gh\n');
    expect(completionsForLine(index, '  job: ')).toEqual(['build']);
  });

  it('returns GitCredential names on a gitCredentialRef line', () => {
    const index = new ResourceIndex();
    index.update('u2', 'kind: GitCredential\nmetadata:\n  name: gh\n');
    expect(completionsForLine(index, '  gitCredentialRef: ')).toEqual(['gh']);
  });

  it('returns empty for unrelated lines', () => {
    const index = new ResourceIndex();
    index.update('u1', 'kind: Job\nmetadata:\n  name: build\n');
    expect(completionsForLine(index, '  name: ')).toEqual([]);
  });
});
