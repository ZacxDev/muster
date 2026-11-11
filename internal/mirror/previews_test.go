package mirror_test

import (
	"context"
	"encoding/json"
	"maps"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestNormalizePreviewsClampsAndRestrictsToListedWindows: a host cannot report
// previews for a window it does not list, every count is clamped to
// [0, MaxPreviewCount], and an entry with nothing to show is dropped (absent
// means "nothing to show").
func TestNormalizePreviewsClampsAndRestrictsToListedWindows(t *testing.T) {
	windows := []mirror.Window{{Session: "s", ID: "@1"}, {Session: "s", ID: "@2"}, {Session: "s", ID: "@3"}, {Session: "s", ID: "@4"}}
	got := mirror.NormalizePreviews(map[string]mirror.WindowPreviews{
		"@1":  {Offered: 2, Open: 1},
		"@2":  {Offered: 500, Open: -3},
		"@3":  {},
		"@4":  {Open: 1},
		"@99": {Offered: 5},
	}, windows)
	want := map[string]mirror.WindowPreviews{
		"@1": {Offered: 2, Open: 1},
		"@2": {Offered: mirror.MaxPreviewCount},
		"@4": {Open: 1},
	}
	if !maps.Equal(got, want) {
		t.Fatalf("NormalizePreviews = %v, want %v", got, want)
	}
	if got := mirror.NormalizePreviews(map[string]mirror.WindowPreviews{"@3": {Offered: -1}}, windows); got != nil {
		t.Fatalf("nothing to show normalised to %v, want nil", got)
	}
}

// TestTheWireNamesOnlyTheTwoCounts pins the message's bytes: the type and,
// per window, "offered" and "open" — nothing about a port or a process.
func TestTheWireNamesOnlyTheTwoCounts(t *testing.T) {
	b, err := json.Marshal(mirror.Msg{Type: mirror.TypePreviews, Previews: map[string]mirror.WindowPreviews{"@7": {Offered: 2, Open: 1}, "@4": {Open: 3}}})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"type":"previews","previews":{"@4":{"open":3},"@7":{"offered":2,"open":1}}}`
	if string(b) != want {
		t.Fatalf("previews message = %s, want %s", b, want)
	}
}

// TestPreviewsMessagesReachEachWindowsEntry: a per-window v2 host's previews
// message lands on its entries — only for windows it lists — and nudges the
// owner's views when it changes (not when it repeats); an empty map clears
// it; a host that never sends one (an older muster-mirror, one without
// -preview) has entries with nothing to preview; and an offline host keeps
// NONE (a count is a claim about now).
func TestPreviewsMessagesReachEachWindowsEntry(t *testing.T) {
	r := newRig(t)
	r.svc.SetNotifyDebounce(time.Millisecond)
	var nudges atomic.Int32
	r.svc.SetNotify(func(int64) { nudges.Add(1) })
	tok, _ := r.store.mint(t, 7, "laptop")
	h := hello("laptop", false)
	h.Windows = []mirror.Window{
		{Session: "scratch2", ID: "@7", Index: 3, Active: true, Claude: true, ClaudeSession: "sess-a"},
		{Session: "scratch2", ID: "@4", Index: 1},
	}
	host, err := r.rawConnect(t, tok, h)
	if err != nil {
		t.Fatal(err)
	}
	entry := func(window string) (mirror.SessionView, bool) {
		for _, hv := range r.svc.Hub().Hosts(7) {
			for _, e := range hv.Entries() {
				if e.Window == window {
					return e, true
				}
			}
		}
		return mirror.SessionView{}, false
	}
	eventually(t, "the host listed", func() bool { _, ok := entry("@7"); return ok })
	// What an older host looks like: nothing to preview anywhere.
	if e, _ := entry("@7"); !e.Previews.IsZero() {
		t.Fatalf("a host that sent no previews message has previews %+v", e.Previews)
	}
	send := func(m mirror.Msg) {
		b, _ := json.Marshal(m)
		if err := host.ws.Write(context.Background(), websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(20 * time.Millisecond)
	before := nudges.Load()
	send(mirror.Msg{Type: mirror.TypePreviews, Previews: map[string]mirror.WindowPreviews{"@7": {Offered: 2, Open: 1}, "@99": {Open: 1}}})
	eventually(t, "the counts", func() bool { e, _ := entry("@7"); return e.Previews == mirror.WindowPreviews{Offered: 2, Open: 1} })
	if got := r.svc.Hub().Hosts(7)[0].Previews; len(got) != 1 {
		t.Fatalf("Previews = %v, want only the listed window's", got)
	}
	if e, _ := entry("@4"); !e.Previews.IsZero() {
		t.Fatalf("@4 has previews %+v it was never sent", e.Previews)
	}
	eventually(t, "a nudge for the changed counts", func() bool { return nudges.Load() > before })
	time.Sleep(20 * time.Millisecond)
	settled := nudges.Load()
	send(mirror.Msg{Type: mirror.TypePreviews, Previews: map[string]mirror.WindowPreviews{"@7": {Offered: 2, Open: 1}}})
	time.Sleep(50 * time.Millisecond)
	if n := nudges.Load(); n != settled {
		t.Fatalf("the same counts again nudged %d times, want none", n-settled)
	}
	// An empty map clears them, and that is a change the views hear.
	send(mirror.Msg{Type: mirror.TypePreviews, Previews: map[string]mirror.WindowPreviews{}})
	eventually(t, "the counts cleared", func() bool { e, _ := entry("@7"); return e.Previews.IsZero() })
	eventually(t, "a nudge for the cleared counts", func() bool { return nudges.Load() > settled })
	send(mirror.Msg{Type: mirror.TypePreviews, Previews: map[string]mirror.WindowPreviews{"@4": {Offered: 1}}})
	eventually(t, "the counts again", func() bool { e, _ := entry("@4"); return e.Previews.Offered == 1 })

	// Offline: no counts are kept.
	host.ws.CloseNow()
	eventually(t, "the host offline", func() bool { hs := r.svc.Hub().Hosts(7); return len(hs) == 1 && !hs[0].Online })
	if e, ok := entry("@4"); !ok || !e.Previews.IsZero() {
		t.Fatalf("an offline host's entry (listed %v) has previews %+v, want none", ok, e.Previews)
	}
}
