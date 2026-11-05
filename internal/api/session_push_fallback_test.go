package api

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/sessionstate"
	"github.com/ZacxDev/muster/internal/textplain"
)

// THE EPISODE PUSH KEEPS THE SESSION'S CODENAME WHEN ITS WINDOW LOOKUP FALLS
// BACK (session_push.go episodeWho, episodePlaceEntry). The regression: when
// no window held the episode's conversation any more (a /clear or a resume
// gave the window a new Claude session id), the title fell back to
// mirror.WhereTarget alone — "tmux scratch13:1 @ ws needs you" — although the
// host still labelled scratch13 ("mango"). The operator asked for the codename
// (2026-11-05, #453's round 0: "Codename (mango:1)").
//
// 🔴 Only names that exist at the base too (viewServer, fakeMirror,
// episodeNotification, the labelSID constants), so each case is shown red
// against the base.

func fallbackEpisode(session, window string, index int) sessionstate.Episode {
	return sessionstate.Episode{ID: 61, Owner: 1, Kind: sessionstate.KindMirror, Key: labelSID, Host: "ws",
		TmuxSession: session, Window: window, Index: index, State: sessionstate.StateNeedsYou}
}

func TestAFallenBackEpisodePushKeepsItsSessionsCodename(t *testing.T) {
	for _, c := range []struct {
		name        string
		offline     bool
		windows     []mirror.Window
		others      []mirror.HostView
		othersFirst bool // others listed before "ws"
		ep          sessionstate.Episode
		title, keep string
	}{
		{
			// The stored window is still listed, running ANOTHER conversation
			// now (a /clear): its label, its index now.
			name:    "the stored window runs another conversation now: its codename",
			windows: []mirror.Window{{Session: "scratch13", ID: "@3", Index: 1, Active: true, Label: "mango:1", Claude: true, ClaudeSession: labelSID2}},
			ep:      fallbackEpisode("scratch13", "@3", 1),
			title:   "mango:1 · tmux scratch13:1 @ ws needs you", keep: "tmux scratch13:1 @ ws",
		},
		{
			name:    "the stored window, renumbered and relabelled since: where it is now",
			windows: []mirror.Window{{Session: "scratch13", ID: "@3", Index: 5, Active: true, Label: "mango:5", Claude: true, ClaudeSession: labelSID2}},
			ep:      fallbackEpisode("scratch13", "@3", 1),
			title:   "mango:5 · tmux scratch13:5 @ ws needs you", keep: "tmux scratch13:5 @ ws",
		},
		{
			// The stored window is gone; its session is still listed and
			// labelled: the session's CODENAME with no index (an index would
			// name the active window, not the stored one), the stored target.
			name: "the stored window is gone, its session labelled: the session's codename and the stored target",
			windows: []mirror.Window{{Session: "scratch13", ID: "@7", Index: 4, Active: true, Label: "mango:4"},
				{Session: "scratch13", ID: "@8", Index: 6, Label: "mango:6"}},
			ep:    fallbackEpisode("scratch13", "@3", 1),
			title: "mango · tmux scratch13:1 @ ws needs you", keep: "tmux scratch13:1 @ ws",
		},
		{
			// The active window's index (2) differs from the stored one (7),
			// and from every other listed index: no index from the label may
			// leak into the name.
			name: "the stored window is gone, the active index differs from the stored one: no index on the codename",
			windows: []mirror.Window{{Session: "scratch11", ID: "@20", Index: 2, Active: true, Label: "wheat:2"},
				{Session: "scratch11", ID: "@21", Index: 3, Label: "wheat:3"}},
			ep:    fallbackEpisode("scratch11", "@19", 7),
			title: "wheat · tmux scratch11:7 @ ws needs you", keep: "tmux scratch11:7 @ ws",
		},
		{
			// A label not of the "<codename>:<its own index>" shape the host
			// builds names no codename the fallback can vouch for: the bare
			// stored target.
			name:    "the stored window is gone, a label not of the codename:index shape: the bare stored target",
			windows: []mirror.Window{{Session: "scratch11", ID: "@20", Index: 2, Active: true, Label: "wheat"}},
			ep:      fallbackEpisode("scratch11", "@19", 7),
			title:   "tmux scratch11:7 @ ws needs you", keep: "tmux scratch11:7 @ ws",
		},
		{
			name:    "an unlabelled session: the bare stored target, unchanged",
			windows: []mirror.Window{{Session: "build", ID: "@4", Index: 4, Active: true, Claude: true, ClaudeSession: labelSID2}},
			ep:      fallbackEpisode("build", "@4", 4),
			title:   "tmux build:4 @ ws needs you", keep: "tmux build:4 @ ws",
		},
		{
			// No label: the stored target as it was stored, even though the
			// window is listed at another index now.
			name:    "an unlabelled stored window, renumbered since: the bare stored target, unchanged",
			windows: []mirror.Window{{Session: "build", ID: "@4", Index: 6, Active: true, Claude: true, ClaudeSession: labelSID2}},
			ep:      fallbackEpisode("build", "@4", 4),
			title:   "tmux build:4 @ ws needs you", keep: "tmux build:4 @ ws",
		},
		{
			name:    "an unlabelled session on a host offline but still listed: unchanged (no mark, as before)",
			offline: true,
			windows: []mirror.Window{{Session: "build", ID: "@4", Index: 4, Active: true, Claude: true, ClaudeSession: labelSID2}},
			ep:      fallbackEpisode("build", "@4", 4),
			title:   "tmux build:4 @ ws needs you", keep: "tmux build:4 @ ws",
		},
		{
			name:    "a session the host no longer lists: the bare stored target, unchanged",
			windows: []mirror.Window{{Session: "scratch2", ID: "@9", Index: 2, Active: true, Label: "Gold:2"}},
			ep:      fallbackEpisode("scratch13", "@3", 1),
			title:   "tmux scratch13:1 @ ws needs you", keep: "tmux scratch13:1 @ ws",
		},
		{
			// Another host labels a session of the same name: never this one's.
			name:    "another host's same-named labelled session lends no codename",
			windows: []mirror.Window{{Session: "scratch2", ID: "@9", Index: 2, Active: true, Label: "Gold:2"}},
			others: []mirror.HostView{{Host: "tower", Online: true, Windows: mirror.NormalizeWindows([]mirror.Window{
				{Session: "scratch13", ID: "@3", Index: 1, Active: true, Label: "mango:1"}}, 0)}},
			othersFirst: true,
			ep:          fallbackEpisode("scratch13", "@3", 1),
			title:       "tmux scratch13:1 @ ws needs you", keep: "tmux scratch13:1 @ ws",
		},
		{
			name:    "a host offline but still listed: the codename and the offline mark, as the resolved path does",
			offline: true,
			windows: []mirror.Window{{Session: "scratch13", ID: "@3", Index: 1, Active: true, Label: "mango:1", Claude: true, ClaudeSession: labelSID2}},
			ep:      fallbackEpisode("scratch13", "@3", 1),
			title:   "mango:1 · tmux scratch13:1 @ ws (host offline) needs you", keep: "tmux scratch13:1 @ ws",
		},
		{
			// The key still resolves: the resolved path, unchanged.
			name:    "a resolved key: unchanged",
			windows: []mirror.Window{{Session: "scratch13", ID: "@3", Index: 2, Active: true, Label: "mango:2", Claude: true, ClaudeSession: labelSID}},
			ep:      fallbackEpisode("scratch13", "@3", 1),
			title:   "mango:2 · tmux scratch13:2 @ ws needs you", keep: "tmux scratch13:2 @ ws",
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
			if n.Title != c.title || n.TitleKeep != c.keep {
				t.Fatalf("title %q keep %q, want %q keep %q", n.Title, n.TitleKeep, c.title, c.keep)
			}
			if n.Body != "Needs you · just now" {
				t.Fatalf("body %q", n.Body)
			}
		})
	}
}

// TestALongFallenBackTitleKeepsItsCodename: a title too long for a phone is
// cut from the END (push.CutClusters at push.MaxNativeTitle), so the codename,
// which leads, is never what is cut — as sent (the title rule over it, then
// the cut), it still starts with the codename.
func TestALongFallenBackTitleKeepsItsCodename(t *testing.T) {
	long := "scratch13-" + strings.Repeat("x", 90)
	fm := &fakeMirror{online: true, statusReport: true,
		windows: []mirror.Window{{Session: long, ID: "@3", Index: 1, Active: true, Label: "mango:1", Claude: true, ClaudeSession: labelSID2}}}
	s, _ := viewServer(t, fm)
	n := s.episodeNotification(operatorCtx(), fallbackEpisode(long, "@3", 1), 0, nil)
	sent := push.CutClusters(textplain.TitleKeeping(n.Title, n.TitleKeep), push.MaxNativeTitle)
	if !strings.HasPrefix(sent, "mango:1 · tmux scratch13-") || !strings.HasSuffix(sent, "…") {
		t.Fatalf("as sent, the long title lost its codename: %q", sent)
	}
}
