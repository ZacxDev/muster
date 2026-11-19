// THE SERVER SWITCHER (docs/design/web-server-switcher.md): the list of muster
// servers this browser knows, drawn in the sidebar, and a switch to another
// one by navigating this window to its origin.
//
// A web page is bound to its origin: it cannot hold another server's sign-in,
// storage or service worker, and it does not try. Each server keeps its own
// list, in its own localStorage, of ORIGINS and the person's own label for
// each, nothing else. Picking a server is a top-level navigation; that
// server's own cookies decide whether you are signed in there.
//
// 🔴 THIS SCRIPT READS NOTHING FROM ANOTHER ORIGIN (the operator's decision
// on review, "Logo via <img>, no CORS"): no fetch, no XHR. Another server's
// logo is an <img> pointing at its public /brand/icon-192.png (logoSrc), which
// the browser DRAWS but no script can read; the page's CSP img-src allows that
// one path on another origin and nothing else (internal/api middleware.go).
// The image request carries no Referer (referrerpolicy="no-referrer" on the
// tag, and muster's pages send Referrer-Policy: no-referrer) and is not a CORS
// request (no crossorigin attribute). A server with no logo answers 404 and
// its entry shows its monogram and name. Logos are requested only while the list
// is on screen, never on a page load with the menu shut.
//
// 🔴 NOTHING OF THIS SERVER IS EVER SENT TO ANOTHER. The switch's navigation
// carries this server's ORIGIN (in the fragment, which no server receives) and
// nothing else. A typed address is reduced to its origin before it is kept,
// so a path or query (which may hold a token) is never stored or requested,
// and an address with a user name or password is refused outright.
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
  var MAX_LABEL = 64;
  var FROM = 'muster-from';
  // The Android app's palette (core/auth Tenants.PALETTE), in its order.
  var PALETTE = ['#7c5cff', '#2bb673', '#ff8a3d', '#3da5ff', '#e84d8a', '#e8c547', '#47d1c9', '#b0b4c0'];
  // The only hosts an http:// server may have (development). Everything else
  // is https.
  var LOOPBACK = { 'localhost': true, '127.0.0.1': true };
  // Another server's logo: its current 192 px icon, at the unversioned path
  // every muster serves (internal/api branding.go: no-cache + ETag, 404 when no
  // logo is set). The ONE cross-origin path the CSP's img-src allows.
  var LOGO_PATH = '/brand/icon-192.png';

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

  // SELF_FALLBACK is this server's name when neither the person nor its
  // branding gave it one.
  var SELF_FALLBACK = 'This server';

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

  // monogram is a server's picture when it has no logo: the first letter or
  // digit of each of the first two words of its name (a word is a run of
  // letters and digits), each upper-cased on its own (kept as it was when its
  // upper case is more than one character, as Go's unicode.ToUpper does). No
  // letter or digit at all: '?'. The same rule as ui server_switcher.go
  // serverMonogram, which draws the row before this script runs; both are
  // held to one table of literal cases.
  function monogram(name) {
    var words = String(name == null ? '' : name).split(/[^\p{L}\p{N}]+/u).filter(Boolean);
    if (!words.length) return '?';
    return words.slice(0, 2).map(function (w) {
      var c = Array.from(w)[0];
      var u = c.toUpperCase();
      return Array.from(u).length === 1 ? u : c;
    }).join('');
  }

  // monogramColour is a monogram's background: the palette colour its name's
  // FNV-1a hash picks (ui serverMonogramColour), so a name is always the same
  // colour.
  function monogramColour(name) { return PALETTE[fnv1a(String(name == null ? '' : name)) % PALETTE.length]; }

  // TOGGLE_LABEL is the row button's accessible name before the server's name
  // (ui ServerSwitcherLabel).
  var TOGGLE_LABEL = 'Switch server, current: ';

  // collisions is the set of names (lower-cased) that more than one of names
  // has: the entries the list must tell apart by their address.
  function collisions(names) {
    var seen = {};
    var out = {};
    names.forEach(function (n) {
      var k = String(n).toLowerCase();
      if (seen[k]) out[k] = true;
      seen[k] = true;
    });
    return out;
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

  // entry is one server as kept: its origin and the person's label, nothing
  // else (a stored entry's other fields are dropped).
  function entry(origin, o) {
    o = o || {};
    return { origin: origin, label: cleanText(o.label) };
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
      // This server's own entry is matched BEFORE the address rules: a
      // server reached over plain http on a LAN address is not one you could
      // ADD (parseServer refuses it), but it is the one you are on, and its
      // label must survive a reload.
      if (s.origin === self) {
        if (!mine) mine = entry(self, s);
        continue;
      }
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

  // rename sets origin's own label ('' clears it: the host shows).
  function rename(list, origin, label) {
    return list.map(function (s) {
      if (s.origin !== origin) return s;
      var c = entry(s.origin, s);
      c.label = cleanText(label);
      return c;
    });
  }

  // display is the label a server is drawn with: the person's own, else
  // name (this server's own display name, which its page already has), else
  // its host. Another server's name is never read.
  function display(s, name) { return s.label || name || hostLabel(s.origin); }

  // logoSrc is the <img> src for origin's logo: that origin's fixed icon path,
  // nothing of this page in it.
  function logoSrc(origin) { return origin + LOGO_PATH; }

  // switchHref is where picking target sends this window: target's root,
  // carrying this server's origin (self) in the fragment for the offer.
  function switchHref(target, self) { return target + '/#' + FROM + '=' + encodeURIComponent(self); }

  // handoff reads the offer from a location hash: the origin it names (valid,
  // canonical, not self), or null.
  // HANDOFF_KEY is where the sign-in page keeps a #muster-from fragment
  // (sessionStorage, this tab only) across the sign-in, which drops it: an
  // unauthenticated switch lands on /login with the fragment, and the
  // post-sign-in redirect has none (internal/api writeAuthDocument,
  // ui.ServerHandoffKeepHTML). Kept at most HANDOFF_MAX_AGE_MS.
  var HANDOFF_KEY = 'muster-from';
  var HANDOFF_MAX_AGE_MS = 10 * 60 * 1000;

  // kept reads a stored hand-off ("<ms>|<hash>"): the hash, when it was kept
  // at most HANDOFF_MAX_AGE_MS before now; else null.
  function kept(value, now) {
    var m = /^(\d+)\|(#muster-from=.*)$/.exec(String(value == null ? '' : value));
    if (!m) return null;
    var age = now - Number(m[1]);
    return age >= 0 && age <= HANDOFF_MAX_AGE_MS ? m[2] : null;
  }

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
    KEY: KEY, MAX: MAX, PALETTE: PALETTE, LOGO_PATH: LOGO_PATH,
    parseServer: parseServer, hostLabel: hostLabel, cleanText: cleanText, colours: colours, normalize: normalize,
    load: load, save: save, add: add, remove: remove, rename: rename, display: display, logoSrc: logoSrc,
    switchHref: switchHref, handoff: handoff, kept: kept, HANDOFF_KEY: HANDOFF_KEY, HANDOFF_MAX_AGE_MS: HANDOFF_MAX_AGE_MS,
    monogram: monogram, monogramColour: monogramColour, collisions: collisions, TOGGLE_LABEL: TOGGLE_LABEL,
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

  // name is the words a server is drawn and announced with. This server is
  // never called by its address (no host:port in the top layer): with no
  // label and no branding name it is SELF_FALLBACK, the words the server drew
  // it with (ui server_switcher.go serverSwitcherName).
  function name(s, box) { return display(s, s.origin === self ? (selfName(box) || SELF_FALLBACK) : ''); }

  // paintMonogram draws name's monogram into its slot, if node has one.
  function paintMonogram(node, n) {
    var mono = node.querySelector('[data-server-monogram]');
    if (!mono) return null;
    mono.textContent = monogram(n);
    mono.style.backgroundColor = monogramColour(n);
    return mono;
  }

  // fill draws one server into a cloned template's [data-server-*] slots.
  // logos: whether another server's logo may be requested now (the list is
  // on screen). This server's own logo is its own, versioned path.
  function fill(node, s, colour, box, logos) {
    var dot = node.querySelector('[data-server-dot]');
    if (dot) dot.style.backgroundColor = colour;
    var n = name(s, box);
    var label = node.querySelector('[data-server-label]');
    if (label) label.textContent = n;
    var host = node.querySelector('[data-server-host]');
    if (host) host.textContent = hostLabel(s.origin);
    var mono = paintMonogram(node, n);
    if (mono) mono.hidden = false;
    var img = node.querySelector('[data-server-logo]');
    if (!img) return;
    // This server: its own logo (versioned, same origin), else — no branding
    // at all — muster's own mark. Another: its icon path, only while the list
    // is on screen.
    var src = s.origin === self ? (box.getAttribute('data-self-logo') || box.getAttribute('data-self-mark') || '') : (logos ? logoSrc(s.origin) : '');
    img.hidden = true;
    if (!src) { img.removeAttribute('src'); return; }
    // Shown, in the monogram's place, once it has loaded; a failure (no logo:
    // 404, a server that is down, a refusal) leaves the monogram.
    img.referrerPolicy = 'no-referrer';
    img.addEventListener('load', function () { img.hidden = false; if (mono) mono.hidden = true; });
    img.addEventListener('error', function () { img.hidden = true; img.removeAttribute('src'); if (mono) mono.hidden = false; });
    img.src = src;
  }

  // fillToggle draws the row button for this server (mine): its name as the
  // button's accessible name and tooltip (the person's own label wins, as in
  // the list) and its monogram. The row has no colour badge (that is the
  // list's). The picture the server drew (its logo, or muster's square mark)
  // stays; one that fails to load gives way to the monogram — whether it
  // failed before this deferred script ran (complete with no width) or fails
  // later.
  function fillToggle(t, mine, box) {
    var n = name(mine, box);
    t.setAttribute('aria-label', TOGGLE_LABEL + n);
    t.setAttribute('title', n);
    var mono = paintMonogram(t, n);
    var pic = t.querySelector('img[data-server-picture]');
    if (!pic || !mono || pic.__musterBound) return;
    pic.__musterBound = true;
    var fail = function () { pic.hidden = true; mono.hidden = false; };
    if (pic.complete && !pic.naturalWidth) fail();
    else pic.addEventListener('error', fail);
  }

  function selfName(box) { return cleanText(box.getAttribute('data-self-name') || ''); }
  function menuOpen() { var m = $('server-switcher-menu'); return !!m && !m.hidden; }

  function render(logos) {
    var box = $('server-switcher');
    if (!box) return;
    if (logos === undefined) logos = menuOpen();
    var list = current();
    var mine = list[0];
    var cols = colours(list.map(function (s) { return s.origin; }));
    fillToggle(box.querySelector('#server-switcher-toggle'), mine, box);

    var ul = $('server-switcher-list');
    var tpl = $('server-item-tpl');
    if (ul && tpl) {
      ul.textContent = '';
      // The list shows names, not addresses; an address only where two
      // entries share a name (case aside), so those two can be told apart.
      var clash = collisions(list.map(function (s) { return name(s, box); }));
      list.forEach(function (s) {
        var li = tpl.content.firstElementChild.cloneNode(true);
        var a = li.querySelector('a');
        fill(li, s, cols[s.origin], box, logos);
        var host = li.querySelector('[data-server-host]');
        if (host) host.hidden = !clash[name(s, box).toLowerCase()];
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
        fill(li, s, cols[s.origin], box, logos);
        li.setAttribute('data-server-origin', s.origin);
        var input = li.querySelector('input');
        input.value = s.label;
        input.placeholder = (s.origin === self && selfName(box)) || hostLabel(s.origin);
        input.setAttribute('aria-label', 'Label for ' + hostLabel(s.origin));
        var rm = li.querySelector('[data-server-remove]');
        if (s.origin === self) {
          rm.hidden = true;
          var tag = li.querySelector('[data-server-current]');
          if (tag) tag.hidden = false;
        } else {
          rm.setAttribute('aria-label', 'Remove ' + display(s, ''));
        }
        rows.appendChild(li);
      });
    }
  }

  function setOpen(open) {
    var t = $('server-switcher-toggle');
    var menu = $('server-switcher-menu');
    if (!t || !menu) return;
    menu.hidden = !open;
    t.setAttribute('aria-expanded', open ? 'true' : 'false');
    if (open) render(true);
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
      t.focus();
    }
    ['server-add', 'server-manage', 'server-offer'].forEach(function (id) {
      var d = $(id);
      if (d) d.querySelectorAll('[data-dialog-close]').forEach(function (b) {
        b.addEventListener('click', function () { d.close(); });
      });
    });

    var manDlg = $('server-manage');
    $('server-manage-open').addEventListener('click', function () { render(true); manDlg.showModal(); });
    manDlg.addEventListener('close', function () { render(); t.focus(); });
    var rows = $('server-manage-list');
    rows.addEventListener('change', function (e) {
      var input = e.target.closest('input');
      var li = e.target.closest('[data-server-origin]');
      if (!input || !li) return;
      persist(rename(current(), li.getAttribute('data-server-origin'), input.value));
      input.blur();
      render(true);
    });
    rows.addEventListener('keydown', function (e) {
      if (e.key !== 'Enter' || e.target.tagName !== 'INPUT') return;
      e.preventDefault();
      var li = e.target.closest('[data-server-origin]');
      var origin = li && li.getAttribute('data-server-origin');
      e.target.dispatchEvent(new Event('change', { bubbles: true }));
      // The rows were redrawn: keep the keyboard in the row just renamed.
      var again = Array.prototype.find.call(rows.querySelectorAll('[data-server-origin]'), function (r) { return r.getAttribute('data-server-origin') === origin; });
      var input = again && again.querySelector('input');
      if (input) input.focus();
    });
    rows.addEventListener('click', function (e) {
      var b = e.target.closest('[data-server-remove]');
      var li = e.target.closest('[data-server-origin]');
      if (!b || !li) return;
      persist(remove(current(), li.getAttribute('data-server-origin'), self));
      render(true);
      var first = rows.querySelector('input');
      if (first) first.focus();
    });

    var offer = $('server-offer');
    if (offer) {
      $('server-offer-add').addEventListener('click', function () {
        var o = offer.getAttribute('data-origin');
        var r = add(current(), o);
        if (r.ok) { persist(r.list); render(); }
        offer.close();
      });
    }
  }

  // The fragment hand-off: a switch from another server names it; offer to
  // put it on this list (never add it unasked). The fragment is removed from
  // the address bar either way.
  // The hand-off arrives in this page's own fragment, or — through a sign-in
  // — as what the sign-in page kept in sessionStorage. Either is read once.
  function offerHandoff() {
    var inHash = /^#muster-from=/.test(location.hash);
    var stored = null;
    try {
      stored = root.sessionStorage.getItem(HANDOFF_KEY);
      if (stored !== null) root.sessionStorage.removeItem(HANDOFF_KEY);
    } catch (e) { stored = null; }
    var hash = inHash ? location.hash : kept(stored, Date.now());
    if (inHash) {
      try { history.replaceState(history.state, '', location.pathname + location.search); } catch (e) { /* keep going */ }
    }
    if (!hash) return;
    var from = handoff(hash, self);
    var offer = $('server-offer');
    if (!from || !offer) return;
    var list = current();
    if (list.some(function (s) { return s.origin === from; }) || list.length >= MAX) return;
    offer.setAttribute('data-origin', from);
    var text = offer.querySelector('[data-server-origin-text]');
    if (text) text.textContent = from;
    offer.showModal();
    // Focus on "Not now", whatever the browser picked: a stray Enter dismisses.
    var no = $('server-offer-dismiss');
    if (no) no.focus();
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
      var menu = $('server-switcher-menu');
      var t = $('server-switcher-toggle');
      if (!menu || !t || t.getAttribute('aria-expanded') !== 'true') return;
      // The row's box also holds the sidebar's search and close buttons, so a
      // click away is one outside the button and the menu. The dialogs live
      // outside the box (in the sidebar's root), and a click in one is not a
      // click away from the menu.
      if (t.contains(e.target) || menu.contains(e.target) || (e.target.closest && e.target.closest('#server-add, #server-manage, #server-offer'))) return;
      setOpen(false);
    });
    root.addEventListener('storage', function (e) { if (e.key === KEY) render(); });
  }

  bind();
  render();
  offerHandoff();
})(typeof globalThis !== 'undefined' ? globalThis : this);
