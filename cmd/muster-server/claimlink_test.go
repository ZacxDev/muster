package main

import (
	"strings"
	"testing"
)

// TestTheClaimLinkIsOnTheBrowserListener: in gotrue mode only the browser
// listener serves a claim, so `invite --claim-admin` on a hub whose
// MUSTER_PUBLIC_URL is its own loopback address on the MAIN port (a blind
// install with no Tailscale, round 3: the link it printed answered 403) writes
// the link on the browser listener's port, and says to set MUSTER_PUBLIC_URL
// there. Every other address is kept as set: https (Tailscale serve routes
// it), another host, another port, password mode, no browser listener.
//
// The fixture's ports are pairwise distinct and none is the default 8105 or
// the documented 8107, so a hard-coded port cannot pass.
func TestTheClaimLinkIsOnTheBrowserListener(t *testing.T) {
	gotrue := func(public string) config {
		c := validGoTrueConfig()
		c.Port, c.BrowserPort, c.PublicURL = 18315, 18327, public
		return c
	}
	for name, c := range map[string]struct {
		cfg      config
		want     string
		wantNote bool
	}{
		"localhost on the main port":      {gotrue("http://localhost:18315"), "http://localhost:18327", true},
		"127.0.0.1 on the main port":      {gotrue("http://127.0.0.1:18315/"), "http://127.0.0.1:18327", true},
		"[::1] on the main port":          {gotrue("http://[::1]:18315"), "http://[::1]:18327", true},
		"already the browser port":        {gotrue("http://localhost:18327"), "http://localhost:18327", false},
		"another loopback port":           {gotrue("http://localhost:18399"), "http://localhost:18399", false},
		"https on the tailnet":            {gotrue("https://hub.example-tailnet.ts.net"), "https://hub.example-tailnet.ts.net", false},
		"another host on the main port":   {gotrue("http://hub.example.com:18315"), "http://hub.example.com:18315", false},
		"loopback on port 80, main is 80": {func() config { c := gotrue("http://localhost"); c.Port = 80; return c }(), "http://localhost:18327", true},
		"password mode":                   {func() config { c := gotrue("http://localhost:18315"); c.AuthMode = "password"; return c }(), "http://localhost:18315", false},
		"no browser listener":             {func() config { c := gotrue("http://localhost:18315"); c.BrowserPort = 0; return c }(), "http://localhost:18315", false},
	} {
		t.Run(name, func(t *testing.T) {
			base, note := claimLinkBase(c.cfg)
			if base != c.want {
				t.Errorf("base = %q, want %q", base, c.want)
			}
			if (note != "") != c.wantNote {
				t.Fatalf("note = %q, want one: %v", note, c.wantNote)
			}
			if c.wantNote {
				for _, w := range []string{envPublicURL + "=" + c.cfg.PublicURL, "machine-token port",
					envBrowserPort + "=18327", "Set " + envPublicURL + "=" + c.want} {
					if !strings.Contains(note, w) {
						t.Errorf("note %q lacks %q", note, w)
					}
				}
			}
		})
	}
}
