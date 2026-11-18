package api

import (
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
)

// TestTheHintNeverSuggestsAnOpenCodeWindow (PR #537 round 1): the plugin
// session hint is an offer to route, and route_to_session takes Claude Code
// windows only — so an opencode window whose focus history matches the message
// exactly as well as a Claude Code window's is NOT hinted, while
// sessions_overview still LISTS it (a read). Control: the Claude Code window
// with the same focus is hinted.
func TestTheHintNeverSuggestsAnOpenCodeWindow(t *testing.T) {
	g, _, _, _, _ := overviewReplay(t)
	const ocSID = "ses_0hintfixture1a2b3c4d"
	ocKey := "opencode:" + ocSID
	const cmSID = "4c5d6e7f-8091-4a2b-9c3d-4e5f60718293"
	for _, key := range []string{ocKey, cmSID} {
		g.setFocus(t, focus.MirrorSubject(key), "Upgrading node-7k: drain and reboot", "")
	}
	now := time.Now().Unix()
	g.srv.ext.Mirror = overviewMirror{hosts: []mirror.HostView{
		overviewHost("box-3", true,
			mirror.Window{Session: "work", ID: "@1", Index: 1, Active: true, Claude: true, ClaudeSession: cmSID, Activity: now},
			mirror.Window{Session: "work", ID: "@2", Index: 2, Runtime: mirror.RuntimeOpenCode, AgentSession: ocSID, Activity: now}),
	}}
	h := g.srv.pluginSessionHints(g.asOwner, g.chief, plugins.Message{ID: 9, Subject: "node-7k upgrade", Body: overviewMessage})
	var ids []string
	for _, s := range h.Sessions {
		ids = append(ids, s.ID)
	}
	got := strings.Join(ids, ",")
	if strings.Contains(got, ocKey) {
		t.Errorf("the hint suggests the opencode window %s (route_to_session cannot take it): %s", ocKey, got)
	}
	if !strings.Contains(got, "mirror:"+cmSID) {
		t.Errorf("control: the Claude Code window with the same focus is not hinted: %s", got)
	}
	// sessions_overview still lists the opencode window.
	if out := g.srv.toolSessionsOverview(g.asOwner, g.chief, `{}`); !strings.Contains(out, `"id":"mirror:`+ocKey+`"`) ||
		!strings.Contains(out, `"runtime":"opencode"`) {
		t.Errorf("sessions_overview no longer lists the opencode window: %s", out)
	}
}
