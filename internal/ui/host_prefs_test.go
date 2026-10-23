package ui

import (
	"strings"
	"testing"
)

// withHostPrefs installs p for the rest of t, and puts back what was there.
func withHostPrefs(t *testing.T, p HostPreferences) {
	t.Helper()
	was := hostPrefs
	t.Cleanup(func() { hostPrefs = was })
	if err := SetHostPreferences(p); err != nil {
		t.Fatalf("SetHostPreferences(%+v): %v", p, err)
	}
}

// TestHostPreferencesRefuseWhatWouldMislead: each refusal is reached by a case
// no earlier check rejects, and named by its own message; a refused setting
// leaves the installed one as it was.
func TestHostPreferencesRefuseWhatWouldMislead(t *testing.T) {
	withHostPrefs(t, HostPreferences{Order: []string{"dock"}})
	all := map[string]string{}
	for i, d := range hostIconSet[:len(hostIconSet)-1] {
		all["h"+string(rune('a'+i))] = d.Key
	}
	for name, c := range map[string]struct {
		p    HostPreferences
		want string
	}{
		"empty order host":   {HostPreferences{Order: []string{"tower", " "}}, "host order: an empty host name"},
		"cluster in order":   {HostPreferences{Order: []string{"Cluster"}}, `host order: "cluster" is the dispatched agents' host`},
		"rest in order":      {HostPreferences{Order: []string{"*"}}, `host order: "*" is not a host name`},
		"repeated host":      {HostPreferences{Order: []string{"tower", "TOWER"}}, `host order: "tower" is listed twice`},
		"cluster icon":       {HostPreferences{Icons: map[string]string{"cluster": "server"}}, `host icons: "cluster" is the dispatched agents' host`},
		"unknown icon":       {HostPreferences{Icons: map[string]string{"tower": "rocket"}}, `host icons: "rocket" is not an icon (one of laptop, server,`},
		"cluster glyph":      {HostPreferences{Icons: map[string]string{"tower": "cluster"}}, `host icons: "cluster" is not an icon`},
		"one glyph twice":    {HostPreferences{Icons: map[string]string{"tower": "server", "dock": "server"}}, `host icons: "server" is given to both`},
		"one host two icons": {HostPreferences{Icons: map[string]string{"tower": "server", "Tower": "laptop"}}, `host icons: "tower" is given two icons`},
		"too many named":     {HostPreferences{Icons: all}, "host icons: 11 named icons leave 1 for every other host; leave at least 2"},
	} {
		err := SetHostPreferences(c.p)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
		if len(hostPrefs.Order) != 1 || hostPrefs.Order[0] != "dock" || len(hostPrefs.Icons) != 0 {
			t.Errorf("%s: a refused setting changed the installed one: %+v", name, hostPrefs)
		}
	}
	// Control: the largest accepted naming — ten icons, two left — installs.
	ten := map[string]string{}
	for i, d := range hostIconSet[:len(hostIconSet)-2] {
		ten["h"+string(rune('a'+i))] = d.Key
	}
	if err := SetHostPreferences(HostPreferences{Order: []string{" Tower ", "laptop"}, Icons: ten}); err != nil {
		t.Fatalf("control: a valid setting was refused: %v", err)
	}
	if strings.Join(hostPrefs.Order, ",") != "tower,laptop" {
		t.Errorf("the order is not normalised: %v", hostPrefs.Order)
	}
}
