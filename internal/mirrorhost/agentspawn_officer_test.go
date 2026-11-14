package mirrorhost

import (
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// TestAnOfficerInstancesPrefaceGetsPastThePrefaceGuard: a host officer's spawn
// (docs/design/officers-on-hosts.md) begins with mirror.OfficerSpawnPreface,
// and the host's guard (mirror.HasSpawnPreface) admits it — measured by the
// request being refused LATER, by the allow-list (no_repo), not by the
// preface. The controls: the chief's preface reaches the same later refusal,
// and a prompt that only STARTS like the officer preface, or is a subcommand,
// is refused by the preface itself.
func TestAnOfficerInstancesPrefaceGetsPastThePrefaceGuard(t *testing.T) {
	const preface = "the prompt does not begin with muster's preface"
	for _, c := range []struct {
		name, prompt, code, detail string
	}{
		{"the officer preface", mirror.OfficerSpawnPreface + ".\n\nYou are officer marketing:example", mirror.SpawnNoRepo, "repo acme/widgets is not on this host's allow-list"},
		{"the chief's preface (control)", mirror.SpawnPreface + " on a host", mirror.SpawnNoRepo, "repo acme/widgets is not on this host's allow-list"},
		{"a truncated officer preface", "muster: your owner's officer", mirror.SpawnFailed, preface},
		{"a subcommand", "update", mirror.SpawnFailed, preface},
	} {
		u := newSpawnUnit(t)
		u.c.cfg.AgentSpawn.Repos = nil
		_, e := u.spawn(t, 61, widgets(c.prompt))
		if e == nil || e.code != c.code || e.detail != c.detail {
			t.Errorf("%s: refusal %+v, want %s %q", c.name, e, c.code, c.detail)
		}
		if n := u.gitCalls(); n != 0 {
			t.Errorf("%s: git ran %d times", c.name, n)
		}
	}
}
