package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/redact"
	"github.com/ZacxDev/muster/internal/textplain"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE ATTENTION QUEUE: one place showing everything blocked on the owner.
//
// 🔴 THREE SOURCES, ONE READ MODEL, AND ONLY ONE OF THEM IS STORED.
//   - "ask": an agent's explicit question (attention_ask), stored in
//     attention_asks (internal/attention, migration 0025).
//   - "session": a mirrored Claude Code window whose session reports
//     mirror.StatusWaiting (a permission prompt, a question) — read LIVE from
//     the hub's window list, never copied anywhere.
//   - "plugin": a reply draft awaiting its owner's approval — read LIVE from
//     plugin_reply_drafts.
// attentionItems is the one function that merges them; the page, the badge,
// and the chief's attention_list all read it, so they cannot
// disagree about what is waiting.
//
// 🔴 A NOTIFICATION PER NEW ITEM, CLEARED BY TAG WHEN IT RESOLVES. An ask
// pushes when it is filed and sends the "resolved" control message (same tag)
// when it is answered, cancelled or expires. A session pushes when the hub's
// change nudge first shows it waiting (not on a host's first observation, nor
// within attentionRepushAfter of its last push) and clears when it stops. A plugin draft
// keeps its existing `plugin` push (it IS that item's notification; a second
// one would double-buzz), withdrawn when the draft is decided, superseded or
// expires (attention_drafts.go).
// ---------------------------------------------------------------------------

const (
	attentionPath = "/attention"
	// attentionSweepInterval is how often lapsed asks are expired.
	attentionSweepInterval = 30 * time.Second
	// attentionMaxWait bounds one attention_wait call (the long poll).
	attentionMaxWait = 10 * time.Minute
	// attentionDefaultWait is attention_wait's wait when it names none. Under
	// five minutes on purpose: Claude Code aborts an HTTP MCP call that has
	// answered nothing for CLAUDE_CODE_MCP_TOOL_IDLE_TIMEOUT (5 minutes by
	// default), and whether the keep-alive whitespace counts as an answer is not
	// documented.
	attentionDefaultWait = 4 * time.Minute
	// attentionPoll is how often a waiter re-reads its ask even without a
	// wake-up: another muster replica may have answered it.
	attentionPoll = 2 * time.Second
	// attentionPushBodyMax bounds a notification body (attentionPushBody), the
	// SAME bound on the browser and on a paired phone (push.MaxNativeBody, the
	// Android app's own cut), so the phone is sent exactly what it shows.
	attentionPushBodyMax = push.MaxNativeBody
	// attentionPushOptionsMax bounds the options line within it: the question
	// gets the rest (at least 99 characters).
	attentionPushOptionsMax = 100
	// attentionNotificationType is the push payload type for a new item.
	attentionNotificationType = "attention"
)

// Item kinds.
const (
	attentionKindAsk     = "ask"
	attentionKindSession = "session"
	attentionKindPlugin  = "plugin"
	// attentionKindQuestion is a session's held AskUserQuestion: answered on
	// the rendered view's question card.
	attentionKindQuestion = "question"
)

// attentionState is the queue's per-process state.
type attentionState struct {
	mu sync.Mutex
	// wakes wakes each OWNER's attention_wait calls when this process changes
	// one of that owner's asks — per owner, so a change for one owner does not
	// make every other owner's waiters re-read the database.
	wakes map[int64]*draftSignal
	// waits counts the attention_wait calls in flight, per asker and per owner
	// (attentionWaitSlot).
	waitsByAsker map[string]int
	waitsByOwner map[int64]int
	// waitsByAsk counts the attention_wait calls in flight per ask id: while
	// one waits on an ask, its answer is the waiter's and no turn is pushed
	// (attention_delivery.go).
	waitsByAsk map[int64]int
	// deliveries tracks the answer deliveries this process started, so a
	// shutdown (and a test) can wait for each to record its outcome;
	// deliveryBy names this process in a delivery's lease.
	deliveries     sync.WaitGroup
	deliveryByOnce sync.Once
	deliveryBy     string
	// graceOverride, when set (tests, before any delivery starts), replaces
	// attentionDeliveryGrace for this server.
	graceOverride *time.Duration
	// waiting is each owner's waiting sessions as last seen, by item key: what
	// a hub nudge is diffed against to push a new one and clear a gone one.
	waiting map[int64]map[string]attentionItem
	// seenHosts is each owner's hosts this process has already observed: a
	// host's FIRST observation seeds its waiting sessions without pushing, so a
	// restart (or the double rollout, or a host reconnecting) re-notifies none
	// — except a wait still inside its grace there, which is held as a new
	// one (diffHeld) and pushed once the grace has passed, if it still waits.
	seenHosts map[int64]map[string]bool
	// diffHeld is each owner's waiting items (by key) the diff is HOLDING:
	// new, not pushed, inside their grace (insideGrace, timed from the value:
	// the first nudge that saw it wait — or, on its host's first sight, its own
	// Since). Re-diffed on the attention sweep (recheckOfflineHosts) until each
	// is pushed or gone.
	diffHeld map[int64]map[string]time.Time
	// pushedAt is when each session item was last pushed: a session that goes
	// waiting again within attentionRepushAfter is not pushed again.
	pushedAt map[string]time.Time
	// episodeWaiting is each owner's covered waiting windows (by item key) as the
	// last hub nudge saw them, with session state on: only a window that
	// STARTED waiting since is looked up and pushed as an episode
	// (episodeWaitingPushes) — the push itself is claimed in the database, so
	// this only spares the reads. Each holds the window's episode key and place
	// as that nudge saw them: what the hook reads once the window stops waiting
	// (episodeWaitsAnswered) or loses its key (episodeHandedOver), when the
	// window itself can no longer name its episode.
	episodeWaiting map[int64]map[string]coveredWait
	// diffShown is each waiting item (by key) this diff PUSHED and has not
	// cleared: its notification is showing. A window that gains its session
	// key mid-wait is seeded as an episode only when it is (session_push.go).
	diffShown map[string]bool
	// drafts are the reply drafts whose `plugin` notification may be showing,
	// by draft id: what an expiry or a supersede clears (attention_drafts.go).
	drafts       map[int64]draftNote
	draftsSeeded bool
	// diffMu serialises attentionMirrorChanged's read-and-commit, so a
	// recompute that read an older hub state can never commit after a newer one.
	diffMu sync.Mutex
	// suggesting tracks the asks' suggestion generations this process started
	// (attention_suggest.go), so a test — and the shutdown, bounded — can wait.
	suggesting sync.WaitGroup
}

// attentionDiffHook, when set (tests), runs between a recompute's read of the
// hub and its commit.
var attentionDiffHook func()

// wakeFor is owner's wake-up signal.
func (st *attentionState) wakeFor(owner int64) *draftSignal {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.wakes == nil {
		st.wakes = map[int64]*draftSignal{}
	}
	w := st.wakes[owner]
	if w == nil {
		w = &draftSignal{}
		st.wakes[owner] = w
	}
	return w
}

// attentionItem is one entry of the unified read model.
type attentionItem struct {
	// ownerCard: a chief's owner card (attention.Ask.OwnerCard), which may be
	// a proposal's card (withAskBoxes looks).
	ownerCard bool
	// proposal is a chief proposal card's body (proposalKickoffBody: its
	// kickoff, read from the proposal row), drawn under the question; set by
	// withAskBoxes for the owner's pages only, nil everywhere else (a push,
	// attention_list), and not in the item's JSON.
	proposal *proposalBody
	// proposalCard: the ask page's ask is a chief proposal's card
	// (isProposalCard), whose answer is recorded and never delivered.
	proposalCard bool
	// noActions: its notification offers no chips and no signed answer
	// actions (a chief proposal's card, proposalCardFiled). Not serialised.
	noActions bool
	// Key is the item's stable id: its DOM id, its notification tag's suffix.
	Key  string `json:"key"`
	Kind string `json:"kind"`
	// Source names who is waiting: the asking agent or session, the window,
	// the plugin.
	Source string `json:"source"`
	// OfficerID is the built-in officer that raised the ask or notice (its
	// "<type>:<area>" id), "" otherwise: Source says it in words ("Product
	// officer (Acme)"), and a machine reader needs the id.
	OfficerID string `json:"officer_id,omitempty"`
	// target is the part of Source that names a mirrored window's tmux target
	// (mirror.WhereTarget), "" for none: every display plains Source with
	// textplain.TitleKeeping(Source, target), so the target reaches the
	// reader verbatim and only the label around it is plained.
	target string
	// at is the window as muster named it before sessions had names
	// (mirror.SessionView.Place, shown "mango:1 @ atlas"), set with target
	// from the same window; the zero Place for none. notifyName adds it after
	// a human name.
	at mirror.Place
	// windowName is the window's plain name as the session lists name it —
	// mirror.SessionView.DisplayTitle over the whole list (windowDisplayName):
	// "fern · window 2" when another window of that session also reads
	// "fern" — set with at; "" for none. attentionTitle heads an unnamed
	// window's card with it.
	windowName string
	// Text is the item's SUBSTANCE in one line or a few: the question; what a
	// waiting session waits on (its pending action, mirror.Pending.Summary —
	// sessionWaitingText when its host says nothing); a held AskUserQuestion's
	// questions; a reply draft's inbound message ("<sender>: <excerpt>").
	Text    string   `json:"text"`
	Options []string `json:"options,omitempty"`
	// Detail is the second line, "" for none: the last thing a waiting
	// session's Claude said (mirror.Pending.Last); a draft's reply excerpt.
	Detail string `json:"detail,omitempty"`
	// Focus is a mirrored session's focus title (internal/focus), "" for none.
	Focus string `json:"focus,omitempty"`
	// Name is the session's stable name (internal/sessionname) when it has a
	// HUMAN one, "" otherwise (set by withFocus): what the Asks list and a
	// push title call the session. Source stays its place — the window's
	// tmux target — which the item's own page and the push body keep. With its
	// owner's "Show tmux sessions" on, a mirrored session's
	// name is followed by its window's codename and host (notifyName).
	Name string `json:"name,omitempty"`
	// human is the session's HUMAN name alone, as Name was before notifyName
	// (withNamesFrom sets both), "" for none: what a codename-first push title
	// puts after the codename (attentionNotification, names.go codenameTitle),
	// so the codename is never said twice.
	human string
	// focusAt is when Focus was written (withFocus), for its age on the Asks
	// list (ui.FocusAgeOf); zero for none or unknown.
	focusAt time.Time
	// focusAgeFields is Focus's age (focus_set_at, focus_age) as attention_list
	// gives it to the chief, a notice's push body says it (attentionPushBody)
	// and the attention summary's why repeats it — set by withFocusAges on the
	// SERVER's clock (s.now), the one the chief's tools age titles by. Unset
	// where nothing ran withFocusAges.
	focusAgeFields
	// Since is when the item started waiting: an ask's or a draft's creation, a
	// session's status time (when it went waiting), a held question's creation.
	// The queue is NEWEST FIRST by it (sortAttentionItems).
	Since time.Time `json:"since"`
	// Link is where the owner acts on it.
	Link string `json:"link"`
	// AskID is the ask's id ("ask" and "notice" items: a notice is an
	// attention_asks row too, dismissed by this id — never answered).
	AskID int64 `json:"ask_id,omitempty"`
	// Target is a notice's mirrored window as tmux takes it,
	// "<session>:<window index>", resolved NOW over the owner's windows
	// (noticeItem); "" for any other item, and for a notice whose session no
	// window holds now. What `muster nudge next` prints.
	Target string `json:"target,omitempty"`
	// Offline: a session item whose host dropped within attentionOfflineGrace,
	// shown as last seen.
	Offline bool `json:"offline,omitempty"`
	// host is a session item's host (its first observation seeds silently);
	// claudeSession its Claude session id (its focus); tool the NAME of the
	// tool it waits on (mirror.Pending.Tool) — all a push may say of it.
	host, claudeSession, tool string
	// subject is the session the item is about (an asking agent or mirrored
	// session, a waiting window's Claude session); zero for a reply draft.
	// sessions_overview reads it.
	subject focus.Subject
	// card is the entry the item is counted on (its sidebar row's attention
	// count, cards_attention.go); zero for none.
	card attentionCard
	// openCode is a waiting OPENCODE window's ask (attention_opencode.go): a
	// permission request or a question, and the answers muster can relay; nil
	// for any other item.
	openCode *openCodeAsk
	// suggestions are an ask's stored suggested replies (attention.Ask's,
	// migration 0057), "" none: what its notification and its page draw as
	// chips. Not in the item's JSON (attention_list), which predates them.
	suggestions []string
	// pushText, when set, replaces Text in the notification body (muster's
	// own unrouted-message ask: unroutedPushText; a reply draft's
	// draftPushBody); the page shows Text.
	pushText string
	// stepUp is a step-up confirmation card's parts (attention.Ask.StepUp,
	// migration 0171), nil for every other ask and for a card filed before
	// 0171: its push title is its Title and its push body its why and the
	// start of its preview (stepUpPushText) — never its Details.
	stepUp *attention.StepUpCard
	// Context is what a builtin officer's ask is about (attention.Ask's,
	// migration 0129), nil for none: drawn apart from the question on the
	// card and the ask page (officerContextHTML), and in attention_list.
	Context *attention.ItemContext `json:"context,omitempty"`
	// Why is why an agent's ask is here (attention.Ask's, migration 0145):
	// its own context, or muster's fallback (focus, last); nil for none.
	// Attachments are the files it carries. Both are the asker's: drawn
	// escaped under the question (ask_media.go), and in attention_list.
	Why         *attention.AskWhy      `json:"why,omitempty"`
	Attachments []attention.Attachment `json:"attachments,omitempty"`
	// heldLast is a held AskUserQuestion's session's last words before the
	// question (its window's Pending.Last, as its host read them), "" for
	// none and for every other item: the fallback why its card draws at
	// render time (heldWhy). Not serialised — never in attention_list or the
	// summary API.
	heldLast string
	// choices are a held question's choice labels, listed in its push only
	// (never in attention_list's options).
	choices []string
	// dictate is the microphone path an ask's answer box carries
	// (Server.dictatePath), "" for no mic; sessionLink the ask card's "View in
	// session" target (attentionSessionPath), "" for no link. Both are set by
	// withAskBoxes for the pages that draw them, never by askItem: the item is
	// built where no server is in reach (a push, attention_list).
	dictate, sessionLink string
	// pageOpen is the owner's view of the page a share link's card asks about
	// (Server.pageAskOpen), "" for every other ask: the card's "Open the
	// page" button, so its question needs no address. Set by withAskBoxes and
	// the ask page, for the pages that draw cards; not serialised.
	pageOpen string
	// dictateOff: the server has no dictation, so the box draws the muted mic
	// linking to its Features ⓘ sheet (answerMic). Set with dictate.
	dictateOff bool
	// systemAsk: an ask muster raised itself (attention.Asker.System) — a task
	// ask, an unrouted message, a chief nudge. It names no session; a chief
	// nudge's is found through its decision (nudgeSessionPath).
	systemAsk bool
	// officerAsk: an ask muster raised for a builtin officer
	// (officerSystemAsk): its push opens officerDecisionCard's card.
	officerAsk bool
	// owner is an ask's owner (attention.Ask.Owner): the reader's own, or —
	// for a member — a TEAM's (team sharing 2b.3). Set by askItem; zero for
	// every other item.
	owner int64
	// watchOnly, when set, is why this ask is drawn UNANSWERABLE for the
	// reader (teamWatcherOf: a viewer of the team that owns it): no options,
	// no answer box, no Dismiss — this sentence in their place. Set by
	// withAskBoxes, for the pages that draw answer forms.
	watchOnly string
	// stepUpWhy, when set, says this ask's answer needs a FRESH sign-in from
	// the reader (teamCardStepUpWhy) and is the confirm dialog's sentence: its
	// answer form is drawn data-step-up-form. Set by withAskBoxes.
	stepUpWhy string
	// taskID is the task an ask is about (attention.Ask.TaskID), 0 for none:
	// its card's footer links to it (askFeedCardHTML). Set by askItem.
	taskID int64
}

