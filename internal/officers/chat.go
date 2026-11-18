package officers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// CHAT SESSIONS (docs/officers.md "Chat sessions")
//
// A team member chats with an officer through the HQ server: HQ stores each
// chat and its messages, and every member turn enqueues (or re-opens) ONE item
// of kind "chat" for the officer — subject "chat:<chat_id>", payload
// {chat_id, seq}. muster polls HQ as it always has; HQ never calls muster.
//
// 🔴 A CHAT ITEM IS NOT QUEUE WORK. It is never delivered to an instance's
// primary CLI: the wake loop leaves it out of every queue count (Decide), and
// the claim tools refuse it (internal/api). Each chat runs in a Claude Code
// session of its OWN on an instance — a ccd lane, session key
// ChatSessionKey(chat_id) — so a chat and the queue work of the same
// instance run at once, and neither waits on the other's turn.
//
// One turn, by the ChatRunner (this file), in muster-server:
//
//  1. claim the chat's item, as ChatHolder(instance) — never the instance's
//     own name, so the primary's claims, heartbeats and releases never touch
//     it, and the wake loop never counts it as the primary's;
//  2. read the chat's messages after the last seq the lane has SEEN
//     (ChatMarks, persisted with the lane's session row);
//  3. send them to the lane as ONE user turn (ChatTurnText), heartbeating the
//     claim while it runs;
//  4. post the lane's final assistant text as the reply, in_reply_to the
//     member seq it answers — HQ completes the item.
//
// A lane muster cannot reach (ccd's 409 busy, 503 not_ready or capacity) is
// not a failure: the claim is released and the item waits for a later pass.
// ---------------------------------------------------------------------------

// ItemKindChat is the kind of an officer item that carries a chat turn.
const ItemKindChat = "chat"

// IsChat reports whether it is a chat item (ItemKindChat). THE one test of it:
// Decide, the claim tools and the chat runner all ask it.
func (it Item) IsChat() bool { return it.Kind == ItemKindChat }

// chatSubjectPrefix is a chat item's subject_key prefix ("chat:<chat_id>").
const chatSubjectPrefix = "chat:"

// ChatOf is chat item it's chat id and the member seq it was enqueued for:
// the payload's {chat_id, seq}, or — a payload without chat_id — the id from
// its subject "chat:<chat_id>" and seq 0. ok is false for an item that is not
// a chat item or names no chat.
func ChatOf(it Item) (chatID string, seq int64, ok bool) {
	if !it.IsChat() {
		return "", 0, false
	}
	var p struct {
		ChatID json.RawMessage `json:"chat_id"`
		Seq    int64           `json:"seq"`
	}
	if len(it.Payload) > 0 && json.Unmarshal(it.Payload, &p) == nil && len(p.ChatID) > 0 {
		var id ItemID // a chat id as HQ spells it: a string or a number
		if id.UnmarshalJSON(p.ChatID) == nil && ValidChatID(string(id)) {
			return string(id), p.Seq, true
		}
	}
	if id, found := strings.CutPrefix(it.SubjectKey, chatSubjectPrefix); found && ValidChatID(id) {
		return id, 0, true
	}
	return "", 0, false
}

// ValidChatID is a chat id muster will put in a URL path segment, a session
// key and a log line: 1..128 bytes of letters, digits and "._-".
func ValidChatID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// ChatSessionKeyPrefix prefixes every chat lane's muster session key.
const ChatSessionKeyPrefix = "officer-chat:"

// ChatSessionKey is the muster session key of chat chatID's lane: the key ccd
// names its CLI by (X-Muster-CC-Session), and the chat_sessions row the lane's
// turns are recorded on.
func ChatSessionKey(chatID string) string { return ChatSessionKeyPrefix + chatID }

// chatHolderSuffix is what a chat claim's holder adds to the instance name.
const chatHolderSuffix = "/chat"

// ChatHolder is the `instance` a chat item is claimed as on instance agent
// instance: never the instance's own name. HQ refuses a claim, heartbeat,
// release and complete from any holder but the claim's, so the primary
// (which acts as its agent name) can never release or complete a chat's
// claim, and a chat claim never reads, to the wake loop, as the primary's.
func ChatHolder(instance string) string { return instance + chatHolderSuffix }

// Chat-session sizing (`chat_sessions`).
const (
	// DefaultChatSessions is how many chat lanes an instance runs at once
	// when the officer declares none: ccd's own default for CCD_MAX_SESSIONS
	// (cmd/ccd defaultMaxLanes; TestTheChatSessionDefaultIsCcdsLaneDefault).
	DefaultChatSessions = 3
	// MaxChatSessions bounds `chat_sessions`. Each lane is a Claude Code CLI,
	// ~250-550 MiB, beside the primary in the instance pod's memory.
	MaxChatSessions = 5
)

// ChatSessions is how many chat lanes each of o's instances runs at once: its
// declared `chat_sessions`, or DefaultChatSessions.
func (o Officer) ChatSessions() int {
	if o.chatSessions > 0 {
		return o.chatSessions
	}
	return DefaultChatSessions
}

// DeclaredChatSessions is o's `chat_sessions` as declared, 0 when it declares
// none: what the instance pod's CCD_MAX_SESSIONS is set to (internal/api
// OfficerClaudeCodeSession) — 0 leaves the pod's spec, and ccd's default, as
// they were.
func (o Officer) DeclaredChatSessions() int { return o.chatSessions }

// WithChatSessions returns o declaring n chat sessions (tests, and wiring
// outside Parse).
func (o Officer) WithChatSessions(n int) Officer { o.chatSessions = n; return o }

