package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	g "maragu.dev/gomponents"

	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE ASKS PAGE'S TABS: "Ask" (everything /attention listed before, its
// Answered section included) and, with plugins served, "Reply drafts" — the
// drafts that used to sit at the bottom of /plugins, decided here.
//
// 🔴 THE ROUTE STAYS /attention, AND THAT IS THE DESIGN, NOT AN OVERSIGHT. A
// URL fragment never reaches the server, so a redirect from /attention to a
// new path could not carry `#ask-12` along in any way the server can see — it
// survives a 30x only by browser convention, and a link that already lives on
// a phone (a push notification's tap target, an agent card's badge, the chief's
// attention_list) is exactly the link that must not break. Keeping the path
// and renaming the page (heading, title, sidebar) costs nothing and breaks no
// link. The tab is in the QUERY (?tab=drafts, ?tab=archived), so it IS seen by
// the server and the page is drawn on the right tab with no script; the
// fragment then lands on the card.
//
// A DRAFT LEAVES "Awaiting decision" THE MOMENT IT IS DECIDED: archived means
// "not pending" (nothing is stored for it — no migration), and the Archived
// view draws every other draft with its LIVE status (draftStatusText, read on
// every render and refreshed on the queue's event), keeping Withdraw while an
// approval is still unclaimed.
//
// THE BADGE STILL COUNTS PENDING DRAFTS: attentionItems is unchanged and still
// returns them; only the Ask tab's list leaves them out (askTabItems), pointing
// at the Reply drafts tab instead, so a badge of 1 over an empty Ask list is
// never unexplained.

const (
	asksTabParam    = "tab"
	asksTabAsk      = "ask"
	asksTabDrafts   = "drafts"
	asksTabArchived = "archived"

	// asksDraftsPath is the drafts tab's live fragment (JSON: both lists and
	// the pending count), what the page re-fetches on the queue's event.
	asksDraftsPath = attentionPath + "/drafts"

	// asksDraftsRead is how many drafts one render reads: DraftCards orders the
	// pending ones first, so both lists are cut at pluginDraftsShown from it.
	asksDraftsRead = 2 * pluginDraftsShown
)

// draftLink is a draft card's address on the Asks page: the push deep link,
// the attention item's link and where a decision lands. A decided draft is in
// the Archived view; the page's script follows the fragment to whichever view
// holds the card, so ?tab=drafts is right for both.
func draftLink(id int64) string {
	return attentionPath + "?" + asksTabParam + "=" + asksTabDrafts + "#" + draftCardAnchor(id)
}

// draftDecidedLink is where an approve, reject or withdraw lands: the card,
// now in Archived.
func draftDecidedLink(id int64) string {
	return attentionPath + "?" + asksTabParam + "=" + asksTabArchived + "#" + draftCardAnchor(id)
}

// asksTab is the tab a request names, asksTabAsk for none or an unknown one.
func asksTab(r *http.Request) string {
	switch v := r.URL.Query().Get(asksTabParam); v {
	case asksTabDrafts, asksTabArchived:
		return v
	}
	return asksTabAsk
}

// askTabItems is items without the reply drafts (the Reply drafts tab's), and
// how many drafts it left out.
func askTabItems(items []attentionItem) ([]attentionItem, int) {
	out := make([]attentionItem, 0, len(items))
	drafts := 0
	for _, it := range items {
		if it.Kind == attentionKindPlugin {
			drafts++
			continue
		}
		out = append(out, it)
	}
	return out, drafts
}

// askTabListHTML is the Ask tab's list: the queue without its reply drafts,
// then — when drafts are waiting — one line pointing at the Reply drafts tab,
// then the Answered section.
func askTabListHTML(items []attentionItem, answered string, now time.Time) string {
	rest, drafts := askTabItems(items)
	var b strings.Builder
	b.WriteString(attentionItemsHTML(rest, now))
	if drafts > 0 {
		n := "1 reply draft awaits"
		if drafts > 1 {
			n = strconv.Itoa(drafts) + " reply drafts await"
		}
		b.WriteString(`<li class="text-sm text-fg2" data-testid="attention-drafts-pointer" data-count="` + strconv.Itoa(drafts) + `">` + n +
			` your decision under <a href="` + attentionPath + `?` + asksTabParam + `=` + asksTabDrafts +
			`" class="text-accent underline">Reply drafts</a>.</li>
`)
	}
	b.WriteString(answered)
	return b.String()
}

