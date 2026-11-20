package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// AN ASKER'S OPEN ASKS: A COUNT IN ITS SESSION PAGE'S HEADER, LISTED ON THE
// ASKS PAGE.
//
// 🔴 NO BLOCK ON THE SESSION PAGE (operator decision): an agent's page (the
// chief's included) and a mirrored window's rendered view used to draw the
// asker's open asks as a block above the composer, with the queue's answer
// forms. It took too much of the screen, and the questions are answered from
// their notification's card or the Asks page anyway. Now the page's HEADER
// carries a count badge (ui.AskerAsksBadge) of the asker's OPEN asks, and a
// tap on it opens the Asks page filtered to them (/attention?from=<key>):
// answered, dismissed and listed there, like every other ask.
//
// 🔴 WHOSE ASKS: an agent page counts the asks whose asker IS that agent
// (Asker.AgentID); a window's view counts the asks of the mirrored Claude
// sessions that window runs, by the rule the sidebar's attention counts and the
// push use (claudeSessionWindow). Never another asker's, and row-level security
// keeps it to the signed-in owner's. The badge's count and the filtered page's
// list are ONE predicate (askerOf.has), and both are counted by the rule of
// every count (waitingOnReader), so the number tapped is the filtered page's
// "N open" (a team's ask a viewer only watches is listed, not counted).
//
// THE KEY (askerRef.key, the ?from= value) follows how an ask records its
// asker: an agent ("agent:<name>"), or a mirrored Claude session, which a page
// reaches through the window that runs it ("window:<host>:<window id>:<tmux
// session>"; the tmux session last, as the one part that may hold a colon).
//
// 🔴 OWNER SCOPING (resolveAsker): a key is resolved AS THE VIEWER — an agent
// the viewer's principal can read, a window among the viewer's own hosts. A key
// that does not resolve (another person's agent or window, a malformed key) is
// IGNORED: the Asks page lists what it lists without one, and names nothing
// the viewer cannot see.
//
// LIVE: the badge re-reads GET /attention/asker on the queue's SSE event
// (EventAttentionChanged) — on the agent page through the shell's htmx SSE, on
// the view (its own document, no htmx) through ui.AttentionLiveScript, the Asks
// page's own script — so an ask answered anywhere, or a new one, moves the
// count without a reload.
// ---------------------------------------------------------------------------

// attentionAskerPath is the badge's fragment route.
const attentionAskerPath = attentionPath + "/asker"

// askerFromParam is the query parameter naming an asker (askerRef.key): on the
// badge's fragment route and on the Asks page, whose Open list it filters.
const askerFromParam = "from"

// askerRef names an asker's session page: an agent (by name) or a mirrored
// window.
type askerRef struct {
	agent  string
	mirror ui.MirrorRef
}

// The key's two prefixes (askerRef.key).
const (
	askerKeyAgent  = "agent:"
	askerKeyWindow = "window:"
)

// key is r as the ?from= value (THE KEY above).
func (r askerRef) key() string {
	if r.agent != "" {
		return askerKeyAgent + r.agent
	}
	return askerKeyWindow + r.mirror.Host + ":" + r.mirror.Window + ":" + r.mirror.Session
}

// parseAskerKey is the askerRef a ?from= value names, and whether it names one.
func parseAskerKey(k string) (askerRef, bool) {
	switch {
	case strings.HasPrefix(k, askerKeyAgent):
		name := strings.TrimPrefix(k, askerKeyAgent)
		return askerRef{agent: name}, name != ""
	case strings.HasPrefix(k, askerKeyWindow):
		p := strings.SplitN(strings.TrimPrefix(k, askerKeyWindow), ":", 3)
		if len(p) != 3 || p[0] == "" || p[2] == "" {
			return askerRef{}, false
		}
		return askerRef{mirror: ui.MirrorRef{Host: p[0], Window: p[1], Session: p[2]}}, true
	}
	return askerRef{}, false
}

