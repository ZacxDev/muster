package main

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/pages"
)

// TestPagesAgentShareIsParsedAndAnythingElseRefusesBoot: unset and "ask" are
// the owner's approval (the default), "auto" (any case, spaces trimmed) is
// auto, and any other value is a boot error naming the variable — never a
// silent default either way.
func TestPagesAgentShareIsParsedAndAnythingElseRefusesBoot(t *testing.T) {
	for v, want := range map[string]bool{"": false, "ask": false, " ASK ": false, "auto": true, "Auto": true, " auto\n": true} {
		got, err := parsePagesAgentShare(v)
		if err != nil || got != want {
			t.Errorf("parsePagesAgentShare(%q) = %v, %v; want %v, nil", v, got, err, want)
		}
	}
	for _, v := range []string{"on", "1", "true", "yes", "autoo", "approve"} {
		if _, err := parsePagesAgentShare(v); err == nil || !strings.Contains(err.Error(), envPagesAgentShare) {
			t.Errorf("parsePagesAgentShare(%q) = %v; want a boot error naming %s", v, err, envPagesAgentShare)
		}
	}
	// Through the whole config load, too: a bad value refuses it.
	env := map[string]string{envPagesAgentShare: "sometimes"}
	if _, err := loadConfig(func(k string) string { return env[k] }); err == nil || !strings.Contains(err.Error(), envPagesAgentShare) {
		t.Fatalf("loadConfig with %s=sometimes = %v; want a boot error naming it", envPagesAgentShare, err)
	}
	env[envPagesAgentShare] = "auto"
	c, err := loadConfig(func(k string) string { return env[k] })
	if err != nil || !c.PagesAgentShareAuto {
		t.Fatalf("loadConfig with %s=auto = %+v, %v; want PagesAgentShareAuto", envPagesAgentShare, c.PagesAgentShareAuto, err)
	}
}

// TestThePagesBannerSaysWhetherAnAgentsLinkWaits: the `shareable pages: ON`
// line names the mode in both directions, and whether links are reusable.
func TestThePagesBannerSaysWhetherAnAgentsLinkWaits(t *testing.T) {
	line := func(out string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "shareable pages: ON") {
				return l
			}
		}
		t.Fatalf("no `shareable pages: ON` line:\n%s", out)
		return ""
	}
	ask := line(renderBanner(t, config{Pages: true}, api.Extensions{Pages: pages.NewPG(nil)}))
	if !strings.Contains(ask, envPagesAgentShare+"=ask") || strings.Contains(ask, "AT ONCE") {
		t.Fatalf("ask mode's line: %s", ask)
	}
	if !strings.Contains(ask, "hash-only") {
		t.Fatalf("without a key the line must say links are hash-only: %s", ask)
	}
	keyed, err := pages.NewPG(nil).WithLinkKey(make([]byte, pages.LinkKeySize))
	if err != nil {
		t.Fatal(err)
	}
	auto := line(renderBanner(t, config{Pages: true, PagesAgentShareAuto: true}, api.Extensions{Pages: keyed, PagesAgentShareAuto: true}))
	if !strings.Contains(auto, envPagesAgentShare+"=auto") || !strings.Contains(auto, "AT ONCE") {
		t.Fatalf("auto mode's line: %s", auto)
	}
	if !strings.Contains(auto, "one reusable link") {
		t.Fatalf("with a key the line must say links are reusable: %s", auto)
	}
}
