package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/sessionwake"
)

// ---------------------------------------------------------------------------
// THE STEP-UP GATE ON A HOST OFFICER'S MACHINE (operator decision, 2026-11-13;
// docs/design/officers-on-hosts.md, "The step-up gate").
//
// The mirror door names its caller by a header the caller sets: the host's
// loopback MCP endpoint maps X-Muster-Pane to a session, and any process of
// the machine's user can send any pane. On a machine that runs a host
// officer, that process may be the officer itself — so an owner-authority
// tool reached through that machine's door (approving a draft, deciding a
// connector request, answering an ask …) proves nothing about WHO decided.
//
// 🔴 SO ON SUCH A MACHINE EVERY OWNER-AUTHORITY TOOL IS A REQUEST, NOT A
// DECISION. hostStepUpGate files a CONFIRMATION CARD to the owner — one card
// per decision: what it does and why on its face (for a draft, the post
// itself), the tool and its exact arguments in its Details
// (host_stepup_card.go) — and runs nothing.
// The decision runs only when the owner answers that card "Confirm" through
// one of two doors, each a fresh proof made for that one decision: their
// paired phone's SIGNED ACTION (device_actions.go), whose challenge is single
// use, lives two minutes and is bound to that card's id and the sha256 of the
// exact answer; or, on the card in muster, a PASSKEY CEREMONY bound to that
// card (stepup_web_confirm.go). An ordinary browser answer (a fresh sign-in
// included), an MCP answer (attention_answer from any machine) and an answer
// after a restart lost the pending decision are refused, and the card stays
// open (or is withdrawn). The decision runs
// once: it is taken out of the pending set, and the card is answered (the
// store's own open-only update, which a dismissal racing it cannot pass), before
// it runs. A card that is no longer open (dismissed, withdrawn, expired) runs
// nothing: a later Confirm drops its decision and is refused
// (hostDecisionForAnswer).
//
// 🔴 A REMOTE CALLER IS ALWAYS GATED (mirror_mcp.go, "A remote caller"): a
// session mirrored to ANOTHER muster, calling this one's admin tools. Its host
// officers are declared on the muster it is mirrored to, not here, so this
// registry cannot say whether its machine runs one — an officer there could
// name the owner's pane and decide as them. So every owner-authority tool from
// a remote caller files the card, whatever this registry says.
//
// 🔴 SO IS A MACHINE THAT STARTS AGENTS FOR THE CHIEF (hostSpawnStepUp): a
// call whose host token is marked as one whose host has started agents
// (mirror_tokens.spawned_at — set by the host's first hello advertising
// -agent-spawn, or a host dispatch reserved on it, and cleared only by
// rotating the token), or whose host advertises -agent-spawn now. A spawned
// agent runs as the machine's user too, so it can name the owner's pane,
// clear its own window's dispatch tag, or read the host's token and call this
// door directly — from anywhere, with a connection of its own: whichever
// window a call names, with such a token it proves nothing about who decided.
// The one carve-out (operator decision): route_reply to the route the chief
// sent to the calling session itself files no card on such a machine.
//
// What is NOT gated: the owner deciding in muster's own web UI (the Reply
// drafts tab, Connectors, Attention), which is the owner's signed-in browser;
// any machine mirrored HERE with no host officer declared on it that does not
// start agents for the chief; the read tools; and chief_restart, on every
// machine, force included (operator decision "fully ungated"; its accepted
// risk is stated at its entry in hostStepUpExempt).
// ---------------------------------------------------------------------------

// hostStepUpTools is THE LEDGER of the mirror door's owner-authority tools, by
// name, with what a confirmation card says it asks to do. Every other mirror
// door tool is classified in hostStepUpExempt (TestEveryMirrorToolIsClassifiedForTheStepUp
// fails when a new one is neither).
var hostStepUpTools = map[string]string{
	attentionAnswerTool:           "answer one of your open asks as you",
	connectorRequestDecideTool:    "decide an agent's connector request as you",
	pluginDraftDecideTool:         "approve or reject a reply or outbound draft as you",
	agentDestroyTool:              "destroy one of your agents",
	dispatchApproveTool:           "approve a held agent start as you",
	routeReplyToolName:            "answer a routed message (which may be relayed to its sender) as you",
	pluginMessageRouteTool:        "route a plugin message as you",
	pluginRequestDecideTool:       "decide a plugin's pairing request (which mints it a token) as you",
	pluginOutboundBatchDecideTool: "approve the ticked follows and likes of an outreach batch, and reject the rest, as you",
	officerItemEnqueueTool:        "queue an item for one of your officers as you (an owner item, which a spent daily budget does not hold back)",
}

