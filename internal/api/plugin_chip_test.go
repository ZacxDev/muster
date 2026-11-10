package api

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// The CSP every response carried BEFORE the session-view chip, spelled out as
// literals (never built by contentSecurityPolicy, which is what is under
// test): every page but the switch's four, and the switch's four.
const (
	cspBeforeNone = "default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'"
	cspBeforeSelf = "default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'self'; base-uri 'self'; form-action 'self'"
	chipZone      = "https://*.viewer.example.net"
	chipZone2     = "https://p.other.example.org"
)

// chipStore is the plugin store, played: List answers ps (the principal's
// rows, as RLS scopes the real one); nothing else is called.
type chipStore struct {
	plugins.Store
	ps    []plugins.Plugin
	lists int
}

func (c *chipStore) List(context.Context) ([]plugins.Plugin, error) {
	c.lists++
	return c.ps, nil
}

func viewerRow(id int64, label, origin, entry string, caps ...plugins.Capability) plugins.Plugin {
	if len(caps) == 0 {
		caps = []plugins.Capability{plugins.CapViewer}
	}
	p := plugins.Plugin{ID: id, Name: "p" + label, Label: label, Capabilities: caps}
	if origin != "" {
		p.Origin, p.EntryURL = &origin, &entry
	}
	return p
}

// chipServer is viewServer's per-window host with the plugin store and
// MUSTER_PLUGIN_ORIGINS (origins "" = unset).
func chipServer(t *testing.T, origins string, ps ...plugins.Plugin) (*Server, func(path string) (*http.Response, string), *chipStore) {
	t.Helper()
	s, ts := viewServer(t, goldHost())
	pats, err := plugins.ParseOrigins(origins)
	if err != nil {
		t.Fatal(err)
	}
	st := &chipStore{ps: ps}
	s.ext.Plugins = st
	s.ext.PluginOrigins = pats
	return s, func(path string) (*http.Response, string) { return mirrorGet(t, s, ts, path) }, st
}

var (
	chipRE     = regexp.MustCompile(`data-plugin-chip="(\d+)"`)
	openLinkRE = regexp.MustCompile(`<a data-plugin-open=""[^>]*>`)
	iframeRE   = regexp.MustCompile(`<iframe [^>]*>`)
)

func chipIDs(page string) []string {
	var out []string
	for _, m := range chipRE.FindAllStringSubmatch(page, -1) {
		out = append(out, m[1])
	}
	return out
}

// TestTheSessionViewsFrameTheOwnersViewerOriginsAndNoOtherPageChanges: with
// MUSTER_PLUGIN_ORIGINS set and the owner holding live viewer plugins, a
// mirrored window's Rendered AND Terminal views carry frame-src 'self' plus
// each plugin's origin once — and draw one chip per plugin — while every other
// page's CSP is BYTE-IDENTICAL to what it was before the chip (the literals
// above). 'self' is kept: the Rendered | Terminal switch frames this origin's
// other view (ui/view_swap.go; e2e view-swap.spec.ts is the browser control).
func TestTheSessionViewsFrameTheOwnersViewerOriginsAndNoOtherPageChanges(t *testing.T) {
	_, get, st := chipServer(t, chipZone+","+chipZone2,
		viewerRow(7, "Previews", chipZone, "https://w.viewer.example.net/__muster/enter"),
		viewerRow(5, "Second", chipZone, "https://x.viewer.example.net/enter"),
		viewerRow(4, "Elsewhere", chipZone2, "https://p.other.example.org/in"))
	want := "default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'self'; " +
		"frame-src 'self' " + chipZone + " " + chipZone2 + "; base-uri 'self'; form-action 'self'"
	for path, win := range map[string]string{ref("scratch2", "@7").ViewPath(): "@7", ref("scratch2", "@7").TerminalPath(): "@7", ref("scratch2", "@4").TerminalPath(): "@4"} {
		resp, page := get(path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200 (fixture)", path, resp.StatusCode)
		}
		if got := resp.Header.Get("Content-Security-Policy"); got != want {
			t.Errorf("GET %s: CSP\n %q\nwant\n %q", path, got, want)
		}
		if got := strings.Join(chipIDs(page), ","); got != "7,5,4" {
			t.Errorf("GET %s: chips %q, want one per live viewer plugin, 7,5,4", path, got)
		}
		if !strings.Contains(page, `data-host="ws"`) || !strings.Contains(page, `data-window="`+win+`"`) {
			t.Errorf("GET %s: the chip row does not aim at this window:\n%s", path, page)
		}
	}
	lists := st.lists
	// Every other page: exactly the CSP it had.
	for path, before := range map[string]string{"/tasks": cspBeforeNone, "/": cspBeforeNone} {
		resp, page := get(path)
		if got := resp.Header.Get("Content-Security-Policy"); got != before {
			t.Errorf("GET %s (%d): CSP\n %q\nwant (unchanged)\n %q", path, resp.StatusCode, got, before)
		}
		if ids := chipIDs(page); len(ids) != 0 {
			t.Errorf("GET %s draws plugin chips %v", path, ids)
		}
	}
	if st.lists != lists {
		t.Errorf("a page that is not a session view read the plugin list (%d reads)", st.lists-lists)
	}
}

