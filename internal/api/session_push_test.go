package api

import (
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/sessionstate"
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
