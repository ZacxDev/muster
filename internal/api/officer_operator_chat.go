package api

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/flows"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/stepup"
	"github.com/ZacxDev/muster/internal/ui"
	"github.com/ZacxDev/muster/internal/users"
)

// ---------------------------------------------------------------------------
// CHAT WITH AN OFFICER ON ITS PAGE (officers UX PR 2/4; docs/
// officers-builtin.md "Chat with it")
//
//	POST /officers/{officer}/chat                         a message to it
//	POST /officers/{officer}/chat/cards/{card}/approve    a confirm card's Approve
//	POST /officers/{officer}/chat/cards/{card}/reject     a confirm card's Reject
//
// A builtin pod officer's owner (or a manager of its team) chats with it on
// its page like with any agent. Each person has ONE chat with each officer:
// a chat lane (officerChatLanes, officer_chat.go — the lane HQ chats run in)
// keyed officers.ChatSessionKey(officers.OperatorChatID(person, officer)) on one of
// its instances, so the chat runs in a Claude Code session of its own beside
// the instance's queue work, and its own tool calls carry the lane's secret
// (verifyChatLaneCall): muster knows a call comes from THAT person's chat.
// In it the officer reads and acts on its own state with its own-state tools
// (officer_own_state.go); queue verbs are refused in a lane as for any chat.
//
// 🔴 A CHAT IS NEVER HELD BY THE BUDGET. Nothing here reads the daily budget:
// a held officer, or one past its cap, still answers. Its spend is COUNTED —
// the lane's Claude usage is the instance's (ccd counts every session's
// transcript), and each turn ends by booking the instance's usage into
// officer_llm_usage (usage.Collector.Flush) — never gated on.
//
// 🔴 APPROVALS ARE CONFIRM CARDS, DECIDED BY THE PERSON. officer_confirm_card
// records one; the card here shows the exact text and media, and its Approve
// is this person's signed-in click (the 12-hour sign-in rule, as every
// approval), deciding EXACTLY what the card showed — the draft's (or batch's)
// one decision (decideDraftAs, decideBatchAs) given the card's own SHA-256s,
// which refuses anything else, and an officer caller before anything
// (officerDeciderRefusal).
//
// ⚠ A TURN IN FLIGHT IS THIS REPLICA'S (operatorChats): another replica's
// page shows the person's message once the lane took it, not "working" before.
// ---------------------------------------------------------------------------

// officerChatSectionID is the chat's section on the officer's page.
const officerChatSectionID = "officer-chat"

// Bounds on a chat.
const (
	// operatorChatMaxRunes bounds one message.
	operatorChatMaxRunes = 4000
	// operatorChatShown is how many of the chat's newest messages the page
	// draws.
	operatorChatShown = 60
	// operatorChatCardsShown is how many of its newest cards the page draws.
	operatorChatCardsShown = 20
)

// How long a turn waits for its instance (a start, a CLI coming up, a busy
// lane), and how often it tries. Vars so a test can shorten them.
var (
	operatorChatWait       = 5 * time.Minute
	operatorChatRetryEvery = 3 * time.Second
)

// operatorChats is the operator chat turns in flight on this replica, and the
// last failure of each chat (shown until its next message).
type operatorChats struct {
	mu    sync.Mutex
	turns map[string]*operatorChatTurn
	wg    sync.WaitGroup
}

// operatorChatTurn is one chat's latest turn.
type operatorChatTurn struct {
	text    string
	started time.Time
	running bool
	failed  string
}

// operatorChatKey keys a chat: the officer and the person's chat id.
func operatorChatKey(o officers.Officer, chatID string) string { return o.Key() + "\x00" + chatID }

// begin records a turn of chat key, false while one is already running.
func (c *operatorChats) begin(key, text string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.turns == nil {
		c.turns = map[string]*operatorChatTurn{}
	}
	if t, ok := c.turns[key]; ok && t.running {
		return false
	}
	c.turns[key] = &operatorChatTurn{text: text, started: now, running: true}
	c.wg.Add(1)
	return true
}

// end records chat key's turn over, with why it failed ("" when it did not).
func (c *operatorChats) end(key, failed string) {
	c.mu.Lock()
	if t, ok := c.turns[key]; ok {
		t.running, t.failed = false, failed
	}
	c.mu.Unlock()
	c.wg.Done()
}

// get is chat key's latest turn.
func (c *operatorChats) get(key string) (operatorChatTurn, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	t, ok := c.turns[key]
	if !ok {
		return operatorChatTurn{}, false
	}
	return *t, true
}

// WaitForOfficerChats waits for every operator chat turn in flight (tests,
// and a graceful stop).
func (s *Server) WaitForOfficerChats() { s.officerChats.wg.Wait() }

