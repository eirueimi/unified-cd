import { parseAllDocuments } from 'yaml';

export interface ResourceRef {
  kind: string;
  name: string;
  uri: string;
}

export function parseResources(text: string, uri: string): ResourceRef[] {
  const refs: ResourceRef[] = [];
  let docs;
  try {
    docs = parseAllDocuments(text);
  } catch {
    return refs;
  }
  for (const doc of docs) {
    let json: unknown;
    try {
      json = doc.toJSON();
    } catch {
      continue;
    }
    if (!json || typeof json !== 'object') {
      continue;
    }
    const obj = json as Record<string, unknown>;
    const kind = obj.kind;
    const metadata = obj.metadata as Record<string, unknown> | undefined;
    const name = metadata?.name;
    if (typeof kind === 'string' && typeof name === 'string') {
      refs.push({ kind, name, uri });
    }
  }
  return refs;
}

export function namesByKind(refs: ResourceRef[], kind: string): string[] {
  const names: string[] = [];
  for (const ref of refs) {
    if (ref.kind === kind && !names.includes(ref.name)) {
      names.push(ref.name);
    }
  }
  return names;
}

export class ResourceIndex {
  private byUri = new Map<string, ResourceRef[]>();

  update(uri: string, text: string): void {
    this.byUri.set(uri, parseResources(text, uri));
  }

  remove(uri: string): void {
    this.byUri.delete(uri);
  }

  all(): ResourceRef[] {
    return [...this.byUri.values()].flat();
  }

  namesByKind(kind: string): string[] {
    return namesByKind(this.all(), kind);
  }
}
