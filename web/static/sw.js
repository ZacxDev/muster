/*
 * muster service worker.
 *
 * Served from "/sw.js" (root scope) so it controls the whole origin. It does
 * three jobs:
 *   1. Provide a minimal offline app shell (cache the root document + CSS so
 *      the UI still opens with no network; live data still needs the network
 *      and degrades gracefully). 🔴 Served ONLY while navigator.onLine is
 *      false — see the navigate branch in the fetch handler for why that gate
 *      is load-bearing rather than defensive.
 *   2. Handle Web Push: render a notification, and close stale ones on a
 *      "resolved" control message.
 *   3. Handle notification clicks: approve/deny a PRIVILEGE request directly
 *      from the worker, or focus/open the app at the relevant card.
 *
 * 🔴 THIS IS NOT THE UPSTREAM WORKER, AND ONE BRANCH IS DELIBERATELY ABSENT.
 * Upstream this file also decides permission requests, by POSTing to the
 * permission router's decision route. That route is on the `router` side of the
 * route partition manifest — muster does not register it and never will.
 * Carrying the branch across would give the worker a button that POSTs to a
 * 404: a tap that reports nothing, changes nothing, and reads as "the approval
 * did not go through" with no error anywhere. The privilege branch IS carried,
 * because `POST /ui/privilege-requests/{id}/{approve,deny}` is on muster's side
 * of that same manifest.
 *
 * ⚠ THE DECISION ROUTE'S PATH IS DELIBERATELY NOT SPELLED ANYWHERE IN THIS
 * FILE. A test asserts this file does not contain it, and a string check on
 * JavaScript cannot tell a comment from a fetch — so naming it here to explain
 * its absence would red the guard from inside the explanation.
 *
 * Fetches use credentials:"include" so the session cookie rides along.
 */

'use strict';

const CACHE = 'muster-shell-v1';
// The minimal shell: the document and the compiled CSS. Vendored JS is fetched
// fresh (it is small and benefits from normal HTTP caching) to avoid serving a
// stale htmx during development.
const SHELL = ['/', '/static/app.css', '/manifest.webmanifest'];

self.addEventListener('install', (event) => {
  event.waitUntil(
    caches
      .open(CACHE)
      .then((cache) => cache.addAll(SHELL))
      // Don't fail install if a shell asset is briefly unavailable; the worker
      // still installs and push keeps working.
      .catch(() => undefined)
      .then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((keys) =>
        Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k)))
      )
      .then(() => self.clients.claim())
  );
});

// Navigations are handled by the BROWSER unless the device is known to be
// offline (see below). app.css is network-first (with a cache fallback + cache
// refresh) — it must never be served stale, because each UI change ships a
// freshly-compiled Tailwind app.css with new classes; a cache-first strategy
// here silently breaks every new feature's styling for returning users. Other
// requests pass through to the network.
self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET') return;

  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return;

  // Never intercept the SSE stream or API calls — they must hit the network.
  if (url.pathname === '/events' || url.pathname.startsWith('/api/')) return;

  if (req.mode === 'navigate') {
    // 🔴 THE OFFLINE SHELL IS ONLY EVER SERVED WHEN THE DEVICE IS OFFLINE, AND
    // THAT GATE IS THE WHOLE POINT OF THIS BRANCH.
    //
    // This used to be `fetch(req).catch(() => caches.match('/'))` — a bare
    // catch, with "the fetch rejected" standing in for "we are offline". Those
    // are NOT the same thing, and the difference is a dead app.
    //
    // When the app sits behind a forward-auth edge, a top-level navigation with
    // an expired session gets a CROSS-ORIGIN 302 to the login portal. Upstream
    // measured a stuck tab reporting deliveryType "cache-storage" and
    // transferSize 0 for the document while its SUBRESOURCES reached the portal
    // in the same second — i.e. the network was up and only the document came
    // from this cache. The browser therefore never followed the 302; the app
    // rendered as if logged in, every subsequent script load was answered with
    // a cross-origin redirect, and a `script-src 'self'` CSP blocked it. A dead
    // page that looks authorised.
    //
    // ⚠ WHY the fetch took the catch branch was NOT established — do not write a
    // mechanism into this comment. What is established is the OBSERVABLE, and
    // this gate makes the observable impossible regardless of the mechanism:
    // while the device is online the worker does not touch the navigation at
    // all, so the browser performs it natively and follows any redirect —
    // including the one to the auth portal.
    //
    // navigator.onLine is used only in the direction it is trustworthy: `false`
    // means the browser has no network route at all. A wrongly-true reading
    // costs the offline shell (degraded); a wrongly-served shell costs the whole
    // app (broken). Fail in the cheap direction.
    if (navigator.onLine !== false) return;
    event.respondWith(caches.match('/').then((r) => r || fetch(req)));
    return;
  }

  if (url.pathname === '/static/app.css') {
    event.respondWith(
      fetch(req)
        .then((res) => {
          // Refresh the cached copy for offline use, then serve the fresh one.
          const copy = res.clone();
          caches.open(CACHE).then((c) => c.put(req, copy)).catch(() => undefined);
          return res;
        })
        .catch(() => caches.match(req))
    );
  }
});

