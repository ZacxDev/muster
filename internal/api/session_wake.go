package api

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/fence"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/sessionwake"
	"github.com/ZacxDev/muster/internal/stepup"
)

// ---------------------------------------------------------------------------
// A DECISION WAKES THE MIRRORED SESSION IT IS FOR (migration 0177;
// docs/attention.md#delivery).
//
// A dispatched agent's ask answer is pushed into its chat as a turn
// (attention_delivery.go). A MIRRORED Claude Code session used to get nothing:
// it had to sit in attention_wait, or its owner went back to it and typed
// "I decided, carry on". Now, when the owner decides something such a session
// is waiting on, muster queues ONE line for it in the window's send queue:
//
//   - an ask it raised, answered or dismissed (answerRecorded, the cancel
//     doors) → the ask's mirror_host / mirror_session;
//   - a reply draft it wrote (a route's answer, plugin_reply_drafts.route_id),
//     approved or rejected by the owner (decideDraftAs, every door), and later
//     that approved draft's send outcome → the route's target_host /
//     target_key;
//   - a page it made, responded to (recordPageResponse) → the page's
//     mirror_host / mirror_session.
//   - work it ASKED for (migration 0178, session_wake_requests.go): an
//     officer's draft for an item it enqueued, decided and later sent or not
//     → the item's requested_host / requested_session; a step-up card its
//     call raised, settled → the card's RaisedByHost / RaisedBySession.
//
// 🔴 THE LINE IS MUSTER'S OWN WORDS AND THE OWNER'S OWN WORDS, NEVER A THIRD
// PARTY'S — by construction, not by care. renderWake is the ONE function that
// builds the pasted text, and it reads a sessionwake.Wake only: ids, a decision
// word, a count, a component name (0177's CHECKed shape) and OperatorText. The
// only writers of OperatorText are the wake* constructors below, each taking
// it from a row the owner authored: an answer the owner gave (answered_by
// operator), a reason the owner gave with a rejection, a response the owner
// sent from their OWN view of their OWN page (ByOwner, responder = the page's
// owner). Every other viewer's response is a count (AddPageResponses), which
// the database refuses to hold text for (0177's session_wakes_count_has_no_text).
// A draft's plugin detail, a page's title, a question's text and a draft's
// body are never on the line.
//
// 🔴 THROUGH THE SEND QUEUE, NEVER MID-TURN, EXACTLY ONCE. A wake is handed to
// the window's send queue as a row of its own origin (send_queue.session_wake_id),
// so it is pasted only once Claude Code has been idle for MUSTER_SEND_QUEUE_IDLE,
// one at a time, with the queue's own claim, lease, "Pasted, not submitted" and
// may-have-delivered rules. The wake's states are the answer delivery's
// (internal/sessionwake): queued → delivering (Hand: the row) → delivered |
// unconfirmed (wakeOutcome, conditional on still being that row's). At most one
// live queue row per wake (0177's send_queue_one_live_per_wake).
//
// 🔴 THE SESSION THAT ALREADY HAS IT IS NOT TYPED INTO. attention_wait
// returning the answer (or the dismissal) marks the wake waited
// (waitResult → MarkWaited), and the paste's gate (sendQueueWakeGate) refuses a
// row whose wake is no longer delivering on it — and, for an ask, an ask whose
// own delivery says the session read it (DeliveryWaited) — so whichever of the
// wait and the paste comes first wins. A session blocked in attention_wait is
// mid-turn (busy), so the queue never pastes into it while the wait runs.
//
// 🔴 ONLY INTO THE SESSION IT IS FOR. A wake is bound to (host, Claude Code
// session id); the window is the one whose host reports that session NOW
// (wakeWindow), and the queue row carries it as its AgentSession, so the host
// refuses the paste if the window moved on (a /clear, a new claude). A wake no
// window shows stays queued, saying why, and after a day settles
// "Not delivered", which the owner's Retry queues again. It never goes to
// another session.
//
// A TEAM's sessions get no wakes in this phase (their queue is the team's, gated
// on who queued; a wake has no person behind it), and neither does an opencode
// window (its relayed answers have their own path).
// ---------------------------------------------------------------------------

// wakeGiveUp is how long a wake may wait for its window (since it was queued
// or retried) before it settles "Not delivered".
const wakeGiveUp = 24 * time.Hour

// wakeMaxAttempts bounds how many queue rows one wake may have refused before
// it stops trying on its own. The day's give-up bounds the TIME, not the ROWS:
// a refusal that recurs at every paste while the window stays listed (the
// account signed out everywhere after the decision, a disabled account) would
// otherwise queue and fail a row on every 30-second sweep — ~2,900 failed rows
// for one line before the day ran out.
const wakeMaxAttempts = 10

// wakeBatch bounds one sweep's wakes.
const wakeBatch = 100

// wakeAnswerMarker / wakeReasonMarker / wakeResponseMarker are the fences the
// owner's own words travel in. The answer's is the dispatched-agent delivery's
// own (attentionDeliveryMarker), so a session reads both the same way.
const (
	wakeAnswerMarker   = attentionDeliveryMarker
	wakeReasonMarker   = "owner-reason"
	wakeResponseMarker = "page-response"
)

// wakesOn reports whether this server delivers wakes: a wakes store, the send
// queue and mirrored sessions.
func (s *Server) wakesOn() bool {
	return s.ext.SessionWakes != nil && s.ext.SendQueue != nil && s.ext.Mirror != nil
}

// ---------------------------------------------------------------------------
// The constructors: the only writers of a wake's OperatorText.
// ---------------------------------------------------------------------------

// wakeClaudeTarget is (host, session) when they name a mirrored Claude Code
// session, ok false otherwise (an opencode window, a dispatched agent, none).
func wakeClaudeTarget(host, session string) (string, string, bool) {
	if mirror.ValidHost(host) != nil || !mirror.ValidClaudeSession(session) {
		return "", "", false
	}
	return host, session, true
}

// wakeForAnswer is the wake an answered ask owes the mirrored session that
// raised it: the OWNER's answer (answered_by operator, through the owner's
// answer transition) — never the chief's answer to a question put to it,
// never an answer typed into the session itself (answered_in_session).
func wakeForAnswer(a attention.Ask) (sessionwake.New, bool) {
	host, sess, ok := wakeClaudeTarget(a.Asker.MirrorHost, a.Asker.MirrorSession)
	if !ok || a.Kind == attention.KindNotice || a.Status != attention.Answered || a.AnsweredBy != attention.ByOperator {
		return sessionwake.New{}, false
	}
	n := sessionwake.New{Kind: sessionwake.KindAskAnswered, AskID: a.ID, OperatorText: a.Answer, Host: host, Session: sess}
	if a.AnsweredVia.ViaSession() {
		// Given through attention_answer from a mirrored session: the line
		// says where (renderWake), never "you answered".
		n.ViaHost, n.ViaSession = a.AnsweredVia.Host, a.AnsweredVia.Session
	}
	return n, true
}