// operatorChatOfficer is the officer owner's officer id runs as, for a chat:
// its pool that takes chats (the judgement pool of a tiered officer; an
// untiered one is its own), when it is a BUILTIN officer in PODS. ok false
// with the reason in plain words.
func (s *Server) operatorChatOfficer(owner int64, id string) (officers.Officer, string, bool) {
	var o officers.Officer
	found := false
	if e, ok := s.officerRT.envOfficer(owner, id); ok {
		o, found = e, true
	} else if st, ok := s.officerRT.declaredView(owner, id); ok && st.officer != nil {
		o, found = *st.officer, true
	}
	switch {
	case !found:
		return o, "It is not running, so it cannot chat: it has nowhere to run.", false
	case !o.IsBuiltin():
		return o, "Its queue is in its HQ: chat with it there.", false
	case o.OnHost():
		return o, "It runs in a window on one of your machines: chat with it in that window.", false
	}
	return o.Pools()[0], "", true
}

// handleOfficerChat is POST /officers/{officer}/chat {message}: the person's
// message, sent to the officer as a turn of their chat with it (in the
// background: the page shows it working, then its answer).
func (s *Server) handleOfficerChat(w http.ResponseWriter, r *http.Request) {
	sc, id, _, _, ok := s.controlTarget(w, r)
	if !ok {
		return
	}
	o, why, ok := s.operatorChatOfficer(sc.Owner, id)
	if !ok {
		s.renderOfficerDetail(w, r, sc, id, http.StatusConflict, why)
		return
	}
	text := strings.TrimSpace(r.FormValue("message"))
	switch {
	case text == "":
		s.renderOfficerDetail(w, r, sc, id, http.StatusBadRequest, "Write a message first.")
		return
	case utf8.RuneCountInString(text) > operatorChatMaxRunes:
		s.renderOfficerDetail(w, r, sc, id, http.StatusBadRequest, fmt.Sprintf("That message is too long: at most %d characters.", operatorChatMaxRunes))
		return
	}
	uid := authz.MustFrom(r.Context()).UserID
	chatID := officers.OperatorChatID(uid, o.Owner, o.ID)
	key := operatorChatKey(o, chatID)
	if !s.officerChats.begin(key, text, time.Now()) {
		s.renderOfficerDetail(w, r, sc, id, http.StatusConflict, "It is still answering your last message. Send this one once it has.")
		return
	}
	ctx := context.WithoutCancel(r.Context())
	safeGo(s.logger, "officer chat turn "+o.ID, func() { s.runOperatorChat(ctx, o, uid, chatID, text, key) })
	http.Redirect(w, r, withNotice(officerHref(sc, id, ""), "chat-sent")+"#"+officerChatSectionID, http.StatusSeeOther)
}

// operatorChatHeaderEnd ends the first turn's header (operatorChatHeader):
// the page draws what follows it, the person's own words.
const operatorChatHeaderEnd = "\n\n--- Your owner says:\n"

// operatorChatHeader is the first turn of a person's chat with officer o:
// who it talks to, and what it may do here.
func operatorChatHeader(o officers.Officer) string {
	return fmt.Sprintf("[muster] This is a chat on your page in muster with your owner, a person, not queue work. Answer them in plain words, briefly. "+
		"Call %s first if you have not loaded your charter in this session. Read your own state with %s. When they want something of yours approved "+
		"(a draft, your outreach batch), call %s: muster shows them a confirm card with the exact text and media, and only their own click decides it — "+
		"you cannot approve anything, ever. When they ask you to re-file or drop a refusal, use %s or %s: each also only shows them a card to confirm. You take no queue work here: "+
		"never claim, complete or release an item in this chat.",
		officers.ToolBrief, officers.ToolMyState, officers.ToolConfirmCard, officers.ToolRefusalRefile, officers.ToolRefusalDrop) + operatorChatHeaderEnd
}

