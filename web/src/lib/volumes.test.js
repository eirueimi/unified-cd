import { describe, it, expect } from 'vitest';
import { fmtBytes, parseQuantity, isK8sAgent, isOnline, volumeRows } from './volumes.js';

describe('fmtBytes', () => {
  it('formats binary units', () => {
    expect(fmtBytes(512)).toBe('512 B');
    expect(fmtBytes(1536)).toBe('1.5 KiB');
    expect(fmtBytes(200 * 2 ** 30)).toBe('200.0 GiB');
  });
  it('renders absent as a dash', () => {
    expect(fmtBytes(null)).toBe('—');
    expect(fmtBytes(undefined)).toBe('—');
  });
});

describe('parseQuantity', () => {
  it('parses binary, decimal and plain forms', () => {
    expect(parseQuantity('200Gi')).toBe(200 * 2 ** 30);
    expect(parseQuantity('1.5Ti')).toBe(1.5 * 2 ** 40);
    expect(parseQuantity('500G')).toBe(5e11);
    expect(parseQuantity('1000000')).toBe(1e6);
  });
  it('returns null when unparseable', () => {
    expect(parseQuantity('abc')).toBeNull();
    expect(parseQuantity(null)).toBeNull();
  });
});

describe('isK8sAgent / isOnline', () => {
  it('detects kubernetes agents by os suffix', () => {
    expect(isK8sAgent({ os: 'linux/k8s' })).toBe(true);
    expect(isK8sAgent({ os: 'linux' })).toBe(false);
  });
  it('uses the 90 second rule', () => {
    expect(isOnline(new Date(Date.now() - 10000).toISOString())).toBe(true);
    expect(isOnline(new Date(Date.now() - 120000).toISOString())).toBe(false);
  });
});

describe('volumeRows', () => {
  const decls = { data: { spec: { mode: 'shared', size: '200Gi' } } };
  const withUsed = {
    volumeStatus: [{ name: 'data', ready: true }],
    storage: { volumes: { data: { freeBytes: 1, totalBytes: 2, usedBytes: 250 * 2 ** 30 } } },
  };
  it('leaves readings null when storage is absent', () => {
    const rows = volumeRows({ volumeStatus: [{ name: 'data', ready: true }], storage: null }, decls);
    expect(rows).toHaveLength(1);
    expect(rows[0]).toMatchObject({
      name: 'data', mode: 'shared', size: '200Gi', ready: true,
      usedBytes: null, freeBytes: null, totalBytes: null, measuredAt: null, claimBlocked: null, overSize: false,
    });
  });
  it('flags over-size when used exceeds the declared size', () => {
    expect(volumeRows(withUsed, decls)[0].overSize).toBe(true);
  });
  it('does not flag when the size is unparseable', () => {
    expect(volumeRows(withUsed, { data: { spec: { mode: 'shared', size: 'bogus' } } })[0].overSize).toBe(false);
  });
  it('uses null mode and size without a declaration', () => {
    const r = volumeRows({ volumeStatus: [{ name: 'x', ready: false, reason: 'r' }] }, {})[0];
    expect(r.mode).toBeNull();
    expect(r.size).toBeNull();
    expect(r.reason).toBe('r');
  });
});
