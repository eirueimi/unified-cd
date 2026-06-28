import { copyFileSync, mkdirSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const src = join(here, '..', '..', '..', 'schemas', 'unified-cd.schema.json');
const destDir = join(here, '..', 'schema');
const dest = join(destDir, 'unified-cd.schema.json');

mkdirSync(destDir, { recursive: true });
copyFileSync(src, dest);
console.log(`copied schema -> ${dest}`);
