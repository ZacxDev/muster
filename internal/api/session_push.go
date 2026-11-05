package api

import (
	"context"
	"errors"
	"html"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/sessionstate"
	"github.com/ZacxDev/muster/internal/ui"
)

// ---------------------------------------------------------------------------
// THE EPISODE PUSH (docs/design/session-state.md, "Who decides what" and
// "Push"). OFF unless MUSTER_SESSION_STATE=1: with it off every push below is
// today's (the waiting diff's, the chief's notice).
//
// ONE PUSH PER EPISODE — one stop of one session — whatever the stop does in
// between (a permission prompt let through, then a stall): stall pushes and
// waiting pushes collapse into it. It is CLAIMED on the episode's pushed_at
// (sessionstate.Store.ClaimPush), so the waiting hook, the state pass and two
// replicas can never both send it, and it survives a restart: an open episode
// that pushed never pushes again, and one that never pushed is pushed by the
// next pass. Its notification is closed when the episode ends (a pass clears
// any ended episode whose push is still shown, so a stop that ended while
// muster was down is closed too).
//
// WHEN IT PUSHES (episodeVerdict):
//   - a WAITING window (a prompt muster cannot answer): at once, from the
//     mirror hub's change hook (attentionMirrorChanged) — never from the 30 s
//     poll, which only catches what the hook missed. Never through the chief.
//   - an IDLE stop, when the owner's chief judges stops (the feature on, the
//     owner live, a chief): on the chief's decision for the stop's streak —
//     a SENT nudge never pushes; a "leave" quiets the stop only when the
//     session asked nothing; a queued nudge holds the push until it is sent,
//     and a nudge that failed, was cancelled, capped or moved on pushes;
//     anything else pushes — a notify, every failure, and any SHADOW
//     decision (a needs-you nudge off its own live list included). No
//     decision within episodeChiefWait pushes; a later streak of a stop the
//     chief already judged (one decision per episode) is such a stop.
//   - an IDLE stop with no chief judging it (the feature off, the owner in
//     shadow mode, no chief): at once. 🔴 SHADOW MODE NEVER QUIETS A STOP.
//   - an agent conversation whose newest message is the OWNER's, with no
//     classifier: a quiet row, never a push (coordinator default N5 — the
//     owner spoke last).
//
// 🔴 NO SESSION TEXT. The notification names the session the way every
// attention surface names a mirrored window (sessionKeyWhere, mirror.Where:
// the host's label for it — a codename from the host's labels file, never
// anything the session wrote — then its tmux session and window INDEX and its
// host; or the agent) and says its state and how long — never a tool, a
// command, a focus title or anything the session said (episodeNotification).
// The label is what the waiting push named before episodes replaced it
// ("Waiting: mango:1 · tmux main:1 @ ws"); without it the push named only the
// raw tmux session, which the operator could not place.
// ---------------------------------------------------------------------------

// episodeRepushFloor: a session whose previous episode pushed within this long
// of this one opening is not pushed again for it (today's attentionRepushAfter,
// one rule).
const episodeRepushFloor = attentionRepushAfter

// episodeChiefWait bounds how long an idle stop's push waits for the chief's
// decision on it: past it, the stop is pushed (a chief that never judged it —
// out of budget, refused, or the stop excluded from its sweep — never quiets
// it).
const episodeChiefWait = 6 * time.Minute

// episodeNudgeHold bounds how long a queued nudge holds its stop's push: a
// nudge not sent by then is treated as failed, and the stop pushed.
const episodeNudgeHold = 10 * time.Minute

// episodeKeyPrefix is an episode's attention key prefix: its notification tag
// (attentionTag) and its tap route's key (attentionOpenURL).
const episodeKeyPrefix = "episode-"

// episodeKey is episode id's attention key.
func episodeKey(id int64) string { return episodeKeyPrefix + strconv.FormatInt(id, 10) }

// episodeKeyID is the episode id of an attention key, 0 when it is not one.
func episodeKeyID(key string) int64 {
	rest, ok := strings.CutPrefix(key, episodeKeyPrefix)
	if !ok || !episodeIDPattern.MatchString(rest) {
		return 0
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return 0
	}
	return id
}

var episodeIDPattern = regexp.MustCompile(`^[1-9][0-9]{0,18}$`)

