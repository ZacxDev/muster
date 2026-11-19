// Package fleet is the chief's view of its owner's whole fleet: each
// session's fleet state, the digest's groups (docs/chief.md "Fleet digest"),
// and the digest's marks (migration 0147, store.go).
//
// Everything in this file is a pure function of what internal/api's fleet
// reader read (fleet_read.go): the hub's own windows, the session-state
// pass's open episodes, the pull requests muster recorded the sessions
// creating, and GitHub's answer for each. No model is called and no session
// text is read here; the digest is muster's own rendering, so it costs no
// chief turn.
//
// 🔴 ONE RULE, ONE PLACE: StateOf decides a session's fleet state and GroupOf
// its digest group, and both fleet_list and the digest card read them. A
// second copy of either is a digest that disagrees with what the chief reads.
package fleet

import (
	"sort"
	"time"
)

// The fleet states (fleet_list's `state`).
const (
	// StateWorking: no open stop — the session is busy, or idle under the
	// stall threshold.
	StateWorking = "working"
	// StateStalled: an open stop the stall classifier did not read as a
	// question.
	StateStalled = "stalled"
	// StateNeedsYou: an open stop that asks the owner something (the
	// session-state pass's needs_you, not waiting on a prompt).
	StateNeedsYou = "needs_you"
	// StateWaiting: a prompt or picker is on screen — the host reports the
	// window waiting now, or its open stop opened waiting.
	StateWaiting = "waiting"
)

// Episode is a session's open stop as the fleet reads it (a
// session_episodes row, internal/sessionstate).
type Episode struct {
	ID int64
	// State is the episode's own state: sessionstate's needs_you or stalled.
	State string
	// Waiting: it opened (or was last seen) waiting on a prompt.
	Waiting bool
	// Since is when the stop opened.
	Since time.Time
	// JevLabel is the stall classifier's label ("" when it did not run).
	JevLabel string
}

// The episode states StateOf reads (sessionstate.StateNeedsYou and
// StateStalled, spelled here so this package imports nothing).
const (
	episodeNeedsYou = "needs_you"
	episodeStalled  = "stalled"
)

// StateOf is a session's fleet state: waitingNow is whether its host reports
// it waiting on a prompt NOW, ep its open episode (nil for none).
//
// Precedence: waiting (now, or the stop opened waiting) > needs_you > stalled >
// working. 🔴 A WAIT ON SCREEN IS WAITING BEFORE ITS EPISODE OPENS: the
// session-state pass holds a waiting window's episode for its grace, and the
// owner is no less needed in that minute.
func StateOf(waitingNow bool, ep *Episode) string {
	switch {
	case waitingNow:
		return StateWaiting
	case ep == nil:
		return StateWorking
	case ep.Waiting:
		return StateWaiting
	case ep.State == episodeNeedsYou:
		return StateNeedsYou
	case ep.State == episodeStalled:
		return StateStalled
	}
	return StateWorking
}

// The pull-request states a fleet row carries (fleet_list's prs[].state).
const (
	PROpen    = "open"
	PRMerged  = "merged"
	PRClosed  = "closed"
	PRUnknown = "unknown" // GitHub was not asked, or did not answer
)

// PR is one pull request a session created (session_prs), newest first, with
// GitHub's state for it.
type PR struct {
	Repo   string `json:"repo"`
	Number int64  `json:"number"`
	State  string `json:"state"`
	// HeadSHA12 is the head commit's first 12 hex ("" when unknown).
	HeadSHA12 string `json:"head_sha12,omitempty"`
	// ClosedAt is when GitHub says it closed (zero while open or unknown).
	ClosedAt time.Time `json:"-"`
}

// The digest's groups, in the order the card draws them.
const (
	GroupNeedsYou  = "needs_you"
	GroupWaitingCI = "waiting_ci"
	GroupWorking   = "working"
	GroupDone      = "done"
	GroupStalled   = "stalled"
)

// GroupOrder is the card's order of the groups.
var GroupOrder = []string{GroupNeedsYou, GroupWaitingCI, GroupWorking, GroupDone, GroupStalled}

// GroupLabel is each group's name on the card.
//
// 🔴 "Waiting on checks" SAYS WHAT IS KNOWN, NOT MORE: muster does not read
// checks yet, so the group is "stopped with an open pull request", and the
// card says the checks are unknown (GroupNote).
var GroupLabel = map[string]string{
	GroupNeedsYou:  "Needs you",
	GroupWaitingCI: "Waiting on checks",
	GroupWorking:   "Working",
	GroupDone:      "Done",
	GroupStalled:   "Stalled",
}

// GroupNote is the one line a group's open view starts with, where the label
// alone would claim more than muster knows.
var GroupNote = map[string]string{
	GroupWaitingCI: "Stopped with a pull request open. Its checks are not read yet, so whether CI is still running is unknown.",
	GroupDone:      "Stopped, its latest pull request merged or closed, nothing queued to it.",
}

