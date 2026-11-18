package ui

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/sessionname"
)

// "SHOW TMUX SESSIONS" ON THE BAR'S PILLS AND THE DOCK'S ROWS
// (internal/api notify_prefs.go). internal/api decides each entry's TmuxAt /
// TmuxCode by its one rule (names.go tmuxPlace); the ui only draws them.

// namedTmuxCard is a window called "Billing migration", its codename
// "mango:1" on host atlas; at and code are what the api's rule set ("" off).
func namedTmuxCard(at, code string) MirrorCardView {
	return MirrorCardView{Host: "atlas", Session: "scratch3", Window: "@5", Label: "mango:1", Online: true,
		SessionName: sessionname.Name{Text: "Billing migration", Source: sessionname.SourceAuto, Technical: "mango:1 · tmux scratch3:5 @ atlas"},
		TmuxAt:      at, TmuxCode: code}
}

func pillOf(t *testing.T, c MirrorCardView) string {
	t.Helper()
	s := NewSwitcher(nil, []MirrorCardView{c}, nil, SwitcherCurrent{})
	return renderRaw(t, switcherEntry(s.All[0]))
}

// pillOffGolden is a named window's pill with the switch OFF, as it was drawn
// before the switch existed (captured from the tree before this change).
const pillOffGolden = `<a href="/mirror/terminal?host=atlas&amp;session=scratch3&amp;window=%405" hx-boost="false" data-switcher-entry="mirror" title="Billing migration · mango:1 · tmux scratch3:5 @ atlas" data-switcher-code="mango:1" data-switcher-title="Billing migration" data-switcher-words="scratch3 atlas" class="press group inline-flex h-11 shrink-0 items-center" data-switcher-name="Billing migration"><span class="inline-flex h-8 items-center gap-1 rounded-full px-2 text-xs font-semibold ring-1 ring-inset bg-s1/70 text-fg ring-line transition group-hover:bg-s2"><span data-host-icon="laptop" role="img" aria-label="Host: atlas" title="Host: atlas" class="inline-flex h-4 w-4 shrink-0 items-center justify-center text-fg2"><svg class="h-4 w-4 shrink-0" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true" focusable="false"><rect x="4" y="5" width="16" height="11" rx="1"/><path d="M2 19h20"/></svg></span><span class="whitespace-nowrap">Billing migration</span></span></a>`

// TestAPillIsByteIdenticalWithTheSwitchOff: off (no TmuxAt / TmuxCode), the
// pill is exactly the pre-switch markup — the literal golden above.
func TestAPillIsByteIdenticalWithTheSwitchOff(t *testing.T) {
	if got := pillOf(t, namedTmuxCard("", "")); got != pillOffGolden {
		t.Fatalf("the OFF pill changed:\n got  %s\n want %s", got, pillOffGolden)
	}
}

// TestAPillWithTheSwitchOnDrawsItsCodenameLine: on, the pill carries the
// dock's value (data-switcher-tmux, the whole place) and draws a second line,
// the codename alone, capped and cut (truncate) — text inside the link, so it
// is part of the pill's accessible name. Everything else about the pill is the
// OFF pill's (the label span the dock reads is still the first match).
func TestAPillWithTheSwitchOnDrawsItsCodenameLine(t *testing.T) {
	got := pillOf(t, namedTmuxCard("mango:1 @ atlas", "mango:1"))
	for _, want := range []string{
		` data-switcher-tmux="mango:1 @ atlas" class="press group`,
		`<span class="` + switcherTwoLineClass + `"><span class="whitespace-nowrap">Billing migration</span><span data-switcher-tmux-line="" class="` + switcherTmuxLineClass + `">mango:1</span></span>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the ON pill lacks\n%s\nin:\n%s", want, got)
		}
	}
	if !strings.Contains(switcherTmuxLineClass, "truncate") || !strings.Contains(switcherTmuxLineClass, "max-w-") {
		t.Errorf("the codename line is not capped and cut: %q", switcherTmuxLineClass)
	}
	// The pill keeps its 32px face: the two lines sit inside it.
	if !strings.Contains(got, `class="inline-flex h-8 items-center`) {
		t.Errorf("the ON pill is no longer h-8:\n%s", got)
	}
}