// Chat-instance autoscaling (`chat_max_instances`, `chat_idle_stop`).
const (
	// DefaultChatMaxInstances is how many instances a pool may run for its
	// chats when the officer declares no `chat_max_instances`.
	DefaultChatMaxInstances = 3
	// MaxChatMaxInstances bounds `chat_max_instances`: an instance number is
	// never above MaxInstancesCap, whichever setting asks for it.
	MaxChatMaxInstances = MaxInstancesCap
	// DefaultChatIdleStop is how long an instance with no chat turn, no
	// claim and no prompt stays up before it is stopped (`chat_idle_stop`).
	DefaultChatIdleStop = 15 * time.Minute
	// MinChatIdleStop and MaxChatIdleStop bound `chat_idle_stop`.
	MinChatIdleStop = time.Minute
	MaxChatIdleStop = 24 * time.Hour
)

// ChatMaxInstances is the chat ceiling of pool o: chats run on its instances
// 1..ChatMaxInstances — its declared `chat_max_instances`, or
// DefaultChatMaxInstances.
func (o Officer) ChatMaxInstances() int {
	if o.chatMaxInstances > 0 {
		return o.chatMaxInstances
	}
	return DefaultChatMaxInstances
}

// WithChatMaxInstances returns o declaring a chat ceiling of n (tests, and
// wiring outside Parse).
func (o Officer) WithChatMaxInstances(n int) Officer { o.chatMaxInstances = n; return o }

// ChatIdleStop is how long an instance of o may go with no chat turn, claim
// or prompt before it is stopped: its declared `chat_idle_stop`, or
// DefaultChatIdleStop.
func (o Officer) ChatIdleStop() time.Duration {
	if o.chatIdleStop > 0 {
		return o.chatIdleStop
	}
	return DefaultChatIdleStop
}

// WithChatIdleStop returns o declaring a chat-idle stop of d (tests, and
// wiring outside Parse).
func (o Officer) WithChatIdleStop(d time.Duration) Officer { o.chatIdleStop = d; return o }

// Slots is how many instance numbers pool o has: 1..Slots. Queue work uses
// 1..MaxInstances of them.
func (o Officer) Slots() int { return o.MaxInstances }

// ChatOnly reports whether instance n of pool o serves chats only: numbered
// past max_instances, within the chat ceiling. Its primary takes no queue
// work (officer_claim refuses it).
func (o Officer) ChatOnly(n int) bool { return n > o.MaxInstances && n <= o.Slots() }

// ChatsPool reports whether pool o runs its officer's chats: an untiered
// officer, or a tiered officer's judgement pool. A routine pool never does —
// a chat has the officer's whole charter and access.
func (o Officer) ChatsPool() bool { return o.Tier != TierRoutine }

// --- the HQ routes ----------------------------------------------------------

// CapChats is the chat routes: an officer's chat messages and its reply.
const CapChats = "chats"

// ChatMessage is one message of a chat as HQ answers it.
type ChatMessage struct {
	Seq int64 `json:"seq"`
	// Role is "member", "officer" or "system".
	Role string `json:"role"`
	// AuthorLogin and AuthorName are who typed a member turn.
	AuthorLogin string `json:"author_login"`
	AuthorName  string `json:"author_name"`
	Text        string `json:"text"`
}

// The chat message roles.
const (
	ChatRoleMember  = "member"
	ChatRoleOfficer = "officer"
	ChatRoleSystem  = "system"
)

// ChatPage is a chat's messages after a seq, and the chat's name when HQ
// sends it ("" when not).
type ChatPage struct {
	Name     string
	Messages []ChatMessage
	// More: HQ left later messages out of this page; read again after the
	// last one's seq.
	More bool
}

// ChatsBackend is the chat routes.
type ChatsBackend interface {
	// ChatMessages is chat's messages with seq > after, ascending.
	ChatMessages(ctx context.Context, chat string, after int64) (ChatPage, error)
	// ChatReply posts text as the officer's reply to member seq inReplyTo and
	// answers the reply's seq. Idempotent on inReplyTo (HQ answers the seq of
	// the reply it already holds).
	ChatReply(ctx context.Context, chat, text string, inReplyTo int64) (int64, error)
}

func (c *Client) chatPath(chat string) string {
	return c.officerPath() + "/chats/" + url.PathEscape(chat)
}

// ChatMessages is GET /officers/{id}/chats/{chat}/messages?after=<seq>,
// answered {chat: {name, …}, messages: [{seq, role, author_login,
// author_name, text, in_reply_to?, at}], more, state}; muster reads the
// messages' seq, role, author_login, author_name and text, the chat's name and
// more.
func (c *Client) ChatMessages(ctx context.Context, chat string, after int64) (ChatPage, error) {
	raw, err := c.Do(ctx, http.MethodGet, c.chatPath(chat)+"/messages?after="+strconv.FormatInt(after, 10), nil)
	if err != nil {
		return ChatPage{}, err
	}
	return decodeChatPage(raw)
}

func decodeChatPage(raw []byte) (ChatPage, error) {
	var env struct {
		Messages []ChatMessage `json:"messages"`
		Chat     *struct {
			Name string `json:"name"`
		} `json:"chat"`
		More bool `json:"more"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return ChatPage{}, fmt.Errorf("%w: chat messages: %v", ErrHQUnavailable, err)
	}
	page := ChatPage{Messages: env.Messages, More: env.More}
	if env.Chat != nil {
		page.Name = env.Chat.Name
	}
	sort.SliceStable(page.Messages, func(i, j int) bool { return page.Messages[i].Seq < page.Messages[j].Seq })
	return page, nil
}

// ChatReply is POST /officers/{id}/chats/{chat}/reply {text, in_reply_to},
// answered {seq}.
func (c *Client) ChatReply(ctx context.Context, chat, text string, inReplyTo int64) (int64, error) {
	raw, err := c.Do(ctx, http.MethodPost, c.chatPath(chat)+"/reply", map[string]any{"text": text, "in_reply_to": inReplyTo})
	if err != nil {
		return 0, err
	}
	var out struct {
		Seq int64 `json:"seq"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, fmt.Errorf("%w: chat reply: %v", ErrHQUnavailable, err)
	}
	return out.Seq, nil
}

