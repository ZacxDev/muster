package ui

import (
	"bytes"
	"strconv"

	"github.com/ZacxDev/muster/internal/textplain"
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// ---------------------------------------------------------------------------
// SUCCESSION ON A SESSION'S VIEW (docs/design/chief-in-agent-view.md, phase 2):
//
//   - the CARD: the chief proposed a new session that continues this one
//     (succession_propose). It shows the exact kickoff muster copied and the
//     settings, and its buttons — Start (or Replace / Spin off) and Not now —
//     are THE AUTHORISATION: nothing starts until the owner taps one. A real
//     form (POST MirrorProposalPath, view_proposal.go) that the script submits in place;
//   - the LINE while the successor starts, and the banner once it runs
//     ("Continued in …" / "Spun off …"), and "superseded" on a replaced view;
//   - the FOLLOW: the view that saw the proposal (or the start) navigates to
//     the successor once it runs — at most once per succession (sessionStorage),
//     the top window when framed — after a 1.5 s "Opening the new session…"
//     with Stay here; a page that comes back (visibilitychange, a stream
//     reconnect) asks again once (successionScript);
//   - the successor's own header line: "Continues <old title> · <mode> · open
//     previous".
// ---------------------------------------------------------------------------

// MirrorSuccessorPath is the view's succession read (JSON: its state, its
// card, where the successor is).
const MirrorSuccessorPath = "/mirror/view/successor"

// SuccessorPath is the entry's succession read.
func (r MirrorRef) SuccessorPath() string { return MirrorSuccessorPath + "?" + r.query() }

// A succession's states as a view reads them.
const (
	SuccessionProposed = "proposed"
	SuccessionExpired  = "expired"
	SuccessionDeclined = "declined"
	SuccessionStarting = "starting"
	SuccessionRunning  = "running"
	SuccessionFailed   = "failed"
	SuccessionEnded    = "ended"
)

// SuccessionView is one succession as a view draws it.
type SuccessionView struct {
	ID    int64
	State string
	// Mode is replace or spinoff, "" while the owner has not chosen.
	Mode                   string
	Target, Model, Repo    string
	BaseNote               string
	SubjectTitle, PredPath string
	Superseded             bool
	// Card is the proposal card while the succession is proposed.
	Card                  *ProposalView
	Host, Title, ViewPath string
	Refusal               string
}

// successionModeText is a mode as a sentence fragment.
func successionModeText(mode string) string {
	switch mode {
	case "replace":
		return "replace"
	case "spinoff":
		return "spin-off"
	}
	return "replace or spin off — you choose"
}

// SuccessionCard is the card (or line, or banner) of the succession of the
// view at ref; nil when there is nothing to draw (none, or declined).
func SuccessionCard(ref MirrorRef, x *SuccessionView) g.Node {
	if x == nil || x.State == SuccessionDeclined {
		return nil
	}
	attrs := []g.Node{g.Attr("data-succession", strconv.FormatInt(x.ID, 10)), g.Attr("data-succession-state", x.State),
		g.Attr("data-succession-mode", x.Mode)}
	if x.ViewPath != "" {
		attrs = append(attrs, g.Attr("data-successor-path", x.ViewPath))
	}
	line := func(cls string, kids ...g.Node) g.Node {
		return Div(append(append(attrs, Class("border-b border-line px-3 py-2 text-xs break-words "+cls)), kids...)...)
	}
	open := func(label string) g.Node {
		if x.ViewPath == "" {
			return nil
		}
		return A(Href(x.ViewPath), g.Attr("data-successor-open", ""), Class("ml-1 font-semibold text-accent underline"), g.Text(label))
	}
	switch x.State {
	case SuccessionExpired:
		return line("bg-s1 text-muted", g.Text("Your chief's proposal to continue this session expired: ask it again."))
	case SuccessionStarting:
		return line("bg-s1 text-fg2", Span(Class("animate-pulse"), g.Text("A successor is starting"+onHost(x.Host)+"… ")),
			Span(Class("text-muted"), g.Text("This view moves to it when it runs.")))
	case SuccessionFailed:
		return line("bg-danger/15 text-fg", g.Text("The successor could not be started: "+x.Refusal))
	case SuccessionEnded:
		return line("bg-s1 text-muted", g.Text("Its successor “"+textplain.Title(x.Title)+"” has ended."), open("open"))
	case SuccessionRunning:
		if x.Mode == "replace" {
			return line("bg-st-warning-bg text-st-warning-fg", Span(Class("font-semibold"), g.Text("Superseded. ")),
				g.Text("Continued in “"+textplain.Title(x.Title)+"”."), open("Open it →"),
				Span(Class("block text-muted"), g.Text("This window keeps running: close it on "+x.Host+" when you are done.")))
		}
		return line("bg-s1 text-fg2", g.Text("Spun off “"+textplain.Title(x.Title)+"”."), open("Open it →"))
	}
	// Proposed: THE chief's proposal card (view_proposal.go), its region's
	// attributes on it.
	if x.Card == nil {
		return nil
	}
	return ProposalCard(ref, *x.Card, attrs...)
}

func onHost(h string) string {
	if h == "" {
		return ""
	}
	return " on " + h
}

// RenderSuccessionCard is SuccessionCard as HTML (the read's card_html).
func RenderSuccessionCard(ref MirrorRef, x *SuccessionView) (string, error) {
	n := SuccessionCard(ref, x)
	if n == nil {
		return "", nil
	}
	var b bytes.Buffer
	err := n.Render(&b)
	return b.String(), err
}

// successorHeader is the successor's line: what it continues, how, and a link
// back.
func successorHeader(p *SuccessionView) g.Node {
	if p == nil {
		return nil
	}
	return P(ID("mv-predecessor"), g.Attr("data-predecessor", strconv.FormatInt(p.ID, 10)),
		Class("border-b border-line bg-s1 px-3 py-1.5 text-xs text-fg2 break-words"),
		g.Text("Continues “"+p.SubjectTitle+"” · "+successionModeText(p.Mode)+" · "),
		A(Href(p.PredPath), g.Attr("data-predecessor-open", ""), Class("font-semibold text-accent underline"), g.Text("open previous")))
}

// successionRegion is the view's succession region: the successor header,
// then the card of this session's own succession, in a container the script
// refreshes from src.
func successionRegion(ref MirrorRef, x, pred *SuccessionView, end *EndView, live bool) g.Node {
	if !live && x == nil && pred == nil && end == nil {
		return nil
	}
	// The session's NEWEST proposal: a succession's, or an end's.
	card := SuccessionCard(ref, x)
	if end != nil {
		card = EndSessionCard(ref, end)
	}
	return g.Group([]g.Node{
		successorHeader(pred),
		Div(ID("mv-succession"), g.Attr("data-src", ref.SuccessorPath()), g.Attr("aria-live", "polite"), card),
		Div(ID("mv-follow"), g.Attr("hidden", ""), g.Attr("role", "status"),
			Class("flex items-center gap-2 border-b border-line bg-accent/15 px-3 py-2 text-sm text-fg"),
			Span(Class("min-w-0 flex-1"), g.Text("Opening the new session…")),
			Button(Type("button"), ID("mv-follow-stay"), Class(proposalButtonClass+"text-fg2 ring-1 ring-inset ring-edge hover:bg-s2"), g.Text("Stay here"))),
	})
}

// viewStreamScript is the view's ONE /events stream (window.musterViewStream),
// shared by the chief strip (view_chief.go) and the succession region: each
// registers its handlers with on(name, fn) and says whether it needs the
// stream with want(key, on); the stream is open while anything wants it. A
// stream per feature would be a second connection per tab against the
// browser's per-host limit (TestTheNavAddsNoStream).
func viewStreamScript() g.Node {
	return Script(g.Raw(`
window.musterViewStream = window.musterViewStream || (function () {
  var es = null, wants = {}, handlers = {};
  function sync() {
    var any = false;
    for (var k in wants) if (wants[k]) any = true;
    if (any && !es && typeof EventSource === 'function') {
      try {
        es = new EventSource('/events');
        for (var n in handlers) handlers[n].forEach(function (f) { es.addEventListener(n, f); });
      } catch (e) { es = null; }
    }
    if (!any && es) { try { es.close(); } catch (e) {} es = null; }
  }
  return {
    on: function (name, fn) { (handlers[name] = handlers[name] || []).push(fn); if (es) es.addEventListener(name, fn); },
    want: function (key, on) { wants[key] = !!on; sync(); }
  };
})();
`))
}

// successionScript is the region's behaviour: the card's tap in place, the
// refresh on the owner's stream, and the follow. The stream is opened only
// while a succession of this view is open (proposed or starting), or once the
// owner sends the chief a line from this view.
func successionScript() g.Node {
	return Script(g.Raw(`
(function () {
  var box = document.getElementById('mv-succession');
  var follow = document.getElementById('mv-follow');
  var stay = document.getElementById('mv-follow-stay');
  if (!box || typeof fetch !== 'function') return;
  var src = box.getAttribute('data-src');
  var vs = window.musterViewStream, armed = {}, stayed = false, timer = null;
  function cur() { return box.querySelector('[data-succession]'); }
  function state(el) { return el ? el.getAttribute('data-succession-state') : ''; }
  function open(s) { return s === ` + strconv.Quote(SuccessionProposed) + ` || s === ` + strconv.Quote(SuccessionStarting) + `; }
  function jumped(id) { try { return sessionStorage.getItem('muster.lineage.jumped.' + id) === '1'; } catch (e) { return false; } }
  function markJumped(id) { try { sessionStorage.setItem('muster.lineage.jumped.' + id, '1'); } catch (e) {} }
  if (vs) { vs.on('lineage.changed', refresh); vs.on('chat.reply', refresh); vs.on('open', refresh); }
  function listen() { if (vs) vs.want('succession', true); }
  function go(path, id) {
    if (stayed || jumped(id)) return;
    follow.hidden = false;
    timer = setTimeout(function () {
      if (stayed) return;
      markJumped(id);
      var w = window;
      try { if (window.top && window.top !== window) w = window.top; } catch (e) {}
      w.location.assign(path);
    }, 1500);
  }
  function seen(el) {
    if (!el) return;
    var id = el.getAttribute('data-succession'), s = state(el);
    if (open(s)) { armed[id] = true; listen(); }
    // 🔴 ONLY THE VIEW THAT SAW IT OPEN JUMPS: a view opened after the
    // successor ran shows the banner instead.
    if (s === ` + strconv.Quote(SuccessionRunning) + ` && armed[id]) {
      var p = el.getAttribute('data-successor-path');
      if (p) go(p, id);
    }
  }
  function read(url) {
    return fetch(url, { credentials: 'same-origin', headers: { 'Accept': 'application/json' } }).then(function (r) {
      return r.ok ? r.json() : null;
    });
  }
  function refresh() {
    // The view's own read; once its window is gone (an ended session), the
    // proposal it showed, by id.
    var el = cur(), id = el ? el.getAttribute('data-succession') : '';
    read(src).then(function (j) {
      if (j || !id) return j;
      return read(` + strconv.Quote(MirrorProposalReadPath) + ` + '?id=' + encodeURIComponent(id));
    }).then(function (j) {
      if (!j) return;
      box.innerHTML = j.card_html || '';
      wire();
      seen(cur());
      if (window.musterPinMeasure) window.musterPinMeasure();
    }).catch(function () {});
  }
  function wire() {
    var form = document.getElementById('mv-proposal-form');
    if (!form || form.getAttribute('data-wired')) return;
    form.setAttribute('data-wired', '1');
    form.addEventListener('submit', function (ev) {
      ev.preventDefault();
      var fd = new FormData(form);
      var btn = ev.submitter;
      if (btn && btn.name) fd.set(btn.name, btn.value);
      var el = cur();
      if (el) armed[el.getAttribute('data-succession')] = true;
      listen();
      Array.prototype.forEach.call(form.querySelectorAll('button'), function (b) { b.disabled = true; });
      fetch(form.getAttribute('action'), { method: 'POST', credentials: 'same-origin', body: new URLSearchParams(fd),
        headers: { 'X-Requested-With': 'XMLHttpRequest' } }).then(function (r) {
        if (r.ok) return refresh();
        return r.text().then(function (t) {
          var err = document.getElementById('mv-proposal-error');
          if (err) { err.textContent = t; err.hidden = false; }
          Array.prototype.forEach.call(form.querySelectorAll('button'), function (b) { b.disabled = false; });
        });
      }).catch(function () {});
    });
  }
  if (stay) stay.addEventListener('click', function () {
    stayed = true; if (timer) clearTimeout(timer); follow.hidden = true;
    var el = cur(); if (el) markJumped(el.getAttribute('data-succession'));
  });
  document.addEventListener('mv-chief-sent', function () { listen(); refresh(); });
  // A page that comes back asks again once (a missed event: a phone asleep, a dropped stream).
  document.addEventListener('visibilitychange', function () { if (!document.hidden) refresh(); });
  window.addEventListener('pageshow', function (ev) { if (ev.persisted) refresh(); });
  wire();
  seen(cur());
  // A Back or Forward to this view may be drawn from the browser's cache, as
  // it was before the succession moved: ask once.
  try {
    var nav = performance.getEntriesByType('navigation')[0];
    if (nav && nav.type === 'back_forward') refresh();
  } catch (e) {}
})();
`))
}