// wakeForDismiss is the wake an ask the owner dismissed owes its session: no
// text, only that no answer is coming.
func wakeForDismiss(a attention.Ask) (sessionwake.New, bool) {
	host, sess, ok := wakeClaudeTarget(a.Asker.MirrorHost, a.Asker.MirrorSession)
	if !ok || a.Kind == attention.KindNotice || a.Status != attention.Cancelled {
		return sessionwake.New{}, false
	}
	return sessionwake.New{Kind: sessionwake.KindAskDismissed, AskID: a.ID, Host: host, Session: sess}, true
}

// wakeDraftTarget is the mirrored session that wrote draft d — its route's
// target, when the route went to a mirrored Claude Code session — ok false for
// a draft the chief or an officer filed.
func wakeDraftTarget(r plugins.Route) (string, string, bool) {
	if r.TargetKind != plugins.RouteTargetMirror {
		return "", "", false
	}
	return wakeClaudeTarget(r.TargetHost, r.TargetKey)
}

// wakeForDraftDecision is the wake the owner's decision on draft d owes the
// session that wrote it (route r): approved or rejected, and — with a
// rejection — the reason the OWNER gave (0123), fenced.
func wakeForDraftDecision(d plugins.Draft, r plugins.Route) (sessionwake.New, bool) {
	host, sess, ok := wakeDraftTarget(r)
	if !ok {
		return sessionwake.New{}, false
	}
	n := sessionwake.New{Kind: sessionwake.KindDraftDecided, DraftID: d.ID, RouteID: r.RouteID, Host: host, Session: sess}
	switch d.Status {
	case plugins.DraftApproved:
		n.Decision = sessionwake.DecisionApproved
	case plugins.DraftRejected:
		n.Decision = sessionwake.DecisionRejected
		if d.DecisionReason != nil {
			n.OperatorText = *d.DecisionReason
		}
	default:
		return sessionwake.New{}, false
	}
	return n, true
}

// wakeForDraftOutcome is the wake an approved draft's send outcome owes its
// session: sent, failed or unconfirmed — the word only, never the plugin's
// detail (a plugin's words are not the owner's).
func wakeForDraftOutcome(d plugins.Draft, r plugins.Route) (sessionwake.New, bool) {
	host, sess, ok := wakeDraftTarget(r)
	if !ok {
		return sessionwake.New{}, false
	}
	n := sessionwake.New{Kind: sessionwake.KindDraftOutcome, DraftID: d.ID, RouteID: r.RouteID, Host: host, Session: sess}
	switch d.Status {
	case plugins.DraftSent:
		n.Decision = sessionwake.DecisionSent
	case plugins.DraftFailed:
		n.Decision = sessionwake.DecisionFailed
	case plugins.DraftUnconfirmed:
		n.Decision = sessionwake.DecisionUnconfirmed
	default:
		return sessionwake.New{}, false
	}
	return n, true
}

// pageWake is what one response on page pg owes the session that made it:
// own true with the owner's own wake when the PAGE'S OWNER sent it from their
// own view — else (a viewer through a link, a team member) only a count, which
// the caller files with AddPageResponses and which carries no text.
func pageWake(pg pages.Page, r pages.Response) (n sessionwake.New, own, ok bool) {
	host, sess, ok := wakeClaudeTarget(pg.Creator.MirrorHost, pg.Creator.MirrorSession)
	if !ok || r.PageID != pg.ID {
		return sessionwake.New{}, false, false
	}
	// 🔴 THE OWNER'S OWN, AND ONLY THEIRS: sent from their own view (no link)
	// AND recorded as sent by the page's owner (responder_user, forced to the
	// caller by the database). A team member's response on a team page is
	// another viewer's.
	if !r.ByOwner || r.LinkID != 0 || r.ResponderUser <= 0 || r.ResponderUser != pg.OwnerID {
		return sessionwake.New{Host: host, Session: sess, PageID: pg.ID}, false, true
	}
	n = sessionwake.New{Kind: sessionwake.KindPageOwn, PageID: pg.ID, ResponseID: r.ID, Component: r.Component, Host: host, Session: sess}
	if r.Kind == pages.KindSubmit {
		n.OperatorText = pageResponseText(r.Data)
	}
	return n, true, true
}

// pageResponseText is an owner's submit as the lines the fence carries: each
// field as `"name": value` in JSON, one per line, sorted — what the owner's
// Send-as-you bar showed them.
func pageResponseText(data []byte) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil || len(m) == 0 {
		return strings.TrimSpace(string(data))
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var lines []string
	for _, k := range keys {
		kj, _ := json.Marshal(k)
		lines = append(lines, string(kj)+": "+string(m[k]))
	}
	out := strings.Join(lines, "\n")
	if len(out) > sessionwake.MaxOperatorTextBytes {
		out = strings.ToValidUTF8(out[:sessionwake.MaxOperatorTextBytes-len("…")], "") + "…"
	}
	return out
}

// ---------------------------------------------------------------------------
// The one renderer.
// ---------------------------------------------------------------------------