// --- the turn ---------------------------------------------------------------

// MaxChatReplyBytes is the HQ server's cap on an officer reply; muster cuts a
// longer one to it, ending with ChatReplyCutMarker, rather than have it
// refused.
const MaxChatReplyBytes = 64 << 10

// ChatReplyCutMarker ends a reply cut to MaxChatReplyBytes.
const ChatReplyCutMarker = "\n\n[reply cut by muster at 64 KiB]"

// capChatReply is the text muster posts for a lane's reply: without NULs
// (HQ refuses one), ChatNoTextReply for no text, and cut on a rune boundary
// to MaxChatReplyBytes, ending with ChatReplyCutMarker.
func capChatReply(reply string) string {
	text := strings.ReplaceAll(reply, "\x00", "")
	if strings.TrimSpace(text) == "" {
		return ChatNoTextReply
	}
	if len(text) <= MaxChatReplyBytes {
		return text
	}
	cut := MaxChatReplyBytes - len(ChatReplyCutMarker)
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + ChatReplyCutMarker
}

// ChatUndeliveredReply is the fallback posted in place of a reply HQ refused
// for good (chatReplyUndeliverable), naming HQ's refusal.
func ChatUndeliveredReply(err error) string {
	reason := "the HQ server refused it"
	var he *HQError
	if errors.As(err, &he) {
		reason = fmt.Sprintf("the HQ server answered %d", he.Status)
		if c := he.Code(); c != "" {
			reason += " " + c
		}
	}
	return "(The officer's reply could not be delivered: " + reason + ".)"
}

// chatReplyUndeliverable reports whether err is HQ refusing a reply's
// CONTENT (400, 413): the same reply would be refused every time.
func chatReplyUndeliverable(err error) bool {
	var he *HQError
	return errors.As(err, &he) && (he.Status == http.StatusBadRequest || he.Status == http.StatusRequestEntityTooLarge)
}

// ChatNoTextReply is the reply posted for a lane turn that ended with no text.
// Any other reply is posted as the lane wrote it: the HQ server caps it.
const ChatNoTextReply = "(The officer's turn ended without a text reply.)"

// ChatTurnText is the one user turn a chat lane is sent: the frame line, then
// each message muster has not given the lane yet, each under who wrote it.
//
//   - first: the lane's conversation is new, so it is told which officer and
//     instance it is and to load its charter (officer_brief) before answering;
//   - retry: the lane already has every member message (its last reply was
//     lost to a restart or a failed post): it is asked to answer again, and no
//     message is re-sent.
func ChatTurnText(o Officer, n int, chatName string, msgs []ChatMessage, first, retry bool) string {
	return chatTurnText(o, n, chatName, msgs, first, retry, false)
}

// ChatCharterReloadLine is added to a lane's turn when the officer's charter
// version moved since the lane's last turn.
const ChatCharterReloadLine = "Your charter changed since your last turn here: call " + ToolBrief + " again and follow the new charter before you answer.\n"

// chatTurnText is ChatTurnText, with reload adding ChatCharterReloadLine.
func chatTurnText(o Officer, n int, chatName string, msgs []ChatMessage, first, retry, reload bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "A team member is chatting with you in session %s; reply in plain text; this is not a queue item.\n", chatName)
	if reload {
		b.WriteString(ChatCharterReloadLine)
	}
	if first {
		fmt.Fprintf(&b, "You are officer %s (instance %s, %s). Call %s once to load your charter before you answer.\n",
			o.ID, o.InstanceLabel(n), o.InstanceName(n), ToolBrief)
	}
	if retry {
		b.WriteString("\nYour reply to the last message above did not reach the team member (muster lost it to a restart or a failed post). Reply to it again.\n")
	}
	for _, m := range msgs {
		b.WriteString("\n")
		switch m.Role {
		case ChatRoleMember:
			who := m.AuthorLogin
			if m.AuthorName != "" && m.AuthorName != m.AuthorLogin {
				who = fmt.Sprintf("%s (@%s)", m.AuthorName, m.AuthorLogin)
			} else if who != "" {
				who = "@" + who
			} else {
				who = "a team member"
			}
			fmt.Fprintf(&b, "%s wrote:\n%s\n", who, m.Text)
		default:
			fmt.Fprintf(&b, "[%s]\n%s\n", m.Role, m.Text)
		}
	}
	return b.String()
}

// ChatMarks is what a chat's lane has been given, persisted with its session
// row so a restart of muster, or an evicted lane, neither re-sends a message
// nor skips one.
type ChatMarks struct {
	// Seen is the highest chat seq the lane knows: the last member turn sent
	// to it, or its own reply once posted.
	Seen int64
	// Answered is the seq of the lane's last reply HQ took; Seen > Answered
	// is a turn whose reply never reached HQ.
	Answered int64
	// Charter is the officer's charter version as the lane last loaded it
	// ("" before its first turn): a different one on HQ's brief adds the
	// reload line to the lane's next turn.
	Charter string
}

// ErrChatLaneBroken is a chat turn refused before anything was typed for a
// reason that will not pass by itself (the runtime could not be resolved,
// refused muster's credential or the request, or was never reached): a
// failure, retried only after a backoff.
var ErrChatLaneBroken = errors.New("officers: the chat's lane cannot take a turn")

// ErrChatLaneUnavailable is a chat turn the lane refused before anything was
// typed (ccd's 409 busy, 503 not_ready or capacity): not a failure — the item
// waits for a later pass.
var ErrChatLaneUnavailable = errors.New("officers: the chat's lane could not take a turn now")

