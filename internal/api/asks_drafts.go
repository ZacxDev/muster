package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	g "maragu.dev/gomponents"

	"github.com/ZacxDev/muster/internal/focus"
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
// returns them; only the Ask tab's list leaves them out (askTabItems). The
// Reply drafts tab's label carries their count ("Reply drafts (2)").
//
// 🔴 APPROVE WHAT YOU SAW: the Awaiting decision list is NEVER re-drawn in
// place when its SET of drafts changed (asksPageScript). A newer draft for the
// same message supersedes the one on screen; re-drawn under the owner's thumb,
// "Approve and send" would approve text they never read. Instead the stale
// cards stay — approving one is refused (409, it is superseded) — and a line
// offers to show the latest. Same set (an expiry moving): re-drawn in place.

const (
	asksTabParam = "tab"
	// asksTabAsk is the Open tab's OLD name: ?tab=ask still opens it (asksTab).
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
// now in Archived — named in ?draft= too, so it is drawn even when it is not
// among the pluginDraftsShown most recent (readDraftViews).
func draftDecidedLink(id int64) string {
	return attentionPath + "?" + asksTabParam + "=" + asksTabArchived + "&" + asksDraftParam + "=" +
		strconv.FormatInt(id, 10) + "#" + draftCardAnchor(id)
}

// asksDraftParam names a draft the page must draw whatever the views' cut.
const asksDraftParam = "draft"

// asksFocus is the draft a request names in ?draft=, 0 for none.
func asksFocus(r *http.Request) int64 {
	id, err := strconv.ParseInt(r.URL.Query().Get(asksDraftParam), 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// asksTab is the tab a request names (asks_feed.go's mapping): drafts,
// archived, answered, nudges and log as named; asksTabOpen for none, for
// "ask" (the Open tab's old name), for "open" and for anything unknown.
func asksTab(r *http.Request) string {
	switch v := r.URL.Query().Get(asksTabParam); v {
	case asksTabDrafts, asksTabArchived, asksTabAnswered, asksTabNudges, asksTabLog:
		return v
	}
	return asksTabOpen
}

// askTabItems is items without what the Drafts tab lists (draftsTabItem).
func askTabItems(items []attentionItem) []attentionItem {
	out := make([]attentionItem, 0, len(items))
	for _, it := range items {
		if !draftsTabItem(it) {
			out = append(out, it)
		}
	}
	return out
}

// draftsTabItem reports whether item it is one the Drafts tab LISTS: a reply
// or outbound draft (draftKey), an outreach batch (batchKey). 🔴 NOT every
// item of the plugin kind: a plugin's pairing request (pairKey) is that kind
// too, and the Drafts tab does not list it — counted as a draft it drew "1
// draft waits" over a tab that did not show it (audit of #787). It is the Open
// feed's own card.
func draftsTabItem(it attentionItem) bool {
	return it.Kind == attentionKindPlugin && (strings.HasPrefix(it.Key, "draft-") || strings.HasPrefix(it.Key, "outreach-"))
}

// askGroups is the Open feed's ORDER (askFeedHTML): each group's key, its
// label (the old group header; a card's badge now says it, askFeedBadge), and
// the item kinds it holds. A draft or outreach batch (attentionKindPlugin) is
// the Drafts tab's (askTabItems strips it first); the plugin kind's group here
// holds what is left of it, a pairing request.
// TestEveryAttentionKindHasAHome pins that every other kind the queue can hold
// is in one.
var askGroups = []struct {
	key, label string
	kinds      []string
}{
	{"questions", "Questions", []string{attentionKindAsk, attentionKindQuestion}},
	{"notices", "Notices", []string{attentionKindNotice}},
	{"waiting", "Waiting", []string{attentionKindSession}},
	{"tasks", "Tasks to route", []string{attentionKindTask}},
	{"pairing", "Pairing requests", []string{attentionKindPlugin}},
}

// splitDraftCards is cards as the two views: awaiting a decision (pending and
// not yet past its expiry) and archived (every other), each at most
// pluginDraftsShown.
func splitDraftCards(cards []plugins.DraftCard, now time.Time) (pending, archived []plugins.DraftCard) {
	for _, c := range cards {
		if c.Draft.Status == plugins.DraftPending && c.Draft.ExpiresAt.After(now) {
			// A pending follow or like is not drawn from this list: its whole
			// batch is (withBatches), so a cut can never draw one short.
			if c.Draft.IsOutbound() && plugins.IsBatchKind(c.Draft.Target().Kind) {
				continue
			}
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
	return draftListHTMLIn(cards, empty, cardPlace{})
}

// draftListHTMLIn is draftListHTML drawn in place p: each card is
// draftCardHTMLIn's, each batch outreachBatchCardHTMLIn's.
func draftListHTMLIn(cards []plugins.DraftCard, empty string, p cardPlace) string {
	if len(cards) == 0 {
		return ui.AsksEmptyHTML(` data-asks-empty="drafts-list" data-drafts-empty`, "", "", empty, "")
	}
	// A pending follow or like is an entry of its outreach batch's ONE card,
	// drawn where the batch's first entry falls (outreach_batch.go).
	_, batches, at := groupBatches(cards)
	var b strings.Builder
	for _, c := range cards {
		d := c.Draft
		if d.Status == plugins.DraftPending && d.IsOutbound() && plugins.IsBatchKind(d.Target().Kind) && d.Officer != nil {
			if i, first := at[d.ID]; first {
				b.WriteString(outreachBatchCardHTMLIn(batches[i], p))
			}
			continue
		}
		b.WriteString(draftCardHTMLIn(c, time.Now(), p))
	}
	return b.String()
}

const (
	draftsPendingEmpty  = "No reply drafts await your decision."
	draftsArchivedEmpty = "No decided drafts yet."
)

// draftsTabLabel is the Drafts tab's label: its pending count when there is
// one (the page's script keeps it current).
func draftsTabLabel(n int) string {
	if n <= 0 {
		return "Drafts"
	}
	return "Drafts (" + strconv.Itoa(n) + ")"
}

// readDraftViews is ctx's principal's drafts as the two views; ok false when
// they could not be read (logged). focus, when not 0, is a draft that must be
// drawn (a decision lands on it): when the views' cut left it out it is read
// on its own and put first in its view. Another owner's, or none, is not drawn.
func (s *Server) readDraftViews(ctx context.Context, focus int64) (pending, archived []plugins.DraftCard, ok bool) {
	cards, err := s.ext.Plugins.DraftCards(ctx, asksDraftsRead)
	if err != nil {
		s.logger.Printf("asks: reply drafts: %v", err)
		return nil, nil, false
	}
	now := time.Now()
	s.withDraftAgentNames(ctx, cards)
	pending, archived = splitDraftCards(cards, now)
	pending = s.withBatches(ctx, pending)
	if focus <= 0 {
		return pending, archived, true
	}
	for _, c := range append(append([]plugins.DraftCard{}, pending...), archived...) {
		if c.Draft.ID == focus {
			return pending, archived, true
		}
	}
	c, err := s.ext.Plugins.DraftCard(ctx, focus)
	if err != nil {
		if !errors.Is(err, plugins.ErrNotFound) {
			s.logger.Printf("asks: reply draft %d: %v", focus, err)
		}
		return pending, archived, true
	}
	if p, _ := splitDraftCards([]plugins.DraftCard{c}, now); len(p) == 1 {
		return append([]plugins.DraftCard{c}, pending...), archived, true
	}
	return pending, append([]plugins.DraftCard{c}, archived...), true
}

// withDraftAgentNames sets each card's AgentDisplay: the drafting agent's
// session name by the one naming rule (subjectName over one focus index read,
// what the sidebar names that agent), once per agent. A card whose agent is
// gone, or a read that fails, keeps drawing its stored AgentName.
func (s *Server) withDraftAgentNames(ctx context.Context, cards []plugins.DraftCard) {
	if s.ext.Agents == nil || len(cards) == 0 {
		return
	}
	fi := s.focusIndex(ctx)
	names := map[int64]string{}
	for i := range cards {
		a := cards[i].Draft.AgentID
		if a == nil || *a <= 0 {
			continue
		}
		n, ok := names[*a]
		if !ok {
			n = s.subjectName(ctx, fi, focus.AgentSubject(*a))
			names[*a] = n
		}
		cards[i].AgentDisplay = n
	}
}

// draftIDs is cards' ids, comma-joined in order: the Awaiting decision list's
// identity, which the page compares before re-drawing it (asksPageScript).
func draftIDs(cards []plugins.DraftCard) string {
	ids := make([]string, len(cards))
	for i, c := range cards {
		ids[i] = strconv.FormatInt(c.Draft.ID, 10)
	}
	return strings.Join(ids, ",")
}

// draftsPanel is the Drafts tab's panel: ONE list — the drafts awaiting a
// decision — and under it a "Show archived" toggle over the decided ones
// (round 0 of #768: no second tab strip inside the swipe area). sub
// asksTabArchived (?tab=archived, where a decision lands: draftDecidedLink)
// draws it with the archived list shown; the page's script keeps ?tab= in
// step with the toggle and opens it for a #draft-<id> inside it.
func draftsPanel(pending, archived []plugins.DraftCard, sub string) g.Node {
	// No intro prose (the design pass): each pending card's reply box says
	// what approving sends, and an approved card's Withdraw is its own offer.
	pendingList := `<p id="asks-drafts-changed" role="status" class="flex flex-wrap items-center gap-2 rounded-md bg-s2 px-3 py-2 text-sm text-fg ring-1 ring-inset ring-line" data-testid="drafts-changed" hidden>
<span>Drafts changed since you opened this list. The cards below are as you saw them.</span>
<button type="button" class="` + ui.AsksSecondaryButtonClass + `" data-drafts-show-latest>Show the latest</button></p>
<ul id="asks-drafts-pending" class="flex flex-col gap-2" data-testid="drafts-pending" data-draft-ids="` + draftIDs(pending) + `">` +
		draftListHTML(pending, draftsPendingEmpty) + `</ul>`
	open := sub == asksTabArchived
	expanded, hidden, label := "false", " hidden", draftsShowArchived
	if open {
		expanded, hidden, label = "true", "", draftsHideArchived
	}
	toggle := `<button type="button" id="asks-drafts-archived-toggle" class="` + ui.AsksSecondaryButtonClass + ` self-start" aria-expanded="` + expanded +
		`" aria-controls="asks-drafts-archived-section" data-drafts-show-archived>` + label + `</button>`
	archivedSection := `<section id="asks-drafts-archived-section" class="flex flex-col gap-2" aria-labelledby="asks-drafts-archived-label" data-drafts-archived-section` + hidden + `>` +
		ui.AsksGroupLabel("asks-drafts-archived-label", "Archived", len(archived)) +
		`<ul id="asks-drafts-archived" class="flex flex-col gap-2" data-testid="drafts-archived">` + draftListHTML(archived, draftsArchivedEmpty) + `</ul></section>`
	return g.Raw(`<div class="flex flex-col gap-2 pt-3">` + pendingList + toggle + archivedSection + `</div>`)
}

// The Drafts tab's archived toggle, shut and open.
const (
	draftsShowArchived = "Show archived"
	draftsHideArchived = "Hide archived"
)

// asksTabsHTML is the page's tabs (asks_feed.go): Open (askList, its feed's
// <ul>), Answered (answered, its feed's <ul>), Drafts when drafts (plugins are
// served), Nudges when nudges is not "" (its panel: the feature is on), and
// always Log (logPanel: the ask log's panel, or its placeholder —
// ask_log_read.go), the tab the request named selected (asksTab) — Open when
// that tab is not drawn. The panels sit in the swipe area (data-asks-swipe):
// a sideways swipe there moves one tab (ui/asks_card.go).
func (s *Server) asksTabsHTML(askList, answered string, drafts bool, pending, archived []plugins.DraftCard, nudges, logPanel, tab string) string {
	top := asksTabOpen
	switch {
	case tab == asksTabAnswered, tab == asksTabLog && logPanel != "":
		top = tab
	case tab == asksTabNudges && nudges != "":
		top = asksTabNudges
	case (tab == asksTabDrafts || tab == asksTabArchived) && drafts:
		top = asksTabDrafts
	}
	panel := func(inner string) g.Node { return g.Raw(`<div class="flex flex-col gap-2 pt-2">` + inner + `</div>`) }
	tabs := []ui.Tab{
		{ID: asksTabOpen, Label: "Open", Panel: panel(askList)},
		{ID: asksTabAnswered, Label: "Answered", Panel: panel(answered)},
	}
	if drafts {
		tabs = append(tabs, ui.Tab{ID: asksTabDrafts, Label: draftsTabLabel(len(pending)), Panel: draftsPanel(pending, archived, tab)})
	}
	if nudges != "" {
		tabs = append(tabs, ui.Tab{ID: asksTabNudges, Label: "Nudges",
			Panel: g.Raw(`<div class="flex flex-col gap-2 pt-3" data-testid="nudges-panel">` + nudges + `</div>`)})
	}
	// The Log tab only with the ask log on (logPanel ""): off, it was a tab of
	// setup prose ("set MUSTER_ASK_LOG=1 and restart"), which is the
	// operator's docs' to say (docs/asks.md), not a tab's.
	if logPanel != "" {
		tabs = append(tabs, ui.Tab{ID: asksTabLog, Label: "Log", Panel: g.Raw(`<div class="flex flex-col gap-2 pt-3" data-testid="log-panel">` + logPanel + `</div>`)})
	}
	t := ui.TabsSelected("asks-tabs", "", "Asks", top, tabs...)
	var b strings.Builder
	b.WriteString(`<div class="flex flex-col" data-asks-tabs><div class="flex overflow-x-auto border-b border-line">`)
	nodes := g.Group{t.List, g.Raw(`</div><div class="` + ui.AsksSwipeAreaClass + `" data-asks-swipe>`), t.Panels, g.Raw(`</div></div>`), ui.TabsScript()}
	if err := nodes.Render(&b); err != nil {
		s.logger.Printf("asks: render the tabs: %v", err)
	}
	return b.String()
}

// handleAsksDrafts is GET /attention/drafts[?draft=N]: the two lists, the
// pending list's ids and the tab's label as JSON, for the page's live refresh.
func (s *Server) handleAsksDrafts(w http.ResponseWriter, r *http.Request) {
	if s.ext.Plugins == nil {
		http.NotFound(w, r)
		return
	}
	pending, archived, ok := s.readDraftViews(r.Context(), asksFocus(r))
	if !ok {
		s.writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "the reply drafts could not be read"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, http.StatusOK, map[string]any{
		"pending":     draftListHTML(pending, draftsPendingEmpty),
		"pending_ids": draftIDs(pending),
		"archived":    draftListHTML(archived, draftsArchivedEmpty),
		"label":       draftsTabLabel(len(pending)),
	})
}

// asksPageScript is the page's own behaviour around the tabs (the cards' —
// expand, answer and dismiss, keys, swipe — is ui/asks_card.go's):
//
//   - the tab is kept in the URL (?tab=), replaced not pushed, so a reload or
//     a shared link opens the same tab. The PATH is always /attention: a
//     refusal drawn in answer to a decision's POST (/plugins/drafts/N/approve)
//     must not leave a POST-only address behind for a reload to 405 on;
//   - a fragment naming a card inside a hidden panel opens that panel's tabs
//     and scrolls to it (a push opened on ?tab=drafts for a draft that was
//     decided meanwhile lands on it in Archived);
//   - the drafts lists follow the queue's event (asks:changed, dispatched by
//     attentionPageScript, whose 60 s backstop fires it too) — never while
//     the confirm dialog is open, so the form it will submit is still in the
//     page. 🔴 Awaiting decision is re-drawn in place ONLY when its set of
//     drafts is unchanged (data-draft-ids); otherwise the cards on screen stay
//     and #asks-drafts-changed offers "Show the latest" (see the file header).
//     Archived is re-drawn, less any card still drawn in Awaiting decision (an
//     id names one element).
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
    var arch = document.getElementById('asks-drafts-archived-section');
    var top = selected('asks-tabs'), v = top === '` + asksTabOpen + `' ? '' : (top === '` + asksTabNudges + `' || top === '` + asksTabLog + `' || top === '` + asksTabAnswered + `' ? top : (arch && !arch.hidden ? '` + asksTabArchived + `' : '` + asksTabDrafts + `'));
    try {
      var u = new URL(window.location.href);
      u.pathname = '` + attentionPath + `';
      if (v) u.searchParams.set('` + asksTabParam + `', v); else u.searchParams.delete('` + asksTabParam + `');
      if (u.href !== window.location.href) history.replaceState(history.state, '', u.href);
    } catch (e) {}
    if (top === '` + asksTabLog + `') loadLog();
  }
  // The Log tab's panel is fetched the first time the tab is opened
  // (ask_log_read.go): the page's own render reads nothing from the log.
  function loadLog() {
    var lazy = root.querySelector('[data-asklog-lazy]');
    if (!lazy || lazy.hasAttribute('data-loading')) return;
    lazy.setAttribute('data-loading', '');
    fetch(lazy.getAttribute('data-src'), { credentials: 'same-origin' }).then(function (r) {
      if (!r.ok) throw new Error(String(r.status));
      return r.text();
    }).then(function (h) {
      var t = document.createElement('template');
      t.innerHTML = h;
      lazy.replaceWith(t.content);
    }).catch(function () {
      lazy.removeAttribute('data-loading');
      var p = lazy.querySelector('[data-asklog-loading]');
      if (p) p.insertAdjacentText('afterbegin', 'The log could not be loaded. ');
    });
  }
  if (window.location.pathname !== '` + attentionPath + `') syncURL();
  root.addEventListener('click', function (e) {
    if (e.target && e.target.closest && e.target.closest('[role="tab"]')) setTimeout(syncURL, 0);
  });
  root.addEventListener('keydown', function () { setTimeout(syncURL, 0); });
  // The Drafts tab's "Show archived" (draftsPanel): shows or hides the
  // decided drafts, and the address says which (?tab=archived / drafts).
  function showArchived(on) {
    var sec = document.getElementById('asks-drafts-archived-section'), b = document.getElementById('asks-drafts-archived-toggle');
    if (!sec || !b) return;
    sec.hidden = !on;
    b.setAttribute('aria-expanded', on ? 'true' : 'false');
    b.textContent = on ? '` + draftsHideArchived + `' : '` + draftsShowArchived + `';
  }
  root.addEventListener('click', function (e) {
    if (!e.target || !e.target.closest || !e.target.closest('[data-drafts-show-archived]')) return;
    var sec = document.getElementById('asks-drafts-archived-section');
    showArchived(!!(sec && sec.hidden));
    setTimeout(syncURL, 0);
  });
  function reveal() {
    var id = decodeURIComponent((window.location.hash || '').slice(1));
    var el = id && document.getElementById(id);
    if (!el || !root.contains(el) || !window.muTabs) return;
    if (el.closest('[data-drafts-archived-section]')) showArchived(true);
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
  // The Answered feed follows the queue's event too (it has no field to keep).
  var answered = document.getElementById('attention-answered');
  if (answered) document.addEventListener('asks:changed', function () {
    fetch('` + attentionPath + `/items?part=answered', { credentials: 'same-origin' }).then(function (r) {
      return r.ok ? r.text() : null;
    }).then(function (h) {
      if (h === null) return;
      answered.innerHTML = h;
      // The card feed's script keeps expanded cards and focus (its reveal
      // handler serves every feed).
      try { answered.dispatchEvent(new Event(` + "'" + ui.DictateRevealEvent + "'" + `, { bubbles: true })); } catch (e) {}
    }).catch(function () {});
  });
  var pendingList = document.getElementById('asks-drafts-pending');
  var archivedList = document.getElementById('asks-drafts-archived');
  var changed = document.getElementById('asks-drafts-changed');
  var draftsTab = document.getElementById('asks-tabs-tab-drafts');
  if (!pendingList || !archivedList) return;
  function apply(j, latest) {
    if (latest || j.pending_ids === (pendingList.getAttribute('data-draft-ids') || '')) {
      pendingList.innerHTML = j.pending;
      pendingList.setAttribute('data-draft-ids', j.pending_ids);
      if (changed) changed.hidden = true;
    } else if (changed) {
      changed.hidden = false;
    }
    var t = document.createElement('template');
    t.innerHTML = j.archived;
    Array.prototype.forEach.call(t.content.querySelectorAll('li[id]'), function (li) {
      if (pendingList.querySelector('[id="' + li.id + '"]')) li.remove();
    });
    archivedList.replaceChildren(t.content);
    if (draftsTab) draftsTab.textContent = j.label;
  }
  function load(latest) {
    var dlg = document.getElementById('term-confirm');
    if (dlg && dlg.open) return;
    var q = '';
    try { var d = new URL(window.location.href).searchParams.get('` + asksDraftParam + `'); if (d) q = '?` + asksDraftParam + `=' + encodeURIComponent(d); } catch (e) {}
    fetch('` + asksDraftsPath + `' + q, { credentials: 'same-origin' }).then(function (r) {
      return r.ok ? r.json() : null;
    }).then(function (j) {
      if (!j || (dlg && dlg.open)) return;
      apply(j, latest);
    }).catch(function () {});
  }
  document.addEventListener('asks:changed', function () { load(false); });
  if (changed) changed.addEventListener('click', function (e) {
    if (e.target && e.target.closest && e.target.closest('[data-drafts-show-latest]')) load(true);
  });
})();
</script>`