// attentionCard is the entry an attention item belongs to: an agent's (an
// asking agent, a reply draft's drafting agent), a mirrored window by its
// address (a waiting window, a held question), or a mirrored Claude session
// on a host (an asking session), which the host's window list resolves to
// its window.
type attentionCard struct {
	agentID int64
	ref     ui.MirrorRef
	// claudeSession, with ref.Host: an asking mirrored session, by its Claude
	// session id.
	claudeSession string
}

// attentionPrincipal is ctx acting as owner, through authz.ViaAttention: how
// the hub's change nudge (which names only an owner) reads that owner's
// windows. internal/authz's withPrincipalLedger lists it.
//
// knownTeam (the session-state pass's team arm, #690 round 1, 🟡4) says the
// caller's own list names owner a TEAM: the team acting principal is built
// from that, with no account read — a failed read would build a member.
func (s *Server) attentionPrincipal(ctx context.Context, owner int64, knownTeam ...bool) context.Context {
	if len(knownTeam) > 0 && knownTeam[0] {
		return authz.WithPrincipal(ctx, teamActingPrincipal(owner, authz.ViaAttention))
	}
	return authz.WithPrincipal(ctx, s.principalFor(ctx, owner, authz.ViaAttention))
}

func attentionAskKey(id int64) string { return "ask-" + strconv.FormatInt(id, 10) }

func draftKey(id int64) string { return "draft-" + strconv.FormatInt(id, 10) }

// sessionKey is a waiting window's key: a hash of its address, so the key is
// DOM- and tag-safe whatever the host's names contain.
func sessionKey(host, session, window string) string {
	sum := sha256.Sum256([]byte(host + "\x00" + session + "\x00" + window))
	return "session-" + hex.EncodeToString(sum[:8])
}

// shownAskLabel is a's stored label as a surface shows it: a built-in
// officer's ask is stored under "officer <id>" (officerAskLabel, the key its
// page matches asks by) and SHOWN by what the officer is for, "Product officer
// (Acme)" (the jargon audit's U-24); any other label as stored.
func shownAskLabel(a attention.Ask) string {
	if id := officerAskID(a); id != "" {
		return ui.OfficerSourceName(id)
	}
	return a.Asker.Label
}

// officerAskID is the built-in officer that raised a, from its stored label
// ("officer <id>"), or "" when an officer did not. An item carries it as
// OfficerID, so a machine reader (the chief's attention_list) keeps the id
// its plain Source no longer spells.
func officerAskID(a attention.Ask) string {
	if id, ok := strings.CutPrefix(a.Asker.Label, officerAskLabel("")); ok && a.Asker.System {
		return id
	}
	return ""
}

// askerLabel is an ask's source by what it stored: its label (for a mirrored
// session, its name and host when it asked — no tmux target), else its ids.
// Every surface shows askSource, which falls back to this.
func askerLabel(a attention.Ask) string {
	if a.Asker.Label != "" {
		return shownAskLabel(a)
	}
	if a.Asker.AgentID > 0 {
		return "agent " + strconv.FormatInt(a.Asker.AgentID, 10)
	}
	return a.Asker.MirrorSession + " @ " + a.Asker.MirrorHost
}

// askNotShown follows an asking mirrored session's stored name when no
// window lists its Claude session now: muster says so rather than name a tmux
// target it cannot vouch for.
const askNotShown = " (not shown in any mirrored window now)"

// whereHostOffline follows a where whose host has dropped: the hub still
// lists its windows for a grace period, so the target is the last one known.
const whereHostOffline = " (host offline)"

// entryWhere is the words an attention surface names a resolved mirrored
// window by: mirror.Where, and whereHostOffline when its host has dropped and
// the hub still lists it.
//
// 🔴 ONE RULE, ONE PLACE for marking an offline-retained host: an ask's source
// (askSourceParts), a notice (noticeItem) and an episode's push title and
// "Stalled / waiting" row (episodeWho, through sessionKeyWhere) all take it
// from here.
func entryWhere(e mirror.SessionView) string {
	if !e.Online {
		return e.Where() + whereHostOffline
	}
	return e.Where()
}

// askSource is what an ask names as its source: for a mirrored session, the
// window running its Claude session NOW, by its exact tmux target
// (mirror.Where, resolved through claudeSessionEntry over hosts) — or its
// stored name and askNotShown when no window does; for every other asker (an
// agent, muster itself) askerLabel.
func askSource(a attention.Ask, hosts []mirror.HostView) string {
	src, _ := askSourceParts(a, hosts)
	return src
}

// askSourceParts is askSource and, when it names a window, that window's
// target (mirror.WhereTarget) — the part a display keeps verbatim
// (textplain.TitleKeeping); "" otherwise.
func askSourceParts(a attention.Ask, hosts []mirror.HostView) (src, target string) {
	src, target, _ = askSourceAt(a, hosts)
	return src, target
}

// askSourceAt is askSourceParts and the window's Place (attentionItem.at),
// the zero Place when it names no window.
func askSourceAt(a attention.Ask, hosts []mirror.HostView) (src, target string, at mirror.Place) {
	if a.Asker.MirrorSession == "" || a.Asker.AgentID > 0 || a.Asker.System {
		return askerLabel(a), "", mirror.Place{}
	}
	if e, ok := claudeSessionEntry(hosts, a.Asker.MirrorHost, a.Asker.MirrorSession); ok {
		return entryWhere(e), e.WhereTarget(), e.Place()
	}
	return askerLabel(a) + askNotShown, "", mirror.Place{}
}

// askNeedsHosts reports whether askItem needs the owner's windows for a
// (askSource): only a mirrored session's ask does.
func askNeedsHosts(a attention.Ask) bool {
	return a.Asker.MirrorSession != "" && a.Asker.AgentID == 0 && !a.Asker.System
}

// askHosts is ctx's principal's windows when a needs them (askNeedsHosts),
// else nil.
func (s *Server) askHosts(ctx context.Context, a attention.Ask) []mirror.HostView {
	if askNeedsHosts(a) && s.ext.Mirror != nil {
		return s.ext.Mirror.Hosts(ctx)
	}
	return nil
}

// askItemNow is askItem over ctx's principal's windows, read only when a
// needs them (askHosts).
func (s *Server) askItemNow(ctx context.Context, a attention.Ask) attentionItem {
	if a.Kind == attention.KindNotice {
		return s.noticeItemNow(ctx, a)
	}
	return askItem(a, s.askHosts(ctx, a)...)
}

// askItem is ask a as an attention item; hosts is its owner's windows, which
// a mirrored session's ask names its source from (askSource) — none given, it
// is named as no window showing it.
func askItem(a attention.Ask, hosts ...mirror.HostView) attentionItem {
	src, target, at := askSourceAt(a, hosts)
	it := attentionItem{Key: attentionAskKey(a.ID), Kind: attentionKindAsk, Source: src, OfficerID: officerAskID(a), target: target, at: at, windowName: windowDisplayName(hosts, at), Text: a.Question,
		Options: a.Options, Since: a.CreatedAt, Link: attentionPath + "#" + attentionAskKey(a.ID), AskID: a.ID, suggestions: a.Suggestions,
		ownerCard: a.OwnerCard, owner: a.Owner, taskID: a.TaskID, Why: a.Why, Attachments: a.Attachments}
	if a.Asker.System {
		it.systemAsk = true
		it.officerAsk = officerSystemAsk(a)
		// muster's own unrouted-message ask: a compact push (unroutedPushText).
		if t, ok := unroutedPushText(a.Question); ok {
			it.pushText = t
		}
		// A builtin officer's ask about an item a plugin message became:
		// officer, item, the note's first line (officerContextPushText).
		if a.Context != nil {
			it.Context = a.Context
			it.pushText = officerContextPushText(a.Context, a.Question)
		}
		// A step-up card: its title, why and preview (host_stepup_card.go).
		if a.StepUp != nil {
			it.stepUp = a.StepUp
			it.pushText = stepUpPushText(a.StepUp)
		}
	}
	switch {
	case a.Asker.AgentID > 0:
		it.subject = focus.AgentSubject(a.Asker.AgentID)
		it.card.agentID = a.Asker.AgentID
	case a.Asker.MirrorSession != "":
		it.subject = focus.MirrorSubject(a.Asker.MirrorSession)
		it.card.ref.Host, it.card.claudeSession = a.Asker.MirrorHost, a.Asker.MirrorSession
	}
	return it
}

// sessionWaitingText is a waiting session's text when its host says nothing
// of what it waits on (an older muster-mirror, a transcript it could not read):
// muster's own words — the prompt itself is in the session, one tap away.
const sessionWaitingText = "Waiting on you: a permission prompt or a question in the session."

// attentionOfflineGrace is how long a host that dropped keeps its waiting
// sessions in the queue, as last seen: a blip (a muster-mirror restart, a
// network hiccup) neither drops them from the view nor clears their
// notification — and when it comes back they are not pushed again. A host gone
// longer is gone from the queue and its notifications are cleared.
const attentionOfflineGrace = 5 * time.Minute

// attentionHostBelieved is THE ONE RULE for whose waiting windows the queue
// lists, read by the view and by the push diff alike (attentionMirrorChanged
// takes its items from waitingSessions), so the page and the phone cannot
// disagree: an online host that reports status (HostView.StatusReport), or a
// host that dropped less than attentionOfflineGrace ago (the hub keeps an
// offline host's statuses only when it reported them).
func attentionHostBelieved(h mirror.HostView, now time.Time) bool {
	if h.Online {
		return h.StatusReport
	}
	return !h.OfflineSince.IsZero() && now.Sub(h.OfflineSince) < attentionOfflineGrace
}

// waitingSessions is ctx's principal's mirrored windows whose Claude Code
// session reports StatusWaiting, in the hub's host order (Mirror.Hosts), for every host
// attentionHostBelieved believes.
func (s *Server) waitingSessions(ctx context.Context) []attentionItem {
	if s.ext.Mirror == nil {
		return nil
	}
	return s.waitingSessionsIn(ctx, s.ext.Mirror.Hosts(ctx))
}

// waitingSessionsIn is waitingSessions over hs, ctx's principal's hosts as
// one read of the hub: a caller that also shows the hosts (the attention
// summary) passes the SAME snapshot, so a host flapping between two reads
// cannot be online in one and offline in the other.
func (s *Server) waitingSessionsIn(ctx context.Context, hs []mirror.HostView) []attentionItem {
	if s.ext.Mirror == nil {
		return nil
	}
	// The open questions of a session's AskUserQuestion (internal/askq,
	// registered by the muster-mirror ask hook), by host and Claude session:
	// such a window is ONE item — the question, with its text — never a
	// "session waiting" item beside a question item (Claude Code reports the
	// window waiting while its picker shows).
	held := map[string]askq.Entry{}
	if owner := requestOwner(ctx); owner > 0 && s.asks != nil {
		for _, e := range s.asks.Pending(owner) {
			if e.Key.Source == askSourceMirror {
				held[e.Key.Scope+"\x00"+e.Key.Session] = e
			}
		}
	}
	type wait struct {
		since   int64
		q       *askq.Entry
		pending *mirror.Pending
		offline bool
		// openCode: an opencode window; answers: its host answers its asks
		// now.
		openCode, answers bool
	}
	now := s.now()
	waiting := map[string]wait{}
	for _, h := range hs {
		if !attentionHostBelieved(h, now) {
			continue
		}
		for _, w := range h.Windows {
			k := sessionKey(h.Host, w.Session, w.ID)
			if rt, ok := w.Spec(); ok && rt.PendingNamesAsk {
				// A waiting window of a runtime that names its asks
				// (opencode): what its host says it waits on (opencode's asks,
				// attention_opencode.go). Its Pending names the ask; a host
				// that names none (older) still lists it as waiting.
				if w.Status == mirror.StatusWaiting {
					waiting[k] = wait{since: w.StatusSince, pending: w.Pending, offline: !h.Online, openCode: true, answers: h.Online && h.AnswersOn(rt)}
				}
				continue
			}
			if !w.Claude {
				continue
			}
			if e, ok := held[h.Host+"\x00"+w.ClaudeSession]; ok && w.ClaudeSession != "" {
				// Its Pending (when the host reads one while the picker shows) is
				// kept for the card's why: the last words before the question.
				waiting[k] = wait{since: e.Created.UnixMilli(), q: &e, pending: w.Pending, offline: !h.Online}
			} else if w.Status == mirror.StatusWaiting {
				waiting[k] = wait{since: w.StatusSince, pending: w.Pending, offline: !h.Online}
			}
		}
	}
	var out []attentionItem
	for _, e := range mirror.Ordered(hs) {
		k := sessionKey(e.Host, e.Session, e.Window)
		wt, ok := waiting[k]
		if !ok {
			continue
		}
		ref := ui.MirrorRef{Host: e.Host, Session: e.Session, Window: e.Window}
		it := attentionItem{Key: k, Kind: attentionKindSession, Source: e.Where(), target: e.WhereTarget(), at: e.Place(), windowName: e.DisplayTitle(), Text: sessionWaitingText, host: e.Host,
			claudeSession: e.ClaudeSession, Offline: wt.offline, Link: ref.ViewPath(), card: attentionCard{ref: ref}}
		if e.ClaudeSession != "" {
			it.subject = focus.MirrorSubject(e.ClaudeSession)
		}
		if wt.openCode {
			s.openCodeItem(&it, e, wt.pending, wt.answers)
			if wt.since > 0 {
				it.Since = time.UnixMilli(wt.since)
			}
			out = append(out, it)
			continue
		}
		switch {
		case wt.q != nil:
			it.Kind, it.Text = attentionKindQuestion, heldQuestionText(*wt.q)
			if wt.pending != nil {
				it.heldLast = wt.pending.Last
			}
			// One question's choices are named in its PUSH (choices, not Options:
			// attention_list serialises Options as answers attention_answer takes,
			// and a held question is answered on its question card, not there);
			// several questions' are each their own.
			if len(wt.q.Questions) == 1 {
				for _, o := range wt.q.Questions[0].Options {
					if l := strings.TrimSpace(o.Label); l != "" {
						it.choices = append(it.choices, l)
					}
				}
			}
		case wt.pending != nil:
			// The host's words, already cleaned and capped on both sides
			// (mirror.CleanPending); the generic line when it names no action.
			if wt.pending.Summary != "" {
				it.Text = wt.pending.Summary
			}
			it.Detail, it.tool = wt.pending.Last, wt.pending.Tool
		}
		if wt.since > 0 {
			it.Since = time.UnixMilli(wt.since)
		}
		out = append(out, it)
	}
	// 🔴 NAMED HERE, NOT ONLY IN withFocus: a held question or a waiting
	// window is PUSHED from this list directly (the queue's diff, an
	// episode's ask), with no withFocus in between — so its push title
	// would name the window's place instead of the session's name.
	s.withNames(ctx, out)
	return out
}