// ChatLanes is how the runner reaches the chat lanes of an officer's
// instances: internal/api implements it over the agents store and the agent
// gateway.
type ChatLanes interface {
	// Home is the instance number chatID's lane lives on (its session row's
	// agent), 0 when it has none yet. A lane's conversation is on its
	// instance's volume, so a chat never moves.
	Home(ctx context.Context, o Officer, chatID string) (int, error)
	// Open finds or makes chatID's lane session on instance n and answers its
	// marks.
	Open(ctx context.Context, o Officer, n int, chatID string) (ChatMarks, error)
	// Mark persists the marks of chatID's lane on instance n.
	Mark(ctx context.Context, o Officer, n int, chatID string, m ChatMarks) error
	// Supports reports whether instance n's runtime names a chat session's
	// MCP calls (its ccd advertises it): a chat runs on no other.
	Supports(ctx context.Context, o Officer, n int) (bool, error)
	// Turn sends text as one user turn to chatID's lane on instance n and
	// answers the lane's final assistant text. ErrChatLaneUnavailable (wrapped)
	// when the lane refused it before anything was typed.
	Turn(ctx context.Context, o Officer, n int, chatID, text string) (string, error)
	// Rehome sets aside chatID's lane session on instance from (stopped, in
	// error or past the chat ceiling), so its next Open on another instance
	// makes a new session there: one with no marks, which starts after the
	// officer's last reply. Its history stays readable where it was. Nothing
	// to set aside is no error.
	Rehome(ctx context.Context, o Officer, chatID string, from int) error
}

// ChatPlan is what pool o's chats need of its instances at one pass
// (ChatRunner.Want; Observation.ChatPlan). Decide reads it, purely.
type ChatPlan struct {
	// Need is how many more instances the waiting chats need: the chats no
	// up instance has a free lane for, ChatSessions to an instance, rounded
	// up. 0 while a lane is free.
	Need int
	// Prefer is the down instances to start first for Need: the homes of
	// chats whose instance is down (its conversation is on that instance's
	// volume), in the order the chats came.
	Prefer []int
	// Waiting is how many pending chats have no free lane now.
	Waiting int
	// Unplaced is how many pending chats have no up home: each goes to an
	// up instance with a free lane, or waits for one Need starts.
	Unplaced int
	// Busy is the chat turns running on each instance, by number.
	Busy map[int]int
	// PendingOn is the pending chats whose home is up, by its number.
	PendingOn map[int]int
	// ActiveAt is when each up instance was last seen active (a chat turn
	// started or ended on it, a live claim of its own or its chat holder's,
	// or first seen up by this process), by number.
	ActiveAt map[int]time.Time
}

// chatLeaseSeconds is a chat claim's lease; chatHeartbeatEvery renews it while
// the turn runs. A muster that dies mid-turn leaves the item claimable once
// the lease lapses.
const (
	chatLeaseSeconds   = 300
	chatHeartbeatEvery = time.Minute
)

// ChatRunner runs officers' chat turns (the package comment above). One per
// muster-server.
type ChatRunner struct {
	lanes   ChatLanes
	backend func(Officer) Backend
	log     *log.Logger
	// base is the context every turn runs under (shutdown cancels it).
	base context.Context
	// heartbeatEvery is chatHeartbeatEvery (tests shorten it).
	heartbeatEvery time.Duration
	// replyRetries are the waits between posts of a reply HQ did not take
	// for an outage (tests shorten them).
	replyRetries []time.Duration

	mu sync.Mutex
	// running is each chat this process is running a turn of, by
	// officer key + chat id, valued with the instance name.
	running map[string]string
	// lanesBusy is how many chat turns run on each instance, by agent name.
	lanesBusy map[string]int
	// backoff is each chat whose last turn failed (chatBackoff), by run key:
	// not run, and held back (Want), until its time.
	backoff map[string]chatBackoff
	// logged is each "this chat cannot run here" line already written, so a
	// chat that waits says why once, not every pass.
	logged map[string]bool
	now    func() time.Time
	wg     sync.WaitGroup
}

// chatBackoff is one failing chat's wait: its consecutive failures, and when
// it may run again.
type chatBackoff struct {
	failures int
	until    time.Time
}

// Backoff bounds of a failing chat: the first failure waits chatBackoffBase,
// each next one twice as long, at most chatBackoffMax.
const (
	chatBackoffBase = 30 * time.Second
	chatBackoffMax  = 10 * time.Minute
)

// settle records how chat key's turn ended: a failure (the lane broken or
// unsupported, a turn, read or reply that failed) backs it off; an answer
// clears it; anything else (not claimed, the lane busy) changes nothing.
func (r *ChatRunner) settle(key string, out ChatOutcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch out {
	case ChatReplied, ChatReplyDropped:
		delete(r.backoff, key)
	case ChatLaneBroken, ChatUnsupported, ChatTurnFailed, ChatReadFailed, ChatReplyFailed:
		b := r.backoff[key]
		b.failures++
		wait := chatBackoffBase
		for i := 1; i < b.failures && wait < chatBackoffMax; i++ {
			wait *= 2
		}
		if wait > chatBackoffMax {
			wait = chatBackoffMax
		}
		b.until = r.now().Add(wait)
		r.backoff[key] = b
	}
}

// backingOff reports whether chat key waits out a backoff at now.
func (r *ChatRunner) backingOff(key string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.backoff[key]
	return ok && now.Before(b.until)
}

// logOnce writes line once per key (per process).
func (r *ChatRunner) logOnce(key, line string) {
	r.mu.Lock()
	seen := r.logged[key]
	r.logged[key] = true
	r.mu.Unlock()
	if !seen {
		r.log.Print(line)
	}
}

// cannotRun is why pool o's instances can run no chat ("" when they can):
// a host instance has no ccd, so no lane.
func cannotRun(o Officer) string {
	if o.OnHost() {
		return "its instances run on a host, which has no ccd to run a chat session in"
	}
	return ""
}

// ChatRunnerConfig is NewChatRunner's input.
type ChatRunnerConfig struct {
	Lanes ChatLanes
	// Backend is each officer's backend (Backends.Backend).
	Backend func(Officer) Backend
	Logger  *log.Logger
	// Context bounds every turn; nil is context.Background().
	Context context.Context
	// HeartbeatEvery and ReplyRetries replace the defaults (tests).
	HeartbeatEvery time.Duration
	ReplyRetries   []time.Duration
	// Now is the clock a chat's backoff is read on; nil is time.Now.
	Now func() time.Time
}

