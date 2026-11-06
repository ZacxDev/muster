package ui

import "strconv"

// A SENT MESSAGE SHOWS AT ONCE, AS PENDING, AND IS RECONCILED WHEN IT LANDS
// (mirrorComposerScript). The operator's ask: no hang between pressing Send
// and seeing the message go out. Before this, Send was disabled and the text
// sat in the box until the host answered — a whole tunnel round trip and a
// paste (measured in e2e/tests/optimistic-send.spec.ts: ~1.8 s with a 1.5 s
// host delay injected, and nothing drawn in the conversation until then).
//
// 🔴 THE INVARIANT EVERYTHING BELOW SERVES: A TYPED MESSAGE IS NEVER SILENTLY
// LOST AND NEVER SENT TWICE.
//
// What the press does now, in the same task:
//
//   - the box is emptied (focus and the keyboard stay: sendKeepsFocusJS), but
//     its DRAFT is not: the message is held in the box's draft
//     (composer_draft.go, musterDraft.hold) until the server answers OK, or
//     the person Dismisses it or sends it again — so a reload, a navigation
//     or a failed browser submit mid-send brings the text back in the box as
//     a draft, and a message the server took never does. Held messages are
//     saved in the order pressed, a refused one put back in the box in its
//     place;
//   - on the Rendered view the message is drawn at the END of the
//     conversation as a pending bubble (data-pending-send): muted, with a
//     status line ("Sending…", role=status). A Queue has no bubble (it is not
//     sent): its status line (#mv-note) says "Queuing…", as a Terminal view's
//     says "Sending…";
//   - the request goes out. Sends from one box are posted ONE AT A TIME, in
//     the order pressed (a second Send while the first is out is drawn at
//     once and posted after it), so two messages cannot overtake each other
//     on the way to the pane.
//
// Then, by what comes back:
//
//   - OK: the bubble says "Sent" and the draft lets the message go. 🔴 THE
//     BUBBLE IS NOT REMOVED THEN: it is removed in the same DOM task that
//     draws the transcript row it stands for (reconcile, called from
//     mirrorViewScript's apply) — or the row of "Queued in Claude Code"
//     (cc_queued.go) when Claude Code holds it — so there is never a frame
//     with both or with neither. A sent bubble no row ever matches (a
//     `/command` Claude Code records differently) is removed after
//     pendingSentSettleMS: the host confirmed it.
//   - a step-up (403 + X-Muster-Step-Up): the bubble waits, "Confirm it's
//     you to send", and the SAME request (same nonce) is posted after the
//     confirm. Cancelling the dialog fails it.
//   - any refusal (4xx/5xx, rate limit, the window gone, Claude waiting on a
//     prompt) or offline: the bubble turns failed with the reason and Retry
//     and Dismiss, and its text is put back in the box if the box is empty.
//     The reason is said ONCE: on the bubble, or — where there is no bubble
//     (a Terminal view, a Queue) — in #mv-note, with Retry (or Send again)
//     and Dismiss beside it. #mv-note says the NEWEST message's status: a
//     later "Sending…" or "Queued." takes it, and the refusal's reason moves
//     beside its Retry and Dismiss, which stay until it is retried or
//     dismissed; when the later message is done, the line is the refusal's
//     again. Text
//     typed since is never overwritten: the message's Retry and the draft
//     still have it. A refusal that MAY
//     have been delivered (X-Muster-Send-Again) turns the bubble UNKNOWN
//     instead, and its button is the composer's Send again (#mv-send-again,
//     again=1): see SendAgainHeader. A new send of the same text replaces a
//     failed or unknown message of it, as its Retry.
//   - 🔴 A REFUSAL HOLDS WHAT CAME AFTER IT. The messages sent after it that
//     have not been posted yet WAIT ("Waiting — an earlier message wasn't
//     sent") until the person Retries the refused one (it then goes first)
//     or Dismisses it. Posting them anyway would deliver a later message
//     before an earlier one. A NEW message sent once the refusal is shown is
//     the person moving on: what waited goes, in order, then it; the refused
//     message keeps its Retry (or Send again) and Dismiss — on its bubble,
//     or, with no bubble, beside the status line with its reason — and,
//     retried later, goes after them.
//   - no answer while online: the browser submits the form itself (native),
//     as before, with the text put back for it. Shown again from the
//     back/forward cache after that, the page settles that message by the
//     draft (delivered: out of the box; not known: refused, Retry) BEFORE
//     anything behind it is posted.
//   - no answer within pendingUnloadWaitMS of a beforeunload: the page may be
//     going, so nothing is decided until it is known to still be here — then
//     it is refused (text kept, Retry), and nothing behind it goes first.
//
// 🔴 ONLY A MESSAGE THE SERVER ANSWERED OK ("Sent") IS EVER SETTLED BY A ROW,
// and the match only TIDIES: it removes a bubble whose message the server
// already confirmed (its draft was let go at the OK). A wrong match is
// cosmetic — a "Sent" bubble removed a little early or late — never a
// delivery decision: it cannot release another message's draft or settle a
// message that is not "Sent". The one message OUT NOW ("Sending…", posted,
// no answer yet) is matched too, because its row can be drawn before the OK
// reaches the page (the host tails the transcript on its own tick): its
// bubble then STANDS ASIDE — off the page in the task that draws the row, so
// there is no frame with both — and its answer decides as before: the OK
// settles it, anything else draws it again with its row given back. A message
// still waiting its turn is never a candidate, and a waiting message is
// always posted.
//
// 🔴 A "NOT CONFIRMED" (UNKNOWN) MESSAGE IS NEVER SETTLED BY A ROW (operator
// decision, PR #450 round 4). A row is matched by its text, and a text match
// cannot tell this message's row from an earlier one's, a later one's that
// begins with it, or a retyped copy's: each such guess settled a message that
// never landed, silently losing it. So it keeps its text (the box, or the
// draft) and its Send again and Dismiss until the person uses one — the
// server sends the page no later confirmation of it. The person checks the
// terminal or the transcript to see whether it landed.
//
// 🔴 HOW A "SENT" BUBBLE IS MATCHED TO ITS ROW. Nothing the page sends survives into
// Claude Code's transcript — the host pastes the text as typed — so the
// match is by the TEXT, compared against the row's RAW prompt (data-prompt,
// MirrorTranscript.Ops: what the transcript holds, before markdown draws a
// link as its label or a fence as a code block), falling back to the row's
// drawn text. Both are reduced to letters and digits, lower-cased, and the
// row must begin with the first pendingMatchChars characters of what was
// TYPED (an attachment's line is drawn in the row as a chip). Only a user row
// that was NOT on the page when the bubble was drawn can match it, and each
// row settles at most one bubble, so a repeated "yes" is not settled by the
// old one. A message with nothing to compare (an emoji, attachments only) is
// matched by POSITION: the first new row that has nothing to compare either. "Queued in Claude Code" rows
// are held to the same rule: only one beyond those shown at the press. (After
// a reset redraws the conversation every row is new; then only the newest
// rows, one per bubble, are candidates.)
//
// The bubble, the note and the hold are all this page's. Nothing is kept for
// a reload except the draft: the page after a reload shows the message in the
// box, as a draft, and a person decides whether to send it.