// heldQuestionText is a held AskUserQuestion's text as an item shows it: its
// question, or — when it asks several — each question's first line, numbered,
// one per line (a push quotes the first line alone).
func heldQuestionText(e askq.Entry) string {
	switch len(e.Questions) {
	case 0:
		return sessionWaitingText
	case 1:
		return e.Questions[0].Question
	}
	lines := make([]string, 0, len(e.Questions))
	for i, q := range e.Questions {
		lines = append(lines, strconv.Itoa(i+1)+". "+mirror.OneLine(firstLine(strings.TrimSpace(q.Question)), heldQuestionLineMax))
	}
	return strings.Join(lines, "\n")
}

// heldQuestionLineMax bounds each line of a multi-question item.
const heldQuestionLineMax = 120

// Draft excerpts (pendingDraftItems): the inbound message's first
// draftInboundMax runes, the reply's first draftReplyMax.
const (
	draftInboundMax = 160
	draftReplyMax   = 160
)

// draftInboundText is a draft's inbound message as one line: who sent it and
// the start of what they wrote (its subject when the body is empty). A sender
// the plugin did not verify is followed by unverifiedSenderNote: this line
// reaches the chief too (attention_list), not only the Attention view.
func draftInboundText(m plugins.Message) string {
	body := m.Body
	if strings.TrimSpace(body) == "" {
		body = m.Subject
	}
	who := mirror.OneLine(m.Sender, 80)
	if who == "" {
		who = "unknown sender"
	}
	if !m.SenderVerified {
		who += " " + unverifiedSenderNote
	}
	return who + ": " + mirror.OneLine(body, draftInboundMax)
}

// pendingDraftItems is ctx's principal's reply drafts awaiting approval.
func (s *Server) pendingDraftItems(ctx context.Context) ([]attentionItem, error) {
	if s.ext.Plugins == nil {
		return nil, nil
	}
	cards, err := s.ext.Plugins.DraftCards(ctx, pluginDraftsShown)
	if err != nil {
		return nil, err
	}
	s.withDraftOfficerNames(cards)
	var out []attentionItem
	now := time.Now()
	// A follow or a like is on its outreach batch's ONE item (outreach_batch.go),
	// counted from the whole batch, not from this cut.
	out = append(out, batchAttentionItems(s.withBatches(ctx, nil), now)...)
	for _, c := range cards {
		if c.Draft.Status != plugins.DraftPending || !c.Draft.ExpiresAt.After(now) || c.Draft.AwaitingCheck() {
			// An unchecked draft is not the owner's to decide yet.
			continue
		}
		if c.Draft.IsOutbound() && plugins.IsBatchKind(c.Draft.Target().Kind) {
			continue
		}
		it := attentionItem{Key: draftKey(c.Draft.ID), Kind: attentionKindPlugin, Source: c.PluginLabel,
			Text: draftInboundText(c.Message), Detail: "Draft reply: " + mirror.OneLine(c.Draft.Body, draftReplyMax),
			Since: c.Draft.CreatedAt, Link: draftLink(c.Draft.ID), pushText: draftPushBody(c.Message, c.PluginLabel)}
		if c.Draft.IsOutbound() {
			it.Text, it.Detail = outboundAttentionText(c), "Draft: "+mirror.OneLine(c.Draft.Body, draftReplyMax)
		}
		// The agent that drafted it (none once that agent is deleted).
		if c.Draft.AgentID != nil {
			it.card.agentID = *c.Draft.AgentID
		}
		out = append(out, it)
	}
	return out, nil
}

// attentionItems is the unified read model for ctx's principal, NEWEST first
// (sortAttentionItems).
// A source that cannot be read is left out and named in the error; the rest
// are still returned.
func (s *Server) attentionItems(ctx context.Context) ([]attentionItem, error) {
	rd := s.readAsks(ctx)
	return s.attentionItemsWith(ctx, rd.hosts, rd.waiting)
}

// asksRead is ONE read of ctx's owner's hub for an Asks render (readAsks):
// the hosts and their waiting sessions (waitingSessionsIn over them). Every
// part of the list — its items (attentionItemsWith), which of them session
// state covers (pageAttentionItems), and the "Stalled / waiting" rows with
// their names and lines (episodeRowsHTML) — is drawn from this one snapshot,
// never a read of its own (TestTheAsksListReadsTheHostsOncePerRender). The
// zero value is a muster that mirrors nothing.
type asksRead struct {
	hosts   []mirror.HostView
	waiting []attentionItem
}

// readAsks is the one asksRead of an Asks render.
func (s *Server) readAsks(ctx context.Context) asksRead {
	if s.ext.Mirror == nil {
		return asksRead{}
	}
	hs := s.ext.Mirror.Hosts(ctx)
	return asksRead{hosts: hs, waiting: s.waitingSessionsIn(ctx, hs)}
}

// attentionItemsWith is attentionItems over one read of ctx's principal's
// hub already made (readAsks): hs, and waiting, waitingSessionsIn over hs. A
// caller that also shows the hosts passes the SAME read. waiting is copied
// in, never changed: an Asks render draws its episode rows from the same
// slice.
func (s *Server) attentionItemsWith(ctx context.Context, hs []mirror.HostView, waiting []attentionItem) ([]attentionItem, error) {
	var out []attentionItem
	var errs []error
	if s.ext.Attention != nil {
		asks, err := s.ext.Attention.Open(ctx)
		if err != nil {
			errs = append(errs, fmt.Errorf("asks: %w", err))
		}
		// The notices' sessions, in one read (attention_notice.go).
		notices, err := s.noticeDecisions(ctx, asks)
		if err != nil {
			errs = append(errs, fmt.Errorf("notices: %w", err))
		}
		for _, a := range asks {
			// A question to the chief is the chief's (chief_ask_tool.go), not
			// the owner's: the chief asks the owner with attention_ask if it must.
			if a.ToChief {
				continue
			}
			// A mirrored session's ask (and a notice's window) is named from
			// the SAME read of the hub as the waiting sessions below (hs): one
			// read per build.
			if a.Kind == attention.KindNotice {
				out = append(out, noticeItem(a, notices, hs))
				continue
			}
			out = append(out, askItem(a, hs...))
		}
	}
	out = append(out, waiting...)
	drafts, err := s.pendingDraftItems(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("reply drafts: %w", err))
	}
	out = append(out, drafts...)
	// Plugin pairing requests (plugin_pairing.go).
	pairs, err := s.pendingPairItems(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("pairing requests: %w", err))
	}
	out = append(out, pairs...)
	// Task events the chief has not taken (task_routing.go).
	tasks, err := s.unroutedTaskItems(ctx)
	if err != nil {
		errs = append(errs, fmt.Errorf("tasks to route: %w", err))
	}
	out = append(out, tasks...)
	sortAttentionItems(out)
	return out, errors.Join(errs...)
}

// sortAttentionItems orders items NEWEST FIRST across every source, by each
// item's own Since (an item with none last), ties broken by Key so the order
// never depends on which source was read first.
func sortAttentionItems(items []attentionItem) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if !a.Since.Equal(b.Since) {
			return a.Since.After(b.Since)
		}
		return a.Key < b.Key
	})
}

// withFocus sets each mirrored item's focus title (its Claude session's
// focus, internal/focus): read once, only when an item has a session, and
// only for the surfaces that show it (the page, the list, attention_list).
//
// A chief NOTICE already carries a title — the decision's snapshot of its
// session's focus (noticeItem) — and is given that focus's age when the
// session's stored focus still has that very title. A notice whose session has
// moved on to another title since shows its snapshot with no age: the stored
// time is the newer title's, not the snapshot's.
func (s *Server) withFocus(ctx context.Context, items []attentionItem) {
	need := false
	for _, it := range items {
		// Any item about a session needs the index too: for its stable name —
		// an agent's, and a mirrored session's own ask, which carries no
		// claudeSession of its own (its card does) and so was once titled by
		// its window's place whatever the session was called.
		need = need || it.claudeSession != "" || noticeFocus(it) || it.subject != (focus.Subject{})
	}
	if !need {
		return
	}
	fi := s.focusIndex(ctx)
	s.withNamesFrom(ctx, fi, items)
	for i := range items {
		if noticeFocus(items[i]) {
			if v, ok := fi[items[i].subject]; ok && v.Title == items[i].Focus {
				items[i].focusAt = v.UpdatedAt
			}
			continue
		}
		if v := fi.mirrorFocus(items[i].claudeSession); v != nil {
			items[i].Focus, items[i].focusAt = v.Title, v.UpdatedAt
		}
	}
}

// withNames sets each item's Name — the session's HUMAN stable name
// (humanName), "" when it has none — from ONE read of the stored names, only
// when an item names a session; an item already named is left alone.
func (s *Server) withNames(ctx context.Context, items []attentionItem) {
	for _, it := range items {
		if it.subject != (focus.Subject{}) && it.Name == "" {
			s.withNamesFrom(ctx, s.focusIndex(ctx), items)
			return
		}
	}
}

// withNamesFrom is withNames over an index the caller already read. A mirrored
// item's name carries its window's codename and host when ctx's principal turned
// on "Show tmux sessions" (notifyName; the preference read
// once, and only when an item has a human name to add it to).
func (s *Server) withNamesFrom(ctx context.Context, fi focusIndex, items []attentionItem) {
	tmux, read := false, false
	for i := range items {
		if items[i].subject != (focus.Subject{}) && items[i].Name == "" {
			n := s.humanName(ctx, fi, items[i].subject)
			if n != "" && !read {
				tmux, read = s.notifyTmuxOn(ctx, requestOwner(ctx)), true
			}
			items[i].Name, items[i].human = notifyName(n, items[i].at, tmux), n
		}
	}
}

// withFocusAges is withFocus, then each item's focus age at the server's clock
// (s.now): what attention_list, a notice's push and the attention summary
// carry (focusAgeFields). One focus read for all the items, as withFocus.
func (s *Server) withFocusAges(ctx context.Context, items []attentionItem) {
	s.withFocus(ctx, items)
	now := s.now()
	for i := range items {
		items[i].focusAgeFields = focusAgeFieldsOf(items[i].focusAt, now)
	}
}

// noticeFocus reports whether it is a chief notice that carries its session's
// focus title (noticeItem).
func noticeFocus(it attentionItem) bool {
	return it.Kind == attentionKindNotice && it.Focus != "" && it.subject.Valid() == nil
}

// attentionFocusHTML is an Asks list row's focus line: the title (plained),
// then its age when ui.FocusAgeOf shows one (" · 2d"), and muted when it is
// stale. The age is its own element, so the title's text is the title.
func attentionFocusHTML(title string, age ui.FocusAge) string {
	esc := html.EscapeString
	cls, stale, after := "break-words text-xs font-semibold text-fg2", "", ""
	if age.Stale {
		cls, stale = cls+" "+ui.FocusStaleClass, ` data-focus-stale`
	}
	if age.Text != "" {
		// Labelled: the card's header already ends in how long it has waited,
		// in the same "· 3m" shape; this one is how old the title is.
		after = `<span class="font-normal" data-focus-age title="` + esc(ui.FocusAgeTooltip(age)) + `" aria-label="` +
			esc(ui.FocusAgeTooltip(age)) + `">` + esc(ui.FocusAgeSuffix(age)) + `</span>`
	}
	return `<span class="` + cls + `"` + stale + `><span data-attention-focus>` + esc(title) + `</span>` + after + `</span>
`
}