// splitDraftCards is cards as the two views: awaiting a decision (pending and
// not yet past its expiry) and archived (every other), each at most
// pluginDraftsShown.
func splitDraftCards(cards []plugins.DraftCard, now time.Time) (pending, archived []plugins.DraftCard) {
	for _, c := range cards {
		if c.Draft.Status == plugins.DraftPending && c.Draft.ExpiresAt.After(now) {
			if len(pending) < pluginDraftsShown {
				pending = append(pending, c)
			}
			continue
		}
		if len(archived) < pluginDraftsShown {
			archived = append(archived, c)
		}
	}
	return pending, archived
}

// draftListHTML is one view's <li>s, or its empty line.
func draftListHTML(cards []plugins.DraftCard, empty string) string {
	if len(cards) == 0 {
		return `<li class="text-sm text-fg2" data-drafts-empty>` + empty + `</li>`
	}
	var b strings.Builder
	for _, c := range cards {
		b.WriteString(draftCardHTML(c))
	}
	return b.String()
}

const (
	draftsPendingEmpty  = "No reply drafts await your decision."
	draftsArchivedEmpty = "No decided drafts yet."
)

// draftsTabLabel is the Reply drafts tab's label: its pending count when there
// is one (the page's script keeps it current).
func draftsTabLabel(n int) string {
	if n <= 0 {
		return "Reply drafts"
	}
	return "Reply drafts (" + strconv.Itoa(n) + ")"
}

// readDraftViews is ctx's principal's drafts as the two views; ok false when
// they could not be read (logged).
func (s *Server) readDraftViews(ctx context.Context) (pending, archived []plugins.DraftCard, ok bool) {
	cards, err := s.ext.Plugins.DraftCards(ctx, asksDraftsRead)
	if err != nil {
		s.logger.Printf("asks: reply drafts: %v", err)
		return nil, nil, false
	}
	pending, archived = splitDraftCards(cards, time.Now())
	return pending, archived, true
}

// draftsPanel is the Reply drafts tab's panel: its own two tabs, Awaiting
// decision and Archived, the one sub selected.
func draftsPanel(pending, archived []plugins.DraftCard, sub string) g.Node {
	pendingList := `<p class="text-xs text-fg2">Your chief drafts replies; nothing is sent until you approve one. The plugin then sends it once.</p>
<ul id="asks-drafts-pending" class="flex flex-col gap-2" data-testid="drafts-pending">` + draftListHTML(pending, draftsPendingEmpty) + `</ul>`
	archivedList := `<p class="text-xs text-fg2">Drafts you approved or rejected, and those replaced or expired, with what became of each. An approval the plugin has not collected yet can still be withdrawn.</p>
<ul id="asks-drafts-archived" class="flex flex-col gap-2" data-testid="drafts-archived">` + draftListHTML(archived, draftsArchivedEmpty) + `</ul>`
	subSel := asksTabDrafts
	if sub == asksTabArchived {
		subSel = asksTabArchived
	}
	t := ui.TabsSelected("asks-drafts-tabs", "", "Reply drafts", subSel,
		ui.Tab{ID: asksTabDrafts, Label: "Awaiting decision", Panel: g.Raw(`<div class="flex flex-col gap-2 pt-3">` + pendingList + `</div>`)},
		ui.Tab{ID: asksTabArchived, Label: "Archived", Panel: g.Raw(`<div class="flex flex-col gap-2 pt-3">` + archivedList + `</div>`)},
	)
	return g.Group{g.Raw(`<div class="flex overflow-x-auto border-b border-line pt-2">`), t.List, g.Raw(`</div>`), t.Panels}
}

// asksTabsHTML is the page's two tabs over askList (the Ask tab's <ul>) and
// the drafts, tab selected.
func (s *Server) asksTabsHTML(askList string, pending, archived []plugins.DraftCard, tab string) string {
	top := asksTabAsk
	if tab != asksTabAsk {
		top = asksTabDrafts
	}
	t := ui.TabsSelected("asks-tabs", "", "Asks", top,
		ui.Tab{ID: asksTabAsk, Label: "Ask", Panel: g.Raw(`<div class="flex flex-col gap-2 pt-3">` + askList + `</div>`)},
		ui.Tab{ID: asksTabDrafts, Label: draftsTabLabel(len(pending)), Panel: draftsPanel(pending, archived, tab)},
	)
	var b strings.Builder
	b.WriteString(`<div class="flex flex-col" data-asks-tabs><div class="flex overflow-x-auto border-b border-line">`)
	nodes := g.Group{t.List, g.Raw(`</div>`), t.Panels, g.Raw(`</div>`), ui.TabsScript()}
	if err := nodes.Render(&b); err != nil {
		s.logger.Printf("asks: render the tabs: %v", err)
	}
	return b.String()
}