// GroupOf is the digest group of a session in fleet state state whose pull
// requests are prs (newest first) and that has a message queued to it or not
// (inFlight). "" for a state the digest does not group (none known).
//
//   - waiting or needs_you: Needs you;
//   - working: Working;
//   - stalled with its latest pull request open: Waiting on CI;
//   - stalled with its latest pull request merged or closed and nothing
//     queued to it: Done, can wrap;
//   - any other stall (no pull request, its state unknown, or a message on
//     its way): Stalled.
//
// 🔴 UNKNOWN IS NOT DONE: a pull request GitHub did not answer for leaves the
// session Stalled, never Done — a wrap proposed on a guess would close a
// session whose work is still open.
func GroupOf(state string, prs []PR, inFlight bool) string {
	switch state {
	case StateWaiting, StateNeedsYou:
		return GroupNeedsYou
	case StateWorking:
		return GroupWorking
	case StateStalled:
		if len(prs) > 0 {
			switch prs[0].State {
			case PROpen:
				return GroupWaitingCI
			case PRMerged, PRClosed:
				if !inFlight {
					return GroupDone
				}
			}
		}
		return GroupStalled
	}
	return ""
}

// Session is one of the owner's sessions as the digest draws it.
type Session struct {
	// ID is fleet_list's id ("mirror:<key>", "agent:<id>").
	ID   string
	Kind string
	Name string
	// Where is a mirrored session's window as the owner finds it in a
	// terminal ("" for an agent).
	Where string
	// Href is where "Open session" goes: the rendered view, the terminal, or
	// the agent's conversation.
	Href    string
	State   string
	Episode *Episode
	PRs     []PR
	// InFlight: a message is queued to it.
	InFlight bool
}

// Ask is one of the chief's own open ask_owner cards: the digest's Needs you
// group lists them beside the sessions, so there is one place to clear them.
type Ask struct {
	ID        int64
	Question  string
	CreatedAt time.Time
}

// Group is one of the digest's groups.
type Group struct {
	Key, Label string
	Sessions   []Session
	// Asks is the chief's open cards (the Needs you group only).
	Asks []Ask
}

// Count is the group's count: its sessions and asks.
func (g Group) Count() int { return len(g.Sessions) + len(g.Asks) }

// Since is what moved since the owner's previous look (Digest.Since).
type Since struct {
	// Baseline is the previous look; zero: none recorded, nothing is drawn.
	Baseline time.Time
	// NewStops: sessions whose open stop opened after it.
	NewStops int
	// Finished: Done sessions whose latest pull request closed after it.
	Finished int
	// NewQuestions: the chief's open cards asked after it.
	NewQuestions int
}

// Digest is the card: every group in GroupOrder (empty ones included, so a
// chip row never moves), the total, and what moved since the last look.
type Digest struct {
	Total  int
	Groups []Group
	Since  Since
}

// Build is the digest of sessions and the chief's open asks, against
// baseline (the owner's previous look; zero for none). Each session is in
// exactly one group (GroupOf), or none when its state is unknown — the total
// counts every session given.
func Build(sessions []Session, asks []Ask, baseline time.Time) Digest {
	by := map[string]*Group{}
	d := Digest{Total: len(sessions), Since: Since{Baseline: baseline}}
	for _, k := range GroupOrder {
		by[k] = &Group{Key: k, Label: GroupLabel[k]}
	}
	for _, s := range sessions {
		k := GroupOf(s.State, s.PRs, s.InFlight)
		g, ok := by[k]
		if !ok {
			continue
		}
		g.Sessions = append(g.Sessions, s)
		if baseline.IsZero() {
			continue
		}
		if s.Episode != nil && s.Episode.Since.After(baseline) {
			d.Since.NewStops++
		}
		if k == GroupDone && s.PRs[0].ClosedAt.After(baseline) {
			d.Since.Finished++
		}
	}
	for _, a := range asks {
		by[GroupNeedsYou].Asks = append(by[GroupNeedsYou].Asks, a)
		if !baseline.IsZero() && a.CreatedAt.After(baseline) {
			d.Since.NewQuestions++
		}
	}
	for _, k := range GroupOrder {
		g := by[k]
		// The oldest stop first: the one that has waited longest.
		sort.SliceStable(g.Sessions, func(i, j int) bool {
			return since(g.Sessions[i]).Before(since(g.Sessions[j]))
		})
		sort.SliceStable(g.Asks, func(i, j int) bool { return g.Asks[i].CreatedAt.Before(g.Asks[j].CreatedAt) })
		d.Groups = append(d.Groups, *g)
	}
	return d
}

// since is s's stop's opening, or the far future for a session with none (so
// working sessions keep their reader's order).
func since(s Session) time.Time {
	if s.Episode == nil {
		return time.Unix(1<<40, 0)
	}
	return s.Episode.Since
}