// withAskBoxes sets what each ask's answer card draws besides its form (in
// place): the microphone (every ask, when dictation is on) and, with links,
// its "View in session" link (attentionSessionPath — the ask page's own rule,
// so the card and the page cannot disagree) — or, for a team's ask read by a
// viewer of the team, watchOnly in place of the answer controls. ctx is the
// reader's principal.
//
// links is false on a session page's asks block (attention_asker.go): those
// asks are that very session's, and a link to the page already open is noise.
func (s *Server) withAskBoxes(ctx context.Context, items []attentionItem, links bool) {
	owner := requestOwner(ctx)
	dict := s.dictatePath()
	for i := range items {
		if items[i].Kind == attentionKindNotice && links && owner > 0 {
			// A notice has no answer box (so no mic): only its session.
			items[i].sessionLink = s.attentionSessionPath(ctx, owner, items[i])
			continue
		}
		if items[i].Kind != attentionKindAsk {
			continue
		}
		// Its suggested replies are drawn (as the card's chips) only while the
		// switches that make them allow it — the ask page's own rule
		// (askShowsSuggestions).
		if len(items[i].suggestions) > 0 && (s.ext.Suggest == nil || s.ext.Suggest.Enabled(ctx, items[i].owner) != "") {
			items[i].suggestions = nil
		}
		// 🔴 A TEAM'S ASK, READ BY A VIEWER OF THE TEAM (team sharing 2b.3,
		// 2b.4): drawn unanswerable, as the session view draws a team host's
		// question cards for a viewer (handleMirrorAsks) — one predicate,
		// teamWatcherOf. Drawing only: the answer and the dismissal are the
		// operator's write role (row-level security) and a team's end card is
		// refused a viewer again on the tap (teamEndCardRefusal).
		if teamWatcherOf(ctx, items[i].owner) {
			items[i].watchOnly = TeamAskViewerNote
		} else {
			items[i].dictate, items[i].dictateOff = dict, s.dictateOff()
		}
		if items[i].ownerCard && owner > 0 {
			items[i].pageOpen = s.pageAskOpen(ctx, items[i].AskID)
			if a, err := s.ext.Attention.Get(ctx, items[i].AskID); err == nil {
				items[i].proposal = s.proposalKickoffBody(ctx, a)
				if items[i].watchOnly == "" {
					items[i].stepUpWhy = s.teamCardStepUpWhy(ctx, a)
				}
			} else {
				// The card's ask could not be read now: it may be a
				// proposal's, so it offers no accept (no start, no End it).
				items[i].proposal = proposalUnreadBody
			}
			items[i].Options = withoutProposalStarts(items[i].proposal, items[i].Options)
		}
		if links && owner > 0 {
			items[i].sessionLink = s.attentionSessionPath(ctx, owner, items[i])
		}
	}
}

// askViewInSession is the label of the ask page's link to the session an ask
// is about (attention_ask_page.go), for a mirrored session and an agent alike.
// The Asks list says no words for it: its card's head IS that link
// (attentionHeadHTML).
const askViewInSession = "View in session"

// --- changes: SSE, push, waiters ---------------------------------------------

// attentionChanged tells owner's open views the queue moved and wakes that
// owner's waiters in this process.
func (s *Server) attentionChanged(owner int64, key string) {
	if owner <= 0 {
		return
	}
	s.attention.wakeFor(owner).signal()
	// A TEAM's item reaches its members' open pages through their own event
	// streams, which subscribe to each of their teams (team sharing 2b.3,
	// streamOwners) — one event, no member read here.
	s.broadcastTo(owner, EventAttentionChanged, key)
}

// attentionNotify pushes one new item to owner's devices, under the
// `attention` toggle. goNotify keeps it off the router: the router has no
// attention card, and its notifications are its own. A tap opens
// attentionOpenURL, which resolves the item's session (or its place on the
// queue) when tapped: nothing is read here.
func (s *Server) attentionNotify(owner int64, it attentionItem) {
	s.goNotify(owner, push.EventAttention, attentionNotification(it, false), nil)
}

// attentionNotification is item it's notification: its title (WHO, and that
// it asks or waits: attentionPushTitle), its body (WHAT: attentionPushBody),
// its tag, its tap URL and — for an ask — the chips a paired phone draws
// (payload v2's `ask`: its id, the asker's options, the suggested replies).
// update marks a re-push of a notification already shown under the same tag
// (the suggestions arrived): payload v2 says so, so the app replaces it
// without alerting again.
func attentionNotification(it attentionItem, update bool) RouterNotification {
	keep := it.target
	if it.Name != "" {
		// The name is plain already (internal/sessionname): kept whole, so
		// the push's own title rule does not plain it a second time.
		keep = it.Name
	}
	url := attentionOpenURL(it.Key)
	if it.officerAsk {
		// An officer's question or escalation: its decision card
		// (officer_push.go). Read here, not at the filing, so a re-push
		// (repushAsk, the suggestions) opens the same card.
		url = officerDecisionCard(officerPushAsk, officerCardRef{ID: it.Key})
	}
	n := RouterNotification{Type: attentionNotificationType, ID: it.Key, Title: attentionPushTitle(it), Body: attentionPushBody(it),
		Tag: attentionTag(it.Key), Data: map[string]string{"url": url}, Update: update, TitleKeep: keep}
	n.CodenameTitle, n.CodenameKeep = attentionCodenameTitle(it)
	if it.Kind == attentionKindAsk && it.AskID > 0 && !it.noActions {
		n.Ask = &push.V2Ask{ID: it.AskID, Options: cleanChips(it.Options), Suggestions: cleanChips(it.suggestions)}
		// The signed ask actions are routed wherever the asks are
		// (device_actions.go): this item is one, so they are offered.
		n.AskActions = true
	}
	return n
}

// cleanChips is chips without any a secret scan would redact (redact.Secrets,
// the body's own rule): a chip is sent whole or not at all, and a redacted one
// would answer with text the asker never offered. nil for none.
func cleanChips(chips []string) []string {
	var out []string
	for _, c := range chips {
		if redact.Secrets(c) == c {
			out = append(out, c)
		}
	}
	return out
}

// sessionPushTitle is a waiting session's fixed title: its WHO and WHERE are
// in its body (attentionPushBody).
const sessionPushTitle = "A session is waiting on you"

// attentionPushTitle names WHO: "<asker> is asking" for an ask and a held
// question (the source is a title, plained once at the sink, goNotify); a
// waiting session's fixed sessionPushTitle.
func attentionPushTitle(it attentionItem) string {
	if it.stepUp != nil && it.Kind == attentionKindAsk {
		// A step-up card names what it asks to do ("Post to X?").
		return stepUpTitleText(it.stepUp)
	}
	switch it.Kind {
	case attentionKindSession:
		return sessionPushTitle
	case attentionKindNotice:
		// A notice asks nothing: it names the session (its stable name when
		// it has one, else where to look: sessionKeyWhere).
		return itemWho(it) + noticePushTitleSuffix
	}
	return itemWho(it) + askPushTitleSuffix
}

// stepUpPushText is step-up card c's push body before its cut: its why, its
// where line, then its preview (a post's text), on one line. Never its
// Details.
func stepUpPushText(c *attention.StepUpCard) string {
	parts := []string{c.Why}
	if c.About != "" {
		parts = append(parts, c.About)
	}
	if c.Preview != "" {
		parts = append(parts, c.Preview)
	}
	if l := c.MediaLine(); l != "" && c.Preview == "" {
		parts = append(parts, l)
	}
	return strings.Join(parts, " · ")
}

// askPushTitleSuffix follows the asker in an ask's or a held question's
// notification title.
const askPushTitleSuffix = " is asking"

// attentionCodenameTitle is item it's push title with its window's codename
// first (names.go codenameTitle), for an owner who turned on "Start
// notification titles with the tmux name": a waiting window's fixed title
// gains the window's codename@host in front ("mango:1@ws · A session is
// waiting on you"); an ask, a held question and a notice put it before the
// session's HUMAN name ("mango:1@ws · Billing migration is asking"), never
// before notifyName's decorated one. "" (the title as it is) for no window, or
// for an ask, question or notice with no human name.
func attentionCodenameTitle(it attentionItem) (title, keep string) {
	if it.stepUp != nil && it.Kind == attentionKindAsk {
		// A step-up card is muster's own: no window to name.
		return "", ""
	}
	switch it.Kind {
	case attentionKindSession:
		return codenameLed(sessionPushTitle, it.at)
	case attentionKindNotice:
		return codenameTitle(it.human, noticePushTitleSuffix, it.at)
	}
	return codenameTitle(it.human, askPushTitleSuffix, it.at)
}

// itemWho is who an item's push title names: the session's stable name when it
// has a human one (Name), else its Source.
func itemWho(it attentionItem) string {
	if it.Name != "" {
		return it.Name
	}
	return it.Source
}

// noticePushTitleSuffix follows a notice's place in its notification title.
const noticePushTitleSuffix = " may be waiting on you"

// attentionPushBody is an item's notification body.
//
// 🔴 FULL TEXT FOR A QUESTION, ON THE LOCK SCREEN TOO (operator decision "Full
// text always", which REPLACED the earlier "Summary only — WHO and WHERE, never
// WHAT" for asks and held questions):
//   - an ask: its question, then "Options: a · b · c" when the asker offered
//     any (the options line at most attentionPushOptionsMax, the question the
//     rest);
//   - a held question: its question (each question's first line, numbered,
//     when it asks several), and one question's choices.
//
// 🔴 A WAITING SESSION STAYS SUMMARY ONLY (operator decision): "Waiting: <tool
// name> · <window> @ <host>", or "Waiting: <window> @ <host>" when its host
// names no tool — never the command, path or URL its pending call carries,
// never what Claude said. Those are on the page. Its source is a TITLE (a
// window's name, often LLM-written), so it reads as the title rule makes it,
// here, before the cut; goNotify plains titles only, never bodies.
//
// Each line is one line (control characters and runs of space folded,
// mirror.OneLine) with anything shaped like a secret redacted
// (redact.Secrets); the whole is cut to attentionPushBodyMax on a character
// boundary with a visible "…" (push.CutClusters). The question is NOT run
// through the title rule: textplain drops a fenced code block whole, and the
// command a question asks about is exactly what must not vanish from it.
//
// Other notifications are unchanged: a reply draft's (draftPushBody) still
// says who and where only — and so does its body here, so this is every
// item's lock-screen text (the attention summary reads it,
// attention_summary.go): a draft item carries its draftPushBody in pushText,
// and one without (built elsewhere) says DraftPushBody, never its message.
func attentionPushBody(it attentionItem) string {
	var lines []string
	switch it.Kind {
	case attentionKindPlugin:
		if it.pushText != "" {
			return it.pushText
		}
		return DraftPushBody
	case attentionKindNotice:
		// The session's focus title, then the chief's message: no options
		// line, because a notice has none (and no buttons, payload v2's `ask`
		// is not sent for it: attentionNotification).
		var parts []string
		if title := pushLine(textplain.Title(it.Focus)); title != "" {
			parts = append(parts, noticePushFocusLine(title, ui.FocusAge{Text: it.Age}))
		}
		if l := pushLine(it.Text); l != "" {
			parts = append(parts, l)
		}
		return push.CutClusters(strings.Join(parts, "\n"), attentionPushBodyMax)
	case attentionKindSession:
		body := "Waiting: " + textplain.TitleKeeping(it.Source, it.target)
		if tool := mirror.OneLine(it.tool, mirror.MaxPendingTool); tool != "" {
			body = "Waiting: " + tool + " · " + textplain.TitleKeeping(it.Source, it.target)
		}
		return push.CutClusters(mirror.OneLine(body, 1<<20), attentionPushBodyMax)
	}
	if it.stepUp != nil && it.Kind == attentionKindAsk {
		// A step-up card: its why and the start of its preview, no options
		// line (Confirm and Refuse are the notification's own buttons).
		return push.CutClusters(pushLine(it.pushText), attentionPushBodyMax)
	}
	opts := ""
	listed := it.Options
	if it.Kind == attentionKindQuestion {
		listed = it.choices
	}
	if len(listed) > 0 {
		parts := make([]string, 0, len(listed))
		for _, o := range listed {
			if o = pushLine(o); o != "" {
				parts = append(parts, o)
			}
		}
		if len(parts) > 0 {
			opts = push.CutClusters("Options: "+strings.Join(parts, " · "), attentionPushOptionsMax)
		}
	}
	room := attentionPushBodyMax
	if opts != "" {
		room -= utf8.RuneCountInString(opts) + 1 // and its line break
	}
	if it.Kind == attentionKindAsk {
		// An ask's question is ONE line: its paragraphs run on.
		text := it.Text
		if it.pushText != "" {
			text = it.pushText
		}
		lines = append(lines, pushLine(text))
	} else {
		// A held question's several questions keep one line each.
		for _, l := range strings.Split(it.Text, "\n") {
			if l = pushLine(l); l != "" {
				lines = append(lines, l)
			}
		}
	}
	q := push.CutClusters(strings.Join(lines, "\n"), room)
	if opts == "" {
		return q
	}
	if q == "" {
		return opts
	}
	return q + "\n" + opts
}

// noticePushFocusLine is a notice's push line for its session's focus title
// (one line already, pushLine): the title, then its age (" · 2d",
// ui.FocusAgeSuffix) when ui.FocusAgeOf shows one — the Asks list's rule, so a
// title set within the day, like one the session has moved on from
// (withFocus), carries none and the line is the title alone.
//
// 🔴 THE AGE IS NEVER CUT, as in the dock and the palette: a line carrying an
// age is at most noticePushFocusMax runes, its title cut (with "…") to leave
// the age room. The body's own cut (attentionPushBodyMax) then only ever
// reaches the chief's message after it, which keeps the other half. A title
// with no age is sent as before, the body's cut alone bounding it.
func noticePushFocusLine(title string, age ui.FocusAge) string {
	suffix := ui.FocusAgeSuffix(age)
	if suffix == "" {
		return title
	}
	return push.CutClusters(title, noticePushFocusMax-utf8.RuneCountInString(suffix)) + suffix
}

// noticePushFocusMax bounds a notice push's focus line when it carries an age:
// half the body. (A summarizer's title is at most focus.TitleCapRunes, far
// under it; the bound is for a longer stored one.)
const noticePushFocusMax = attentionPushBodyMax / 2

// pushLine is s as one line of a notification: secrets redacted, control
// characters and runs of space folded (no length cut: the caller cuts on a
// character boundary).
func pushLine(s string) string {
	return mirror.OneLine(redact.Secrets(s), 1<<20)
}

// attentionClear closes an item's notification on owner's devices.
func (s *Server) attentionClear(owner int64, key string) {
	s.goNotify(owner, push.EventResolved, RouterNotification{Type: push.TypeResolved, ID: key, Tag: attentionTag(key)}, nil)
}

// attentionTag is an item's notification tag.
func attentionTag(key string) string { return "attention-" + key }

// askSettled is every path that moved an ask out of open: clear its
// notification, update the views, wake the waiters.
func (s *Server) askSettled(a attention.Ask) {
	s.attentionClear(a.Owner, attentionAskKey(a.ID))
	s.attentionChanged(a.Owner, attentionAskKey(a.ID))
}

// attentionRepushAfter: a session that goes waiting again within this long of
// its last push is not pushed again (waiting → busy → waiting is one episode).
const attentionRepushAfter = 2 * time.Minute

// AttentionMirrorChanged is attentionMirrorChanged for the mirror hub's
// waiting-set hook (mirror.Service.SetWaitingNotify; cmd/muster-server).
func (s *Server) AttentionMirrorChanged(owner int64) {
	if s == nil {
		return
	}
	s.attentionMirrorChanged(owner)
}