// hostStepUpExempt is every other mirror door tool, with why it is not an
// owner-authority tool.
var hostStepUpExempt = map[string]string{
	attentionAskTool:           "the session's own question to the owner",
	attentionWaitTool:          "waits on the session's own ask",
	attentionCancelTool:        "withdraws the session's own ask",
	tasksListTool:              "the tasks this session created",
	taskGetTool:                "the tasks this session created",
	taskCommentTool:            "the tasks this session created",
	taskSetStatusTool:          "the tasks this session created",
	chiefAskTool:               "a question to the chief, never the owner's turn",
	agentsListTool:             "a read",
	connectorRequestsListTool:  "a read",
	pluginDraftsListTool:       "a read",
	pluginDraftRetryTool:       "re-sends only the bytes the owner already approved, after an attempt its plugin said sent nothing, one attempt at a time (no step-up: the retry contract derived from the operator's ask to approve once and have it retry on its own, with a manual retry via MCP)",
	pluginOutboundBatchGetTool: "a read",
	pluginMessagesListTool:     "a read",
	dispatchRequestTool:        "files a held start that starts nothing; dispatch_approve is the decision",
	pageCreateTool:             "the session's own page; a link needs the owner's tap",
	pageUpdateTool:             "the session's own page",
	pageGetTool:                "a read",
	pageListTool:               "a read",
	agentReportDoneTool:        "the session's own dispatch",
	// chief_restart is exempt BY DECISION (operator decision "fully
	// ungated"): the owner wants a mirrored session to restart their chief
	// without a phone confirmation. 🔴 THE ACCEPTED RISK: on a machine this
	// gate covers, any process of the machine's user — an agent muster started
	// there for the chief included, by naming the owner's pane — can now
	// restart the owner's chief, and force cuts off a turn the chief is in. The
	// worst outcome is an interrupted chief turn: no data is lost, no other
	// agent is touched (the tool resolves only the owner's own chief), and it
	// is what the web Stop/Start already allows with a session alone. A
	// session the server recorded as a host dispatch is still refused by the
	// tool itself (ownersOwnSession).
	chiefRestartTool: "restarts only the owner's own chief (Stop then Start); the worst outcome is an interrupted chief turn, as the web Stop/Start, which is not step-up gated either",
	// track_task is exempt BY DECISION, not by default: it is no owner
	// authority. It writes only the declaration of the session the call is
	// bound to (no subject argument), which every reader treats as untrusted
	// data; on a host-officer machine a header-named caller can therefore only
	// mislabel that one session's tasks, never act as the owner.
	trackTaskTool:             "the session's own declared tasks (untrusted data; no owner authority)",
	pageShareRequestTool:      "on a machine this gate covers it always asks the owner for a link, whatever their pages setting says (pageShareGoesLive), so their tap decides",
	pageResponsesTool:         "a read of the session's own page",
	pageLinkRevokeTool:        "revokes a link of the session's own page",
	pageAssetUploadTool:       "an image or video for the session's own page (shown once a version of it uses it)",
	pageAssetDeleteTool:       "deletes an asset of the session's own page that no version uses",
	notifyOwnerTool:           "tells the owner something, labelled as the session; it decides nothing",
	"connectors_list":         "a host instance's own (hostOfficerMirrorTool)",
	"connector_request":       "a host instance's own (hostOfficerMirrorTool)",
	"connector_status":        "a host instance's own (hostOfficerMirrorTool)",
	postgresQueryToolName:     "a host instance's own (hostOfficerMirrorTool)",
	internalHTTPToolName:      "a host instance's own (hostOfficerMirrorTool)",
	pluginRequestsListTool:    "a read",
	officers.ToolBrief:        "a host instance's own (hostOfficerMirrorTool)",
	officers.ToolItems:        "a host instance's own (hostOfficerMirrorTool)",
	officers.ToolClaim:        "a host instance's own (hostOfficerMirrorTool)",
	officers.ToolHeartbeat:    "a host instance's own (hostOfficerMirrorTool)",
	officers.ToolComplete:     "a host instance's own (hostOfficerMirrorTool)",
	officers.ToolRelease:      "a host instance's own (hostOfficerMirrorTool)",
	officers.ToolAskPerson:    "a host instance's own (hostOfficerMirrorTool)",
	officers.ToolPostDraft:    "a host instance's own (hostOfficerMirrorTool); the draft itself waits for the owner",
	officers.ToolContactGet:   "a host instance's own (hostOfficerMirrorTool)",
	officers.ToolContactsList: "a host instance's own (hostOfficerMirrorTool)",
	officers.ToolContactNote:  "a host instance's own (hostOfficerMirrorTool); it can only set a mark",
}

// hostDecisionLabel is the confirmation card's asker label: how a card whose
// pending decision a restart lost is still recognised (and refused).
const hostDecisionLabel = "step-up"

// hostDecision is one pending owner-authority call, waiting for its card.
type hostDecision struct {
	owner      int64
	host       string
	caller     toolCaller
	tool, args string
	// question is the card's own text, exactly as filed: what a paired phone's
	// challenge for this card hands back (stepUpCardConfirms), so the step-up
	// prompt shows what the signature confirms.
	question string
	// confirm, when set, is what a Confirm runs in place of a mirror door
	// tool: muster's own decision (adding a passkey, passkey_enrol.go). Its
	// answer is the outcome's words.
	confirm func(context.Context) string
	// phoneOnly: only the paired phone decides this card, never a passkey in
	// the browser (adding a passkey must not be approved by a passkey a
	// password could have added).
	phoneOnly bool
}

// hostDecisions is the pending set, by card (ask) id. In memory: muster runs
// one replica, and a restart can only LOSE a pending decision (its card is
// then refused and withdrawn), never run one.
type hostDecisions struct {
	mu      sync.Mutex
	pending map[int64]hostDecision
	// deciding is every card whose decision a door has CLAIMED (claim) and
	// not yet settled: taken out of pending, its answer not yet recorded. A
	// second door reaching the card then finds no pending decision, and this
	// tells it the card is being decided, never that a restart lost it.
	deciding map[int64]bool
}