// --- Web Push ---

self.addEventListener('push', (event) => {
  let payload = {};
  if (event.data) {
    try {
      payload = event.data.json();
    } catch (e) {
      payload = { title: 'muster', body: event.data.text() };
    }
  }

  // A "resolved" control message: close any open notification for this id on
  // this device (it was decided elsewhere). Render nothing.
  if (payload.type === 'resolved') {
    event.waitUntil(closeByTag(payload.tag || payload.id));
    return;
  }

  // A "notify" message: a fire-and-forget, informational notification. It is
  // NOT a decision prompt, so render a plain notification with no action
  // buttons and let it auto-clear (requireInteraction: false). A body tap just
  // opens the app.
  if (payload.type === 'notify') {
    const data = Object.assign({ type: 'notify' }, payload.data || {});
    if (!data.url) data.url = '/';
    event.waitUntil(
      self.registration.showNotification(payload.title || 'muster', {
        body: payload.body || '',
        icon: '/static/icons/icon-192.png',
        badge: '/static/icons/badge-72.png',
        tag: payload.tag || undefined,
        data: data,
        requireInteraction: false,
      })
    );
    return;
  }

  const title = payload.title || 'muster';
  const tag = payload.tag || payload.id || 'muster';
  // Always carry type + id on the notification data so notificationclick can
  // route a privilege decision to
  // POST /ui/privilege-requests/<id>/{approve,deny}. payload.data may
  // add/override fields — a privilege push sets url:'/agents'; a task push sets
  // url:'/tasks/<id>' in the common case (the server derives it from the task
  // id), falling back to '/tasks' when there is no id (the coalesced
  // N-tasks-created push) and to an explicit '/agents/<name>' for the
  // agent-ready push.
  const data = Object.assign({ type: payload.type, id: payload.id }, payload.data || {});
  if (!data.url) data.url = '/#' + (payload.id || '');
  // Only a PRIVILEGE push carries Approve/Deny actions here. A "task" push is
  // informational (task created / agent ready / ready for review): render title
  // + body, no actions, and don't pin it — a body tap focuses/opens data.url.
  //
  // 🔴 THE TEST IS `type === 'privilege'`, NOT `type !== 'task'`. The negative
  // spelling is what upstream uses, because upstream can decide two kinds of
  // request; here it would paint Approve/Deny onto every push type this worker
  // does not recognise — including any the router might one day send — and each
  // of those buttons would POST to the privilege route with an id that is not a
  // privilege request. Naming the one type that CAN be decided makes an
  // unknown type render as a plain notification, which is the truthful one.
  const isDecision = payload.type === 'privilege';
  const options = {
    body: payload.body || '',
    icon: '/static/icons/icon-192.png',
    badge: '/static/icons/badge-72.png',
    tag: tag,
    data: data,
    requireInteraction: isDecision,
    actions: isDecision
      ? [
          { action: 'approve', title: '✅ Approve' },
          { action: 'deny', title: '❌ Deny' },
        ]
      : [],
  };
  event.waitUntil(self.registration.showNotification(title, options));
});

async function closeByTag(tag) {
  if (!tag) return;
  const notes = await self.registration.getNotifications({ tag });
  notes.forEach((n) => n.close());
}

// --- Notification clicks ---

self.addEventListener('notificationclick', (event) => {
  const note = event.notification;
  const data = note.data || {};
  const id = data.id || note.tag;
  note.close();

  // Action buttons: record the decision directly from the worker. We
  // deliberately do not openWindow here so an approve/deny is a single tap with
  // no app load.
  if (event.action === 'approve' || event.action === 'deny') {
    if (data.type === 'privilege') {
      event.waitUntil(decidePrivilege(id, event.action));
      return;
    }
    // Any other type reaching here has no decision endpoint on this service.
    // Open the app rather than POSTing somewhere that would 404.
    event.waitUntil(focusOrOpen(data.url || '/'));
    return;
  }

  // Body tap: bring the app forward (or open it) at this card.
  const target = (data.url || '/#' + (id || '')).toString();
  event.waitUntil(focusOrOpen(target));
});

async function decidePrivilege(id, action) {
  if (!id) return;
  const path = action === 'approve' ? '/approve' : '/deny';
  try {
    await fetch('/ui/privilege-requests/' + encodeURIComponent(id) + path, {
      method: 'POST',
      credentials: 'include',
    });
  } catch (e) {
    // Offline / failed: open the Agents tab so the user can decide there.
    await focusOrOpen('/agents');
  }
}

async function focusOrOpen(target) {
  const all = await self.clients.matchAll({ type: 'window', includeUncontrolled: true });
  for (const client of all) {
    // Reuse any open tab.
    if ('focus' in client) {
      await client.focus();
      if ('navigate' in client && target) {
        try {
          await client.navigate(target);
        } catch (e) {
          /* cross-origin or not allowed; focus is enough */
        }
      }
      return;
    }
  }
  if (self.clients.openWindow) {
    await self.clients.openWindow(target || '/');
  }
}