// query is r as the ?from= query.
func (r askerRef) query() string { return url.Values{askerFromParam: {r.key()}}.Encode() }

// src is the badge's fragment URL.
func (r askerRef) src() string { return attentionAskerPath + "?" + r.query() }

// asksHref is the Asks page filtered to r's asks: where the badge goes.
func (r askerRef) asksHref() string { return attentionPath + "?" + r.query() }

// askerOf is an askerRef resolved for one viewer (resolveAsker): the agent's id
// or the window, and the name the Asks page's filter line calls it.
type askerOf struct {
	ref     askerRef
	agentID int64
	label   string
}

// resolveAsker is r resolved as ctx's principal, and whether it resolves: an
// agent ctx can read (row-level security: another person's agent is not
// found), or a window among hosts() — ctx's principal's own hosts. hosts is
// called only for a window.
func (s *Server) resolveAsker(ctx context.Context, r askerRef, hosts func() []mirror.HostView) (askerOf, bool) {
	if r.agent != "" {
		if s.ext.Agents == nil {
			return askerOf{}, false
		}
		a, err := s.ext.Agents.GetByName(ctx, r.agent)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				s.logger.Printf("attention: the asks of agent %q: %v", r.agent, err)
			}
			return askerOf{}, false
		}
		return askerOf{ref: r, agentID: a.ID, label: firstNonBlank(a.DisplayName, a.Name)}, true
	}
	for _, e := range mirror.Ordered(hosts()) {
		if e.Host == r.mirror.Host && e.Session == r.mirror.Session && (r.mirror.Window == "" || e.Window == r.mirror.Window) {
			return askerOf{ref: r, label: e.DisplayTitle()}, true
		}
	}
	return askerOf{}, false
}

// has reports whether it is one of k's asks: an ask (not a notice, not another
// kind of item) raised by k (from).
func (k askerOf) has(it attentionItem, hosts func() []mirror.HostView) bool {
	return it.Kind == attentionKindAsk && k.from(it, hosts)
}

// from reports whether it was raised by k, whatever its kind: by k's agent, or
// by a Claude session k's window runs (claudeSessionWindow over hosts(),
// called only for a window's item).
func (k askerOf) from(it attentionItem, hosts func() []mirror.HostView) bool {
	if k.ref.agent != "" {
		return it.card.agentID == k.agentID
	}
	if it.card.claudeSession == "" {
		return false
	}
	ref, ok := claudeSessionWindow(hosts(), it.card.ref.Host, it.card.claudeSession)
	w := k.ref.mirror
	return ok && ref.Host == w.Host && ref.Session == w.Session && (w.Window == "" || ref.Window == w.Window)
}

// only is items without what is not k's (has).
func (k askerOf) only(items []attentionItem, hosts []mirror.HostView) []attentionItem {
	hs := func() []mirror.HostView { return hosts }
	out := items[:0:0]
	for _, it := range items {
		if k.has(it, hs) {
			out = append(out, it)
		}
	}
	return out
}

// askerAsks is ctx's principal's open asks raised by r's asker, as the Asks
// page builds them (attentionItemsWith: a question to the chief and a notice
// are not asks waiting on the owner), and whether ANY open ask was raised by
// it, those two included (the suggested replies' block: suggest.go withholds
// them while any ask from the session is open). nil, false without the
// queue's store, for an asker that does not resolve for ctx, and on a read
// error (logged): the page then shows no badge rather than failing.
func (s *Server) askerAsks(ctx context.Context, r askerRef) ([]attentionItem, bool) {
	if s.ext.Attention == nil {
		return nil, false
	}
	// The owner's windows, read once and only when a window or a mirrored ask
	// needs them.
	var hosts []mirror.HostView
	listed := false
	hostViews := func() []mirror.HostView {
		if !listed && s.ext.Mirror != nil {
			hosts, listed = s.ext.Mirror.Hosts(ctx), true
		}
		return hosts
	}
	k, ok := s.resolveAsker(ctx, r, hostViews)
	if !ok {
		return nil, false
	}
	asks, err := s.ext.Attention.Open(ctx)
	if err != nil {
		s.logger.Printf("attention: the asks for a session page: %v", err)
		return nil, false
	}
	var out []attentionItem
	anyOpen := false
	for _, a := range asks {
		var hs []mirror.HostView
		if askNeedsHosts(a) {
			hs = hostViews()
		}
		it := askItem(a, hs...)
		if !k.from(it, hostViews) {
			continue
		}
		anyOpen = true
		if !a.ToChief && a.Kind != attention.KindNotice {
			out = append(out, it)
		}
	}
	return out, anyOpen
}