// NewChatRunner returns a runner over cfg.
func NewChatRunner(cfg ChatRunnerConfig) (*ChatRunner, error) {
	if cfg.Lanes == nil || cfg.Backend == nil {
		return nil, errors.New("officers: a chat runner needs its lanes and the officers' backends")
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	if cfg.Context == nil {
		cfg.Context = context.Background()
	}
	if cfg.HeartbeatEvery <= 0 {
		cfg.HeartbeatEvery = chatHeartbeatEvery
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.ReplyRetries == nil {
		cfg.ReplyRetries = []time.Duration{2 * time.Second, 10 * time.Second, 30 * time.Second}
	}
	return &ChatRunner{lanes: cfg.Lanes, backend: cfg.Backend, log: cfg.Logger, base: cfg.Context, heartbeatEvery: cfg.HeartbeatEvery,
		replyRetries: cfg.ReplyRetries, running: map[string]string{}, lanesBusy: map[string]int{}, backoff: map[string]chatBackoff{},
		logged: map[string]bool{}, now: cfg.Now}, nil
}

// Wait blocks until every turn this runner started has ended.
func (r *ChatRunner) Wait() { r.wg.Wait() }

// On reports whether muster runs officer o's chats (Observation.Chats): a
// runner is wired and o's backend serves CapChats — for every pool of o, the
// routine pool too (it runs none, but must not take a chat item for queue
// work either). nil: off.
func (r *ChatRunner) On(o Officer) bool {
	if r == nil {
		return false
	}
	_, err := Serves[ChatsBackend](r.backend(o), CapChats)
	return err == nil
}

// serves is o's backend as a ChatsBackend, when it serves chats and o's pool
// runs them.
func (r *ChatRunner) serves(o Officer) (Backend, ChatsBackend, bool) {
	if r == nil || !o.ChatsPool() {
		return nil, nil, false
	}
	b := r.backend(o)
	cb, err := Serves[ChatsBackend](b, CapChats)
	if err != nil {
		return nil, nil, false
	}
	return b, cb, true
}

// PendingChats is the chat items of obs that a chat turn could claim at now:
// open, or claimed with a lapsed lease — less a blocked item or one not due
// (its claim is refused) and one that names no chat. Pure.
func PendingChats(obs Observation, now time.Time) []Item {
	var out []Item
	add := func(it Item) {
		if !it.IsChat() || it.Blocked || !it.Due(now) {
			return
		}
		if _, _, ok := ChatOf(it); ok {
			out = append(out, it)
		}
	}
	for _, it := range obs.Open {
		add(it)
	}
	for _, it := range obs.Claimed {
		if !it.LeaseLive(now) {
			add(it)
		}
	}
	return out
}

// Want is the instance numbers pool o must have up for its pending chats
// (Observation.ChatWant): each pending chat's home instance, and — for a chat
// with no home yet while no instance of o is up — instance 1; and the
// pending chats the pool cannot serve now (Observation.ChatHeldBack), which
// keep nothing up: every chat of a pool that runs none here (cannotRun), a
// chat whose home cannot be read or is beyond max_instances, and a chat
// waiting out a backoff. Nothing for a pool that runs no chats.
func (r *ChatRunner) Want(ctx context.Context, o Officer, obs Observation, now time.Time) (ChatPlan, map[ItemID]bool) {
	want, held := r.want(ctx, o, obs, now)
	return ChatPlan{Prefer: want, Need: len(want)}, held
}

func (r *ChatRunner) want(ctx context.Context, o Officer, obs Observation, now time.Time) ([]int, map[ItemID]bool) {
	if !r.On(o) || !o.ChatsPool() {
		return nil, nil
	}
	pending := PendingChats(obs, now)
	if len(pending) == 0 {
		return nil, nil
	}
	held := map[ItemID]bool{}
	if why := cannotRun(o); why != "" {
		for _, it := range pending {
			held[it.ID] = true
		}
		r.logOnce("cannot-run "+o.Key(), fmt.Sprintf("officers: %s: %d chat(s) wait and none is run: %s; they stay unclaimed", o.label(), len(pending), why))
		return nil, held
	}
	anyUp := false
	for _, in := range obs.Instances {
		if in.Up && !in.Error {
			anyUp = true
		}
	}
	want := map[int]bool{}
	for _, it := range pending {
		chat, _, _ := ChatOf(it)
		if r.backingOff(chatRunKey(o, chat), now) {
			held[it.ID] = true
			continue
		}
		n, err := r.lanes.Home(ctx, o, chat)
		switch {
		case err != nil:
			held[it.ID] = true
			r.log.Printf("officers: %s: chat %s: reading which instance runs it: %v (it waits for a later pass)", o.label(), chat, err)
		case n > o.MaxInstances:
			held[it.ID] = true
			r.logOnce("home "+chatRunKey(o, chat), fmt.Sprintf("officers: %s: chat %s lives on instance %s, beyond max_instances %d: it is not run until that instance is declared again",
				o.label(), chat, o.InstanceLabel(n), o.MaxInstances))
		case n > 0:
			want[n] = true
		case !anyUp:
			want[1] = true
		}
	}
	out := make([]int, 0, len(want))
	for n := range want {
		out = append(out, n)
	}
	sort.Ints(out)
	return out, held
}

func chatRunKey(o Officer, chat string) string { return o.Key() + "\x00" + chat }

// Dispatch starts a turn for every pending chat of pool o that this process
// is not already running, on its home instance — or, for a chat with no home
// yet, the up instance running the fewest chats (lowest number first) — when
// that instance is up, not in error, and has a free lane (o.ChatSessions()).
// A chat whose instance has no free lane is left UNCLAIMED for a later pass.
// It returns at once; each turn runs on its own goroutine.
func (r *ChatRunner) Dispatch(ctx context.Context, o Officer, obs Observation, now time.Time) {
	if _, _, ok := r.serves(o); !ok {
		return
	}
	if why := cannotRun(o); why != "" {
		if n := len(PendingChats(obs, now)); n > 0 {
			r.logOnce("cannot-run "+o.Key(), fmt.Sprintf("officers: %s: %d chat(s) wait and none is run: %s; they stay unclaimed", o.label(), n, why))
		}
		return
	}
	up := map[int]InstanceState{}
	for _, in := range obs.Instances {
		if in.Up && !in.Error && in.Exists {
			up[in.N] = in
		}
	}
	for _, it := range PendingChats(obs, now) {
		chat, _, _ := ChatOf(it)
		key := chatRunKey(o, chat)
		r.mu.Lock()
		_, busy := r.running[key]
		r.mu.Unlock()
		if busy || r.backingOff(key, now) {
			continue
		}
		home, err := r.lanes.Home(ctx, o, chat)
		if err != nil {
			r.log.Printf("officers: %s: chat %s: reading which instance runs it: %v (it waits for a later pass)", o.label(), chat, err)
			continue
		}
		n := home
		if n == 0 {
			n = r.leastBusy(o, up)
		}
		if _, isUp := up[n]; n == 0 || !isUp {
			continue // its instance is not up: Want starts it
		}
		name := o.InstanceName(n)
		r.mu.Lock()
		if _, again := r.running[key]; again || r.lanesBusy[name] >= o.ChatSessions() {
			r.mu.Unlock()
			continue // every lane busy: left unclaimed for a later pass
		}
		r.running[key] = name
		r.lanesBusy[name]++
		r.mu.Unlock()
		r.wg.Add(1)
		go func(it Item, chat string, n int) {
			defer r.wg.Done()
			defer r.done(key, name)
			defer func() {
				if rec := recover(); rec != nil {
					r.log.Printf("officers: %s: PANIC in chat %s's turn: %v", o.label(), chat, rec)
				}
			}()
			r.settle(key, r.Run(r.base, o, n, it))
		}(it, chat, n)
	}
}

// leastBusy is the up instance with the fewest chat turns running (lowest
// number first), 0 when none is up. Whether it has a free lane is Dispatch's
// one check.
func (r *ChatRunner) leastBusy(o Officer, up map[int]InstanceState) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	best, bestBusy := 0, 0
	for n := 1; n <= o.MaxInstances; n++ {
		if _, ok := up[n]; !ok {
			continue
		}
		busy := r.lanesBusy[o.InstanceName(n)]
		if best == 0 || busy < bestBusy {
			best, bestBusy = n, busy
		}
	}
	return best
}

func (r *ChatRunner) done(key, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.running, key)
	if r.lanesBusy[name]--; r.lanesBusy[name] <= 0 {
		delete(r.lanesBusy, name)
	}
}

