# Design: a server switcher in the web app

**Status: implemented** (this page's first commit is the design; the code
follows it on the same branch). The Android app's switcher is the reference:
[ZacxDev/muster-android](https://github.com/ZacxDev/muster-android) README,
"Several muster servers (0.2.0)", and [multi-tenant.md](multi-tenant.md)
phase 3.

## The ask

Operator decision, 2026-11-13: give the installable web app (the PWA) the
Android app's server switcher. A bar shows the server in view (colour and
label); tap it for the list of servers, **Add server** and **Manage servers**;
on Android each server's branding comes from its own
[`GET /api/instance`](../branding.md#the-instance-document) (on the web, only
the logo, drawn as an image: [option (a)](#a-a-list-per-origin-switching-navigates-the-window-chosen)); every paired
server still notifies, and a notification's tap opens that server; two
servers never share an origin or a session.

This reverses one line of [multi-tenant.md](multi-tenant.md#operator-decisions)
("The PWA is out of scope: a browser already isolates servers by origin").
The isolation is still the browser's; what is new is a way to get from one
server to another.

## The constraint

A web app is bound to one origin. The Android app gives each server its own
WebView profile (own cookies, storage and service workers) inside one app; a
page cannot do that. Everything a muster page can keep (cookies,
`localStorage`, IndexedDB, its service worker and push subscription) is its
origin's, and is invisible to another muster's pages. So the question is not
"how do we hold several sessions in one page" (we cannot) but "how does a
person get from one server's page to another's quickly, and see which one
they are on".

## Options

### (a) A list per origin; switching navigates the window (chosen)

Each muster origin keeps, in its own `localStorage`, a list of servers: their
origins and the person's own label for each, nothing else. The switcher draws
that list; picking a server is a top-level navigation to its origin. Each
server keeps its own sign-in, cookies, service worker and push subscription,
exactly as if the person had typed its address.

- **Nothing on another origin is read.** Operator decision, 2026-11-13, on
  this PR's round-0 review ("Logo via <img>, no CORS"). The first revision
  read each server's name and logo cross-origin from its `/api/instance`
  with `Access-Control-Allow-Origin: *`. That let any website read the name and
  logo of a muster the visitor can reach but the internet cannot (a LAN, a
  tailnet, `localhost`), which confirms that the visitor runs one there and
  names the team. So:
  - **the logo** is a plain `<img>` of the other server's
    `/brand/icon-192.png`: the unversioned path every muster already serves
    (the current icon, `no-cache` plus an `ETag`, so a new logo shows at
    once; 404 with no logo). A browser draws a cross-origin image, but no
    script can read it. It needs no CORS and no server change. The tag has
    no `crossorigin` attribute, so it is not a CORS request, and it has
    `referrerpolicy="no-referrer"`. A 404, or a server that is down, leaves
    its monogram and its label. Logos are requested only while the list is on
    screen;
  - **the label** is the person's own, else the host name. Another server's
    display name is not shown: reading it would need the cross-origin read
    this decision removes. (The server in view still shows its own name,
    which its page already has.);
  - **the CSP**: `connect-src` is unchanged (`'self'`). `img-src` gains
    exactly `/brand/icon-192.png` on any `https` origin, and on
    `http://localhost` / `http://127.0.0.1` for development. It gains no
    other path and no prefix;
  - **cookies on the image request**: muster's session cookie
    (`muster_session`) is `SameSite=Lax` and host-only, so a cross-site
    image request carries none. Two musters on sibling subdomains of one
    site are *same-site*, and there the image request carries the other
    server's **own** cookie to that same server, to a public route that
    ignores it. 🔴 **Two musters on different ports of one host share one
    cookie jar.** Cookies are not port-scoped, so both servers already
    receive each other's `muster_session` on every request, whether or not
    the switcher exists. The switcher adds nothing to that beyond ordinary
    navigation between them. Run separate servers on **separate host names**;
  - **an IP address**: a CSP host wildcard (`https://*:*`) does not match an
    IP address in browsers that follow the CSP spec, so a server addressed
    by IP (other than the two loopback sources listed) may show no logo,
    only its monogram and label.
- **The list is per origin**, so a server added on A is not on B's list. The
  switch carries one thing across: A's own origin, in the URL fragment of the
  navigation (`#muster-from=<A's origin>`; a fragment is never sent to a
  server). B's page reads it, removes it from the address bar, and, when A is
  not in B's list, **offers** to add it. It never adds it by itself, and,
  since anyone can link to `B/#muster-from=…`, the offer claims nothing about
  where the person came from. It says "A link asked to add this server",
  shows the **full origin** (scheme, host and port) prominently, and starts
  its focus on **Not now**, so a stray Enter dismisses it.
  - **Through a sign-in:** a switch to a server you are not signed in to
    lands on its `/login` with the fragment, and the post-sign-in redirect
    drops it. The sign-in page keeps it in `sessionStorage` (this tab only,
    with the time), and the first signed-in page offers it, then forgets it.
    A kept hand-off older than ten minutes is ignored. This was chosen over
    "the offer appears only if you were already signed in", because a first
    switch to a server is exactly when you are not.
- **Privacy.** The list holds origins and labels only. Nothing of one server
  (a cookie, a token, a path, a page's data) is ever sent to another. The
  only cross-origin requests are the logo images, with no referrer. muster's
  pages already send `Referrer-Policy: no-referrer`, so the navigation
  carries no Referer either. A pasted address is reduced to its origin before
  it is stored, so a path or a query (which might hold a token) is never kept
  or requested. An address with a user name or password in it is refused.

### (b) A hub page embedding the others in frames (rejected)

One server's page frames the others. Every muster response says
`frame-ancestors 'none'` / `X-Frame-Options: DENY` (clickjacking), and lifting
that for "another muster" would need each server to know the others. Even
then: a framed server is third-party to the hub, so its sign-in cookie is a
third-party cookie (blocked by Safari, partitioned by Chrome); passkeys in a
cross-origin frame need `publickey-credentials-get` delegation from the hub;
push and the service worker are the hub's, not the framed server's; and the
installed app would be the hub's alone. Every one of the Android app's
guarantees would be weaker than (a)'s.

### (c) The server keeps the list, or proxies the branding (rejected)

Storing each person's other servers on a server (in its database) tells that
server which other muster deployments the person uses, and fetching their
branding server-side makes every muster a URL-fetching proxy (an SSRF surface
the project has avoided: `internal/safedial`). A server-side list would follow
the person across browsers, which (a) cannot; that is the only gain, and it
is not worth either cost.

### (d) A signed export/import of the list (not now)

A list could be exported from A and imported on B. The fragment hand-off in
(a) covers the common case (two or three servers, added once) with one tap;
an export format is a contract to keep forever. Not built; nothing in (a)
prevents it later.

## Where it sits in the web layout

Android draws a bar above the page. The web app's equivalent place is the
sidebar's top row: the sidebar is on every signed-in page, a rail on a wide
screen and a slide-out (the menu button) on a phone.

**One row, the logo only** (operator decision, "One row: the logo is the
switcher", on the ask to make the switcher smaller and less cluttered). The
sidebar used to open with two rows: the brand (logo and name) with search and
close, and under it the server row (colour, name, chevron). They are one row
now:

- the server in view's **picture is the button**, with a small chevron, then
  search (and close, where the sidebar is a slide-out). The picture is the
  server's logo; with no logo, a **monogram** of its name; with no branding at
  all, muster's compact square mark (the brand pack's `favicon.svg`, in the
  same 28 px slot as a logo; operator decision on round 0, Q1: not the
  horizontal lockup). No name and no address is drawn in the row: the name is
  the button's accessible name ("Switch server, current: <name>") and its
  tooltip;
- the row draws the picture alone: **no colour dot** (round 0, D2). The
  server's colour, the Android app's colour for it, stays in the open list as
  a small badge on each entry's picture, where it tells two entries with the
  same picture apart;
- the row is the old brand row's height (68 px, lining up with the content
  header's border) and is a 44 px target;
- the button opens the menu whether or not other servers are known: **Add
  server** lives there.

The menu lists the servers (the one in view marked **In view**), each as its
picture (logo, else monogram) and its name, then **Add server** and **Manage
servers**. It shows **no address**, except where two entries have the same
name (compared without case): only those show their host under the name, so
they can still be told apart. Manage servers rows show the label too, with
the exact address behind each row's ⓘ (Copy).

A **monogram** is the first character of each of the first two words of the
name, on a palette colour picked by the name's FNV-1a hash: the same name is
the same monogram everywhere. It shows wherever a logo is absent or fails to
load.

The server draws the row's monogram before the script runs (Go
`serverMonogram`), and the script draws the menu's (`monogram`). The rule asks
**no Unicode table**, because the two languages' tables differ: Go's are
Unicode 15.0 and a browser's are newer, and Go upper-cases with the simple
mapping where JavaScript uses the full one. So:
- words split on fixed separator ranges: controls, ASCII and Latin-1
  punctuation and symbols, General Punctuation, and the ideographic space;
- a first character is upper-cased only when it and its one-code-point
  uppercase both lie in Latin, Greek or Cyrillic (U+0000–U+024F,
  U+0370–U+04FF). Anything else, an emoji or a CJK character for example, is
  kept as it is.

Both implementations are held to a table of literal cases
(`internal/ui/testdata/server-monograms.json`), and to the monogram of every
code point from U+0000 to U+04FF. That second table was generated apart from
both, from Python's full case mapping
(`internal/ui/testdata/server-monogram-letters.json`).

The row is drawn by the server at a fixed size, and the script only fills it
in (its colours, letters and name), so it moves nothing when it loads (the
layout-shift gate, `cls.spec.ts`).

## Rules of the list

- An entry is an origin: `https:`, or `http:` only for `localhost` and
  `127.0.0.1`. No user name or password. A path, query or fragment typed with
  it is dropped (only the origin is kept).
- At most 8 servers, the server in view included (the Android app's palette
  has 8 colours).
- No duplicates (compared by origin, so `https://a.example` and
  `https://A.example:443/x` are one).
- The server in view is always on its own list, first, and cannot be removed
  from it.
- Each server's colour comes from its origin, the same way on every server's
  list (a hash of the origin picks the palette slot; a clash takes the next
  free slot, in origin order), so a server keeps its colour from one list to
  the next when the lists hold the same servers.
- The label is the person's own, else the host name, with no scheme and no
  port — the port only when another server on the list has the same host
  name, to tell the two apart. The server in view's own entry falls back to
  its own display name, then "This server", never its address. A label is at
  most 64 characters, with control and text-direction characters removed,
  and is drawn as text, never markup.
- A row shows the label only. The exact address is behind the ⓘ on each
  Manage servers row (the shared details sheet), with Copy. The one
  exception is the menu: where two entries' labels collide (case aside),
  each shows its host under it, so the two can be told apart.
- A typed address must look like one: a host that is not DNS labels
  (letters, digits, `-`, `_`) or an IP is refused ("That doesn't look like a
  server address."). One word with no dot, no port and no scheme (other than
  `localhost`) is refused with "That doesn't look like a server address. If
  it is one, add https:// in front of it." (`acme:40001` and
  `https://acme` are accepted). This is checked by servers.js itself,
  not left to the browser's URL parser: Chromium's percent-encodes a space in
  a host where the standard refuses it, which once listed "not a server" as
  `https://not%20a%20server`.
- A trailing dot on a host is dropped, so `atlas.example.` and
  `atlas.example` are one server.
- 🔴 A SAVED entry is never lost to a typing rule newer than it: on load, a
  trailing-dot entry is migrated to its dot-less form with its label, and a
  host the typing rules would refuse is kept. Only junk (a space or `%` in the
  host) is dropped.
- Another server's logo is `<origin>/brand/icon-192.png`, built from the
  stored origin alone, requested only while the list is shown.

## Web vs Android

| | Android 0.2.0 | web (this design) |
|---|---|---|
| see which server is in view | bar above the page: colour, label | the sidebar's top row: the server's logo (or monogram, or muster's square mark) alone; its colour is in the list; the name is the button's accessible name and tooltip (on a phone, inside the menu) |
| switch | instant, each server's WebView kept | a page load of the other server's origin |
| separate sign-ins | per-server WebView profile | per-origin by the browser, always |
| add a server | Add server, then pair the phone | Add server (an address); sign in there as usual |
| the list | one list for the app | one list **per server, per browser**; a switch offers to add the server it names (also through a sign-in) |
| the other server's name | from its `GET /api/instance` | **not shown**: the label is yours, else the host (no cross-origin read, by operator decision) |
| the other server's logo | from `/api/instance`, cached by etag | drawn as an `<img>` of its `/brand/icon-192.png` (current icon, revalidated by the browser); a monogram of its label when it has none |
| notifications from every server | yes, one UnifiedPush instance each | yes, if notifications were turned on **on each server** (Web Push is per origin); each arrives with that server's icon |
| a notification's tap opens its server | yes, and switches the bar | yes: the server's own service worker opens its own page |
| label on a notification | server label as subtext | not added: the icon (the server's logo) is the cue |
| manage: rename, remove | yes, and remove ends the pairing | rename the label, remove from the list; removing does not sign out or turn off that server's notifications (do that on that server) |
| installed app | one app | each server installs as its own app; the switcher in an installed app opens the other server in the browser's out-of-scope view (or that server's own installed app, where the browser hands links to it) |
| passkeys | per server (asset links) | per origin, unchanged |

## UNVERIFIED

- **iOS Safari, installed to the home screen**: a navigation to another
  origin from a standalone web app opens it in an in-app Safari view (or
  Safari itself, depending on the iOS version), not in the standalone window.
  Not measured on a device.
- **Chrome / Edge installed apps**: whether a switch opens the other server's
  own installed app ("link capturing") depends on the browser version and
  settings. Not measured.
- A browser that clears `localStorage` for an installed app separately from
  the tab (Safari keeps separate storage for a home-screen app) has a
  separate list there.
- **The `img-src` source `https://*:*/brand/icon-192.png`** was measured in
  Chromium only: the wildcard host, both the default and a non-default port,
  and other paths refused. Safari and Firefox were not run.

## Tests

- `e2e/servers.test.mjs` (node): the address rules (https only but loopback,
  no credentials, origin only), dedupe, the cap, colours, labels, an entry
  keeping only its origin and label, the logo path built from the origin
  alone, and a source check that the script makes no request of its own (no
  fetch, XHR, beacon, socket or event stream); the monogram (against the
  literal table Go is held to as well), which names collide, and the row
  button's name.
- `internal/api`: `GET /api/instance` and `/brand/*` send no
  `Access-Control-Allow-Origin` (404, 200, 304); the unversioned icon path is
  served revalidated; `connect-src` is `'self'` and `img-src` adds exactly
  the icon path.
- `e2e/tests/server-switcher.spec.ts`, at phone and desktop sizes:
  - a fake second server with no CORS header, whose logo the spec turns on
    and off;
  - zero cross-origin fetch or XHR from the page, next to the controls of a
    same-origin read and a cross-origin image;
  - the logo request is an image (`no-cors`) with no cookie, referrer or
    Origin, and none is made while the menu is shut;
  - the real muster under a second origin: its data cannot be read (CSP and
    no CORS), and only the icon path can be drawn;
  - adding, rejecting bad addresses, renaming (the keyboard stays in the
    row), removing, switching, and the hand-off offer on the other side;
  - a crafted hand-off: neutral copy, the full origin, focus on Not now,
    nothing stored, and Enter dismisses;
  - a switch to a server you are not signed in to: the offer survives the
    sign-in, and it is read once;
  - one row (at 375 px and 1280 px): the picture button, search and close
    side by side, 68 px tall, no text in it; a tap opens the list, which
    shows names and no address; a server with no logo shows its monogram;
    two servers with the same name show their addresses, and only they;
    switching from either size.
- `e2e/tests/branding.spec.ts`: the row is the instance's logo, alone; a logo
  that fails to load gives way to the monogram; a name with no logo is its
  monogram.
- `internal/ui` and `internal/api`: the sign-in page keeps the hand-off under
  the key `servers.js` reads; the row button is announced once, by its
  aria-label; the row for each branding state (logo, name only, logo only,
  none), pinned by literal; the sidebar's top is one row; a menu entry's
  address starts hidden; the monogram against the literal table; the
  stylesheet's server colours are the script's palette, and black letters
  read on each at 4.5:1 or more; the offer's copy claims no origin.
