package main

import "testing"

// TestPluginAutostartIsOffUnlessSet: MUSTER_PLUGIN_AUTOSTART is the
// deployment's switch over plugin auto-start (docs/plugins.md#auto-start):
// unset, empty or anything but a true spelling is off; "1" is on.
func TestPluginAutostartIsOffUnlessSet(t *testing.T) {
	for _, c := range []struct {
		val  string
		want bool
	}{{"", false}, {"0", false}, {"off", false}, {"1", true}, {"true", true}} {
		env := map[string]string{"MUSTER_UI_PASSWORD": "a-synthetic-password-for-tests"}
		if c.val != "" {
			env[envPluginAutostart] = c.val
		}
		cfg, err := loadEnv(t, env)
		if err != nil {
			t.Fatalf("%q: %v", c.val, err)
		}
		if cfg.PluginAutostart != c.want {
			t.Errorf("MUSTER_PLUGIN_AUTOSTART=%q: PluginAutostart %v, want %v", c.val, cfg.PluginAutostart, c.want)
		}
	}
}
