// The service worker's notification tap (web/static/sw.js notificationclick),
// run in node against stubbed clients.
//
//   node --test e2e/sw-notify-url.test.mjs
//
// 🔴 A TAP NEVER STEERS A MUSTER TAB OFF MUSTER. A notification's url is a
// path on this origin; anything else — another origin, a scheme-relative
// "//host", a "/\host" (a browser reads it as "//host"), javascript: or
// data: — opens muster's home instead. An agent's notification that links an
// allowlisted external address (notify_owner) carries muster's own
// /notify/open as its url, marked open: "external": that one opens in a NEW
// window (the server re-checks the address and redirects), never in the open
// muster tab.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const raw = fs.readFileSync(path.join(here, '..', 'web', 'static', 'sw.js'), 'utf8');
const ORIGIN = 'https://muster.test';

// load runs sw.js with one open, controlled muster tab. It returns the
// handlers and what the tab and the window opener were asked to do.
function load() {
  const src = raw
    .replace('"__MUSTER_BUILD__"', '"build-n"')
    .replace('"__MUSTER_APP_CSS__"', '"/static/app.css?v=nnnnnnnnnnnn"')
    .replace('"__MUSTER_SHELL_JS__"', '[]');
  const handlers = {};
  const seen = { navigated: [], opened: [], focused: 0 };
  const tab = {
    id: 'tab-1',
    focus: async () => {
      seen.focused++;
    },
    navigate: async (u) => {
      seen.navigated.push(u);
      return tab;
    },
    postMessage: () => {},
  };
  const sandbox = {
    self: {
      addEventListener: (t, f) => (handlers[t] = f),
      location: { origin: ORIGIN },
      clients: {
        claim: async () => {},
        matchAll: async () => [tab],
        openWindow: async (u) => {
          seen.opened.push(u);
        },
      },
      registration: {},
    },
    caches: { open: async () => ({}), keys: async () => [], delete: async () => true },
    fetch: async () => {
      throw new Error('offline');
    },
    URL, Promise, JSON, Object, Array, String, Error, console, Set, setTimeout, clearTimeout,
    MessageChannel,
  };
  vm.runInNewContext(src, sandbox);
  return { handlers, seen };
}

async function tap(data) {
  const { handlers, seen } = load();
  let p;
  handlers.notificationclick({
    notification: { data, tag: 'notify-x', close: () => {} },
    action: '',
    waitUntil: (x) => (p = x),
  });
  await p;
  return seen;
}

test('a muster path is opened in the open tab', async () => {
  for (const url of ['/tasks/42', '/pages/7/preview?v=2', '/attention?tab=drafts#draft-3']) {
    const seen = await tap({ type: 'task', url });
    assert.deepEqual(seen.navigated, [url], url);
    assert.deepEqual(seen.opened, [], url);
  }
});

test('an absolute URL on this origin is kept as its path', async () => {
  const seen = await tap({ type: 'task', url: ORIGIN + '/tasks/1?x=1#c' });
  assert.deepEqual(seen.navigated, ['/tasks/1?x=1#c']);
});

test('anything that would leave this origin opens muster home instead', async () => {
  for (const url of [
    'https://evil.example/x',
    'http://muster.test.evil.example/',
    '//evil.example/x',
    '/\\evil.example',
    '/\\/evil.example',
    'javascript:alert(1)',
    'JaVaScRiPt:alert(1)',
    'data:text/html,hi',
    ' //evil.example',
    '/tasks\n1',
  ]) {
    const seen = await tap({ type: 'task', url });
    assert.deepEqual(seen.navigated, ['/'], JSON.stringify(url));
    assert.deepEqual(seen.opened, [], JSON.stringify(url));
  }
});

test("an agent's external link opens muster's /notify/open in a NEW window, never the open tab", async () => {
  const url = '/notify/open?to=' + encodeURIComponent('https://docs.example.com/r/9');
  const seen = await tap({ type: 'task', url, open: 'external' });
  assert.deepEqual(seen.opened, [url]);
  assert.deepEqual(seen.navigated, [], 'the open muster tab was not moved');
  assert.equal(seen.focused, 0);
});

test('the external mark on anything but /notify/open is ignored', async () => {
  for (const url of ['https://docs.example.com/r/9', '//evil.example', '/tasks/3']) {
    const seen = await tap({ type: 'task', url, open: 'external' });
    assert.deepEqual(seen.opened, [], JSON.stringify(url));
    assert.equal(seen.navigated.length, 1, JSON.stringify(url));
    assert.ok(seen.navigated[0] === '/' || seen.navigated[0] === '/tasks/3', JSON.stringify(seen.navigated));
  }
});

test('no url opens the notification id fallback on this origin', async () => {
  const seen = await tap({ type: 'task', id: '42' });
  assert.deepEqual(seen.navigated, ['/#42']);
});