// handleAsksDrafts is GET /attention/drafts: the two lists and the pending
// count as JSON, for the page's live refresh.
func (s *Server) handleAsksDrafts(w http.ResponseWriter, r *http.Request) {
	if s.ext.Plugins == nil {
		http.NotFound(w, r)
		return
	}
	pending, archived, ok := s.readDraftViews(r.Context())
	if !ok {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the reply drafts could not be read"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]any{
		"pending":  draftListHTML(pending, draftsPendingEmpty),
		"archived": draftListHTML(archived, draftsArchivedEmpty),
		"count":    len(pending),
		"label":    draftsTabLabel(len(pending)),
	})
}

// asksPageScript is the page's own behaviour around the tabs:
//
//   - the tab is kept in the URL (?tab=), replaced not pushed, so a reload or
//     a shared link opens the same tab;
//   - a fragment naming a card inside a hidden panel opens that panel's tabs
//     and scrolls to it (a push opened on ?tab=drafts for a draft that was
//     decided meanwhile lands on it in Archived);
//   - the drafts lists follow the queue's event (asks:changed, dispatched by
//     attentionPageScript) and a 30 s backstop — but never while the confirm
//     dialog is open, so the form it will submit is still in the page.
const asksPageScript = `<script>
(function () {
  var root = document.querySelector('[data-asks-tabs]');
  if (!root) return;
  function selected(id) {
    var list = document.getElementById(id);
    var t = list && list.querySelector('[role="tab"][aria-selected="true"]');
    return t ? t.getAttribute('data-tab') : '';
  }
  function syncURL() {
    var top = selected('asks-tabs'), v = top === 'ask' ? '' : (selected('asks-drafts-tabs') || 'drafts');
    try {
      var u = new URL(window.location.href);
      if (v) u.searchParams.set('` + asksTabParam + `', v); else u.searchParams.delete('` + asksTabParam + `');
      if (u.href !== window.location.href) history.replaceState(history.state, '', u.href);
    } catch (e) {}
  }
  root.addEventListener('click', function (e) {
    if (e.target && e.target.closest && e.target.closest('[role="tab"]')) setTimeout(syncURL, 0);
  });
  root.addEventListener('keydown', function () { setTimeout(syncURL, 0); });
  function reveal() {
    var id = decodeURIComponent((window.location.hash || '').slice(1));
    var el = id && document.getElementById(id);
    if (!el || !root.contains(el) || !window.muTabs) return;
    for (var p = el.parentElement; p; p = p.parentElement) {
      if (p.getAttribute && p.getAttribute('role') === 'tabpanel' && p.hidden) {
        var tab = document.getElementById(p.getAttribute('aria-labelledby'));
        if (tab) window.muTabs.select(tab.closest('[data-tabs]'), tab, {});
      }
    }
    el.scrollIntoView({ block: 'start' });
  }
  reveal();
  window.addEventListener('hashchange', reveal);
  var pendingList = document.getElementById('asks-drafts-pending');
  var archivedList = document.getElementById('asks-drafts-archived');
  var draftsTab = document.getElementById('asks-tabs-tab-drafts');
  if (!pendingList || !archivedList) return;
  function refresh() {
    var dlg = document.getElementById('term-confirm');
    if (dlg && dlg.open) return;
    fetch('` + asksDraftsPath + `', { credentials: 'same-origin' }).then(function (r) {
      return r.ok ? r.json() : null;
    }).then(function (j) {
      if (!j || (dlg && dlg.open)) return;
      pendingList.innerHTML = j.pending;
      archivedList.innerHTML = j.archived;
      if (draftsTab) draftsTab.textContent = j.label;
    }).catch(function () {});
  }
  document.addEventListener('asks:changed', refresh);
  setInterval(refresh, 30000);
})();
</script>`

// asksIntroDrafts is the page's intro clause for the drafts tab, "" without
// plugins.
func asksIntroDrafts(plugins bool) string {
	if !plugins {
		return ""
	}
	return " and, under Reply drafts, replies your chief drafted awaiting your approval"
}