// episodeFacts is what episodeVerdict reads about one open, unpushed episode.
type episodeFacts struct {
	// Waiting: the session waits on a prompt muster cannot answer.
	Waiting bool
	// Asked: the leave rule's input (sessionstate.Asked) — a classifier that
	// failed or never ran counts as asked.
	Asked bool
	// OwnerLast: the owner spoke last, known without a classifier (an agent
	// conversation's newest message is theirs, and no classifier ran).
	OwnerLast bool
	// ChiefJudges: the owner's chief judges idle stops live (the feature on,
	// the owner live, a chief).
	ChiefJudges bool
	// Decided: the chief's decision for the stop's streak, when there is one.
	Decided bool
	D       chiefnudge.Decision
	// OpenFor is how long the episode has been open.
	OpenFor time.Duration
	// Now is the time of the observation (for a queued nudge's hold).
	Now time.Time
}

// The verdicts.
const (
	verdictPush = "push"
	verdictHold = "hold" // not now: a later pass decides
)

// episodeVerdict is what one open, unpushed episode does now: verdictPush,
// verdictHold, or a sessionstate.Quiet* reason (a quiet row, no push).
//
// 🔴 A FAILURE NEVER QUIETS A STOP: only a sent nudge, a "leave" on a stop
// that asked nothing, and the owner having spoken last are quiet; everything
// unknown pushes, at once or after its bound.
func episodeVerdict(f episodeFacts) string {
	switch {
	case f.Waiting:
		return verdictPush
	case f.OwnerLast:
		return sessionstate.QuietOwnerLast
	case !f.ChiefJudges:
		return verdictPush
	case !f.Decided:
		if f.OpenFor < episodeChiefWait {
			return verdictHold
		}
		return verdictPush
	case f.D.Mode != chiefnudge.ModeLive:
		return verdictPush // shadow mode never quiets a stop
	}
	switch f.D.Outcome {
	case chiefnudge.OutcomeSent:
		return sessionstate.QuietNudged
	case chiefnudge.OutcomeQueued:
		if f.Now.Sub(f.D.DecidedAt) < episodeNudgeHold {
			return verdictHold
		}
		return verdictPush
	case chiefnudge.OutcomeLeft:
		if f.Asked {
			return verdictPush // "Push it anyway" (operator decision 12)
		}
		return sessionstate.QuietLeft
	}
	return verdictPush
}

// streakDecisionReader is the nudge store's read of a streak's decision
// (chiefnudge.PG.ForStreak); a store without it decides nothing.
type streakDecisionReader interface {
	ForStreak(ctx context.Context, kind, key, streak string) (chiefnudge.Decision, bool, error)
}

// episodeChiefJudges reports whether octx's owner's chief judges idle stops
// live: the feature on, the owner live, a chief.
func (s *Server) episodeChiefJudges(octx context.Context, owner int64) bool {
	if !s.chiefNudgesOn() || !s.chiefNudgeLive(owner) {
		return false
	}
	_, err := s.chiefAgent(octx)
	return err == nil
}

// streakDecision is the chief's decision for ep's streak, if any. A failed
// read is no decision (the stop then pushes after episodeChiefWait). 🔴 A
// DECISION TAKEN IN ANOTHER EPISODE IS NOT THIS STOP'S: a window gone and back
// with the same idle-since opens a new stop on the same streak, and the old
// stop's sent nudge or leave must not quiet it.
func (s *Server) streakDecision(octx context.Context, ep sessionstate.Episode) (chiefnudge.Decision, bool) {
	r, ok := s.ext.ChiefNudges.(streakDecisionReader)
	if !ok || s.ext.ChiefNudges == nil || ep.StreakKey == "" {
		return chiefnudge.Decision{}, false
	}
	d, found, err := r.ForStreak(octx, ep.Kind, ep.Key, ep.StreakKey)
	if err != nil {
		s.logger.Printf("session state: episode %d: the chief's decision: %v", ep.ID, err)
		return chiefnudge.Decision{}, false
	}
	if found && d.EpisodeID != 0 && d.EpisodeID != ep.ID {
		return chiefnudge.Decision{}, false
	}
	return d, found
}

