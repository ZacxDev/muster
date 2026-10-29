package main

import (
	"strings"
	"testing"
)

// TestTheHostPreferencesAreReadAndChecked: MUSTER_HOST_ICONS reaches the
// config as ui.HostPreferences (names lower-cased), unset is none, and a value
// that is not host=icon — or that ui.HostPreferences.Normalized refuses —
// stops the boot, naming the variable. (MUSTER_HOST_ORDER, the retired Agents
// list's host-tab order, is read no more.)
func TestTheHostPreferencesAreReadAndChecked(t *testing.T) {
	base := map[string]string{envDatabase: "postgres://x/y", envUIPassword: strings.Repeat("p", 40)}
	load := func(icons string) (config, error) {
		return loadConfig(func(k string) string {
			if k == envHostIcons {
				return icons
			}
			return base[k]
		})
	}
	cfg, err := load("")
	if err != nil {
		t.Fatalf("unset refused: %v", err)
	}
	if len(cfg.HostPrefs.Icons) != 0 {
		t.Errorf("unset names hosts: %+v", cfg.HostPrefs)
	}
	cfg, err = load("Laptop=laptop, tower=server")
	if err != nil {
		t.Fatalf("a valid setting refused: %v", err)
	}
	if cfg.HostPrefs.Icons["laptop"] != "laptop" || cfg.HostPrefs.Icons["tower"] != "server" || len(cfg.HostPrefs.Icons) != 2 {
		t.Errorf("icons = %v", cfg.HostPrefs.Icons)
	}
	for _, c := range []struct{ icons, want string }{
		{"tower", `invalid MUSTER_HOST_ICONS: entry "tower" is not host=icon`},
		{"tower=", `invalid MUSTER_HOST_ICONS: entry "tower=" is not host=icon`},
		{"tower=server,Tower=laptop", `invalid MUSTER_HOST_ICONS: host "Tower" is given two icons`},
		{"tower=rocket", `invalid MUSTER_HOST_ICONS: host icons: "rocket" is not an icon`},
		{"cluster=server", `invalid MUSTER_HOST_ICONS: host icons: "cluster" is the dispatched agents' host`},
		{"my laptop=laptop", `invalid MUSTER_HOST_ICONS: host icons: "my laptop" is not a host name`},
	} {
		if _, err := load(c.icons); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("icons %q: err = %v, want %q", c.icons, err, c.want)
		}
	}
}
