package ui

import "strconv"

// A SENT MESSAGE SHOWS AT ONCE, AS PENDING, AND IS RECONCILED WHEN IT LANDS
// (mirrorComposerScript). The operator's ask: no hang between pressing Send
// and seeing the message go out. Before this, Send was disabled and the text
// sat in the box until the host answered — a whole tunnel round trip and a
// paste (measured in e2e/tests/optimistic-send.spec.ts: ~1.8 s with a 1.5 s
// host delay injected, and nothing drawn in the conversation until then).
//
// What the press does now, in the same task:
//
//   - the box is emptied (focus and the keyboard stay: sendKeepsFocusJS);
//   - on the Rendered view the message is drawn at the END of the
//     conversation as a pending bubble (data-pending-send): muted, with a
//     status line ("Sending…", role=status). A Queue is drawn instead as a
//     pending row in the send queue's list ("Queuing…"), never in the
//     conversation: it is not sent. A Terminal view has no conversation, so
//     its status line (#mv-note) says "Sending…";
//   - the request goes out. Sends from one box are posted ONE AT A TIME, in
//     the order pressed (a second Send while the first is out is drawn at
//     once and posted after it), so two messages cannot overtake each other
//     on the way to the pane.
//
// Then, by what comes back:
//
//   - OK: the bubble says "Sent". 🔴 IT IS NOT REMOVED THEN: it is removed in
//     the same DOM task that draws the transcript row it stands for
//     (reconcile, called from mirrorViewScript's apply) — or the row of
//     "Queued in Claude Code" (cc_queued.go) when Claude Code holds it — so
//     there is never a frame with both or with neither. A sent bubble no row
//     ever matches (a `/command` Claude Code records differently) is removed
//     after pendingSentSettleMS: the host confirmed it.
//   - a step-up (403 + X-Muster-Step-Up): the bubble waits, "Confirm it's
//     you to send", and the SAME request (same nonce) is posted after the
//     confirm. Cancelling the dialog fails it.
//   - any refusal (4xx/5xx, rate limit, the window gone, Claude waiting on a
//     prompt) or offline: the bubble turns failed with the reason and Retry
//     and Dismiss, and its text is put back in the box if the box is empty —
//     nothing typed is lost. #mv-note says the reason too, as it always did.
//     A refusal that MAY have been delivered (X-Muster-Send-Again) turns the
//     bubble UNKNOWN instead, and its button is the composer's Send again
//     (#mv-send-again, again=1): see SendAgainHeader. A new send of the same
//     text replaces a failed or unknown bubble of it.
//   - no answer while online: the browser submits the form itself (native),
//     as before, with the text put back for it.
//
// 🔴 HOW A BUBBLE IS MATCHED TO ITS ROW. Nothing the page sends survives into
// Claude Code's transcript — the host pastes the text as typed — so the
// match is by the TEXT: letters and digits only, lower-cased (the row is
// rendered markdown; `*bold*` arrives as "bold"), the row's text beginning
// with the first pendingMatchChars characters of what was TYPED (an
// attachment's line is drawn in the row as a chip, not as its text). Only a user row that
// was NOT on the page when the bubble was drawn can match it, and each row
// settles at most one bubble, so a repeated "yes" is not settled by the old
// one. (After a reset redraws the conversation every row is new; then only the
// newest rows, one per bubble, are candidates.)
//
// 🔴 A RELOAD MID-SEND LEAVES NO GHOST. Each pending send is kept in
// sessionStorage (this tab only) under the box's draft key, which names the
// account and the window, so it never shows in another account's or another
// window's box. The draft itself is cleared at the press — the pending
// entry holds the text instead, so a reload does not put it back in the box
// as well. After a reload the bubble is drawn again as "Checking…"; the
// conversation the page reads then settles it if the message landed; if no
// row has matched it within pendingRestoreSettleMS it turns UNKNOWN — "Not
// confirmed: it may or may not have been sent." — with Send again (again=1)
// and Dismiss. Entries older than pendingMaxAgeMS are dropped
// unread. A browser that refuses storage keeps none: the bubble then goes
// with the page.

