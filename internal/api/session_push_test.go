package api

import (
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sessionstate"
	"github.com/ZacxDev/muster/internal/ui"
)

// TestEpisodeVerdict pins the push rule (session_push.go) case by case, with
// literal expectations.
func TestEpisodeVerdict(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	live := func(outcome string) chiefnudge.Decision {
		return chiefnudge.Decision{Mode: chiefnudge.ModeLive, Outcome: outcome, DecidedAt: now.Add(-time.Minute)}
	}
	shadow := chiefnudge.Decision{Mode: chiefnudge.ModeShadow, Outcome: chiefnudge.OutcomeLeft, DecidedAt: now.Add(-time.Minute)}
	for _, c := range []struct {
		name string
		f    episodeFacts
		want string
	}{
		{"waiting pushes at once", episodeFacts{Waiting: true, ChiefJudges: true}, verdictPush},
		{"owner spoke last is quiet", episodeFacts{OwnerLast: true}, sessionstate.QuietOwnerLast},
		{"no chief pushes", episodeFacts{}, verdictPush},
		{"undecided holds", episodeFacts{ChiefJudges: true, OpenFor: time.Minute}, verdictHold},
		{"undecided past the bound pushes", episodeFacts{ChiefJudges: true, OpenFor: episodeChiefWait + time.Second}, verdictPush},
		{"a shadow leave on a stop that asked nothing pushes", episodeFacts{ChiefJudges: true, Decided: true, D: shadow}, verdictPush},
		{"sent is quiet", episodeFacts{ChiefJudges: true, Decided: true, D: live(chiefnudge.OutcomeSent)}, sessionstate.QuietNudged},
		{"queued holds", episodeFacts{ChiefJudges: true, Decided: true, D: live(chiefnudge.OutcomeQueued), Now: now}, verdictHold},
		{"queued past the hold pushes", episodeFacts{ChiefJudges: true, Decided: true, D: live(chiefnudge.OutcomeQueued), Now: now.Add(episodeNudgeHold)}, verdictPush},
		{"failed pushes", episodeFacts{ChiefJudges: true, Decided: true, D: live(chiefnudge.OutcomeFailed)}, verdictPush},
		{"capped pushes", episodeFacts{ChiefJudges: true, Decided: true, D: live(chiefnudge.Suppressed(chiefnudge.GuardHourlyCap))}, verdictPush},
		{"moved on pushes", episodeFacts{ChiefJudges: true, Decided: true, D: live(chiefnudge.Suppressed(chiefnudge.GuardMovedOn))}, verdictPush},
		{"notify pushes", episodeFacts{ChiefJudges: true, Decided: true, D: live(chiefnudge.Suppressed(chiefnudge.GuardEpisode))}, verdictPush},
		// Step 3a: a notify, and every failure, pushes — never a leave.
		{"a 3a notify pushes", episodeFacts{ChiefJudges: true, Decided: true, D: live("notified")}, verdictPush},
		{"a shadow notify pushes", episodeFacts{ChiefJudges: true, Decided: true,
			D: chiefnudge.Decision{Mode: chiefnudge.ModeShadow, Outcome: "notified"}}, verdictPush},
		{"a dropped nudge pushes", episodeFacts{ChiefJudges: true, Decided: true, D: live(chiefnudge.Suppressed(chiefnudge.GuardSuperseded))}, verdictPush},
		{"a failed call pushes", episodeFacts{ChiefJudges: true, Decided: true, D: live(chiefnudge.Suppressed(chiefnudge.GuardChiefError))}, verdictPush},
		{"an unreadable answer pushes", episodeFacts{ChiefJudges: true, Decided: true, D: live(chiefnudge.Suppressed(chiefnudge.GuardInvalid))}, verdictPush},
		{"lanes full past the bound pushes", episodeFacts{ChiefJudges: true, Decided: true, D: live(chiefnudge.Suppressed(chiefnudge.GuardLanesFull))}, verdictPush},
		{"chief not ready past the bound pushes", episodeFacts{ChiefJudges: true, Decided: true, D: live(chiefnudge.Suppressed(chiefnudge.GuardChiefNotReady))}, verdictPush},
		{"a shadow would_send pushes", episodeFacts{ChiefJudges: true, Decided: true,
			D: chiefnudge.Decision{Mode: chiefnudge.ModeShadow, Outcome: chiefnudge.OutcomeWouldSend}}, verdictPush},
		{"leave, asked nothing, is quiet", episodeFacts{ChiefJudges: true, Decided: true, D: live(chiefnudge.OutcomeLeft)}, sessionstate.QuietLeft},
		{"leave on a stop that asked pushes", episodeFacts{Asked: true, ChiefJudges: true, Decided: true, D: live(chiefnudge.OutcomeLeft)}, verdictPush},
	} {
		if got := episodeVerdict(c.f); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

// TestEpisodeBody pins the push body: state and idle time, nothing else.
func TestEpisodeBody(t *testing.T) {
	for _, c := range []struct {
		state string
		idle  time.Duration
		want  string
	}{
		{sessionstate.StateNeedsYou, 0, "Needs you · just now"},
		{sessionstate.StateStalled, 12*time.Minute + 30*time.Second, "Stalled · idle " + attentionAge(12*time.Minute+30*time.Second)},
	} {
		if got := episodeBody(c.state, c.idle); got != c.want {
			t.Errorf("%s %v: %q, want %q", c.state, c.idle, got, c.want)
		}
	}
}

// TestEpisodeCover pins the hook's half of the shared rule case by case: only a
// plain waiting window on a host the state pass reads (online AND reporting
// status) whose window the stall facts list is covered — never an older
// host's Hermes window, whose runtime muster no longer knows.
func TestEpisodeCover(t *testing.T) {
	claude := mirror.Window{Session: "main", ID: "@1", Index: 1, Claude: true, ClaudeSession: "0b6e9c7c-9d0d-4a8e-9c62-3f1b2a4d5e6f", Status: mirror.StatusWaiting}
	hermes := mirror.Window{Session: "main", ID: "@1", Index: 1, Runtime: "hermes", AgentSession: "hermes-synthetic-1", Status: mirror.StatusWaiting}
	item := attentionItem{Kind: attentionKindSession, card: attentionCard{ref: ui.MirrorRef{Host: "ws", Session: "main", Window: "@1"}}}
	host := func(w mirror.Window, online, report bool) []mirror.HostView {
		return []mirror.HostView{{Host: "ws", Online: online, StatusReport: report, Windows: []mirror.Window{w}}}
	}
	for _, c := range []struct {
		name string
		hs   []mirror.HostView
		want bool
	}{
		{"a keyed window on a healthy host", host(claude, true, true), true},
		{"an older host's Hermes window naming its conversation", host(hermes, true, true), false},
		{"an offline host", host(claude, false, true), false},
		{"an online host that reports no status", host(claude, true, false), false},
	} {
		if _, _, _, ok := episodeCover(c.hs, item); ok != c.want {
			t.Errorf("%s: covered %v, want %v", c.name, ok, c.want)
		}
	}
}
