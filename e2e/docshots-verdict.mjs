// The verdict on a docshots CHECK run (`make docshots-check`), read from the
// Playwright JSON report rather than the exit code: `playwright test` exits 0
// when every test SKIPPED, and when its filter matched nothing at all.
//
// It refuses: any failure, flake or skip; fewer than MIN passes; and any result
// from a file other than docshots/docs.spec.ts. That last one is the instrument
// check — the docshots project's testMatch was once unanchored and, in a checkout
// whose directory name ended in "docshots", collected 977 suite tests instead of
// this file's 13 (e2e/playwright.config.ts).
//
//   node docshots-verdict.mjs test-results/results.json
import fs from 'node:fs';

const FILE = 'docshots/docs.spec.ts';
// The tests docs.spec.ts holds today. A floor, not an exact count, so adding a
// shot does not need an edit here; removing one does.
const MIN = 13;

const report = JSON.parse(fs.readFileSync(process.argv[2] ?? 'test-results/results.json', 'utf8'));
const files = new Map();
let passed = 0;
const bad = [];
const walk = (suite) => {
  for (const s of suite.suites ?? []) walk(s);
  for (const spec of suite.specs ?? []) {
    files.set(spec.file, (files.get(spec.file) ?? 0) + 1);
    for (const t of spec.tests) {
      const statuses = t.results.map((r) => r.status);
      if (t.status === 'expected' && statuses.length === 1 && statuses[0] === 'passed') passed++;
      else bad.push(`${spec.file}: ${spec.title}: ${t.status} [${statuses.join(',') || 'did not run'}]`);
    }
  }
};
for (const s of report.suites ?? []) walk(s);

const errors = [];
for (const e of report.errors ?? []) errors.push(`run error: ${(e.message ?? '').split('\n')[0]}`);
for (const [f, n] of files) if (f !== FILE) errors.push(`${n} test(s) from ${f}: the docshots project collected a file that is not ${FILE}`);
errors.push(...bad);
if (passed < MIN) errors.push(`${passed} passed, want at least ${MIN}`);

console.log(`docshots: ${passed} passed, ${bad.length} not passed, files ${JSON.stringify(Object.fromEntries(files))}`);
if (errors.length) {
  for (const e of errors) console.error('docshots-verdict: ' + e);
  process.exit(1);
}
