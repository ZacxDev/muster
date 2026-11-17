package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/officers"
)

// OFFICER CHAT LANES, BOUND TO muster (internal/officers chat.go;
// docs/officers.md "Chat sessions").
//
// officerChatLanes is officers.ChatLanes over the agents store and the agent
// gateway. A chat's lane is a chat_sessions row on one instance's agent, on
// the claude-code runtime, keyed officers.ChatSessionKey(chat_id) — so its
// turns carry X-Muster-CC-Session (withSessionRoute, the route every
// claude-code session of every agent takes) and ccd runs the chat in a CLI of
// its own beside the instance's primary. Its turns and replies are recorded on
// that row, so the chat reads on the instance's agent page like any session.
//
// 🔴 NOT THE PRIMARY'S TURN CONTEXT. OfficerTurnContext (a fresh_session_per_item
// pool's new conversation per item) is the primary CLI's; a chat lane is one
// conversation for the chat's whole life, so its turn is never sent under it
// (TestAChatTurnIsALaneTurnAndNeverAFreshConversation).
type officerChatLanes struct{ s *Server }

var _ officers.ChatLanes = officerChatLanes{}

// errNoChatStore is a chat on a deployment whose agents store has no 0147
// half (no database).
var errNoChatStore = errors.New("officers: this deployment's agents store cannot keep a chat's session (it needs the database)")

func (l officerChatLanes) store() (agents.OfficerChatStore, error) {
	st, ok := l.s.ext.Agents.(agents.OfficerChatStore)
	if !ok {
		return nil, errNoChatStore
	}
	return st, nil
}

// Home implements officers.ChatLanes: the instance whose agent holds the
// chat's session row, 0 when none does. A row on an agent that is not one of
// o's chat-pool instances is an error, never a home (the chat waits).
func (l officerChatLanes) Home(ctx context.Context, o officers.Officer, chatID string) (int, error) {
	st, err := l.store()
	if err != nil {
		return 0, err
	}
	octx := l.s.officerPrincipal(ctx, o.Owner)
	sess, err := st.SessionByKeyAnyAgent(octx, officers.ChatSessionKey(chatID))
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	a, err := l.s.ext.Agents.Get(octx, sess.AgentID)
	if err != nil {
		return 0, fmt.Errorf("its session's agent %d: %w", sess.AgentID, err)
	}
	p, n, ok := l.s.officerRT.reg.ForAgentName(a.Name)
	if !ok || p.Key() != o.Key() || p.Tier != o.Tier {
		return 0, fmt.Errorf("its session %d is on agent %s, which is not an instance of %s", sess.ID, a.Name, o.ID)
	}
	return n, nil
}

// instance is instance n of o's agent row.
func (l officerChatLanes) instance(ctx context.Context, o officers.Officer, n int) (agents.Agent, error) {
	return l.s.ext.Agents.GetByName(l.s.officerPrincipal(ctx, o.Owner), o.InstanceName(n))
}

// session is chatID's lane session on instance a, made when it has none.
func (l officerChatLanes) session(ctx context.Context, o officers.Officer, a agents.Agent, chatID string) (agents.ChatSession, error) {
	st, err := l.store()
	if err != nil {
		return agents.ChatSession{}, err
	}
	return st.EnsureClaudeCodeSession(l.s.officerPrincipal(ctx, o.Owner), a.ID, officers.ChatSessionKey(chatID), "Chat "+chatID)
}

// Open implements officers.ChatLanes.
func (l officerChatLanes) Open(ctx context.Context, o officers.Officer, n int, chatID string) (officers.ChatMarks, error) {
	a, err := l.instance(ctx, o, n)
	if err != nil {
		return officers.ChatMarks{}, err
	}
	sess, err := l.session(ctx, o, a, chatID)
	if err != nil {
		return officers.ChatMarks{}, err
	}
	st, _ := l.store()
	seen, answered, err := st.OfficerChatMarks(l.s.officerPrincipal(ctx, o.Owner), sess.ID)
	return officers.ChatMarks{Seen: seen, Answered: answered}, err
}

// Mark implements officers.ChatLanes.
func (l officerChatLanes) Mark(ctx context.Context, o officers.Officer, n int, chatID string, m officers.ChatMarks) error {
	a, err := l.instance(ctx, o, n)
	if err != nil {
		return err
	}
	sess, err := l.session(ctx, o, a, chatID)
	if err != nil {
		return err
	}
	st, _ := l.store()
	return st.SetOfficerChatMarks(l.s.officerPrincipal(ctx, o.Owner), sess.ID, m.Seen, m.Answered)
}