// ChatOutcome is how one chat turn ended (Run), for logs and tests.
type ChatOutcome string

// The outcomes of Run.
const (
	ChatReplied     ChatOutcome = "replied"
	ChatNotClaimed  ChatOutcome = "not_claimed"
	ChatLaneBusy    ChatOutcome = "lane_unavailable"
	ChatTurnFailed  ChatOutcome = "turn_failed"
	ChatReplyFailed ChatOutcome = "reply_failed"
	// ChatReplyDropped: HQ refused the reply for good (409 chat_closed or
	// stale_turn): the turn is dropped, never retried.
	ChatReplyDropped ChatOutcome = "reply_dropped"
	// ChatLaneBroken: the lane refused the turn for a reason that does not
	// pass (ErrChatLaneBroken): released, and the chat backs off.
	ChatLaneBroken ChatOutcome = "lane_broken"
	// ChatUnsupported: the instance's runtime does not say it names a chat
	// session's MCP calls (ChatLanes.Supports): nothing claimed.
	ChatUnsupported ChatOutcome = "unsupported"
	ChatNothingNew  ChatOutcome = "nothing_new"
	ChatReadFailed  ChatOutcome = "read_failed"
	ChatNotServed   ChatOutcome = "not_served"
)

// Run is one chat turn of item it on instance n of pool o, start to end
// (claim, read, turn, reply), synchronously. Dispatch runs it on a goroutine;
// tests call it directly.
func (r *ChatRunner) Run(ctx context.Context, o Officer, n int, it Item) ChatOutcome {
	b, cb, ok := r.serves(o)
	if !ok {
		return ChatNotServed
	}
	chat, payloadSeq, _ := ChatOf(it)
	holder := ChatHolder(o.InstanceName(n))
	// 🔴 ONLY ON A RUNTIME THAT NAMES A CHAT SESSION'S CALLS: an older ccd
	// would run the lane with the pod's plain MCP config, and its calls would
	// read as the primary's. Nothing is claimed; the chat backs off.
	if ok, err := r.lanes.Supports(ctx, o, n); err != nil || !ok {
		why := "its ccd does not name a chat session's MCP calls (no mcp_session_lanes: an image from before chat sessions)"
		if err != nil {
			why = "its ccd could not be asked whether it names a chat session's MCP calls: " + err.Error()
		}
		r.logOnce("unsupported "+o.InstanceName(n)+" "+chat, fmt.Sprintf("officers: %s: chat %s is not run on instance %s: %s; it stays unclaimed", o.label(), chat, o.InstanceLabel(n), why))
		return ChatUnsupported
	}
	if _, err := b.Claim(ctx, it.ID, holder, chatLeaseSeconds, ""); err != nil {
		if hqStatus(err) != http.StatusConflict && hqStatus(err) != http.StatusGone {
			r.log.Printf("officers: %s: chat %s: claim item %s: %v (retried on a later pass)", o.label(), chat, it.ID, err)
		}
		return ChatNotClaimed
	}
	// The claim is ours from here: every path below ends it — the reply
	// completes the item; anything else releases it.
	hbCtx, stopHB := context.WithCancel(ctx)
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		t := time.NewTicker(r.heartbeatEvery)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				if _, err := b.Heartbeat(hbCtx, it.ID, holder, chatLeaseSeconds); err != nil && hbCtx.Err() == nil {
					r.log.Printf("officers: %s: chat %s: heartbeat item %s: %v", o.label(), chat, it.ID, err)
				}
			}
		}
	}()
	stop := func() { stopHB(); <-hbDone }
	release := func(why string) {
		stop()
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, err := b.Release(rctx, it.ID, holder); err != nil && hqStatus(err) != http.StatusConflict {
			r.log.Printf("officers: %s: chat %s: release item %s (%s): %v; it is claimable again once its lease lapses", o.label(), chat, it.ID, why, err)
		}
	}
	marks, err := r.lanes.Open(ctx, o, n, chat)
	if err != nil {
		r.log.Printf("officers: %s: chat %s: its session on instance %s: %v", o.label(), chat, o.InstanceLabel(n), err)
		release("no session")
		return ChatReadFailed
	}
	first := marks.Seen == 0 && marks.Answered == 0
	page, err := r.readChat(ctx, cb, chat, marks.Seen)
	if errors.Is(err, ErrHQAnswerTooLarge) && payloadSeq > 0 {
		// 🔴 A HISTORY OVER muster's ANSWER CAP NEVER WEDGES THE CHAT: the
		// turn the item names is read on its own (a re-homed chat whose
		// session is new reads from 0, which can be the whole history).
		r.log.Printf("officers: %s: chat %s: its messages after %d are over muster's answer cap; reading the item's own turn %d", o.label(), chat, marks.Seen, payloadSeq)
		page, err = r.readChat(ctx, cb, chat, payloadSeq-1)
	}
	if err != nil {
		r.log.Printf("officers: %s: chat %s: read its messages after %d: %v", o.label(), chat, marks.Seen, err)
		release("messages unreadable")
		return ChatReadFailed
	}
	// A lane with no marks (a new session: the chat's first turn, or a chat
	// re-homed onto another instance) starts after the officer's last reply:
	// what came before was answered already, and is not replayed.
	from := marks.Seen
	if first {
		for _, m := range page.Messages {
			if m.Role == ChatRoleOfficer && m.Seq > from {
				from = m.Seq
			}
		}
	}
	var fresh []ChatMessage
	var lastMember int64
	for _, m := range page.Messages {
		if m.Seq <= from || m.Role == ChatRoleOfficer {
			continue
		}
		fresh = append(fresh, m)
		if m.Role == ChatRoleMember && m.Seq > lastMember {
			lastMember = m.Seq
		}
	}
	name := page.Name
	if name == "" {
		name = chat
	}
	retry := false
	inReplyTo := lastMember
	switch {
	case lastMember > 0:
	case marks.Seen > marks.Answered:
		// The lane has every member turn; its reply never reached HQ.
		retry, inReplyTo = true, marks.Seen
	case payloadSeq > 0 && payloadSeq <= marks.Answered:
		// Already answered (the reply landed; HQ re-offered the item): post
		// it again — HQ answers the reply it holds, and completes the item.
		inReplyTo = payloadSeq
	default:
		r.log.Printf("officers: %s: chat %s: item %s has no member turn after seq %d to answer; released", o.label(), chat, it.ID, marks.Seen)
		release("nothing to answer")
		return ChatNothingNew
	}
	reply := ""
	if lastMember > 0 || retry {
		prev := marks
		next := marks
		if lastMember > next.Seen {
			next.Seen = lastMember
		}
		// The charter the lane works under: a version that moved since its
		// last turn asks it to load the charter again (its first turn always
		// does). An unreadable brief asks nothing and records nothing.
		charter := r.charterVersion(ctx, b)
		reload := !first && charter != "" && prev.Charter != "" && charter != prev.Charter
		if charter != "" {
			next.Charter = charter
		}
		// Marked SENT before the send: a muster that dies mid-turn must not
		// send the same messages again (it asks for the reply instead).
		if err := r.lanes.Mark(ctx, o, n, chat, next); err != nil {
			r.log.Printf("officers: %s: chat %s: record what its lane was sent: %v", o.label(), chat, err)
			release("marks unwritable")
			return ChatReadFailed
		}
		reply, err = r.lanes.Turn(ctx, o, n, chat, chatTurnText(o, n, name, fresh, first, retry, reload))
		if errors.Is(err, ErrChatLaneUnavailable) || errors.Is(err, ErrChatLaneBroken) {
			// Nothing was typed: the lane has not seen them after all.
			if merr := r.lanes.Mark(context.WithoutCancel(ctx), o, n, chat, prev); merr != nil {
				r.log.Printf("officers: %s: chat %s: restore its marks after a refused turn: %v", o.label(), chat, merr)
			}
			release("lane refused the turn")
			if errors.Is(err, ErrChatLaneBroken) {
				r.log.Printf("officers: %s: chat %s: instance %s's lane cannot take the turn: %v; it backs off", o.label(), chat, o.InstanceLabel(n), err)
				return ChatLaneBroken
			}
			r.log.Printf("officers: %s: chat %s: instance %s's lane did not take the turn: %v; a later pass tries again", o.label(), chat, o.InstanceLabel(n), err)
			return ChatLaneBusy
		}
		if err != nil {
			r.log.Printf("officers: %s: chat %s: its turn on instance %s failed: %v (released; the next claim asks the lane to answer again)", o.label(), chat, o.InstanceLabel(n), err)
			release("turn failed")
			return ChatTurnFailed
		}
		marks = next
	} else {
		reply = ChatNoTextReply
	}
	seq, err := r.postReply(ctx, cb, chat, capChatReply(reply), inReplyTo)
	switch {
	case err != nil && chatReplyFinal(err):
		// 🔴 FINAL FOR THIS TURN (409 chat_closed: the member closed the chat;
		// 409 stale_turn: a newer member turn is the one to answer). Dropped:
		// marked answered so no later claim asks the lane again, and the item
		// left to HQ — released, so a newer turn it carries is run next; an
		// item re-offered for a turn already answered is completed instead.
		r.dropTurn(ctx, o, n, chat, &marks)
		r.log.Printf("officers: %s: chat %s: HQ refused the reply to seq %d for good (%v); the turn is dropped", o.label(), chat, inReplyTo, err)
		if lastMember == 0 && !retry {
			stop()
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			_, cerr := b.Complete(cctx, it.ID, holder, Completion{Outcome: "no_action", Summary: "chat turn already answered; HQ refused a repeat reply: " + err.Error()})
			cancel()
			if cerr == nil {
				return ChatReplyDropped
			}
		}
		release("reply refused for good")
		return ChatReplyDropped
	case err != nil && chatReplyUndeliverable(err):
		// 🔴 HQ WILL NEVER TAKE THIS REPLY (400, 413): posting it again would
		// fail the same way every claim. The member is told, once, that the
		// reply could not be delivered and why, and the turn is answered.
		r.dropTurn(ctx, o, n, chat, &marks)
		r.log.Printf("officers: %s: chat %s: HQ refused the reply to seq %d (%v); posting the fallback", o.label(), chat, inReplyTo, err)
		if _, ferr := r.postReply(ctx, cb, chat, ChatUndeliveredReply(err), inReplyTo); ferr != nil {
			r.log.Printf("officers: %s: chat %s: the fallback reply: %v", o.label(), chat, ferr)
			release("reply undeliverable")
		} else {
			stop()
		}
		return ChatReplyDropped
	case err != nil:
		r.log.Printf("officers: %s: chat %s: post the reply to seq %d: %v (released; the next claim asks the lane to answer again)", o.label(), chat, inReplyTo, err)
		release("reply refused")
		return ChatReplyFailed
	}
	stop()
	if seq > marks.Seen {
		marks.Seen = seq
	}
	marks.Answered = marks.Seen
	if err := r.lanes.Mark(context.WithoutCancel(ctx), o, n, chat, marks); err != nil {
		r.log.Printf("officers: %s: chat %s: record the reply %d: %v", o.label(), chat, seq, err)
	}
	return ChatReplied
}

