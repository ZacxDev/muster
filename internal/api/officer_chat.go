package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ZacxDev/muster/internal/agentgateway"
	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/ccpool"
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

// errNoChatStore is a chat on a deployment whose agents store has no 0149
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
	// 🔴 A HOME PAST THE POOL'S SLOTS IS STILL A HOME (chat_max_instances
	// lowered since): its number is answered, so the runner re-homes the chat
	// rather than holding it for ever on an error.
	for n := 1; n <= officers.MaxInstancesCap; n++ {
		if o.InstanceName(n) == a.Name {
			return n, nil
		}
	}
	return 0, fmt.Errorf("its session %d is on agent %s, which is not an instance of %s", sess.ID, a.Name, o.ID)
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
	m, err := st.OfficerChatMarks(l.s.officerPrincipal(ctx, o.Owner), sess.ID)
	return officers.ChatMarks{Seen: m.Seen, Answered: m.Answered, Charter: m.Charter}, err
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
	return st.SetOfficerChatMarks(l.s.officerPrincipal(ctx, o.Owner), sess.ID, agents.ChatLaneMarks{Seen: m.Seen, Answered: m.Answered, Charter: m.Charter})
}

// Rehome implements officers.ChatLanes: the chat's session on instance
// from's agent is set aside (agents.OfficerChatStore.SetAsideOfficerChat: its
// key renamed, its lane secret cleared, its history kept on that agent), so
// the key is free for the session Open makes on the chat's new instance. No
// such agent, or no such session on it, is nothing to set aside.
func (l officerChatLanes) Rehome(ctx context.Context, o officers.Officer, chatID string, from int) error {
	st, err := l.store()
	if err != nil {
		return err
	}
	a, err := l.instance(ctx, o, from)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // no agent: its sessions went with it
	}
	if err != nil {
		return err
	}
	_, err = st.SetAsideOfficerChat(l.s.officerPrincipal(ctx, o.Owner), a.ID, officers.ChatSessionKey(chatID))
	return err
}

// mcpSessionLaneReader is the agent gateway's read of a ccd's
// mcp_session_lanes (agentgateway.Gateway.MCPSessionLanes), asserted: a
// gateway without it runs no chat.
type mcpSessionLaneReader interface {
	MCPSessionLanes(ctx context.Context, a agents.Agent) (bool, error)
}

var _ mcpSessionLaneReader = (*agentgateway.Gateway)(nil)

// Supports implements officers.ChatLanes: instance n's ccd says, on its
// /healthz, that it starts a chat lane with an MCP config naming the lane
// (mcp_session_lanes). An older ccd would run the lane on the pod's plain MCP
// config, where its calls read as the primary's — so it runs no chat.
func (l officerChatLanes) Supports(ctx context.Context, o officers.Officer, n int) (bool, error) {
	r, ok := l.s.ext.Gateway.(mcpSessionLaneReader)
	if !ok {
		return false, nil
	}
	a, err := l.instance(ctx, o, n)
	if err != nil {
		return false, err
	}
	return r.MCPSessionLanes(l.s.officerPrincipal(ctx, o.Owner), a)
}

// officerChatTurnSlack is added to the kind's turn timeout for a chat turn's
// deadline, as for a re-prompt.
const officerChatTurnSlack = 2 * time.Minute

// Turn implements officers.ChatLanes: one turn in the chat's lane, recorded on
// its session (the user turn once the lane took it; the reply when there is
// one). A turn ccd refused before typing anything (chatTurnError) is
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
	st, _ := l.store()
	secret, err := st.EnsureChatLaneSecret(octx, sess.ID)
	if err != nil {
		return "", fmt.Errorf("officers: chat %s's lane secret: %w", chatID, err)
	}
	tctx, cancel := context.WithTimeout(officerChatTurnContext(octx, a, sess, secret), agents.KindTurnTimeout(a.Kind)+officerChatTurnSlack)
	defer cancel()
	reply, err := func() (string, error) {
		// Held open for the outcome watchdog while it runs, as a job's turn.
		defer s.OutcomeHold(a)()
		return s.ext.Gateway.Chat(tctx, a, sess.SessionKey, text, nil)
	}()
	cerr, unsent := chatTurnError(err)
	if unsent {
		return "", cerr
	}
	store := context.WithoutCancel(octx)
	_, _ = s.ext.Agents.AddChatMessage(store, agents.ChatMessage{AgentID: a.ID, SessionID: sess.ID, Role: "user", Content: text})
	if reply != "" {
		_, _ = s.ext.Agents.AddChatMessage(store, agents.ChatMessage{AgentID: a.ID, SessionID: sess.ID, Role: "assistant", Content: reply})
	}
	return reply, cerr
}

