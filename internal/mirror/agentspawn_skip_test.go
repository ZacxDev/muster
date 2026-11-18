package mirror_test

import (
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestTheHubKeepsAHostsSkipPermissionsOnlyBesideARuntime: the hub lists
// HostView.AgentSpawnSkipPermissions from the hello only when that hello also
// offers a runtime it spawns — a host that spawns nothing starts nothing
// without prompts — and an older host, which never sends the field, reads as
// false (normal permission mode, what it does).
func TestTheHubKeepsAHostsSkipPermissionsOnlyBesideARuntime(t *testing.T) {
	r := newRig(t)
	for _, c := range []struct {
		host     string
		runtimes []string
		skip     bool
		want     bool
	}{
		{"skip-claude", []string{mirror.SpawnRuntimeClaude}, true, true},
		{"keep-claude", []string{mirror.SpawnRuntimeClaude}, false, false},
		{"skip-nothing", nil, true, false},
	} {
		tok, _ := r.store.mint(t, 1, c.host)
		h := hello(c.host, true, "shell")
		h.AgentSpawn = c.runtimes
		h.AgentSpawnSkipPermissions = c.skip
		if _, err := r.rawConnect(t, tok, h); err != nil {
			t.Fatalf("%s: %v", c.host, err)
		}
		r.online(t, 1, c.host, "shell")
		if got := hostOf(t, r, c.host).AgentSpawnSkipPermissions; got != c.want {
			t.Errorf("%s: the hub lists AgentSpawnSkipPermissions=%v, want %v", c.host, got, c.want)
		}
	}
}