// readChat is chat's messages after `after`: every page HQ answers (more:
// true), at most maxChatPages.
func (r *ChatRunner) readChat(ctx context.Context, cb ChatsBackend, chat string, after int64) (ChatPage, error) {
	page := ChatPage{}
	for i := 0; ; i++ {
		p, err := cb.ChatMessages(ctx, chat, after)
		if err != nil {
			return ChatPage{}, err
		}
		if page.Name == "" {
			page.Name = p.Name
		}
		page.Messages = append(page.Messages, p.Messages...)
		if !p.More || len(p.Messages) == 0 || i+1 >= maxChatPages {
			return page, nil
		}
		after = p.Messages[len(p.Messages)-1].Seq
	}
}

// postReply posts text as the reply to inReplyTo, posting again after an
// outage (replyRetries).
func (r *ChatRunner) postReply(ctx context.Context, cb ChatsBackend, chat, text string, inReplyTo int64) (int64, error) {
	var seq int64
	var err error
	for attempt := 0; ; attempt++ {
		seq, err = cb.ChatReply(ctx, chat, text, inReplyTo)
		if err == nil || !errors.Is(err, ErrHQUnavailable) || attempt >= len(r.replyRetries) || ctx.Err() != nil {
			return seq, err
		}
		select {
		case <-ctx.Done():
		case <-time.After(r.replyRetries[attempt]):
		}
	}
}

