export const SCHEME_ID = 'unified-cd';
export const SCHEMA_URI = 'unified-cd://schema/unified-cd.schema.json';

const UNIFIED_CD_API_VERSION = /^\s*apiVersion\s*:\s*unified-cd\/v1\s*$/m;

export function isUnifiedCdDocument(text: string): boolean {
  return UNIFIED_CD_API_VERSION.test(text);
}
