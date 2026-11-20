// The app tab's page script (internal/ui/app_page.go appPageScriptSrc), run as
// the browser runs it against a minimal fake document, fetch and clock:
//
//   - at load it mints {"plugin", "target": {"app": {…}}} (with the page's ref
//     when it has one) and puts a NEW frame, carrying every attribute of the
//     drawn one and its src set BEFORE it is inserted, in that one's place;
//   - while the page is visible it polls the alive route every 30 s, and a
//     404 REMOVES the frame and says the app was removed (a 204 keeps it);
//   - in the Android app (window.musterApp) no frame is loaded: the frame is
//     removed, the card shown, and Open mints at tap time and navigates the
//     top level to the minted URL.
//
//   node --test e2e/app-page.test.mjs
import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const go = fs.readFileSync(path.join(here, '..', 'internal', 'ui', 'app_page.go'), 'utf8');
const m = /var appPageScriptSrc = staticJS\("app-page", `([\s\S]*?)`\)/.exec(go);

const ENTRY = 'https://w.meet.example.test/enter?muster_code=CODE&muster_plugin=7';

function el(tag, attrs = {}) {
  const e = {
    tag, hidden: 'hidden' in attrs, textContent: '', removed: false, replacedBy: null, srcAtInsert: undefined,
    attributes: Object.entries(attrs).map(([name, value]) => ({ name, value })),
    getAttribute(n) { const a = this.attributes.find((x) => x.name === n); return a ? a.value : null; },
    setAttribute(n, v) {
      const a = this.attributes.find((x) => x.name === n);
      if (a) a.value = String(v); else this.attributes.push({ name: n, value: String(v) });
    },
    remove() { this.removed = true; },
    replaceWith(n) { this.replacedBy = n; this.removed = true; n.srcAtInsert = n.src; },
    closest(sel) { return this.matches(sel) ? this : null; },
    matches(sel) { const k = /^\[([a-z-]+)\]$/.exec(sel); return !!k && this.getAttribute(k[1]) !== null; },
  };
  return e;
}

function world({ app = false, ref = null, aliveStatus = 204, mintStatus = 200, visible = 'visible' } = {}) {
  const main = el('main', {
    id: 'app-main', 'data-plugin': '7', 'data-mint': '/plugin-handoffs', 'data-alive': '/ui/apps/7/alive',
    'data-poll': '30', 'data-removed': 'This app was removed.', ...(ref ? { 'data-ref': ref } : {}),
  });
  const frame = el('iframe', { 'data-app-frame': '', sandbox: 'allow-scripts allow-same-origin', allow: 'camera https://w.meet.example.test', title: "Tom's & Jerry's" });
  const card = el('div', { 'data-app-card': '', hidden: '' });
  const status = el('p', { id: 'app-status', hidden: '' });
  const newtab = el('button', { 'data-app-newtab': '' });
  let current = frame;
  main.querySelector = (sel) => {
    if (sel === '[data-app-frame]') {
      while (current && current.removed) current = current.replacedBy;
      return current;
    }
    if (sel === '[data-app-card]') return card.removed ? null : card;
    return null;
  };
  const listeners = {};
  const intervals = [];
  const fetches = [];
  let assigned = null;
  const doc = {
    readyState: 'complete', visibilityState: visible,
    getElementById: (id) => (id === 'app-main' ? main : id === 'app-status' ? status : null),
    querySelector: (sel) => (sel === '[data-app-newtab]' ? newtab : null),
    createElement: (tag) => el(tag),
    addEventListener: (t, fn) => { (listeners[t] ||= []).push(fn); },
  };
  const reply = (status, body) => ({
    status, ok: status >= 200 && status < 300, type: 'basic',
    headers: { get: () => 'application/json' }, json: async () => body,
  });
  const fetch = async (url, init = {}) => {
    fetches.push({ url, init });
    if (url === '/plugin-handoffs') return reply(mintStatus, mintStatus === 200 ? { url: ENTRY } : { error: 'this plugin is gone' });
    if (url === '/ui/apps/7/alive') return { status: aliveStatus, ok: aliveStatus === 204, type: 'basic', headers: { get: () => '' } };
    throw new Error('unexpected fetch ' + url);
  };
  const window = { location: { assign: (u) => { assigned = u; } }, open: () => null };
  if (app) window.musterApp = {};
  const sandbox = {
    window, document: doc, fetch, Number, JSON, Error,
    setInterval: (fn, ms) => { intervals.push({ fn, ms }); return intervals.length; },
    clearInterval: (i) => { if (intervals[i - 1]) intervals[i - 1].cleared = true; },
  };
  vm.runInNewContext(m[1], sandbox);
  const settle = () => new Promise((r) => setTimeout(r, 0));
  return {
    main, frame, card, status, newtab, fetches, intervals, doc, settle,
    current: () => main.querySelector('[data-app-frame]'),
    assigned: () => assigned,
    tick: async () => { for (const i of intervals) if (!i.cleared) i.fn(); await settle(); await settle(); },
    click: async (target) => { for (const fn of listeners.click || []) fn({ target, preventDefault() {} }); await settle(); await settle(); },
    setAlive: (s) => { aliveStatus = s; },
  };
}