// dropTurn marks chat's lane's turn answered: no later claim asks the lane
// to answer it again.
func (r *ChatRunner) dropTurn(ctx context.Context, o Officer, n int, chat string, marks *ChatMarks) {
	marks.Answered = marks.Seen
	if err := r.lanes.Mark(context.WithoutCancel(ctx), o, n, chat, *marks); err != nil {
		r.log.Printf("officers: %s: chat %s: record the dropped turn: %v", o.label(), chat, err)
	}
}

// charterVersion is the charter version HQ's brief answers for the officer
// (`type.charter_version`), "" when the brief cannot be read or names none.
func (r *ChatRunner) charterVersion(ctx context.Context, b Backend) string {
	raw, err := b.Brief(ctx)
	if err != nil {
		return ""
	}
	var brief struct {
		Type struct {
			CharterVersion json.RawMessage `json:"charter_version"`
		} `json:"type"`
	}
	if json.Unmarshal(raw, &brief) != nil || len(brief.Type.CharterVersion) == 0 || string(brief.Type.CharterVersion) == "null" {
		return ""
	}
	return strings.Trim(string(brief.Type.CharterVersion), `"`)
}

// maxChatPages bounds the messages pages one turn reads (ChatPage.More).
const maxChatPages = 50

// chatReplyFinal reports whether err is HQ refusing a reply for good: 409
// chat_closed or stale_turn. 409 not_claimed (the lease lapsed) is not: the
// next claim posts it again.
func chatReplyFinal(err error) bool {
	var he *HQError
	if !errors.As(err, &he) || he.Status != http.StatusConflict {
		return false
	}
	switch he.Code() {
	case "chat_closed", "stale_turn":
		return true
	}
	return false
}

// NonChat is items less the chat items: what the transcript forwarder and the
// LLM proxy's claim sync are told (Config.Observed). A chat claim is held as
// ChatHolder, which names no instance's primary session.
func NonChat(items []Item) []Item {
	out := make([]Item, 0, len(items))
	for _, it := range items {
		if !it.IsChat() {
			out = append(out, it)
		}
	}
	return out
}