// Pending sends' visible words. The e2e spec and the docs say these.
const (
	PendingSendingText  = "Sending…"
	PendingCheckingText = "Checking…"
	PendingSentText     = "Sent"
	PendingStepUpText   = "Confirm it's you to send"
	PendingQueuingText  = "Queuing…"
	PendingFailedText   = "Not sent"
	PendingUnknownText  = "Not confirmed: it may or may not have been sent."
	pendingCancelled    = "you did not confirm it's you."
)

// Pending sends' times.
const (
	// pendingSentSettleMS: a send the host confirmed that no row has matched
	// in this long is removed (it was delivered; the conversation is what
	// shows it).
	pendingSentSettleMS = 30_000
	// pendingRestoreSettleMS: a send drawn again after a reload that no row
	// has matched in this long turns unknown, with Retry.
	pendingRestoreSettleMS = 10_000
	// pendingMaxAgeMS: an entry older than this is not drawn after a reload.
	pendingMaxAgeMS = 10 * 60_000
	// pendingMatchChars: how much of a message must begin its row.
	pendingMatchChars = 200
)

// pendingStoreKey is the sessionStorage key of this tab's pending sends.
const pendingStoreKey = "muster.pending-send.v1"

// pendingBubbleClass is a pending bubble: the user bubble's shape and side
// (chatBubbleOf), muted. pendingFailedClass is a failed or unknown one.
const (
	pendingBubbleClass = "max-w-[85%] self-end break-words rounded-2xl bg-accent/10 px-3 py-2 text-sm text-fg2 ring-1 ring-inset ring-accent/30 opacity-70"
	pendingFailedClass = "max-w-[85%] self-end break-words rounded-2xl bg-danger/10 px-3 py-2 text-sm text-fg ring-1 ring-inset ring-danger/40"
	pendingQueueClass  = "flex items-start gap-2 px-3 py-1 opacity-70"
	pendingActionClass = "press min-h-[44px] rounded-lg px-3 text-xs font-semibold text-fg ring-1 ring-inset ring-line transition hover:bg-s2"
)

