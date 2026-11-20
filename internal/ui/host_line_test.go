package ui

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/sessionname"
)

// TestTheHostLineIsNotRepeatedWhenTheTitleNamesTheHost: two computers'
// windows that share a label are titled apart by their computer ("mango ·
// atlas"); the "On atlas" line under it would say it again, so it is not
// drawn — on the rendered view and the terminal. Controls: a qualifier that is
// not the host ("window 2"), and a human name (it carries no qualifier), keep
// the line.
func TestTheHostLineIsNotRepeatedWhenTheTitleNamesTheHost(t *testing.T) {
	human := sessionname.Name{Text: "Billing migration", Source: sessionname.SourceManual}
	for name, c := range map[string]struct {
		view MirrorViewView
		term AgentTerminalView
		line bool
	}{
		"title names the host": {
			MirrorViewView{Host: "atlas", Session: "s1", Window: "@1", Label: "mango:1", Qualifier: "atlas"},
			AgentTerminalView{Name: "s1", DisplayName: "mango", RawName: "s1", TitleQualifier: "atlas", Badge: MirrorBadgeText("atlas"), BadgeHost: "atlas", BackHref: "/agents"},
			false,
		},
		"qualifier is not the host": {
			MirrorViewView{Host: "atlas", Session: "s1", Window: "@1", Label: "mango:1", Qualifier: "window 2"},
			AgentTerminalView{Name: "s1", DisplayName: "mango", RawName: "s1", TitleQualifier: "window 2", Badge: MirrorBadgeText("atlas"), BadgeHost: "atlas", BackHref: "/agents"},
			true,
		},
		"a human name": {
			MirrorViewView{Host: "atlas", Session: "s1", Window: "@1", Label: "mango:1", Qualifier: "atlas", SessionName: human},
			AgentTerminalView{Name: "s1", DisplayName: "mango", RawName: "s1", TitleQualifier: "atlas", SessionName: human, Badge: MirrorBadgeText("atlas"), BadgeHost: "atlas", BackHref: "/agents"},
			true,
		},
	} {
		for page, html := range map[string]string{
			"view": renderString(t, mirrorViewTitleButton(c.view)),
			"term": renderString(t, terminalTitle(c.term)),
		} {
			if got := strings.Contains(html, `data-term-badge`); got != c.line {
				t.Errorf("%s, %s: the On-atlas line drawn %v, want %v:\n%s", name, page, got, c.line, html)
			}
		}
	}
}