// chatTurnError is a chat turn's gateway error as the runner reads it, and
// whether nothing was typed (unsent: the turn is recorded nowhere):
//
//   - a refusal that passes by itself (agentgateway.Unsent: 409 busy, 503
//     not_ready, 503 capacity): officers.ErrChatLaneUnavailable, unsent;
//   - 🔴 a 409 `rate_limited` — a pane parked on the CLI's usage-limit menu,
//     which Unsent reads as busy so a deliverer queues it again — is NOT
//     busy: busy passes within a turn, the limit lasts until its reset. It is
//     ErrChatLaneUnavailable AND officers.ErrChatRateLimited, unsent, so the
//     runner backs off and then tells the member;
//   - every other Unsent reason (unresolved, unauthorized, rejected, never
//     connected): officers.ErrChatLaneBroken, unsent;
//   - a typed turn that failed `rate_limited` (ccd's 429):
//     officers.ErrChatRateLimited, wrapping err;
//   - anything else: err as it is (nil for a turn that answered).
func chatTurnError(err error) (error, bool) {
	limited := chatTurnRateLimited(err)
	switch unsent := agentgateway.Unsent(err); unsent {
	case "":
	case agentgateway.UnsentBusy, agentgateway.UnsentNotReady, agentgateway.UnsentCapacity:
		if limited {
			return fmt.Errorf("%w: %w (nothing was typed): %v%s", officers.ErrChatLaneUnavailable, officers.ErrChatRateLimited, err, rateLimitDetail(err)), true
		}
		// Passes by itself: another turn, a CLI starting, every lane busy.
		if unsent == agentgateway.UnsentBusy {
			// A turn runs in the lane (or its pane shows a dialog): the
			// runner waits a turn in flight out (officers.ErrChatLaneBusy).
			return fmt.Errorf("%w: %w (%s)", officers.ErrChatLaneUnavailable, officers.ErrChatLaneBusy, unsent), true
		}
		return fmt.Errorf("%w (%s)", officers.ErrChatLaneUnavailable, unsent), true
	default:
		// Does not pass by itself (unresolved, unauthorized, rejected, never
		// connected): a failure the runner backs off from.
		return fmt.Errorf("%w (%s)", officers.ErrChatLaneBroken, unsent), true
	}
	if err != nil && limited {
		return fmt.Errorf("%w: %w%s", officers.ErrChatRateLimited, err, rateLimitDetail(err)), false
	}
	return err, false
}

// rateLimitDetail is the runtime's own message of a typed failure, as
// " (<message>)" — where the CLI states its reset ("resets Nov 23, 1am
// (UTC)"), which the runner quotes to the member. 🔴 err's text alone is not
// enough: a RuntimeError's Error() carries a snippet of the body, which can be
// cut short or empty, and the message is what ccd states the reset in.
func rateLimitDetail(err error) string {
	var rt *agents.RuntimeError
	if !errors.As(err, &rt) || rt.Message == "" {
		return ""
	}
	return " (" + rt.Message + ")"
}

// chatTurnRateLimited reports whether err is the runtime's typed
// `rate_limited` (cmd/ccd failure.go): a 429 for a turn the account's limit
// failed, or a 409 from a pane parked on the usage-limit menu.
func chatTurnRateLimited(err error) bool {
	var rt *agents.RuntimeError
	return errors.As(err, &rt) && rt.Type == ccpool.FailureRateLimited
}

// officerChatTurnContext is the context a chat turn on agent a's session sess
// is sent under: the session's own route (withSessionRoute), plus
// agents.HeaderCCMCPSession — ccd starts the chat's CLI with an MCP config
// naming its session, so the lane's own tool calls reach dispatchOfficerTool
// as the chat's (chatLaneCall), never as the primary's. withRoute, the
// route every other session (the chief's) takes, is unchanged.
func officerChatTurnContext(ctx context.Context, a agents.Agent, sess agents.ChatSession, secret string) context.Context {
	ctx = withSessionRoute(ctx, a, sess)
	r, _ := agents.TurnRouteFrom(ctx)
	h := r.Header.Clone()
	if h == nil {
		h = http.Header{}
	}
	h.Set(agents.HeaderCCMCPSession, "1")
	h.Set(agents.HeaderCCLaneSecret, secret)
	r.Header = h
	return agents.WithTurnRoute(ctx, r)
}

