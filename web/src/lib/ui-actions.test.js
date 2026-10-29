import { describe, it, expect } from 'vitest';
import { readdirSync, readFileSync, statSync } from 'node:fs';
import { join, relative, sep } from 'node:path';
import actions from './ui-actions.json';

const srcDir = join(__dirname, '..');
const STATE_CHANGING = /^(POST|PUT|DELETE|PATCH)$/i;

// findMethodCalls returns every `method: ...` / `method = ...` in source that
// can send a state-changing request: a literal POST/PUT/DELETE/PATCH (any case,
// any quote style) or a value that is not a plain literal (a variable, an
// expression, a template literal with substitutions), which cannot be proven
// safe and so must be listed in the inventory too. GET/HEAD literals are ignored.
export function findMethodCalls(source) {
  const found = [];
  const re = /\bmethod\s*[:=]\s*(?:(["'`])([^"'`]*)\1|([^,}\n;)]+))/gi;
  let m;
  while ((m = re.exec(source)) !== null) {
    const literal = m[2];
    if (literal !== undefined && !literal.includes('${')) {
      if (STATE_CHANGING.test(literal.trim())) found.push({ method: literal.trim().toUpperCase() });
    } else {
      found.push({ method: null, expression: (literal ?? m[3]).trim() });
    }
  }
  return found;
}

function sourceFiles(dir) {
  const out = [];
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) out.push(...sourceFiles(p));
    else if ((name.endsWith('.svelte') || name.endsWith('.js')) && !name.endsWith('.test.js')) out.push(p);
  }
  return out;
}

describe('findMethodCalls detector', () => {
  it('detects literals in any case and quote style', () => {
    expect(findMethodCalls(`f(u, { method: 'post' })`)).toEqual([{ method: 'POST' }]);
    expect(findMethodCalls('f(u, { method: "DELETE" })')).toEqual([{ method: 'DELETE' }]);
    expect(findMethodCalls('f(u, { method: `PUT` })')).toEqual([{ method: 'PUT' }]);
    expect(findMethodCalls(`f(u, { Method: "Patch" })`)).toEqual([{ method: 'PATCH' }]);
  });

  it('detects a template-literal method and a variable method as non-literal', () => {
    expect(findMethodCalls('f(u, { method: `${verb}` })')).toEqual([{ method: null, expression: '${verb}' }]);
    expect(findMethodCalls('f(u, { method: verb })')).toEqual([{ method: null, expression: 'verb' }]);
    expect(findMethodCalls('opts.method = pick()')).toHaveLength(1);
  });

  it('ignores GET', () => {
    expect(findMethodCalls(`f(u, { method: 'GET' })`)).toEqual([]);
  });
});

describe('ui-actions.json inventory', () => {
  const listed = {};
  for (const a of actions) listed[a.file] = (listed[a.file] || 0) + 1;

  for (const path of sourceFiles(srcDir)) {
    const file = relative(srcDir, path).split(sep).join('/');
    const calls = findMethodCalls(readFileSync(path, 'utf8'));
    if (calls.length === 0 && !listed[file]) continue;
    it(`${file} lists every state-changing call`, () => {
      if (calls.length > 0 && !listed[file]) {
        throw new Error(`${file} has an unlisted state-changing call: add it to web/src/lib/ui-actions.json`);
      }
      expect(listed[file] || 0, `${file}: ${calls.length} call(s) in source`).toBe(calls.length);
    });
  }

  it('lists only files that exist', () => {
    const files = new Set(sourceFiles(srcDir).map((p) => relative(srcDir, p).split(sep).join('/')));
    for (const a of actions) expect(files.has(a.file), a.file).toBe(true);
  });
});
