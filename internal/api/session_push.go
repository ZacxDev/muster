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
//     anything else pushes. No decision within episodeChiefWait pushes.
//   - an IDLE stop with no chief judging it (the feature off, the owner in
//     shadow mode, no chief): at once. 🔴 SHADOW MODE NEVER QUIETS A STOP.
//   - an agent conversation whose newest message is the OWNER's, with no
//     classifier: a quiet row, never a push (coordinator default N5 — the
//     owner spoke last).
//
// 🔴 NO SESSION TEXT. The notification names the session (tmux session and
// window INDEX, host; or the agent) and says its state and how long — never a
// window's name (often LLM-written), a tool, a command, a focus title or
// anything the session said (episodeNotification).
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
// read is no decision (the stop then pushes after episodeChiefWait).
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
	return d, found
}

// settleEpisode is one open episode's push, for the state pass: f's verdict
// pushes it, holds it, or records why it is quiet. idle is the session's idle
// time, for the notification.
func (s *Server) settleEpisode(octx context.Context, ep sessionstate.Episode, f episodeFacts, idle time.Duration) {
	if !ep.Open() || !ep.PushedAt.IsZero() {
		return
	}
	if !f.Waiting && f.ChiefJudges && !f.OwnerLast {
		f.D, f.Decided = s.streakDecision(octx, ep)
	}
	switch v := episodeVerdict(f); v {
	case verdictHold:
	case verdictPush:
		s.pushEpisode(octx, ep, idle)
	default:
		if ep.Quiet != v {
			if err := s.ext.Episodes.SetQuiet(octx, ep.ID, v); err != nil {
				s.logger.Printf("session state: episode %d: quiet: %v", ep.ID, err)
			}
		}
	}
}