// chatLaneKey is the context key of a chat lane's tool call (withChatLaneCall).
type chatLaneKey struct{}

// withChatLaneCall marks ctx as a tool call from the chat lane whose session
// key is sessionKey — POST /mcp's agents.HeaderCCSession — when that is an
// officer chat's key (officers.ChatSessionKeyPrefix and a valid chat id);
// any other value leaves ctx as it is, so no other session's call changes.
func withChatLaneCall(ctx context.Context, sessionKey string) context.Context {
	chat, ok := strings.CutPrefix(sessionKey, officers.ChatSessionKeyPrefix)
	if !ok || officers.ChatSessionKey(chat) != sessionKey || !officers.ValidChatID(chat) {
		return ctx
	}
	return context.WithValue(ctx, chatLaneKey{}, chat)
}

// chatLaneSecrets is the store's read of a chat lane's secret
// (agents.OfficerChatStore.ChatLaneSecret), asserted alone so a test store
// can answer it.
type chatLaneSecrets interface {
	ChatLaneSecret(ctx context.Context, agentID int64, sessionKey string) (string, error)
}

// verifyChatLaneCall is POST /mcp's check of a call naming session key
// sessionKey with lane secret secret, for agent a:
//
//   - no officer chat key: ctx unchanged, no refusal (every other call);
//   - an officer chat key with the secret muster keeps on a's session of that
//     key (agents.HeaderCCLaneSecret, minted per lane): ctx marked as that
//     lane's call (withChatLaneCall);
//   - an officer chat key without it — no secret, another secret, another
//     agent's session, no such session, an unreadable one: a refusal.
//
// What the secret proves: the call carries a value muster gave that lane's
// ccd, which wrote it only into that lane's MCP config. Processes in the same
// pod run as the same user and could read that file: the pod is one trust
// domain (its token is shared too); the secret keeps a call from NAMING a
// lane it was not given, and the chat claim the lane acts on is still the
// instance's own (heldItem checks it per item).
func (s *Server) verifyChatLaneCall(ctx context.Context, a agents.Agent, sessionKey, secret string) (context.Context, string) {
	lane := withChatLaneCall(ctx, sessionKey)
	if _, ok := chatLaneCall(lane); !ok {
		return ctx, ""
	}
	refuse := toolErr("this call names chat session %s, but its lane secret does not match the one muster gave that session: refused", sessionKey)
	st, ok := s.ext.Agents.(chatLaneSecrets)
	if !ok || secret == "" {
		return ctx, refuse
	}
	want, err := st.ChatLaneSecret(ctx, a.ID, sessionKey)
	if err != nil {
		s.logger.Printf("officers: %s: a call naming chat session %s: its lane secret could not be read: %v; refused", a.Name, sessionKey, err)
		return ctx, refuse
	}
	if want == "" || subtle.ConstantTimeCompare([]byte(want), []byte(secret)) != 1 {
		return ctx, refuse
	}
	return lane, ""
}

// chatLaneCall is the chat id of the lane ctx's tool call comes from; false
// for every other call (the primary's, any non-officer agent's).
func chatLaneCall(ctx context.Context) (string, bool) {
	chat, ok := ctx.Value(chatLaneKey{}).(string)
	return chat, ok && chat != ""
}

// officerCallerName is the `instance` agent a's officer tool call acts as with
// the backend: a chat lane's is its chat claim's holder (officers.ChatHolder),
// so its item verbs reach its chat item and never the primary's claims; every
// other call is the agent's own name, as before.
func officerCallerName(ctx context.Context, a agents.Agent) string {
	if _, ok := chatLaneCall(ctx); ok {
		return officers.ChatHolder(a.Name)
	}
	return a.Name
}

// chatLaneQueueRefusal is the refusal, from a chat lane, of a queue verb
// (claim, heartbeat, complete, release), a verb that ends its holder's lease
// (an ask), or a ship action (bound to the primary's item, or a write to
// GitHub): a chat session works its chat, and muster holds and ends its
// chat's item.
func chatLaneQueueRefusal(tool, chat string) string {
	return toolErr("%s is not offered to a chat session: this session is chat %s, which muster claims, keeps and answers for you. "+
		"Reply to the team member in plain text; queue work and shipping are your instance's primary session's", tool, chat)
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