// attentionMirrorChanged diffs owner's waiting sessions against the last
// nudge's. A session that started waiting is pushed once it has waited out
// its GRACE (insideGrace, Server.waitingGrace: 60 s, the episode's own) — held
// until then (diffHeld), re-diffed on the attention sweep, and never pushed
// if it stopped waiting first — unless its host is observed for the first
// time in this process (seeded silently: a restart or a reconnect re-notifies
// nothing that was due before; a wait still inside its grace there is held
// like a new one) or it was pushed within attentionRepushAfter; one that
// stopped is cleared. Called on the hub's change nudge
// (BroadcastAgentsChangedFor).
func (s *Server) attentionMirrorChanged(owner int64) {
	if s.ext.Mirror == nil || owner <= 0 {
		return
	}
	s.attention.diffMu.Lock()
	defer s.attention.diffMu.Unlock()
	now := map[string]attentionItem{}
	hosts := map[string]bool{} // online now
	octx := s.attentionPrincipal(context.Background(), owner)
	hs := s.ext.Mirror.Hosts(octx)
	for _, h := range hs {
		if h.Online {
			hosts[h.Host] = true
		}
	}
	// The same nudges — a host connected, a window started waiting — are when
	// muster reads a waiting window's transcript for the AskUserQuestion it
	// shows open (mirror_ask_scan.go): a question outlives a restart.
	s.scanOpenAsks(owner, hs)
	waiting := s.waitingSessionsIn(octx, hs)
	// episodePushed: the items the episode path covered at the last nudge
	// (session state on) — handed to an episode then, so the diff does not
	// push them as new now — except one whose episode is still HELD
	// (episodeHandedOver).
	var episodePushed map[string]bool
	// A TEAM host's waiting window is an episode like a person's (team
	// sharing 2b.3): the state pass visits every team holding a mirror token
	// (sessionstate.PG.Owners' team arm), settles the episode and pushes it as
	// the team, and the push fan-out reaches the team's operators and
	// managers once each. (2b.2 kept a team's waits on the diff path until
	// that arm existed.)
	if s.sessionStateOn() {
		// 🔴 SESSION STATE ON: A WAITING WINDOW THAT SESSION STATE KEYS
		// (episodeCover) — WHATEVER IT WAITS ON, AN ANSWERABLE ASK TOO — IS AN
		// EPISODE, opened from here and pushed once per stop, claimed — held
		// for its grace and then pushed by the state pass (session_push.go,
		// EpisodeWaitingGrace) — and left out of this diff, which would push it
		// a second time and clear it the moment it stops waiting. Only a
		// window session state cannot key stays here, held by the same grace.
		kept, fresh, seed := waiting[:0:0], []attentionItem(nil), []attentionItem(nil)
		cur := map[string]coveredWait{}
		listed := map[string]bool{} // every item waiting now, covered or not
		s.attention.mu.Lock()
		if s.attention.episodeWaiting == nil {
			s.attention.episodeWaiting = map[int64]map[string]coveredWait{}
		}
		prev := s.attention.episodeWaiting[owner]
		for _, it := range waiting {
			listed[it.Key] = true
			key, _, w, ok := episodeCover(hs, it)
			if !ok {
				kept = append(kept, it)
				continue
			}
			if s.attention.diffShown[it.Key] {
				// 🔴 A WINDOW THAT GAINED ITS KEY MID-WAIT (its session id
				// arrived while it waited): today's path already PUSHED this
				// stop, and that notification is showing. It stays in today's
				// diff until it stops waiting — which closes that notification
				// as today — and its episode is SEEDED (claimed, nothing sent),
				// so neither path pushes it a second time. (One today's path
				// only SEEDED — a host's first sight, offline — was never
				// shown, so it is the episode's to push.)
				kept, seed = append(kept, it), append(seed, it)
				continue
			}
			cur[it.Key] = coveredWait{key: key, host: it.card.ref.Host, session: w.Session, window: w.ID}
			if _, was := prev[it.Key]; !was {
				fresh = append(fresh, it) // started waiting since the last nudge (or first seen by this process)
			}
		}
		// 🔴 A COVERED WINDOW THAT STOPPED WAITING (its prompt answered) ENDS
		// ITS STOP HERE, at once (episodeWaitsAnswered): left to the next pass, a
		// question asked right after it would join its episode and be timed from
		// ITS opening — pushed early, or at once, though the new question was
		// answered inside its own grace (audit round 1 F1).
		var left []coveredWait
		for k, cw := range prev {
			if _, still := cur[k]; !still && !listed[k] {
				left = append(left, cw)
			}
		}
		s.attention.episodeWaiting[owner] = cur
		s.attention.mu.Unlock()
		s.episodeWaitsAnswered(octx, hs, left)
		s.episodeWaitingPushes(octx, owner, fresh, hs, true)
		s.episodeWaitingPushes(octx, owner, seed, hs, false)
		// prev is never written again (cur replaced it), so it is read
		// unlocked here.
		episodePushed = s.episodeHandedOver(octx, kept, prev)
		waiting = kept
	}
	for _, it := range waiting {
		now[it.Key] = it
	}
	if attentionDiffHook != nil {
		attentionDiffHook()
	}
	at := s.now()
	var push []attentionItem
	var clear []string
	s.attention.mu.Lock()
	if s.attention.waiting == nil {
		s.attention.waiting = map[int64]map[string]attentionItem{}
		s.attention.seenHosts = map[int64]map[string]bool{}
		s.attention.pushedAt = map[string]time.Time{}
	}
	if s.attention.diffShown == nil {
		s.attention.diffShown = map[string]bool{}
	}
	if s.attention.diffHeld == nil {
		s.attention.diffHeld = map[int64]map[string]time.Time{}
	}
	held := s.attention.diffHeld[owner]
	if held == nil {
		held = map[string]time.Time{}
		s.attention.diffHeld[owner] = held
	}
	before := s.attention.waiting[owner]
	seen := s.attention.seenHosts[owner]
	if seen == nil {
		seen = map[string]bool{}
		s.attention.seenHosts[owner] = seen
	}
	// 🔴 A HOST THAT DROPPED (a blip, a restart of its muster-mirror) is not a
	// session that stopped waiting: waitingSessions keeps its items for
	// attentionOfflineGrace (attentionHostBelieved — the SAME rule the view
	// reads, so the page and the notification agree), so they are not cleared
	// here; and it is forgotten as seen, so when it comes back its waiting
	// sessions are seeded silently again. Past the grace they leave both.
	for h := range seen {
		if !hosts[h] {
			delete(seen, h)
		}
	}
	s.attention.waiting[owner] = now
	for k := range held {
		if _, ok := now[k]; !ok {
			delete(held, k) // stopped waiting inside its grace: never pushed
		}
	}
	for k, it := range now {
		from, holding := held[k]
		if _, ok := before[k]; ok && !holding {
			continue
		}
		if !holding {
			// A new wait's grace runs from now — the first nudge that saw it, as
			// an episode's runs from its opening.
			from = at
			if !seen[it.host] || episodePushed[k] {
				// The host's first observation: seed, do not push — and stamp it, so
				// the re-push window covers a seeded session too. Likewise a window
				// whose stop an EPISODE push covered at the last nudge: the stop was
				// pushed once already. 🔴 EXCEPT, on a first observation, a wait
				// on an online host that NO EARLIER PROCESS CAN HAVE PUSHED
				// (diffNeverPushed: its grace, by its own Since, ran out after this
				// process started — a restart, slow or not, or a reconnect, caught
				// it mid-hold, audit round 1 F2): it is held for the rest of its
				// grace, or pushed now if that has passed, once, if it still waits.
				// A wait with no Since (its age unknown), one whose grace ran out
				// before this process started (its push was due, and sent, then),
				// or one on an offline host is seeded, as before: a restart never
				// re-notifies it.
				since := it.Since
				if since.After(at) {
					since = at // a host clock ahead of this one: never a longer hold
				}
				if episodePushed[k] || it.Offline || !s.diffNeverPushed(since) {
					s.attention.pushedAt[k] = at
					continue
				}
				from = since
			}
		}
		// 🔴 THE HOLD (insideGrace — the episode's own rule): a new wait is
		// pushed only once it has waited out its grace, and only if it still
		// waits then; one answered inside it is never pushed. A held item on a
		// host that dropped stays held while it is listed, and pushes once the
		// grace has passed — a delay, never a silence.
		if insideGrace(at.Sub(from), s.waitingGrace) {
			held[k] = from
			continue
		}
		delete(held, k)
		// The re-push window is timed from when this wait STARTED (from), not
		// from its held push: waiting again within attentionRepushAfter of the
		// last push is the same episode, however long its grace held it.
		if last, ok := s.attention.pushedAt[k]; ok && from.Sub(last) < attentionRepushAfter {
			continue // the same episode, back again
		}
		s.attention.pushedAt[k] = at
		s.attention.diffShown[k] = true
		push = append(push, it)
	}
	moved := false // an item still waiting whose substance changed
	for k, old := range before {
		cur, ok := now[k]
		if !ok {
			delete(s.attention.diffShown, k)
			clear = append(clear, k)
		} else if cur.Text != old.Text || cur.Detail != old.Detail || cur.Offline != old.Offline {
			moved = true
		}
	}
	for h := range hosts {
		seen[h] = true
	}
	s.attention.mu.Unlock()
	for _, it := range push {
		s.attentionNotify(owner, it)
	}
	for _, k := range clear {
		s.attentionClear(owner, k)
	}
	if len(push) > 0 || len(clear) > 0 || len(now) != len(before) || moved {
		s.attentionChanged(owner, "")
	}
}

// diffNeverPushed reports whether a waiting item that started at since can
// have been pushed by no process before this one: its grace (insideGrace,
// timed from since) ran out at or after this process started, so the push it
// was held for was never due while an earlier process ran. False for a zero
// since (its age unknown). 🔴 THE DIFF'S PUSHES ARE NOT RECORDED ANYWHERE A
// RESTART CAN READ, so this is what a host's first sight trusts: a wait whose
// push fell due before this process started is taken as pushed then — and so
// is one whose push fell due while muster was DOWN (accepted: a restart never
// re-notifies, and nothing tells the two apart).
func (s *Server) diffNeverPushed(since time.Time) bool {
	return !since.IsZero() && !since.Add(s.waitingGrace).Before(s.startedAt)
}

// coveredWait is a waiting window session state covers (episodeCover) as a hub
// nudge saw it: its episode key and where it is.
type coveredWait struct {
	key, host, session, window string
}

// recheckOfflineHosts re-runs the diff for every owner with an item of a host
// that dropped — nothing nudges for a host that is gone, so without this its
// items would outlive attentionOfflineGrace on the owner's devices — and for
// every owner with an item the diff is HOLDING (diffHeld): nothing nudges for
// a window that just sits waiting, so this is what pushes it once its grace
// has passed. On the attention sweep.
func (s *Server) recheckOfflineHosts() {
	var owners []int64
	s.attention.mu.Lock()
	for owner, items := range s.attention.waiting {
		if len(s.attention.diffHeld[owner]) > 0 {
			owners = append(owners, owner)
			continue
		}
		for _, it := range items {
			if it.Offline {
				owners = append(owners, owner)
				break
			}
		}
	}
	s.attention.mu.Unlock()
	for _, o := range owners {
		s.attentionMirrorChanged(o)
	}
}

