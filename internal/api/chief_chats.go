package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/chiefusage"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE CHIEF'S PRIMARY AND SIDE CHATS (internal/agents chief_chats.go,
// migration 0115; docs/chief.md#primary-and-side-chats).
//
//	POST /agents/{id}/sessions/side              a new side chat
//	POST /agents/{id}/sessions/{sid}/archive     archive a side chat
//	POST /agents/{id}/sessions/{sid}/unarchive   unarchive it
//	POST /agents/{id}/sessions/fresh-primary     a fresh primary, with a handover
//	GET  /ui/agents/{name}/chat-context          the chat's context chip
//
// 🔴 OWNER ISOLATION IS THE AGENT READ: every route reads the agent (and the
// session) under the caller's principal, so another owner's id is a 404, and a
// session that is not the agent's is a 404. Every route is the CHIEF's only: a
// worker's sessions have no primary to protect, and no side chats.
//
// 🔴 A SIDE CHAT IS A CLAUDE SESSION OF ITS OWN. A legacy session of a
// claude-code chief has no lane: ccd runs it in its ONE primary CLI, i.e. in
// the primary's conversation (session_runtime.go). So a claude-code chief's
// side chat is opened on the claude-code session runtime — X-Muster-CC-Session,
// a CLI of its own (cmd/ccd/lanes.go) — whether or not the runtime PICKER is
// offered (MUSTER_CHIEF_SESSION_RUNTIMES gates the picker, not this). A gateway
// chief's is a new session key, which is a new gateway conversation.

func (s *Server) registerChiefChatRoutes(mux Mux) {
	mux.HandleFunc("POST /agents/{id}/sessions/side", s.requireOperatorWrite(s.handleChiefSideChat))
	// The handover runs chief turns: only with a gateway.
	mux.HandleFunc("POST /agents/{id}/sessions/fresh-primary", s.requireSession(s.requireGatewayProvisioner(s.requireSameOrigin(s.handleChiefFreshPrimary))))
	mux.HandleFunc("POST /agents/{id}/sessions/{sid}/archive", s.requireOperatorWrite(s.handleChiefChatArchive))
	mux.HandleFunc("POST /agents/{id}/sessions/{sid}/unarchive", s.requireOperatorWrite(s.handleChiefChatUnarchive))
	mux.HandleFunc("GET /ui/agents/{name}/chat-context", s.requireSession(s.handleChiefChatContext))
}

// chiefChatStore is the store's 0115 half, when it has one.
func (s *Server) chiefChatStore() (agents.ChatStore, bool) {
	st, ok := s.ext.Agents.(agents.ChatStore)
	return st, ok
}

// chiefFor reads agent {id} under the caller's principal and answers 404 for
// any agent that is not visible to them or is not their chief.
func (s *Server) chiefFor(w http.ResponseWriter, r *http.Request) (agents.Agent, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return agents.Agent{}, false
	}
	a, err := s.ext.Agents.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			http.NotFound(w, r)
			return agents.Agent{}, false
		}
		s.logger.Printf("chief chats: agent %d: %v", id, err)
		http.Error(w, "could not load agent", http.StatusInternalServerError)
		return agents.Agent{}, false
	}
	if !s.isChiefAgent(r.Context(), a) {
		http.Error(w, "side chats are the chief's", http.StatusNotFound)
		return agents.Agent{}, false
	}
	return a, true
}

// sideChatRuntime is the runtime a's new chats (a side chat, a fresh primary)
// are opened on: claude-code for a claude-code chief (a lane of its own), the
// agent's own (legacy) otherwise.
func sideChatRuntime(a agents.Agent) string {
	if agents.ResolveKind(a.Kind) == agents.KindClaudeCode {
		return agents.SessionRuntimeClaudeCode
	}
	return agents.SessionRuntimeLegacy
}

// openSideChat opens a new side chat of chief a: its own session key, and on a
// claude-code chief its own lane.
//
// ⚠ THE NEWEST SIDE CHAT IS REUSED WHILE IT IS EMPTY, as POST
// /agents/{id}/sessions reuses an empty latest one: a double tap opens one chat.
func (s *Server) openSideChat(ctx context.Context, a agents.Agent) (agents.ChatSession, error) {
	runtime := sideChatRuntime(a)
	if cs, ok := s.chiefChatStore(); ok {
		if list, err := cs.ListChats(ctx, a.ID); err == nil {
			sw := agents.SwitcherOrder(list)
			if len(sw.Side) > 0 && sw.Side[0].Runtime == runtime {
				if msgs, err := s.ext.Agents.ListRecentChatMessages(ctx, sw.Side[0].ID, 1); err == nil && len(msgs) == 0 {
					return sw.Side[0].ChatSession, nil
				}
			}
		}
	}
	if runtime == agents.SessionRuntimeLegacy {
		return s.ext.Agents.CreateSession(ctx, a.ID, a.Name)
	}
	st, ok := s.sessionRuntimeStore()
	if !ok {
		return s.ext.Agents.CreateSession(ctx, a.ID, a.Name)
	}
	return st.CreateSessionOn(ctx, a.ID, a.Name, runtime, "")
}