// pushEpisode claims and sends ep's one push, unless its session's previous
// episode pushed within episodeRepushFloor of this one opening (then it is
// quiet for good: the same stop back again).
func (s *Server) pushEpisode(octx context.Context, ep sessionstate.Episode, idle time.Duration) {
	if ep.Quiet == sessionstate.QuietRepushFloor {
		return
	}
	prev, err := s.ext.Episodes.PreviousPush(octx, ep.Kind, ep.Key, ep.Seq)
	if err != nil {
		s.logger.Printf("session state: episode %d: previous push: %v", ep.ID, err)
	} else if !prev.IsZero() && ep.OpenedAt.Sub(prev) < episodeRepushFloor {
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
	s.goNotify(ep.Owner, push.EventAttention, s.episodeNotification(octx, ep, idle), nil)
	s.attentionChanged(ep.Owner, episodeKey(ep.ID))
}

// episodeNotification is ep's notification: WHO and WHERE in the title, the
// state and how long in the body — 🔴 NO SESSION TEXT (the file's header).
func (s *Server) episodeNotification(octx context.Context, ep sessionstate.Episode, idle time.Duration) RouterNotification {
	key := episodeKey(ep.ID)
	who, keep := s.episodeWho(octx, ep)
	verb := " is stalled"
	if ep.State == sessionstate.StateNeedsYou {
		verb = " needs you"
	}
	return RouterNotification{Type: attentionNotificationType, ID: key, Title: who + verb, Body: episodeBody(ep.State, idle),
		Tag: attentionTag(key), Data: map[string]string{"url": attentionOpenURL(key)}, TitleKeep: keep}
}

// episodeWho names ep's session: "<tmux session>:<window index> @ <host>" for
// a mirrored window (keep: the tmux target, kept verbatim by the title rule),
// the agent's name for an agent conversation.
func (s *Server) episodeWho(octx context.Context, ep sessionstate.Episode) (who, keep string) {
	if ep.Kind == sessionstate.KindAgent {
		if s.ext.Agents != nil && ep.AgentID > 0 {
			if a, err := s.ext.Agents.Get(octx, ep.AgentID); err == nil {
				return a.Name, a.Name
			}
		}
		return "An agent", ""
	}
	target := ep.TmuxSession + ":" + strconv.Itoa(ep.Index)
	if ep.TmuxSession == "" {
		target = "A session"
	}
	if ep.Host == "" {
		return target, target
	}
	return target + " @ " + ep.Host, target
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

// episodeWaitingPushes opens (or finds) the episode of each of octx's owner's
// plain waiting windows in items and pushes it at once: the waiting push,
// from the mirror hub's change hook. A window whose session key is unknown
// (a Hermes window naming no conversation) is the state pass's, which keys it
// as the stall facts do.
func (s *Server) episodeWaitingPushes(octx context.Context, owner int64, items []attentionItem, hs []mirror.HostView) {
	for _, it := range items {
		if !plainWaitingItem(it) || it.claudeSession == "" {
			continue
		}
		e, ok := sessionKeyEntry(hs, it.host, it.claudeSession)
		if !ok {
			continue
		}
		ep, has, err := s.ext.Episodes.OpenFor(octx, sessionstate.KindMirror, it.claudeSession)
		if err != nil {
			s.logger.Printf("session state: waiting %s: %v", it.Key, err)
			continue
		}
		if !has {
			opened, claimed, err := s.ext.Episodes.Open(octx, sessionstate.Episode{Kind: sessionstate.KindMirror, Key: it.claudeSession,
				State: sessionstate.StateNeedsYou, Waiting: true, Asked: true, Runtime: windowRuntime(hs, e),
				Host: e.Host, TmuxSession: e.Session, Window: e.Window, Index: e.Index})
			if err != nil {
				s.logger.Printf("session state: waiting %s: open: %v", it.Key, err)
				continue
			}
			if !claimed {
				// Another process opened this stop this instant: it pushes.
				continue
			}
			ep = opened
		}
		if ep.PushedAt.IsZero() {
			ep.State = sessionstate.StateNeedsYou
			s.pushEpisode(octx, ep, 0)
		}
	}
}

// windowRuntime is the runtime of e's window as the stall facts key it
// (nudgeWindowKey): "" for a Claude Code window, else
// mirror.Window.AgentRuntime.
func windowRuntime(hs []mirror.HostView, e mirror.SessionView) string {
	for _, h := range hs {
		if h.Host != e.Host {
			continue
		}
		for _, w := range h.Windows {
			if w.Session == e.Session && w.ID == e.Window && !w.Claude {
				return w.AgentRuntime()
			}
		}
	}
	return ""
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
	sessionstate.QuietRepushFloor: "quiet: the same stop again within 2 minutes",
}

// pageAttentionItems is items as the Asks list draws them: with session state
// on, a plain waiting window is a row of the "Stalled / waiting" section
// (episodeSectionHTML), not a list item too.
func (s *Server) pageAttentionItems(items []attentionItem) []attentionItem {
	if !s.sessionStateOn() {
		return items
	}
	out := items[:0:0]
	for _, it := range items {
		if !plainWaitingItem(it) {
			out = append(out, it)
		}
	}
	return out
}

// episodeSectionHTML is the "Stalled / waiting" section of ctx's owner's Asks
// page: one plain row per open episode, oldest first — where the session is,
// its state and for how long, and a link to it. 🔴 NO ANSWER BOX: a row is
// looked at, never answered. A quiet row is drawn muted and says why it did
// not push. "" with session state off.
func (s *Server) episodeSectionHTML(ctx context.Context, now time.Time) string {
	if !s.sessionStateOn() {
		return ""
	}
	eps, err := s.ext.Episodes.OpenEpisodes(ctx)
	var b strings.Builder
	b.WriteString(`<section class="flex flex-col gap-2" data-testid="attention-episodes" aria-labelledby="attention-episodes-h">
<h2 id="attention-episodes-h" class="text-sm font-semibold text-fg">Stalled / waiting</h2>
<ul class="flex flex-col gap-2" data-attention-episodes>
`)
	switch {
	case err != nil:
		s.logger.Printf("attention: episodes: %v", err)
		b.WriteString(`<li class="text-sm text-fg2" data-attention-episodes-error>The stalled and waiting sessions could not be read.</li>` + "\n")
	case len(eps) == 0:
		b.WriteString(`<li class="text-sm text-fg2" data-attention-episodes-empty>No session is stalled or waiting.</li>` + "\n")
	}
	esc := html.EscapeString
	for _, ep := range eps {
		who, _ := s.episodeWho(ctx, ep)
		state := "Stalled"
		if ep.State == sessionstate.StateNeedsYou {
			state = "Needs you"
		}
		cls := flowsCardClass
		quiet := ""
		if q, ok := episodeQuietText[ep.Quiet]; ok && ep.PushedAt.IsZero() {
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
	b.WriteString("</ul>\n</section>\n")
	return b.String()
}