// TestAPillWithTheSwitchOnIsTheOffPillPlusItsCodename: the WHOLE ON pill,
// byte for byte — the OFF golden with exactly two edits and nothing else: the
// dock's data-switcher-tmux on the link, and the label wrapped with its
// codename line (literal classes, not the constants, so a class change is
// seen). A mutant that changes anything else about the pill when on fails here.
func TestAPillWithTheSwitchOnIsTheOffPillPlusItsCodename(t *testing.T) {
	want := strings.Replace(pillOffGolden, ` class="press group`, ` data-switcher-tmux="mango:1 @ atlas" class="press group`, 1)
	want = strings.Replace(want, `<span class="whitespace-nowrap">Billing migration</span>`,
		`<span class="flex flex-col items-start leading-tight"><span class="whitespace-nowrap">Billing migration</span>`+
			`<span data-switcher-tmux-line="" class="block max-w-[8rem] truncate text-[10px] font-normal text-fg2">mango:1</span></span>`, 1)
	if strings.Count(want, "mango:1 @ atlas") != 1 || !strings.Contains(want, "data-switcher-tmux-line") {
		t.Fatal("fixture: the ON golden was not built from the OFF one")
	}
	if got := pillOf(t, namedTmuxCard("mango:1 @ atlas", "mango:1")); got != want {
		t.Fatalf("the ON pill:\n got  %s\n want %s", got, want)
	}
}

// TestNoCodenameLineForAnUnnamedWindowOrAnAgent: the api's rule sets nothing
// for a window called by its codename already, nor for an agent; the ui draws
// nothing for either (no data-switcher-tmux, no second line).
func TestNoCodenameLineForAnUnnamedWindowOrAnAgent(t *testing.T) {
	unnamed := MirrorCardView{Host: "atlas", Session: "scratch3", Window: "@5", Label: "mango:1", Online: true}
	agent := AgentCardView{ID: 7, Name: "brave-otter", DisplayName: "Billing migration", Status: "running"}
	page := renderRaw(t, switcherBar(NewSwitcher([]AgentCardView{agent}, []MirrorCardView{unnamed}, nil, SwitcherCurrent{}), true))
	for _, bad := range []string{"data-switcher-tmux", switcherTmuxLineClass} {
		if strings.Contains(page, bad) {
			t.Errorf("an unnamed window or an agent drew %q", bad)
		}
	}
	// CONTROL: the same bar with a named window that has one draws it.
	on := renderRaw(t, switcherBar(NewSwitcher([]AgentCardView{agent}, []MirrorCardView{unnamed, namedTmuxCard("mango:1 @ atlas", "mango:1")}, nil, SwitcherCurrent{}), true))
	if !strings.Contains(on, `data-switcher-tmux="mango:1 @ atlas"`) {
		t.Fatal("control: the named window's codename is not drawn")
	}
}

// TestTheDockDrawsTheCodenameLineFromThePill: the dock's row script reads the
// pill's data-switcher-tmux into a third (or second) muted line,
// data-dock-tmux, after the focus line.
func TestTheDockDrawsTheCodenameLineFromThePill(t *testing.T) {
	js := dockJS
	i, j := strings.Index(js, "function rowOf(p)"), strings.Index(js, "li.appendChild(r);")
	if i < 0 || j < i {
		t.Fatal("the dock's rowOf is not found")
	}
	row := js[i:j]
	for _, want := range []string{
		"var tx = p.getAttribute('data-switcher-tmux');",
		"var l3 = el('span', 'truncate text-xs text-muted', tx);",
		"l3.setAttribute('data-dock-tmux', '');",
		"r.appendChild(l3);",
	} {
		if !strings.Contains(row, want) {
			t.Errorf("rowOf lacks %q", want)
		}
	}
	if strings.Index(row, "data-switcher-focus'") > strings.Index(row, "data-switcher-tmux'") {
		t.Error("the codename line is drawn before the focus line")
	}
}
