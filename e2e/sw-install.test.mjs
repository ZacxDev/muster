// The service worker's install and carry-forward rules for the pages' scripts
// (web/static/sw.js), run in node against stubbed caches and fetch.
//
//   node --test e2e/sw-install.test.mjs
//
// 🔴 THE CASE IS A ROLLOUT. A worker installing for build C fetches
// /static/js/x.js?v=<C's hash>, and the request can land on a pod still
// serving build B — which answers 200 with B's bytes (?v= only chooses the
// Cache-Control). Stored, those bytes would be served cache-first under C's
// URL until the next deploy. A script entry is stored only when its body IS
// the bytes its ?v= names.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import crypto from 'node:crypto';
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const raw = fs.readFileSync(path.join(here, '..', 'web', 'static', 'sw.js'), 'utf8');
const ORIGIN = 'https://muster.test';
const sha12 = (s) => crypto.createHash('sha256').update(s).digest('hex').slice(0, 12);

// res is a same-origin 200, as a fetch from the worker returns it.
function res(body) {
  const mk = () => ({
    status: 200, type: 'basic', redirected: false,
    clone: mk,
    text: async () => body,
    arrayBuffer: async () => new TextEncoder().encode(body).buffer,
  });
  return mk();
}

// load runs sw.js with its placeholders filled, against caches holding
// `stores` ({cacheName: {url: body}}) and a network answering `net`
// ({url: body}). It returns the handlers and the caches.
function load(shellJS, stores, net) {
  const src = raw
    .replace('"__MUSTER_BUILD__"', '"build-c"')
    .replace('"__MUSTER_APP_CSS__"', '"/static/app.css?v=cccccccccccc"')
    .replace('"__MUSTER_SHELL_JS__"', JSON.stringify(shellJS));
  const caches = new Map(Object.entries(stores).map(([k, v]) => [k, new Map(Object.entries(v).map(([u, b]) => [u, res(b)]))]));
  const handlers = {};
  const cacheAPI = {
    open: async (name) => {
      if (!caches.has(name)) caches.set(name, new Map());
      const m = caches.get(name);
      return { match: async (u) => m.get(u), put: async (u, r) => void m.set(u, r) };
    },
    keys: async () => [...caches.keys()],
    delete: async (name) => caches.delete(name),
  };
  const sandbox = {
    self: { addEventListener: (t, f) => (handlers[t] = f), location: { origin: ORIGIN }, clients: { claim: async () => {} } },
    caches: cacheAPI,
    fetch: async (u) => {
      const key = String(u);
      if (!(key in net)) throw new Error('offline: ' + key);
      return res(net[key]);
    },
    crypto: globalThis.crypto,
    URL, TextEncoder, Uint8Array, Promise, JSON, Object, Array, String, Error, console,
  };
  vm.runInNewContext(src, sandbox);
  return { handlers, caches };
}

async function fire(handlers, type) {
  let p;
  handlers[type]({ waitUntil: (x) => (p = x) });
  await p;
}

const OLD = '(function(){/* build B */})();';
const NEW = '(function(){/* build C */})();';
const SAME = '(function(){/* unchanged */})();';
const changed = `/static/js/app.js?v=${sha12(NEW)}`;
const unchanged = `/static/js/tags.js?v=${sha12(SAME)}`;

test('install stores a script only when its bytes are the ones its URL names', async () => {
  // The rollout: the changed script is answered by the OLD pod.
  const { handlers, caches } = load([changed, unchanged], {}, { [changed]: OLD, [unchanged]: SAME });
  await fire(handlers, 'install');
  const shell = caches.get('muster-shell-build-c');
  assert.ok(shell.has(unchanged), 'control: a script whose bytes match its hash is stored');
  assert.equal(shell.has(changed), false, "the previous build's bytes are not stored under the new build's URL");
});

test('install stores the new bytes when the new pod answers', async () => {
  const { handlers, caches } = load([changed], {}, { [changed]: NEW });
  await fire(handlers, 'install');
  assert.ok(caches.get('muster-shell-build-c').has(changed));
});

test('a script URL with no ?v= hash is never stored', async () => {
  const bare = '/static/js/app.js';
  const { handlers, caches } = load([bare], {}, { [bare]: NEW });
  await fire(handlers, 'install');
  assert.equal(caches.get('muster-shell-build-c').has(bare), false);
});

test('activate carries forward only script entries whose bytes match their URL', async () => {
  // An older cache holds a "/" that links two scripts; one entry holds the
  // wrong bytes for its URL. The new build's own cache is empty (installed
  // while signed out), so both would be carry-forward candidates.
  const doc = `<script src="${changed}"></script><script src="${unchanged}"></script>`;
  const { handlers, caches } = load([], { 'muster-shell-build-b': { '/': doc, [changed]: OLD, [unchanged]: SAME } }, {});
  await fire(handlers, 'activate');
  const shell = caches.get('muster-shell-build-c');
  assert.ok(shell.has('/'), 'control: the shell document is carried');
  assert.ok(shell.has(unchanged), 'control: a matching script entry is carried');
  assert.equal(shell.has(changed), false, 'a mismatched script entry is not carried');
});