// renderWake is THE text a wake becomes — every line muster pastes into a
// mirrored session for a decision. muster's own sentence (ids, decision
// words, counts, and where a decision made from a mirrored session came from,
// re-checked against the mirror wire's shapes) outside any fence; the owner's
// own words, when the wake carries any (OperatorText), inside one, every line
// sanitised (fence.Sanitize) behind the fence's content prefix — and cut, with
// a line saying so and where to read it whole, to fit the paste's bound
// (mirror.MaxPaste). Nothing else of a row is read, so nothing else reaches
// the window.
func renderWake(w sessionwake.Wake) string {
	ask := "ask #" + strconv.FormatInt(w.AskID, 10)
	draft := "reply draft #" + strconv.FormatInt(w.DraftID, 10)
	if w.RouteID != "" {
		draft += " (route " + w.RouteID + ")"
	}
	page := "page #" + strconv.FormatInt(w.PageID, 10)
	readPage := "page_responses{page_id: " + strconv.FormatInt(w.PageID, 10) + "}"
	via := wakeViaFrom(w)
	switch w.Kind {
	case sessionwake.KindAskAnswered:
		readAsk := "attention_wait{id: " + strconv.FormatInt(w.AskID, 10) + "}"
		tail := " act on it now. It is delivered once; you do not need to call attention_wait for it."
		if via != "" {
			return fenced(ask+", which this session asked, has an answer — answered via attention_answer from "+via+
				", not by you on the Attention view. The block below is that answer, relayed by muster:"+tail, wakeAnswerMarker, "answer:", w.OperatorText, readAsk)
		}
		return fenced("you answered "+ask+", which this session asked. The block below is your answer, relayed by muster:"+tail,
			wakeAnswerMarker, "answer:", w.OperatorText, readAsk)
	case sessionwake.KindAskDismissed:
		return "muster: you dismissed " + ask + ", which this session asked, without answering it. No answer is coming: " +
			"carry on without it, or ask again if you still need the decision."
	case sessionwake.KindDraftDecided:
		who, whose := "you approved "+draft+", which this session wrote.", "you gave"
		if w.Decision != sessionwake.DecisionApproved {
			who = "you rejected " + draft + ", which this session wrote."
		}
		if via != "" {
			verb := "approved"
			if w.Decision != sessionwake.DecisionApproved {
				verb = "rejected"
			}
			who, whose = draft+", which this session wrote, was "+verb+" — via plugin_draft_decide from "+via+", not by you on the Asks page.", "given"
		}
		if w.Decision == sessionwake.DecisionApproved {
			return "muster: " + who + " Its plugin sends it next; muster tells this session how that went."
		}
		pre := who + " It will not be sent."
		if strings.TrimSpace(w.OperatorText) == "" {
			return "muster: " + pre
		}
		return fenced(pre+" The block below is the reason "+whose+", relayed by muster.", wakeReasonMarker, "reason:", w.OperatorText, "")
	case sessionwake.KindDraftOutcome:
		who := "you approved " + draft + ", which this session wrote"
		if via != "" {
			who = draft + ", which this session wrote and which was approved via plugin_draft_decide from " + via + ","
		}
		switch w.Decision {
		case sessionwake.DecisionSent:
			return "muster: " + who + " — it was sent."
		case sessionwake.DecisionFailed:
			return "muster: " + who + " — it could not be sent (its plugin reported a failure). Nothing more is sent on its own."
		default:
			return "muster: " + who + " — it may or may not have been sent (its plugin did not confirm it). Nothing more is sent on its own."
		}
	case sessionwake.KindPageOwn:
		at := page
		if w.Component != "" {
			at += " (component " + w.Component + ")"
		}
		if strings.TrimSpace(w.OperatorText) == "" {
			return "muster: you responded on " + at + ", which this session made, from your own view: read it with " + readPage + "."
		}
		return fenced("you responded on "+at+", which this session made, from your own view. The block below is your response, relayed by muster.",
			wakeResponseMarker, "response:", w.OperatorText, readPage)
	case sessionwake.KindPageResponses:
		n := "1 new response"
		if w.Count != 1 {
			n = strconv.Itoa(w.Count) + " new responses"
		}
		return "muster: " + page + ", which this session made, got " + n + " from its viewers — read them with " + readPage + "."
	case sessionwake.KindOfficerDraftDecided:
		// An officer's draft for an item this session enqueued
		// (session_wake_requests.go): the draft and the item by id, never
		// the draft's body or the officer's words.
		ref := officerDraftRef(w)
		verb := "approved"
		if w.Decision != sessionwake.DecisionApproved {
			verb = "rejected"
		}
		who, whose := "you "+verb+" "+ref+".", "you gave"
		if via != "" {
			who, whose = ref+", was "+verb+" — via plugin_draft_decide from "+via+", not by you on the Asks page.", "given"
		}
		if w.Decision == sessionwake.DecisionApproved {
			return "muster: " + who + " Its plugin sends it next; muster tells this session how that went."
		}
		pre := who + " It will not be sent."
		if strings.TrimSpace(w.OperatorText) == "" {
			return "muster: " + pre
		}
		return fenced(pre+" The block below is the reason "+whose+", relayed by muster.", wakeReasonMarker, "reason:", w.OperatorText, "")
	case sessionwake.KindOfficerDraftOutcome:
		who := officerDraftRef(w)
		if via != "" {
			who += ", approved via plugin_draft_decide from " + via
		}
		switch w.Decision {
		case sessionwake.DecisionSent:
			return "muster: " + who + " — it was sent."
		case sessionwake.DecisionFailed:
			if w.Count > 1 {
				return "muster: " + who + " — it could not be sent after " + strconv.Itoa(w.Count) +
					" attempts (its plugin reported a failure), and muster stopped retrying it. Nothing more is sent on its own."
			}
			return "muster: " + who + " — it could not be sent (its plugin reported a failure). Nothing more is sent on its own."
		default:
			return "muster: " + who + " — it may or may not have been sent (its plugin did not confirm it). Nothing more is sent on its own."
		}
	case sessionwake.KindStepUpDecided:
		// A step-up card this session's call raised (session_wake_requests.go):
		// the card by id and the tool by its ledger name, never its arguments
		// or its output.
		card := stepUpCardRef(w)
		switch w.Decision {
		case sessionwake.DecisionConfirmed:
			return "muster: you confirmed " + card + ", on your phone: muster ran that call once, exactly as asked. " +
				"Do not call it again; read what it did with the list tools."
		case sessionwake.DecisionConfirmedFailed:
			return "muster: you confirmed " + card + ", on your phone: muster ran that call once, and the tool refused it. " +
				"Read why with the list tools before you ask again."
		case sessionwake.DecisionRefused:
			return "muster: you refused " + card + ", on your phone: the call did not run, and nothing was done."
		case sessionwake.DecisionDismissed:
			return "muster: you dismissed " + card + ", without confirming it: the call did not run, and nothing was done."
		case sessionwake.DecisionSettledApproved:
			return "muster: " + card + ", was settled when you approved the draft it is about on another door; " +
				"the call itself was not run. Read the draft with the list tools."
		case sessionwake.DecisionSettledRejected:
			return "muster: " + card + ", was settled when you rejected the draft it is about on another door; " +
				"the call itself was not run. Read the draft with the list tools."
		}
	}
	return ""
}

// wakeViaFrom names, for text written OUTSIDE the fence in muster's voice, the
// mirrored session w's decision was made from: "<host>/<session>" when both
// have the mirror wire's shapes, "a mirrored Claude Code session" when they do
// not (re-checked here although 0177's CHECKs hold them: mcpAnswerFrom's
// reason), "" for a decision made on muster's own pages or the phone.
func wakeViaFrom(w sessionwake.Wake) string {
	if w.ViaHost == "" && w.ViaSession == "" {
		return ""
	}
	if mirror.ValidHost(w.ViaHost) == nil && mirror.ValidClaudeSession(w.ViaSession) {
		return w.ViaHost + "/" + w.ViaSession
	}
	return "a mirrored Claude Code session"
}

// wakeCutNote is the line a cut text ends with: where to read it whole, or
// only that it was cut.
func wakeCutNote(readWhole string) string {
	if readWhole == "" {
		return "(cut by muster to fit one message)"
	}
	return "(cut by muster to fit one message: read it whole with " + readWhole + ")"
}