// runOperatorChat sends text as person uid's next turn in their chat with o:
// on an up instance (instance 1 started when none is up), waiting while its
// CLI comes up or its lane is busy, then books the instance's usage.
//
// 🔴 NEVER THE BUDGET: nothing here reads it (the turn of a held officer, or
// one past its cap, is sent like any other).
func (s *Server) runOperatorChat(ctx context.Context, o officers.Officer, uid int64, chatID, text, key string) {
	failed := ""
	defer func() { s.officerChats.end(key, failed) }()
	defer func() {
		if rec := recover(); rec != nil {
			s.logger.Printf("officers: %s: PANIC in user %d's chat turn: %v", o.ID, uid, rec)
			failed = "Something went wrong sending that. Try again."
		}
	}()
	lanes := officerChatLanes{s}
	n, err := s.operatorChatInstance(ctx, lanes, o, chatID)
	if err != nil {
		s.logger.Printf("officers: %s: user %d's chat: no instance: %v", o.ID, uid, err)
		failed = "It could not be started just now. Try again in a moment."
		return
	}
	deadline := time.Now().Add(operatorChatWait)
	var reply string
	for {
		reply, err = s.operatorChatTurn(ctx, lanes, o, n, chatID, text)
		if err == nil || !errors.Is(err, errOperatorChatRetry) || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-time.After(operatorChatRetryEvery):
			continue
		}
		break
	}
	// 🔴 COUNTED, NEVER GATED: the instance's usage (this lane's turn in it)
	// booked now into officer_llm_usage, as a stop books it.
	if rt := s.officerRT; rt != nil && rt.usage != nil && officers.DeclaredCapabilities(o).Has(officers.CapUsage) {
		rt.usage.Flush(ctx, o, o.InstanceName(n))
	}
	switch {
	case errors.Is(err, errOperatorChatOld):
		failed = "Its software is too old to chat. It can chat once it is updated."
	case err != nil:
		s.logger.Printf("officers: %s: user %d's chat turn on instance %s: %v", o.ID, uid, o.InstanceLabel(n), err)
		failed = "It did not answer just now. Try again in a moment."
	case strings.TrimSpace(reply) == "":
		failed = "It finished without a reply."
	}
}

// Why a chat turn was not sent.
var (
	// errOperatorChatRetry: not yet (its CLI coming up, its lane busy).
	errOperatorChatRetry = errors.New("officers: the chat's instance cannot take the turn yet")
	// errOperatorChatOld: its ccd cannot name a chat's own calls, so a chat
	// would act as the instance's primary: it is never run there.
	errOperatorChatOld = errors.New("officers: the instance's ccd cannot run a chat lane")
)

// operatorChatTurn is one attempt at the turn: the lane's first turn carries
// operatorChatHeader, and a sent turn is marked seen on the lane.
func (s *Server) operatorChatTurn(ctx context.Context, lanes officerChatLanes, o officers.Officer, n int, chatID, text string) (string, error) {
	ok, err := lanes.Supports(ctx, o, n)
	switch {
	case err != nil:
		return "", fmt.Errorf("%w: %v", errOperatorChatRetry, err)
	case !ok:
		return "", errOperatorChatOld
	}
	marks, err := lanes.Open(ctx, o, n, chatID)
	if err != nil {
		return "", err
	}
	send := text
	if marks.Seen == 0 {
		send = operatorChatHeader(o) + text
	}
	reply, err := lanes.Turn(ctx, o, n, chatID, send)
	if errors.Is(err, officers.ErrChatLaneUnavailable) {
		return "", fmt.Errorf("%w: %v", errOperatorChatRetry, err)
	}
	if err != nil {
		return reply, err
	}
	marks.Seen++
	marks.Answered = marks.Seen
	if merr := lanes.Mark(ctx, o, n, chatID, marks); merr != nil {
		s.logger.Printf("officers: %s: mark chat %s seen: %v", o.ID, chatID, merr)
	}
	return reply, nil
}

// operatorChatInstance is the instance chat chatID's turn of o goes to:
//
//   - its HOME (officerChatLanes.Home: the instance whose agent holds its
//     session) when that is up;
//   - else, when another instance is up, that one — the chat RE-HOMED there
//     first (officerChatLanes.Rehome: its session on the old home set aside,
//     so its key is free for the new one; chat_sessions.session_key is
//     unique across every agent);
//   - else its home (or instance 1 for a chat with none), started now as the
//     wake loop starts one (its kickoff read now), the session kept.
func (s *Server) operatorChatInstance(ctx context.Context, lanes officerChatLanes, o officers.Officer, chatID string) (int, error) {
	fleet := officerFleet{s: s}
	states, err := fleet.Instances(ctx, o)
	if err != nil {
		return 0, err
	}
	home, err := lanes.Home(ctx, o, chatID)
	if err != nil {
		return 0, fmt.Errorf("its chat's home: %w", err)
	}
	up := map[int]bool{}
	first := 0
	for _, in := range states {
		if in.Up && !in.Error {
			up[in.N] = true
			if first == 0 {
				first = in.N
			}
		}
	}
	switch {
	case home > 0 && up[home]:
		return home, nil
	case first > 0:
		if home > 0 {
			if err := lanes.Rehome(ctx, o, chatID, home); err != nil {
				return 0, fmt.Errorf("re-home its chat from instance %s: %w", o.InstanceLabel(home), err)
			}
			s.logger.Printf("officers: %s: chat %s moved from instance %s (not running) to %s", o.ID, chatID, o.InstanceLabel(home), o.InstanceLabel(first))
		}
		return first, nil
	}
	n := home
	if n == 0 || n > o.MaxInstances {
		n = 1
	}
	if home > 0 && n != home {
		if err := lanes.Rehome(ctx, o, chatID, home); err != nil {
			return 0, fmt.Errorf("re-home its chat from instance %s: %w", o.InstanceLabel(home), err)
		}
	}
	if err := fleet.Start(ctx, o, n, s.officerKickoff(ctx, o, n)); err != nil {
		return 0, err
	}
	return n, nil
}

