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
    the colour and the label. Logos are requested only while the list is on
    screen;
  - **the label** is the person's own, else the host. Another server's
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
    only its colour and label.
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
sidebar, under its brand row: the sidebar is on every signed-in page, a rail
on a wide screen and a slide-out (the menu button) on a phone. The row is the
server in view: its colour, its label, a chevron. It opens a menu: the
servers (the one in view marked current), **Add server**, **Manage servers**.
It is drawn by the server at a fixed height, and the script only fills it in,
so it moves nothing when it loads (the layout-shift gate, `cls.spec.ts`).

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
- The label is the person's own, else the host (and the port when it is not
  the default). The server in view's own entry falls back to its own display
  name before its host. A label is at most 64 characters, with control and
  text-direction characters removed, and is drawn as text, never markup.
- Another server's logo is `<origin>/brand/icon-192.png`, built from the
  stored origin alone, requested only while the list is shown.

## Web vs Android

| | Android 0.2.0 | web (this design) |
|---|---|---|
| see which server is in view | bar above the page: colour, label | sidebar row: colour, label (on a phone, inside the menu) |
| switch | instant, each server's WebView kept | a page load of the other server's origin |
| separate sign-ins | per-server WebView profile | per-origin by the browser, always |
| add a server | Add server, then pair the phone | Add server (an address); sign in there as usual |
| the list | one list for the app | one list **per server, per browser**; a switch offers to add the server it names (also through a sign-in) |
| the other server's name | from its `GET /api/instance` | **not shown**: the label is yours, else the host (no cross-origin read, by operator decision) |
| the other server's logo | from `/api/instance`, cached by etag | drawn as an `<img>` of its `/brand/icon-192.png` (current icon, revalidated by the browser); colour and label when it has none |
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
  fetch, XHR, beacon, socket or event stream).
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
    sign-in, and it is read once.
- `internal/ui` and `internal/api`: the sign-in page keeps the hand-off under
  the key `servers.js` reads; the row button is announced once; the offer's
  copy claims no origin.
