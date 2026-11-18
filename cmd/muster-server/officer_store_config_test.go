package main

import (
	"strings"
	"testing"
)

// TestAnEmptyOfficerRosterTurnsTheStoreOn: MUSTER_OFFICERS set to `[]`
// declares no officer and still marks the officer runtime ON (OfficersSet),
// which with a database turns the built-in store's admin API and Attention
// sweep on; unset (or blank) leaves it off. A declared officer sets it too.
func TestAnEmptyOfficerRosterTurnsTheStoreOn(t *testing.T) {
	base := map[string]string{envDatabase: "postgres://x/y", envUIPassword: strings.Repeat("p", 40)}
	for _, c := range []struct {
		name, v string
		want    bool
	}{
		{"unset", "", false},
		{"blank", "   ", false},
		{"empty roster", "[]", true},
		{"a builtin officer", `[{"id":"marketing:example","backend":"builtin"}]`, true},
	} {
		cfg, err := loadConfig(func(k string) string {
			if k == envOfficers {
				return c.v
			}
			return base[k]
		})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if cfg.OfficersSet != c.want {
			t.Fatalf("%s: OfficersSet = %v, want %v", c.name, cfg.OfficersSet, c.want)
		}
		if c.v == "[]" && len(cfg.Officers) != 0 {
			t.Fatalf("empty roster declared %d officers", len(cfg.Officers))
		}
	}
}