// handleChiefSideChat handles POST /agents/{id}/sessions/side.
func (s *Server) handleChiefSideChat(w http.ResponseWriter, r *http.Request) {
	a, ok := s.chiefFor(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	// The primary is elected first, so the new chat can never be taken for it.
	if _, err := s.primarySession(ctx, a); err != nil {
		s.logger.Printf("chief chats: primary of %d: %v", a.ID, err)
		http.Error(w, "could not open the primary chat", http.StatusInternalServerError)
		return
	}
	sess, err := s.openSideChat(ctx, a)
	if err != nil {
		s.logger.Printf("chief chats: side chat of %d: %v", a.ID, err)
		http.Error(w, "could not open a side chat", http.StatusInternalServerError)
		return
	}
	s.logger.Printf("chief chats: %s: side chat %d (runtime %q)", a.Name, sess.ID, sess.Runtime)
	s.answerSessionOpened(ctx, w, r, a, sess)
}

// handleChiefChatArchive handles POST /agents/{id}/sessions/{sid}/archive,
// answered with the PRIMARY (the chat the operator is left on).
func (s *Server) handleChiefChatArchive(w http.ResponseWriter, r *http.Request) {
	s.setChatArchived(w, r, true)
}

// handleChiefChatUnarchive handles POST /agents/{id}/sessions/{sid}/unarchive,
// answered with the chat itself.
func (s *Server) handleChiefChatUnarchive(w http.ResponseWriter, r *http.Request) {
	s.setChatArchived(w, r, false)
}

// setChatArchived is the two archive routes' one body.
func (s *Server) setChatArchived(w http.ResponseWriter, r *http.Request, archived bool) {
	{
		a, ok := s.chiefFor(w, r)
		if !ok {
			return
		}
		sid, err := strconv.ParseInt(r.PathValue("sid"), 10, 64)
		if err != nil {
			http.Error(w, "bad session id", http.StatusBadRequest)
			return
		}
		cs, ok := s.chiefChatStore()
		if !ok {
			http.Error(w, "this deployment's store cannot archive chats", http.StatusNotImplemented)
			return
		}
		ctx := r.Context()
		sess, err := cs.SetArchived(ctx, a.ID, sid, archived)
		switch {
		case errors.Is(err, agents.ErrArchivePrimary):
			http.Error(w, err.Error(), http.StatusConflict)
			return
		case errors.Is(err, pgx.ErrNoRows):
			http.NotFound(w, r)
			return
		case err != nil:
			s.logger.Printf("chief chats: archive %d of %d: %v", sid, a.ID, err)
			http.Error(w, "could not archive the chat", http.StatusInternalServerError)
			return
		}
		if archived {
			if sess, err = s.primarySession(ctx, a); err != nil {
				http.Error(w, "could not open the primary chat", http.StatusInternalServerError)
				return
			}
		}
		s.answerSessionOpened(ctx, w, r, a, sess)
	}
}

// handleChiefFreshPrimary handles POST /agents/{id}/sessions/fresh-primary: a
// new primary chat, the old one archived (agents.ChatStore.FreshPrimary — one
// transaction, so every delivery lands in exactly one of the two), and the
// HANDOVER started in the background (handOverPrimary).
func (s *Server) handleChiefFreshPrimary(w http.ResponseWriter, r *http.Request) {
	a, ok := s.chiefFor(w, r)
	if !ok {
		return
	}
	cs, ok := s.chiefChatStore()
	if !ok {
		http.Error(w, "this deployment's store has no primary chat", http.StatusNotImplemented)
		return
	}
	ctx := r.Context()
	if _, err := s.primarySession(ctx, a); err != nil {
		http.Error(w, "could not open the primary chat", http.StatusInternalServerError)
		return
	}
	// 🔴 ONE FRESH PRIMARY PER CHIEF AT A TIME, HANDOVER INCLUDED: held from
	// here until the handover's last turn ends, so a second tap (another tab,
	// a double tap) gets a clean 409 instead of a second switch and a second
	// handover. The store's NOWAIT lock (ErrFreshPrimaryBusy) is the same rule
	// across processes, for the switch itself.
	if _, busy := s.freshPrimaries.LoadOrStore(a.ID, struct{}{}); busy {
		http.Error(w, agents.ErrFreshPrimaryBusy.Error(), http.StatusConflict)
		return
	}
	release := func() { s.freshPrimaries.Delete(a.ID) }
	old, fresh, err := cs.FreshPrimary(ctx, a.ID, a.Name, sideChatRuntime(a), "")
	if errors.Is(err, agents.ErrFreshPrimaryBusy) {
		release()
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if err != nil {
		release()
		s.logger.Printf("chief chats: fresh primary of %d: %v", a.ID, err)
		http.Error(w, "could not start a fresh primary chat", http.StatusInternalServerError)
		return
	}
	s.logger.Printf("chief chats: %s: fresh primary %d (old primary %d archived)", a.Name, fresh.ID, old.ID)
	s.handOverPrimary(ctx, a, old, fresh, release)
	s.answerSessionOpened(ctx, w, r, a, fresh)
}

// FreshPrimaryHandoverPrompt is the final turn muster sends in the OLD primary
// when a fresh one takes over: the chief writes its own handover there.
const FreshPrimaryHandoverPrompt = "[muster] Your owner started a FRESH primary chat; this conversation is now archived " +
	"and nothing more will be delivered to it. Write a short handover for your next primary chat — at most 200 words: " +
	"what is in flight, what you are waiting on, and anything you promised your owner. Plain text, no preamble."

// FreshPrimaryHandoverPrefix starts the new primary's first turn, before the
// handover the old chat wrote.
const FreshPrimaryHandoverPrefix = "[muster] This is your new primary chat: every delivery to you arrives here from now on. " +
	"Your previous primary chat was archived; its handover follows. Read it, then answer with one line saying you have it.\n\n"

// FreshPrimaryNoHandover is the new primary's first turn when the old chat
// wrote none (its turn failed or answered no text).
const FreshPrimaryNoHandover = "[muster] This is your new primary chat: every delivery to you arrives here from now on. " +
	"Your previous primary chat was archived, and it wrote no handover. Answer with one line saying you are ready."

// handOverPrimary runs the handover of a fresh primary, in the background:
//
//  1. ONE FINAL TURN IN THE OLD PRIMARY (FreshPrimaryHandoverPrompt): the chief
//     writes the summary in the conversation that actually holds the context;
//  2. the NEW primary's first turn carries it (FreshPrimaryHandoverPrefix), so
//     the new conversation starts with it in its context — not only on screen.
//
// 🔴 WHY THE CHIEF WRITES IT, NOT A SUMMARIZER: the context to summarise is the
// old CONVERSATION, which only its runtime holds — muster stores the visible
// transcript, not the tool results and reads the chief's session holds, and
// has no model of its own to summarise with on every deployment (a gateway
// chief's OpenRouter key is the agent's). One turn of the chief's own costs one
// turn and is right by construction.
//
// Both turns are ordinary chat turns (runUserTurn: counted for "working…",
// recorded in the chief's usage). Deliveries already go to the new primary
// while they run (the switch committed first); one that arrives meanwhile
// queues behind the handover turn in the agent's turn order.
//
// done is called once the handover has ended, however it ended.
func (s *Server) handOverPrimary(ctx context.Context, a agents.Agent, old, fresh agents.ChatSession, done func()) {
	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*agentTurnBudget(a.Kind))
	safeGo(s.logger, "chief handover", func() {
		defer done()
		defer cancel()
		parts, err := s.runUserTurn(hctx, a, old, FreshPrimaryHandoverPrompt, nil)
		if err != nil {
			s.logger.Printf("chief chats: %s: the old primary %d wrote no handover: %v", a.Name, old.ID, err)
			parts = turnFailedParts(err)
		}
		s.persistTurn(hctx, a, old, parts)
		summary := handoverText(parts, err)
		text := FreshPrimaryNoHandover
		if summary != "" {
			text = FreshPrimaryHandoverPrefix + summary
		}
		next, err := s.runUserTurn(hctx, a, fresh, text, nil)
		if err != nil {
			s.logger.Printf("chief chats: %s: the new primary %d's first turn failed: %v", a.Name, fresh.ID, err)
			next = turnFailedParts(err)
		}
		s.persistTurn(hctx, a, fresh, next)
	})
}

// handoverText is the handover a turn's parts carry: its text parts joined,
// "" for a failed turn or one with no text.
func handoverText(parts []agents.TurnPart, err error) string {
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Kind != "text" || strings.TrimSpace(p.Content) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(strings.TrimSpace(p.Content))
	}
	return b.String()
}