// settleEpisode is one open episode's push, for the state pass: f's verdict
// pushes it, holds it, or records why it is quiet. idle is the session's idle
// time, for the notification; hosts the owner's windows as the pass read them
// (episodeNotification).
func (s *Server) settleEpisode(octx context.Context, ep sessionstate.Episode, f episodeFacts, idle time.Duration, hosts []mirror.HostView) {
	if !ep.Open() || !ep.PushedAt.IsZero() {
		return
	}
	if !f.Waiting && f.ChiefJudges && !f.OwnerLast {
		f.D, f.Decided = s.streakDecision(octx, ep)
	}
	switch v := episodeVerdict(f); v {
	case verdictHold:
	case verdictPush:
		s.pushEpisode(octx, ep, idle, hosts)
	default:
		if ep.Quiet != v {
			if err := s.ext.Episodes.SetQuiet(octx, ep.ID, v); err != nil {
				s.logger.Printf("session state: episode %d: quiet: %v", ep.ID, err)
			}
		}
	}
}

// pushEpisode claims and sends ep's one push. 🔴 THE REPUSH FLOOR DELAYS, IT
// NEVER SUPPRESSES: while its session's previous episode pushed less than
// episodeRepushFloor ago, ep is held (quiet repush_floor, drawn as such), and
// a later pass pushes it once the floor has passed — if it is still open. A
// stop is never left silent by the floor. hosts is the owner's windows as the
// caller read them, nil for none read (episodeNotification).
func (s *Server) pushEpisode(octx context.Context, ep sessionstate.Episode, idle time.Duration, hosts []mirror.HostView) {
	prev, err := s.ext.Episodes.PreviousPush(octx, ep.Kind, ep.Key, ep.Seq)
	if err != nil {
		s.logger.Printf("session state: episode %d: previous push: %v", ep.ID, err)
	} else if !prev.IsZero() && s.now().Sub(prev) < episodeRepushFloor {
		if err := s.ext.Episodes.SetQuiet(octx, ep.ID, sessionstate.QuietRepushFloor); err != nil {
			s.logger.Printf("session state: episode %d: quiet: %v", ep.ID, err)
		}
		return
	}
	claimed, err := s.ext.Episodes.ClaimPush(octx, ep.ID, s.now())
	if err != nil {
		s.logger.Printf("session state: episode %d: claim the push: %v", ep.ID, err)
		return
	}
	if !claimed {
		return // pushed already — by the hook, another pass or a replica
	}
	s.goNotify(ep.Owner, push.EventAttention, s.episodeNotification(octx, ep, idle, hosts), nil)
	s.attentionChanged(ep.Owner, episodeKey(ep.ID))
}

// episodeNotification is ep's notification: WHO and WHERE in the title, the
// state and how long in the body — 🔴 NO SESSION TEXT (the file's header).
// hosts is the owner's windows as the caller already read them (the state
// pass's one read, the hub hook's); nil reads them once here (episodeHosts).
func (s *Server) episodeNotification(octx context.Context, ep sessionstate.Episode, idle time.Duration, hosts []mirror.HostView) RouterNotification {
	key := episodeKey(ep.ID)
	if hosts == nil {
		hosts = s.episodeHosts(octx, ep)
	}
	who, keep := s.episodeWho(octx, ep, hosts)
	verb := " is stalled"
	if ep.State == sessionstate.StateNeedsYou {
		verb = " needs you"
	}
	return RouterNotification{Type: attentionNotificationType, ID: key, Title: who + verb, Body: episodeBody(ep.State, idle),
		Tag: attentionTag(key), Data: map[string]string{"url": attentionOpenURL(key)}, TitleKeep: keep}
}

// episodeWho names ep's session, for its push title and its "Stalled /
// waiting" row: for a mirrored window, the window running its conversation
// NOW over hosts, by its session key, as every attention surface names it
// (sessionKeyWhere: "mango:1 · tmux main:1 @ ws", "(host offline)" after it
// when its host has dropped and the hub still lists it — entryWhere; keep,
// its tmux target, kept verbatim by the title rule) — so a window renumbered
// since names its new index. Only when no window holds the conversation now,
// the place stored on the episode in the same shape without the label
// (mirror.WhereTarget: "tmux main:1 @ ws"). An agent conversation is named by
// its agent.
func (s *Server) episodeWho(octx context.Context, ep sessionstate.Episode, hosts []mirror.HostView) (who, keep string) {
	if ep.Kind == sessionstate.KindAgent {
		if s.ext.Agents != nil && ep.AgentID > 0 {
			if a, err := s.ext.Agents.Get(octx, ep.AgentID); err == nil {
				return a.Name, a.Name
			}
		}
		return "An agent", ""
	}
	if ep.Host != "" && ep.Key != "" {
		if _, where, keep, ok := sessionKeyWhere(hosts, ep.Host, ep.Key); ok {
			return where, keep
		}
	}
	if ep.TmuxSession != "" && ep.Host != "" {
		where := mirror.WhereTarget(ep.Host, ep.TmuxSession, ep.Window, ep.Index)
		return where, where
	}
	// No tmux target or no host stored: as much of the place as is known.
	target := ep.TmuxSession + ":" + strconv.Itoa(ep.Index)
	if ep.TmuxSession == "" {
		target = "A session"
	}
	if ep.Host == "" {
		return target, target
	}
	return target + " @ " + ep.Host, target
}