// RunAttentionSweep expires lapsed asks until ctx ends (interval <= 0: every
// 30s): each is cleared from its owner's devices and views, and its waiters
// woken. It also delivers every owed answer (deliverDueAnswers: after a
// restart, a busy agent, a retry). It also clears the notification of a reply draft that expired
// (expireDraftNotes), and puts in front of the owner every plugin message the
// chief has not acted on (sweepUnroutedMessages, unrouted.go). Started by
// cmd/muster-server beside the other background loops; it does nothing without
// a store.
func (s *Server) RunAttentionSweep(ctx context.Context, interval time.Duration) {
	if s.ext.Attention == nil {
		return
	}
	if interval <= 0 {
		interval = attentionSweepInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		s.expireAsks(ctx)
		// A dispatch card whose hold lapsed is withdrawn with it (hold_lapse.go).
		s.withdrawLapsedHoldCards(ctx)
		s.deliverDueAnswers(ctx)
		s.putDueQuestions(ctx)
		s.expireDraftNotes(ctx)
		s.lapseDraftChecks(ctx)
		s.recheckOfflineHosts()
		s.sweepUnroutedMessages(ctx)
		// The answer-attempt log's retention (ask_attempts.go): hourly.
		s.pruneAskAttempts(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// expireAsks is one sweep.
func (s *Server) expireAsks(ctx context.Context) {
	due, err := s.ext.Attention.ExpireDue(ctx)
	if err != nil {
		s.logger.Printf("attention: expire: %v", err)
		return
	}
	for _, a := range due {
		s.askSettled(a)
	}
}

// --- routes ------------------------------------------------------------------

func (s *Server) registerAttentionRoutes(mux Mux) {
	mux.HandleFunc("GET "+attentionPath, s.requireSession(s.handleAttentionPage))
	mux.HandleFunc("GET "+attentionPath+"/items", s.requireSession(s.handleAttentionItems))
	mux.HandleFunc("GET "+asksDraftsPath, s.requireSession(s.handleAsksDrafts))
	mux.HandleFunc("GET "+attentionAskerPath, s.requireSession(s.handleAttentionAsker))
	mux.HandleFunc("GET "+attentionOpenPath+"{key}", s.requireSession(s.handleAttentionOpen))
	mux.HandleFunc("GET "+askPagePrefix+"{id}", s.requireSession(s.handleAskPage))
	mux.HandleFunc("GET "+ui.AttentionBadgePath, s.requireSession(s.handleAttentionBadge))
	// What a status bar polls (attention_summary.go): a PERSONAL token only, so
	// it reads as that token's owner. It is the one route a bar-summary token
	// (apitokens.ScopeSummary) reaches (tokenScopeRoutes).
	mux.HandleFunc("GET "+attentionSummaryPath, s.requireAPIToken(s.handleAttentionSummary))
	mux.HandleFunc("POST "+attentionPath+"/asks/{id}/answer", s.requireOperatorWrite(s.handleAttentionAnswerForm))
	mux.HandleFunc("POST "+attentionPath+"/asks/{id}/cancel", s.requireOperatorWrite(s.handleAttentionCancelForm))
	mux.HandleFunc("POST "+attentionPath+"/asks/{id}/redeliver", s.requireOperatorWrite(s.handleAttentionRedeliverForm))
	mux.HandleFunc("POST "+attentionOpenCodePath, s.requireOperatorWrite(s.handleAttentionOpenCodeAnswer))
}

// handleAttentionPage is GET /attention, the Asks page (asks_drafts.go):
// served when the queue or plugins (its Reply drafts tab) is.
func (s *Server) handleAttentionPage(w http.ResponseWriter, r *http.Request) {
	if s.ext.Attention == nil && s.ext.Plugins == nil && !s.chiefNudgesOn() {
		http.NotFound(w, r)
		return
	}
	// The same URL answers a navigation and the search box's live request
	// differently (asksSearchLive).
	w.Header().Add("Vary", "HX-Request, HX-Target")
	if asksSearchLive(r) {
		s.writeAsksSearchLive(w, r)
		return
	}
	s.renderAttentionPage(w, r, http.StatusOK, "")
}

// renderAttentionPage is the Asks page on the tab the request names.
func (s *Server) renderAttentionPage(w http.ResponseWriter, r *http.Request, code int, problem string) {
	s.renderAsksPage(w, r, code, problem, asksTab(r), asksFocus(r))
}

// renderAsksPage writes the Asks page on tab: with plugins served, the Ask
// and Reply drafts tabs (asks_drafts.go); without, the Ask list alone. focus
// (0 for none) is a draft drawn whatever the views' cut (readDraftViews).
func (s *Server) renderAsksPage(w http.ResponseWriter, r *http.Request, code int, problem, tab string, focus int64) {
	// THE SEARCH (asks_search.go): ?q= on the page itself. Submitted (Enter,
	// or a link), an id that finds exactly one thing goes straight to it;
	// anything else lists its results where the tabs would be. (The box's
	// live request never reaches here: handleAttentionPage answers it.)
	searchRaw, search, searching := asksSearchRequest(r)
	var found asksSearchResult
	if searching && r.URL.Path == attentionPath {
		found = s.searchAsks(r.Context(), search)
		// Not when htmx restores a history entry it holds no copy of (Back to
		// an address the typing set): Back draws the list it left, not the ask.
		if to := asksSearchJump(search, found); to != "" && code == http.StatusOK && problem == "" && r.Header.Get("HX-History-Restore-Request") != "true" {
			http.Redirect(w, r, to, http.StatusSeeOther)
			return
		}
	} else {
		searching = false
	}
	rd := s.readAsks(r.Context()) // the render's one hub read (asksRead)
	items, err := s.attentionItemsWith(r.Context(), rd.hosts, rd.waiting)
	s.withFocus(r.Context(), items)
	s.withAskBoxes(r.Context(), items, true)
	if err != nil {
		s.logger.Printf("attention: page: %v", err)
		if problem == "" {
			problem = "Part of the queue could not be read."
		}
	}
	var b strings.Builder
	now := s.now() // the server's clock: a focus title's age is the chief's and the push's (FocusAgeOf)
	// A session page's asks badge lands here filtered to that session's asks
	// (attention_asker.go, ?from=): the Open list is those asks alone.
	flt, filtered := s.asksFilter(r.Context(), r.URL.Query(), rd.hosts)
	var feed string
	var open int
	if filtered {
		// The count is THE ONE OPEN COUNT's rule over the filtered asks
		// (asksPendingOf: what waits on the reader — a team's ask a viewer
		// watches is listed, not counted); no drafts or stalled rows here.
		mine := flt.only(items, rd.hosts)
		feed, _ = askFeedHTML(mine, "", 0, now)
		open = asksPendingOf(r.Context(), mine, 0)
	} else {
		feed, open = s.askFeedNow(r.Context(), items, rd, now)
	}
	// THE CARD FEED (asks_feed.go): the heading and the open count, then the
	// tabs, each a feed of cards.
	b.WriteString(`<section class="flex flex-col gap-3" data-testid="attention-page">
<div class="flex items-center gap-2 px-1"><h1 class="text-balance text-base font-semibold text-fg">Asks</h1>` + ui.AsksOpenCountHTML(open) +
		// THE SEARCH BOX ends the heading's row (asks_search.go): on it, not
		// under it, so the feed starts where it did but for a few pixels.
		ui.AsksSearchFormHTML(attentionPath, searchRaw) + `</div>
`)
	b.WriteString(problemNode(problem))
	// THE RESULTS REGION, then THE PAGE (tabs and what goes over them), both
	// always drawn: while the region holds results the stylesheet hides the
	// page, so the search as you type can empty it and the page is back, live,
	// with no reload (ui/asks_search.go).
	results := ""
	if searching {
		results = asksSearchHTML(searchRaw, found, now)
	}
	b.WriteString(ui.AsksSearchRegionHTML(results))
	b.WriteString(`<div class="` + ui.AsksMainClass + `" data-asks-main>`)
	if filtered {
		b.WriteString(ui.AsksFilterHTML(flt.label, attentionPath))
	}
	b.WriteString(attentionNotice(r))
	// The feed's notice: a card post refused after the card left the page.
	b.WriteString(ui.AsksFeedNoticeHTML)
	askList := `<ul id="attention-items" class="` + ui.AsksFeedClass + `" data-testid="attention-items" data-asks-feed aria-live="polite">` + feed + `</ul>`
	answered := `<ul id="attention-answered" class="` + ui.AsksFeedClass + `" data-testid="attention-answered" data-asks-feed>` + s.answeredAsksNow(r.Context(), now) + `</ul>`
	// ALWAYS TABS (Open and Answered at least). The Log tab
	// (ask_log_read.go) is drawn only with the ask log on.
	var pending, archived []plugins.DraftCard
	if s.ext.Plugins != nil {
		var ok bool
		pending, archived, ok = s.readDraftViews(r.Context(), focus)
		if !ok && problem == "" {
			b.WriteString(problemNode("The reply drafts could not be read."))
		}
	}
	nudges := ""
	if s.chiefNudgesOn() {
		nudges = s.chiefNudgesPanelHTML(r.Context(), now)
	}
	// The log is read only when the page is opened ON its tab; otherwise the
	// panel is a placeholder the page's script fills when the tab is opened.
	logPanel := ""
	switch {
	case s.askLogStore() == nil:
		// The ask log off: no Log tab (asksTabsHTML), and ?tab=log opens Open.
	case tab == asksTabLog:
		logPanel = s.askLogPanelHTML(r.Context(), r.URL.Query())
	default:
		logPanel = askLogLazyHTML(r.URL.Query())
	}
	b.WriteString(s.asksTabsHTML(askList, answered, s.ext.Plugins != nil, pending, archived, nudges, logPanel, tab))
	b.WriteString(`</div>
</section>
<template id="asks-empty-tpl">` + ui.AsksAllClearHTML + `</template>
<template id="asks-drafts-wait-tpl">` + ui.AsksDraftsWaitHTML(2) + `</template>
`)
	if filtered {
		// The live list keeps the filter.
		b.WriteString(ui.AttentionLiveScript("attention-items", "/attention/items?"+flt.ref.query(), EventAttentionChanged))
	} else {
		b.WriteString(attentionPageScript)
	}
	// The answer boxes' mic (once, after the list it reveals; the live
	// refresh's new boxes are shown by ui.DictateRevealEvent).
	b.WriteString(ui.DictateScriptHTML(s.dictatePath()))
	b.WriteString(asksPageScript)
	b.WriteString(ui.AsksFeedScriptHTML())
	// The cards' id chips (ui/ask_id.go): plain text until this runs.
	b.WriteString(ui.IDCopyScriptHTML())
	// The feed area's tab swipe (ui/tab_swipe.go, the session panel's too).
	b.WriteString(ui.TabSwipeScriptHTML())
	// The search box's script (ui/asks_search.go).
	b.WriteString(ui.AsksSearchScriptHTML())
	// The confirm dialog, ALWAYS: a gated form can arrive with the list's live
	// refresh (a team's card), not only with the drafts tab. Its default
	// sentence is the drafts'; a team card's form carries its own.
	b.WriteString(s.draftStepUpHTML(r))
	title := "Asks"
	if searching {
		title = "Search asks"
	}
	// The Asks page is a page of cards: the wide column on a desktop.
	s.writeNavDocument(w, r, code, title, ui.NavAttention, false, b.String(), navWide)
}

// attentionPageScript keeps the queue's list live (ui.AttentionLiveScript).
var attentionPageScript = ui.AttentionLiveScript("attention-items", "/attention/items", EventAttentionChanged)

func (s *Server) handleAttentionItems(w http.ResponseWriter, r *http.Request) {
	if s.ext.Attention == nil && s.ext.Plugins == nil {
		http.NotFound(w, r)
		return
	}
	if r.URL.Query().Get("part") == "answered" {
		s.handleAttentionAnswered(w, r)
		return
	}
	rd := s.readAsks(r.Context()) // the refresh's one hub read (asksRead)
	items, err := s.attentionItemsWith(r.Context(), rd.hosts, rd.waiting)
	if err != nil {
		s.logger.Printf("attention: items: %v", err)
	}
	s.withFocus(r.Context(), items)
	s.withAskBoxes(r.Context(), items, true)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	now := s.now() // as renderAsksPage
	// The filtered page's live list (?from=, renderAsksPage): the same filter.
	if flt, ok := s.asksFilter(r.Context(), r.URL.Query(), rd.hosts); ok {
		feed, _ := askFeedHTML(flt.only(items, rd.hosts), "", 0, now)
		_, _ = w.Write([]byte(feed))
		return
	}
	_, _ = w.Write([]byte(s.askListHTML(r.Context(), items, rd, now)))
}

// handleAttentionAnswered is GET /attention/items?part=answered: the Answered
// tab's feed, for the page's live refresh (asksPageScript).
func (s *Server) handleAttentionAnswered(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(s.answeredAsksNow(r.Context(), s.now())))
}

func (s *Server) handleAttentionBadge(w http.ResponseWriter, r *http.Request) {
	if s.ext.Attention == nil {
		http.NotFound(w, r)
		return
	}
	// THE ONE OPEN COUNT (asks_count.go): the page's "N open", the ☰ badge
	// and the Log's Open pill show the same number.
	n, err := s.asksPendingCount(r.Context())
	if err != nil {
		s.logger.Printf("attention: badge: %v", err)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(ui.AttentionBadgeHTML(n)))
}

// waitingOnReader is items without the asks ctx's person only WATCHES — a
// team's ask read by a viewer of the team (teamWatcherOf, the rule that draws
// it unanswerable): what the Asks badge and the sidebar's counts count. Such
// an ask is still LISTED (with why it cannot be answered); it is not waiting
// on this reader, so it is not counted as waiting on them.
func waitingOnReader(ctx context.Context, items []attentionItem) []attentionItem {
	out := items[:0:0]
	for _, it := range items {
		if it.Kind == attentionKindAsk && teamWatcherOf(ctx, it.owner) {
			continue
		}
		out = append(out, it)
	}
	return out
}

// attentionItemsHTML is the list's <li>s.
func attentionItemsHTML(items []attentionItem, now time.Time) string {
	return attentionItemsHTMLTo(items, now, "")
}

// attentionItemsHTMLTo is attentionItemsHTML whose ask forms send the browser
// back to returnTo once answered or dismissed ("" = the queue): THE item and
// answer partial, shared by /attention and the session pages
// (attention_asker.go). A form on a session page opts out of htmx's boost, so
// it is a plain POST whose redirect reloads that page.
func attentionItemsHTMLTo(items []attentionItem, now time.Time, returnTo string) string {
	if len(items) == 0 {
		return attentionEmptyHTML
	}
	var b strings.Builder
	for _, it := range items {
		b.WriteString(attentionItemHTML(it, now, returnTo))
	}
	return b.String()
}

// attentionItemHTML is ONE item's <li> (attentionItemsHTMLTo's).
//
// THE CARD (operator decision 2026-11-12, the Asks page's design pass): its
// head names the session — attentionHeadHTML: the name as the title, the age
// and the tmux target muted under it — and IS the link to the session when
// there is one to open, with a chevron (a real <a>, never a div with a click
// handler). An item with nothing to answer here (a waiting window, a held
// question answered in the session, a task) is ONE link as a
// whole (ui.AsksRowClass). An item with controls (an ask's answer form, a
// notice's Dismiss, an opencode window's relayed answers) is a card whose head
// is the link and whose controls sit under it, Dismiss a small icon button in
// the card's footer, never in the head. No "View in session" / "Open the session" words: the card is
// the way in. The one exception is a held question's muted "Answer in session"
// (attentionHeldCueHTML, an operator decision): it says WHERE the
// answer goes, not how to get there.
func attentionItemHTML(it attentionItem, now time.Time, returnTo string) string {
	if returnTo == "" {
		// The Asks page: the card feed's card (asks_feed.go).
		return askFeedCardHTML(it, now)
	}
	esc := html.EscapeString
	open := `<li id="` + esc(it.Key) + `" class="`
	attrs := `" data-attention-item data-kind="` + esc(it.Kind) + `">
`
	// Source (an agent's or window's name) and Focus (an LLM summary) are
	// titles, so they read as their plain text (textplain).
	focus := ""
	if f := textplain.Title(it.Focus); f != "" {
		focus = attentionFocusHTML(f, ui.FocusAgeOf(it.focusAt, now))
	}
	text, detail := esc(it.Text), esc(it.Detail)
	if it.openCode != nil {
		// An opencode ask quotes a command: drawn left to right and isolated
		// (<bdi>), so right-to-left text in it cannot reorder what the
		// person reads around it.
		text, detail = `<bdi dir="ltr">`+text+`</bdi>`, `<bdi dir="ltr">`+detail+`</bdi>`
	}
	detailHTML := ""
	if it.Detail != "" {
		detailHTML = `<span class="` + ui.AsksMutedTextClass + `" data-attention-detail>` + detail + `</span>`
	}

	// A ROW: the whole card is the link (it.Link: the rendered view, the
	// session's question card, the draft, the task). Its one line is what it
	// waits on — a waiting window's pending call ("Bash: npm test"), a held
	// question's first line — exactly the server's own text (it.Text: the
	// host's words, cleaned and capped by mirror.CleanPending), escaped.
	row := func(line string) string {
		return open + `min-w-0` + attrs + `<a href="` + esc(it.Link) + `" class="` + ui.AsksRowClass + `" data-attention-open>
<span class="` + ui.AsksHeadTextClass + ` gap-0.5">` + attentionHeadLines(it, now) + focus + line + detailHTML + `</span>` + ui.AsksChevron + `</a>
</li>
`
	}
	switch {
	case it.Kind == attentionKindSession && it.openCode == nil:
		return row(attentionWaitLineHTML(it))
	case it.Kind == attentionKindQuestion && it.openCode == nil:
		// A HELD QUESTION is answered in its session, never here: the row
		// says so in one muted line under the question (an operator
		// decision), inside the row link, so the card stays one link.
		return row(attentionWaitLineHTML(it) + attentionHeldCueHTML)
	case it.Kind == attentionKindTask:
		return row(`<span class="` + ui.AsksMutedTextClass + `" data-attention-text>` + text + `</span>`)
	}

	// A CARD: a head, the text, the controls.
	var b strings.Builder
	b.WriteString(open + ui.AsksCardClass + attrs)
	back, formAttrs := "", ""
	if returnTo != "" {
		back = `<input type="hidden" name="return" value="` + esc(returnTo) + `">`
		// A form on a session page opts out of htmx's boost, so it is a plain
		// POST whose redirect reloads that page.
		formAttrs = ` hx-boost="false"`
	}
	id := strconv.FormatInt(it.AskID, 10)
	dismissForm := "ask-" + id + "-dismiss"
	// 🔴 DISMISS IS ITS OWN FORM, posting to /cancel — the icon button only
	// SITS in the card's FOOTER (the answer form's last row, or a notice's own
	// row), joined to that form by the form= attribute. 🔴 NEVER IN THE HEAD:
	// beside the head link's chevron a thumb aimed at "open the session" could
	// cancel the ask, with no confirmation (Round 0 of #620). A
	// formaction on the answer form would also send the typed answer and run
	// through whatever binds the answer form's submit; this way the two posts
	// are exactly what they were.
	dismiss := ""
	switch it.Kind {
	case attentionKindAsk:
		dismiss = askDismissButton(dismissForm, "Dismiss without answering")
	case attentionKindNotice:
		dismiss = askDismissButton(dismissForm, "Dismiss")
	}
	href := ""
	switch it.Kind {
	case attentionKindAsk, attentionKindNotice:
		href = it.sessionLink
	default:
		href = it.Link
	}
	b.WriteString(attentionHeadHTML(it, now, href) + "\n")
	b.WriteString(focus)
	b.WriteString(officerNoteLabelHTML(it.Context))
	b.WriteString(`<p class="` + ui.AsksTextClass + `" data-attention-text>` + text + `</p>
`)
	if it.Detail != "" {
		b.WriteString(`<p class="break-words text-sm text-fg2" data-attention-detail>` + detail + `</p>
`)
	}
	b.WriteString(proposalBodyHTML(it.proposal))
	b.WriteString(officerContextHTML(it.Context))
	b.WriteString(pageAskOpenHTML(it.pageOpen))
	switch {
	case it.Kind == attentionKindNotice:
		// 🔴 NO ANSWER BOX: a notice points at a session; it is looked at or
		// dismissed, never answered (attention.ErrNotice). Its tmux target is
		// in the head (the place it names, data-attention-source); Dismiss is
		// its footer, at the card's end.
		b.WriteString(`<div class="flex justify-end">` + dismiss + `</div>
<form id="` + dismissForm + `" method="POST" action="` + attentionPath + `/asks/` + id + `/cancel" hidden` + formAttrs + `>` + back + `</form>
`)
	case it.Kind == attentionKindAsk && it.watchOnly != "":
		// A VIEWER of the team that owns it (withAskBoxes): no option, no
		// answer box and no Dismiss — nothing the reader could tap that the
		// server would refuse. Why, in their place.
		b.WriteString(`<p class="` + ui.AsksMutedTextClass + `" data-attention-watch-only>` + esc(it.watchOnly) + `</p>
`)
	case it.Kind == attentionKindAsk:
		// THE answer form, the Asks card's own (askAnswerFormHTML): Dismiss
		// ends its answer row (a session block has no footer).
		gate := ""
		if it.stepUpWhy != "" {
			// A team card answered with a fresh sign-in (teamCardStepUpWhy): it
			// confirms in place, on the page's dialog (stepUpFormsHTML).
			gate = ` data-step-up-form data-step-up-why="` + esc(it.stepUpWhy) + `"`
		}
		b.WriteString(askAnswerFormHTML(it, id, askFormPlace{back: back, attrs: gate + formAttrs, dismiss: dismiss}))
		b.WriteString(`<form id="` + dismissForm + `" method="POST" action="` + attentionPath + `/asks/` + id + `/cancel" hidden` + formAttrs + `>` + back + `</form>
`)
	default:
		// An opencode window's ask: its relayed answers, or why it can't be
		// answered here.
		b.WriteString(openCodeAskHTML(it))
	}
	b.WriteString("</li>\n")
	return b.String()
}

// attentionHeadHTML is an item card's head: its lines (attentionHeadLines)
// and, with href, the head is the link to it, ended by a chevron.
//
// A head that names a window is followed by its ⓘ (attentionPlaceSheetHTML) —
// beside the link, never inside it — in one row.
func attentionHeadHTML(it attentionItem, now time.Time, href string) string {
	lines := `<span class="` + ui.AsksHeadTextClass + `">` + attentionHeadLines(it, now) + `</span>`
	head := `<div class="` + ui.AsksHeadClass + ` flex-1">` + lines + `</div>`
	if href != "" {
		head = `<a href="` + html.EscapeString(href) + `" class="` + ui.AsksHeadLinkClass + ` flex-1" data-attention-session>` + lines + ui.AsksChevron + `</a>`
	}
	if info := attentionPlaceSheetHTML(it, it.Key); info != "" {
		return `<div class="flex min-w-0 items-center gap-1">` + head + info + `</div>`
	}
	return head
}

// attentionHeadLines is a card head's two lines: the title (attentionTitle) and,
// muted under it, how long it has waited, where it runs in words ("on atlas":
// data-attention-source; the tmux target is the card's ⓘ) and whether its
// host is offline.
func attentionHeadLines(it attentionItem, now time.Time) string {
	esc := html.EscapeString
	title, src, srcIsTitle := attentionTitle(it)
	if src == title {
		// A name that IS its place (an agent named by its label): one line.
		srcIsTitle = true
	}
	attrs := ` data-attention-title`
	if it.Name != "" {
		attrs += ` data-attention-name`
	}
	if srcIsTitle {
		attrs += ` data-attention-source`
	}
	var sub []string
	if !it.Since.IsZero() {
		sub = append(sub, `<time datetime="`+it.Since.UTC().Format(time.RFC3339)+`">`+esc(attentionAge(now.Sub(it.Since)))+`</time>`)
	}
	if !srcIsTitle && src != "" {
		sub = append(sub, `<span class="`+ui.AsksTargetClass+`" data-attention-source>`+esc(src)+`</span>`)
	}
	if it.Offline {
		sub = append(sub, `<span data-attention-offline>host offline</span>`)
	}
	out := `<span class="` + ui.AsksTitleClass + `"` + attrs + `>` + esc(title) + `</span>`
	if len(sub) > 0 {
		out += `<span class="` + ui.AsksSubClass + `">` + strings.Join(sub, ` · `) + `</span>`
	}
	return out
}

// attentionTitle is what an item's head calls it, and the place it shows
// under that (src, "" for none): the session's HUMAN name when it has one,
// its place under it; else a mirrored window's own name — its codename (its
// label, or its tmux session: mirror.Place's Code) with tmux's window index
// dropped (mirror.PlainCode: "fern", never "fern:1") — with its place under
// it; else its Source itself (an agent, muster, a plugin), and srcIsTitle says
// the title IS the source.
//
// 🔴 A WINDOW'S PLACE IS WORDS, NOT ITS ADDRESS (operator rule:
// muster's top layer shows no ids). It is "on <host>" (mirror.PlaceWords), and
// " (host offline)" after it when its host has dropped and only the Source
// says so; the exact tmux target (it.target) is the card's ⓘ details'
// (attentionPlaceSheetHTML), never this line's.
//
// The title keeps the tmux session's part of a codename VERBATIM
// (textplain.TitleKeeping): a session named "_scratch_" is typed as
// "_scratch_", never as the "scratch" a markdown stripper reads.
func attentionTitle(it attentionItem) (title, src string, srcIsTitle bool) {
	if it.stepUp != nil {
		// A step-up card is muster's own question: never its internal asker
		// label ("step-up").
		return stepUpCardSource, "", true
	}
	if host := attentionPlaceHost(it); host != "" {
		src = mirror.PlaceWords(host)
		if !it.Offline && strings.HasSuffix(it.Source, whereHostOffline) {
			src += whereHostOffline
		}
	} else {
		src = textplain.TitleKeeping(it.Source, it.target)
	}
	switch {
	case it.Name != "":
		return it.Name, src, false
	case it.target != "" && it.at.Code != "":
		// The list's name for it (windowName: qualified, "fern · window 2"),
		// else its codename without tmux's window index.
		name := it.windowName
		if name == "" {
			name = mirror.PlainCode(it.at.Code)
		}
		addr := strings.TrimSuffix(strings.TrimPrefix(it.target, "tmux "), " @ "+it.at.Host)
		return textplain.TitleKeeping(name, mirror.PlainCode(addr)), src, false
	}
	return textplain.TitleKeeping(it.Source, it.target), "", true
}

// stepUpCardSource is what a step-up card's head calls who asks.
const stepUpCardSource = "muster"

// attentionPlaceHost is the computer the window an item names runs on: its
// Place's host (every producer sets at with target); "" for an item that
// names no window.
func attentionPlaceHost(it attentionItem) string {
	if it.target == "" {
		return ""
	}
	return it.at.Host
}

// windowDisplayName is the plain name the session lists give the window at
// (mirror.Ordered's DisplayTitle, qualified over the whole list), found by
// its host and tmux address; "" when hosts does not list it.
func windowDisplayName(hosts []mirror.HostView, at mirror.Place) string {
	if at.Addr == "" {
		return ""
	}
	for _, e := range mirror.Ordered(hosts) {
		if e.Host == at.Host && e.Address() == at.Addr {
			return e.DisplayTitle()
		}
	}
	return ""
}

// attentionPlaceSheetHTML is an item's ⓘ: the exact tmux address of the window
// it names (ui.SessionPlaceSheet), "" for an item that names none. key makes
// its id unique in the page (an item's Key; the ask page passes its own).
func attentionPlaceSheetHTML(it attentionItem, key string) string {
	if it.target == "" {
		return ""
	}
	name, _, _ := attentionTitle(it)
	return ui.InfoSheetHTML(ui.SessionPlaceSheet(attentionPlaceSheetID(key), name, attentionPlaceHost(it), it.target))
}

// attentionPlaceSheetID is the ⓘ sheet's element id for an item keyed key: an
// id token whatever the key holds.
func attentionPlaceSheetID(key string) string {
	var b strings.Builder
	b.WriteString("place-")
	for _, c := range key {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' {
			b.WriteRune(c)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// attentionWaitLineHTML is a waiting row's one line: what it waits on, as the
// server already has it (it.Text — a pending call's summary, "Bash: npm test";
// a held question, cut to its first line; sessionWaitingText when the host
// named nothing). Escaped, never rendered as markup.
func attentionWaitLineHTML(it attentionItem) string {
	line := firstLine(strings.TrimSpace(it.Text))
	cls := ui.AsksWaitLineClass
	if it.Text == sessionWaitingText {
		cls = ui.AsksMutedTextClass
	}
	return `<span class="` + cls + `" data-attention-text>` + html.EscapeString(line) + `</span>`
}

// attentionHeldCueHTML is a held question's row cue: where it is answered.
const attentionHeldCueHTML = `<span class="` + ui.AsksMutedTextClass + `" data-attention-held-cue>Answer in session</span>`

// attentionEmptyHTML is the list's line when nothing is in it.
const attentionEmptyHTML = `<li class="text-sm text-fg2" data-testid="attention-empty">Nothing is waiting on you.</li>`

// attentionAge is a short age.
func attentionAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m ago"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h ago"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + "d ago"
}

// askIDParam is the {id} path value.
func askIDParam(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

// answerAsk records the operator's answer given on the Attention view.
func (s *Server) answerAsk(ctx context.Context, id int64, answer string) (attention.Ask, error) {
	return s.answerAskFrom(ctx, id, answer, attention.AnswerSource{})
}

// answerAskFrom records the operator's answer — given on the Attention view
// (src zero) or through their mirrored session's attention_answer (src names
// it) — and settles it: its notification cleared, its waiters woken, and, for an
// agent's ask, its delivery into the agent's session started once the grace
// has passed (attention_delivery.go; the sweep delivers it if this process stops
// first). The ONE answer path both doors take.
func (s *Server) answerAskFrom(ctx context.Context, id int64, answer string, src attention.AnswerSource) (attention.Ask, error) {
	var a attention.Ask
	// A host officer machine's confirmation card (host_stepup.go): answered
	// only from the owner's paired phone, and on "Confirm" the owner-authority
	// call it holds runs once — after the hook has recorded the answer (its
	// claim on the open card), so nothing is recorded here; any other door
	// is refused and the card stays open.
	if handled, rec, err := s.hostDecisionForAnswer(ctx, id, answer); handled {
		if err != nil {
			if errors.Is(err, attention.ErrNotOpen) {
				// A card closed meanwhile: its state, for the refusal's words.
				a, _ = s.ext.Attention.Get(ctx, id)
			}
			return a, err
		}
		s.answerRecorded(ctx, rec)
		return rec, nil
	}
	// A dispatch card whose hold lapsed (hold_lapse.go): nothing is recorded;
	// the card is withdrawn, the chief told, and the owner reads why.
	if err := s.lapsedCardRefusal(ctx, id); err != nil {
		return a, err
	}
	// A chief's dispatch APPROVAL card (dispatch_approval.go): never answered
	// from the phone's notification action, which cannot show the brief —
	// refused before anything is recorded or done; the card stays open.
	if err := s.approvalSurfaceRefusal(ctx, id); err != nil {
		return a, err
	}
	// A Release option of a routed task's ask (answer_release.go): checked
	// before the answer is recorded, applied before anyone is woken by it.
	rel, err := s.releaseForAnswer(ctx, id, answer, src)
	if err != nil {
		return a, err
	}
	// An auto-start's Stop ask (plugin_autostart.go): the agent is stopped
	// before the answer is recorded; a stop that failed leaves the ask open.
	if err := s.autostartStopForAnswer(ctx, id, answer); err != nil {
		return a, err
	}
	// A chief proposal's card (chief_proposal.go): the owner's tap DOES what
	// was proposed here, before the answer is recorded; an accept refused
	// before it did anything leaves the card open, with its sentence.
	note, err := s.proposalForAnswer(ctx, id, answer, src)
	if err != nil {
		return a, err
	}
	// A routing rule's card (routing_rules.go): the same tap, the same rules —
	// the owner's door, a tapped button, once — approving or declining it.
	if note == nil {
		if note, err = s.ruleForAnswer(ctx, id, answer, src); err != nil {
			return a, err
		}
	}
	// A page link's card (page_share.go): the owner's own door, one of its two
	// options, the link still pending — then the link goes live or is denied.
	teamCard := false
	if note == nil {
		if teamCard, err = s.pageShareForAnswer(ctx, id, answer, src); err != nil {
			return a, err
		}
	}
	if teamCard {
		// A team member's answer to a team agent's page share card: its own
		// recording door (page_share.go recordTeamPageCardAnswer).
		a, err = s.recordTeamPageCardAnswer(ctx, id, answer)
	} else if src.ViaSession() {
		a, err = s.ext.Attention.AnswerFromSession(ctx, id, answer, src.Host, src.Session)
	} else {
		a, err = s.ext.Attention.Answer(ctx, id, answer)
	}
	if err == nil && rel != nil {
		s.applyAnswerRelease(ctx, a, rel)
	}
	if err == nil && note != nil {
		// 🔴 A CHIEF PROPOSAL'S CARD IS NEVER DELIVERED BACK AS AN ANSWER: a
		// tap is not a request for a turn. Its delivery is closed (MarkWaited:
		// nothing is pushed into the chief's session, now or by the sweep),
		// and the chief is told in muster's own sentence instead. Should
		// MarkWaited fail, the sweep delivers the card's question and the
		// tapped label — muster's own words: the question never holds the
		// kickoff (successionCardQuestion).
		if _, werr := s.ext.Attention.MarkWaited(ctx, a.ID); werr != nil {
			s.logger.Printf("proposal: close the delivery of card %d: %v", a.ID, werr)
		}
		a.Delivery = attention.DeliveryWaited
		s.noteProposalToChief(ctx, *note)
	}
	if err == nil {
		s.answerRecorded(ctx, a)
	}
	return a, err
}

// answerRecorded settles answer a, just recorded: answerAskFrom's last step on
// every path that records one (a confirmation card's included).
func (s *Server) answerRecorded(ctx context.Context, a attention.Ask) {
	s.askSettled(a)
	if a.Delivery == attention.DeliveryQueued {
		s.startAnswerDelivery(a.Owner, a.ID, s.deliveryGrace()+500*time.Millisecond)
	}
	// An ask raised for a plugin message the chief did not act on
	// (unrouted.go): the answer settles that message.
	s.settleAnsweredMessage(ctx, a)
	// A chief nudge raises a notice, which has no answer (attention_notice.go):
	// nothing of a nudge's is relayed from here.
}

// cancelAsk withdraws an ask and settles it.
func (s *Server) cancelAsk(ctx context.Context, id int64) (attention.Ask, error) {
	a, err := s.ext.Attention.Cancel(ctx, id)
	if err == nil {
		s.askSettled(a)
	}
	return a, err
}

// attentionErrText is a store error as the owner reads it.
func attentionErrText(err error, cur attention.Ask) string {
	switch {
	case errors.Is(err, attention.ErrNotFound):
		return "No such question."
	case errors.Is(err, attention.ErrNotice):
		return "That is a notice, not a question: it has no answer. Open its session, or dismiss it."
	case errors.Is(err, attention.ErrNotOpen):
		return "That question is no longer open (it is " + string(cur.Status) + ")."
	case errors.Is(err, attention.ErrInvalid):
		return strings.TrimPrefix(err.Error(), attention.ErrInvalid.Error()+": ")
	}
	return "That could not be recorded."
}

func attentionErrCode(err error) int {
	switch {
	case errors.Is(err, attention.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, attention.ErrNotOpen), errors.Is(err, attention.ErrNotice):
		return http.StatusConflict
	case errors.Is(err, attention.ErrInvalid):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func (s *Server) handleAttentionAnswerForm(w http.ResponseWriter, r *http.Request) {
	if s.ext.Attention == nil {
		http.NotFound(w, r)
		return
	}
	id, ok := askIDParam(r)
	if !ok {
		s.renderAttentionPage(w, r, http.StatusNotFound, "No such question.")
		return
	}
	// An option button wins over the free text: it is what was tapped.
	answer := r.PostFormValue("option")
	if strings.TrimSpace(answer) == "" {
		answer = r.PostFormValue("answer")
	}
	ctx := r.Context()
	if strings.TrimSpace(r.PostFormValue("option")) != "" {
		// A button TAPPED, not text typed (a chief proposal accepts only
		// that: chief_proposal.go optionTapped).
		ctx = withOptionTap(ctx)
	}
	a, err := s.answerAsk(ctx, id, answer)
	if err != nil {
		if attentionErrCode(err) == http.StatusInternalServerError {
			s.logger.Printf("attention: answer %d: %v", id, err)
		}
		s.attentionFormFailed(w, r, err, a)
		return
	}
	if asksCardRequest(r) {
		// The Asks page's card (asks_feed.go): nothing to draw; its script
		// settles the card.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, attentionReturn(r), http.StatusSeeOther)
}

func (s *Server) handleAttentionCancelForm(w http.ResponseWriter, r *http.Request) {
	if s.ext.Attention == nil {
		http.NotFound(w, r)
		return
	}
	id, ok := askIDParam(r)
	if !ok {
		s.renderAttentionPage(w, r, http.StatusNotFound, "No such question.")
		return
	}
	a, err := s.cancelAsk(r.Context(), id)
	if err != nil {
		s.attentionFormFailed(w, r, err, a)
		return
	}
	if asksCardRequest(r) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, attentionReturn(r), http.StatusSeeOther)
}

// attentionAnsweredPath is the Asks page on its Answered tab: where a Retry
// lands, and where its refusal is drawn.
const attentionAnsweredPath = attentionPath + "?" + asksTabParam + "=" + asksTabAnswered

// handleAttentionRedeliverForm is an unconfirmed delivery's Retry: back to
// queued (the caller's own ask only — row-level security), then delivered now.
func (s *Server) handleAttentionRedeliverForm(w http.ResponseWriter, r *http.Request) {
	if s.ext.Attention == nil {
		http.NotFound(w, r)
		return
	}
	id, ok := askIDParam(r)
	if !ok {
		s.renderAttentionPage(w, r, http.StatusNotFound, "No such question.")
		return
	}
	a, err := s.ext.Attention.RetryDelivery(r.Context(), id)
	switch {
	case err == nil:
	case errors.Is(err, attention.ErrNotRetryable):
		s.renderAsksPage(w, r, http.StatusConflict, "That answer's delivery has not failed, so there is nothing to retry.", asksTabAnswered, 0)
		return
	default:
		if attentionErrCode(err) == http.StatusInternalServerError {
			s.logger.Printf("attention: retry the delivery of %d: %v", id, err)
		}
		s.renderAsksPage(w, r, attentionErrCode(err), attentionErrText(err, a), asksTabAnswered, 0)
		return
	}
	s.attentionChanged(a.Owner, attentionAskKey(a.ID))
	s.startAnswerDelivery(a.Owner, a.ID, 0)
	// Back to the Answered tab, where the Retry was (and its card's new state).
	http.Redirect(w, r, attentionAnsweredPath, http.StatusSeeOther)
}

// attentionAnsweredWindow / attentionAnsweredShown bound the "Answered" list:
// the answers of the last day, at most this many.
const (
	attentionAnsweredWindow = 24 * time.Hour
	attentionAnsweredShown  = 20
)

// recentAnswers is ctx's principal's recent answers with a delivery to show.
func (s *Server) recentAnswers(ctx context.Context) []attention.Ask {
	if s.ext.Attention == nil {
		return nil
	}
	out, err := s.ext.Attention.Answered(ctx, time.Now().Add(-attentionAnsweredWindow), attentionAnsweredShown)
	if err != nil {
		s.logger.Printf("attention: recent answers: %v", err)
	}
	return out
}

// answerDeliveryText is where an answer went, as the owner reads it, and
// whether it offers Retry. who is the asker as askSource names it — for a
// mirrored session the exact tmux target of the window running it now, or
// "not shown" — since a waiting, queued or failed delivery sends the owner to
// that session. proposal: a is a chief proposal's card, whose answer is
// recorded and never delivered once answerAskFrom closed its delivery
// (waited); one whose close failed says where it went, as any other.
func answerDeliveryText(a attention.Ask, who string, proposal bool) (string, bool) {
	if proposal && a.Delivery == attention.DeliveryWaited {
		return proposalAnsweredText, false
	}
	switch a.Delivery {
	case attention.DeliveryQueued:
		if a.DeliveryDetail != "" {
			// A reason that never clears on its own (the agent stopped, no
			// agent gateway) is the owner's to act on, so it is said, with
			// what to do; a transient one (busy, mid-turn, being replaced,
			// the runtime took nothing) is muster's to retry.
			if t := deliveryWaitStuckText(a.DeliveryDetail, who); t != "" {
				return t, false
			}
			return "Will reach " + who + " when it's back.", false
		}
		return "Queued for " + who + ": delivering it as a new message in its session.", false
	case attention.DeliveryDelivering:
		return "Delivering to " + who + " now.", false
	case attention.DeliveryDelivered:
		return "Delivered to " + who + " as a new message in its session.", false
	case attention.DeliveryWaited:
		return who + " read it while waiting for it.", false
	case attention.DeliveryUnconfirmed:
		return "Delivery failed: " + firstNonBlank(a.DeliveryDetail, "unknown") + ".", true
	case attention.DeliveryPull:
		return "Waiting for " + who + " to read it: a mirrored session reads its answer itself (attention_wait); muster does not type into it.", false
	}
	if a.Asker.System {
		return "Recorded. muster raised this question itself, so there is nobody to deliver it to.", false
	}
	return "Not delivered.", false
}

// answeredAsksNow is answeredAsksHTML over ctx's principal's recent answers
// and, when one of them is a mirrored session's, its windows (askSource).
func (s *Server) answeredAsksNow(ctx context.Context, now time.Time) string {
	asks := s.recentAnswers(ctx)
	var hosts []mirror.HostView
	if s.ext.Mirror != nil && slices.ContainsFunc(asks, askNeedsHosts) {
		hosts = s.ext.Mirror.Hosts(ctx)
	}
	proposals := map[int64]bool{}
	for _, a := range asks {
		if s.isProposalCard(ctx, a) {
			proposals[a.ID] = true
		}
	}
	return answeredFeedHTML(asks, s.answeredItems(ctx, asks, hosts), now, proposals)
}

// answerLabel names who gave an answer on the Answered tab: the owner, or —
// for a session's question to the chief (chief_ask_tool.go) — the chief.
func answerLabel(a attention.Ask) string {
	if a.AnsweredBy == attention.ByChief {
		return "Your chief's answer: "
	}
	return "Your answer: "
}

// answeredViaHTML is an Answered item's provenance line: "" for an answer given
// on this view; for attention_answer, the mirrored session it came from.
func answeredViaHTML(a attention.Ask) string {
	if !a.AnsweredVia.ViaSession() {
		return ""
	}
	return `<p class="break-words text-xs text-fg2" data-attention-answered-via>Given via MCP from your mirrored session ` +
		html.EscapeString(a.AnsweredVia.String()) + `</p>
`
}

func nonNilItems(items []attentionItem) []attentionItem {
	if items == nil {
		return []attentionItem{}
	}
	return items
}

// --- answered in the session -------------------------------------------------

// askerAddressed is the operator writing to an asker DIRECTLY — a message
// pasted into a mirrored session's window (mirrorDeliver), queued for it
// (sendQueueAdd), or sent to a dispatched agent's chat (handleAgentSend,
// handleAgentWS) — never a line typed to the chief from a session's view
// (handleViewChief: the chief's asks are settled only from its own chat;
// operator decision, PR #537 round 1): every ask that asker raised before the message that is still
// open leaves the queue as
// answered_in_session, the message (capped) its answer — its notification
// cleared, its waiters woken (askSettled), so the asker's attention_wait
// returns {status: "answered_in_session", answer}.
//
// 🔴 ONLY THE OPERATOR'S OWN MESSAGES CALL THIS. A routed message (session
// routing's send-queue rows), the chief's hand-back turn, a machine-token
// message and anything an agent sends are not the operator answering, and
// none of those paths call it. ctx is the operator's principal (row-level
// security keeps it to their own asks).
func (s *Server) askerAddressed(ctx context.Context, asker attention.Asker, message string) {
	s.askerAddressedBy(ctx, asker, message, 0)
}

// askerAddressedBy is askerAddressed, recording the send-queue row (queueID,
// 0 = none) the message is, so that row can say it answered a question.
func (s *Server) askerAddressedBy(ctx context.Context, asker attention.Asker, message string, queueID int64) {
	if s.ext.Attention == nil || asker.Valid() != nil {
		return
	}
	// 🔴 A TYPED LINE NEVER SETTLES A TEAM'S DISPATCH CARD (PR #682 round 1):
	// it is approved only on the card, by an operator with a fresh sign-in
	// (teamCardRefusal), and a line typed into the team's room — by a stale
	// or any other member — would close it as answered in the chat, leaving
	// nothing to approve. It stays open (teamDispatchCardKept).
	bg := context.WithoutCancel(ctx)
	asks, err := s.ext.Attention.AnswerInSession(bg, asker, message, queueID,
		func(a attention.Ask) bool { return s.teamDispatchCardKept(bg, a) })
	if err != nil {
		s.logger.Printf("attention: answered in session by a message to %s: %v", askerName(asker), err)
		return
	}
	for _, a := range asks {
		s.logger.Printf("attention: ask %d answered in session (a message to %s)", a.ID, askerName(asker))
		s.askSettled(a)
	}
}

// mirrorAddressed is askerAddressed for a mirrored window's Claude Code
// session — ONLY for a message pasted into that session's CLAUDE PANE
// (mirror.PasteClaude: the rendered view's Send, a queued message). A Terminal
// send goes to the window's ACTIVE pane, which may be a shell beside Claude
// Code, so it answers nothing. queueID is the send-queue row the message is,
// 0 for a direct Send. Nothing for a window that runs no Claude Code.
func (s *Server) mirrorAddressed(ctx context.Context, host string, e mirror.SessionView, target mirror.PasteTarget, message string, queueID int64) {
	if target != mirror.PasteClaude || !e.Claude || e.ClaudeSession == "" {
		return
	}
	s.askerAddressedBy(ctx, attention.Asker{MirrorHost: host, MirrorSession: e.ClaudeSession}, message, queueID)
}

// askerName names an asker in the log: its ids, never its label.
func askerName(a attention.Asker) string {
	if a.AgentID > 0 {
		return "agent " + strconv.FormatInt(a.AgentID, 10)
	}
	return "session " + a.MirrorSession + " @ " + a.MirrorHost
}
