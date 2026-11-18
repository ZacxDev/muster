package mirror_test

import (
	"context"
	"slices"
	"testing"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestAHostsSpawnReadinessReachesItsView (officers O-B): the readiness an
// -interactive host sends in its hello — its allow-list, scratch spawns,
// claude found and logged in — is on its HostView, cleaned (a malformed
// allow-list entry dropped, an unknown login read as unknown); a later
// spawn-ready replaces it; a READ-ONLY host's is never kept (a spawn is a
// write); and a host that sends none reads as nil (unknown), as an older
// muster-mirror does.
func TestAHostsSpawnReadinessReachesItsView(t *testing.T) {
	r := newRig(t)
	tok, _ := r.store.mint(t, 1, "box1")
	h := connectJSON(t, r, tok, `{"type":"hello","version":2,"host":"box1","interactive":true,"windows":[],`+
		`"spawnReady":{"repos":["example-org/example-site","example-org/*","not a repo","Example-Org/Shout"],"scratch":true,"claude":"found","login":"sideways"}}`)
	view := func() *mirror.SpawnReadiness {
		for _, v := range r.svc.Hub().Hosts(1) {
			if v.Host == "box1" {
				return v.SpawnReady
			}
		}
		return nil
	}
	eventually(t, "the hello's readiness", func() bool { return view() != nil })
	got := view()
	if want := []string{"example-org/example-site", "example-org/*", "example-org/shout"}; !slices.Equal(got.Repos, want) || !got.Scratch ||
		got.Claude != mirror.ClaudeFound || got.Login != mirror.LoginUnknown {
		t.Fatalf("the hello's readiness reads %+v, want repos %v, scratch, claude found, login unknown", got, want)
	}
	if !mirror.SpawnRepoAllowed(got.Repos, "Example-Org/Anything") || !mirror.SpawnRepoAllowed(got.Repos, "example-org/example-site") ||
		mirror.SpawnRepoAllowed(got.Repos, "example-org-evil/x") {
		t.Fatalf("the allow-list rule reads %v wrong", got.Repos)
	}
	// A changed readiness (logged in now, claude still found).
	if err := h.ws.Write(context.Background(), websocket.MessageText,
		[]byte(`{"type":"spawn-ready","spawnReady":{"repos":[],"claude":"found","login":"yes"}}`)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the changed readiness", func() bool { v := view(); return v != nil && v.Login == mirror.LoginYes })
	if v := view(); len(v.Repos) != 0 || v.Scratch {
		t.Fatalf("the update did not replace the readiness whole: %+v", v)
	}

	// A read-only host's readiness is never kept.
	tok2, _ := r.store.mint(t, 1, "box2")
	connectJSON(t, r, tok2, `{"type":"hello","version":2,"host":"box2","windows":[],"spawnReady":{"repos":["example-org/x"],"claude":"found","login":"yes"}}`)
	eventually(t, "box2 registered", func() bool {
		return slices.ContainsFunc(r.svc.Hub().Hosts(1), func(v mirror.HostView) bool { return v.Host == "box2" })
	})
	if v := hostView(t, r, 1, "box2").SpawnReady; v != nil {
		t.Fatalf("a read-only host's readiness was kept: %+v", v)
	}
	// Positive control for the nil above: the same hello, interactive, is kept.
	tok3, _ := r.store.mint(t, 1, "box3")
	connectJSON(t, r, tok3, `{"type":"hello","version":2,"host":"box3","interactive":true,"windows":[],"spawnReady":{"repos":["example-org/x"],"claude":"missing","login":"yes"}}`)
	eventually(t, "box3's readiness", func() bool {
		return slices.ContainsFunc(r.svc.Hub().Hosts(1), func(v mirror.HostView) bool { return v.Host == "box3" && v.SpawnReady != nil })
	})
	if v := hostView(t, r, 1, "box3").SpawnReady; v.Claude != mirror.ClaudeMissing {
		t.Fatalf("box3 reads %+v", v)
	}
}

// TestCleanSpawnReady: nil stays nil (unknown); a Claude value muster does not
// know reads as missing; free text is bounded.
func TestCleanSpawnReady(t *testing.T) {
	if mirror.CleanSpawnReady(nil) != nil {
		t.Fatal("nil is not kept as unknown")
	}
	long := make([]byte, 3*mirror.MaxReadyDetail)
	for i := range long {
		long[i] = 'x'
	}
	c := mirror.CleanSpawnReady(&mirror.SpawnReadiness{Claude: "maybe", Login: mirror.LoginNo, ClaudeDetail: string(long)})
	if c.Claude != mirror.ClaudeMissing || c.Login != mirror.LoginNo || len(c.ClaudeDetail) != mirror.MaxReadyDetail || c.Repos == nil {
		t.Fatalf("cleaned %+v", c)
	}
}