// episodeHosts is ctx's owner's windows, read once for episodes (episodeWho),
// nil when none of them is a mirrored window or muster mirrors nothing.
func (s *Server) episodeHosts(ctx context.Context, eps ...sessionstate.Episode) []mirror.HostView {
	if s.ext.Mirror == nil {
		return nil
	}
	for _, ep := range eps {
		if ep.Kind == sessionstate.KindMirror {
			return s.ext.Mirror.Hosts(ctx)
		}
	}
	return nil
}

// episodeBody is an episode push's body: its state and idle time.
func episodeBody(state string, idle time.Duration) string {
	label := "Stalled"
	if state == sessionstate.StateNeedsYou {
		label = "Needs you"
	}
	if idle < time.Minute {
		return label + " · just now"
	}
	return label + " · idle " + attentionAge(idle)
}

// clearEpisode closes ended episode ep's notification when it was shown, and
// records that it was.
func (s *Server) clearEpisode(octx context.Context, ep sessionstate.Episode) {
	if ep.PushedAt.IsZero() || !ep.ClearedAt.IsZero() {
		return
	}
	s.attentionClear(ep.Owner, episodeKey(ep.ID))
	if err := s.ext.Episodes.MarkCleared(octx, ep.ID, s.now()); err != nil {
		s.logger.Printf("session state: episode %d: cleared: %v", ep.ID, err)
	}
	s.attentionChanged(ep.Owner, episodeKey(ep.ID))
}

// clearEndedEpisodes closes every ended episode of octx's owner whose push is
// still shown: the restart rule (a stop that ended while muster was down, or
// one ended in a pass that read it before it pushed).
func (s *Server) clearEndedEpisodes(octx context.Context, owner int64) {
	eps, err := s.ext.Episodes.Uncleared(octx)
	if err != nil {
		s.logger.Printf("session state: owner %d: uncleared episodes: %v", owner, err)
		return
	}
	for _, ep := range eps {
		s.clearEpisode(octx, ep)
	}
}

// --- the waiting hook --------------------------------------------------------

// plainWaitingItem reports whether it is a waiting window muster cannot
// answer — a plain "session waiting" item, which session state turns into an
// episode — rather than an answerable ask (answerableItem), which keeps its
// own full-text push.
func plainWaitingItem(it attentionItem) bool {
	return it.Kind == attentionKindSession && !answerableItem(it)
}

// episodeCover is the episode key session state keeps waiting item it under,
// and where it is: ok only for a plain waiting window (plainWaitingItem) on a
// host the state pass reads (online, reporting status) whose window it lists
// (stateWindowKey, the stall facts' own rule). 🔴 ONLY A COVERED ITEM LEAVES
// TODAY'S WAITING DIFF: a window session state cannot key (an older host that
// sends no Claude session id, a runtime window naming no conversation) keeps
// today's summary-only push, never silence.
func episodeCover(hs []mirror.HostView, it attentionItem) (key, runtime string, w mirror.Window, ok bool) {
	if !plainWaitingItem(it) {
		return "", "", mirror.Window{}, false
	}
	for _, h := range hs {
		if h.Host != it.card.ref.Host || !h.Online || !h.StatusReport {
			continue
		}
		for _, x := range h.Windows {
			if x.Session == it.card.ref.Session && x.ID == it.card.ref.Window {
				key, runtime, _ = stateWindowKey(x, sessionStateListsHermes)
				return key, runtime, x, key != ""
			}
		}
	}
	return "", "", mirror.Window{}, false
}

