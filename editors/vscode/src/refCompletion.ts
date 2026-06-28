import { ResourceIndex } from './resourceIndex';

export type RefTargetKind = 'Job' | 'GitCredential';

const JOB_FIELD = /^\s*job\s*:/;
const GIT_CRED_FIELD = /^\s*gitCredentialRef\s*:/;

export function detectRefTarget(lineText: string): RefTargetKind | undefined {
  if (GIT_CRED_FIELD.test(lineText)) {
    return 'GitCredential';
  }
  if (JOB_FIELD.test(lineText)) {
    return 'Job';
  }
  return undefined;
}

export function completionsForLine(index: ResourceIndex, lineText: string): string[] {
  const target = detectRefTarget(lineText);
  if (!target) {
    return [];
  }
  return index.namesByKind(target);
}
