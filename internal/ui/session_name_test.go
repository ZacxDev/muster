package ui

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/sessionname"
)

// TestANamedEntryHidesItsTechnicalIDs: a mirrored entry with a human name
// (internal/sessionname) is called by it on its pill and its sidebar row; its
// codename stays what the palette finds it by first (data-switcher-code); its
// host and tmux place are in its tooltip, and NOT written by the dock
// (no data-switcher-host). An unnamed entry keeps all of them as before.
func TestANamedEntryHidesItsTechnicalIDs(t *testing.T) {
	named := MirrorCardView{Host: "tower", Session: "w", Window: "@2", Label: "mango:2", Online: true,
		SessionName: sessionname.Name{Text: "Billing migration", Source: sessionname.SourceAuto, Technical: "mango:2 · tmux w:2 @ tower"}}
	plain := MirrorCardView{Host: "tower", Session: "v", Window: "@3", Label: "kiwi:3", Online: true}
	sw := NewSwitcher(nil, []MirrorCardView{named, plain}, nil, SwitcherCurrent{})
	html := renderString(t, switcherRefresh(sw))
	pill := func(code string) string {
		i := strings.Index(html, `data-switcher-code="`+code+`"`)
		if i < 0 {
			t.Fatalf("no pill for %s in:\n%s", code, html)
		}
		start := strings.LastIndex(html[:i], "<a ")
		return html[start : start+strings.Index(html[start:], "</a>")]
	}
	p := pill("mango:2")
	for _, want := range []string{`>Billing migration<`, `title="Billing migration · mango:2 · tmux w:2 @ tower"`, `data-switcher-name="Billing migration"`} {
		if !strings.Contains(p, want) {
			t.Errorf("the named pill lacks %s:\n%s", want, p)
		}
	}
	if strings.Contains(p, `data-switcher-host=`) || strings.Contains(p, `>mango:2<`) {
		t.Errorf("the named pill still spells a technical id:\n%s", p)
	}
	q := pill("kiwi:3")
	if !strings.Contains(q, `>kiwi:3<`) || !strings.Contains(q, `data-switcher-host="tower"`) || !strings.Contains(q, `title="kiwi:3"`) {
		t.Errorf("the unnamed pill changed:\n%s", q)
	}
}
