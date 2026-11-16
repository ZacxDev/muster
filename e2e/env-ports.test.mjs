// Every e2e port in e2e/lib/env.ts is its own: each exported `port + N`
// (including a `?? port + N` default behind an env override) names a
// different N. Two specs given one offset start two servers on one port, and
// whichever starts second fails — or, worse, talks to the other's server.
// (draftCardPort and pagesSharePort were both +405.)
//
//   node --test e2e/env-ports.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const src = fs.readFileSync(path.join(here, 'lib', 'env.ts'), 'utf8');

// offsets maps every exported const's port offset to the names using it.
export function offsets(text) {
  const out = new Map();
  const re = /export const (\w+)\s*=[^;\n]*?\bport \+ (\d+)/g;
  for (let m; (m = re.exec(text)); ) {
    const n = Number(m[2]);
    out.set(n, [...(out.get(n) ?? []), m[1]]);
  }
  return out;
}

test('the reader finds the offsets (positive control)', () => {
  const seen = offsets(src);
  assert.ok(seen.size > 100, `only ${seen.size} offsets read from env.ts`);
  assert.deepEqual(offsets('export const a = port + 1;\nexport const b = Number(process.env.X ?? port + 1);\n').get(1), ['a', 'b'],
    'a duplicate (one behind an env default) is not seen');
});

test('no two exported e2e ports share an offset', () => {
  const dup = [...offsets(src)].filter(([, names]) => names.length > 1).map(([n, names]) => `+${n}: ${names.join(', ')}`);
  assert.deepEqual(dup, []);
});
