package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/pins"
	"github.com/ZacxDev/muster/internal/ui"
)

// TestANameThatIsTheCodenameGetsNoCodenameAgain: a session whose HUMAN name
// is its window's codename (a manual name "mango:1" on the window the host
// labels "mango:1") is already headed by the codename, so with its owner's
// "Show tmux sessions" ON it gains nothing: no codename line on its pill, no
// data-switcher-tmux for its dock row, and no "mango:1 · mango:1 @ ws" on its
// ask's name or push title. The control: the SAME window under a name that is
// not its codename gains all three.
func TestANameThatIsTheCodenameGetsNoCodenameAgain(t *testing.T) {
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{}, windows: namedWindows()} // @1 labelled "mango:1"
	store := namedFocusStore()
	s, _ := focusMirrorServer(t, fm, store, nil)
	prefs := &memNotifyPrefs{}
	s.ext.NotifyPrefs = prefs
	_ = prefs.SetTmuxAddress(context.Background(), authz.OperatorUserID, true)
	name := func(n string) {
		store.mu.Lock()
		store.rows[focus.MirrorSubject(focusSID)] = focus.Focus{Subject: focus.MirrorSubject(focusSID), Title: "Fixing the Stripe webhook retry",
			UpdatedAt: time.Now(), Name: focus.Name{Manual: n, ManualAt: time.Now()}}
		store.mu.Unlock()
	}
	card := func() ui.MirrorCardView {
		t.Helper()
		for _, c := range s.mirrorCards(operatorCtx(), pins.Set{}, s.focusIndex(operatorCtx())) {
			if c.Window == "@1" {
				return c
			}
		}
		t.Fatal("no card for window @1")
		return ui.MirrorCardView{}
	}
	ask := func() attentionItem {
		items := []attentionItem{askItem(attention.Ask{ID: 71, Owner: 1, Asker: attention.Asker{MirrorHost: "ws", MirrorSession: focusSID, Label: "mango:1"},
			Question: "Ship it?"}, s.ext.Mirror.Hosts(operatorCtx())...)}
		s.withFocus(operatorCtx(), items)
		return items[0]
	}
	pill := func(c ui.MirrorCardView) string {
		var b strings.Builder
		if err := ui.RenderSwitcherPin(&b, false, nil, "/", ui.NewSwitcher(nil, []ui.MirrorCardView{c}, nil, ui.SwitcherCurrent{})); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}

	// CONTROL: a name that is not the codename gains the codename everywhere.
	name("Billing migration")
	if c := card(); c.TmuxAt != "mango:1 @ ws" || c.TmuxCode != "mango:1" {
		t.Fatalf("control: card TmuxAt %q TmuxCode %q, want the codename", c.TmuxAt, c.TmuxCode)
	}
	if p := pill(card()); !strings.Contains(p, `data-switcher-tmux="mango:1 @ ws"`) || !strings.Contains(p, "data-switcher-tmux-line") {
		t.Fatalf("control: the pill lacks the codename:\n%s", p)
	}
	if it := ask(); it.Name != "Billing migration · mango:1 @ ws" {
		t.Fatalf("control: the ask is named %q", it.Name)
	}

	// The name IS the codename: nothing added, anywhere.
	name("mango:1")
	if c := card(); c.TmuxAt != "" || c.TmuxCode != "" {
		t.Errorf("a name that is the codename: card TmuxAt %q TmuxCode %q, want none", c.TmuxAt, c.TmuxCode)
	}
	if p := pill(card()); strings.Contains(p, "data-switcher-tmux") {
		t.Errorf("a name that is the codename: the pill draws it again (line or dock attr):\n%s", p)
	}
	it := ask()
	if it.Name != "mango:1" {
		t.Errorf("a name that is the codename: the ask is named %q, want %q", it.Name, "mango:1")
	}
	if got := attentionNotification(it, false).Title; got != "mango:1 is asking" {
		t.Errorf("a name that is the codename: push title %q, want %q", got, "mango:1 is asking")
	}
}
