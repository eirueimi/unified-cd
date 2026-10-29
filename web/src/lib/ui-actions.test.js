import { describe, it, expect } from 'vitest';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative, sep } from 'node:path';
import actions from './ui-actions.json';

const srcDir = join(__dirname, '..');

function svelteFiles(dir) {
  const out = [];
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) out.push(...svelteFiles(p));
    else if (name.endsWith('.svelte')) out.push(p);
  }
  return out;
}

describe('ui-actions.json inventory', () => {
  const listed = {};
  for (const a of actions) listed[a.file] = (listed[a.file] || 0) + 1;

  for (const path of svelteFiles(srcDir)) {
    const file = relative(srcDir, path).split(sep).join('/');
    const calls = (readFileSync(path, 'utf8').match(/method:\s*["'](POST|PUT|DELETE|PATCH)["']/g) || []).length;
    if (calls === 0 && !listed[file]) continue;
    it(`${file} lists every state-changing call`, () => {
      if (calls > 0 && !listed[file]) {
        throw new Error(`${file} has an unlisted state-changing call: add it to web/src/lib/ui-actions.json`);
      }
      expect(listed[file] || 0, `${file}: ${calls} call(s) in source`).toBe(calls);
    });
  }

  it('lists only files that exist', () => {
    const files = new Set(svelteFiles(srcDir).map((p) => relative(srcDir, p).split(sep).join('/')));
    for (const a of actions) expect(files.has(a.file), a.file).toBe(true);
  });
});