// TestOnlyALiveViewerPluginWithAnAllowedOriginDrawsAChip: a revoked token, a
// messages-only one, and one whose origin the admin has since dropped from
// MUSTER_PLUGIN_ORIGINS draw nothing (each would be refused at mint) — and
// with only those, the views' CSP is the switch's own, byte for byte, with no
// frame-src. The live one beside them is the positive control.
func TestOnlyALiveViewerPluginWithAnAllowedOriginDrawsAChip(t *testing.T) {
	revoked := viewerRow(9, "Revoked", chipZone, "https://w.viewer.example.net/e")
	at := time.Unix(1_900_000_000, 0)
	revoked.RevokedAt = &at
	dead := []plugins.Plugin{
		revoked,
		// Origin set although 0103 ties it to viewer: the capability check,
		// not the missing origin, must be what drops it.
		viewerRow(8, "Messages", chipZone, "https://w.viewer.example.net/m", plugins.CapMessages),
		viewerRow(6, "Retired", chipZone2, "https://p.other.example.org/in"),
	}
	live := viewerRow(3, "Live", chipZone, "https://w.viewer.example.net/e")
	view := ref("scratch2", "@7").ViewPath()

	_, get, _ := chipServer(t, chipZone, append(dead, live)...)
	resp, page := get(view)
	if got := strings.Join(chipIDs(page), ","); got != "3" {
		t.Errorf("chips %q, want the live one alone (3)", got)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-src 'self' "+chipZone+"; ") || strings.Contains(csp, chipZone2) {
		t.Errorf("CSP %q: want frame-src of the live plugin's origin only", csp)
	}

	_, get, _ = chipServer(t, chipZone, dead...)
	for _, path := range []string{view, ref("scratch2", "@7").TerminalPath()} {
		resp, page = get(path)
		if ids := chipIDs(page); len(ids) != 0 || strings.Contains(page, `id="plugin-chips"`) {
			t.Errorf("GET %s: a revoked, messages-only or retired plugin drew chips %v", path, ids)
		}
		if got := resp.Header.Get("Content-Security-Policy"); got != cspBeforeSelf {
			t.Errorf("GET %s with no live viewer plugin: CSP\n %q\nwant (unchanged)\n %q", path, got, cspBeforeSelf)
		}
	}
}

// TestWithMusterPluginOriginsUnsetThereIsNoChipAndNoFrameSrc: the same live
// viewer plugin, the variable unset — no chip, no frame-src, and the store is
// not even read.
func TestWithMusterPluginOriginsUnsetThereIsNoChipAndNoFrameSrc(t *testing.T) {
	_, get, st := chipServer(t, "", viewerRow(3, "Live", chipZone, "https://w.viewer.example.net/e"))
	for _, path := range []string{ref("scratch2", "@7").ViewPath(), ref("scratch2", "@7").TerminalPath()} {
		resp, page := get(path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d", path, resp.StatusCode)
		}
		if got := resp.Header.Get("Content-Security-Policy"); got != cspBeforeSelf {
			t.Errorf("GET %s, MUSTER_PLUGIN_ORIGINS unset: CSP\n %q\nwant (unchanged)\n %q", path, got, cspBeforeSelf)
		}
		if strings.Contains(page, "data-plugin-chip") || strings.Contains(page, "plugin-chip") {
			t.Errorf("GET %s, MUSTER_PLUGIN_ORIGINS unset: a chip (or its script) is drawn", path)
		}
	}
	if st.lists != 0 {
		t.Errorf("the plugin store was read %d times with the seam off", st.lists)
	}
}

// TestTheChipsOpenAndFrameAreLockedDown pins the two ways in as drawn: Open is
// a new tab with rel="noopener noreferrer"; Show here's frame has EXACTLY the
// design's sandbox set (no allow-top-navigation of any kind), allow="" and
// referrerpolicy="no-referrer".
func TestTheChipsOpenAndFrameAreLockedDown(t *testing.T) {
	_, get, _ := chipServer(t, chipZone, viewerRow(3, "Live", chipZone, "https://w.viewer.example.net/e"))
	_, page := get(ref("scratch2", "@7").ViewPath())
	a := openLinkRE.FindString(page)
	if !strings.Contains(a, `target="_blank"`) || !strings.Contains(a, `rel="noopener noreferrer"`) {
		t.Errorf("Open link %q: want target=_blank rel=\"noopener noreferrer\"", a)
	}
	frames := iframeRE.FindAllString(page, -1)
	var fr string
	for _, f := range frames {
		if strings.Contains(f, "sandbox=") {
			fr = f
		}
	}
	const sandbox = `sandbox="allow-scripts allow-same-origin allow-forms allow-popups allow-downloads allow-modals"`
	if !strings.Contains(fr, sandbox) || !strings.Contains(fr, `allow=""`) || !strings.Contains(fr, `referrerpolicy="no-referrer"`) {
		t.Errorf("Show here frame %q: want %s, allow=\"\", referrerpolicy=\"no-referrer\"", fr, sandbox)
	}
	if strings.Contains(page, "allow-top-navigation") {
		t.Error("the page grants allow-top-navigation somewhere")
	}
	if ui.PluginChipSandbox != strings.TrimSuffix(strings.TrimPrefix(sandbox, `sandbox="`), `"`) {
		t.Errorf("ui.PluginChipSandbox = %q", ui.PluginChipSandbox)
	}
}

// TestTheCSPWithoutFrameSrcIsTheOneBefore: contentSecurityPolicy with no
// frame-src is byte-identical to the pre-chip policy for both ancestors.
func TestTheCSPWithoutFrameSrcIsTheOneBefore(t *testing.T) {
	if got := contentSecurityPolicy("'none'", ""); got != cspBeforeNone {
		t.Errorf("'none': %q", got)
	}
	if got := contentSecurityPolicy("'self'", ""); got != cspBeforeSelf {
		t.Errorf("'self': %q", got)
	}
}
