// The rules of the server switcher's list (web/static/servers.js), loaded as
// the browser loads it, minus the DOM: with no `document` the file only
// publishes its pure core (globalThis.musterServersCore) and stops.
//
//   node --test e2e/servers.test.mjs
//
// Every address here is synthetic (example.com / example.net / .test, and
// loopback).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const src = fs.readFileSync(path.join(here, '..', 'web', 'static', 'servers.js'), 'utf8');
const sandbox = { URL, JSON, TextDecoder, btoa, Math, Array, String, Number, Uint8Array, Promise, isFinite };
sandbox.globalThis = sandbox;
vm.runInNewContext(src, sandbox);
const core = sandbox.musterServersCore;
const plain = (x) => JSON.parse(JSON.stringify(x));
// The core's values are the sandbox's (another realm): compared as plain JSON.
const deq = (a, b, m) => assert.deepEqual(plain(a), plain(b), m);

const SELF = 'https://a.example.com';
const B = 'https://b.example.net';
const NOW = 1_000_000_000_000;

test('the core loads without a DOM', () => {
  assert.equal(typeof core.parseServer, 'function');
  assert.equal(core.MAX, 8);
  assert.equal(core.PALETTE.length, core.MAX, 'one colour per server');
});

// ---- addresses --------------------------------------------------------------

test('an address is https, or http on localhost / 127.0.0.1 only', () => {
  const ok = (input, origin) => deq(plain(core.parseServer(input)), { ok: true, origin }, input);
  const no = (input, error) => deq(plain(core.parseServer(input)), { ok: false, error }, input);
  ok('https://b.example.net', B);
  ok('b.example.net', B);
  ok('  B.Example.NET  ', B);
  ok('https://b.example.net:443', B);
  ok('https://b.example.net:8443', 'https://b.example.net:8443');
  ok('http://localhost:8105', 'http://localhost:8105');
  ok('http://127.0.0.1:8105', 'http://127.0.0.1:8105');
  no('http://b.example.net', 'scheme');
  no('http://localhost.example.net', 'scheme');
  no('http://127.0.0.2', 'scheme');
  no('http://192.0.2.1', 'scheme');
  no('ftp://b.example.net', 'scheme');
  no('javascript://b.example.net/%0aalert(1)', 'scheme');
  no('', 'empty');
  no('   ', 'empty');
  no(null, 'empty');
  no('https://', 'invalid');
  no('https://exa mple.com', 'invalid');
});

test('an address with a user name or password in it is refused, not stripped', () => {
  for (const input of ['https://user:secret@b.example.net', 'https://user@b.example.net', 'user:secret@b.example.net', 'https://:secret@b.example.net']) {
    deq(plain(core.parseServer(input)), { ok: false, error: 'credentials' }, input);
  }
});

test('only the origin is kept: a path, query or fragment (a token in it) never is', () => {
  const r = core.parseServer('https://b.example.net/agents/7?token=mst_synthetic_0000#frag');
  deq(plain(r), { ok: true, origin: B });
  const added = core.add(core.normalize(null, SELF), 'https://b.example.net/x?token=mst_synthetic_0000');
  assert.equal(added.ok, true);
  assert.ok(!JSON.stringify(added.list).includes('mst_'), 'nothing of the query is stored');
  assert.ok(!JSON.stringify(added.list).includes('/x'), 'nothing of the path is stored');
});

// ---- the list ---------------------------------------------------------------

test('the list always starts with this server, which cannot be removed', () => {
  const l = core.normalize(null, SELF);
  deq(l.map((s) => s.origin), [SELF]);
  const r = core.add(l, B);
  deq(r.list.map((s) => s.origin), [SELF, B]);
  deq(core.remove(r.list, SELF, SELF).map((s) => s.origin), [SELF, B]);
  deq(core.remove(r.list, B, SELF).map((s) => s.origin), [SELF]);
  // A stored list that left this server out, or put it later, is mended.
  const stored = { v: 1, servers: [{ origin: B }, { origin: SELF, label: 'Mine' }] };
  deq(core.normalize(stored, SELF).map((s) => [s.origin, s.label]), [[SELF, 'Mine'], [B, '']]);
});

test('no duplicates: the same origin however typed', () => {
  let l = core.add(core.normalize(null, SELF), 'b.example.net').list;
  for (const again of ['https://b.example.net', 'https://B.example.net:443/tasks', 'b.example.net/']) {
    const r = core.add(l, again);
    deq(plain(r), { ok: false, error: 'duplicate', origin: B }, again);
  }
  assert.equal(core.add(l, SELF).error, 'duplicate', 'this server too');
  // A stored list with a duplicate keeps the first.
  const stored = { v: 1, servers: [{ origin: SELF }, { origin: B, label: 'one' }, { origin: B, label: 'two' }] };
  deq(core.normalize(stored, SELF).map((s) => s.label), ['', 'one']);
});