func (h *hostDecisions) put(id int64, d hostDecision) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.pending == nil {
		h.pending = map[int64]hostDecision{}
	}
	h.pending[id] = d
}

// take removes and returns card id's decision: single use.
func (h *hostDecisions) take(id int64) (hostDecision, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	d, ok := h.pending[id]
	delete(h.pending, id)
	return d, ok
}

// claim is take for the door that DECIDES card id: the decision is removed
// (single use) and the card marked as being decided until settled.
func (h *hostDecisions) claim(id int64) (hostDecision, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	d, ok := h.pending[id]
	if !ok {
		return d, false
	}
	delete(h.pending, id)
	if h.deciding == nil {
		h.deciding = map[int64]bool{}
	}
	h.deciding[id] = true
	return d, true
}

// settled ends claim's mark on card id: its answer was recorded, or was not
// and its decision is pending again (put).
func (h *hostDecisions) settled(id int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.deciding, id)
}

// isDeciding reports whether a door has claimed card id and not settled it.
func (h *hostDecisions) isDeciding(id int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.deciding[id]
}

func (h *hostDecisions) peek(id int64) (hostDecision, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	d, ok := h.pending[id]
	return d, ok
}

// hostOfficersOnMachine is what gates owner's machine host: the host officers
// of owner declared on it, and every officer one of whose windows is still
// open there (hostOfficerWindowsOpen). A non-empty answer gates the door.
//
// 🔴 THE ROSTER IS NOT ENOUGH (round 0's F4). Deleting a host officer takes
// it out of the roster at once, but its teardown closes only an IDLE window
// (agent-close at once would cut a mid-turn one off): a window still working
// its last turn stays open, running as the machine user, and with the roster
// alone the gate dropped while it did. So an officer-spawned window muster
// recorded and the host still lists gates the machine too, whether its
// officer is paused, deleted or moved, until the window is gone.
func (s *Server) hostOfficersOnMachine(ctx context.Context, owner int64, host string) []string {
	if s.officerRT == nil {
		return nil
	}
	var out []string
	for _, o := range s.officerRT.reg.All() {
		if o.OnHost() && o.Host == host && o.Owner == owner {
			out = append(out, o.ID)
		}
	}
	for _, id := range s.hostOfficerWindowsOpen(ctx, owner, host) {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// hostOfficerWindowsOpen is the officers (ids, sorted) with a window open on
// owner's host now: a spawn muster recorded whose window — the id the host's
// own reply named, never a tag a window can set — the host lists. A host this
// muster cannot see answers none (no call reaches the door from it).
// ⚠ An unreadable spawn store answers one entry, "unknown", so the gate holds
// rather than drop on a read error.
func (s *Server) hostOfficerWindowsOpen(ctx context.Context, owner int64, host string) []string {
	st := s.officerHostSpawns()
	if st == nil {
		return nil
	}
	octx := s.officerPrincipal(ctx, owner)
	v, _ := s.officerHostView(octx, host)
	var windows []string
	for _, w := range v.Windows {
		if w.ID != "" {
			windows = append(windows, w.ID)
		}
	}
	if len(windows) == 0 {
		return nil
	}
	rows, err := st.HostSpawnsInWindows(octx, host, windows)
	if err != nil {
		s.logger.Printf("officers: read host %s's officer windows: %v; its door stays gated", host, err)
		return []string{"unknown: its officer windows could not be read"}
	}
	var out []string
	for _, r := range rows {
		if !slices.Contains(out, r.Officer) {
			out = append(out, r.Officer)
		}
	}
	sort.Strings(out)
	return out
}

// officerSpawnsListing applies one listing of owner's host to the officer
// spawn windows muster recorded there (officers.HostSpawnStore.
// ObserveHostListing): a window the host has stopped listing, two listings in
// a row, is gone for good, and hostOfficerWindowsOpen never matches its row
// again.
//
// 🔴 WHY BY OBSERVATION, AND NOT BY A TAG (round 2's second finding). The gate
// matched a recorded window by host and window id alone, and the rows were
// never closed — so once a deleted officer's window had ended, a tmux window
// id the host later reissued (a restarted tmux server numbers from the start
// again) to any other window re-gated the machine for an officer that is
// gone. The window's @muster-dispatch tag would tell the two apart, but the
// tag is the window's own option, which anything running IN the window can
// set or clear: the very officer window the gate holds for could then
// un-gate its machine. The window id is the tmux server's, never reused while
// that server lives, and the host's listing is what the hub was told — so a
// window id absent from two listings in a row (one transient miss frees
// nothing, as the host-dispatch reconciler's hostMissingToEnd) is the old
// window gone, and its row closes. A row younger than hostPendingCountdown is
// not counted: its window may not be listed yet.
//
// ⚠ What it cannot see: a window that ends, a tmux server restart, and the
// reissue of that very id, all while the host is DISCONNECTED (no listing in
// between). The mirror reports no tmux server identity to tell them apart.
func (s *Server) officerSpawnsListing(ctx context.Context, owner int64, host string, windows []mirror.Window) {
	st := s.officerHostSpawns()
	if st == nil {
		return
	}
	ids := make([]string, 0, len(windows))
	for _, w := range windows {
		if w.ID != "" {
			ids = append(ids, w.ID)
		}
	}
	if err := st.ObserveHostListing(s.officerPrincipal(ctx, owner), host, ids, s.officerRT.now().Add(-hostPendingCountdown)); err != nil {
		s.logger.Printf("officers: apply host %s's listing to its officer windows: %v", host, err)
	}
}

// The card's two answers.
const (
	hostDecisionConfirm = "Confirm"
	hostDecisionRefuse  = "Refuse"
)

// stepUpCoverage is THE ONE PREDICATE for whether the step-up gate covers a
// call from c on host: the machine runs a host officer (ids), c is a remote
// caller, or the machine starts agents for the chief (spawn, why). The gate
// and page_share_request (pageShareGoesLive) both read it.
func (s *Server) stepUpCoverage(ctx context.Context, host string, c toolCaller) (ids []string, spawn string, covered bool) {
	p, _ := authz.From(ctx)
	ids = s.hostOfficersOnMachine(ctx, p.UserID, host)
	if len(ids) > 0 || c.remote {
		return ids, "", true
	}
	spawn = s.hostSpawnStepUp(ctx, host)
	return ids, spawn, spawn != ""
}

// hostStepUpGate is the mirror door's gate, run on every call after the host
// instance rule: an owner-authority tool from a machine that runs a host
// officer, from a machine that starts agents for the chief (hostSpawnStepUp),
// or from a remote caller, files a confirmation card and runs nothing (ok
// true, the tool's answer is the card); any other call is not the gate's (ok
// false). Which window the call names plays no part.
func (s *Server) hostStepUpGate(ctx context.Context, host string, c toolCaller, name, args string) (string, bool) {
	what, gated := hostStepUpTools[name]
	if !gated {
		return "", false
	}
	p, _ := authz.From(ctx)
	// A call the tool itself refuses as a session muster started for the
	// chief files no card: the owner is never asked to confirm a call that
	// could only be refused (ownersOwnSession runs again inside the tool).
	if why := s.ownersOwnSession(ctx, c); why == ownerAuthoritySpawned {
		return toolErr("%s", why), true
	}
	// Nor for an officer_item_enqueue whose expect_session is not the bound
	// caller: refused at once, so its CLI falls back as on an uncovered machine.
	if name == officerItemEnqueueTool {
		var in struct {
			Expect string `json:"expect_session"`
		}
		if json.Unmarshal([]byte(orEmptyJSON(args)), &in) == nil {
			if why := expectSessionRefusal(c, in.Expect); why != "" {
				return why, true
			}
		}
	}
	ids, spawn, covered := s.stepUpCoverage(ctx, host, c)
	if !covered {
		return "", false
	}
	if len(ids) == 0 && !c.remote {
		// The one carve-out on a machine that only starts agents: a reply to a
		// route the chief sent to THIS session (operator decision).
		if name == routeReplyToolName && s.routeReplyToItsOwnSession(ctx, c, args) {
			return "", false
		}
	}
	// A dispatch_approve card names what it starts, not only the hold's id.
	held := ""
	if name == dispatchApproveTool {
		sum, ok := s.stepUpHoldSummary(ctx, args)
		if !ok {
			return toolErr("%s: the held dispatch could not be read, so nothing was asked of your owner; check the hold_id dispatch_request returned", name), true
		}
		held = sum
	}
	// why is the reason the card gives: this machine's officers, that it
	// starts agents for the chief, or — a remote caller — that this muster
	// cannot see its machine at all.
	why := "This machine runs host officer " + strings.Join(ids, ", ")
	switch {
	case len(ids) > 0:
	case spawn != "":
		why = "This machine " + spawn + "; such an agent runs as the machine's user"
		ids = []string{"none; it starts agents for the chief"}
	default:
		why = "This call came from a host that is mirrored to another muster, so this one cannot tell whether that machine runs a host officer"
		ids = []string{"unknown: mirrored elsewhere"}
	}
	if s.ext.Attention == nil {
		return toolErr("%s from %s needs your owner's confirmation on their phone (%s), and this server has no "+
			"attention store to ask through; decide it in muster's web UI", name, host, strings.ToLower(why[:1])+why[1:]), true
	}
	// The card's parts (host_stepup_card.go): its title, why and preview on
	// its face, the boilerplate in its Details. Its question is built from
	// them (attention.Clean), the one text the push, the page and the
	// phone's prompt read.
	details := stepUpDetails(host, firstNonBlank(c.asker.Label, c.asker.MirrorSession), what, name, args, why)
	card := s.stepUpCardFor(ctx, host, c, name, args, what, held, details)
	// The session the call came from, as muster bound it (never the call's
	// arguments): it is told how the owner settles the card
	// (session_wake_requests.go).
	// A bound Claude Code session only: not a dispatched agent, not muster
	// itself, not a remote caller, not an opencode session (whose key is not a
	// Claude session id, so wakeClaudeTarget refuses it).
	if host, sess, ok := wakeClaudeTarget(c.asker.MirrorHost, c.asker.MirrorSession); ok && card != nil && c.agent == nil &&
		c.asker.AgentID == 0 && !c.asker.System && !c.remote {
		card.RaisedByHost, card.RaisedBySession = host, sess
	}
	ask, err := s.ext.Attention.Create(s.attentionPrincipal(ctx, p.UserID), attention.New{
		Asker:   attention.Asker{System: true, Label: hostDecisionLabel},
		StepUp:  card,
		Options: []string{hostDecisionConfirm, hostDecisionRefuse},
	})
	if err != nil {
		s.logger.Printf("step-up: file a card for %s from %s/%s: %v", name, host, c.asker.MirrorSession, err)
		return toolErr("%s needs your owner's confirmation on their phone, and the confirmation could not be filed; try again", name), true
	}
	s.hostDecisions.put(ask.ID, hostDecision{owner: p.UserID, host: host, caller: c, tool: name, args: args, question: ask.Question})
	s.askFiled(ask)
	s.logger.Printf("step-up: %s from %s/%s waits for card %d (host officer %s on the machine; %s)", name, host, c.asker.MirrorSession, ask.ID, strings.Join(ids, ", "), firstNonBlank(spawn, "no agent spawn"))
	where := "on their phone"
	if s.passkeysServed(ctx) {
		// Only where a passkey can decide a card at all.
		where += ", or in muster with their passkey"
	}
	return toolJSON(map[string]any{"status": "awaiting_owner_confirmation", "card": ask.ID,
		"next": "Nothing was done yet. Your owner confirms or refuses this " + where + "; it then runs once, exactly as asked. " +
			"Do not call it again; read the outcome with the list tools later."}), true
}

// errHostDecisionNeedsPhone refuses a confirmation card's answer from any door
// but the owner's paired phone and a passkey ceremony made for that card.
var errHostDecisionNeedsPhone = fmt.Errorf("%w: this confirmation is answered only from your paired phone (a signed action) "+
	"or with your passkey on its card in muster; or decide the request itself in muster's web UI", attention.ErrInvalid)

// errHostDecisionLost refuses a card whose pending decision is gone (a restart).
var errHostDecisionLost = fmt.Errorf("%w: this confirmation's request was lost when muster restarted; the session must ask again", attention.ErrInvalid)

// errHostDecisionClosed refuses an answer to a card that is no longer open
// (dismissed, withdrawn or expired): its decision is dropped, nothing runs.
var errHostDecisionClosed = fmt.Errorf("%w: this confirmation was closed, so its request was dropped and nothing ran", attention.ErrNotOpen)

// errHostDecisionDecided refuses an answer to a card another door has
// already decided (answered, or being answered right now): nothing more runs.
var errHostDecisionDecided = fmt.Errorf("%w: this confirmation was already decided, so nothing more was run", attention.ErrNotOpen)

// errHostDecisionBusy answers a door that reaches a card another door is
// deciding at this moment: retry. Not ErrNotOpen — the other door's write may
// fail and put the decision back, and then this card is open again.
var errHostDecisionBusy = errors.New("step-up: this confirmation is being decided right now; try again in a moment")

// stepUpCardDecided reports whether card a was DECIDED — answered with one of
// its own two options, by the phone or the browser's passkey — as opposed to
// closed some other way (dismissed, expired, settled by its draft's decision
// on another door, whose answer says so: stepUpDraftAnswer).
func stepUpCardDecided(a attention.Ask) bool {
	return a.Status == attention.Answered && (a.Answer == hostDecisionConfirm || a.Answer == hostDecisionRefuse)
}

// hostDecisionForAnswer is answerAskFrom's hook for a confirmation card:
// handled false for any other ask. For a card: a door that is neither the
// owner's paired phone nor a passkey ceremony made for this card (stepUpDoor)
// is refused (the card stays open); an answer that is
// neither option is refused; otherwise the card is answered HERE — rec is the
// recorded answer, and answerAskFrom records nothing more — and then
// "Confirm" runs the decision once, as the session that asked, while
// "Refuse" runs nothing.
//
// 🔴 THE CARD IS CLAIMED BEFORE ITS DECISION RUNS. The answer is recorded by
// the store's update of an OPEN ask only (attention.Store.Answer), and only
// then does the call run: a dismissal racing the Confirm either lands first
// (the answer is refused as not open, nothing runs) or is itself refused as
// not open (the call runs, the card says Confirm). It used to run the call and
// record the answer after it, so a dismissal in between left the phone a 409
// for a call that HAD run.
//
// A read or a write of the card that fails for any other reason (the database,
// a timeout) is returned as it is, the decision left pending: the card may
// well be open, and the owner's next Confirm runs it.
func (s *Server) hostDecisionForAnswer(ctx context.Context, id int64, answer string) (handled bool, rec attention.Ask, err error) {
	pending, ok := s.hostDecisions.peek(id)
	if !ok {
		if s.ext.Attention == nil {
			return false, rec, nil
		}
		a, err := s.ext.Attention.Get(ctx, id)
		if err != nil || !a.Asker.System || a.Asker.Label != hostDecisionLabel {
			return false, rec, nil
		}
		switch {
		case stepUpCardDecided(a):
			// Decided on another door (the phone, the browser's passkey): the
			// loser of a race, or a retry, is told so, and nothing more runs.
			return true, rec, errHostDecisionDecided
		case a.Status != attention.Open:
			// Closed (and its decision dropped then): a retry of a refused
			// Confirm says so, never that a restart lost it.
			return true, rec, errHostDecisionClosed
		case s.hostDecisions.isDeciding(id):
			// 🔴 ANOTHER DOOR HAS CLAIMED IT AND IS RECORDING ITS ANSWER: the
			// card is still open for that moment, and withdrawing it as lost
			// (below) would make the winner's claim fail and run nothing. Not
			// "decided" either: the winner's write may yet fail and put the
			// decision back — so retry.
			return true, rec, errHostDecisionBusy
		}
		if _, err := s.cancelAsk(ctx, id); err != nil && !errors.Is(err, attention.ErrNotOpen) {
			s.logger.Printf("step-up: withdraw lost card %d: %v", id, err)
		}
		return true, rec, errHostDecisionLost
	}
	// Every check BEFORE the decision is taken: a refused answer leaves it
	// pending for the owner's phone.
	p, ok := authz.From(ctx)
	if !ok || p.UserID != pending.owner {
		return true, rec, errHostDecisionLost
	}
	// 🔴 TWO DOORS, BOTH A FRESH PROOF MADE FOR THIS DECISION: the paired
	// phone's signed action (its principal's door, set only after the
	// signature verified), or a passkey ceremony in the browser bound to THIS
	// card (stepup_web_confirm.go; its proof names the card it was made for).
	door := stepUpDoor(ctx, p, id)
	if door == "" || pending.phoneOnly && door != stepUpDoorPhone {
		return true, rec, errHostDecisionNeedsPhone
	}
	if answer != hostDecisionConfirm && answer != hostDecisionRefuse {
		return true, rec, fmt.Errorf("%w: answer %q or %q", attention.ErrInvalid, hostDecisionConfirm, hostDecisionRefuse)
	}
	if s.ext.Attention == nil {
		// Never: a card is filed in the attention store.
		return true, rec, errHostDecisionLost
	}
	// 🔴 A CLOSED CARD RUNS NOTHING. Dismissing a card (the Asks dismiss, the
	// phone's dismiss, a withdrawal) or its expiry closes the ask but cannot
	// reach this in-memory set, so its decision is still pending. The card's
	// own state is read here, on every door: a card that is not open (or is
	// gone) drops its decision and is refused as not open. A read that FAILED
	// says nothing about the card, so it drops nothing.
	card, err := s.ext.Attention.Get(ctx, id)
	switch {
	case errors.Is(err, attention.ErrNotFound):
		s.hostDecisions.take(id)
		return true, rec, errHostDecisionClosed
	case err != nil:
		return true, rec, fmt.Errorf("step-up: read card %d: %w", id, err)
	case card.Status != attention.Open:
		s.hostDecisions.take(id)
		return true, rec, errHostDecisionClosed
	}
	// 🔴 THE SINGLE-EXECUTION GUARD: one door claims the decision; any other
	// reaching the card now finds none (and isDeciding).
	d, ok := s.hostDecisions.claim(id)
	if !ok {
		// Another door claimed it between the read above and here: it is
		// being decided (retry: the winner's write may yet fail and put it
		// back), or was (the card answered), or was closed.
		if s.hostDecisions.isDeciding(id) {
			return true, rec, errHostDecisionBusy
		}
		switch cur, gerr := s.ext.Attention.Get(ctx, id); {
		case gerr == nil && stepUpCardDecided(cur):
			return true, rec, errHostDecisionDecided
		case gerr == nil && cur.Status != attention.Open:
			return true, rec, errHostDecisionClosed
		}
		return true, rec, errHostDecisionLost
	}
	// The claim: open → answered, or nothing (see above). The mark is cleared
	// however this ends — a panic in the store included — so a card is never
	// left "being decided" for good.
	rec, err = func() (attention.Ask, error) {
		defer s.hostDecisions.settled(id)
		rec, err := s.ext.Attention.Answer(ctx, id, answer)
		if err != nil && !errors.Is(err, attention.ErrNotOpen) && !errors.Is(err, attention.ErrNotFound) {
			// Not recorded, for a reason that is not the card's: pending
			// again, for the owner's next Confirm (put before the mark
			// clears, so no door sees the card as lost in between).
			s.hostDecisions.put(id, d)
		}
		return rec, err
	}()
	switch {
	case errors.Is(err, attention.ErrNotOpen), errors.Is(err, attention.ErrNotFound):
		// Closed between the read and this write: dropped (taken above).
		if cur, gerr := s.ext.Attention.Get(ctx, id); gerr == nil && stepUpCardDecided(cur) {
			return true, rec, errHostDecisionDecided
		}
		return true, rec, errHostDecisionClosed
	case err != nil:
		return true, rec, fmt.Errorf("step-up: record card %d's answer: %w", id, err)
	}
	s.auditStepUpDecision(ctx, p, id, d.tool, answer, door)
	if answer == hostDecisionRefuse {
		s.logger.Printf("step-up: card %d refused %s: %s from %s/%s not run", id, stepUpDoorWords(door), d.tool, d.host, d.caller.asker.MirrorSession)
		s.wakeStepUpDecided(ctx, rec, sessionwake.DecisionRefused)
		return true, rec, nil
	}
	var out string
	if d.confirm != nil {
		// muster's own decision (passkey_enrol.go), not a mirror door tool.
		out = d.confirm(ctx)
	} else {
		out = s.runStepUpTool(s.stepUpPrincipal(ctx, d.owner), d.host, d.caller, d.tool, d.args)
	}
	outcome, woke := "ran", sessionwake.DecisionConfirmed
	if isToolErr(out) {
		outcome, woke = "ran and was refused: "+mirror.OneLine(out, 200), sessionwake.DecisionConfirmedFailed
	}
	// The session whose call raised the card is told it ran (or was refused
	// by the tool) — the word only, never the tool's output.
	s.wakeStepUpDecided(ctx, rec, woke)
	s.logger.Printf("step-up: card %d confirmed %s: %s from %s/%s %s", id, stepUpDoorWords(door), d.tool, d.host, d.caller.asker.MirrorSession, outcome)
	return true, rec, nil
}

// stepUpPrincipal is the principal a CONFIRMED owner-authority call runs
// under: the SAME owner who just confirmed it, on the phone or with a passkey
// (hostDecisionForAnswer checked the principal's user is the card's owner), at the door the
// call was asked through — authz.ViaMirrorHost — so the tool runs exactly as
// the mirror door would have run it. Never another user id.
//
// 🔴 A TEAM OWNER PANICS HERE (stepUpTeamRefusal). ViaMirrorHost is
// team-acting since team sharing 2b.2 (a team host's MCP door), so
// WithPrincipal no longer refuses a team at this door; this does. A team has
// no phone, and a team host files no card — every tool in hostStepUpTools is
// refused for a team token first (teamHostTools) — so a team owner here is a
// bug, never a decision to run.
func (s *Server) stepUpPrincipal(ctx context.Context, owner int64) context.Context {
	p := s.principalFor(ctx, owner, authz.ViaMirrorHost)
	if p.Team {
		panic(stepUpTeamRefusal)
	}
	return authz.WithPrincipal(context.WithoutCancel(ctx), p)
}

// stepUpTeamRefusal is stepUpPrincipal's panic on a team owner.
const stepUpTeamRefusal = "step-up: a team has no phone, and a team host files no step-up card (teamHostTools refuses every owner-authority tool)"

// runStepUpTool runs a confirmed owner-authority tool exactly as the mirror
// door would have, for caller c of host. ONLY hostDecisionForAnswer calls it
// (TestTheStepUpGateIsALedger).
func (s *Server) runStepUpTool(ctx context.Context, host string, c toolCaller, name, args string) string {
	if name == routeReplyToolName {
		return s.toolRouteReply(ctx, c, args)
	}
	if name == officerItemEnqueueTool {
		return s.toolOfficerItemEnqueue(ctx, c, args)
	}
	if text, ok := s.dispatchPluginMessageRouteTool(ctx, c, name, args); ok {
		return text
	}
	if text, ok := s.dispatchHoldTool(ctx, c, name, args); ok {
		return text
	}
	if text, ok := s.dispatchCallerTool(ctx, c, name, args); ok {
		return text
	}
	return toolErr("unknown tool %q", name)
}

// hostStepUpToolNames is the gated set, sorted (for the docs and the tests).
func hostStepUpToolNames() []string {
	var out []string
	for n := range hostStepUpTools {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// spawnedTokenWhy is hostSpawnStepUp's reason for a host token marked as
// one whose host has started agents (mirror.Token.SpawnedAt).
const spawnedTokenWhy = "has started agents for the chief with this host token (it stays marked until the token is rotated)"

// hostSpawnStepUp is why the call's host is gated as a machine that starts
// agents for the chief — a clause for the card — or "" when it is not one.
// Every read is the SERVER's, and none is anything a window can set:
//
//   - the token the call came with is marked (mirror_tokens.spawned_at,
//     0150): its host advertised -agent-spawn in a hello, or had a host
//     dispatch reserved on it, at any time. 🔴 NOTHING BUT ROTATING THE
//     TOKEN CLEARS IT. A spawned agent runs as the machine's user and can
//     read the token, so neither a restart of muster, nor the host no longer
//     advertising -agent-spawn, nor another connection with the same token
//     says anything about who holds it;
//   - an agent muster started there for the chief may still be running: a
//     host dispatch of the owner's on host that is not ended. 🔴 THIS IS WHAT
//     A ROTATION CANNOT CLEAR: a new token is unmarked, but the agent still
//     running there can read it, so the host stays gated until its spawned
//     agents have ended (docs: rotate only after that). An unreadable record
//     gates (the closed direction);
//   - the host's hello says it starts agents now (HostView.AgentSpawn).
func (s *Server) hostSpawnStepUp(ctx context.Context, host string) string {
	if t, ok := ctx.Value(mirrorHostCtxKey{}).(mirror.Token); ok && t.Host == host && t.SpawnedAt != nil {
		return spawnedTokenWhy
	}
	if st := s.hostDispatches(); st != nil {
		rows, err := st.OpenHostDispatchesOnHost(ctx, host)
		switch {
		case err != nil:
			s.logger.Printf("step-up: read host %s's open dispatches: %v; its door stays gated", host, err)
			return "could not be checked for agents muster started there that are still running"
		case len(rows) > 0:
			return "has an agent muster started there for the chief that has not ended"
		}
	}
	if s.ext.Mirror != nil {
		for _, v := range s.ext.Mirror.Hosts(ctx) {
			if v.Host == host && len(v.AgentSpawn) > 0 {
				return "starts agents for the chief (-agent-spawn)"
			}
		}
	}
	return ""
}

// routeReplyToItsOwnSession reports whether args name a route still known to
// this owner that was routed to c's own session ON c's own host (the route
// records both: plugins.Route.SentToMirrorSessionOn; route_reply checks the
// binding again), and no agent muster started is still running on that host.
// Such a reply files no card on a machine that starts agents (operator
// decision); every other owner-authority call from it still does.
func (s *Server) routeReplyToItsOwnSession(ctx context.Context, c toolCaller, args string) bool {
	if !routesOn(s.ext) {
		return false
	}
	var in struct {
		RouteID string `json:"route_id"`
	}
	if err := json.Unmarshal([]byte(orEmptyJSON(args)), &in); err != nil || !plugins.ValidRouteID(in.RouteID) {
		return false
	}
	r, err := s.ext.Routes.Route(ctx, in.RouteID)
	if err != nil || !r.SentToMirrorSessionOn(c.asker.MirrorHost, c.asker.MirrorSession) {
		return false
	}
	// 🔴 NOT WHILE AN AGENT MUSTER STARTED MAY BE RUNNING THERE. The session a
	// caller names is its claim, and a spawned agent on this very host can
	// name the routed (owner's) pane: while any host dispatch on it is open,
	// the reply is carded like every other decision.
	st := s.hostDispatches()
	if st == nil {
		return true
	}
	open, err := st.OpenHostDispatchesOnHost(ctx, c.asker.MirrorHost)
	return err == nil && len(open) == 0
}

// stepUpHoldSummary is what a dispatch_approve card says it starts: the held
// request itself (holdSummary, and the Claude account), read from the hold
// args names — never only its id. "" and false when the hold cannot be read
// (no card is filed then: the tool would refuse it too).
func (s *Server) stepUpHoldSummary(ctx context.Context, args string) (string, bool) {
	if s.ext.ChiefDispatch == nil {
		return "", false
	}
	var in struct {
		HoldID string `json:"hold_id"`
	}
	if err := json.Unmarshal([]byte(orEmptyJSON(args)), &in); err != nil || strings.TrimSpace(in.HoldID) == "" {
		return "", false
	}
	hold, err := s.ext.ChiefDispatch.DispatchHold(ctx, strings.TrimSpace(in.HoldID))
	if err != nil {
		return "", false
	}
	var h heldDispatch
	if err := json.Unmarshal(hold.Detail, &h); err != nil || h.Target == "" {
		return "", false
	}
	sum := holdSummary(h)
	if h.CCAccount == "" {
		sum += " (muster chooses the Claude account)"
	}
	return sum + stepUpPromptBlock(h.Prompt), true
}

// stepUpPromptShown is how many characters of a held start's instructions a
// dispatch_approve card shows. They are its preview, with the hold's summary,
// which a card holds up to attention.MaxStepUpPreviewRunes (2,600) of;
// 1,500 here keeps the summary and the block under it. A held prompt may
// be up to chiefPromptMax (8 KiB), more than any card can carry, so a longer
// one is shown as an EXCERPT and the card says so.
const stepUpPromptShown = 1500

// stepUpPromptBlock is the card's instructions block: their length and the
// SHA-256 of the full text, then the text itself — in full when it fits, or
// labelled as an excerpt of its first stepUpPromptShown characters, with a
// plain warning that the rest is not on the card.
func stepUpPromptBlock(prompt string) string {
	if prompt == "" {
		return ""
	}
	r := []rune(prompt)
	head := fmt.Sprintf("%d characters, SHA-256 %s", len(r), promptDigest(prompt))
	if len(r) <= stepUpPromptShown {
		return "\n\nThe instructions in full (" + head + "):\n" + prompt
	}
	return fmt.Sprintf("\n\nAN EXCERPT of the instructions: the first %d of %s:\n%s…\n\n⚠ The other %d characters are NOT shown on this card. "+
		"Confirm only if you know what the rest says; otherwise refuse, and read the full text in the session that asked.",
		stepUpPromptShown, head, string(r[:stepUpPromptShown]), len(r)-stepUpPromptShown)
}

// stepUpCardConfirms is what a paired phone's challenge for an action on ask
// target says it confirms (the challenge's `confirms`): the confirmation
// card's own text, when target is a card whose decision is pending for the
// device's owner; "" for any other ask, a lost card, or another owner's.
//
// It is DISPLAY, never authority: the challenge it rides on is the same
// single-use, two-minute challenge bound to the card's id and the sha256 of
// the exact answer, and hostDecisionForAnswer still refuses every door but
// the paired phone. It is read from the pending decision itself — the text the
// card was filed with, in memory beside the decision it runs — so what the
// phone's prompt shows and what a Confirm runs cannot come from two places.
func (s *Server) stepUpCardConfirms(ctx context.Context, target string) string {
	id, err := strconv.ParseInt(target, 10, 64)
	if err != nil || id <= 0 {
		return ""
	}
	d, ok := s.hostDecisions.peek(id)
	if !ok {
		return ""
	}
	if p, ok := authz.From(ctx); !ok || p.UserID != d.owner {
		return ""
	}
	return d.question
}