// sessionStateListsHermes: session state lists Hermes windows (design default
// D3). 🔴 ONE CONSTANT FOR BOTH SIDES of the shared rule — the state pass's
// stall facts and the hook's episodeCover — so they cannot drift apart and
// leave a window the hook covers that the pass never sees, or the reverse.
const sessionStateListsHermes = true

// episodeWaitingPushes opens (or finds) the episode of each covered waiting
// window in items (episodeCover) and pushes it at once: the waiting push, from
// the mirror hub's change hook. With send false it SEEDS instead: the episode
// is claimed as pushed and nothing is sent — for a window today's path already
// pushed in this stop (it gained its key mid-wait), so the stop is not pushed
// a second time.
func (s *Server) episodeWaitingPushes(octx context.Context, owner int64, items []attentionItem, hs []mirror.HostView, send bool) {
	for _, it := range items {
		key, runtime, w, ok := episodeCover(hs, it)
		if !ok {
			continue
		}
		ep, has, err := s.ext.Episodes.OpenFor(octx, sessionstate.KindMirror, key)
		if err != nil {
			s.logger.Printf("session state: waiting %s: %v", it.Key, err)
			continue
		}
		if !has {
			// Its streak, as the pass keys it (windowStreakKey): a wait the owner
			// already handled (opened the window, say) is not opened again while
			// it is the same wait (sessionstate.Handled).
			streak := windowStreakKey(s.ext.Mirror.AgentState(octx, it.card.ref.Host, w.Session, w.ID))
			opened, claimed, err := s.ext.Episodes.Open(octx, sessionstate.Episode{Kind: sessionstate.KindMirror, Key: key,
				State: sessionstate.StateNeedsYou, Waiting: true, Asked: true, Runtime: runtime,
				Host: it.card.ref.Host, TmuxSession: w.Session, Window: w.ID, Index: w.Index, StreakKey: streak})
			if err != nil {
				s.logger.Printf("session state: waiting %s: open: %v", it.Key, err)
				continue
			}
			if !claimed {
				// Another process opened this stop this instant (it pushes), or
				// the owner already handled this wait.
				continue
			}
			ep = opened
		}
		switch {
		case !ep.PushedAt.IsZero():
		case send:
			ep.State = sessionstate.StateNeedsYou
			s.pushEpisode(octx, ep, 0, hs)
		default:
			if _, err := s.ext.Episodes.ClaimPush(octx, ep.ID, s.now()); err != nil {
				s.logger.Printf("session state: waiting %s: seed: %v", it.Key, err)
			}
		}
	}
}

// --- the tap route -----------------------------------------------------------

// episodeOpenTarget is where an episode's notification opens for owner now:
// its session's current window (by session key, so a renumbered window still
// resolves), or its agent's conversation; else its row on the queue page.
func (s *Server) episodeOpenTarget(ctx context.Context, owner, id int64, key string) string {
	if s.ext.Episodes == nil {
		return attentionItemPath(key)
	}
	ep, ok, err := s.ext.Episodes.Get(ctx, id)
	if err != nil && !errors.Is(err, context.Canceled) {
		s.logger.Printf("attention: the tap target of %s: %v (opening the queue instead)", key, err)
	}
	// 🔴 OWNER-SCOPED TWICE: Get reads under row-level security, and the
	// explicit check holds with it rolled back.
	if err != nil || !ok || ep.Owner != owner {
		return attentionItemPath(key)
	}
	switch ep.Kind {
	case sessionstate.KindAgent:
		if ep.AgentID > 0 {
			if p := s.askingAgentHref(ctx, owner, ep.AgentID, ep.ChatSessionID); p != "" {
				return p
			}
		}
	case sessionstate.KindMirror:
		if s.ext.Mirror != nil && ep.Host != "" {
			if e, ok := sessionKeyEntry(s.ext.Mirror.Hosts(ctx), ep.Host, ep.Key); ok {
				return ui.MirrorRef{Host: e.Host, Session: e.Session, Window: e.Window}.ViewPath()
			}
		}
	}
	return attentionItemPath(key)
}

// --- the Attention section: "Stalled / waiting" -----------------------------

// episodeQuietText is what a quiet row says of why it did not push.
var episodeQuietText = map[string]string{
	sessionstate.QuietLeft:        "quiet: the chief left it, and it asked nothing",
	sessionstate.QuietNudged:      "quiet: the chief's nudge was sent",
	sessionstate.QuietOwnerLast:   "quiet: you spoke last",
	sessionstate.QuietRepushFloor: "held: the session was pushed under 2 minutes ago",
}