// fenced is "muster: " + preface, then the owner's words under head, fenced
// with marker. Line breaks of every kind (\r\n, a lone \r, \n) are one break,
// so a form's \r\n never leaves a replacement character on each line. When
// the whole would not fit one paste (mirror.MaxPaste), the words are cut, by
// whole runes, and end with wakeCutNote(readWhole) — never dropped.
func fenced(preface, marker, head, text, readWhole string) string {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	render := func(t string, cut bool) string {
		lines := []string{head}
		for _, l := range strings.Split(t, "\n") {
			lines = append(lines, "  "+fence.Sanitize(l))
		}
		if cut {
			lines = append(lines, wakeCutNote(readWhole))
		}
		return fence.Block{Preface: "muster: " + preface, Marker: marker, Count: 1, Lines: lines}.Render(fence.NewNonce())
	}
	out := render(text, false)
	if len(out) <= mirror.MaxPaste {
		return out
	}
	r := []rune(text)
	for len(r) > 0 {
		over := len(out) - mirror.MaxPaste
		// Drop at least the overflow's worth of runes (each at least a byte),
		// then measure again: a sanitised rune can grow, never shrink.
		n := max(over/4, 1)
		if n > len(r) {
			n = len(r)
		}
		r = r[:len(r)-n]
		if out = render(string(r), true); len(out) <= mirror.MaxPaste {
			return out
		}
	}
	return render("", true)
}

// ---------------------------------------------------------------------------
// Filing.
// ---------------------------------------------------------------------------

// fileWake records wake n for owner and starts its delivery. A team owner, a
// server without wakes, or a store error files nothing (logged).
func (s *Server) fileWake(ctx context.Context, owner int64, n sessionwake.New) {
	if !s.wakesOn() || owner <= 0 {
		return
	}
	octx := s.queuedPrincipal(context.WithoutCancel(ctx), owner)
	if team, err := s.sendQueueOwnerIsTeam(octx, owner); err != nil || team {
		return
	}
	if !s.wakeHostHere(octx, n.Host) {
		return
	}
	// The sign-in that decided it (its freshness is judged when the line is
	// queued: wakeNeedsSignIn, the one rule).
	n.StepUpAt = wakeDecisionSignIn(ctx)
	w, fresh, err := s.ext.SessionWakes.Create(octx, n)
	if err != nil {
		s.logger.Printf("session wake: %s for owner %d: %v", n.Kind, owner, err)
		return
	}
	if fresh {
		s.wakeChanged(w)
		s.startWakeDelivery(owner, w.ID)
	}
}

// wakeHostHere reports whether host mirrors its sessions to THIS muster: it
// holds one of the owner's unrevoked mirror tokens. A session mirrored to
// another muster (its calls reach here as a remote caller, mirror_mcp.go) has
// no window here, so a wake for it could never arrive: none is filed, and its
// card keeps the old wording. A host list that cannot be read files none.
func (s *Server) wakeHostHere(ctx context.Context, host string) bool {
	hosts, err := s.ext.Mirror.LiveTokenHosts(ctx)
	if err != nil {
		return false
	}
	return slices.Contains(hosts, host)
}

// wakeDecisionSignIn is the sign-in a decision was made with, as the ONE
// freshness rule (wakeNeedsSignIn) judges it: the request's browser sign-in
// (stepup.AuthTime), or — for an action the paired phone's step-up key signed
// and the server verified (withVerifiedDeviceAction, set only after the
// signature checked out) — the moment of that verification, the later of
// the two. Zero for neither: a phone request with no verified signature, a
// session's MCP call. The door a principal came through proves nothing here.
func wakeDecisionSignIn(ctx context.Context) time.Time {
	at, _ := stepup.AuthTime(ctx)
	if dv, ok := verifiedDeviceActionAt(ctx); ok && dv.After(at) {
		at = dv
	}
	return at
}

// wakeAnswered files the wake an answered ask owes (answerRecorded).
func (s *Server) wakeAnswered(ctx context.Context, a attention.Ask) {
	if n, ok := wakeForAnswer(a); ok {
		s.fileWake(ctx, a.Owner, n)
	}
}

// wakeDismissed files the wake an ask the OWNER dismissed owes: called only
// from the owner's own Dismiss doors (the Asks page, the phone), never from
// muster withdrawing an ask itself or the asker cancelling its own.
func (s *Server) wakeDismissed(ctx context.Context, a attention.Ask) {
	if n, ok := wakeForDismiss(a); ok {
		s.fileWake(ctx, a.Owner, n)
	}
	if a.Status == attention.Cancelled {
		// A step-up card a session's call raised (session_wake_requests.go).
		s.wakeStepUpDecided(ctx, a, sessionwake.DecisionDismissed)
	}
}

// draftRoute is draft d's route, ok false for a draft no route made.
func (s *Server) draftRoute(ctx context.Context, d plugins.Draft) (plugins.Route, bool) {
	if d.RouteID == nil || *d.RouteID == "" || s.ext.Routes == nil {
		return plugins.Route{}, false
	}
	r, err := s.ext.Routes.Route(ctx, *d.RouteID)
	if err != nil {
		return plugins.Route{}, false
	}
	return r, true
}

// wakeDraftDecided files the wake the owner's decision on draft d owes
// (decideDraftAs).
func (s *Server) wakeDraftDecided(ctx context.Context, d plugins.Draft, via plugins.DraftDecisionSource) {
	if !s.wakesOn() {
		return
	}
	// The step-up cards about d that this decision settled, and an officer's
	// draft for an item a session enqueued (session_wake_requests.go).
	s.wakeStepUpsSettledBy(ctx, d, via.Via)
	octx := s.queuedPrincipal(context.WithoutCancel(ctx), d.OwnerID)
	r, ok := s.draftRoute(octx, d)
	if !ok {
		s.wakeOfficerDraftDecided(ctx, d, via)
		return
	}
	if n, ok := wakeForDraftDecision(d, r); ok {
		if via.Via == plugins.DraftViaMCP {
			// Decided through plugin_draft_decide from a mirrored session:
			// the line says where (renderWake), never "you approved".
			n.ViaHost, n.ViaSession = via.Host, via.Session
		}
		s.fileWake(ctx, d.OwnerID, n)
	}
}

// wakeDraftOutcome files the wake approved draft d's outcome owes, once — only
// for a draft whose approval the owner decided (its decided wake exists), so
// an approval muster made itself (an allowlisted sender) owes no outcome line.
func (s *Server) wakeDraftOutcome(ctx context.Context, d plugins.Draft) {
	if !s.wakesOn() || d.OwnerID <= 0 {
		return
	}
	octx := s.queuedPrincipal(context.WithoutCancel(ctx), d.OwnerID)
	decided, err := s.ext.SessionWakes.ForDrafts(octx, []int64{d.ID})
	if err != nil {
		return
	}
	w, ok := decided[d.ID]
	if !ok || w.Kind != sessionwake.KindDraftDecided || w.Decision != sessionwake.DecisionApproved {
		return
	}
	r, ok := s.draftRoute(octx, d)
	if !ok {
		return
	}
	if n, ok := wakeForDraftOutcome(d, r); ok {
		// Where the approval was made is the outcome line's too.
		n.ViaHost, n.ViaSession = w.ViaHost, w.ViaSession
		s.fileWake(ctx, d.OwnerID, n)
	}
}