// handleChiefChatContext serves GET /ui/agents/{name}/chat-context?session=N:
// the chief chat's context chip (ui.RenderChiefContextChip), from the latest
// chief_turn_usage row of THAT session that reported usage — input + cache
// read + cache write, the chip's Claude derivation — of the model's window
// (modelContextWindow). Nothing (an empty 200) before any turn reported usage.
func (s *Server) handleChiefChatContext(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	a, err := s.ext.Agents.GetByName(ctx, r.PathValue("name"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sid, err := strconv.ParseInt(r.URL.Query().Get("session"), 10, 64)
	if err != nil || sid <= 0 {
		http.Error(w, "bad session", http.StatusBadRequest)
		return
	}
	sess, err := s.ext.Agents.GetSession(ctx, sid)
	if err != nil || sess.AgentID != a.ID {
		http.NotFound(w, r)
		return
	}
	if !s.isChiefAgent(ctx, a) {
		http.NotFound(w, r)
		return
	}
	c, found := s.chatContext(ctx, sess)
	if !found {
		return
	}
	window := s.modelContextWindow(ctx, c.model)
	if err := ui.RenderChiefContextChip(w, ui.ChiefContextView(c.tokens, window, c.model)); err != nil {
		s.logger.Printf("chief chats: render context chip: %v", err)
	}
}

// chiefChatViews is chief a's chats as the switcher lists them —
// agents.SwitcherOrder: the primary first, the side chats by last activity,
// the archived ones last (each marked) — with the unread dot, active marking
// activeID. ok false on a store without the 0115 half (the caller lists as
// before). An ARCHIVED chat that is the one open is listed with the side chats,
// so the open chat is always in the list.
func (s *Server) chiefChatViews(ctx context.Context, a agents.Agent, activeID int64) ([]ui.SessionView, bool, error) {
	cs, ok := s.chiefChatStore()
	if !ok {
		return nil, false, nil
	}
	list, err := cs.ListChats(ctx, a.ID)
	sw := agents.SwitcherOrder(list)
	ordered := make([]agents.ChatListing, 0, len(list))
	if sw.Primary != nil {
		ordered = append(ordered, *sw.Primary)
	}
	ordered = append(ordered, sw.Side...)
	ordered = append(ordered, sw.Archived...)
	sessions := make([]agents.ChatSession, 0, len(ordered))
	unread := make(map[int64]bool, len(ordered))
	for _, c := range ordered {
		sessions = append(sessions, c.ChatSession)
		unread[c.ID] = c.Unread && c.ID != activeID
	}
	views := s.sessionViews(ctx, a, sessions, activeID)
	for i := range views {
		views[i].Unread = unread[views[i].ID]
	}
	return views, true, err
}

// chiefChatBarFor is the chat bar of chief a's chat sess (ui.ChiefChatBar).
func chiefChatBarFor(a agents.Agent, sess agents.ChatSession) *ui.ChiefChatBar {
	return &ui.ChiefChatBar{AgentID: a.ID, AgentName: a.Name, SessionID: sess.ID,
		Primary: sess.IsDefault, Archived: sess.ArchivedAt != nil}
}

// chiefContextReader is the telemetry store's context read (chiefusage.PGStore).
type chiefContextReader = chiefusage.ContextReader

// sessionContextFill is a chat's latest context fill (chatContext).
type sessionContextFill struct {
	tokens int64
	model  string
	at     time.Time
}

// chatContext is session sess's latest context fill from the chief's usage
// rows; found false with no telemetry store or no usage row yet.
func (s *Server) chatContext(ctx context.Context, sess agents.ChatSession) (sessionContextFill, bool) {
	rd, ok := s.ext.ChiefUsage.(chiefContextReader)
	if !ok {
		return sessionContextFill{}, false
	}
	c, found, err := rd.LatestContext(ctx, sess.ID)
	if err != nil {
		s.logger.Printf("chief chats: context of session %d: %v", sess.ID, err)
		return sessionContextFill{}, false
	}
	if !found {
		return sessionContextFill{}, false
	}
	return sessionContextFill{tokens: c.Tokens, model: c.Model, at: c.At}, true
}

// modelContextWindow is model's context window in tokens, where muster knows
// it, 0 where it does not (the chip then shows the count alone, untinted):
//
//   - a Claude model — an alias, a claude-* id, "claude" — by
//     agents.ClaudeContextWindow (Claude Code reports no window);
//   - an OpenRouter model ("openrouter/<id>") by its catalogue entry's
//     context_length (CatalogModel.ContextLength), read from the process's one
//     catalogue cache with a short bound, so a cold cache costs the chip, not
//     the page (the chip loads on its own request).
//
// The only other place muster knows windows is a mirrored opencode session's
// own provider list (internal/opencodehost ContextLimit), which is not the
// chief's.
func (s *Server) modelContextWindow(ctx context.Context, model string) int64 {
	if w := agents.ClaudeContextWindow(model); w > 0 {
		return w
	}
	id, ok := strings.CutPrefix(model, "openrouter/")
	if !ok || s.ext.ModelCatalog == nil {
		return 0
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	list, err := s.ext.ModelCatalog.Models(cctx)
	if err != nil {
		return 0
	}
	for _, m := range list {
		if m.ID == id {
			return m.ContextLength
		}
	}
	return 0
}
