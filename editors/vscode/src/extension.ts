import * as fs from 'node:fs';
import * as path from 'node:path';
import * as vscode from 'vscode';

import { ResourceIndex } from './resourceIndex';
import { completionsForLine } from './refCompletion';
import { SCHEME_ID, SCHEMA_URI, isUnifiedCdDocument } from './schemaContributor';

const YAML_GLOB = '**/*.{yaml,yml}';

export async function activate(context: vscode.ExtensionContext): Promise<void> {
  const index = new ResourceIndex();

  await registerSchemaContributor(context);
  registerCompletionProvider(context, index);
  await initResourceIndex(context, index);
}

export function deactivate(): void {
  // no-op
}

async function registerSchemaContributor(context: vscode.ExtensionContext): Promise<void> {
  const yamlExt = vscode.extensions.getExtension('redhat.vscode-yaml');
  if (!yamlExt) {
    vscode.window.showWarningMessage(
      'unified-cd YAML: redhat.vscode-yaml が見つかりません。スキーマ補完は無効です。',
    );
    return;
  }

  const yamlApi = await yamlExt.activate();
  if (!yamlApi || typeof yamlApi.registerContributor !== 'function') {
    vscode.window.showWarningMessage(
      'unified-cd YAML: vscode-yaml の schema API を利用できません。スキーマ補完は無効です。',
    );
    return;
  }

  const schemaPath = path.join(context.extensionPath, 'schema', 'unified-cd.schema.json');

  const requestSchema = (resource: string): string | undefined => {
    const doc = vscode.workspace.textDocuments.find((d) => d.uri.toString() === resource);
    if (doc && isUnifiedCdDocument(doc.getText())) {
      return SCHEMA_URI;
    }
    return undefined;
  };

  const requestSchemaContent = (uri: string): string | undefined => {
    if (uri !== SCHEMA_URI) {
      return undefined;
    }
    return fs.readFileSync(schemaPath, 'utf8');
  };

  yamlApi.registerContributor(SCHEME_ID, requestSchema, requestSchemaContent);
}

function registerCompletionProvider(
  context: vscode.ExtensionContext,
  index: ResourceIndex,
): void {
  const provider: vscode.CompletionItemProvider = {
    provideCompletionItems(document, position) {
      const lineText = document.lineAt(position).text;
      return completionsForLine(index, lineText).map(
        (name) => new vscode.CompletionItem(name, vscode.CompletionItemKind.Reference),
      );
    },
  };

  context.subscriptions.push(
    vscode.languages.registerCompletionItemProvider({ language: 'yaml' }, provider, ' ', ':'),
  );
}

async function initResourceIndex(
  context: vscode.ExtensionContext,
  index: ResourceIndex,
): Promise<void> {
  const readAndUpdate = async (uri: vscode.Uri): Promise<void> => {
    try {
      const bytes = await vscode.workspace.fs.readFile(uri);
      index.update(uri.toString(), Buffer.from(bytes).toString('utf8'));
    } catch {
      index.remove(uri.toString());
    }
  };

  const uris = await vscode.workspace.findFiles(YAML_GLOB, '**/node_modules/**');
  await Promise.all(uris.map(readAndUpdate));

  const watcher = vscode.workspace.createFileSystemWatcher(YAML_GLOB);
  watcher.onDidCreate(readAndUpdate);
  watcher.onDidChange(readAndUpdate);
  watcher.onDidDelete((uri) => index.remove(uri.toString()));
  context.subscriptions.push(watcher);
}
