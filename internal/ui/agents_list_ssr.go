package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"
)

// THE AGENTS LIST IS IN THE AGENTS TAB'S FIRST PAINT.
//
// The tab's document used to draw #agents-list EMPTY and fill it from its own
// `load` request (GET /ui/agents): every visit painted a filter bar over
// nothing, then the cards. The document now carries the cards (ShellData.WithList,
// built by internal/api from the SAME agentsListCards the partial is built from),
// and the live wiring is unchanged — every SSE nudge, resync and local action
// still morphs the list in place.
//
// 🔴 ONE RENDERER. The document's list body and the partial's are both
// agentsListBody of the same views, so they cannot drift apart:
// TestTheAgentsDocumentDrawsTheListPartial (internal/api) compares the two for
// one state, byte for byte.
//
// 🔴 THE FIRST LIVE REFRESH IS A NO-OP WHEN NOTHING CHANGED. The SSE stream's
// open fires muster:resync (resyncScript), which refreshes the list right after
// every load. The document stamps the list with the VERSION of the state it was
// drawn from (data-list-v, AgentsListVersion); a refresh sends it back
// (agentsListVersionScript: AgentsListVersionHeader) and the server answers 204
// No Content while it is still current — htmx swaps nothing on a 204. A 200
// carries the new version in the same header and the script stamps it.
//
// ⚠ THE VERSION IS OF THE DATA, NOT OF THE BYTES. The bytes carry wall-clock
// renderings (a relative "42s", the idle tokens) that change with no change of
// state; the client keeps those current on its own clock (relTimeScript, the
// filter's restamp). So a version that names the bytes would never repeat, and
// one that names the views says exactly "nothing the server knows has moved".

// AgentsListVersionAttr is the list's version attribute, on #agents-list.
const AgentsListVersionAttr = "data-list-v"

// AgentsListVersionHeader carries the version both ways: the client's on a
// refresh request, the server's on a 200 response.
const AgentsListVersionHeader = "X-Muster-List-V"

// shellList is the list a document draws on its first paint.
type shellList struct {
	cards   []AgentCardView
	mirrors []MirrorCardView
	version string
}

// WithList is WithRecent plus the list's own cards, drawn into #agents-list on
// the first paint, stamped with version (AgentsListVersion of the same views).
func (d ShellData) WithList(list []AgentCardView, mirrors []MirrorCardView, refs []RecentRef, version string) ShellData {
	d = d.WithRecent(list, mirrors, refs)
	d.list = &shellList{cards: list, mirrors: mirrors, version: version}
	return d
}

// AgentsListVersion is the version of the list's state: a hash of the views it
// is rendered from and the viewer's recents (the out-of-band strip rides along
// with every list render).
func AgentsListVersion(list []AgentCardView, mirrors []MirrorCardView, refs []RecentRef) string {
	b, err := json.Marshal(struct {
		A []AgentCardView
		M []MirrorCardView
		R []RecentRef
	}{list, mirrors, refs})
	if err != nil {
		// Never equal to a version a client holds: the refresh is then a 200.
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:12])
}

// agentsListBody is what #agents-list holds: the one list, by activity, pins
// first (AgentsCardsWithMirrors), or the empty state.
func agentsListBody(list []AgentCardView, mirrors []MirrorCardView) g.Node {
	return AgentsCardsWithMirrors(list, mirrors)
}

// drawnList is what a document's #agents-list carries of its list: its version
// and its cards; nothing for a list the document does not draw (nil).
func drawnList(l *shellList) g.Node {
	if l == nil {
		return nil
	}
	return g.Group{g.Attr(AgentsListVersionAttr, l.version), agentsListBody(l.cards, l.mirrors)}
}

// listTrigger is the list's hx-trigger: no first load for a list the document
// drew (see agentsListTrigger).
func listTrigger(l *shellList) string {
	if l != nil {
		return agentsListLiveTrigger
	}
	return agentsListTrigger
}

// agentsListVersionScript sends the list's version with each of its refreshes
// and keeps it current. Bound once per window, like the other shell scripts;
// the list is looked up at use time (a boosted swap replaces it).
func agentsListVersionScript() g.Node {
	return Script(g.Raw(`
(function () {
  if (window.__musterListVersion) return;
  window.__musterListVersion = true;
  var ATTR = '` + AgentsListVersionAttr + `', HDR = '` + AgentsListVersionHeader + `';
  document.addEventListener('htmx:configRequest', function (e) {
    var t = e.detail && e.detail.elt;
    if (!t || t.id !== 'agents-list') return;
    var v = t.getAttribute(ATTR);
    if (v) e.detail.headers[HDR] = v;
  });
  document.addEventListener('htmx:afterRequest', function (e) {
    var t = e.detail && e.detail.elt, x = e.detail && e.detail.xhr;
    if (!t || t.id !== 'agents-list' || !x || x.status !== 200) return;
    var v = x.getResponseHeader(HDR);
    if (v) t.setAttribute(ATTR, v); else t.removeAttribute(ATTR);
  });
})();
`))
}
