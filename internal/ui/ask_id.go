package ui

import (
	"html"
	"strconv"
)

// AN ASK'S ID ON ITS CARD (docs/attention.md "Ids on the cards"). Agents name
// asks by id ("I asked in ask 859"); the id is drawn on every card so the
// owner can find the one they mean, and the Asks page's search box takes it:
// pasted there, an id goes straight to its ask or draft — any the signed-in
// person can already open (the ask page, the Drafts tab), so a chip on a team
// draft a member reads finds that draft. Words are searched in the person's
// own asks and drafts only (docs/attention.md "Searching your asks").
//
// 🔴 TWO NAMESPACES, ALWAYS LABELLED. An ask (attention_asks) and a plugin
// draft (plugin_reply_drafts) are numbered apart, so "#484" alone could be
// either. The id is never drawn bare: "Ask #859" on an ask's card, "Draft
// #484" on a draft's (and in a step-up card's title, for the draft it
// confirms). AskIDLabel / DraftIDLabel are the ONE spelling of each.
//
// THE CHIP: small and muted, at the end of a card's top row. The server draws
// it as PLAIN TEXT (a <span data-id-copy>): without script that is all it is.
// The id script (IDCopyScriptHTML) turns each one into a real <button> —
// keyboard reachable, named "Copy Ask #859" — whose tap copies the label to
// the clipboard and says "Copied" for a moment. The button keeps its name
// while it says so; the copy is ANNOUNCED through one polite live region the
// script keeps on the page ([data-id-copy-status]: "Copied Ask #859"), since
// a focused control's changing name is not reliably read out. It is a control (a <button>),
// so the Asks feed's script never takes a tap on it for the card's expand
// (asksFeedJS CONTROLS). The chip is never inside a card's head toggle: a
// button cannot hold a button.

// AskIDLabel is ask id's label: "Ask #859".
func AskIDLabel(id int64) string { return "Ask #" + strconv.FormatInt(id, 10) }

// DraftIDLabel is plugin draft id's label: "Draft #484".
func DraftIDLabel(id int64) string { return "Draft #" + strconv.FormatInt(id, 10) }

// IDChipClass is the id chip: muted, small, a 44px target once it is a button.
const IDChipClass = "press inline-flex min-h-[44px] shrink-0 items-center whitespace-nowrap rounded-lg px-2 text-xs font-medium tabular-nums text-fg2 transition hover:bg-s2 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-focus"

// AskIDChipHTML is ask id's chip ("Ask #859"); "" for no id.
func AskIDChipHTML(id int64) string {
	if id <= 0 {
		return ""
	}
	return idChipHTML("ask", id, AskIDLabel(id))
}

// DraftIDChipHTML is plugin draft id's chip ("Draft #484"); "" for no id.
func DraftIDChipHTML(id int64) string {
	if id <= 0 {
		return ""
	}
	return idChipHTML("draft", id, DraftIDLabel(id))
}

func idChipHTML(kind string, id int64, label string) string {
	return `<span class="` + IDChipClass + `" data-id-copy="` + html.EscapeString(label) + `" data-id-kind="` + kind + `" data-id="` +
		strconv.FormatInt(id, 10) + `">` + html.EscapeString(label) + `</span>`
}

// IDCopiedText is what a chip says for a moment once its label is copied.
const IDCopiedText = "Copied"

// IDCopyLabelPrefix leads a chip button's accessible name ("Copy Ask #859"):
// its visible label is in its name.
const IDCopyLabelPrefix = "Copy "

// idCopyJS turns every plain id chip on the page — and every one a live
// refresh or a lazy panel puts there later (a MutationObserver) — into a
// <button>, and copies a chip's label when one is pressed. Delegated on the
// document, bound once per window.
var idCopyJS = `(function () {
  if (window.__idCopyInit) return;
  window.__idCopyInit = true;
  function upgrade(root) {
    if (!root || !root.querySelectorAll) return;
    var spans = root.matches && root.matches('span[data-id-copy]') ? [root] : root.querySelectorAll('span[data-id-copy]');
    Array.prototype.forEach.call(spans, function (s) {
      var b = document.createElement('button');
      b.type = 'button';
      for (var i = 0; i < s.attributes.length; i++) b.setAttribute(s.attributes[i].name, s.attributes[i].value);
      var label = s.getAttribute('data-id-copy') || '';
      b.setAttribute('aria-label', '` + IDCopyLabelPrefix + `' + label);
      b.setAttribute('title', '` + IDCopyLabelPrefix + `' + label);
      b.textContent = s.textContent;
      s.replaceWith(b);
    });
  }
  // The one polite live region a copy is announced in (made at start, so it
  // is on the page before anything is said in it).
  function status() {
    var r = document.querySelector('[data-id-copy-status]');
    if (!r && document.body) {
      r = document.createElement('div');
      r.setAttribute('data-id-copy-status', '');
      r.setAttribute('role', 'status');
      r.setAttribute('aria-live', 'polite');
      r.className = 'sr-only';
      document.body.appendChild(r);
    }
    return r;
  }
  function all() { status(); upgrade(document.body || document.documentElement); }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', all); else all();
  document.addEventListener('htmx:load', function (e) { upgrade(e.target); });
  try {
    new MutationObserver(function (ms) {
      ms.forEach(function (m) {
        Array.prototype.forEach.call(m.addedNodes, function (n) { if (n.nodeType === 1) upgrade(n); });
      });
    }).observe(document.documentElement, { childList: true, subtree: true });
  } catch (e) {}
  document.addEventListener('click', function (e) {
    var b = e.target && e.target.closest && e.target.closest('button[data-id-copy]');
    if (!b) return;
    e.preventDefault();
    var label = b.getAttribute('data-id-copy') || '';
    var copy = window.cgCopyText ? window.cgCopyText(label) : (navigator.clipboard ? navigator.clipboard.writeText(label) : Promise.reject());
    copy.then(function () {
      // The chip keeps its width while it says Copied: nothing beside it moves.
      if (!b.style.minWidth) b.style.minWidth = b.offsetWidth + 'px';
      b.textContent = '` + IDCopiedText + `';
      b.setAttribute('data-id-copied', '');
      // Said once in the live region: emptied first, so a second copy of the
      // same id is announced again.
      var r = status();
      if (r) {
        r.textContent = '';
        setTimeout(function () { r.textContent = '` + IDCopiedText + ` ' + label; }, 50);
      }
      clearTimeout(b.__idCopyTimer);
      b.__idCopyTimer = setTimeout(function () {
        b.textContent = label;
        b.removeAttribute('data-id-copied');
      }, 1500);
    }, function () {});
  });
})();
`

var idCopyScriptSrc = staticJS("id-copy", idCopyJS)

// IDCopyScriptHTML is the id script's tag: the Asks page and the ask page
// write it once.
func IDCopyScriptHTML() string { return nodeString(staticScript(idCopyScriptSrc)) }
