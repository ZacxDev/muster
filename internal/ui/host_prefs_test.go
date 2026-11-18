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
	withHostPrefs(t, HostPreferences{Icons: map[string]string{"dock": "laptop"}})
	for name, c := range map[string]struct {
		p    HostPreferences
		want string
	}{
		"icon host not name": {HostPreferences{Icons: map[string]string{"my:laptop": "laptop"}}, `host icons: "my:laptop" is not a host name`},
		"space in a name":    {HostPreferences{Icons: map[string]string{"tower laptop": "laptop"}}, `host icons: "tower laptop" is not a host name`},
		"wrong separator":    {HostPreferences{Icons: map[string]string{"tower;laptop": "laptop"}}, `host icons: "tower;laptop" is not a host name`},
		"leading hyphen":     {HostPreferences{Icons: map[string]string{"-tower": "laptop"}}, `host icons: "-tower" is not a host name`},
		"too long":           {HostPreferences{Icons: map[string]string{strings.Repeat("a", 64): "laptop"}}, `is not a host name`},
		"empty host":         {HostPreferences{Icons: map[string]string{" ": "laptop"}}, `host icons: "" is not a host name`},
		"cluster icon":       {HostPreferences{Icons: map[string]string{"cluster": "server"}}, `host icons: "cluster" is the dispatched agents' host`},
		"unknown icon":       {HostPreferences{Icons: map[string]string{"tower": "rocket"}}, `host icons: "rocket" is not an icon (one of laptop, server,`},
		"cluster glyph":      {HostPreferences{Icons: map[string]string{"tower": "cluster"}}, `host icons: "cluster" is not an icon`},
		"one glyph twice":    {HostPreferences{Icons: map[string]string{"tower": "server", "dock": "server"}}, `host icons: "server" is given to both`},
		"one host two icons": {HostPreferences{Icons: map[string]string{"tower": "server", "Tower": "laptop"}}, `host icons: "tower" is given two icons`},
	} {
		err := SetHostPreferences(c.p)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
		if len(hostPrefs.Icons) != 1 || hostPrefs.Icons["dock"] != "laptop" {
			t.Errorf("%s: a refused setting changed the installed one: %+v", name, hostPrefs)
		}
	}
	// Control: names at the pattern's edges install (63 characters, inner
	// hyphens, digits, case folded).
	ok := HostPreferences{Icons: map[string]string{" Tower ": "server", "a": "laptop", strings.Repeat("b", 63): "phone", "x-1-y": "box"}}
	if err := SetHostPreferences(ok); err != nil {
		t.Fatalf("control: a valid setting was refused: %v", err)
	}
	if hostPrefs.Icons["tower"] != "server" || len(hostPrefs.Icons) != 4 {
		t.Errorf("the names are not normalised: %v", hostPrefs.Icons)
	}
}