// officerChatTurnSlack is added to the kind's turn timeout for a chat turn's
// deadline, as for a re-prompt.
const officerChatTurnSlack = 2 * time.Minute

// Turn implements officers.ChatLanes: one turn in the chat's lane, recorded on
// its session (the user turn once the lane took it; the reply when there is
// one). A turn ccd refused before typing anything (agentgateway.Unsent: 409
// busy, 503 not_ready, 503 capacity) is officers.ErrChatLaneUnavailable and is
// recorded nowhere.
func (l officerChatLanes) Turn(ctx context.Context, o officers.Officer, n int, chatID, text string) (string, error) {
	s := l.s
	if s.ext.Gateway == nil {
		return "", errors.New("officers: no agent gateway to send a chat turn through")
	}
	a, err := l.instance(ctx, o, n)
	if err != nil {
		return "", err
	}
	sess, err := l.session(ctx, o, a, chatID)
	if err != nil {
		return "", err
	}
	octx := s.officerPrincipal(ctx, o.Owner)
	tctx, cancel := context.WithTimeout(officerChatTurnContext(octx, a, sess), agents.KindTurnTimeout(a.Kind)+officerChatTurnSlack)
	defer cancel()
	reply, err := func() (string, error) {
		// Held open for the outcome watchdog while it runs, as a job's turn.
		defer s.OutcomeHold(a)()
		return s.ext.Gateway.Chat(tctx, a, sess.SessionKey, text, nil)
	}()
	if unsent := agentgateway.Unsent(err); unsent != "" {
		return "", fmt.Errorf("%w (%s)", officers.ErrChatLaneUnavailable, unsent)
	}
	store := context.WithoutCancel(octx)
	_, _ = s.ext.Agents.AddChatMessage(store, agents.ChatMessage{AgentID: a.ID, SessionID: sess.ID, Role: "user", Content: text})
	if reply != "" {
		_, _ = s.ext.Agents.AddChatMessage(store, agents.ChatMessage{AgentID: a.ID, SessionID: sess.ID, Role: "assistant", Content: reply})
	}
	return reply, err
}

// officerChatTurnContext is the context a chat turn on agent a's session sess
// is sent under: the session's own route (withSessionRoute) and nothing else.
func officerChatTurnContext(ctx context.Context, a agents.Agent, sess agents.ChatSession) context.Context {
	return withSessionRoute(ctx, a, sess)
}

// chatItemRefusal is officer_claim's answer when the item it claimed is a chat
// item: muster released it at once (the chat runner's, never the primary's).
type chatItemRefusal struct{ item officers.ItemID }

func (e *chatItemRefusal) Error() string {
	return fmt.Sprintf("officers: item %s is a chat turn, which is not queue work", e.item)
}

func (e *chatItemRefusal) result() string {
	return toolJSON(map[string]any{
		"claimed": false, "chat": true, "item_id": e.item,
		"note": fmt.Sprintf("not claimed: item %s is a chat turn (kind %q). muster runs every chat in a session of its own on this instance — it is not queue work: "+
			"never claim a chat item. Claim another open item by id (%s lists them; skip kind %q)", e.item, officers.ItemKindChat, officers.ToolItems, officers.ItemKindChat),
	})
}

// claimedChatItem reports whether body, a successful claim's answer, claimed
// a chat item (its item's kind), for an officer whose chats muster runs.
func (s *Server) claimedChatItem(o officers.Officer, body []byte) (officers.ItemID, bool) {
	if s.officerRT == nil || !s.officerRT.chats.On(o) {
		return "", false
	}
	var res struct {
		Item *officers.Item `json:"item"`
	}
	if json.Unmarshal(body, &res) != nil || res.Item == nil || !res.Item.IsChat() {
		return "", false
	}
	return res.Item.ID, true
}

// releaseChatClaim gives back a chat item the primary claimed by mistake
// (claimedChatItem), as the instance that holds it. A failed release is
// logged: the item is claimable again once its lease lapses.
func (s *Server) releaseChatClaim(ctx context.Context, c officers.Backend, o officers.Officer, instance string, item officers.ItemID) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err := c.Release(rctx, item, instance); err != nil {
		s.logger.Printf("officers: %s: release chat item %s claimed by %s's primary session: %v (claimable again once its lease lapses)", o.ID, item, instance, err)
		return
	}
	s.logger.Printf("officers: %s: %s's primary session claimed chat item %s; released it for the chat runner", o.ID, instance, item)
}