// Pending sends' visible words. The e2e spec and the docs say these.
const (
	PendingSendingText = "Sending…"
	PendingSentText    = "Sent"
	PendingStepUpText  = "Confirm it's you to send"
	PendingQueuingText = "Queuing…"
	PendingFailedText  = "Not sent"
	PendingUnknownText = "Not confirmed: it may or may not have been sent."
	// PendingUnknownPrefix leads the server's reason on a bubble that may
	// have been delivered.
	PendingUnknownPrefix = "Not confirmed — "
	PendingWaitingText   = "Waiting — an earlier message wasn't sent"
	pendingCancelled     = "you did not confirm it's you."
	// pendingNetworkFailed: the request failed while the page stayed.
	pendingNetworkFailed = "the connection failed before the server answered."
	// pendingHandOffUnknown: back from the back/forward cache, the browser's
	// own submit of it is not known to have gone.
	pendingHandOffUnknown = "your browser's send of it did not say it went."
)

// Pending sends' times and bounds.
const (
	// pendingSentSettleMS: a send the host confirmed that no row has matched
	// in this long is removed (it was delivered; the conversation is what
	// shows it).
	pendingSentSettleMS = 30_000
	// pendingMatchChars: how much of a message must begin its row.
	pendingMatchChars = 200
	// pendingUnloadWaitMS: after a beforeunload, how long a failed request
	// waits to see whether the page is leaving before it is a failure.
	pendingUnloadWaitMS = 3_000
)

