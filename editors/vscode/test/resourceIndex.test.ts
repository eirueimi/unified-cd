import { describe, it, expect } from 'vitest';
import { parseResources, namesByKind, ResourceIndex } from '../src/resourceIndex';

describe('parseResources', () => {
  it('extracts kind and metadata.name from a single document', () => {
    const text = 'apiVersion: unified-cd/v1\nkind: Job\nmetadata:\n  name: hello\n';
    expect(parseResources(text, 'file:///a.yaml')).toEqual([
      { kind: 'Job', name: 'hello', uri: 'file:///a.yaml' },
    ]);
  });

  it('handles multi-document YAML', () => {
    const text =
      'kind: Job\nmetadata:\n  name: a\n---\nkind: GitCredential\nmetadata:\n  name: b\n';
    expect(parseResources(text, 'file:///m.yaml')).toEqual([
      { kind: 'Job', name: 'a', uri: 'file:///m.yaml' },
      { kind: 'GitCredential', name: 'b', uri: 'file:///m.yaml' },
    ]);
  });

  it('skips documents missing kind or name', () => {
    const text = 'kind: Job\n---\nmetadata:\n  name: noKind\n';
    expect(parseResources(text, 'file:///p.yaml')).toEqual([]);
  });

  it('skips broken YAML without throwing', () => {
    const text = 'kind: Job\nmetadata:\n  name: ok\n---\n: : : not valid : :\n';
    expect(parseResources(text, 'file:///b.yaml')).toEqual([
      { kind: 'Job', name: 'ok', uri: 'file:///b.yaml' },
    ]);
  });
});

describe('namesByKind', () => {
  it('returns deduplicated names of the given kind', () => {
    const refs = [
      { kind: 'Job', name: 'a', uri: 'u1' },
      { kind: 'Job', name: 'a', uri: 'u2' },
      { kind: 'Job', name: 'b', uri: 'u3' },
      { kind: 'GitCredential', name: 'c', uri: 'u4' },
    ];
    expect(namesByKind(refs, 'Job')).toEqual(['a', 'b']);
    expect(namesByKind(refs, 'GitCredential')).toEqual(['c']);
  });
});

describe('ResourceIndex', () => {
  it('aggregates across files and reflects updates and removals', () => {
    const index = new ResourceIndex();
    index.update('file:///jobs.yaml', 'kind: Job\nmetadata:\n  name: j1\n');
    index.update('file:///creds.yaml', 'kind: GitCredential\nmetadata:\n  name: g1\n');
    expect(index.namesByKind('Job')).toEqual(['j1']);
    expect(index.namesByKind('GitCredential')).toEqual(['g1']);

    index.update('file:///jobs.yaml', 'kind: Job\nmetadata:\n  name: j2\n');
    expect(index.namesByKind('Job')).toEqual(['j2']);

    index.remove('file:///jobs.yaml');
    expect(index.namesByKind('Job')).toEqual([]);
  });
});
