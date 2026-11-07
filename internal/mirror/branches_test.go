package mirror_test

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestValidBranchKeepsOnlyWholeNames: what a window's branch may be. A name
// outside the grammar is refused WHOLE — never trimmed or cut, since a cut
// name is another branch's — and the boundary is MaxBranch bytes exactly.
func TestValidBranchKeepsOnlyWholeNames(t *testing.T) {
	for in, want := range map[string]bool{
		"feat/x-1":                              true,
		"main":                                  true,
		"release/2026.11+hotfix":                true,
		strings.Repeat("a", mirror.MaxBranch):   true,
		strings.Repeat("a", mirror.MaxBranch+1): false,
		"":                                      false,
		"feat/é-1":                              false,
		"feat/x 1":                              false,
		"feat/x\n1":                             false,
		"a..b":                                  false,
		"-leading":                              false,
		"trailing/":                             false,
		"feat/<x>&\"y\"":                        false,
	} {
		if got := mirror.ValidBranch(in); got != want {
			t.Errorf("ValidBranch(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestNormalizeBranchesDropsNeverCuts: only listed windows, only valid names,
// each kept byte-for-byte; an over-long or multibyte name is dropped, not cut.
func TestNormalizeBranchesDropsNeverCuts(t *testing.T) {
	windows := mirror.NormalizeWindows([]mirror.Window{
		{Session: "s", ID: "@1", Index: 0}, {Session: "s", ID: "@2", Index: 1},
		{Session: "s", ID: "@3", Index: 2}, {Session: "s", ID: "@4", Index: 3},
	}, 0)
	long := "feat/" + strings.Repeat("x", mirror.MaxBranch)
	got := mirror.NormalizeBranches(map[string]string{
		"@1": "feat/x-1", "@2": long, "@3": "feat/日本-1", "@4": "", "@99": "feat/ghost-1",
	}, windows)
	if want := map[string]string{"@1": "feat/x-1"}; !maps.Equal(got, want) {
		t.Fatalf("NormalizeBranches = %v, want %v", got, want)
	}
	if got := mirror.NormalizeBranches(map[string]string{"@2": long}, windows); got != nil {
		t.Fatalf("nothing valid left should be nil, got %v", got)
	}
}

// TestAnOfflineHostViewsEntriesCarryNoBranch: Entries gives an offline host's
// windows no branch even when the view holds one (the hub keeps none offline
// either; this is the second, independent half).
func TestAnOfflineHostViewsEntriesCarryNoBranch(t *testing.T) {
	ws := mirror.NormalizeWindows([]mirror.Window{{Session: "s", ID: "@1", Index: 0, Active: true, Claude: true, ClaudeSession: "x"}}, 0)
	for _, online := range []bool{true, false} {
		h := mirror.HostView{Host: "box", Windows: ws, Online: online, Branches: map[string]string{"@1": "feat/x-1"}}
		want := ""
		if online {
			want = "feat/x-1"
		}
		if got := h.Entries()[0].Branch; got != want {
			t.Errorf("online=%v: entry branch %q, want %q", online, got, want)
		}
	}
}

// TestABranchesMessageLandsOnOnlineEntries: a per-window v2 host's branches
// message lands on its entries — only for windows it lists, only valid names —
// a later one replaces it (an empty one clears it), an offline host's entries
// carry NONE (a checkout is a claim about now), a host that never sends one
// (an older muster-mirror) has entries with no branch, and a v1 host's message
// is dropped.
func TestABranchesMessageLandsOnOnlineEntries(t *testing.T) {
	r := newRig(t)
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
	branchOf := func(owner int64, window string) string {
		for _, hv := range r.svc.Hub().Hosts(owner) {
			for _, e := range hv.Entries() {
				if e.Window == window {
					return e.Branch
				}
			}
		}
		return "<unlisted>"
	}
	eventually(t, "the host listed", func() bool { return branchOf(7, "@7") == "" })
	time.Sleep(20 * time.Millisecond)
	// Control: before any branches message — what an older host looks like.
	if got := branchOf(7, "@4"); got != "" {
		t.Fatalf("a host that sent no branches message has branch %q", got)
	}
	send := func(m mirror.Msg) {
		b, _ := json.Marshal(m)
		if err := host.ws.Write(context.Background(), websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
	}
	send(mirror.Msg{Type: mirror.TypeBranches, Branches: map[string]string{"@7": "feat/x-1", "@4": "a..b", "@99": "feat/ghost-1"}})
	eventually(t, "the branch", func() bool { return branchOf(7, "@7") == "feat/x-1" })
	if got := r.svc.Hub().Hosts(7)[0].Branches; !maps.Equal(got, map[string]string{"@7": "feat/x-1"}) {
		t.Fatalf("Branches = %v, want only the listed window's valid branch", got)
	}
	send(mirror.Msg{Type: mirror.TypeBranches, Branches: map[string]string{"@7": "feat/x-2"}})
	eventually(t, "the moved branch", func() bool { return branchOf(7, "@7") == "feat/x-2" })
	send(mirror.Msg{Type: mirror.TypeBranches})
	eventually(t, "the cleared branch", func() bool { return branchOf(7, "@7") == "" })
	send(mirror.Msg{Type: mirror.TypeBranches, Branches: map[string]string{"@7": "feat/x-3"}})
	eventually(t, "the branch again", func() bool { return branchOf(7, "@7") == "feat/x-3" })
	// Offline: no branch at all.
	host.ws.CloseNow()
	eventually(t, "the host offline", func() bool { hs := r.svc.Hub().Hosts(7); return len(hs) == 1 && !hs[0].Online })
	if got := branchOf(7, "@7"); got != "" {
		t.Fatalf("an offline host's entry has branch %q, want none", got)
	}

	tok1, _ := r.store.mint(t, 8, "oldbox")
	h1 := hello("oldbox", false)
	h1.Version = 1
	h1.Windows = []mirror.Window{{Session: "s", ID: "@1", Index: 0, Active: true}}
	old, err := r.rawConnect(t, tok1, h1)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(mirror.Msg{Type: mirror.TypeBranches, Branches: map[string]string{"@1": "feat/x-1"}})
	if err := old.ws.Write(context.Background(), websocket.MessageText, b); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	for _, hv := range r.svc.Hub().Hosts(8) {
		if len(hv.Branches) != 0 {
			t.Fatalf("a v1 host's branches were kept: %v", hv.Branches)
		}
	}
}