// wakePageResponse files what response r on page id owes the session that
// made the page: the owner's own response, fenced, or one more in the page's
// count line (recordPageResponse).
func (s *Server) wakePageResponse(ctx context.Context, owner, id int64, r pages.Response) {
	if !s.wakesOn() || s.ext.Pages == nil || owner <= 0 {
		return
	}
	octx := s.queuedPrincipal(context.WithoutCancel(ctx), owner)
	pg, err := s.ext.Pages.Page(octx, id)
	if err != nil || pg.OwnerID != owner {
		return
	}
	n, own, ok := pageWake(pg, r)
	if !ok {
		return
	}
	if own {
		s.fileWake(ctx, owner, n)
		return
	}
	if team, err := s.sendQueueOwnerIsTeam(octx, owner); err != nil || team {
		return
	}
	if !s.wakeHostHere(octx, n.Host) {
		return
	}
	w, err := s.ext.SessionWakes.AddPageResponses(octx, pg.ID, n.Host, n.Session, 1)
	if err != nil {
		s.logger.Printf("session wake: page %d responses: %v", pg.ID, err)
		return
	}
	// Not delivered at once: the sweep hands it over, so a burst of responses
	// folds into one line.
	s.wakeChanged(w)
}

// ---------------------------------------------------------------------------
// Delivery.
// ---------------------------------------------------------------------------

// startWakeDelivery delivers wake id in the background, registered so a
// shutdown (and a test) waits for it.
func (s *Server) startWakeDelivery(owner, id int64) {
	ctx, cancel := s.streamContext(context.Background())
	s.attention.deliveries.Add(1)
	safeGo(s.logger, "session wake delivery", func() {
		defer s.attention.deliveries.Done()
		defer cancel()
		s.deliverWake(ctx, owner, id)
	})
}

// Why a wake waits (sessionwake Hold's detail), as the owner reads it after
// "Waiting: ".
const (
	wakeWaitNoWindow = "no window shows the session now"
	wakeWaitOffline  = "the session's host is offline"
	wakeWaitReadOnly = "the session's host takes no messages from muster (it runs without -interactive)"
	wakeWaitNoStatus = "the session's host cannot tell muster whether Claude Code is busy"
	wakeWaitQueue    = "it could not be queued"
)

// wakeWindow is the window of w's host that runs w's Claude Code session now,
// or why there is none. Only the owner's own hosts (ctx is the owner's).
func (s *Server) wakeWindow(ctx context.Context, w sessionwake.Wake) (mirror.SessionView, string) {
	for _, h := range s.ext.Mirror.Hosts(ctx) {
		if h.Host != w.Host {
			continue
		}
		if !h.Online {
			return mirror.SessionView{}, wakeWaitOffline
		}
		for _, e := range h.Entries() {
			if !e.Claude || e.ClaudeSession != w.Session {
				continue
			}
			switch {
			case !h.Interactive:
				return mirror.SessionView{}, wakeWaitReadOnly
			case !h.StatusReport:
				return mirror.SessionView{}, wakeWaitNoStatus
			}
			return e, ""
		}
		return mirror.SessionView{}, wakeWaitNoWindow
	}
	return mirror.SessionView{}, wakeWaitOffline
}

// Delivery results (deliverWake's answer; read by tests).
const (
	wakeHanded     = "handed"
	wakeWaiting    = "waiting"
	wakeSettled    = "settled"
	wakeNothing    = "nothing"
	wakeGaveUp     = "gave_up"
	wakeInTheQueue = "in_queue"
)

// deliverWake moves wake id one step, as owner: a queued wake is handed to
// its window's send queue (or held, saying why, or given up after a day); a
// delivering one whose queue row finished somewhere else (the dispatcher's
// recovery, an expiry, a Cancel in the queue list) is settled from that row.
func (s *Server) deliverWake(parent context.Context, owner, id int64) string {
	if !s.wakesOn() {
		return wakeNothing
	}
	ctx := s.queuedPrincipal(parent, owner)
	w, err := s.ext.SessionWakes.Get(ctx, id)
	if err != nil {
		return wakeNothing
	}
	switch w.State {
	case sessionwake.Queued:
		return s.handWake(ctx, w)
	case sessionwake.Delivering:
		return s.settleWakeRow(ctx, w)
	}
	return wakeNothing
}

// wakeCarriesOwnerText reports whether w's line carries the owner's own words
// (an answer, a rejection's reason, their own page response): THE predicate
// for which lines need the owner's fresh sign-in. A dismissal, an approval
// without a reason, an outcome and a count are muster's sentence alone.
func wakeCarriesOwnerText(w sessionwake.Wake) bool { return strings.TrimSpace(w.OperatorText) != "" }

// wakeNeedsSignIn reports whether w may not be queued now for want of a fresh
// sign-in: it carries the owner's words and the sign-in that decided it (or
// their later Send to session) is none or older than the step-up window — the
// send queue's rule for the owner's own message (sendQueueAuthorization's
// "fresh when queued"), measured at the moment it would be queued.
func wakeNeedsSignIn(w sessionwake.Wake, now time.Time) bool {
	return wakeCarriesOwnerText(w) && (w.StepUpAt.IsZero() || now.Sub(w.StepUpAt) > stepup.Window())
}

