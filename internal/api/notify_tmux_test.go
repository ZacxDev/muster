package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/focus"
)

// memNotifyPrefs is notifypref.Store in memory: owner → on, or err on every
// read.
type memNotifyPrefs struct {
	mu  sync.Mutex
	on  map[int64]bool
	err error
}

func (m *memNotifyPrefs) TmuxAddress(ctx context.Context, owner int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return false, m.err
	}
	return m.on[owner], nil
}

func (m *memNotifyPrefs) SetTmuxAddress(ctx context.Context, owner int64, on bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.on == nil {
		m.on = map[int64]bool{}
	}
	m.on[owner] = on
	return nil
}

// namedAskItem is an ask from the window whose conversation namedFocusStore
// names "Billing migration": its place is the label and the tmux target.
func namedAskItem() attentionItem {
	return attentionItem{Key: "ask-1", Kind: attentionKindAsk, Source: "mango:1 · tmux main:1 @ ws", target: "tmux main:1 @ ws",
		subject: focus.MirrorSubject(focusSID), claudeSession: focusSID}
}

// TestNotifyNameAddsTheAddressOnlyWhenOnAndNamed pins the one rule with
// literal strings: off, or no human name, or no tmux target, is the input
// unchanged; on, the name, " · " and the target without "tmux ".
func TestNotifyNameAddsTheAddressOnlyWhenOnAndNamed(t *testing.T) {
	for _, c := range []struct {
		name, target string
		on           bool
		want         string
	}{
		{"Billing migration", "tmux main:1 @ ws", false, "Billing migration"},
		{"Billing migration", "tmux main:1 @ ws", true, "Billing migration · main:1 @ ws"},
		{"Billing migration", "tmux scratch3 @ host-b", true, "Billing migration · scratch3 @ host-b"},
		// No human name: the caller shows the place already.
		{"", "tmux main:1 @ ws", true, ""},
		// No window to vouch for (an agent, a session no window holds now).
		{"Billing migration", "", true, "Billing migration"},
		// A place that is not a tmux target is never spelled as one.
		{"Billing migration", "main:1", true, "Billing migration"},
	} {
		if got := notifyName(c.name, c.target, c.on); got != c.want {
			t.Errorf("notifyName(%q, %q, %v) = %q, want %q", c.name, c.target, c.on, got, c.want)
		}
	}
}

// TestShowTmuxSessionNamesAnAttentionItemByItsNameAndAddress: through
// withFocus (the Asks list, the summary, a filed ask's push all name items
// there), a named session's ask with the owner's switch ON is called
// "<name> · <session>:<index> @ <host>" by its push title (kept whole), its
// Asks row and the attention summary; OFF — no store, a store with no row, a
// store that fails, or another user's switch on — every one of them is
// byte for byte what it was.
func TestShowTmuxSessionNamesAnAttentionItemByItsNameAndAddress(t *testing.T) {
	const want = "Billing migration · main:1 @ ws"
	fm := &fakeMirror{online: true, v2: &fakeMirrorV2{}, windows: namedWindows()}
	s, _ := focusMirrorServer(t, fm, namedFocusStore(), nil)
	named := func() attentionItem {
		items := []attentionItem{namedAskItem()}
		s.withFocus(operatorCtx(), items)
		return items[0]
	}
	assertOff := func(what string) {
		t.Helper()
		it := named()
		n := attentionNotification(it, false)
		if it.Name != "Billing migration" || n.Title != "Billing migration is asking" || n.TitleKeep != "Billing migration" {
			t.Fatalf("%s: name %q, push %q keep %q — want today's text", what, it.Name, n.Title, n.TitleKeep)
		}
		if span := attentionSourceSpan(it); span != `<span data-attention-source data-attention-name title="mango:1 · tmux main:1 @ ws">Billing migration</span>` {
			t.Fatalf("%s: Asks row %s", what, span)
		}
		if got := attentionSummaryItemOf(it, "", time.Now()).Title; got != "Billing migration" {
			t.Fatalf("%s: summary title %q", what, got)
		}
	}
	assertOff("no store")
	prefs := &memNotifyPrefs{}
	s.ext.NotifyPrefs = prefs
	assertOff("a store with no choice")
	_ = prefs.SetTmuxAddress(context.Background(), 2, true)
	assertOff("another user's switch on")

	_ = prefs.SetTmuxAddress(context.Background(), authz.OperatorUserID, true)
	it := named()
	if it.Name != want {
		t.Fatalf("on: item name %q, want %q", it.Name, want)
	}
	n := attentionNotification(it, false)
	if n.Title != want+" is asking" || n.TitleKeep != want {
		t.Fatalf("on: push title %q keep %q", n.Title, n.TitleKeep)
	}
	if span := attentionSourceSpan(it); span != `<span data-attention-source data-attention-name title="mango:1 · tmux main:1 @ ws">`+want+`</span>` {
		t.Fatalf("on: Asks row %s", span)
	}
	if got := attentionSummaryItemOf(it, "", time.Now()).Title; got != want {
		t.Fatalf("on: summary title %q", got)
	}
	// An unnamed session is already called by its place: no address twice.
	unnamed := []attentionItem{{Key: "ask-2", Kind: attentionKindAsk, Source: "kiwi:2 · tmux side:2 @ ws", target: "tmux side:2 @ ws",
		subject: focus.MirrorSubject("sid-unnamed"), claudeSession: "sid-unnamed"}}
	s.withFocus(operatorCtx(), unnamed)
	if got := attentionNotification(unnamed[0], false).Title; got != "kiwi:2 · tmux side:2 @ ws is asking" {
		t.Fatalf("on, unnamed: push title %q, want its place once", got)
	}

	prefs.err = errors.New("read failed")
	assertOff("a failed read")
	prefs.err = nil
	_ = prefs.SetTmuxAddress(context.Background(), authz.OperatorUserID, false)
	assertOff("switched off again")
}