var mirrorComposerJS = `
(function () {
  var form = document.getElementById('mv-form');
  if (!form) return;
  var input = document.getElementById('mv-input');
  var send = document.getElementById('mv-send');
  (` + sendKeepsFocusJS + `)(send);
  var note = document.getElementById('mv-note');
  var nonce = document.getElementById('mv-nonce');
  // The conversation (the Rendered view's), and the send queue's list.
  var log = document.getElementById('mv-log');
  var qbox = document.getElementById('mv-queue');
  function say(msg) { note.textContent = msg || ''; note.hidden = !msg; note.removeAttribute('data-ok'); }
  function sayOK(msg) { say(msg); note.setAttribute('data-ok', ''); }
  function hex(n) {
    if (!window.crypto || !crypto.getRandomValues) return '';
    var b = new Uint8Array(n), s = '';
    crypto.getRandomValues(b);
    for (var i = 0; i < b.length; i++) s += (b[i] < 16 ? '0' : '') + b[i].toString(16);
    return s;
  }
  // A message is done with its nonce once it has been posted: the next one is
  // a new message, and must not be taken for a repeat of this one. 🔴 With no
  // way to mint one the field is EMPTIED: no nonce is no protection, and a
  // reused one would have the next message taken for this one.
  function renew() { if (nonce) nonce.value = hex(16); }
  // A notice in the address has been shown (the server drew it): take it out,
  // so reloading the page does not repeat it.
  try {
    var here = new URL(location.href);
    if (here.searchParams.has('notice')) {
      here.searchParams.delete('notice');
      history.replaceState(history.state, '', here.pathname + here.search + here.hash);
    }
  } catch (e) {}
  // againField: the field (again=1) that tells the server a send IS the
  // person's Send again — for the browser's own submit (native), which
  // carries only the form's fields. A script post builds its body (bodyOf).
  function againField(on) {
    var f = form.querySelector('input[name="` + SendAgainField + `"]');
    if (on && !f) { f = document.createElement('input'); f.type = 'hidden'; f.name = '` + SendAgainField + `'; f.value = '1'; form.appendChild(f); }
    if (!on && f) f.parentNode.removeChild(f);
  }
  againField(false);
  // 🔴 "SEND AGAIN" IS THE ONLY RETRY OF A SEND THAT MAY HAVE LANDED, where
  // there is no bubble to carry it (a Terminal view, a Queue): the server
  // holds the message by its TEXT under any nonce (internal/api
  // sendNonces.hold), so this button sends it with a NEW nonce and again=1.
  var againBtn = null;
  function offerAgain(p) {
    if (!againBtn) {
      againBtn = document.createElement('button');
      againBtn.type = 'button';
      againBtn.id = 'mv-send-again';
      againBtn.className = '` + composerSendClass + `';
      againBtn.textContent = 'Send again';
      note.parentNode.insertBefore(againBtn, note.nextSibling);
    }
    againBtn.hidden = false;
    againBtn.onclick = function () {
      againBtn.hidden = true;
      say('');
      if (input.value === p.text) clearBox();
      start({ text: p.text, typed: p.typed, queued: p.queued, to: p.to, again: true });
    };
  }
  function dropAgain() { if (againBtn) againBtn.hidden = true; }
  // The Queue button (send_queue.go), when the composer has one: a submit
  // whose formaction is the queue route.
  var queueBtn = document.getElementById('mv-queue-send');
  // 🔴 WHICH BUTTON SUBMITTED, WHERE e.submitter IS MISSING (older Safari):
  // the last one clicked. Guessing "Send" for a Queue tap would deliver the
  // message NOW, which is the one thing the person asked not to happen.
  var clicked = null;
  if (queueBtn) queueBtn.addEventListener('click', function () { clicked = queueBtn; });
  send.addEventListener('click', function () { clicked = null; });
  function clearBox() {
    input.value = '';
    if (window.musterComposerFit) window.musterComposerFit(input);
    // Its draft goes with it: the pending send holds the text now.
    if (window.musterDraft) window.musterDraft.sent(input);
  }
  // restore puts a message that did not go back in the box — only into an
  // empty box: what was typed since is not overwritten (the bubble's Retry
  // still has it).
  function restore(t) {
    if (input.value.trim()) return;
    input.value = t;
    if (window.musterComposerFit) window.musterComposerFit(input);
    if (window.musterDraft) window.musterDraft.sent(input);
  }
  // bodyOf is the form's fields with this message's text, nonce and again.
  function bodyOf(p) {
    var b = new URLSearchParams();
    for (var i = 0; i < form.elements.length; i++) {
      var el = form.elements[i];
      if (!el.name || el.disabled || el.name === '` + SendAgainField + `') continue;
      if (el === input) b.append(el.name, p.text);
      else if (el === nonce) b.append(el.name, p.nonce);
      else b.append(el.name, el.value);
    }
    if (p.again) b.append('` + SendAgainField + `', '1');
    return b.toString();
  }
  // native hands the message to the browser: an ordinary form submission,
  // which the server answers with a redirect back to this view. Its text goes
  // back in the box for it, and the pending entry goes: the page it lands on
  // says what happened (the draft's own pending covers the text).
  function native(p) {
    forget(p);
    if (p.el) p.el.remove();
    input.value = p.text;
    if (nonce) nonce.value = p.nonce;
    againField(!!p.again);
    if (p.queued && queueBtn) form.setAttribute('action', queueBtn.getAttribute('formaction'));
    if (window.musterDraft) window.musterDraft.pending(input);
    browserSubmit();
  }
  // browserSubmit is the browser's own submit of the form, as it stands.
  function browserSubmit() { HTMLFormElement.prototype.submit.call(form); }

  // ---- The pending sends, kept for a reload (sessionStorage). ----
  var KEY = input.getAttribute('data-draft-key') || '';
  function store() { try { return KEY ? window.sessionStorage : null; } catch (e) { return null; } }
  function readAll() {
    var s = store(); if (!s) return [];
    try { var a = JSON.parse(s.getItem('` + pendingStoreKey + `') || '[]'); return Array.isArray(a) ? a : []; } catch (e) { return []; }
  }
  function writeAll(a) {
    var s = store(); if (!s) return;
    try { if (a.length) s.setItem('` + pendingStoreKey + `', JSON.stringify(a)); else s.removeItem('` + pendingStoreKey + `'); } catch (e) {}
  }
  function keep(p) {
    // Only a bubble is kept: a Terminal view's send has no row to settle it.
    if (p.queued || !log) return;
    var a = readAll().filter(function (x) { return x.id !== p.id; });
    a.push({ id: p.id, k: KEY, text: p.text, typed: p.typed, to: p.to, at: p.at, state: p.state, reason: p.reason || '', again: !!p.again });
    writeAll(a);
  }
  function forget(p) { writeAll(readAll().filter(function (x) { return x.id !== p.id; })); }

  // ---- What a pending send looks like. ----
  var live = [];
  var LABEL = {
    sending: ` + strconv.Quote(PendingSendingText) + `,
    checking: ` + strconv.Quote(PendingCheckingText) + `,
    sent: ` + strconv.Quote(PendingSentText) + `,
    stepup: ` + strconv.Quote(PendingStepUpText) + `,
    queuing: ` + strconv.Quote(PendingQueuingText) + `,
    unknown: ` + strconv.Quote(PendingUnknownText) + `
  };
  function tail() { var t = document.getElementById('` + mirrorAsksID + `'); return t && log && t.parentNode === log ? t : null; }
  function draw(p) {
    var el;
    if (p.queued ? !qbox : !log) { paint(p); return; }
    if (p.queued) {
      el = document.createElement('div');
      el.className = '` + pendingQueueClass + `';
      var chip = document.createElement('span');
      chip.className = 'mt-0.5 shrink-0 rounded-full bg-s2 px-2 py-0.5 text-[11px] font-semibold text-fg2';
      chip.setAttribute('data-pending-status', '');
      chip.setAttribute('role', 'status');
      var pv = document.createElement('p');
      pv.className = 'line-clamp-2 min-w-0 flex-1 text-xs text-fg [overflow-wrap:anywhere]';
      pv.textContent = p.text;
      el.appendChild(chip); el.appendChild(pv);
      qbox.appendChild(el);
    } else {
      el = document.createElement('div');
      var body = document.createElement('div');
      body.className = 'whitespace-pre-wrap [overflow-wrap:anywhere]';
      body.textContent = p.text;
      var st = document.createElement('p');
      st.setAttribute('data-pending-status', '');
      st.setAttribute('role', 'status');
      st.className = 'mt-1 text-right text-[11px] text-muted';
      el.appendChild(body); el.appendChild(st);
      var empty = log.querySelector('[data-mirror-empty]');
      if (empty) empty.remove();
      log.insertBefore(el, tail());
      // Its own message: brought into view, whatever was being read.
      log.scrollTop = log.scrollHeight;
    }
    el.setAttribute('data-pending-id', p.id);
    p.el = el;
    paint(p);
  }
  // paint shows p's state on its bubble (or row, or the status line).
  function paint(p) {
    var el = p.el;
    if (!el) {
      // No conversation to draw in (a Terminal view): the status line.
      if (p.state === 'failed') return;
      if (p.state === 'sent') { say(''); return; }
      sayOK(LABEL[p.state] || '');
      return;
    }
    el.setAttribute('data-pending-send', p.state);
    var st = el.querySelector('[data-pending-status]');
    var bad = p.state === 'failed' || p.state === 'unknown';
    if (!p.queued) el.className = bad ? '` + pendingFailedClass + `' : '` + pendingBubbleClass + `';
    st.textContent = p.state === 'failed' ? ` + strconv.Quote(PendingFailedText) + ` + (p.reason ? ': ' + p.reason : '') : (LABEL[p.state] || '');
    if (st.classList) st.classList.toggle('text-st-error-fg', bad);
    var acts = el.querySelector('[data-pending-actions]');
    if (bad && !acts) {
      acts = document.createElement('div');
      acts.setAttribute('data-pending-actions', '');
      acts.className = 'mt-1 flex justify-end gap-2';
      var r = document.createElement('button');
      r.type = 'button'; r.className = '` + pendingActionClass + `'; r.textContent = 'Retry';
      r.setAttribute('data-pending-retry', '');
      r.addEventListener('click', function () { retry(p); });
      var d = document.createElement('button');
      d.type = 'button'; d.className = '` + pendingActionClass + `'; d.textContent = 'Dismiss';
      d.addEventListener('click', function () { settle(p); });
      acts.appendChild(r); acts.appendChild(d);
      el.appendChild(acts);
    } else if (!bad && acts) acts.remove();
    // The send the server holds as possibly delivered: its Retry is Send again.
    var rb = el.querySelector('[data-pending-retry]');
    if (rb && p.state === 'unknown') {
      var prev = document.getElementById('mv-send-again');
      if (prev && prev !== rb) {
        if (prev === againBtn) { againBtn.remove(); againBtn = null; }
        else { prev.removeAttribute('id'); prev.textContent = 'Retry'; }
      }
      rb.id = 'mv-send-again';
      rb.textContent = 'Send again';
    }
  }
  function set(p, state, reason) {
    clearTimeout(p.timer);
    p.state = state;
    if (reason !== undefined) p.reason = reason;
    paint(p);
    if (state === 'sent' || state === 'failed' || state === 'unknown' || state === 'checking' || state === 'sending') keep(p);
    if (state === 'sent') p.timer = setTimeout(function () { settle(p); }, ` + strconv.Itoa(pendingSentSettleMS) + `);
    if (state === 'checking') p.timer = setTimeout(function () { if (p.state === 'checking') set(p, 'unknown'); }, ` + strconv.Itoa(pendingRestoreSettleMS) + `);
  }
  // settle: p is done (its row arrived, it was dismissed, retried or removed).
  function settle(p) {
    clearTimeout(p.timer);
    p.done = true;
    if (p.el) p.el.remove();
    p.el = null;
    forget(p);
    live = live.filter(function (x) { return x !== p; });
  }
  function retry(p) {
    settle(p);
    if (input.value === p.text) clearBox();
    // A message that may have landed is sent again ON PURPOSE (again=1).
    start({ text: p.text, typed: p.typed, queued: p.queued, to: p.to, again: p.again || p.state === 'unknown' });
  }

  // ---- Matching a bubble to the row it stands for. ----
  var normRe = null;
  try { normRe = new RegExp('[^\\p{L}\\p{N}]+', 'gu'); } catch (e) { normRe = /[\W_]+/g; }
  function norm(s) { return String(s || '').toLowerCase().replace(normRe, ''); }
  var used = typeof WeakSet === 'function' ? new WeakSet() : null;
  // seenRows: the user rows on the page now. A bubble drawn now is never
  // settled by one of them.
  function seenRows() {
    var s = typeof WeakSet === 'function' ? new WeakSet() : null;
    if (!s || !log) return null;
    var rs = log.querySelectorAll('[data-bubble="user"]');
    for (var i = 0; i < rs.length; i++) s.add(rs[i]);
    return s;
  }
  // p.typed is what the person typed: the row draws an attachment line as a
  // chip, not as its text, so only the typed part is compared.
  function matches(p, text) {
    var a = norm(p.typed !== undefined ? p.typed : p.text).slice(0, ` + strconv.Itoa(pendingMatchChars) + `), b = norm(text);
    if (!a) return true;
    return b.indexOf(a) === 0;
  }
  // reconcile settles every pending send whose row is now on the page, then
  // keeps the rest at the conversation's end. mirrorViewScript calls it after
  // each batch it draws — in the SAME task, so the row and its bubble are
  // never on screen together.
  function reconcile() {
    if (!log) return;
    var open = live.filter(function (p) { return !p.queued && !p.done && p.el && p.state !== 'failed' && p.state !== 'stepup'; });
    if (open.length) {
      var rows = Array.prototype.slice.call(log.querySelectorAll('[data-bubble="user"]'));
      var queued = Array.prototype.slice.call(document.querySelectorAll('#` + ccQueuedID + ` [data-cc-queued-item]'));
      open.forEach(function (p) {
        var cand = rows.filter(function (r) { return !(used && used.has(r)) && !(p.before && p.before.has(r)); });
        if (!p.before) cand = cand.slice(-open.length);
        for (var i = 0; i < cand.length; i++) {
          if (matches(p, cand[i].textContent)) { if (used) used.add(cand[i]); settle(p); return; }
        }
        // Held by Claude Code itself: the queued region shows it.
        for (var j = 0; j < queued.length; j++) {
          if (matches(p, queued[j].textContent)) { settle(p); return; }
        }
      });
    }
    // What is still pending stays the newest thing in the conversation.
    var t = tail();
    live.forEach(function (p) { if (!p.queued && p.el && p.el.parentNode === log) log.insertBefore(p.el, t); });
  }
  window.musterPendingSends = {
    reconcile: reconcile,
    // kept: the bubbles a reset of the conversation keeps (mirrorViewScript's
    // clearLog). A reset redraws every row, so none is "seen" any more.
    kept: function () {
      live.forEach(function (p) { p.before = null; });
      return live.filter(function (p) { return !p.queued && p.el; }).map(function (p) { return p.el; });
    }
  };

  var leaving = false;
  window.addEventListener('pagehide', function () { leaving = true; });
  // A beforeunload that does not end in leaving (a download link) ends here:
  // a page that did leave runs no timer.
  window.addEventListener('beforeunload', function () { leaving = true; setTimeout(function () { leaving = false; }, 3000); });
  window.addEventListener('pageshow', function () { leaving = false; });

  // ---- Sending. ----
  // One at a time, in the order pressed.
  var chain = Promise.resolve();
  function start(o) {
    // A message sent again supersedes its failed or unconfirmed bubble.
    live.slice().forEach(function (x) { if (!x.queued && x.text === o.text && (x.state === 'failed' || x.state === 'unknown')) settle(x); });
    var p = { id: hex(8) || String(Date.now()) + String(Math.random()).slice(2), text: o.text, typed: o.typed, queued: !!o.queued, to: o.to, again: !!o.again,
      at: Date.now(), nonce: hex(16), state: o.queued ? 'queuing' : 'sending', before: o.queued ? null : seenRows() };
    live.push(p);
    draw(p);
    if (!p.queued) keep(p);
    chain = chain.then(function () { return post(p); }, function () { return post(p); });
    return p;
  }
  // post sends p and settles its promise when it has an answer — a step-up
  // included: the next message waits for this one's confirm.
  function post(p) {
    if (p.done) return;
    var req, body = bodyOf(p);
    try {
      // 🔴 keepalive: THE BOX EMPTIES AT THE PRESS, SO A PERSON CAN LEAVE THE
      // PAGE WHILE THE MESSAGE IS STILL ON ITS WAY — and a request the
      // browser abandons mid-paste is one the host never confirms, which the
      // server then holds as "may have been delivered" (measured: the next
      // send of the same text was refused, Not sent again). A keepalive
      // request outlives the page; the page it leaves for settles the bubble
      // (kept above). The browser caps keepalive bodies at 64 KiB in flight,
      // so a body near that goes as an ordinary request.
      req = fetch(p.to, { method: 'POST', body: body, credentials: 'same-origin', keepalive: body.length < 48000,
        headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'X-Requested-With': 'XMLHttpRequest' } });
    } catch (e) { native(p); return; }
    return req.then(function (r) {
      if (r.ok) {
        dropAgain();
        if (p.queued) {
          sayOK('Queued.');
          // The list's next draw (send_queue.go, at once on mv-queued) has the
          // server's row: it replaces this one in the same paint.
          p.state = 'queued';
          if (p.el) p.el.setAttribute('data-pending-send', 'queued');
          live = live.filter(function (x) { return x !== p; });
          document.dispatchEvent(new Event('mv-queued'));
        } else if (!p.el) {
          // A Terminal view: the pane shows it; nothing to settle.
          say('');
          settle(p);
        } else if (!p.done) {
          set(p, 'sent');
          // The row may already be here (it can arrive before the answer).
          reconcile();
        }
        // What was sent, for the suggested replies' edit distance
        // (suggest_row.go): it stays on this page.
        document.dispatchEvent(new CustomEvent('mv-sent', { detail: { text: p.text, queued: p.queued } }));
        return;
      }
      if (r.status === 403 && r.headers.get('X-Muster-Step-Up')) {
        var stepUp = window.musterStepUp;
        if (!stepUp) { native(p); return; }
        stepUp.stale();
        if (!p.queued) set(p, 'stepup'); else if (!p.el) sayOK(LABEL.stepup);
        return new Promise(function (resolve) {
          // The SAME request again, nonce and all: nothing was delivered.
          stepUp.ask('', function () { if (!p.queued) set(p, 'sending'); return Promise.resolve(post(p)).then(resolve, resolve); },
            function () { refused(p, ` + strconv.Quote(pendingCancelled) + `, false); resolve(); });
        });
      }
      var again = !!r.headers.get('` + SendAgainHeader + `');
      return r.text().then(function (t) {
        refused(p, t.trim() || ((p.queued ? 'Not queued' : 'Not sent') + ' (HTTP ' + r.status + ')'), again);
      });
    }, function () {
      // 🔴 THE PAGE IS GOING (a reload, a navigation): the browser abandoned
      // the request, which is not a failure to hand to the browser's own
      // submit — that would post it again from a page being unloaded. Its
      // entry stays, and the page that comes next shows it (Checking…).
      if (leaving) return;
      if (navigator.onLine === false) { refused(p, form.getAttribute('data-down'), false); return; }
      native(p);
    }).then(null, function () { native(p); });
  }
  // refused: p did not go. Its reason is said, its text goes back in the box,
  // and its bubble turns failed with Retry — or, with no bubble (a Queue, a
  // Terminal view), the box and the status line are the retry, and a send
  // that may have landed offers Send again.
  function refused(p, reason, again) {
    p.again = again;
    say(reason);
    // Its row already arrived (the conversation shows it): nothing to undo.
    if (p.done) return;
    restore(p.text);
    // 🔴 MAY HAVE LANDED: not "failed" — it is unknown, and its Retry is the
    // composer's Send again (#mv-send-again, the one such button on the page).
    if (p.el && !p.queued) { set(p, again ? 'unknown' : 'failed', reason); return; }
    settle(p);
    if (again) offerAgain(p);
  }

  form.addEventListener('submit', function (e) {
    // From here on a failure of THIS handler leaves the event alone, and the
    // browser submits the form itself.
    if (input.disabled) { e.preventDefault(); return; }
    var att = window.musterAttach;
    if (att && att.pending()) { e.preventDefault(); say(att.pendingMessage); return; }
    if (!input.value.trim() && !(att && att.count())) { e.preventDefault(); say(form.getAttribute('data-empty')); input.focus(); return; }
    if (typeof fetch !== 'function' || typeof URLSearchParams !== 'function' || typeof Promise !== 'function') return;
    var sub = ('submitter' in e) ? e.submitter : clicked;
    clicked = null;
    var queued = !!(sub && sub === queueBtn);
    // The message as sent: the text, then a line per attachment (attach.go).
    var typed = input.value;
    var text = att ? att.compose(typed) : typed;
    // A plain Send is never a Send again: that is the bubble's Retry, or
    // the button offered for it.
    var again = !!form.querySelector('input[name="` + SendAgainField + `"]');
    e.preventDefault();
    say('');
    dropAgain();
    againField(false);
    clearBox();
    if (att) att.sent();
    renew();
    try { start({ text: text, typed: typed, queued: queued, to: queued ? sub.getAttribute('formaction') : form.getAttribute('action'), again: again }); }
    catch (err) { input.value = text; browserSubmit(); }
  });
  input.addEventListener('input', function () { if (input.value.trim()) say(''); });
  input.addEventListener('keydown', function (e) {
    if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      // 🔴 A HELD ENTER SENDS ONCE: its auto-repeats are not more sends (a
      // focus that moved into the box mid-hold must not send what it holds).
      if (e.repeat) return;
      // requestSubmit is missing from older Safari; a click on Send is the same
      // submit, through the same handler.
      if (form.requestSubmit) form.requestSubmit(); else send.click();
    }
  });

  // ---- After a reload: this box's sends that were still out. ----
  if (log) (function () {
    var now = Date.now(), all = readAll(), mine = [], rest = [];
    all.forEach(function (x) {
      if (!x || typeof x.text !== 'string' || !(now - x.at < ` + strconv.Itoa(pendingMaxAgeMS) + `)) return;
      if (x.k === KEY && KEY) mine.push(x); else rest.push(x);
    });
    writeAll(rest);
    mine.forEach(function (x) {
      var p = { id: x.id, text: x.text, typed: typeof x.typed === 'string' ? x.typed : undefined, queued: false, to: x.to, again: !!x.again, at: x.at, nonce: hex(16), state: 'checking', before: null };
      live.push(p);
      draw(p);
      if (x.state === 'failed' || x.state === 'unknown') set(p, x.state, x.reason);
      else if (x.state === 'sent') set(p, 'sent');
      else set(p, 'checking');
    });
    // The conversation may already be drawn (a snapshot): settle what it shows.
    if (mine.length) reconcile();
  })();
})();
`