// handWake hands queued wake w to its window's send queue.
func (s *Server) handWake(ctx context.Context, w sessionwake.Wake) string {
	st := s.ext.SessionWakes
	if s.sendQueueNow().Sub(w.QueuedAt) >= wakeGiveUp || w.Attempts >= wakeMaxAttempts {
		why := "Not delivered: no window showed the session within a day"
		if w.Detail != "" {
			why += " (" + w.Detail + ")"
		}
		if w.Attempts >= wakeMaxAttempts {
			why = "Not delivered: the session's window refused it " + strconv.Itoa(w.Attempts) + " times"
			if w.Detail != "" {
				why += " (last: " + w.Detail + ")"
			}
		}
		if ok, err := st.GiveUp(ctx, w.ID, why); err == nil && ok {
			w.State = sessionwake.Unconfirmed
			s.wakeChanged(w)
		}
		return wakeGaveUp
	}
	if wakeNeedsSignIn(w, s.sendQueueNow()) {
		// 🔴 THE OWNER'S OWN WORDS NEED THE OWNER'S FRESH SIGN-IN, as their own
		// queued message does: held until their "Send to session" from a fresh
		// sign-in records one (retryWake). Nothing is queued meanwhile.
		if w.Detail != sessionwake.HeldForSignIn {
			if err := st.Hold(ctx, w.ID, sessionwake.HeldForSignIn); err == nil {
				s.wakeChanged(w)
			}
		}
		return wakeWaiting
	}
	win, why := s.wakeWindow(ctx, w)
	if why != "" {
		if why != w.Detail {
			if err := st.Hold(ctx, w.ID, why); err == nil {
				s.wakeChanged(w)
			}
		}
		return wakeWaiting
	}
	target := sendqueue.Target{Host: w.Host, Session: win.Session, Window: win.Window}
	as := s.ext.Mirror.AgentState(ctx, w.Host, win.Session, win.Window)
	// 🔴 A ROW OF ITS OWN ORIGIN, PINNED TO THE SESSION: gated on the wake at
	// the paste (sendQueueWakeGate), refused by the host if the window no
	// longer holds w.Session (AgentSession), never edited or retried as a
	// person's. Its sign-in time is the DECIDING request's for a line that
	// carries the owner's words (checked again at the paste, the send queue's
	// own rule: sendQueueAuthorization), and the server's for one that carries
	// none (muster's sentence alone: nothing of the owner's to authorise).
	stepUpAt := s.sendQueueNow()
	if wakeCarriesOwnerText(w) {
		stepUpAt = w.StepUpAt
	}
	it, err := s.ext.SendQueue.Enqueue(ctx, sendqueue.Item{Target: target, Body: renderWake(w), StepUpAt: stepUpAt,
		AfterSince: as.Pasted, AgentSession: w.Session, SessionWakeID: w.ID})
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			// Another delivery of this wake queued its row this instant: it
			// hands the wake over (or its row is refused at the paste, the
			// wake not being its, and the next pass queues one again).
			return wakeInTheQueue
		}
		detail := wakeWaitQueue + ": " + unprefixed(err)
		if detail != w.Detail {
			_ = st.Hold(ctx, w.ID, detail)
			s.wakeChanged(w)
		}
		return wakeWaiting
	}
	ok, err := st.Hand(ctx, w.ID, it.ID)
	if err != nil || !ok {
		// The wake moved on meanwhile (the session read it while waiting): the
		// row is not its, so it goes now (and the gate would refuse it anyway).
		if _, cerr := s.ext.SendQueue.Cancel(ctx, it.ID, s.sendQueueNow()); cerr != nil && !errors.Is(cerr, sendqueue.ErrNotCancellable) {
			s.logger.Printf("session wake %d: cancel its unused queue row %d: %v", w.ID, it.ID, cerr)
		}
		return wakeNothing
	}
	w.State, w.SendQueueID = sessionwake.Delivering, it.ID
	s.wakeChanged(w)
	s.kickSendQueue()
	return wakeHanded
}

// settleWakeRow settles delivering wake w from its queue row, when the row
// finished without the paste recording it (the dispatcher's recovery, an
// expiry, a Cancel in the queue list) — and gives up on a row a window kept
// busy for a day.
func (s *Server) settleWakeRow(ctx context.Context, w sessionwake.Wake) string {
	it, err := s.ext.SendQueue.Get(ctx, w.SendQueueID)
	if err != nil {
		return wakeNothing
	}
	st := s.ext.SessionWakes
	moved := false
	switch it.State {
	case sendqueue.Sent:
		moved, _ = st.Finish(ctx, w.ID, it.ID, true, "")
	case sendqueue.Failed:
		if it.MayHaveDelivered {
			moved, _ = st.Finish(ctx, w.ID, it.ID, false, it.Detail)
		} else {
			moved, _ = st.Requeue(ctx, w.ID, it.ID, it.Detail)
		}
	case sendqueue.Cancelled:
		moved, _ = st.Finish(ctx, w.ID, it.ID, false, "Not delivered: it was cancelled in the session's queue")
	case sendqueue.Queued:
		if s.sendQueueNow().Sub(w.QueuedAt) < wakeGiveUp {
			return wakeNothing
		}
		if _, cerr := s.ext.SendQueue.Cancel(ctx, it.ID, s.sendQueueNow()); cerr != nil {
			return wakeNothing // being sent this instant: the paste settles it
		}
		moved, _ = st.Finish(ctx, w.ID, it.ID, false, "Not delivered: the session was not idle within a day")
	default:
		return wakeNothing
	}
	if moved {
		s.wakeChanged(w)
		return wakeSettled
	}
	return wakeNothing
}

// deliverDueWakes is one wake sweep (the attention sweep's): every owner's
// owed and in-flight wakes, each moved one step as its owner.
func (s *Server) deliverDueWakes(ctx context.Context) {
	if !s.wakesOn() {
		return
	}
	due, err := s.ext.SessionWakes.Due(ctx, wakeBatch)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.logger.Printf("session wakes: owed wakes: %v", err)
		}
		return
	}
	for _, w := range due {
		s.deliverWake(ctx, w.Owner, w.ID)
	}
}

// wakeDraftOutcomesDue files the outcome wake every approved draft that has
// settled since owes its session: the ONE writer of an outcome line, whichever
// way the draft settled (the plugin's result, a lease that lapsed). Its delay
// is the attention sweep's (30 s). A reply draft is never retried (only an
// officer's outbound draft has a retry chain, plugins.RetryDraft and FinishDraft
// refuse a reply draft one), so the draft that settles is the decided one.
func (s *Server) wakeDraftOutcomesDue(ctx context.Context) {
	if !s.wakesOn() || s.ext.Plugins == nil {
		return
	}
	// And an officer's drafts for items a session enqueued
	// (session_wake_requests.go).
	s.wakeOfficerOutcomesDue(ctx)
	owed, err := s.ext.SessionWakes.DraftOutcomesOwed(ctx, wakeBatch)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			s.logger.Printf("session wakes: draft outcomes owed: %v", err)
		}
		return
	}
	for _, o := range owed {
		octx := s.queuedPrincipal(ctx, o.Owner)
		d, err := s.ext.Plugins.Draft(octx, o.DraftID)
		if err != nil {
			continue
		}
		s.wakeDraftOutcome(octx, d)
	}
}

// --- the send queue's gate and outcome --------------------------------------------

// Why a wake's row is not pasted (sendQueueWakeGate).
const (
	SendQueueWakeOff     = "Not sent: session wakes are off on this server."
	SendQueueWakeClosed  = "Not sent: muster's line is no longer waiting to be sent"
	SendQueueWakeWaited  = "Not sent: the session already read it while waiting for it."
	SendQueueWakeUnknown = "Not sent: muster's line could not be read."
)

