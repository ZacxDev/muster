#!/usr/bin/env node
// verdict.mjs — read Playwright's JSON report(s) and refuse a run that proves
// nothing.
//
// 🔴 A COUNT, NOT AN EXIT CODE. `playwright test` exits 0 when every test
// skipped, and a suite pointed at no database skips by design. This prints the
// PAIR (how many passed, how many skipped) and fails when fewer than FLOOR tests
// passed or more than CAP skipped.
//
// FLOOR is the number of tests in e2e/tests/ (measured: every one passes
// against a seeded database — see the PR). A floor below the real count is
// indistinguishable from no floor, so when a spec is added, raise it in the
// same change.
//
// 🔴 CI RUNS THE SUITE IN LEGS (e2e/legs.json), SO THERE ARE THREE MODES:
//
//   node verdict.mjs REPORT                  the whole suite in one run (make e2e)
//   node verdict.mjs --leg N REPORT          one CI leg
//   node verdict.mjs --all REPORT REPORT…    every leg's report, summed
//   node verdict.mjs --docshots REPORT       the docs screenshot spec (make docshots-check)
//
// The failed == 0 and skipped <= CAP rules hold in every mode. FLOOR is a TOTAL,
// so it holds in the whole-suite and `--all` modes; a leg's floor defaults to 1.
//
// 🔴 `--all` IS THE SEAM GUARD: NO SPEC DROPPED, NONE RUN TWICE. Over the summed
// reports, every spec file on disk must be present with at least one passing
// test (else NOT RUN / NO PASS), no file may appear in two reports (RAN TWICE),
// and nothing may run that is not on disk (STRAY). It reads what RAN, so it
// catches a spec in no leg, a spec in two legs, and a legs.json entry spelled so
// it matches nothing (its real file is then NOT RUN) — measured, each against
// real leg reports, before a static legs.json check was dropped as redundant (#58).
import fs from 'node:fs';
import { specFiles } from './legs.mjs';

// 🔴 --docshots: THE DOCS SCREENSHOT SPEC, RUN AS A CHECK (`make docshots-check`,
// CI's `docshots` job). Stricter than the suite's modes, because the spec is a
// generator nothing else runs: every test must come from DOCSHOTS_FILE (the
// project's testMatch was once unanchored and collected 977 suite tests in a
// checkout named *docshots), at least DOCSHOTS_MIN must pass on their first and
// only attempt, and NOTHING may skip, flake, not run, or pass as an expected
// failure. A run error, or a missing or empty report, fails too.
//
// DOCSHOTS_MIN is the number of test() calls docs.spec.ts makes in a default run:
// 12, since mirror-terminal.png is opted out until #623 (see the spec). It is
// the ONE copy of that count; adding or removing a shot edits it here.
const DOCSHOTS_FILE = 'docshots/docs.spec.ts';
const DOCSHOTS_MIN = 12;

function docshotsVerdict(path) {
  let report;
  try {
    const text = fs.readFileSync(path, 'utf8');
    if (!text.trim()) throw new Error('the report is empty');
    report = JSON.parse(text);
  } catch (e) {
    console.error(`FAIL: docshots: no readable report at ${path}: ${e.message}`);
    return 1;
  }
  const errors = [];
  for (const e of report.errors ?? []) errors.push(`run error: ${String(e.message ?? e).split('\n')[0]}`);
  const files = new Map();
  let passed = 0;
  const visit = (suite) => {
    for (const spec of suite.specs ?? []) {
      const file = spec.file ?? suite.file;
      files.set(file, (files.get(file) ?? 0) + 1);
      for (const t of spec.tests ?? []) {
        const statuses = (t.results ?? []).map((r) => r.status);
        if (t.status === 'expected' && (t.expectedStatus ?? 'passed') === 'passed' && statuses.length === 1 && statuses[0] === 'passed') passed++;
        else errors.push(`${file}: ${spec.title}: ${t.status} (expected ${t.expectedStatus ?? 'passed'}) [${statuses.join(',') || 'did not run'}]`);
      }
    }
    for (const s of suite.suites ?? []) visit(s);
  };
  for (const s of report.suites ?? []) visit(s);
  for (const [f, n] of files) if (f !== DOCSHOTS_FILE) errors.push(`${n} test(s) from ${f}: the docshots project collected a file that is not ${DOCSHOTS_FILE}`);
  if (passed < DOCSHOTS_MIN) errors.push(`${passed} passed is under the docshots floor of ${DOCSHOTS_MIN}`);
  console.log(`e2e verdict (docshots): ${passed} passed, ${errors.length} problem(s), files ${JSON.stringify(Object.fromEntries(files))}`);
  for (const e of errors) console.error(`FAIL: docshots: ${e}`);
  return errors.length ? 1 : 0;
}