// askerAsksBadge is ctx's principal's badge for r: its content
// (ui.AskerAsksBadgeHTML) and its fragment URL; both "" when the queue is not
// served (the page draws no badge).
//
// 🔴 IT COUNTS WHAT WAITS ON THE READER (waitingOnReader, the rule of every
// other count): a team's ask a viewer of the team only watches is listed on
// the filtered page, but it is not "waiting on your answer", so it is not
// counted — a viewer on the team chief's page saw "2" for questions they
// could not answer, and the page the badge opened then said "0 open".
func (s *Server) askerAsksBadge(ctx context.Context, r askerRef) (string, string) {
	if s.ext.Attention == nil {
		return "", ""
	}
	asks, anyOpen := s.askerAsks(ctx, r)
	return ui.AskerAsksBadgeHTML(len(waitingOnReader(ctx, asks)), r.asksHref(), anyOpen), r.src()
}

// handleAttentionAsker is GET /attention/asker?from=<key>: the badge's
// content, for its live refresh.
func (s *Server) handleAttentionAsker(w http.ResponseWriter, r *http.Request) {
	if s.ext.Attention == nil {
		http.NotFound(w, r)
		return
	}
	ref, ok := parseAskerKey(r.URL.Query().Get(askerFromParam))
	if !ok {
		http.Error(w, "name an asker: from=agent:<name> or from=window:<host>:<window>:<session>", http.StatusBadRequest)
		return
	}
	body, _ := s.askerAsksBadge(r.Context(), ref)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(body))
}

// asksFilter is the Asks page's ?from= filter (THE KEY above), resolved as
// ctx's principal over hosts (the render's one hub read); false for none — no
// ?from=, a malformed one, or one the viewer cannot see (OWNER SCOPING:
// ignored, the page lists what it lists without it).
func (s *Server) asksFilter(ctx context.Context, q url.Values, hosts []mirror.HostView) (askerOf, bool) {
	ref, ok := parseAskerKey(q.Get(askerFromParam))
	if !ok {
		return askerOf{}, false
	}
	return s.resolveAsker(ctx, ref, func() []mirror.HostView { return hosts })
}

// attentionReturn is where an answer or a dismissal given on a page that
// draws the answer form outside the Asks page sends the browser back to: the
// form's `return`, when it is one of those pages (an ask's own page, an
// officer's page) on this origin (safeReturnTo — never an open redirect);
// otherwise the queue.
func attentionReturn(r *http.Request) string {
	if v, ok := attentionReturnPage(r); ok {
		return v
	}
	return attentionPath
}

// attentionReturnPage is the form's `return` when it is such a page
// (attentionReturn's rule), and whether it is. 🔴 A session page (an agent's,
// a window's rendered view) is not one any more: it draws no answer form (see
// NO BLOCK ON THE SESSION PAGE above), so a `return` naming one lands on the
// queue.
func attentionReturnPage(r *http.Request) (string, bool) {
	v := r.PostFormValue("return")
	if v == "" || safeReturnTo(v) != v {
		return "", false
	}
	return v, askPageReturn.MatchString(v) || officerPageReturn.MatchString(v) ||
		morningReturn.MatchString(v)
}

// askPageReturn is an ask page's own address (attention_ask_page.go), a page
// that draws the answer form.
var askPageReturn = regexp.MustCompile(`^` + askPagePrefix + `[1-9][0-9]{0,18}$`)