// sendQueueWakeGate is why wake row it may not be pasted now, "" when it may:
// its wake still delivering on THIS row and — an ask's — the session not
// having read the ask itself. A wake that cannot be read is SendQueueNudgeWait:
// the row goes back to queued for a later pass. ctx is the row's owner.
func (s *Server) sendQueueWakeGate(ctx context.Context, it sendqueue.Item) string {
	if s.ext.SessionWakes == nil {
		return SendQueueWakeOff
	}
	w, err := s.ext.SessionWakes.Get(ctx, it.SessionWakeID)
	switch {
	case errors.Is(err, sessionwake.ErrNotFound):
		return SendQueueWakeUnknown
	case err != nil:
		return SendQueueNudgeWait
	}
	if w.State == sessionwake.Waited {
		return SendQueueWakeWaited
	}
	if w.State != sessionwake.Delivering || w.SendQueueID != it.ID {
		return SendQueueWakeClosed + " (" + string(w.State) + ")."
	}
	if wakeCarriesOwnerText(w) {
		// The owner's words: the row's sign-in (the deciding request's) must
		// have been fresh when it was queued, as for their own message.
		if why := sendQueueAuthorization(it, s.sendQueueNow()); why != "" {
			return why
		}
	}
	if w.AskID > 0 && s.ext.Attention != nil {
		// 🔴 THE ASK'S OWN RECORD, READ AGAIN AT THE PASTE: attention_wait
		// marks it waited as it returns the answer (waitResult), whether or
		// not its MarkWaited of the wake landed.
		a, err := s.ext.Attention.Get(ctx, w.AskID)
		if err != nil {
			return SendQueueNudgeWait
		}
		if a.Delivery == attention.DeliveryWaited {
			if _, err := s.ext.SessionWakes.MarkWaited(ctx, a.ID); err == nil {
				s.wakeChanged(w)
			}
			return SendQueueWakeWaited
		}
	}
	return ""
}

// wakeOutcome records what the paste of wake row it came to (sendQueueDeliver,
// every return): sent → delivered; a paste that may have landed (the host did
// not confirm it, or it sits unsent in Claude's input box) → unconfirmed, never
// resent; one that provably reached nothing → queued again for the next pass;
// a superseded one stays with its row, which goes back to the queue.
// Conditional on the wake still delivering on this row, so a gate refusal that
// already settled it changes nothing.
func (s *Server) wakeOutcome(ctx context.Context, it sendqueue.Item, o sendqueue.Outcome) {
	if s.ext.SessionWakes == nil || o.Superseded {
		return
	}
	octx := s.queuedPrincipal(context.WithoutCancel(ctx), it.Owner)
	st := s.ext.SessionWakes
	var moved bool
	var err error
	switch {
	case o.Sent:
		moved, err = st.Finish(octx, it.SessionWakeID, it.ID, true, "")
	case o.MayHaveDelivered || o.Pasted:
		moved, err = st.Finish(octx, it.SessionWakeID, it.ID, false, o.Detail)
	default:
		moved, err = st.Requeue(octx, it.SessionWakeID, it.ID, o.Detail)
	}
	if err != nil {
		s.logger.Printf("session wake %d: record its paste (row %d): %v", it.SessionWakeID, it.ID, err)
		return
	}
	if moved {
		if w, err := st.Get(octx, it.SessionWakeID); err == nil {
			s.wakeChanged(w)
		}
	}
}

// --- the session reading it itself -------------------------------------------------

// wakeRead records that ask a's session read its answer or dismissal itself
// (attention_wait returned it): its wake is never pasted, and a row already
// queued for it is cancelled.
func (s *Server) wakeRead(ctx context.Context, a attention.Ask) {
	if s.ext.SessionWakes == nil {
		return
	}
	// 🔴 READ, WHETHER OR NOT THE WAKE EXISTS YET: a dismissal's wake is filed
	// after the Dismiss settled the ask (and woke this wait), so a missing one
	// is written as read and the wake filed after it finds it so (MarkRead).
	var moved []sessionwake.Wake
	var err error
	if n, ok := wakeForAnswer(a); ok {
		moved, err = s.ext.SessionWakes.MarkRead(context.WithoutCancel(ctx), n)
	} else if n, ok := wakeForDismiss(a); ok {
		moved, err = s.ext.SessionWakes.MarkRead(context.WithoutCancel(ctx), n)
	} else {
		moved, err = s.ext.SessionWakes.MarkWaited(context.WithoutCancel(ctx), a.ID)
	}
	if err != nil {
		s.logger.Printf("session wake: ask %d: record that its session read it: %v", a.ID, err)
		return
	}
	for _, w := range moved {
		if w.SendQueueID > 0 && s.ext.SendQueue != nil {
			if _, err := s.ext.SendQueue.Cancel(context.WithoutCancel(ctx), w.SendQueueID, s.sendQueueNow()); err != nil &&
				!errors.Is(err, sendqueue.ErrNotCancellable) {
				s.logger.Printf("session wake %d: cancel its queue row %d: %v", w.ID, w.SendQueueID, err)
			}
		}
		s.wakeChanged(w)
	}
}

// wakeChanged redraws the owner's pages that show w (the Asks page's cards).
func (s *Server) wakeChanged(w sessionwake.Wake) {
	switch {
	case w.AskID > 0:
		s.attentionChanged(w.Owner, attentionAskKey(w.AskID))
	case w.DraftID > 0:
		s.attentionChanged(w.Owner, draftKey(w.DraftID))
	}
}

// --- what the owner reads ----------------------------------------------------------

// wakeAction is what a wake's line offers the owner: Label "" for nothing,
// else a button posting to sessionWakeRetryPath — "Retry delivery" for a line
// that did not arrive, "Send to the session" for one held for a fresh sign-in.
// StepUp: the post needs the owner's fresh sign-in (the line carries their
// words), so its form is a step-up form (data-step-up-form).
type wakeAction struct {
	Label  string
	StepUp bool
}

// wakeDeliveryText is where wake w went, as the owner reads it — who is the
// session in words — and what it offers.
func wakeDeliveryText(w sessionwake.Wake, who string) (string, wakeAction) {
	switch w.State {
	case sessionwake.Queued:
		if w.Detail == sessionwake.HeldForSignIn {
			return "Sign in again to send to " + who + ".", wakeAction{Label: "Send to the session", StepUp: true}
		}
		if w.Detail != "" {
			return "Waiting to send it to " + who + ": " + w.Detail + ".", wakeAction{}
		}
		return "Waiting for " + who + " to be idle.", wakeAction{}
	case sessionwake.Delivering:
		return "Waiting for " + who + " to be idle: queued in its window.", wakeAction{}
	case sessionwake.Delivered:
		return "Sent to " + who + ".", wakeAction{}
	case sessionwake.Waited:
		return who + " read it while waiting for it.", wakeAction{}
	case sessionwake.Unconfirmed:
		d := strings.TrimSuffix(firstNonBlank(w.Detail, "Not delivered"), ".")
		if !strings.HasPrefix(d, "Not delivered") && !strings.HasPrefix(d, "May have been sent") {
			d = "Not confirmed: " + d
		}
		return d + ".", wakeAction{Label: "Retry delivery", StepUp: wakeCarriesOwnerText(w)}
	}
	return "", wakeAction{}
}