// --- the page ----------------------------------------------------------------------

// chatEntry is one line of the chat as the page draws it: a message or a card.
type chatEntry struct {
	at   time.Time
	html string
}

// officerChatHTML is the chat section of officer v's page for the viewer, ""
// when they cannot chat with it (not a manager of it, or it is not a builtin
// pod officer).
func (s *Server) officerChatHTML(r *http.Request, sc officerScope, v declaredOfficerView) string {
	esc := html.EscapeString
	ctx := r.Context()
	if !sc.Manage || v.Backend != officers.BackendBuiltin || s.officerRT == nil {
		return ""
	}
	name := v.Name
	if name == "" {
		name = v.ID
	}
	var b strings.Builder
	b.WriteString(`<section class="` + adminCard + `" id="` + officerChatSectionID + `"><h2 class="text-base font-semibold text-fg">Chat with it</h2>`)
	o, why, ok := s.operatorChatOfficer(sc.Owner, v.ID)
	if !ok {
		b.WriteString(`<p class="text-sm text-fg2" data-chat-unavailable>` + esc(why) + `</p></section>`)
		return b.String()
	}
	uid := authz.MustFrom(ctx).UserID
	chatID := officers.OperatorChatID(uid, o.Owner, o.ID)
	entries := s.chatMessageEntries(ctx, o, chatID, name)
	cards, err := s.officerStore.ChatCards(ctx, sc.Owner, v.ID, operatorChatCardsShown)
	if err != nil {
		s.logger.Printf("officers: read user %d's confirm cards with %s: %v", uid, v.ID, err)
	}
	for _, c := range cards {
		entries = append(entries, chatEntry{at: c.CreatedAt, html: s.chatCardHTML(ctx, sc, c)})
	}
	sortChatEntries(entries)
	if len(entries) == 0 {
		b.WriteString(`<p class="text-sm text-fg2" data-chat-empty>Ask it what it is doing, what waits on you, or to get something ready for you to approve. It answers here, and it can read and act on its own work.</p>`)
	} else {
		b.WriteString(`<ol class="flex flex-col gap-2" data-chat-log>`)
		for _, e := range entries {
			b.WriteString(e.html)
		}
		b.WriteString(`</ol>`)
	}
	if t, ok := s.officerChats.get(operatorChatKey(o, chatID)); ok {
		switch {
		case t.running:
			b.WriteString(`<p class="text-sm text-fg2" role="status" data-chat-working>Answering… (reload to see its reply)</p>`)
		case t.failed != "":
			b.WriteString(`<p class="text-sm text-st-error-fg" role="status" data-chat-failed>` + esc(t.failed) + `</p>`)
		}
	}
	b.WriteString(`<form method="POST" action="` + esc(officersPagePath+"/"+url.PathEscape(v.ID)+"/chat") + `" class="flex flex-col gap-2" data-chat-form>` + sc.ownerField() + `
<label for="officer-chat-message" class="sr-only">Message to ` + esc(name) + `</label>
<textarea id="officer-chat-message" name="message" rows="3" maxlength="` + strconv.Itoa(operatorChatMaxRunes) + `" class="` + adminInput + `" placeholder="Message ` + esc(name) + `"></textarea>
<button type="submit" class="` + adminButton + `">Send</button></form></section>`)
	return b.String()
}