// morningReturn is a morning report's page (morning_report.go): its open
// questions are the queue's own cards, so it draws the answer form too.
var morningReturn = regexp.MustCompile(`^` + morningPathPrefix + `[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

// attentionFormFailed answers a refused answer or dismissal: given on a page
// attentionReturnPage names, that page again with a notice saying why;
// given on the queue, the queue page with its problem, as before.
func (s *Server) attentionFormFailed(w http.ResponseWriter, r *http.Request, err error, cur attention.Ask) {
	if asksCardRequest(r) {
		// An Asks page card's htmx post: its status and a small fragment.
		asksCardFailed(w, err, cur)
		return
	}
	if page, ok := attentionReturnPage(r); ok {
		code := noticeAskFailed
		switch {
		case isReleaseRefusal(err): // before ErrInvalid, which it wraps
			code = noticeAskReleaseRefused
		case errors.Is(err, attention.ErrNotOpen), errors.Is(err, attention.ErrNotFound):
			code = noticeAskClosed
		// A step-up card's two refusals (host_stepup.go), before ErrInvalid,
		// which they wrap: each says what the owner can do.
		case errors.Is(err, errHostDecisionNeedsPhone):
			code = noticeAskNeedsPhone
		case errors.Is(err, errHostDecisionLost):
			code = noticeAskLost
		case errors.Is(err, attention.ErrAnswerTooLong): // before ErrInvalid, which it wraps
			code = noticeAskTooLong
		case errors.Is(err, attention.ErrInvalid):
			code = noticeAskInvalid
		}
		http.Redirect(w, r, withNotice(page, code), http.StatusSeeOther)
		return
	}
	s.renderAttentionPage(w, r, attentionErrCode(err), attentionErrText(err, cur))
}

// An answer or a dismissal given on such a page that was not recorded: the
// page is shown again with one of these notices (composerNotices), saying why.
const (
	noticeAskClosed  = "ask-closed"
	noticeAskInvalid = "ask-invalid"
	noticeAskFailed  = "ask-failed"
	// noticeAskReleaseRefused: a Release option refused (answer_release.go).
	noticeAskReleaseRefused = "ask-release-refused"
	// noticeAskNeedsPhone / noticeAskLost: a step-up card's answer refused
	// (errHostDecisionNeedsPhone, errHostDecisionLost).
	noticeAskNeedsPhone = "ask-needs-phone"
	noticeAskLost       = "ask-lost"
	// noticeAskTooLong: an answer over attention.MaxAnswerRunes (a long
	// dictation can run past it).
	noticeAskTooLong = "ask-too-long"
)

func init() {
	composerNotices[noticeAskClosed] = struct {
		text string
		ok   bool
	}{"Not recorded: that question is no longer open — it was answered, dismissed or expired elsewhere.", false}
	composerNotices[noticeAskInvalid] = struct {
		text string
		ok   bool
	}{"Type an answer or pick an option.", false}
	composerNotices[noticeAskTooLong] = struct {
		text string
		ok   bool
	}{"Not recorded: the answer is longer than " + strconv.Itoa(attention.MaxAnswerRunes) + " characters. Shorten it and send it again.", false}
	composerNotices[noticeAskFailed] = struct {
		text string
		ok   bool
	}{"Not recorded: the answer could not be saved. Try again, or answer on the Asks page.", false}
	composerNotices[noticeAskNeedsPhone] = struct {
		text string
		ok   bool
	}{"Confirm this on your paired phone.", false}
	composerNotices[noticeAskLost] = struct {
		text string
		ok   bool
	}{"This request was lost when muster restarted; the session must ask again.", false}
	composerNotices[noticeAskReleaseRefused] = struct {
		text string
		ok   bool
	}{"Not released: the task changed (its gate is already off, a gate of yours is on it, or it is no longer open), " +
		"or only your own tap can release it. Nothing was recorded; open the task.", false}
}
