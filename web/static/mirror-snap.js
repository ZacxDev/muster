// THE MIRRORED VIEW'S SNAPSHOT: a per-user copy, in this browser, of what a
// mirrored window's Rendered view drew (the socket's batches) and where its
// socket stood (gen + offset), so the next open paints at once and RESUMES from
// that offset instead of waiting for the host to replay the transcript.
//
// What is stored is the socket's own batches as the server sent them — the
// HTML the page already shows (the host redacted it before it was framed) — and
// never the live DOM: the page replays them through its own apply(), the same
// path a live batch takes, so the scripts that decorate the conversation (tool
// groups, bubble clamps, the message index) see exactly what they see live.
//
// 🔴 PER USER, AND CHECKED, NOT JUST KEYED. The key carries the signed-in user
// and the build (both server-rendered on this script's tag), and a read
// compares the record's own user against the page's again: a record that is
// another user's, another build's, or older than TTL_MS is deleted, never
// painted. Every page load also sweeps such records away.
//
// 🔴 PURGED ON SIGN-OUT: every server-written auth page deletes the database
// (/login among them — internal/api login.go writeAuthDocument). Both sign-out
// routes answer with a redirect to /login, and nobody signs in again without
// passing through it, so it is the one deterministic place; the sign-out
// button itself lives on /account, which does not load this file.
//
// 🔴 A SNAPSHOT IS PROVISIONAL UNTIL THE SERVER SPEAKS. The view paints it and
// resumes from its offset; a socket that closes, errors or is refused (a 401,
// a 404) before its first batch DISCARDS the snapshot — removed from the page
// and from storage — rather than leaving it on screen as if it were current.
//
// WARMING: on the Agents list and on agent pages, the likely-next windows
// (pinned, recently viewed, and this browser's own recent snapshots; at most
// WARM_MAX) are refreshed in an idle callback by opening their transcript
// SOCKET — never their page. The socket (GET /mirror/view/ws) records no visit
// and marks nothing read; only the page's own GET does (recordMirrorVisit,
// internal/api mirror_view.go), so a warmed window does not move in Recent.
// Warming stops on a hidden tab and under Save-Data / 2G, and is bounded per
// window (bytes, time) and per pass.
//
// Every storage access is wrapped: with IndexedDB missing, blocked or throwing,
// the view connects exactly as it did before this file existed.
(function (root) {
  'use strict';

  var DB = 'muster-snap';
  var STORE = 'mirror';
  var DB_VERSION = 1;
  // How long a snapshot may be painted from. Past it the record is ignored
  // (and deleted): the conversation has likely moved on, and a short TTL bounds
  // how stale a first paint can ever be.
  var TTL_MS = 5 * 60 * 1000;
  // One snapshot's batches, as JSON characters: past it the OLDEST batches are
  // dropped (the newest part of the conversation is what an open shows).
  var MAX_BYTES = 2 * 1024 * 1024;
  // Snapshots kept in all; the least recently written go first.
  var MAX_ENTRIES = 12;
  // How long an open waits for storage before connecting without it.
  var READ_WAIT_MS = 400;

  var WARM_MAX = 5;
  var WARM_DELAY_MS = 1500;
  var WARM_EVERY_MS = 2 * 60 * 1000;
  // A snapshot younger than this is fresh enough: not warmed again.
  var WARM_FRESH_MS = TTL_MS / 2;
  // One window's warm socket ends after this long without a batch, or this
  // long in all.
  var WARM_QUIET_MS = 600;
  var WARM_CAP_MS = 5000;
  // Bytes one warm pass may read, across every window it warms.
  var WARM_PASS_BYTES = 3 * 1024 * 1024;

  // refOf is the mirrored window a rendered view's page or socket path names
  // ({host, session, window}), or null for any other path. Decoded rather than
  // compared as text, so a card's link and the page's socket agree however
  // each was encoded.
  function refOf(path) {
    if (typeof path !== 'string') return null;
    var u;
    try { u = new URL(path, 'http://x.invalid'); } catch (e) { return null; }
    if (u.pathname !== '/mirror/view' && u.pathname !== '/mirror/view/ws') return null;
    var host = u.searchParams.get('host') || '', session = u.searchParams.get('session') || '';
    if (!host || !session) return null;
    return { host: host, session: session, window: u.searchParams.get('window') || '' };
  }

  function keyOf(user, build, ref) {
    return JSON.stringify([String(user), String(build), ref.host, ref.session, ref.window || '']);
  }

  function refKey(ref) { return JSON.stringify([ref.host, ref.session, ref.window || '']); }

  // usable says whether rec may be painted for this user and build at now:
  // 'ok', or why not.
  function usable(rec, user, build, now) {
    if (!rec || typeof rec !== 'object') return 'missing';
    if (String(rec.user) !== String(user)) return 'foreign';
    if (String(rec.build) !== String(build)) return 'build';
    if (typeof rec.at !== 'number' || now - rec.at > TTL_MS || rec.at - now > 60000) return 'expired';
    if (!rec.gen || !Array.isArray(rec.batches) || rec.batches.length === 0) return 'empty';
    return 'ok';
  }

  // bound keeps the newest batches whose JSON fits in max characters. A batch
  // that starts the conversation over (reset) makes everything before it
  // irrelevant, so only it and what follows are kept.
  function bound(batches, max) {
    var start = 0;
    for (var i = batches.length - 1; i >= 0; i--) {
      if (batches[i] && batches[i].reset) { start = i; break; }
    }
    var kept = [], bytes = 0;
    for (var j = batches.length - 1; j >= start; j--) {
      var n = JSON.stringify(batches[j]).length;
      if (bytes + n > max) break;
      kept.push(batches[j]);
      bytes += n;
    }
    kept.reverse();
    return { batches: kept, bytes: bytes };
  }

  // worth is whether a batch changes what the page draws: busy-only markers
  // and an accepted resume's empty marker are not kept (the record's gen and
  // offset say where the socket stood).
  function worth(m) {
    return !!(m.reset || (m.ops && m.ops.length) || m.index || m.context);
  }

  var core = {
    refOf: refOf, keyOf: keyOf, refKey: refKey, usable: usable, bound: bound, worth: worth,
    TTL_MS: TTL_MS, MAX_BYTES: MAX_BYTES, MAX_ENTRIES: MAX_ENTRIES, WARM_MAX: WARM_MAX,
  };
  root.musterSnapCore = core;
  if (typeof document === 'undefined') return; // loaded by a unit test

  var me = document.currentScript;
  var user = (me && me.getAttribute('data-snap-user')) || '';
  var build = (me && me.getAttribute('data-snap-build')) || '';
  var idb = null;
  try { idb = root.indexedDB || null; } catch (e) { idb = null; }
  var enabled = !!(idb && user && user !== '0' && build);

  var dbPromise = null;
  function open() {
    if (!enabled) return Promise.resolve(null);
    if (dbPromise) return dbPromise;
    dbPromise = new Promise(function (resolve) {
      try {
        var req = idb.open(DB, DB_VERSION);
        req.onupgradeneeded = function () {
          try { req.result.createObjectStore(STORE, { keyPath: 'k' }); } catch (e) {}
        };
        req.onsuccess = function () {
          var db = req.result;
          // A database someone else created at this version without the store
          // (never this file) is unusable: remove it, and the next open builds it.
          if (!db.objectStoreNames.contains(STORE)) {
            try { db.close(); idb.deleteDatabase(DB); } catch (e) {}
            dbPromise = null;
            resolve(null);
            return;
          }
          // A purge (an auth page in another tab) deletes the database: let it.
          db.onversionchange = function () { try { db.close(); } catch (e) {} dbPromise = null; };
          resolve(db);
        };
        req.onerror = function () { dbPromise = null; resolve(null); };
        req.onblocked = function () { dbPromise = null; resolve(null); };
      } catch (e) { dbPromise = null; resolve(null); }
    });
    return dbPromise;
  }

  // tx runs fn(store) in one transaction and resolves with fn's request's
  // result (or null on any failure).
  function tx(mode, fn) {
    return open().then(function (db) {
      if (!db) return null;
      return new Promise(function (resolve) {
        try {
          var t = db.transaction(STORE, mode), st = t.objectStore(STORE), req = fn(st), out = null;
          if (req) req.onsuccess = function () { out = req.result; };
          t.oncomplete = function () { resolve(out === undefined ? null : out); };
          t.onerror = t.onabort = function () { resolve(null); };
        } catch (e) { resolve(null); }
      });
    }, function () { return null; });
  }

  function all() {
    return tx('readonly', function (st) { return st.getAll(); }).then(function (rs) { return Array.isArray(rs) ? rs : []; });
  }

  function del(k) { return tx('readwrite', function (st) { st.delete(k); return null; }); }

  // get is ref's snapshot when it may be painted, else null (and a record
  // that may not be is deleted).
  function get(ref) {
    if (!enabled || !ref) return Promise.resolve(null);
    var k = keyOf(user, build, ref);
    return tx('readonly', function (st) { return st.get(k); }).then(function (rec) {
      if (!rec) return null;
      if (rec.k !== k || usable(rec, user, build, Date.now()) !== 'ok') { del(rec.k); return null; }
      return rec;
    }, function () { return null; });
  }

  // put stores ref's snapshot: the batches since the conversation's last
  // reset, bounded, and where the socket stood.
  function put(ref, s) {
    if (!enabled || !ref || !s || !s.gen) return Promise.resolve(null);
    var b = bound(s.batches || [], MAX_BYTES);
    if (b.batches.length === 0) return Promise.resolve(null);
    var rec = { k: keyOf(user, build, ref), user: String(user), build: String(build), host: ref.host, session: ref.session,
      window: ref.window || '', at: Date.now(), gen: s.gen, offset: s.offset || 0, batches: b.batches, bytes: b.bytes };
    return tx('readwrite', function (st) { st.put(rec); return null; }).then(evict);
  }

  // evict keeps MAX_ENTRIES snapshots, and none that may not be painted.
  function evict() {
    return all().then(function (rs) {
      var now = Date.now(), keep = [];
      rs.forEach(function (r) {
        if (!r || usable(r, user, build, now) !== 'ok' || r.k !== keyOf(user, build, r)) { if (r) del(r.k); } else keep.push(r);
      });
      keep.sort(function (a, b) { return b.at - a.at; });
      keep.slice(MAX_ENTRIES).forEach(function (r) { del(r.k); });
    });
  }

  function drop(ref) {
    if (!enabled || !ref) return Promise.resolve(null);
    return del(keyOf(user, build, ref));
  }

  // recent is this browser's snapshots for this user, newest first.
  function recent() {
    return all().then(function (rs) {
      var now = Date.now();
      return rs.filter(function (r) { return usable(r, user, build, now) === 'ok'; }).sort(function (a, b) { return b.at - a.at; });
    });
  }

  // wsURL is a WARM socket's address: the view's own socket, marked warm=1.
  // The server needs no mark to keep it side-effect free (no view socket
  // records a visit or marks anything read; only the page does), so the mark
  // is for whoever reads the request — a log line, a test — to tell warming
  // from a page.
  function wsURL(ref, from) {
    var q = new URLSearchParams({ host: ref.host, session: ref.session });
    if (ref.window) q.set('window', ref.window);
    q.set('warm', '1');
    var url = (location.protocol === 'https:' ? 'wss:' : 'ws:') + '//' + location.host + '/mirror/view/ws?' + q.toString();
    if (from && from.gen && from.offset > 0) url += '&gen=' + encodeURIComponent(from.gen) + '&offset=' + from.offset;
    return url;
  }

  // --- warming --------------------------------------------------------------

  var warming = false, lastWarm = 0, current = null;

  function frugal() {
    try {
      var c = navigator.connection;
      return !!(c && (c.saveData || /(^|-)2g$/.test(c.effectiveType || '')));
    } catch (e) { return false; }
  }

  function framed() {
    try { return root.top !== root; } catch (e) { return true; }
  }

  // warmable: this page warms (the Agents list or an agent page), and now.
  function warmable() {
    if (!enabled || document.hidden || frugal() || framed()) return false;
    var p = location.pathname;
    return p === '/' || p === '/agents' || p.indexOf('/agents/') === 0 || p === '/mirror/view';
  }

  // here is the window this page shows, when it is a rendered view.
  function here() {
    var log = document.getElementById('mv-log');
    return log ? refOf(log.getAttribute('data-ws')) : null;
  }

  // candidates is the likely-next windows, in order: pinned, then recently
  // viewed (as the page lists them), then any entry a page marks
  // data-snap-warm, then this browser's own recent snapshots; at most WARM_MAX,
  // never the window on screen.
  function candidates() {
    var seen = {}, out = [], self = here();
    if (self) seen[refKey(self)] = true;
    function add(ref) {
      if (!ref) return;
      var k = refKey(ref);
      if (seen[k]) return;
      seen[k] = true;
      out.push(ref);
    }
    try {
      document.querySelectorAll('a[data-pinned-chip-link], a[data-recent-entry="mirror"], a[data-snap-warm]').forEach(function (a) {
        add(refOf(a.getAttribute('href')));
      });
    } catch (e) {}
    return recent().then(function (rs) {
      rs.forEach(function (r) { add({ host: r.host, session: r.session, window: r.window }); });
      return out.slice(0, WARM_MAX);
    });
  }

  // warmOne refreshes ref's snapshot through its transcript socket: resuming
  // from prev when there is one, else from the start. budget bounds the bytes
  // read. It resolves with the bytes it read.
  function warmOne(ref, prev, budget) {
    return new Promise(function (resolve) {
      var got = prev ? prev.batches.slice() : [], gen = prev ? prev.gen : '', off = prev ? prev.offset : 0;
      var n = 0, bytes = 0, quiet = 0, cap = 0, done = false, sock;
      function finish(ok) {
        if (done) return;
        done = true;
        current = null;
        clearTimeout(quiet);
        clearTimeout(cap);
        try { sock.close(); } catch (e) {}
        if (ok && n > 0 && gen) {
          put(ref, { gen: gen, offset: off, batches: got }).then(function () { resolve(bytes); }, function () { resolve(bytes); });
          return;
        }
        // Refused, or closed before it said anything: a snapshot it was
        // resuming may not be painted again.
        if (prev) drop(ref);
        resolve(bytes);
      }
      try { sock = new WebSocket(wsURL(ref, prev)); } catch (e) { resolve(0); return; }
      current = { stop: function () { finish(n > 0); } };
      cap = setTimeout(function () { finish(n > 0); }, WARM_CAP_MS);
      sock.onmessage = function (ev) {
        bytes += (ev.data && ev.data.length) || 0;
        var m;
        try { m = JSON.parse(ev.data); } catch (e) { return; }
        if (m.type === 'error') { finish(false); return; }
        if (m.type !== 'batch') return;
        n++;
        if (m.reset) got = [];
        if (m.gen) gen = m.gen;
        if (typeof m.offset === 'number') off = m.offset;
        if (worth(m)) got.push(m);
        clearTimeout(quiet);
        quiet = setTimeout(function () { finish(true); }, WARM_QUIET_MS);
        if (bytes > budget) finish(true);
      };
      sock.onclose = function () { finish(n > 0); };
    });
  }

  function warmPass() {
    if (warming || !warmable()) return Promise.resolve();
    warming = true;
    lastWarm = Date.now();
    var left = WARM_PASS_BYTES;
    return candidates().then(function (list) {
      var i = 0;
      function next() {
        if (i >= list.length || left <= 0 || !warmable()) return null;
        var ref = list[i++];
        return get(ref).then(function (prev) {
          if (prev && Date.now() - prev.at < WARM_FRESH_MS) return next();
          return warmOne(ref, prev, left).then(function (used) { left -= used; return next(); });
        });
      }
      return next();
    }).then(function () { warming = false; }, function () { warming = false; });
  }

  function idle(fn) {
    if (typeof root.requestIdleCallback === 'function') root.requestIdleCallback(function () { fn(); }, { timeout: 3000 });
    else setTimeout(fn, 200);
  }

  // --- wiring ---------------------------------------------------------------

  if (enabled) {
    // Another user's, another build's and expired snapshots go at every load.
    setTimeout(function () { evict(); }, 0);
    var arm = function () {
      setTimeout(function () { idle(warmPass); }, WARM_DELAY_MS);
      setInterval(function () { if (Date.now() - lastWarm >= WARM_EVERY_MS) idle(warmPass); }, 30000);
    };
    if (document.readyState === 'complete') arm(); else root.addEventListener('load', arm);
    document.addEventListener('visibilitychange', function () {
      if (document.hidden) { if (current) current.stop(); return; }
      if (Date.now() - lastWarm >= WARM_EVERY_MS) setTimeout(function () { idle(warmPass); }, 500);
    });
  }

  root.musterSnap = {
    enabled: enabled, refOf: refOf, get: get, put: put, drop: drop,
    worth: worth, readWaitMs: READ_WAIT_MS,
  };
})(typeof window !== 'undefined' ? window : globalThis);
