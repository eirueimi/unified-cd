import { describe, it, expect } from 'vitest';
import { isUnifiedCdDocument, SCHEME_ID, SCHEMA_URI } from '../src/schemaContributor';

describe('isUnifiedCdDocument', () => {
  it('matches a unified-cd document', () => {
    const text = 'apiVersion: unified-cd/v1\nkind: Job\n';
    expect(isUnifiedCdDocument(text)).toBe(true);
  });

  it('matches when apiVersion is not the first line', () => {
    const text = '# comment\n---\napiVersion: unified-cd/v1\nkind: Schedule\n';
    expect(isUnifiedCdDocument(text)).toBe(true);
  });

  it('does not match a different apiVersion', () => {
    const text = 'apiVersion: apps/v1\nkind: Deployment\n';
    expect(isUnifiedCdDocument(text)).toBe(false);
  });

  it('does not match empty text', () => {
    expect(isUnifiedCdDocument('')).toBe(false);
  });

  it('exposes scheme id and schema uri constants', () => {
    expect(SCHEME_ID).toBe('unified-cd');
    expect(SCHEMA_URI).toBe('unified-cd://schema/unified-cd.schema.json');
  });
});