// wakeActionFormHTML is wake id's action as a form ("" for none), with attrs
// on its button: THE one form the Answered card, the ask page and the draft
// card draw.
func wakeActionFormHTML(id int64, a wakeAction, class, attrs string) string {
	if a.Label == "" {
		return ""
	}
	step := ""
	if a.StepUp {
		step = " data-step-up-form"
	}
	return `<form method="POST" action="` + sessionWakeRetryPath(id) + `"` + step + ` data-asks-controls><button type="submit" class="` + class + `"` + attrs + `>` +
		html.EscapeString(a.Label) + `</button></form>
`
}

// wakeDeliveryPill is wakeDeliveryText in a pill's few words.
func wakeDeliveryPill(w sessionwake.Wake) string {
	switch w.State {
	case sessionwake.Queued, sessionwake.Delivering:
		if w.Detail == sessionwake.HeldForSignIn {
			return "Sign in again to send"
		}
		return "Waiting for the session"
	case sessionwake.Delivered:
		return "Sent to the session"
	case sessionwake.Waited:
		return "Read by the session"
	case sessionwake.Unconfirmed:
		return "Not delivered"
	}
	return ""
}

// wakesForAsks is the wake of each of asks (by ask id), for the cards; empty
// without wakes or on a read error.
func (s *Server) wakesForAsks(ctx context.Context, asks []attention.Ask) map[int64]sessionwake.Wake {
	if s.ext.SessionWakes == nil || len(asks) == 0 {
		return nil
	}
	var ids []int64
	for _, a := range asks {
		if a.Asker.MirrorSession != "" {
			ids = append(ids, a.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	ws, err := s.ext.SessionWakes.ForAsks(ctx, ids)
	if err != nil {
		s.logger.Printf("session wakes: read for asks: %v", err)
		return nil
	}
	return ws
}

// withWakeLines sets each draft card's wake line (display only): where the
// owner's decision on it — or its outcome — went.
func (s *Server) withWakeLines(ctx context.Context, cards []plugins.DraftCard) {
	if s.ext.SessionWakes == nil || len(cards) == 0 {
		return
	}
	var ids []int64
	for _, c := range cards {
		if c.Draft.RouteID != nil && c.Route.Kind == plugins.RouteTargetMirror {
			ids = append(ids, c.Draft.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	ws, err := s.ext.SessionWakes.ForDrafts(ctx, ids)
	if err != nil {
		s.logger.Printf("session wakes: read for drafts: %v", err)
		return
	}
	for i := range cards {
		w, ok := ws[cards[i].Draft.ID]
		if !ok {
			continue
		}
		who := firstNonBlank(strings.TrimSpace(cards[i].Route.Label), "the session that wrote it")
		text, act := wakeDeliveryText(w, who)
		cards[i].WakeLine = text
		if act.Label != "" {
			cards[i].WakeRetryID, cards[i].WakeRetryLabel, cards[i].WakeStepUp = w.ID, act.Label, act.StepUp
		}
	}
}

// --- Retry -------------------------------------------------------------------------

// sessionWakeRetryPath is a wake's Retry (POST, the owner's own wakes only).
func sessionWakeRetryPath(id int64) string {
	return attentionPath + "/wakes/" + strconv.FormatInt(id, 10) + "/retry"
}

// WakeStepUpRequired is what a stale "Send to the session" or Retry delivery
// of a line carrying the owner's words is refused with.
const WakeStepUpRequired = "Confirm it's you first: muster types your own words into a session only with a recent sign-in."

// wakeRetryStale refuses, and reports true, when the request asking to send
// (or retry) wake cur may not: cur carries the owner's words and the request's
// sign-in is not fresh — the one rule the owner's own Send is held to
// (stepup.Fresh). A document navigation gets the Asks page with the dialog's
// marker (the form is a data-step-up-form, so the page's script asks first);
// any other request the plain 403 the script reads.
func (s *Server) wakeRetryStale(w http.ResponseWriter, r *http.Request, cur sessionwake.Wake) bool {
	if !wakeCarriesOwnerText(cur) || stepup.Fresh(r.Context(), s.stepUpNow()) {
		return false
	}
	if isDocumentNavigation(r) {
		w.Header().Set("X-Muster-Step-Up", "required")
		tab := asksTabAnswered
		if cur.DraftID > 0 {
			tab = asksTabDrafts
		}
		s.renderAsksPage(w, r, http.StatusForbidden, WakeStepUpRequired, tab, 0)
		return true
	}
	refuseStale(w)
	return true
}

// retryWake queues the principal's wake id once more — unconfirmed, or held
// for a fresh sign-in — recording the request's sign-in for a line carrying
// the owner's words (the caller checked it is fresh: wakeRetryStale), and
// starts its delivery.
func (s *Server) retryWake(ctx context.Context, cur sessionwake.Wake) (sessionwake.Wake, error) {
	var at time.Time
	if wakeCarriesOwnerText(cur) {
		at, _ = stepup.AuthTime(ctx)
	}
	w, err := s.ext.SessionWakes.Retry(ctx, cur.ID, at)
	if err != nil {
		return w, err
	}
	s.wakeChanged(w)
	s.startWakeDelivery(w.Owner, w.ID)
	return w, nil
}

// handleSessionWakeRetry is POST /attention/wakes/{id}/retry: a wake's Retry
// delivery, or its Send to the session after a fresh sign-in, back to the
// Asks page it was on.
func (s *Server) handleSessionWakeRetry(w http.ResponseWriter, r *http.Request) {
	if s.ext.SessionWakes == nil {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		s.renderAsksPage(w, r, http.StatusNotFound, "No such delivery.", asksTabDrafts, 0)
		return
	}
	cur, err := s.ext.SessionWakes.Get(r.Context(), id)
	if err != nil {
		s.renderAsksPage(w, r, http.StatusNotFound, "No such delivery.", asksTabDrafts, 0)
		return
	}
	if s.wakeRetryStale(w, r, cur) {
		return
	}
	wk, err := s.retryWake(r.Context(), cur)
	switch {
	case err == nil:
	case errors.Is(err, sessionwake.ErrNotFound):
		s.renderAsksPage(w, r, http.StatusNotFound, "No such delivery.", asksTabDrafts, 0)
		return
	case errors.Is(err, sessionwake.ErrNotRetryable):
		s.renderAsksPage(w, r, http.StatusConflict, "That delivery is not waiting for you, so there is nothing to send again.", asksTabDrafts, 0)
		return
	default:
		s.logger.Printf("session wake %d: retry: %v", id, err)
		s.renderAsksPage(w, r, http.StatusInternalServerError, "That could not be recorded.", asksTabDrafts, 0)
		return
	}
	if wk.DraftID > 0 {
		http.Redirect(w, r, draftDecidedLink(wk.DraftID), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, attentionAnsweredPath, http.StatusSeeOther)
}

// wakeStepUpNow is the auth time a wake row's paste is checked against: the
// delivery moment (the wake gates it, not a sign-in).
func (s *Server) wakeStepUpNow(ctx context.Context) context.Context {
	return stepup.WithAuthTime(ctx, s.stepUpNow())
}