test('at most MAX servers, this one included', () => {
  let l = core.normalize(null, SELF);
  for (let i = 1; i < core.MAX; i++) {
    const r = core.add(l, `https://s${i}.example.net`);
    assert.equal(r.ok, true, `server ${i + 1}`);
    l = r.list;
  }
  assert.equal(l.length, core.MAX);
  deq(plain(core.add(l, 'https://one-too-many.example.net')), { ok: false, error: 'full' });
  const stored = { v: 1, servers: Array.from({ length: 12 }, (_, i) => ({ origin: `https://t${i}.example.net` })) };
  const n = core.normalize(stored, SELF);
  assert.equal(n.length, core.MAX, 'a stored list over the cap is cut');
  assert.equal(n[0].origin, SELF, 'this server is kept, first');
});

test('a stored entry is re-checked: a bad origin, or one that is not its own origin, is dropped', () => {
  const stored = {
    v: 1,
    servers: [
      { origin: 'http://b.example.net' },
      { origin: 'https://user:pw@b.example.net' },
      { origin: 'https://b.example.net/path' },
      { origin: 'https://B.EXAMPLE.NET' },
      { origin: 42 },
      null,
      { origin: 'https://c.example.net', label: 'C', logo: 'javascript:alert(1)', etag: 'nothex!' },
    ],
  };
  const l = core.normalize(stored, SELF);
  deq(l.map((s) => s.origin), [SELF, 'https://c.example.net']);
  assert.equal(l[1].logo, '', 'a logo that is not a PNG data: URL is not kept');
  assert.equal(l[1].etag, '');
  deq(core.normalize({ v: 2, servers: [{ origin: B }] }, SELF).map((s) => s.origin), [SELF], 'another version is not read');
  deq(core.normalize('garbage', SELF).map((s) => s.origin), [SELF]);
});

test('load and save survive a missing or throwing storage', () => {
  const mem = new Map();
  const st = { getItem: (k) => mem.get(k) ?? null, setItem: (k, v) => mem.set(k, v) };
  const l = core.add(core.load(st, SELF), B).list;
  assert.equal(core.save(st, l), true);
  deq(core.load(st, SELF).map((s) => s.origin), [SELF, B]);
  mem.set(core.KEY, '{not json');
  deq(core.load(st, SELF).map((s) => s.origin), [SELF]);
  const bad = { getItem() { throw new Error('denied'); }, setItem() { throw new Error('full'); } };
  deq(core.load(bad, SELF).map((s) => s.origin), [SELF]);
  assert.equal(core.save(bad, l), false);
});

test('labels: the person’s own, else the server’s name, else its host', () => {
  let l = core.add(core.normalize(null, 'http://localhost:8105'), 'https://b.example.net:8443').list;
  assert.equal(core.display(l[0]), 'localhost:8105');
  assert.equal(core.display(l[1]), 'b.example.net:8443');
  l[1].name = 'Team B';
  assert.equal(core.display(l[1]), 'Team B');
  l = core.rename(l, l[1].origin, '  Work  ');
  assert.equal(core.display(l[1]), 'Work');
  l = core.rename(l, l[1].origin, '');
  assert.equal(core.display(l[1]), 'Team B', 'an empty label clears it');
  assert.equal(core.hostLabel('https://b.example.net:443'), 'b.example.net');
});

test('a label or name is cleaned: control and direction characters go, at most 64', () => {
  assert.equal(core.cleanText('a‮b\u0000c⁦d\n'), 'abcd');
  assert.equal(core.cleanText('x\u{E0041}y'), 'xy', 'tag characters');
  assert.equal(Array.from(core.cleanText('é'.repeat(80))).length, 64);
  assert.equal(core.cleanText(7), '');
  assert.equal(core.cleanText('<b>bold</b>'), '<b>bold</b>', 'text, drawn as text (textContent), never markup');
});