// chatMessageEntries is the chat's newest messages as entries: the person's
// words (the first turn's header cut off) and the officer's replies.
func (s *Server) chatMessageEntries(ctx context.Context, o officers.Officer, chatID, name string) []chatEntry {
	esc := html.EscapeString
	st, ok := s.ext.Agents.(agents.OfficerChatStore)
	if !ok {
		return nil
	}
	octx := s.officerPrincipal(ctx, o.Owner)
	sess, err := st.SessionByKeyAnyAgent(octx, officers.ChatSessionKey(chatID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		s.logger.Printf("officers: %s: read chat %s: %v", o.ID, chatID, err)
		return nil
	}
	// 🔴 ONLY A SESSION ON ONE OF o's OWN INSTANCES IS ITS CHAT.
	a, err := s.ext.Agents.Get(octx, sess.AgentID)
	if err != nil {
		return nil
	}
	if oo, _, ok := s.officerOf(a); !ok || oo.Owner != o.Owner || oo.ID != o.ID {
		return nil
	}
	msgs, err := s.ext.Agents.ListRecentChatMessages(octx, sess.ID, operatorChatShown)
	if err != nil {
		s.logger.Printf("officers: %s: read chat %s's messages: %v", o.ID, chatID, err)
		return nil
	}
	var out []chatEntry
	for _, m := range msgs {
		if m.Kind != "" && m.Kind != "text" {
			continue
		}
		text := m.Content
		who, cls, mark := name, "self-start bg-s2", "assistant"
		if m.Role == "user" {
			if _, after, found := strings.Cut(text, operatorChatHeaderEnd); found {
				text = after
			}
			who, cls, mark = "You", "self-end bg-accent/15", "user"
		} else if m.Role != "assistant" {
			continue
		}
		out = append(out, chatEntry{at: m.CreatedAt, html: `<li class="flex max-w-[90%] flex-col gap-1 rounded-xl px-3 py-2 ` + cls + `" data-chat-message="` + mark + `">` +
			`<span class="text-xs font-semibold text-fg2">` + esc(who) + `</span>` +
			`<p class="whitespace-pre-wrap break-words text-sm text-fg">` + esc(flows.VisibleEscape(text)) + `</p></li>`})
	}
	return out
}

// sortChatEntries orders entries oldest first (stable: a message and a card
// of the same instant keep their order).
func sortChatEntries(es []chatEntry) {
	for i := 1; i < len(es); i++ {
		for j := i; j > 0 && es[j].at.Before(es[j-1].at); j-- {
			es[j], es[j-1] = es[j-1], es[j]
		}
	}
}

// chatCardHTML draws confirm card c: what it is for, the exact text and media
// (or the batch's entries), and — undecided, and still exactly what it
// showed — Approve and Reject.
func (s *Server) chatCardHTML(ctx context.Context, sc officerScope, c builtin.ChatCard) string {
	esc := html.EscapeString
	var b strings.Builder
	b.WriteString(`<li class="flex flex-col gap-2 rounded-xl bg-bg px-3 py-3 ring-1 ring-inset ring-edge" data-chat-card="` + strconv.FormatInt(c.ID, 10) +
		`" data-chat-card-kind="` + esc(c.Kind) + `">`)
	base := officersPagePath + "/" + url.PathEscape(c.Officer) + "/chat/cards/" + strconv.FormatInt(c.ID, 10)
	live, state := false, ""
	switch c.Kind {
	case builtin.CardDraft:
		b.WriteString(`<span class="text-xs font-semibold text-fg2">It asks you to approve this</span>`)
		card, err := s.ext.Plugins.DraftCard(ctx, c.DraftID)
		if err != nil {
			state = "It is no longer waiting on you."
			break
		}
		d := card.Draft
		// The draft as the Asks page's draft card draws it: what it answers
		// (draftAnswersHTML: the outside message quoted and marked untrusted,
		// or that muster holds no such message), the post's preview
		// (stepUpPostHTML over draftPostCard) and its Details.
		b.WriteString(draftAnswersHTML(card) + `<div class="flex min-w-0 flex-col gap-2" data-chat-card-body>` + stepUpPostHTML(draftPostCard(card), false) + stepUpDetailsHTML(draftDetailsHTML(card)) + `</div>`)
		switch {
		case plugins.DraftBodySHA256(d.Body) != c.BodySHA256 || strings.Join(d.MediaSHA256(), ",") != strings.Join(c.MediaSHA256, ","):
			state = "It changed since this card was drawn, so it cannot be approved here: ask it for a new card."
		case d.Status != plugins.DraftPending:
			state = "It is no longer waiting on you."
		case len(d.MissingMedia()) > 0:
			state = "Not every image or video is uploaded yet, so it cannot be approved yet."
		default:
			live = true
		}
	case builtin.CardBatch:
		b.WriteString(`<span class="text-xs font-semibold text-fg2">It asks you to approve today's follows and likes</span>`)
		ps, ok := s.ext.Plugins.(*plugins.PGStore)
		if !ok {
			state = "It is no longer waiting on you."
			break
		}
		bt, err := ps.OutreachBatch(ctx, c.BatchID)
		if err != nil {
			state = "It is no longer waiting on you."
			break
		}
		b.WriteString(`<ul class="flex flex-col gap-1" data-chat-card-entries>`)
		for _, e := range s.readOutreachBatch(ctx, bt) {
			d := e.card.Draft
			b.WriteString(`<li class="text-sm text-fg">` + esc(capFirst(d.Target().Kind)) + ` ` + esc(flows.VisibleEscape(entryPerson(e.card))) + ` · ` + esc(venueWords(d.VenueText())) + `</li>`)
		}
		b.WriteString(`</ul>`)
		if bt.SHA256 != c.BatchSHA256 {
			state = "It changed since this card was drawn, so it cannot be approved here: ask it for a new card."
		} else {
			live = true
		}
	case builtin.CardRefile, builtin.CardDrop:
		var rf *builtin.Refusal
		if list, err := s.officerStore.RefusalsAwaitingOwner(ctx, c.OfficerOwner, c.Officer, s.officerNow().Add(-builtin.RefusalWindow)); err == nil {
			for i := range list {
				if list[i].Item == c.RefusalItem {
					rf = &list[i]
				}
			}
		}
		what := "a refused post"
		if rf != nil {
			what = strings.ToLower(refusalWhat(rf.TargetKind)[:1]) + refusalWhat(rf.TargetKind)[1:] + " that was refused"
		}
		if c.Kind == builtin.CardRefile && c.RefileAt != nil {
			b.WriteString(`<span class="text-xs font-semibold text-fg2" data-chat-card-refusal>It asks to file ` + esc(what) + ` again at ` + esc(fmtTime(*c.RefileAt)) + `</span>`)
		} else {
			b.WriteString(`<span class="text-xs font-semibold text-fg2" data-chat-card-refusal>It asks to drop ` + esc(what) + `, never to redraft it</span>`)
		}
		if rf == nil {
			state = "It is no longer waiting on you."
			break
		}
		if rf.Detail != "" {
			b.WriteString(`<p class="break-words text-sm text-fg2">` + esc(capFirst(refusalBy(rf.By))) + ` said: ` + esc(flows.VisibleEscape(rf.Detail)) + `</p>`)
		}
		live = true
	}
	switch {
	case c.Decision == builtin.CardApproved:
		b.WriteString(`<p class="text-sm text-fg" data-chat-card-decided="approved">You approved it.</p>`)
	case c.Decision == builtin.CardRejected && (c.Kind == builtin.CardRefile || c.Kind == builtin.CardDrop):
		b.WriteString(`<p class="text-sm text-fg" data-chat-card-decided="rejected">You dismissed it.</p>`)
	case c.Decision == builtin.CardRejected:
		b.WriteString(`<p class="text-sm text-fg" data-chat-card-decided="rejected">You rejected it.</p>`)
	case !live:
		b.WriteString(`<p class="text-sm text-fg2" data-chat-card-state>` + esc(state) + `</p>`)
	default:
		bind := `<input type="hidden" name="card_sha256" value="` + esc(c.CardSHA256) + `">` + sc.ownerField()
		yes, no, rejectFresh := "Approve and send", "Reject", ""
		switch c.Kind {
		case builtin.CardRefile:
			yes, no = "File it again then", "Dismiss"
		case builtin.CardDrop:
			yes, no = "Drop it", "Dismiss"
		case builtin.CardBatch:
			// rejecting the batch rejects every entry: a recent sign-in,
			// as its Approve (handleOfficerChatCard)
			rejectFresh = ` data-step-up-form`
		}
		b.WriteString(`<div class="` + ui.AsksDecisionClass + `"><form method="POST" action="` + esc(base+"/approve") + `" data-step-up-form class="min-w-0">` + bind +
			`<button type="submit" class="` + ui.AsksPrimaryButtonClass + `" data-chat-card-approve>` + yes + `</button></form>` +
			`<form method="POST" action="` + esc(base+"/reject") + `"` + rejectFresh + ` class="flex flex-wrap items-center gap-2">` + bind +
			`<button type="submit" class="` + ui.AsksSecondaryButtonClass + `" data-chat-card-reject>` + no + `</button></form></div>`)
	}
	b.WriteString(`</li>`)
	return b.String()
}

// officerChatHasOpenCard reports whether the chat section draws an Approve
// (the page then carries the step-up dialog).
func officerChatHasOpenCard(section string) bool {
	return strings.Contains(section, "data-chat-card-approve")
}

// --- the card's decision ---------------------------------------------------------

func (s *Server) handleOfficerChatCardApprove(w http.ResponseWriter, r *http.Request) {
	s.handleOfficerChatCard(w, r, true)
}

func (s *Server) handleOfficerChatCardReject(w http.ResponseWriter, r *http.Request) {
	s.handleOfficerChatCard(w, r, false)
}

// handleOfficerChatCard is a confirm card's Approve or Reject. 🔴 THE STEP-UP
// IS CHECKED HERE, ON THE SERVER, FOR AN APPROVAL, as for every approval
// (decideDraft).
func (s *Server) handleOfficerChatCard(w http.ResponseWriter, r *http.Request, approve bool) {
	sc, id, _, _, ok := s.controlTarget(w, r)
	if !ok {
		return
	}
	card, err := strconv.ParseInt(r.PathValue("card"), 10, 64)
	if err != nil || card <= 0 {
		s.renderOfficerDetail(w, r, sc, id, http.StatusNotFound, "No such card.")
		return
	}
	// 🔴 A BATCH CARD'S REJECT NEEDS A RECENT SIGN-IN TOO: it rejects every
	// entry at once, as the Asks page's batch decision does (handleOutreach-
	// BatchDecide checks the step-up whatever is ticked). A single draft's
	// Reject stays as the Asks page has it.
	needFresh := approve
	if !approve {
		if c, err := s.officerStore.ChatCard(r.Context(), sc.Owner, id, card); err == nil && c.Kind == builtin.CardBatch {
			needFresh = true
		}
	}
	if needFresh && !stepup.Fresh(r.Context(), s.stepUpNow()) {
		if isDocumentNavigation(r) {
			w.Header().Set("X-Muster-Step-Up", "required")
			s.renderOfficerDetail(w, r, sc, id, http.StatusForbidden, DraftStepUpRequired)
			return
		}
		refuseStale(w)
		return
	}
	notice, refused := s.decideChatCard(r.Context(), sc.Owner, id, card, r.PostFormValue("card_sha256"), approve)
	if refused != nil {
		s.renderOfficerDetail(w, r, sc, id, refused.code, refused.msg)
		return
	}
	http.Redirect(w, r, withNotice(officerHref(sc, id, ""), notice)+"#"+officerChatSectionID, http.StatusSeeOther)
}

// The refusals of a card's decision.
const (
	ChatCardNotShown = "This is not the card you were shown: reload the page and look at it again."
	ChatCardChanged  = "It changed since this card was drawn, so nothing was decided: ask it for a new card."
)

// decideChatCard is THE DECISION ON CONFIRM CARD id of the caller's chat with
// officerOwner's officer: the card's own draft (or batch) decision, given
// the SHA-256s the card was drawn with — so the plugins store refuses it
// under its row lock if what it showed has changed — and then the card's own
// outcome. Answers the page's notice, or the refusal.
func (s *Server) decideChatCard(ctx context.Context, officerOwner int64, officer string, id int64, posted string, approve bool) (string, *draftRefusal) {
	// 🔴 AN OFFICER NEVER DECIDES ITS OWN WORK — refused by the ONE rule in
	// the decision every card ends in (decideDraftAs, decideBatchAs: their
	// officerDeciderRefusal), before anything is decided and before the
	// card's own outcome is written. Not repeated here: one rule, one place.
	c, err := s.officerStore.ChatCard(ctx, officerOwner, officer, id)
	switch {
	case errors.Is(err, builtin.ErrNotFound):
		return "", &draftRefusal{http.StatusNotFound, "No such card.", ""}
	case err != nil:
		s.logger.Printf("officers: read confirm card %d of %s: %v", id, officer, err)
		return "", &draftRefusal{http.StatusInternalServerError, "That could not be recorded. Try again in a moment.", ""}
	case c.Decision != "":
		return "", &draftRefusal{http.StatusConflict, "That card was already decided.", ""}
	case posted == "" || posted != c.CardSHA256 || builtin.CardSHA256(c) != c.CardSHA256:
		return "", &draftRefusal{http.StatusConflict, ChatCardNotShown, ""}
	}
	via := plugins.DraftDecisionSource{Via: plugins.DraftViaBrowser}
	switch c.Kind {
	case builtin.CardDraft:
		dec := plugins.DraftDecision{Approve: approve, Via: via}
		if approve {
			// 🔴 THE CARD'S SHA-256s, NEVER THE DRAFT'S AS IT IS NOW: what the
			// card showed is what is approved, or nothing is.
			dec.BodySHA256, dec.MediaSHA256 = c.BodySHA256, append([]string{}, c.MediaSHA256...)
		}
		if _, refused := s.decideDraftAs(ctx, c.DraftID, dec); refused != nil {
			if refused.code == http.StatusConflict && approve {
				return "", &draftRefusal{http.StatusConflict, ChatCardChanged + " (" + refused.msg + ")", ""}
			}
			return "", refused
		}
	case builtin.CardBatch:
		dec := plugins.BatchDecision{BatchSHA256: c.BatchSHA256, Approve: []int64{}, Via: via}
		if approve {
			dec.Approve = append([]int64{}, c.BatchEntries...)
		}
		if _, refused := s.decideBatchAs(ctx, c.BatchID, dec); refused != nil {
			if refused.code == http.StatusConflict {
				return "", &draftRefusal{http.StatusConflict, ChatCardChanged, ""}
			}
			return "", refused
		}
	case builtin.CardRefile, builtin.CardDrop:
		if approve {
			// The officer page's own choice, made by THIS person's click:
			// the decider rule first (no decision door here ends in
			// decideDraftAs), then the store call the page's route makes —
			// a re-file as the officer's own work (RefileRefusalFromChat),
			// never as an owner's item that gets past the budget.
			if msg := officerDeciderRefusal(ctx); msg != "" {
				return "", &draftRefusal{http.StatusForbidden, msg, ""}
			}
			if refused := s.decideRefusalCard(ctx, officerOwner, officer, c); refused != nil {
				return "", refused
			}
		}
	default:
		return "", &draftRefusal{http.StatusConflict, ChatCardNotShown, ""}
	}
	decision, notice := builtin.CardRejected, "card-rejected"
	switch {
	case approve && c.Kind == builtin.CardRefile:
		decision, notice = builtin.CardApproved, "refiled"
	case approve && c.Kind == builtin.CardDrop:
		decision, notice = builtin.CardApproved, "dropped"
	case approve:
		decision, notice = builtin.CardApproved, "card-approved"
	case c.Kind == builtin.CardRefile || c.Kind == builtin.CardDrop:
		notice = "card-dismissed"
	}
	if err := s.officerStore.DecideChatCard(ctx, c.ID, decision, s.officerNow()); err != nil {
		// The decision itself is made and recorded on the draft (or batch);
		// the card then reads "no longer waiting".
		s.logger.Printf("officers: record confirm card %d of %s %s: %v", c.ID, officer, decision, err)
	}
	return notice, nil
}

// decideRefusalCard makes refusal card c's choice as the caller (a person's
// signed-in session): Re-file at the card's time, or Drop — the store calls
// the officer page's routes make, refused by row-level security for a person
// who may not, and audited for a team as theirs are.
func (s *Server) decideRefusalCard(ctx context.Context, officerOwner int64, officer string, c builtin.ChatCard) *draftRefusal {
	now := s.officerNow()
	p := authz.MustFrom(ctx)
	by := users.Actor{UserID: p.UserID, Via: p.Via}
	refused := func(err error) *draftRefusal {
		code, msg := refusalRefused(err)
		if code == http.StatusInternalServerError {
			s.logger.Printf("officers: confirm card %d (%s refusal %d of %s): %v", c.ID, c.Kind, c.RefusalItem, officer, err)
		}
		return &draftRefusal{code, msg, ""}
	}
	if c.Kind == builtin.CardDrop {
		if err := s.officerStore.DropRefusal(ctx, officerOwner, officer, c.RefusalItem, now); err != nil {
			return refused(err)
		}
		s.logger.Printf("officers: user %d dropped refusal %d of %s (owner %d) from their chat's card", p.UserID, c.RefusalItem, officer, officerOwner)
		if s.officerOwnerIsTeam(ctx, officerOwner) {
			s.auditTeam(ctx, by, officerOwner, "officer.refusal.drop", "officer_def:"+officer, map[string]any{"item": c.RefusalItem, "via": "chat"})
		}
		return nil
	}
	at := now
	if c.RefileAt != nil && c.RefileAt.After(now) {
		at = *c.RefileAt
	}
	carrier, err := s.officerStore.RefileRefusalFromChat(ctx, officerOwner, officer, c.RefusalItem, at, now)
	if err != nil {
		return refused(err)
	}
	s.logger.Printf("officers: user %d re-filed refusal %d of %s (owner %d) from their chat's card, at %s as item %d", p.UserID, c.RefusalItem, officer, officerOwner,
		at.UTC().Format(time.RFC3339), carrier)
	if s.officerOwnerIsTeam(ctx, officerOwner) {
		s.auditTeam(ctx, by, officerOwner, "officer.refusal.refile", "officer_def:"+officer, map[string]any{"item": c.RefusalItem, "at": at.UTC(), "as": carrier, "via": "chat"})
	}
	if s.officerRT != nil && s.officerRT.rec != nil && !at.After(now) {
		if o, ok := s.officerRT.reg.ForOwner(officerOwner, officer); ok {
			s.officerRT.rec.Due(o.Key())
		}
	}
	return nil
}
