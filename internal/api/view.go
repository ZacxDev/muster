package api

import (
	"net/http"
	"net/url"
	"regexp"
	"strconv"
)

// --- Which document issued this request --------------------------------------
//
// A task card renders in one of two shapes (see ui.TaskCardView.Detail): the
// compact BOARD card that /tasks lists, or the full DETAIL card that IS the
// /tasks/{id} document. Every mutation route re-renders through renderNoteCard,
// so the shape has to be decided from the request — one predicate, one place, no
// duplicated routes.
//
// 🔴 IT MUST NOT BE DECIDED BY A HEADER THE MARKUP STAMPS. That was the first
// design: `hx-headers='{"X-Muster-View":"detail"}'` on the detail document's
// <body>, read back here. htmx boosts with target=document.body and
// swap=innerHTML, and an innerHTML swap replaces a node's CHILDREN, never its
// ATTRIBUTES — so a boosted sidebar click (all eight tabs are boosted off-shell,
// by design) navigated to the board and left the stamp on the live <body>. The
// board's own status PATCH / tag edit / comment POST then inherited it and got
// DETAIL cards morphed into #tasks-list: a second <h1>, a full body and comment
// form inside a list card, and no card-link, so the card stopped being
// clickable. Nothing errored.
//
// HX-Current-URL does not have that failure mode. htmx recomputes it from
// `location` when it builds each request's headers (htmx 2.0.4, the shared
// header builder: "HX-Current-URL": getDocument().location.href), so it is a
// statement about where the browser IS at request time, not about markup that
// happened to survive a swap. A non-htmx caller (curl, the JSON API) sends none
// and correctly gets the board shape.

// ⚠ IT IS A CLIENT-SUPPLIED HEADER, and that is fine HERE and only here. It
// selects which SHAPE of a card the caller gets back — never what they are
// allowed to read or write. A caller who forges it gets a differently-laid-out
// rendering of a task they were already authorised to fetch and mutate. Do not
// grow a second use of it that gates access.

// hxCurrentURL is htmx's own per-request header naming the document's live URL.
const hxCurrentURL = "HX-Current-URL"

// detailDocumentPath matches the ONE path whose document renders a task in
// DETAIL shape. Anchored and digits-only on purpose: /tasks, /tasks/741/edit and
// /tasks/merge are all board-shaped, and a loose prefix match would claim them.
var detailDocumentPath = regexp.MustCompile(`^/tasks/([0-9]+)$`)

// canonicalTaskPath is the ONE URL a task's detail document is served at, and it
// is the ONE spelling this file's regexp accepts.
//
// 🔴 IT EXISTS BECAUSE TWO PARSERS DISAGREED. handleTaskDetail read the id with
// strconv.ParseInt, which accepts a leading '+' (and a leading '-'); the regexp
// above does not. So GET /tasks/+12 rendered the DETAIL document for task 12
// perfectly, while every mutation issued from that page arrived with
// HX-Current-URL=/tasks/+12, matched nothing here, and came back as a BOARD card
// morphed into #task-12 — the exact F1 symptom, on a URL a paste can produce.
// (/tasks/012 was always fine: the regexp matches it and ParseInt agrees.)
//
// The fix is not a second, matching parser — that is the duplicated predicate
// that regenerates the same bug. handleTaskDetail REDIRECTS any non-canonical
// spelling here, so by the time a document exists to issue requests, its URL is
// one this file matches. TestDetailRouteAndViewSeamAgreeOnEveryIdSpelling pins
// the relationship rather than either half.
func canonicalTaskPath(id int64) string {
	return "/tasks/" + strconv.FormatInt(id, 10)
}

// detailDocumentTaskID reports the task id of the /tasks/{id} document that
// issued this request, if it was issued by one at all.
func detailDocumentTaskID(r *http.Request) (int64, bool) {
	raw := r.Header.Get(hxCurrentURL)
	if raw == "" {
		return 0, false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return 0, false
	}
	m := detailDocumentPath.FindStringSubmatch(u.Path)
	if m == nil {
		return 0, false
	}
	id, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// rendersInDetailShape reports whether a card for noteID, written back to the
// document that issued r, must be the DETAIL card.
//
// 🔴 THE IDS MUST AGREE, not merely "some detail page asked". The detail
// document contains exactly one card — its own — so a request from /tasks/999
// that mutates task 741 is not a detail-shaped write-back for anyone: it would
// answer with a full-body card carrying a second <h1> for an element that is not
// that document's subject.
func rendersInDetailShape(r *http.Request, noteID int64) bool {
	id, ok := detailDocumentTaskID(r)
	return ok && id == noteID
}