test('colours: one per server, from the origin, the same on every list holding the same servers', () => {
  const set = [SELF, B, 'https://c.example.org', 'http://localhost:8105'];
  const c1 = core.colours(set);
  const c2 = core.colours(set.slice().reverse());
  deq(plain(c1), plain(c2), 'order of the list does not matter');
  assert.equal(new Set(Object.values(c1)).size, set.length, 'no two servers share a colour');
  for (const v of Object.values(c1)) assert.ok(core.PALETTE.includes(v));
  const full = Array.from({ length: 8 }, (_, i) => `https://f${i}.example.net`);
  assert.equal(new Set(Object.values(core.colours(full))).size, 8, 'eight servers, eight colours');
  // Not all one colour: two lone servers whose hashes differ get different slots.
  const lone = [B, 'https://c.example.org', 'https://d.example.com', SELF].map((o) => core.colours([o])[o]);
  assert.ok(new Set(lone).size > 1, `a lone server's colour comes from its origin: ${lone}`);
});

test('stale: unread, older than the TTL, or from the future', () => {
  assert.equal(core.stale({ checkedAt: 0 }, NOW), true);
  assert.equal(core.stale({ checkedAt: NOW - core.TTL_MS + 1 }, NOW), false);
  assert.equal(core.stale({ checkedAt: NOW - core.TTL_MS - 1 }, NOW), true);
  assert.equal(core.stale({ checkedAt: NOW + 60_000 }, NOW), true);
});

// ---- branding ---------------------------------------------------------------

const PNG = Uint8Array.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3, 4]);
const resp = (status, body, headers = {}) => ({
  ok: status >= 200 && status < 300,
  status,
  headers: { get: (k) => headers[k.toLowerCase()] ?? null },
  arrayBuffer: async () => (typeof body === 'string' ? new TextEncoder().encode(body) : body).buffer,
});
// A fake server: each path answers from routes; every call is recorded with
// its options.
function fakeFetch(routes) {
  const calls = [];
  const f = async (url, opts) => {
    calls.push({ url, opts: plain(opts) });
    const r = routes[url];
    if (!r) return resp(404, '');
    return typeof r === 'function' ? r() : r;
  };
  f.calls = calls;
  return f;
}
const doc = (name, etag) => JSON.stringify({ name, logo: etag ? { url: `/brand/logo.png?v=${etag}`, icon192: `/brand/icon-192.png?v=${etag}`, icon512: `/brand/icon-512.png?v=${etag}`, etag } : null });

test('🔴 every read sends no credentials and no referrer, follows no redirect, and goes only to the server’s own /api/instance and /brand/', async () => {
  const f = fakeFetch({
    [`${B}/api/instance`]: resp(200, doc('Team B', 'aa11')),
    [`${B}/brand/icon-192.png?v=aa11`]: resp(200, PNG),
  });
  const e = core.normalize({ v: 1, servers: [{ origin: B }] }, SELF)[1];
  await core.refresh(e, f, NOW);
  deq(f.calls.map((c) => c.url), [`${B}/api/instance`, `${B}/brand/icon-192.png?v=aa11`]);
  for (const c of f.calls) {
    deq(c.opts, { credentials: 'omit', referrerPolicy: 'no-referrer', redirect: 'error', mode: 'cors' }, c.url);
  }
});

test('the name, the logo as a PNG data: URL, and its etag are cached', async () => {
  const f = fakeFetch({
    [`${B}/api/instance`]: resp(200, doc('  Team‮ B ', 'aa11')),
    [`${B}/brand/icon-192.png?v=aa11`]: resp(200, PNG),
  });
  const e = core.normalize({ v: 1, servers: [{ origin: B }] }, SELF)[1];
  const n = await core.refresh(e, f, NOW);
  assert.equal(n.name, 'Team B');
  assert.equal(n.etag, 'aa11');
  assert.equal(n.logo, 'data:image/png;base64,' + Buffer.from(PNG).toString('base64'));
  assert.equal(n.checkedAt, NOW);
  // And it survives a save/load (the data: URL passes the stored-logo check).
  assert.equal(core.normalize({ v: 1, servers: [n] }, SELF)[1].logo, n.logo);
});

test('the logo is fetched again only when the etag changes', async () => {
  let icon = 0;
  let etag = 'aa11';
  const routes = {
    [`${B}/api/instance`]: () => resp(200, doc('Team B', etag)),
    [`${B}/brand/icon-192.png?v=aa11`]: () => { icon++; return resp(200, PNG); },
    [`${B}/brand/icon-192.png?v=bb22`]: () => { icon++; return resp(200, Uint8Array.from([...PNG, 9])); },
  };
  const f = fakeFetch(routes);
  let e = core.normalize({ v: 1, servers: [{ origin: B }] }, SELF)[1];
  e = await core.refresh(e, f, NOW);
  assert.equal(icon, 1);
  const first = e.logo;
  e = await core.refresh(e, f, NOW + 1);
  assert.equal(icon, 1, 'same etag: the cached logo, no fetch');
  assert.equal(e.logo, first);
  etag = 'bb22';
  e = await core.refresh(e, f, NOW + 2);
  assert.equal(icon, 2, 'new etag: fetched');
  assert.notEqual(e.logo, first);
  assert.equal(e.etag, 'bb22');
  // The logo removed on the server: the cache drops it.
  routes[`${B}/api/instance`] = () => resp(200, doc('Team B', null));
  e = await core.refresh(e, f, NOW + 3);
  assert.equal(e.logo, '');
  assert.equal(e.etag, '');
});