// pendingBubbleClass is a pending bubble: the user bubble's shape and side
// (chatBubbleOf), muted. pendingFailedClass is a failed or unknown one.
const (
	pendingBubbleClass = "max-w-[85%] self-end break-words rounded-2xl bg-accent/10 px-3 py-2 text-sm text-fg2 ring-1 ring-inset ring-accent/30 opacity-70"
	pendingFailedClass = "max-w-[85%] self-end break-words rounded-2xl bg-danger/10 px-3 py-2 text-sm text-fg ring-1 ring-inset ring-danger/40"
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
  // The conversation (the Rendered view's).
  var log = document.getElementById('mv-log');
  function say(msg) { note.textContent = msg || ''; note.hidden = !msg; note.removeAttribute('data-ok'); }
  function sayOK(msg) { say(msg); note.setAttribute('data-ok', ''); }
  // noteBy: the message the status line is about, when it is about one. The
  // line says the NEWEST message's status ("Sending…", "Queued.", a refusal),
  // so a later message's progress is never hidden. A message only clears its
  // OWN line, and when it does the line goes back to the open refusal, if
  // there is one: a later one's "Sent" never wipes an earlier one's refusal.
  // While the line is about another message, the refusal's reason is said
  // beside its own buttons instead (offer) — still once.
  var noteBy = null;
  function sayFor(p, msg, ok) { noteBy = p; if (ok) sayOK(msg); else say(msg); offer(); }
  function clearFor(p) {
    if (noteBy !== p) return;
    noteBy = null;
    var r = openRefusal(p);
    if (r) sayFor(r, r.reason || ''); else { say(''); offer(); }
  }
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
  // The Queue button (send_queue.go), when the composer has one: a submit
  // whose formaction is the queue route.
  var queueBtn = document.getElementById('mv-queue-send');
  // 🔴 WHICH BUTTON SUBMITTED, WHERE e.submitter IS MISSING (older Safari):
  // the last one clicked. Guessing "Send" for a Queue tap would deliver the
  // message NOW, which is the one thing the person asked not to happen.
  var clicked = null;
  if (queueBtn) queueBtn.addEventListener('click', function () { clicked = queueBtn; });
  send.addEventListener('click', function () { clicked = null; });

  // ---- The box and its draft. ----
  // (Read when used: the draft script may run after this one.)
  function D() { return window.musterDraft || null; }
  function fit() { if (window.musterComposerFit) window.musterComposerFit(input); }
  // hold/release: the message stays in the box's draft from the press until
  // the server's OK (or the person dismisses it, or sends it again), so the
  // page going away loses nothing, and a message the server took never comes
  // back. A row never releases it.
  function hold(p) { var d = D(); if (d && d.hold) d.hold(input, p.id, p.text, p.seq); }
  function release(p) { var d = D(); if (d && d.release) d.release(input, p.id); }
  // saveBox: the box's draft as the box stands now.
  function saveBox() { var d = D(); if (d && d.sent) d.sent(input); }
  function clearBox() { input.value = ''; fit(); }
  // shown reveals the box when it holds text (composer_reveal.go): every
  // write of a text into it below calls it.
  // focusBox focuses the box, revealing it first where it is collapsed
  // (composer_reveal.go): a hidden box takes no focus.
  function focusBox() { if (window.musterComposerReveal) window.musterComposerReveal(input, true); else input.focus(); }
  function shown() { if (input.value) { ` + composerRevealCallJS("input") + ` } }
  // restore puts a message that did not go back in the box — only into an
  // empty box: what was typed since is not overwritten (the message's Retry,
  // and the draft, still have it). It reports whether the box has it now.
  // 🔴 It stays HELD (in its place, as the box's text): it is not delivered,
  // and a refused message and one held behind it are saved in pressed order.
  function restore(p) {
    var t = p.text, d = D();
    if (input.value !== t) {
      if (input.value.trim()) return false;
      input.value = t;
      shown();
      fit();
    }
    if (d && d.inBox) d.inBox(input, p.id); else release(p);
    return true;
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
  // which the server answers with a redirect back to this view. 🔴 The draft
  // is saved FIRST, with every message still held and what was typed since:
  // if this submit fails too (no network for the browser either), the page
  // after it — a Back, a reload — has all of it back in the box. The page it
  // lands on takes this message out of the draft only if it says it went
  // (composer_draft.go, musterDraft.pending).
  function native(p) {
    // The page is leaving with p: nothing after it is posted from here (the
    // draft, saved just below, has them all).
    handedOff = { p: p, box: input.value, action: form.getAttribute('action') };
    var d = D();
    if (d) d.sent(input);
    if (p.el) p.el.remove();
    p.el = null;
    if (p.aside && used) used.delete(p.aside);
    p.aside = null;
    input.value = p.text;
    shown();
    if (nonce) nonce.value = p.nonce;
    againField(!!p.again);
    if (p.queued && queueBtn) form.setAttribute('action', queueBtn.getAttribute('formaction'));
    if (d) d.pending(input);
    browserSubmit();
  }
  // browserSubmit is the browser's own submit of the form, as it stands.
  function browserSubmit() { HTMLFormElement.prototype.submit.call(form); }

  // ---- What a pending send looks like. ----
  var live = [];
  var LABEL = {
    sending: ` + strconv.Quote(PendingSendingText) + `,
    sent: ` + strconv.Quote(PendingSentText) + `,
    stepup: ` + strconv.Quote(PendingStepUpText) + `,
    queuing: ` + strconv.Quote(PendingQueuingText) + `,
    held: ` + strconv.Quote(PendingWaitingText) + `,
    unknown: ` + strconv.Quote(PendingUnknownText) + `
  };
  function tail() { var t = document.getElementById('` + mirrorAsksID + `'); return t && log && t.parentNode === log ? t : null; }
  // bubbled: p is drawn in the conversation (a Send on the Rendered view).
  function bubbled(p) { return !p.queued && !!log; }
  function draw(p) {
    if (!bubbled(p)) { paint(p); return; }
    var el = document.createElement('div');
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
    el.setAttribute('data-pending-id', p.id);
    p.el = el;
    paint(p);
  }
  // paint shows p's state on its bubble, or — with no bubble — on the status
  // line.
  function paint(p) {
    var el = p.el;
    if (!el) {
      if (p.state === 'failed' || p.state === 'unknown') { sayFor(p, p.reason || ''); offer(); return; }
      if (p.state === 'held') return;
      if (p.state === 'sent') { clearFor(p); return; }
      // A later message's progress takes the line; an earlier refusal keeps
      // its reason and buttons beside it (offer) until it is retried or
      // dismissed.
      sayFor(p, LABEL[p.state] || '', true);
      return;
    }
    el.setAttribute('data-pending-send', p.state);
    var st = el.querySelector('[data-pending-status]');
    var bad = p.state === 'failed' || p.state === 'unknown';
    el.className = bad ? '` + pendingFailedClass + `' : '` + pendingBubbleClass + `';
    // The reason is the server's own words ("Pasted, not submitted: … Check
    // the terminal."), said once, here.
    st.textContent = p.state === 'failed' ? ` + strconv.Quote(PendingFailedText) + ` + (p.reason ? ': ' + p.reason : '')
      : p.state === 'unknown' && p.reason ? ` + strconv.Quote(PendingUnknownPrefix) + ` + p.reason : (LABEL[p.state] || '');
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
      d.addEventListener('click', function () { dismiss(p); });
      acts.appendChild(r); acts.appendChild(d);
      el.appendChild(acts);
    } else if (!bad && acts) acts.remove();
    // The send the server holds as possibly delivered: its Retry is Send again.
    var rb = el.querySelector('[data-pending-retry]');
    if (rb && p.state === 'unknown') {
      var prev = document.getElementById('mv-send-again');
      if (prev && prev !== rb) {
        if (prev === againBtn) hideOffer();
        // Another bubble's keeps its label (it is still a Send again); only
        // the one id moves.
        else prev.removeAttribute('id');
      }
      rb.id = 'mv-send-again';
      rb.textContent = 'Send again';
    }
  }
  function set(p, state, reason) {
    clearTimeout(p.timer);
    p.state = state;
    if (reason !== undefined) p.reason = reason;
    // Stood aside for its row (reconcile) and now answered OK: done, here and
    // now — its row is already on the page. Settled directly, so a detached
    // bubble never waits on a later reconcile or the pendingSentSettleMS
    // timer, and is never drawn again (unpark, below) to be matched anew.
    if (p.aside && state === 'sent') { settle(p); return; }
    paint(p);
    // Stood aside for a row (reconcile), and now anything but on its way
    // (failed, Not confirmed, a step-up): drawn again — the row decided
    // nothing.
    if (p.aside && state !== 'sending') unpark(p);
    if (state === 'sent' && p.el) p.timer = setTimeout(function () { settle(p); }, ` + strconv.Itoa(pendingSentSettleMS) + `);
  }
  // unpark draws p's bubble again, at the conversation's end, and gives back
  // the row it stood aside for.
  function unpark(p) {
    var row = p.aside;
    p.aside = null;
    if (used && row) used.delete(row);
    if (p.el && log && !p.el.parentNode) { log.insertBefore(p.el, tail()); log.scrollTop = log.scrollHeight; }
  }

  // ---- With no bubble (a Terminal view, a Queue): the refused message's
  // own buttons, beside the status line. Retry (#mv-send-retry) — or Send
  // again when it may have been delivered (the one such button on the page,
  // #mv-send-again) — and Dismiss, ALWAYS: the refused message is the
  // person's to retry or let go for as long as it is offered, whether or not
  // anything waits behind it. When later messages wait, the word that they
  // wait; when the status line is about a later message, the refusal's own
  // reason.
  var againBtn = null, offerBox = null, dismissBtn = null, heldNote = null;
  // openRefusal: the newest refused message with no bubble that is still
  // the person's to Retry or Dismiss (it no longer holds anything once a new
  // message was sent past it, but its offer stays). except: one that is
  // going (dismissed, or replaced by a send of it).
  function openRefusal(except) {
    for (var i = live.length - 1; i >= 0; i--) {
      var x = live[i];
      if (x !== except && !x.el && !x.done && (x.state === 'failed' || x.state === 'unknown')) return x;
    }
    return null;
  }
  function refusalText(p) {
    return p.state === 'unknown' ? ` + strconv.Quote(PendingUnknownPrefix) + ` + (p.reason || '')
      : ` + strconv.Quote(PendingFailedText) + ` + (p.reason ? ': ' + p.reason : '');
  }
  // noteHeld: the status line is a refused message's reason. A new press or
  // typing does not wipe it (a later message's own status replaces it, and
  // it comes back when that one is done).
  function noteHeld() { var x = noteBy; return !!(x && !x.el && !x.done && (x.state === 'failed' || x.state === 'unknown')); }
  function offer() {
    var p = blocker && !blocker.el ? blocker : openRefusal();
    if (!p || p.el) { hideOffer(); return; }
    var waiting = blocker === p && queue.length > 0;
    if (!offerBox) {
      offerBox = document.createElement('div');
      offerBox.setAttribute('data-send-offer', '');
      offerBox.className = 'flex flex-wrap items-center gap-2';
      againBtn = document.createElement('button');
      againBtn.type = 'button';
      againBtn.className = '` + composerSendClass + `';
      dismissBtn = document.createElement('button');
      dismissBtn.type = 'button';
      dismissBtn.className = '` + pendingActionClass + `';
      dismissBtn.textContent = 'Dismiss';
      heldNote = document.createElement('span');
      heldNote.className = 'text-xs text-muted';
      heldNote.setAttribute('data-pending-held', '');
      offerBox.appendChild(againBtn); offerBox.appendChild(dismissBtn); offerBox.appendChild(heldNote);
      note.parentNode.insertBefore(offerBox, note.nextSibling);
    }
    offerBox.hidden = false;
    // 🔴 "SEND AGAIN" IS THE ONLY RETRY OF A SEND THAT MAY HAVE LANDED: the
    // server holds the message by its TEXT under any nonce (internal/api
    // sendNonces.hold), so this sends it with a NEW nonce and again=1.
    againBtn.id = p.again ? 'mv-send-again' : 'mv-send-retry';
    againBtn.textContent = p.again ? 'Send again' : 'Retry';
    againBtn.onclick = function () { retry(p); };
    dismissBtn.hidden = false;
    dismissBtn.onclick = function () { dismiss(p); };
    // The reason is said once: on the status line while that is about p,
    // else here, beside p's buttons.
    var words = noteBy === p ? '' : refusalText(p);
    if (waiting) words = words ? words + ' · ' + LABEL.held : LABEL.held;
    heldNote.hidden = !words;
    heldNote.textContent = words;
  }
  function hideOffer() {
    if (!offerBox) return;
    offerBox.hidden = true;
    againBtn.removeAttribute('id');
  }

  // settle: p is done (it was "Sent" and its row arrived or its timer ran, it
  // was sent from a Terminal view, it was dismissed, or a new send of it
  // replaced it).
  function settle(p) {
    clearTimeout(p.timer);
    p.done = true;
    if (p.el) p.el.remove();
    p.el = null;
    live = live.filter(function (x) { return x !== p; });
    if (blocker === p) unblock();
  }
  // dismiss: the person let a refused message go. What waited behind it is
  // posted now. 🔴 A "NOT CONFIRMED" MESSAGE IS FORGOTTEN: it may have been
  // delivered, so its text leaves the box (while the box is still exactly
  // that text) and the draft — a reload must not bring it back for a later
  // Send to paste twice. Dismiss is the person's "done with it", after
  // checking the terminal. A FAILED one (the server said it did not go) keeps
  // its text where it is, the box if it went back there: it cannot be a
  // duplicate, and the words are the person's to edit or send.
  function dismiss(p) {
    var forget = p.state === 'unknown' && input.value === p.text;
    if (forget) clearBox();
    release(p);
    if (forget) saveBox();
    clearFor(p); settle(p); offer();
  }
  function retry(p) {
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
  // ccItems: the "Queued in Claude Code" rows now, by their compared text.
  function ccItems() {
    return Array.prototype.slice.call(document.querySelectorAll('#` + ccQueuedID + ` [data-cc-queued-item]')).map(function (el) { return norm(el.textContent); });
  }
  // key: what of p is compared — what the person TYPED (the row draws an
  // attachment line as a chip). Empty: nothing to compare (by position).
  function key(p) { return norm(p.typed !== undefined ? p.typed : p.text).slice(0, ` + strconv.Itoa(pendingMatchChars) + `); }
  // rowText: the row's raw prompt when it carries one (data-prompt), else
  // what it draws.
  function rowText(r) { var raw = r.getAttribute('data-prompt'); return raw !== null ? raw : r.textContent; }
  function begins(a, text) { return norm(text).indexOf(a) === 0; }
  // reconcile removes every SENT bubble whose row is now on the page (and
  // stands aside the one out now whose row is), then keeps the rest at the
  // conversation's end. mirrorViewScript calls it after each batch it draws —
  // in the SAME task, so the row and its bubble are never on screen together.
  function reconcile() {
    if (!log) return;
    // 🔴 ONLY "SENT" IS SETTLED: the server already confirmed these, and their
    // drafts were let go at the OK. A message still waiting its turn, asking
    // for a step-up, refused or NOT CONFIRMED is never settled by a row (see
    // the file's comment), so a wrong match below can only remove a "Sent"
    // bubble early or late, or hide the one out now until its answer —
    // settle() of a sent message touches nothing else.
    // 🔴 The one message OUT NOW (posted, "Sending…") is a candidate too: its
    // row can be drawn BEFORE the host's OK reaches the page (the host tails
    // the transcript on its own tick). Matched, its bubble only STANDS ASIDE
    // (aside): taken off the page in this same task, so the row and the bubble
    // are never on screen together — but nothing is decided. It is not done,
    // its draft is not let go, and its answer still settles it: the OK takes
    // it away for good (set → settle); anything else draws it again
    // (set → unpark), with its row given back. A wrong match can only hide a
    // bubble until its answer, never settle a message.
    var open = live.filter(function (p) { return p.el && !p.done && !p.aside && (p.state === 'sent' || (p === current && p.state === 'sending')); });
    if (open.length) {
      var rows = Array.prototype.slice.call(log.querySelectorAll('[data-bubble="user"]'));
      var cc = null, ccTaken = {};
      open.forEach(function (p) {
        var cand = rows.filter(function (r) { return !(used && used.has(r)) && !(p.before && p.before.has(r)); });
        if (!p.before) cand = cand.slice(-open.length);
        var a = key(p), hit = null;
        // Nothing to compare: by position — the first new row that has
        // nothing to compare either (an emoji's, an attachment's), never just
        // whatever row comes next.
        if (!a) { for (var k = 0; k < cand.length && !hit; k++) { if (!norm(rowText(cand[k]))) hit = cand[k]; } }
        else { for (var i = 0; i < cand.length && !hit; i++) { if (begins(a, rowText(cand[i]))) hit = cand[i]; } }
        if (hit) {
          if (used) used.add(hit);
          if (p.state === 'sent') settle(p);
          else { p.aside = hit; p.el.remove(); }
          return;
        }
        // The cc-queued region below settles; a message not yet answered OK
        // is never settled.
        if (p.state !== 'sent') return;
        // Held by Claude Code itself: the queued region shows it — one row
        // beyond as many of that text as it showed at the press, each row
        // for one bubble.
        if (!a) return;
        if (!cc) cc = ccItems();
        var n = 0;
        cc.forEach(function (t) { if (t.indexOf(a) === 0) n++; });
        var had = 0;
        (p.beforeCC || []).forEach(function (t) { if (t.indexOf(a) === 0) had++; });
        var taken = ccTaken[a] || 0;
        if (n > had + taken) { ccTaken[a] = taken + 1; settle(p); }
      });
    }
    // What is still pending stays the newest thing in the conversation.
    var t = tail();
    live.forEach(function (p) { if (p.el && p.el.parentNode === log) log.insertBefore(p.el, t); });
  }
  window.musterPendingSends = {
    reconcile: reconcile,
    // kept: the bubbles a reset of the conversation keeps (mirrorViewScript's
    // clearLog). A reset redraws every row, so none is "seen" any more.
    kept: function () {
      live.forEach(function (p) { p.before = null; });
      // One standing aside is not on the page, and a reset does not put it
      // back (its row is redrawn as a new one).
      return live.filter(function (p) { return p.el && !p.aside; }).map(function (p) { return p.el; });
    }
  };

  // ---- The page going away. ----
  // gone: pagehide fired (the page left, or went into the back/forward
  // cache). unloadUntil: a beforeunload fired, and until then the page may be
  // about to go. 🔴 A beforeunload does not always end in leaving (a download
  // link fires one and the page stays), so it never decides a failure by
  // itself: a request that failed meanwhile waits (whenHere) and is then a
  // failure, with its text kept and Retry, if the page is still here.
  var gone = false, unloadUntil = 0, onShow = [];
  window.addEventListener('pagehide', function () { gone = true; });
  window.addEventListener('beforeunload', function () { unloadUntil = Date.now() + ` + strconv.Itoa(pendingUnloadWaitMS) + `; });
  // whenHere runs fn once the page is known to still be here: after the
  // beforeunload's wait, or — for a page that went into the back/forward
  // cache — when it is shown again. A page that left for good runs nothing.
  function whenHere(fn) {
    if (gone) { onShow.push(fn); return; }
    var w = unloadUntil - Date.now();
    if (w > 0) { setTimeout(function () { whenHere(fn); }, w + 50); return; }
    fn();
  }
  window.addEventListener('pageshow', function (e) {
    gone = false;
    unloadUntil = 0;
    if (!e.persisted) return;
    // Back from the back/forward cache after a message was handed to the
    // browser's own submit (native): settle THAT message before the queue
    // runs again.
    if (handedOff) backFromHandOff();
    var fs = onShow; onShow = [];
    fs.forEach(function (fn) { fn(); });
    pump();
  });
  // backFromHandOff: this page is shown again after it handed p to the
  // browser's own submit, with p in the box (native put it there). 🔴 The page
  // that submit landed on took p out of the draft only if it said p went
  // (composer_draft.go, musterDraft.pending), so the draft is the answer:
  //   - p is not in it: p was delivered. The box gets back what was typed in
  //     it before the hand-off, and p is done — sending the restored box
  //     would have sent p a second time;
  //   - p is in it (or nothing can be read): it is not known to have gone. It
  //     is refused, with its text and Retry, and what came after it waits —
  //     the server refuses a Retry of a text it already delivered
  //     (X-Muster-Send-Again), so it is never sent twice unasked.
  function backFromHandOff() {
    var h = handedOff, p = h.p, d = D();
    handedOff = false;
    if (d && d.thaw) d.thaw(input);
    var st = d && d.stored ? d.stored(input) : null;
    input.value = h.box;
    shown();
    fit();
    if (nonce) renew();
    againField(false);
    form.setAttribute('action', h.action);
    if (st !== null && st.indexOf(p.text) < 0) { release(p); saveBox(); settle(p); return; }
    if (bubbled(p)) { p.state = 'sending'; draw(p); }
    refused(p, ` + strconv.Quote(pendingHandOffUnknown) + `, false);
  }

  // ---- Sending: one at a time, in the order pressed. ----
  // queue: the messages not posted yet, in order. current: the one out now.
  // blocker: a refused message the ones in queue wait behind.
  // handedOff: the message handed to the browser's own submit ({p, box,
  // action}), if any. seqN: the order of the presses (the draft saves in it).
  var queue = [], current = null, blocker = null, handedOff = false, seqN = 0;
  function paintQueue() {
    queue.forEach(function (p) {
      var st = blocker ? 'held' : (p.queued && !p.el ? 'queuing' : 'sending');
      if (p.state !== st) set(p, st);
    });
    offer();
  }
  function unblock() { blocker = null; paintQueue(); pump(); }
  function pump() {
    if (current || blocker || handedOff || !queue.length) return;
    var p = queue.shift();
    current = p;
    var next = function () { if (current === p) current = null; pump(); };
    var r;
    try { r = post(p); } catch (e) { r = null; try { console.error('composer: a send failed', e); } catch (e2) {} }
    Promise.resolve(r).then(next, next);
  }
  function start(o) {
    // A message sent again replaces its refused one — as that one's Retry,
    // so it goes FIRST, before what waited behind it.
    var first = false, seq = ++seqN;
    live.slice().forEach(function (x) {
      if (x.queued === !!o.queued && x.text === o.text && (x.state === 'failed' || x.state === 'unknown')) {
        if (x === blocker) { first = true; seq = x.seq; }
        release(x);
        clearFor(x);
        x.done = true; if (x.el) x.el.remove(); x.el = null;
        live = live.filter(function (y) { return y !== x; });
        if (x === blocker) blocker = null;
      }
    });
    // 🔴 A NEW MESSAGE SENT AFTER A REFUSAL WAS SHOWN IS THE PERSON MOVING
    // ON: what waited behind the refused message goes, in order, then this
    // one. The refused message keeps its Retry (or Send again) and Dismiss —
    // on its bubble, or beside the status line with its reason (offer) — so
    // it is not lost; retried later, it goes after these. Only messages
    // pressed BEFORE the refusal came wait for the person's answer to it.
    if (blocker && !first) blocker = null;
    var p = { id: hex(8) || String(Date.now()) + String(Math.random()).slice(2), text: o.text, typed: o.typed, queued: !!o.queued, to: o.to, again: !!o.again,
      seq: seq, nonce: hex(16), state: blocker && !first ? 'held' : (o.queued ? 'queuing' : 'sending'), before: seenRows(), beforeCC: ccItems() };
    live.push(p);
    hold(p);
    if (first) queue.unshift(p); else queue.push(p);
    draw(p);
    paintQueue();
    pump();
    return p;
  }
  // post sends p; its promise settles when it has an answer — a step-up
  // included: the next message waits for this one's confirm. 🔴 It never
  // skips p: a message in the queue is always posted.
  function post(p) {
    var req, body = bodyOf(p);
    try {
      req = fetch(p.to, { method: 'POST', body: body, credentials: 'same-origin',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'X-Requested-With': 'XMLHttpRequest' } });
    } catch (e) { native(p); return; }
    return req.then(function (r) {
      // 🔴 FROM HERE THE HOST HAS ANSWERED: nothing below hands p to the
      // browser's own submit, which would post it a second time.
      try { return answered(p, r); } catch (e) { try { console.error('composer: an answer was not shown', e); } catch (e2) {} }
    }, function () {
      // 🔴 THE PAGE MAY BE GOING (a reload, a navigation): the browser
      // abandons the request, which is not a failure to hand to the browser's
      // own submit — that would post it again from a page being unloaded. The
      // draft holds the message, and the page that comes next has it in the
      // box. But a beforeunload that did NOT end in leaving (a download link)
      // must not swallow a real failure: once the page is known to still be
      // here, it is refused — its text kept, Retry — and the promise settles
      // only then, so nothing after it goes ahead of it.
      if (gone || Date.now() < unloadUntil) {
        return new Promise(function (resolve) {
          whenHere(function () { if (!p.done) refused(p, ` + strconv.Quote(pendingNetworkFailed) + `, false); resolve(); });
        });
      }
      if (navigator.onLine === false) { refused(p, form.getAttribute('data-down'), false); return; }
      native(p);
    });
  }
  function answered(p, r) {
    if (r.ok) {
      // The server has it: the draft lets it go.
      release(p);
      if (p.queued) {
        p.state = 'queued';
        sayFor(p, 'Queued.', true);
        live = live.filter(function (x) { return x !== p; });
        document.dispatchEvent(new Event('mv-queued'));
      } else if (!p.el) {
        // A Terminal view: the pane shows it; nothing to settle.
        p.state = 'sent';
        clearFor(p);
        settle(p);
      } else {
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
      set(p, 'stepup');
      return new Promise(function (resolve) {
        // One answer only: a confirm after a cancel (or the other way) is
        // ignored, so p is never both refused and posted.
        var over = false;
        // The SAME request again, nonce and all: nothing was delivered.
        stepUp.ask('', function () { if (over) return; over = true; set(p, p.queued && !p.el ? 'queuing' : 'sending'); return Promise.resolve(post(p)).then(resolve, resolve); },
          function () { if (over) return; over = true; refused(p, ` + strconv.Quote(pendingCancelled) + `, false); resolve(); });
      });
    }
    var again = !!r.headers.get('` + SendAgainHeader + `');
    return r.text().then(function (t) {
      refused(p, t.trim() || ((p.queued ? 'Not queued' : 'Not sent') + ' (HTTP ' + r.status + ')'), again);
    });
  }
  // refused: p did not go. Its text goes back, and it waits for the person:
  // a failed bubble with the reason, Retry and Dismiss — or, with no bubble,
  // the reason on the status line, the text in the box, and Send again (it
  // may have landed) or Retry, and Dismiss. 🔴 What was pressed before this
  // answer and not posted yet waits (blocker).
  function refused(p, reason, again) {
    p.again = again;
    if (p.done) {
      // Settled before its answer (it cannot be: only a "Sent" message is
      // settled by a row) — drawn again, so the refusal is never lost.
      p.done = false;
      live.push(p);
      if (bubbled(p)) { p.state = 'sending'; draw(p); }
    }
    restore(p);
    blocker = p;
    set(p, again ? 'unknown' : 'failed', reason);
    paintQueue();
  }

  form.addEventListener('submit', function (e) {
    // From here on a failure of THIS handler leaves the event alone, and the
    // browser submits the form itself.
    if (input.disabled) { e.preventDefault(); return; }
    var att = window.musterAttach;
    if (att && att.pending()) { e.preventDefault(); say(att.pendingMessage); return; }
    if (!input.value.trim() && !(att && att.count())) { e.preventDefault(); say(form.getAttribute('data-empty')); focusBox(); return; }
    if (typeof fetch !== 'function' || typeof URLSearchParams !== 'function' || typeof Promise !== 'function') return;
    var sub = ('submitter' in e) ? e.submitter : clicked;
    clicked = null;
    var queued = !!(sub && sub === queueBtn);
    // The message as sent: the text, then a line per attachment (attach.go).
    var typed = input.value;
    var text = att ? att.compose(typed) : typed;
    // A plain Send is never a Send again: that is the refused message's own
    // button.
    var again = !!form.querySelector('input[name="` + SendAgainField + `"]');
    e.preventDefault();
    // A refused message's line and buttons stay (it is still refused); any
    // other line is done with.
    if (!noteHeld()) { noteBy = null; say(''); }
    againField(false);
    clearBox();
    if (att) att.sent();
    renew();
    try { start({ text: text, typed: typed, queued: queued, to: queued ? sub.getAttribute('formaction') : form.getAttribute('action'), again: again }); }
    catch (err) { input.value = text; shown(); browserSubmit(); }
  });
  input.addEventListener('input', function () { if (input.value.trim() && !noteHeld()) say(''); });
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
})();
`
