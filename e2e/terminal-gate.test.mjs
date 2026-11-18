// The terminal page's reconnect and step-up rules for a TEAM agent's terminal,
// whose WHOLE attach needs a fresh sign-in (internal/ui AgentTerminalView
// StepUpAttach). The real rendered script (e2e/testdata/terminal-*.html,
// generated and kept current by internal/ui
// TestTheTerminalScriptFixtureIsCurrent) runs against a stub DOM and a
// WebSocket whose outcome the test decides, on a fake clock.
//
// A refused upgrade and a server that is not there look the same to a
// browser socket. A fresh operator's terminal must ride out a restart or a
// network drop with the ordinary backoff, never asking "Confirm it's you";
// a stale one must ask.
//
//   node --test e2e/terminal-gate.test.mjs
//
// (Adapted from the round-1 audit's harness for #717.)
import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const fixture = (name) => fs.readFileSync(path.join(here, 'testdata', name), 'utf8');

function page(html, { fresh }) {
  const src = /<script>([\s\S]*?)<\/script>/.exec(html)[1];
  const attrs = {};
  const tag = /<div id="term"[^>]*>/.exec(html)[0];
  for (const m of tag.matchAll(/\s([\w-]+)(?:="([^"]*)")?/g)) attrs[m[1]] = m[2] ?? '';
  const log = [];
  const els = {};
  const el = (id) => (els[id] ||= {
    id, hidden: true, textContent: '', disabled: false, style: {}, dataset: {}, options: {},
    classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
    listeners: {},
    addEventListener(t, fn) { (this.listeners[t] ||= []).push(fn); },
    removeEventListener() {},
    getAttribute(n) { return id === 'term' ? (n in attrs ? attrs[n] : null) : null; },
    hasAttribute(n) { return id === 'term' ? n in attrs : false; },
    setAttribute() {}, removeAttribute() {}, appendChild() {}, contains() { return false; },
    querySelector() { return null; }, querySelectorAll() { return []; },
    getBoundingClientRect() { return { width: 800, height: 600, top: 0, left: 0 }; },
    clientWidth: 800, clientHeight: 600, scrollTop: 0, scrollHeight: 0,
    matches() { return false; }, closest() { return null; }, focus() {},
  });
  const timers = [];
  let now = 0;
  const sockets = [];
  const server = { up: true, admits: true };
  class WS {
    constructor(url) { this.url = url; this.readyState = 0; sockets.push(this); log.push('ws ' + /mode=(\w+)/.exec(url)[1]); }
    send() {}
    close() { this.readyState = 3; }
  }
  const settle = (s) => {
    if (!server.up || !server.admits) { s.readyState = 3; s.onclose && s.onclose({ code: 1006 }); return; }
    s.readyState = 1;
    s.onopen && s.onopen();
    s.onmessage && s.onmessage({ data: JSON.stringify({ type: 'ready', mode: /mode=(\w+)/.exec(s.url)[1] }) });
  };
  const stepUp = {
    _fresh: fresh,
    fresh() { return this._fresh; },
    stale() { this._fresh = false; log.push('stale'); },
    ask(why, cb) { log.push('ASK'); this._cb = cb; },
    asking() { return false; },
  };
  const win = {
    musterStepUp: stepUp,
    location: { protocol: 'http:', host: 'x', href: 'http://x/agents/a/terminal', pathname: '/agents/a/terminal', search: '' },
    addEventListener() {}, removeEventListener() {}, matchMedia: () => ({ matches: false, addEventListener() {} }),
    requestAnimationFrame: () => 0, getComputedStyle: () => ({ getPropertyValue: () => '' }), devicePixelRatio: 1,
    innerWidth: 800, innerHeight: 600, visualViewport: null,
  };
  const sandbox = {
    window: win, self: win, location: win.location, navigator: { onLine: true, userAgent: 'node' },
    document: {
      hidden: false, visibilityState: 'visible', getElementById: el, querySelector: () => null, querySelectorAll: () => [],
      addEventListener() {}, removeEventListener() {}, createElement: () => el('x'), body: el('body'), documentElement: el('html'), fonts: null,
    },
    Terminal: class {
      constructor() { this.cols = 80; this.rows = 24; this.options = {}; this.buffer = { active: { viewportY: 0, length: 0 } }; }
      loadAddon() {} open() {} write() {} resize(c, r) { this.cols = c; this.rows = r; } onData() {} onBinary() {} onResize() {}
      onRender() {} onScroll() {} focus() {} reset() {} clear() {} attachCustomKeyEventHandler() {}
    },
    FitAddon: { FitAddon: class { fit() {} proposeDimensions() { return { cols: 80, rows: 24 }; } } },
    WebSocket: WS, TextEncoder, TextDecoder, Uint8Array, ArrayBuffer, JSON, Math, Date: { now: () => now }, console,
    setTimeout: (f, d) => { timers.push({ f, at: now + (d || 0) }); return timers.length; }, clearTimeout: () => {},
    setInterval: () => 0, clearInterval: () => {}, performance: { now: () => now },
    ResizeObserver: class { observe() {} disconnect() {} }, MutationObserver: class { observe() {} disconnect() {} },
    HTMLElement: class { togglePopover() {} }, URL, URLSearchParams, Event: class {}, CustomEvent: class {}, Promise,
    getComputedStyle: () => ({ getPropertyValue: () => '' }), requestAnimationFrame: () => 0, matchMedia: win.matchMedia,
  };
  Object.assign(win, sandbox, { window: win });
  vm.createContext(sandbox);
  vm.runInContext(src, sandbox);
  const pump = (steps = 200) => {
    for (let i = 0; i < steps; i++) {
      const s = sockets.find((x) => x.readyState === 0 && !x._settled);
      if (s) { s._settled = true; settle(s); continue; }
      if (!timers.length) return;
      timers.sort((a, b) => a.at - b.at);
      const t = timers.shift();
      now = Math.max(now, t.at);
      t.f();
    }
  };
  const drop = () => { // the live socket closes (a restart, a network drop)
    const live = sockets.filter((s) => s.readyState === 1).pop();
    live.readyState = 3;
    live.onclose({ code: 1006 });
  };
  return {
    log, server, stepUp, pump, drop, sockets,
    advance(ms) { now += ms; },
    status: () => el('term-status').textContent,
    error: () => (el('term-error').hidden ? '' : el('term-error-text').textContent),
    reconnect: () => el('term-reconnect').listeners.click.forEach((f) => f({})),
  };
}

// outage: the live attach drops and the server answers nothing for `failures`
// attempts' worth of time, then comes back.
function outage(p, failures) {
  p.pump(5);
  assert.equal(p.status(), 'Connected', 'CONTROL: the page attached at load');
  p.server.up = false;
  p.drop();
  let failed = 0;
  for (let i = 0; i < 400 && failed < failures; i++) {
    const before = p.sockets.length;
    p.pump(1);
    if (p.sockets.length > before) { p.pump(1); failed++; }
  }
  p.server.up = true;
  p.advance(120_000);
  p.pump(400);
  return failed;
}

const gated = () => fixture('terminal-gated.html');
const control = () => fixture('terminal-control.html');

test('the fixtures are the gated and the control page', () => {
  assert.match(gated(), /<div id="term"[^>]*data-step-up-attach=""/);
  assert.doesNotMatch(control(), /<div id="term"[^>]*data-step-up-attach/);
});

for (const failures of [1, 2, 5]) {
  test(`a FRESH operator's team terminal rides out a restart (${failures} failed reconnect(s)) without asking`, () => {
    const p = page(gated(), { fresh: true });
    const failed = outage(p, failures);
    assert.ok(!p.log.includes('ASK'), 'it asked "Confirm it\'s you": ' + p.log.join(', '));
    assert.ok(!p.log.includes('stale'), 'it marked a fresh sign-in stale: ' + p.log.join(', '));
    assert.equal(failed, failures, `it stopped retrying after ${failed} failed reconnect(s) of ${failures}: ` + p.log.join(', '));
    assert.equal(p.error(), '', 'an error is still shown after the server came back');
    assert.equal(p.status(), 'Connected', 'it did not reconnect: ' + p.log.join(', '));
    assert.ok(p.log.filter((l) => l === 'ws ro').length === 0, 'a gated attach fell back to read-only: ' + p.log.join(', '));
  });
}

test('…and rides out a SECOND drop after recovering from the first', () => {
  const p = page(gated(), { fresh: true });
  assert.equal(outage(p, 2), 2);
  assert.ok(!p.log.includes('stale'), 'the first drop marked the sign-in stale: ' + p.log.join(', '));
  assert.equal(p.status(), 'Connected');
  p.server.up = false;
  p.drop();
  p.pump(3);
  p.server.up = true;
  p.advance(120_000);
  p.pump(400);
  assert.ok(!p.log.includes('ASK') && !p.log.includes('stale'), p.log.join(', '));
  assert.equal(p.status(), 'Connected');
});

test('the control page (live keys gated, attach not) rides out the same restart', () => {
  const p = page(control(), { fresh: true });
  assert.equal(outage(p, 5), 5, 'CONTROL: the outage made 5 reconnects fail');
  assert.ok(!p.log.includes('ASK'), p.log.join(', '));
  assert.equal(p.status(), 'Connected');
});

test("a STALE operator's team terminal attaches nothing until Reconnect asks and the confirm lands", () => {
  const p = page(gated(), { fresh: false });
  p.pump(5);
  assert.equal(p.sockets.length, 0, 'it attached on a stale sign-in');
  assert.match(p.error(), /needs a recent sign-in/);
  p.reconnect();
  assert.ok(p.log.includes('ASK'), 'Reconnect did not ask');
  p.stepUp._fresh = true;
  p.stepUp._cb();
  p.pump(5);
  assert.deepEqual(p.log.filter((l) => l.startsWith('ws')), ['ws rw']);
  assert.equal(p.status(), 'Connected');
});

test('a confirm is spent once its attach opens: a later restart is ridden out, not read as the confirm refused', () => {
  const p = page(gated(), { fresh: false });
  p.pump(5);
  p.reconnect();
  p.stepUp._fresh = true;
  p.stepUp._cb();
  const failed = outage(p, 2);
  assert.equal(failed, 2, 'it stopped retrying: ' + p.log.join(', '));
  assert.ok(!p.log.includes('stale'), 'a drop after a confirmed attach marked the sign-in stale: ' + p.log.join(', '));
  assert.equal(p.error(), '');
  assert.equal(p.status(), 'Connected');
});

test('a sign-in that went stale while open is asked for on the next refused attach, not retried forever', () => {
  const p = page(gated(), { fresh: true });
  p.pump(5);
  p.stepUp._fresh = false; // the page's freshness ran out on its clock
  p.server.admits = false;
  p.drop();
  p.pump(50);
  assert.match(p.error(), /needs a recent sign-in/);
  const n = p.sockets.length;
  p.advance(600_000);
  p.pump(50);
  assert.equal(p.sockets.length, n, 'it kept retrying a refused attach');
});