test('a logo on another origin, outside /brand/, not a PNG or too large is not kept', async () => {
  const base = core.normalize({ v: 1, servers: [{ origin: B }] }, SELF)[1];
  const cases = {
    'another origin': { logo: { icon192: 'https://evil.example.org/brand/icon-192.png', etag: 'aa11' } },
    'protocol-relative': { logo: { icon192: '//evil.example.org/brand/i.png', etag: 'aa11' } },
    'not under /brand/': { logo: { icon192: '/api/tasks', etag: 'aa11' } },
  };
  for (const [what, d] of Object.entries(cases)) {
    const f = fakeFetch({ [`${B}/api/instance`]: resp(200, JSON.stringify(d)) });
    const n = await core.refresh(base, f, NOW);
    assert.equal(n.logo, '', what);
    deq(f.calls.map((c) => c.url), [`${B}/api/instance`], `${what}: no second fetch`);
  }
  const notPNG = fakeFetch({ [`${B}/api/instance`]: resp(200, doc('B', 'aa11')), [`${B}/brand/icon-192.png?v=aa11`]: resp(200, '<svg/>') });
  assert.equal((await core.refresh(base, notPNG, NOW)).logo, '', 'not a PNG');
  const big = new Uint8Array(core.MAX_LOGO_BYTES + 1);
  big.set(PNG);
  const tooBig = fakeFetch({ [`${B}/api/instance`]: resp(200, doc('B', 'aa11')), [`${B}/brand/icon-192.png?v=aa11`]: resp(200, big) });
  assert.equal((await core.refresh(base, tooBig, NOW)).logo, '', 'over the cap');
  const declared = fakeFetch({ [`${B}/api/instance`]: resp(200, doc('B', 'aa11')), [`${B}/brand/icon-192.png?v=aa11`]: resp(200, PNG, { 'content-length': String(10 ** 9) }) });
  assert.equal((await core.refresh(base, declared, NOW)).logo, '', 'a declared length over the cap');
});

test('a failed read keeps what was cached, and waits for the TTL', async () => {
  const cached = core.normalize({ v: 1, servers: [{ origin: B, name: 'Team B', etag: 'aa11', logo: 'data:image/png;base64,AAAA', checkedAt: 1 }] }, SELF)[1];
  const failures = [
    fakeFetch({}),
    fakeFetch({ [`${B}/api/instance`]: resp(200, '{not json') }),
    fakeFetch({ [`${B}/api/instance`]: resp(200, 'x'.repeat(64 * 1024 + 1)) }),
    async () => { throw new TypeError('CORS or network'); },
  ];
  for (const f of failures) {
    const n = await core.refresh(cached, f, NOW);
    deq([n.name, n.etag, n.logo], ['Team B', 'aa11', 'data:image/png;base64,AAAA']);
    assert.equal(n.checkedAt, NOW);
  }
});

// ---- the switch and the hand-off ---------------------------------------------

test('the switch carries this server’s origin in the fragment, and nothing else', () => {
  assert.equal(core.switchHref(B, SELF), `${B}/#muster-from=${encodeURIComponent(SELF)}`);
  assert.equal(core.handoff(`#muster-from=${encodeURIComponent(SELF)}`, B), SELF);
});

test('the hand-off is read only for a valid, canonical origin that is not this server', () => {
  const h = (v) => core.handoff('#muster-from=' + encodeURIComponent(v), B);
  assert.equal(h('http://b.example.net'), null, 'not https');
  assert.equal(h('https://user:pw@a.example.com'), null, 'credentials');
  assert.equal(h('https://a.example.com/path'), null, 'not an origin');
  assert.equal(h('https://A.example.com'), null, 'not canonical');
  assert.equal(h(B), null, 'this server');
  assert.equal(core.handoff('#muster-from=%E0%A4%A', B), null, 'bad escape');
  assert.equal(core.handoff('#task-12', B), null);
  assert.equal(core.handoff('', B), null);
  assert.equal(h('http://localhost:8105'), 'http://localhost:8105');
});