test('the script is found in the Go source', () => {
  assert.ok(m, 'appPageScriptSrc was not found in internal/ui/app_page.go');
});

test('at load it mints an app target and swaps in a NEW frame with its src set before insertion', async () => {
  const w = world();
  await w.settle(); await w.settle();
  const mint = w.fetches.find((f) => f.url === '/plugin-handoffs');
  assert.ok(mint, 'no hand-off was minted at load');
  assert.deepEqual(JSON.parse(mint.init.body), { plugin: 7, target: { app: {} } });
  assert.equal(mint.init.method, 'POST');
  const f = w.current();
  assert.notEqual(f, w.frame, 'the drawn frame was navigated in place, not replaced');
  assert.equal(f.srcAtInsert, ENTRY, 'the new frame was inserted before its src was set');
  for (const n of ['sandbox', 'allow', 'title', 'data-app-frame']) {
    assert.equal(f.getAttribute(n), w.frame.getAttribute(n), `the new frame lost its ${n}`);
  }
});

test("the page's ref rides the mint's app target", async () => {
  const w = world({ ref: 'abc' });
  await w.settle(); await w.settle();
  const mint = w.fetches.find((f) => f.url === '/plugin-handoffs');
  assert.deepEqual(JSON.parse(mint.init.body), { plugin: 7, target: { app: { ref: 'abc' } } });
});

test('a refused mint is said in place of the frame', async () => {
  const w = world({ mintStatus: 404 });
  await w.settle(); await w.settle();
  assert.equal(w.current(), null, 'the frame is still drawn after a refused mint');
  assert.equal(w.status.textContent, 'this plugin is gone');
  assert.equal(w.status.hidden, false);
});

test('the alive poll runs every 30 s and a 404 removes the frame', async () => {
  const w = world();
  await w.settle(); await w.settle();
  assert.equal(w.intervals.length, 1);
  assert.equal(w.intervals[0].ms, 30000);
  await w.tick();
  assert.ok(w.current(), 'CONTROL: a 204 removed the frame');
  assert.equal(w.fetches.filter((f) => f.url === '/ui/apps/7/alive').length, 1, 'the tick did not poll');
  w.setAlive(404);
  await w.tick();
  assert.equal(w.current(), null, 'the frame is still on the page after the alive poll answered 404');
  assert.equal(w.status.textContent, 'This app was removed.');
  assert.equal(w.newtab.hidden, true, 'New tab is still offered for a removed app');
  assert.equal(w.intervals[0].cleared, true, 'the poll keeps running after the removal');
});

test('a hidden page does not poll', async () => {
  const w = world({ visible: 'hidden' });
  await w.settle();
  await w.tick();
  assert.equal(w.fetches.filter((f) => f.url === '/ui/apps/7/alive').length, 0);
});

test('in the Android app no frame is loaded: the card is shown and Open navigates the top level', async () => {
  const w = world({ app: true });
  await w.settle(); await w.settle();
  assert.equal(w.fetches.filter((f) => f.url === '/plugin-handoffs').length, 0, 'it minted at load in the app');
  assert.equal(w.current(), null, 'the frame is still drawn in the app');
  assert.equal(w.card.hidden, false, 'the Open card is not shown in the app');
  assert.equal(w.newtab.hidden, true);
  const open = el('button', { 'data-app-open': '' });
  await w.click(open);
  assert.equal(w.fetches.filter((f) => f.url === '/plugin-handoffs').length, 1, 'Open did not mint at tap time');
  assert.equal(w.assigned(), ENTRY, 'Open did not navigate the top level to the minted URL');
});
