package api

import (
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sessionstate"
)

// THE EPISODE PUSH NAMES THE WINDOW BY ITS LABEL (session_push.go episodeWho,
// attention_notice.go sessionKeyWhere). The regression: session-state PR 2
// titled an episode's push from the RAW tmux session stored on the episode
// ("scratch11:8 @ <host> needs you"), so the operator could not tell which
// window it meant — every other surface, and the waiting push it replaced
// ("Waiting: mango:1 · tmux main:1 @ ws"), names the window by the host's
// label for it.
//
// 🔴 Only names that exist at the base too (viewServer, fakeMirror,
// episodeNotification), so each case is shown red against the base.

const (
	labelSID  = "4c1f6a2e-8b3d-4e5f-9a7b-1c2d3e4f5a6b"
	labelSID2 = "7e6d5c4b-3a29-4180-b7c6-d5e4f3a2b1c0"
)

func TestAnEpisodePushNamesItsWindowByItsLabelResolvedNow(t *testing.T) {
	needs := func(ep sessionstate.Episode) sessionstate.Episode {
		ep.ID, ep.Owner, ep.Kind, ep.Host, ep.State = 41, 1, sessionstate.KindMirror, "ws", sessionstate.StateNeedsYou
		return ep
	}
	for _, c := range []struct {
		name        string
		offline     bool
		windows     []mirror.Window
		others      []mirror.HostView
		othersFirst bool // others listed before "ws"
		ep          sessionstate.Episode
		title       string
		titleKeep   string
	}{
		{
			name:    "a labelled window is named by its label, then its tmux target and host",
			windows: []mirror.Window{{Session: "scratch11", ID: "@3", Index: 8, Label: "mango:8", Claude: true, ClaudeSession: labelSID}},
			ep:      needs(sessionstate.Episode{Key: labelSID, TmuxSession: "scratch11", Window: "@3", Index: 8}),
			title:   "mango:8 · tmux scratch11:8 @ ws needs you", titleKeep: "tmux scratch11:8 @ ws",
		},
		{
			name: "a window renumbered (and relabelled by its host) since the episode opened names where it is NOW",
			windows: []mirror.Window{{Session: "scratch11", ID: "@3", Index: 2, Label: "mango:2", Claude: true, ClaudeSession: labelSID},
				{Session: "scratch11", ID: "@9", Index: 8, Label: "mango:8", Claude: true, ClaudeSession: labelSID2}},
			ep:    needs(sessionstate.Episode{Key: labelSID, TmuxSession: "scratch11", Window: "@3", Index: 8}),
			title: "mango:2 · tmux scratch11:2 @ ws needs you", titleKeep: "tmux scratch11:2 @ ws",
		},
		{
			// The other host lists the SAME session id (a host-local key: two
			// hosts' transcripts can collide), listed first, so only the host
			// filter (claudeSessionEntry) picks this host's window.
			name:    "a label and session id another host also lists: this host's window, qualified by its host",
			windows: []mirror.Window{{Session: "scratch11", ID: "@3", Index: 1, Label: "mango:1", Claude: true, ClaudeSession: labelSID}},
			others: []mirror.HostView{{Host: "tower", Online: true, Windows: mirror.NormalizeWindows([]mirror.Window{
				{Session: "mango", ID: "@1", Index: 1, Label: "mango:1", Claude: true, ClaudeSession: labelSID}}, 0)}},
			othersFirst: true,
			ep:          needs(sessionstate.Episode{Key: labelSID, TmuxSession: "scratch11", Window: "@3", Index: 1}),
			title:       "mango:1 · tmux scratch11:1 @ ws needs you", titleKeep: "tmux scratch11:1 @ ws",
		},
		{
			name:    "an unlabelled window is its tmux target and host",
			windows: []mirror.Window{{Session: "build", ID: "@4", Index: 4, Claude: true, ClaudeSession: labelSID}},
			ep:      needs(sessionstate.Episode{Key: labelSID, TmuxSession: "build", Window: "@4", Index: 4}),
			title:   "tmux build:4 @ ws needs you", titleKeep: "tmux build:4 @ ws",
		},
		{
			// The stored window @3 is gone; its session is still listed and
			// labelled, so the stored place keeps the session's codename
			// (episodePlaceEntry; session_push_fallback_test.go). As first
			// built this read the bare "tmux scratch11:8 @ ws".
			name:    "no window holds the conversation now: the place stored on the episode, under its session's codename",
			windows: []mirror.Window{{Session: "scratch11", ID: "@9", Index: 8, Label: "mango:8", Claude: true, ClaudeSession: labelSID2}},
			ep:      needs(sessionstate.Episode{Key: labelSID, TmuxSession: "scratch11", Window: "@3", Index: 8}),
			title:   "mango:8 · tmux scratch11:8 @ ws needs you", titleKeep: "tmux scratch11:8 @ ws",
		},
		{
			name:    "a host offline but still listed (its grace period) is marked, as a notice is",
			offline: true,
			windows: []mirror.Window{{Session: "scratch11", ID: "@3", Index: 8, Label: "mango:8", Claude: true, ClaudeSession: labelSID}},
			ep:      needs(sessionstate.Episode{Key: labelSID, TmuxSession: "scratch11", Window: "@3", Index: 8}),
			title:   "mango:8 · tmux scratch11:8 @ ws (host offline) needs you", titleKeep: "tmux scratch11:8 @ ws",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			fm := &fakeMirror{online: !c.offline, statusReport: true, windows: c.windows, others: c.others}
			s, _ := viewServer(t, fm)
			var hosts []mirror.HostView // nil: episodeNotification reads them
			if c.othersFirst {
				hs := fm.Hosts(operatorCtx())
				hosts = append(hs[1:], hs[0])
			}
			n := s.episodeNotification(operatorCtx(), c.ep, 0, hosts)
			if n.Title != c.title || n.TitleKeep != c.titleKeep {
				t.Fatalf("title %q keep %q, want %q keep %q", n.Title, n.TitleKeep, c.title, c.titleKeep)
			}
			if n.Body != "Needs you · just now" {
				t.Fatalf("body %q", n.Body)
			}
		})
	}
}

// TestAnAgentEpisodeIsStillNamedByItsAgent: the label rule is a mirrored
// window's; an agent conversation's push is unchanged — its agent's name, or
// "An agent" when it cannot be read.
func TestAnAgentEpisodeIsStillNamedByItsAgent(t *testing.T) {
	fm := &fakeMirror{online: true, windows: []mirror.Window{{Session: "main", ID: "@1", Index: 1, Label: "mango:1", Claude: true, ClaudeSession: labelSID}}}
	s, _ := viewServer(t, fm)
	ep := sessionstate.Episode{ID: 42, Owner: 1, Kind: sessionstate.KindAgent, Key: labelSID, AgentID: 8, State: sessionstate.StateStalled}
	if n := s.episodeNotification(operatorCtx(), ep, 0, nil); n.Title != plainName+" is stalled" || n.TitleKeep != plainName {
		t.Fatalf("title %q keep %q", n.Title, n.TitleKeep)
	}
	ep.AgentID = 999 // no such agent
	if n := s.episodeNotification(operatorCtx(), ep, 0, nil); n.Title != "An agent is stalled" || n.TitleKeep != "" {
		t.Fatalf("an unreadable agent: title %q keep %q", n.Title, n.TitleKeep)
	}
}
