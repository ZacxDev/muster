// The rules of the rendered view's snapshot store (web/static/mirror-snap.js),
// loaded as the browser loads it, minus the DOM: with no `document` the file
// only publishes its pure core (globalThis.musterSnapCore) and stops.
//
//   node --test e2e/mirror-snap.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const src = fs.readFileSync(path.join(here, '..', 'web', 'static', 'mirror-snap.js'), 'utf8');
const sandbox = { URL, JSON };
sandbox.globalThis = sandbox;
vm.runInNewContext(src, sandbox);
const core = sandbox.musterSnapCore;
// The core's values are the sandbox's: compared as plain JSON.
const plain = (x) => JSON.parse(JSON.stringify(x));

const NOW = 1_000_000_000_000;
const rec = (o = {}) => ({ user: '7', build: 'b1', at: NOW - 1000, gen: 'g', offset: 10, batches: [{ type: 'batch', reset: true, ops: [] }], ...o });

test('the core loads without a DOM, and touches none', () => {
  assert.equal(typeof core.usable, 'function');
  assert.equal(sandbox.musterSnap, undefined, 'the browser half did not run');
});

test("a record is painted only for its own user and build, inside the TTL", () => {
  assert.equal(core.usable(rec(), '7', 'b1', NOW), 'ok');
  assert.equal(core.usable(rec(), 7, 'b1', NOW), 'ok', 'the user compares as text: a number from the tag is the same user');
  assert.equal(core.usable(rec({ user: '8' }), '7', 'b1', NOW), 'foreign');
  assert.equal(core.usable(rec({ user: '70' }), '7', 'b1', NOW), 'foreign', 'no prefix match');
  assert.equal(core.usable(rec({ build: 'b0' }), '7', 'b1', NOW), 'build');
  assert.equal(core.usable(null, '7', 'b1', NOW), 'missing');
  assert.equal(core.usable(rec({ gen: '' }), '7', 'b1', NOW), 'empty');
  assert.equal(core.usable(rec({ batches: [] }), '7', 'b1', NOW), 'empty');
});

test('the TTL is five minutes, measured at both sides of it and far past it', () => {
  assert.equal(core.TTL_MS, 5 * 60 * 1000);
  assert.equal(core.usable(rec({ at: NOW - 4.5 * 60_000 }), '7', 'b1', NOW), 'ok');
  assert.equal(core.usable(rec({ at: NOW - core.TTL_MS }), '7', 'b1', NOW), 'ok', 'exactly at the TTL is still inside it');
  assert.equal(core.usable(rec({ at: NOW - core.TTL_MS - 1 }), '7', 'b1', NOW), 'expired');
  assert.equal(core.usable(rec({ at: NOW - 3_600_000 }), '7', 'b1', NOW), 'expired');
  assert.equal(core.usable(rec({ at: NOW + 120_000 }), '7', 'b1', NOW), 'expired', 'a record from the future is not trusted');
  assert.equal(core.usable(rec({ at: 'yesterday' }), '7', 'b1', NOW), 'expired');
});

test("the key names the user, the build and the window, and two users' never meet", () => {
  const w = { host: 'h-1', session: 'work', window: '@3' };
  assert.equal(core.keyOf('7', 'b1', w), JSON.stringify(['7', 'b1', 'h-1', 'work', '@3']));
  assert.notEqual(core.keyOf('7', 'b1', w), core.keyOf('8', 'b1', w));
  assert.notEqual(core.keyOf('7', 'b1', w), core.keyOf('7', 'b2', w));
  // A separator in a value cannot forge another window's key.
  assert.notEqual(core.keyOf('7', 'b1', { host: 'a|b', session: 'c' }), core.keyOf('7', 'b1', { host: 'a', session: 'b|c' }));
});

test("refOf reads a view's page or socket path, however it was encoded, and nothing else", () => {
  const want = { host: 'h 1', session: 'work', window: '@3' };
  assert.deepEqual(plain(core.refOf('/mirror/view?host=h+1&session=work&window=%403')), want);
  assert.deepEqual(plain(core.refOf('/mirror/view/ws?window=@3&session=work&host=h%201')), want);
  assert.deepEqual(plain(core.refOf('/mirror/view?host=h&session=s')), { host: 'h', session: 's', window: '' });
  for (const p of ['/mirror/terminal?host=h&session=s', '/agents/x', '/mirror/view?host=h', '/mirror/viewx?host=h&session=s', '', null]) {
    assert.equal(core.refOf(p), null, String(p));
  }
});

test('bound keeps the newest batches that fit, and nothing before the last reset', () => {
  const b = (i, extra = {}) => ({ type: 'batch', i, ops: [{ op: 'append', html: 'x'.repeat(90) }], ...extra });
  const one = JSON.stringify(b(0)).length;
  const all = [b(0, { reset: true }), b(1), b(2), b(3, { reset: true }), b(4), b(5)];
  assert.deepEqual(plain(core.bound(all, 1e9).batches.map((x) => x.i)), [3, 4, 5], 'a reset starts the conversation over');
  // Room for two and a half: the two newest.
  const two = core.bound(all, Math.floor(one * 2.5));
  assert.deepEqual(plain(two.batches.map((x) => x.i)), [4, 5]);
  assert.ok(two.bytes <= one * 2.5);
  assert.deepEqual(plain(core.bound(all, 1).batches), [], 'nothing fits: nothing kept');
  assert.equal(core.MAX_BYTES, 2 * 1024 * 1024);
});

test('worth keeps what draws and drops a bare marker', () => {
  assert.equal(core.worth({ type: 'batch', offset: 5, busy: true, ops: [] }), false);
  assert.equal(core.worth({ type: 'batch', offset: 5, ops: [{ op: 'append' }] }), true);
  assert.equal(core.worth({ type: 'batch', reset: true, ops: [] }), true);
  assert.equal(core.worth({ type: 'batch', index: { items: [] }, ops: [] }), true);
  assert.equal(core.worth({ type: 'batch', context: { label: '1k context' }, ops: [] }), true);
});
