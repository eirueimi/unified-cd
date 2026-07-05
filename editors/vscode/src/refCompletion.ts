import { ResourceIndex } from './resourceIndex';

export type RefTargetKind = 'Job';

const JOB_FIELD = /^\s*job\s*:/;

export function detectRefTarget(lineText: string): RefTargetKind | undefined {
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
