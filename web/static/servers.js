// THE SERVER SWITCHER (docs/design/web-server-switcher.md): the list of muster
// servers this browser knows, drawn in the sidebar, and a switch to another
// one by navigating this window to its origin.
//
// A web page is bound to its origin: it cannot hold another server's sign-in,
// storage or service worker, and it does not try. Each server keeps its own
// list, in its own localStorage, of ORIGINS and the display data its
// GET /api/instance publishes (name, a small logo, the logo's etag). Picking a
// server is a top-level navigation; that server's own cookies decide whether
// you are signed in there.
//
// 🔴 NOTHING OF THIS SERVER IS EVER SENT TO ANOTHER. The only requests to
// another origin are its public /api/instance and /brand/ icon, made with
// FETCH_OPTIONS: no credentials, no referrer, no redirects. The switch's
// navigation carries this server's ORIGIN (in the fragment, which no server
// receives) and nothing else; muster's pages send Referrer-Policy:
// no-referrer, so it carries no Referer either. A typed address is reduced to
// its origin before it is kept, so a path or query (which may hold a token) is
// never stored or fetched, and an address with a user name or password is
// refused outright.
//
// 🔴 THE FRAGMENT IS AN OFFER, NEVER AN ADD. Anyone can link to
// <server>/#muster-from=<origin>; the page asks before adding that origin,
// showing its exact host.
//
// Without a document (node --test e2e/servers.test.mjs) the file publishes its
// pure core as globalThis.musterServersCore and stops.
(function (root) {
  'use strict';

  var KEY = 'muster-servers';
  var VERSION = 1;
  // At most this many servers on one list, this one included: the Android
  // app's palette has eight colours, and so does this one.
  var MAX = 8;
  // A server's branding is read again when the copy is older than this (the
  // Android app reads it every 15 minutes).
  var TTL_MS = 15 * 60 * 1000;
  var MAX_INSTANCE_BYTES = 64 * 1024;
  var MAX_LOGO_BYTES = 256 * 1024;
  var MAX_LABEL = 64;
  var FROM = 'muster-from';
  // The Android app's palette (core/auth Tenants.PALETTE), in its order.
  var PALETTE = ['#7c5cff', '#2bb673', '#ff8a3d', '#3da5ff', '#e84d8a', '#e8c547', '#47d1c9', '#b0b4c0'];
  // The only hosts an http:// server may have (development). Everything else
  // is https.
  var LOOPBACK = { 'localhost': true, '127.0.0.1': true };
  // 🔴 EVERY CROSS-ORIGIN READ USES THESE, AND ONLY THESE.
  var FETCH_OPTIONS = { credentials: 'omit', referrerPolicy: 'no-referrer', redirect: 'error', mode: 'cors' };
  var PNG_SIG = [0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a];
  var DATA_PNG = 'data:image/png;base64,';

  // parseServer turns what a person typed into a server's origin, or says why
  // not: 'empty', 'invalid', 'credentials' (a user name or password in it), or
  // 'scheme' (not https, nor http on a loopback host). A missing scheme is
  // https. Only the origin is returned: a path, query or fragment is dropped.
  function parseServer(input) {
    var s = String(input == null ? '' : input).trim();
    if (!s) return { ok: false, error: 'empty' };
    if (!/^[a-z][a-z0-9+.-]*:\/\//i.test(s)) s = 'https://' + s;
    var u;
    try { u = new URL(s); } catch (e) { return { ok: false, error: 'invalid' }; }
    if (u.username || u.password) return { ok: false, error: 'credentials' };
    if (!u.hostname) return { ok: false, error: 'invalid' };
    if (u.protocol !== 'https:' && !(u.protocol === 'http:' && LOOPBACK[u.hostname])) {
      return { ok: false, error: 'scheme' };
    }
    return { ok: true, origin: u.origin };
  }

  // hostLabel is an origin's fallback label: its host, with the port when it
  // is not the scheme's default (URL.host already leaves that out).
  function hostLabel(origin) {
    try { return new URL(origin).host; } catch (e) { return String(origin); }
  }

  // cleanText strips control, text-direction and tag characters, trims, and
  // keeps at most MAX_LABEL characters. Not a string: ''.
  function cleanText(x) {
    if (typeof x !== 'string') return '';
    var s = x.replace(/[\u0000-\u001f\u007f-\u009f‎‏‪-‮⁦-⁩]/g, '')
      .replace(/\udb40[\udc00-\udc7f]/g, '').trim();
    var cps = Array.from(s);
    return cps.length > MAX_LABEL ? cps.slice(0, MAX_LABEL).join('').trim() : s;
  }

  // fnv1a is a 32-bit FNV-1a over a string's UTF-16 units: the colour's hash.
  function fnv1a(s) {
    var h = 0x811c9dc5;
    for (var i = 0; i < s.length; i++) {
      h ^= s.charCodeAt(i);
      h = Math.imul(h, 0x01000193) >>> 0;
    }
    return h >>> 0;
  }

  // colours gives each origin a palette colour: its hash's slot, or the next
  // free one, taking the origins in sorted order — a function of the SET, so
  // two servers' lists that hold the same servers agree on every colour.
  function colours(origins) {
    var sorted = origins.slice().sort();
    var used = {};
    var out = {};
    for (var i = 0; i < sorted.length; i++) {
      var slot = fnv1a(sorted[i]) % PALETTE.length;
      for (var n = 0; n < PALETTE.length && used[slot]; n++) slot = (slot + 1) % PALETTE.length;
      used[slot] = true;
      out[sorted[i]] = PALETTE[slot];
    }
    return out;
  }

  function validEtag(x) { return typeof x === 'string' && /^[0-9a-f]{1,64}$/.test(x) ? x : ''; }
  function validLogo(x) { return typeof x === 'string' && x.indexOf(DATA_PNG) === 0 && x.length <= DATA_PNG.length + Math.ceil(MAX_LOGO_BYTES / 3) * 4 ? x : ''; }

  // entry is one server as kept: the origin and its display data, nothing else.
  function entry(origin, o) {
    o = o || {};
    return {
      origin: origin,
      label: cleanText(o.label),
      name: cleanText(o.name),
      etag: validEtag(o.etag),
      logo: validLogo(o.logo),
      checkedAt: typeof o.checkedAt === 'number' && isFinite(o.checkedAt) ? o.checkedAt : 0,
    };
  }

  // normalize makes a stored value a valid list: every origin re-checked (one
  // that does not come back from parseServer as itself is dropped), no
  // duplicates, this server (self) first and always present, at most MAX.
  function normalize(stored, self) {
    var list = stored && stored.v === VERSION && Array.isArray(stored.servers) ? stored.servers : [];
    var seen = {};
    var out = [];
    var mine = null;
    for (var i = 0; i < list.length; i++) {
      var s = list[i];
      if (!s || typeof s.origin !== 'string') continue;
      var p = parseServer(s.origin);
      if (!p.ok || p.origin !== s.origin || seen[p.origin]) continue;
      seen[p.origin] = true;
      if (p.origin === self) mine = entry(self, s);
      else out.push(entry(p.origin, s));
    }
    out.unshift(mine || entry(self));
    return out.slice(0, MAX);
  }

  function load(storage, self) {
    var raw = null;
    try { raw = JSON.parse(storage.getItem(KEY) || 'null'); } catch (e) { raw = null; }
    return normalize(raw, self);
  }

  function save(storage, list) {
    try { storage.setItem(KEY, JSON.stringify({ v: VERSION, servers: list })); return true; } catch (e) { return false; }
  }

  // add puts the server typed as input on list: { ok, list, origin } or
  // { ok: false, error } — parseServer's errors, 'duplicate', 'full'.
  function add(list, input) {
    var p = parseServer(input);
    if (!p.ok) return p;
    for (var i = 0; i < list.length; i++) {
      if (list[i].origin === p.origin) return { ok: false, error: 'duplicate', origin: p.origin };
    }
    if (list.length >= MAX) return { ok: false, error: 'full' };
    return { ok: true, origin: p.origin, list: list.concat([entry(p.origin)]) };
  }

  // remove takes origin off list; never this server (self).
  function remove(list, origin, self) {
    if (origin === self) return list;
    return list.filter(function (s) { return s.origin !== origin; });
  }

  // rename sets origin's own label ('' clears it: the name or host shows).
  function rename(list, origin, label) {
    return list.map(function (s) {
      if (s.origin !== origin) return s;
      var c = entry(s.origin, s);
      c.label = cleanText(label);
      return c;
    });
  }

  // display is the label a server is drawn with: the person's own, else the
  // server's name, else its host.
  function display(s) { return s.label || s.name || hostLabel(s.origin); }

  function stale(s, now) { return !s.checkedAt || now - s.checkedAt > TTL_MS || s.checkedAt > now; }

  // logoURL is the icon an instance document names (icon192, else url),
  // resolved against the server's origin — or null when it is not on that
  // origin, or not under /brand/.
  function logoURL(origin, doc) {
    var logo = doc && doc.logo;
    if (!logo || typeof logo !== 'object') return null;
    var ref = typeof logo.icon192 === 'string' ? logo.icon192 : (typeof logo.url === 'string' ? logo.url : '');
    if (!ref) return null;
    var u;
    try { u = new URL(ref, origin + '/'); } catch (e) { return null; }
    if (u.origin !== origin || u.pathname.indexOf('/brand/') !== 0) return null;
    return u.href;
  }

  function isPNG(bytes) {
    if (bytes.length < PNG_SIG.length) return false;
    for (var i = 0; i < PNG_SIG.length; i++) if (bytes[i] !== PNG_SIG[i]) return false;
    return true;
  }

  function base64(bytes) {
    var s = '';
    for (var i = 0; i < bytes.length; i += 0x8000) s += String.fromCharCode.apply(null, bytes.subarray(i, i + 0x8000));
    return root.btoa(s);
  }

  // readBounded reads a response's body, or null past max bytes.
  async function readBounded(r, max) {
    var len = Number(r.headers && r.headers.get && r.headers.get('content-length'));
    if (len > max) return null;
    var buf = new Uint8Array(await r.arrayBuffer());
    return buf.length > max ? null : buf;
  }

  // refresh reads s's branding from its own origin with fetchFn (window.fetch
  // in the page) and answers the updated entry. The logo is fetched only when
  // the document's etag differs from the cached one (or none is cached). Any
  // failure keeps what was cached, and stamps checkedAt so the next try waits
  // for the TTL.
  async function refresh(s, fetchFn, now) {
    var next = entry(s.origin, s);
    next.checkedAt = now;
    try {
      var r = await fetchFn(s.origin + '/api/instance', FETCH_OPTIONS);
      if (!r || !r.ok) return next;
      var body = await readBounded(r, MAX_INSTANCE_BYTES);
      if (!body) return next;
      var doc = JSON.parse(new TextDecoder().decode(body));
      if (!doc || typeof doc !== 'object') return next;
      next.name = cleanText(doc.name);
      var etag = validEtag(doc.logo && doc.logo.etag);
      if (!etag) { next.etag = ''; next.logo = ''; return next; }
      if (etag === s.etag && validLogo(s.logo)) return next;
      var url = logoURL(s.origin, doc);
      if (!url) { next.etag = ''; next.logo = ''; return next; }
      var ri = await fetchFn(url, FETCH_OPTIONS);
      if (!ri || !ri.ok) return next;
      var bytes = await readBounded(ri, MAX_LOGO_BYTES);
      if (!bytes || !isPNG(bytes)) return next;
      next.logo = DATA_PNG + base64(bytes);
      next.etag = etag;
      return next;
    } catch (e) {
      return next;
    }
  }

  // switchHref is where picking target sends this window: target's root,
  // carrying this server's origin (self) in the fragment for the offer.
  function switchHref(target, self) { return target + '/#' + FROM + '=' + encodeURIComponent(self); }

  // handoff reads the offer from a location hash: the origin it names (valid,
  // canonical, not self), or null.
  function handoff(hash, self) {
    var m = /^#muster-from=([^&#]*)$/.exec(String(hash || ''));
    if (!m) return null;
    var v;
    try { v = decodeURIComponent(m[1]); } catch (e) { return null; }
    var p = parseServer(v);
    if (!p.ok || p.origin !== v || p.origin === self) return null;
    return p.origin;
  }

  var core = {
    KEY: KEY, MAX: MAX, TTL_MS: TTL_MS, PALETTE: PALETTE, FETCH_OPTIONS: FETCH_OPTIONS, MAX_LOGO_BYTES: MAX_LOGO_BYTES,
    parseServer: parseServer, hostLabel: hostLabel, cleanText: cleanText, colours: colours, normalize: normalize,
    load: load, save: save, add: add, remove: remove, rename: rename, display: display, stale: stale,
    logoURL: logoURL, refresh: refresh, switchHref: switchHref, handoff: handoff,
  };
  root.musterServersCore = core;
  if (typeof document === 'undefined') return;

  // ---- the page ---------------------------------------------------------------

  var self = location.origin;
  var ERRORS = {
    empty: 'Enter the server’s address.',
    invalid: 'That is not a web address.',
    credentials: 'Leave the user name and password out of the address.',
    scheme: 'The address must start with https:// (http:// only for localhost).',
    duplicate: 'That server is already on the list.',
    full: 'The list is full (' + MAX + ' servers). Remove one first.',
  };

  function storage() { try { return root.localStorage; } catch (e) { return null; } }
  function current() {
    var st = storage();
    return st ? load(st, self) : normalize(null, self);
  }
  function persist(list) { var st = storage(); if (st) save(st, list); }
  function $(id) { return document.getElementById(id); }

  // fill draws one server into a cloned template's [data-server-*] slots.
  function fill(node, s, colour, box) {
    var dot = node.querySelector('[data-server-dot]');
    if (dot) dot.style.backgroundColor = colour;
    var label = node.querySelector('[data-server-label]');
    if (label) label.textContent = display(s);
    var host = node.querySelector('[data-server-host]');
    if (host) host.textContent = hostLabel(s.origin);
    var img = node.querySelector('[data-server-logo]');
    if (img) {
      var src = s.origin === self ? (box.getAttribute('data-self-logo') || '') : s.logo;
      if (src) { img.src = src; img.hidden = false; } else { img.removeAttribute('src'); img.hidden = true; }
    }
  }

  function render() {
    var box = $('server-switcher');
    if (!box) return;
    var list = current();
    var mine = list[0];
    if (!mine.name) mine.name = cleanText(box.getAttribute('data-self-name') || '');
    var cols = colours(list.map(function (s) { return s.origin; }));
    fill(box.querySelector('#server-switcher-toggle'), mine, cols[self], box);

    var ul = $('server-switcher-list');
    var tpl = $('server-item-tpl');
    if (ul && tpl) {
      ul.textContent = '';
      list.forEach(function (s) {
        var li = tpl.content.firstElementChild.cloneNode(true);
        var a = li.querySelector('a');
        fill(li, s, cols[s.origin], box);
        a.setAttribute('data-server-origin', s.origin);
        if (s.origin === self) {
          a.setAttribute('href', '/');
          a.setAttribute('aria-current', 'true');
          var tag = li.querySelector('[data-server-current]');
          if (tag) tag.hidden = false;
        } else {
          a.setAttribute('href', switchHref(s.origin, self));
        }
        ul.appendChild(li);
      });
    }
    var add = $('server-add-open');
    if (add) add.disabled = list.length >= MAX;

    var rows = $('server-manage-list');
    var rtpl = $('server-manage-row-tpl');
    if (rows && rtpl && !(rows.contains(document.activeElement) && document.activeElement.tagName === 'INPUT')) {
      rows.textContent = '';
      list.forEach(function (s) {
        var li = rtpl.content.firstElementChild.cloneNode(true);
        fill(li, s, cols[s.origin], box);
        li.setAttribute('data-server-origin', s.origin);
        var input = li.querySelector('input');
        input.value = s.label;
        input.placeholder = s.name || hostLabel(s.origin);
        input.setAttribute('aria-label', 'Label for ' + hostLabel(s.origin));
        var rm = li.querySelector('[data-server-remove]');
        if (s.origin === self) {
          rm.hidden = true;
          var tag = li.querySelector('[data-server-current]');
          if (tag) tag.hidden = false;
        } else {
          rm.setAttribute('aria-label', 'Remove ' + display(s));
        }
        rows.appendChild(li);
      });
    }
  }

  // refreshStale reads every stale server's branding (never this one: its
  // own page already draws it), one at a time, and redraws after each.
  var refreshing = false;
  async function refreshStale() {
    if (refreshing || typeof root.fetch !== 'function') return;
    refreshing = true;
    try {
      var list = current();
      for (var i = 0; i < list.length; i++) {
        var s = list[i];
        if (s.origin === self || !stale(s, Date.now())) continue;
        var next = await refresh(s, root.fetch.bind(root), Date.now());
        // Merge into what is stored NOW: the person may have renamed or
        // removed it meanwhile.
        var now = current();
        var changed = false;
        now = now.map(function (x) {
          if (x.origin !== next.origin) return x;
          changed = true;
          var m = entry(x.origin, next);
          m.label = x.label;
          return m;
        });
        if (changed) { persist(now); render(); }
      }
    } finally {
      refreshing = false;
    }
  }

  function setOpen(open) {
    var t = $('server-switcher-toggle');
    var menu = $('server-switcher-menu');
    if (!t || !menu) return;
    menu.hidden = !open;
    t.setAttribute('aria-expanded', open ? 'true' : 'false');
    if (open) refreshStale();
  }

  function showError(el, code) {
    if (!el) return;
    el.textContent = code ? (ERRORS[code] || ERRORS.invalid) : '';
    el.hidden = !code;
  }

  function bind() {
    var box = $('server-switcher');
    if (!box || box.__musterServersBound) return;
    box.__musterServersBound = true;
    var t = $('server-switcher-toggle');
    t.addEventListener('click', function () { setOpen(t.getAttribute('aria-expanded') !== 'true'); });

    var addDlg = $('server-add');
    var addInput = $('server-add-input');
    var addErr = $('server-add-error');
    $('server-add-open').addEventListener('click', function () {
      addInput.value = '';
      showError(addErr, '');
      addInput.removeAttribute('aria-invalid');
      addDlg.showModal();
      addInput.focus();
    });
    addInput.addEventListener('keydown', function (e) {
      if (e.key === 'Enter') { e.preventDefault(); submitAdd(); }
    });
    $('server-add-submit').addEventListener('click', submitAdd);
    function submitAdd() {
      var r = add(current(), addInput.value);
      if (!r.ok) {
        showError(addErr, r.error);
        addInput.setAttribute('aria-invalid', 'true');
        addInput.focus();
        return;
      }
      persist(r.list);
      addDlg.close();
      render();
      refreshStale();
      t.focus();
    }
    ['server-add', 'server-manage', 'server-offer'].forEach(function (id) {
      var d = $(id);
      if (d) d.querySelectorAll('[data-dialog-close]').forEach(function (b) {
        b.addEventListener('click', function () { d.close(); });
      });
    });

    var manDlg = $('server-manage');
    $('server-manage-open').addEventListener('click', function () { render(); manDlg.showModal(); });
    manDlg.addEventListener('close', function () { render(); t.focus(); });
    var rows = $('server-manage-list');
    rows.addEventListener('change', function (e) {
      var input = e.target.closest('input');
      var li = e.target.closest('[data-server-origin]');
      if (!input || !li) return;
      persist(rename(current(), li.getAttribute('data-server-origin'), input.value));
      input.blur();
      render();
    });
    rows.addEventListener('keydown', function (e) {
      if (e.key === 'Enter' && e.target.tagName === 'INPUT') { e.preventDefault(); e.target.dispatchEvent(new Event('change', { bubbles: true })); }
    });
    rows.addEventListener('click', function (e) {
      var b = e.target.closest('[data-server-remove]');
      var li = e.target.closest('[data-server-origin]');
      if (!b || !li) return;
      persist(remove(current(), li.getAttribute('data-server-origin'), self));
      render();
      var first = rows.querySelector('input');
      if (first) first.focus();
    });

    var offer = $('server-offer');
    if (offer) {
      $('server-offer-add').addEventListener('click', function () {
        var o = offer.getAttribute('data-origin');
        var r = add(current(), o);
        if (r.ok) { persist(r.list); render(); refreshStale(); }
        offer.close();
      });
    }
  }

  // The fragment hand-off: a switch from another server names it; offer to
  // put it on this list (never add it unasked). The fragment is removed from
  // the address bar either way.
  function offerHandoff() {
    var from = handoff(location.hash, self);
    if (!/^#muster-from=/.test(location.hash)) return;
    try { history.replaceState(history.state, '', location.pathname + location.search); } catch (e) { /* keep going */ }
    var offer = $('server-offer');
    if (!from || !offer) return;
    var list = current();
    if (list.some(function (s) { return s.origin === from; }) || list.length >= MAX) return;
    offer.setAttribute('data-origin', from);
    var host = offer.querySelector('[data-server-host]');
    if (host) host.textContent = hostLabel(from);
    offer.showModal();
  }

  // Once per window: Escape and a click outside close the menu (focus back
  // on its button), and another tab's change to the list redraws this one.
  if (!root.__musterServersInit) {
    root.__musterServersInit = true;
    document.addEventListener('keydown', function (e) {
      var t = $('server-switcher-toggle');
      if (e.key !== 'Escape' || !t || t.getAttribute('aria-expanded') !== 'true') return;
      if (document.querySelector('#server-add[open], #server-manage[open], #server-offer[open]')) return;
      setOpen(false);
      t.focus();
    });
    document.addEventListener('click', function (e) {
      var box = $('server-switcher');
      var t = $('server-switcher-toggle');
      if (!box || !t || t.getAttribute('aria-expanded') !== 'true') return;
      // The dialogs live outside the row's box (in the sidebar's root), and a
      // click in one is not a click away from the menu.
      if (box.contains(e.target) || (e.target.closest && e.target.closest('#server-add, #server-manage, #server-offer'))) return;
      setOpen(false);
    });
    root.addEventListener('storage', function (e) { if (e.key === KEY) render(); });
  }

  bind();
  render();
  offerHandoff();
})(typeof globalThis !== 'undefined' ? globalThis : this);