const args = process.argv.slice(2);
if (args[0] === '--docshots') {
  if (args.length > 2) {
    console.error(`usage: node verdict.mjs --docshots REPORT (got ${args.length - 1} reports)`);
    process.exit(2);
  }
  process.exit(docshotsVerdict(args[1] ?? 'test-results/results.json'));
}
let mode = 'whole';
let leg = '';
if (args[0] === '--leg') {
  mode = 'leg';
  leg = args[1] ?? '';
  args.splice(0, 2);
} else if (args[0] === '--all') {
  mode = 'all';
  args.splice(0, 1);
}
const files = args.length ? args : ['test-results/results.json'];
if (mode !== 'all' && files.length !== 1) {
  console.error(`usage: one report per run unless --all (got ${files.length})`);
  process.exit(2);
}

const FLOOR = Number(process.env.E2E_PASS_FLOOR ?? (mode === 'leg' ? 1 : 120));
const CAP = Number(process.env.E2E_SKIP_CAP ?? 0);

// perFile walks a report's suite tree: file -> { tests, passed }.
function perFile(report) {
  const out = new Map();
  const visit = (suite, file) => {
    for (const spec of suite.specs ?? []) {
      for (const t of spec.tests ?? []) {
        const e = out.get(file) ?? { tests: 0, passed: 0 };
        e.tests++;
        if (t.status === 'expected' || t.status === 'flaky') e.passed++;
        out.set(file, e);
      }
    }
    for (const s of suite.suites ?? []) visit(s, file);
  };
  for (const s of report.suites ?? []) visit(s, s.file);
  return out;
}

const sum = { expected: 0, flaky: 0, unexpected: 0, skipped: 0 };
const seen = new Map(); // file -> report path
const overlaps = [];
const ran = new Map();
for (const f of files) {
  const report = JSON.parse(fs.readFileSync(f, 'utf8'));
  const s = report.stats ?? {};
  for (const k of Object.keys(sum)) sum[k] += s[k] ?? 0;
  for (const [file, e] of perFile(report)) {
    if (seen.has(file)) overlaps.push(`${file} is in ${seen.get(file)} AND ${f}`);
    seen.set(file, f);
    const prev = ran.get(file) ?? { tests: 0, passed: 0 };
    ran.set(file, { tests: prev.tests + e.tests, passed: prev.passed + e.passed });
  }
}

const passed = sum.expected + sum.flaky;
const label = mode === 'leg' ? `e2e verdict (leg ${leg})` : mode === 'all' ? `e2e verdict (${files.length} reports)` : 'e2e verdict';
console.log(`${label}: ${passed} passed (${sum.flaky} flaky), ${sum.unexpected} failed, ${sum.skipped} skipped — floor ${FLOOR}, skip cap ${CAP}`);
let bad = false;
if (sum.unexpected > 0) { console.error('FAIL: tests failed'); bad = true; }
if (passed < FLOOR) { console.error(`FAIL: ${passed} passed is under the floor of ${FLOOR}`); bad = true; }
if (sum.skipped > CAP) { console.error(`FAIL: ${sum.skipped} skipped exceeds the cap of ${CAP}`); bad = true; }

if (mode === 'all') {
  const want = specFiles();
  const wantSet = new Set(want);
  const passing = [...ran].filter(([, e]) => e.passed > 0).length;
  console.log(`spec files: ${want.length} on disk, ${ran.size} in the report(s), ${passing} with a passing test`);
  for (const o of overlaps) { console.error(`FAIL: RAN TWICE: ${o}`); bad = true; }
  for (const f of want) {
    if (!ran.has(f)) { console.error(`FAIL: NOT RUN: ${f} is on disk but absent from every report`); bad = true; }
    else if (ran.get(f).passed === 0) { console.error(`FAIL: NO PASS: ${f} ran but no test in it passed`); bad = true; }
  }
  for (const f of ran.keys()) {
    if (!wantSet.has(f)) { console.error(`FAIL: STRAY: ${f} ran but is not a spec file on disk`); bad = true; }
  }
}
process.exit(bad ? 1 : 0);