// askListHTML is the Asks list's <li>s, as the page draws them and as its live
// refresh redraws them: the items (pageAttentionItems), the recent answers,
// then the "Stalled / waiting" section (episodeSectionHTML). With a section
// drawn, the list's "Nothing is waiting on you." line is dropped — a stalled
// session is waiting on you.
func (s *Server) askListHTML(ctx context.Context, items []attentionItem, now time.Time) string {
	list := askTabListHTML(s.pageAttentionItems(ctx, items), s.answeredAsksNow(ctx, now), now)
	sec := s.episodeSectionHTML(ctx, now)
	if sec != "" {
		list = strings.Replace(list, attentionEmptyHTML, "", 1)
	}
	return list + sec
}

// pageAttentionItems is items as the Asks list draws them: with session state
// on, a waiting window session state covers (episodeCover — the hook's own
// rule) is a row of the "Stalled / waiting" section (episodeSectionHTML), not
// a list item too. One it cannot key stays a list item, as it stays in
// today's push.
func (s *Server) pageAttentionItems(ctx context.Context, items []attentionItem) []attentionItem {
	if !s.sessionStateOn() || s.ext.Mirror == nil {
		return items
	}
	hs := s.ext.Mirror.Hosts(ctx)
	out := items[:0:0]
	for _, it := range items {
		if _, _, _, ok := episodeCover(hs, it); !ok {
			out = append(out, it)
		}
	}
	return out
}

// episodeSectionHTML is the "Stalled / waiting" section of ctx's owner's Asks
// list: one plain row per open episode, oldest first — where the session is,
// its state and for how long, and a link to it. 🔴 NO ANSWER BOX: a row is
// looked at, never answered. A quiet row is drawn muted and says why it has
// not pushed. It is ONE <li> of the list, so the list's live refresh
// (handleAttentionItems) redraws it: an Asks page left open shows a window
// that starts waiting. "" with session state off, or with no open episode.
func (s *Server) episodeSectionHTML(ctx context.Context, now time.Time) string {
	if !s.sessionStateOn() {
		return ""
	}
	eps, err := s.ext.Episodes.OpenEpisodes(ctx)
	if err == nil && len(eps) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<li class="flex flex-col gap-2" data-attention-episodes-item><section class="flex flex-col gap-2" data-testid="attention-episodes" aria-labelledby="attention-episodes-h">
<h2 id="attention-episodes-h" class="text-sm font-semibold text-fg">Stalled / waiting</h2>
<ul class="flex flex-col gap-2" data-attention-episodes>
`)
	if err != nil {
		s.logger.Printf("attention: episodes: %v", err)
		b.WriteString(`<li class="text-sm text-fg2" data-attention-episodes-error>The stalled and waiting sessions could not be read.</li>` + "\n")
	}
	esc := html.EscapeString
	hosts := s.episodeHosts(ctx, eps...)
	for _, ep := range eps {
		who, _ := s.episodeWho(ctx, ep, hosts)
		state := "Stalled"
		if ep.State == sessionstate.StateNeedsYou {
			state = "Needs you"
		}
		cls := flowsCardClass
		quiet := ""
		// A quiet reason is only ever held by an unpushed episode (SetQuiet
		// refuses a pushed one, ClaimPush clears it).
		if q, ok := episodeQuietText[ep.Quiet]; ok {
			cls += " opacity-60"
			quiet = ` · <span data-attention-episode-quiet="` + esc(ep.Quiet) + `">` + esc(q) + `</span>`
		}
		key := episodeKey(ep.ID)
		b.WriteString(`<li id="` + esc(key) + `" class="` + cls + `" data-attention-episode data-state="` + esc(ep.State) + `">
<span class="text-xs text-fg2"><span class="font-semibold">` + esc(state) + `</span> · <code data-attention-episode-where>` + esc(who) + `</code> · <time datetime="` +
			ep.OpenedAt.UTC().Format(time.RFC3339) + `">` + esc(attentionAge(now.Sub(ep.OpenedAt))) + `</time>` + quiet + `</span>
<a href="` + esc(attentionOpenURL(key)) + `" class="inline-flex min-h-[44px] items-center text-sm text-accent underline" data-attention-episode-open>` + askViewInSession + `</a>
</li>
`)
	}
	b.WriteString("</ul>\n</section></li>\n")
	return b.String()
}
