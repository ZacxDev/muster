package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// The CSP every response carried BEFORE the session-view chip, spelled out as
// literals (never built by contentSecurityPolicy, which is what is under
// test): every page but the switch's four, and the switch's four. Their
// img-src has since gained the server switcher's logo path on other origins
// (serverSwitcherImgSrc, docs/design/web-server-switcher.md), on every page
// alike.
const (
	cspBeforeNone = "default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data: https://*:*/brand/icon-192.png http://localhost:*/brand/icon-192.png http://127.0.0.1:*/brand/icon-192.png; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'"
	cspBeforeSelf = "default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data: https://*:*/brand/icon-192.png http://localhost:*/brand/icon-192.png http://127.0.0.1:*/brand/icon-192.png; connect-src 'self'; frame-ancestors 'self'; base-uri 'self'; form-action 'self'"
	chipZone      = "https://*.viewer.example.net"
	chipZone2     = "https://p.other.example.org"
)

// chipStore is the plugin store, played: List answers ps (the principal's
// rows, as RLS scopes the real one); nothing else is called.
type chipStore struct {
	plugins.Store
	ps    []plugins.Plugin
	err   error
	lists int
}

func (c *chipStore) List(context.Context) ([]plugins.Plugin, error) {
	c.lists++
	return c.ps, c.err
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
// MUSTER_PLUGIN_ORIGINS (origins "" = unset), and something to preview in the
// windows the tests open (@7 and @4), so the button's second gate is open:
// chipServerFm's fm is how a test closes it.
func chipServer(t *testing.T, origins string, ps ...plugins.Plugin) (*Server, func(path string) (*http.Response, string), *chipStore) {
	t.Helper()
	s, get, st, _ := chipServerFm(t, origins, ps...)
	return s, get, st
}

func chipServerFm(t *testing.T, origins string, ps ...plugins.Plugin) (*Server, func(path string) (*http.Response, string), *chipStore, *fakeMirror) {
	t.Helper()
	fm := goldHost()
	fm.previews = map[string]mirror.WindowPreviews{"@7": {Offered: 2, Open: 1}, "@4": {Offered: 1}}
	s, ts := viewServer(t, fm)
	pats, err := plugins.ParseOrigins(origins)
	if err != nil {
		t.Fatal(err)
	}
	st := &chipStore{ps: ps}
	s.ext.Plugins = st
	s.ext.PluginOrigins = pats
	return s, func(path string) (*http.Response, string) { return mirrorGet(t, s, ts, path) }, st, fm
}

// setPreviews replaces the host's counts (as a previews message would).
func (f *fakeMirror) setPreviews(p map[string]mirror.WindowPreviews) {
	f.mu.Lock()
	f.previews = p
	f.mu.Unlock()
}

var (
	panelRE    = regexp.MustCompile(`<aside id="preview-panel"[^>]*data-plugin="(\d+)"`)
	optionRE   = regexp.MustCompile(`<option value="(\d+)"`)
	toggleRE   = regexp.MustCompile(`<button[^>]*id="preview-toggle"[^>]*>`)
	openLinkRE = regexp.MustCompile(`<a data-preview-newtab=""[^>]*>`)
	iframeRE   = regexp.MustCompile(`<iframe [^>]*>`)
)

// chipIDs is the plugins the page's panel can open: its picker's options when
// it has one, else the one plugin the panel names; nil without a panel.
func chipIDs(page string) []string {
	m := panelRE.FindStringSubmatch(page)
	if m == nil {
		return nil
	}
	var out []string
	for _, o := range optionRE.FindAllStringSubmatch(page, -1) {
		out = append(out, o[1])
	}
	if out == nil {
		out = []string{m[1]}
	}
	return out
}

// toggleOf is the page's Preview button (its opening tag), "" for none.
func toggleOf(page string) string { return toggleRE.FindString(page) }

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
	want := "default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval'; style-src 'self' 'unsafe-inline'; img-src 'self' data: https://*:*/brand/icon-192.png http://localhost:*/brand/icon-192.png http://127.0.0.1:*/brand/icon-192.png; connect-src 'self'; frame-ancestors 'self'; " +
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
			t.Errorf("GET %s: the panel offers plugins %q, want one per live viewer plugin, 7,5,4", path, got)
		}
		if !strings.Contains(page, `data-host="ws"`) || !strings.Contains(page, `data-window="`+win+`"`) {
			t.Errorf("GET %s: the panel does not aim at this window:\n%s", path, page)
		}
		if toggleOf(page) == "" {
			t.Errorf("GET %s: no Preview button for a window with previews", path)
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
		if ids := chipIDs(page); len(ids) != 0 || strings.Contains(page, `id="preview-slot"`) || toggleOf(page) != "" {
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
		if strings.Contains(page, "preview-panel") || strings.Contains(page, "preview-slot") || toggleOf(page) != "" {
			t.Errorf("GET %s, MUSTER_PLUGIN_ORIGINS unset: the Preview button, its slot or its panel (or its script) is drawn", path)
		}
	}
	if st.lists != 0 {
		t.Errorf("the plugin store was read %d times with the seam off", st.lists)
	}
}

// TestThePanelsNewTabAndFrameAreLockedDown pins the two ways in as drawn: New
// tab is a new tab with rel="noopener noreferrer"; the panel's frame has EXACTLY the
// design's sandbox set (no allow-top-navigation of any kind), allow="" and
// referrerpolicy="no-referrer".
func TestThePanelsNewTabAndFrameAreLockedDown(t *testing.T) {
	_, get, _ := chipServer(t, chipZone, viewerRow(3, "Live", chipZone, "https://w.viewer.example.net/e"))
	_, page := get(ref("scratch2", "@7").ViewPath())
	a := openLinkRE.FindString(page)
	if !strings.Contains(a, `target="_blank"`) || !strings.Contains(a, `rel="noopener noreferrer"`) {
		t.Errorf("New tab link %q: want target=_blank rel=\"noopener noreferrer\"", a)
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
		t.Errorf("the panel's frame %q: want %s, allow=\"\", referrerpolicy=\"no-referrer\"", fr, sandbox)
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

// TestAPluginListErrorDrawsNoChipAndLeavesTheCSP: the store failing to list
// is logged and the views render as without plugins — no chip, the switch's
// own CSP — rather than failing the page or framing anything. The control is
// the same rows with no error, which do draw.
func TestAPluginListErrorDrawsNoChipAndLeavesTheCSP(t *testing.T) {
	for _, broken := range []bool{false, true} {
		_, get, st := chipServer(t, chipZone, viewerRow(3, "Live", chipZone, "https://w.viewer.example.net/e"))
		if broken {
			st.err = errors.New("the plugin store is down")
		}
		resp, page := get(ref("scratch2", "@7").ViewPath())
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("broken=%v: GET view = %d, want 200", broken, resp.StatusCode)
		}
		csp, ids := resp.Header.Get("Content-Security-Policy"), chipIDs(page)
		switch {
		case broken && (len(ids) != 0 || csp != cspBeforeSelf):
			t.Errorf("a List error drew chips %v with CSP %q; want none and\n %q", ids, csp, cspBeforeSelf)
		case !broken && (len(ids) != 1 || !strings.Contains(csp, "frame-src")):
			t.Errorf("CONTROL: no error drew chips %v with CSP %q; want one and a frame-src", ids, csp)
		}
		if st.lists == 0 {
			t.Errorf("broken=%v: the store was never asked", broken)
		}
	}
}

// TestThePreviewButtonIsDrawnOnlyWhileTheWindowHasSomethingToPreview: with a
// live viewer plugin, the Preview button is in a window's header only while
// its host reports something to preview there (operator decision 1). A window
// with nothing — a zero count, no entry, or a host that never reports (an
// older muster-mirror) — draws NO button; its slot, panel and frame-src stay,
// so the button can appear live. The window with counts is the control.
func TestThePreviewButtonIsDrawnOnlyWhileTheWindowHasSomethingToPreview(t *testing.T) {
	_, get, _, fm := chipServerFm(t, chipZone, viewerRow(3, "Live", chipZone, "https://w.viewer.example.net/e"))
	frame := "frame-src 'self' " + chipZone + "; "
	check := func(what, path string, wantCount string) {
		t.Helper()
		resp, page := get(path)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: GET %s = %d", what, path, resp.StatusCode)
		}
		b := toggleOf(page)
		switch {
		case wantCount == "" && b != "":
			t.Errorf("%s: GET %s draws a Preview button %q with nothing to preview", what, path, b)
		case wantCount != "" && !strings.Contains(b, `data-preview-count="`+wantCount+`"`):
			t.Errorf("%s: GET %s: Preview button %q, want one counting %s", what, path, b, wantCount)
		}
		// The second row's button (below sm) follows the same gate.
		row := regexp.MustCompile(`<button[^>]*id="preview-toggle-row"[^>]*>`).FindString(page)
		switch {
		case wantCount == "" && row != "":
			t.Errorf("%s: GET %s draws a second-row Preview button %q with nothing to preview", what, path, row)
		case wantCount != "" && !strings.Contains(row, `data-preview-count="`+wantCount+`"`):
			t.Errorf("%s: GET %s: second-row Preview button %q, want one counting %s", what, path, row, wantCount)
		}
		if !strings.Contains(page, `id="preview-slot-row"`) || !strings.Contains(page, `place=row`) {
			t.Errorf("%s: GET %s: no second-row slot polling its own place", what, path)
		}
		if !strings.Contains(page, `id="preview-slot"`) || len(chipIDs(page)) != 1 {
			t.Errorf("%s: GET %s: the slot and panel are not drawn for a live viewer plugin", what, path)
		}
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, frame) {
			t.Errorf("%s: GET %s: CSP %q has no %q (the panel could not frame a button that appears later)", what, path, csp, frame)
		}
	}
	// CONTROL: something to preview, on both views.
	check("@7 with 2 offered + 1 open", ref("scratch2", "@7").ViewPath(), "3")
	check("@7 with 2 offered + 1 open", ref("scratch2", "@7").TerminalPath(), "3")
	check("@4 with 1 offered", ref("scratch2", "@4").TerminalPath(), "1")
	// @9 has no entry in the map.
	check("@9 with no entry", ref("scratch2", "@9").ViewPath(), "")
	// A zero entry (a host would not send one; muster must not draw it).
	fm.setPreviews(map[string]mirror.WindowPreviews{"@7": {}})
	check("@7 with a zero entry", ref("scratch2", "@7").ViewPath(), "")
	// An older host: no previews message at all.
	fm.setPreviews(nil)
	check("an older muster-mirror", ref("scratch2", "@7").ViewPath(), "")
	check("an older muster-mirror", ref("scratch2", "@4").TerminalPath(), "")
}

// TestThePreviewButtonsPollFollowsTheCountsAndThePlugins: the slot's poll
// (ui.PreviewButtonPath) answers the button while the window has something to
// preview and the owner a live viewer plugin, and NOTHING otherwise — the
// counts going to zero, the plugin revoked since the page loaded, a window the
// host does not list. A window with nothing never reads the plugin store.
func TestThePreviewButtonsPollFollowsTheCountsAndThePlugins(t *testing.T) {
	_, get, st, fm := chipServerFm(t, chipZone, viewerRow(3, "Live", chipZone, "https://w.viewer.example.net/e"))
	poll := func(win string) string {
		t.Helper()
		resp, body := get(ui.PreviewButtonSrc("ws", "scratch2", win, ui.PreviewInHeader))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("poll %s = %d, want 200 (an empty answer, never an error)", win, resp.StatusCode)
		}
		if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-store") {
			t.Errorf("poll %s: Cache-Control %q", win, cc)
		}
		return body
	}
	if b := poll("@7"); !strings.Contains(b, `data-preview-count="3"`) || !strings.Contains(b, `id="preview-toggle"`) {
		t.Fatalf("CONTROL: poll @7 with 3 to preview = %q", b)
	}
	if _, b := get(ui.PreviewButtonSrc("ws", "scratch2", "@7", ui.PreviewInRow)); !strings.Contains(b, `id="preview-toggle-row"`) || !strings.Contains(b, `data-preview-count="3"`) {
		t.Errorf("poll @7 for the second row = %q, want the second row's button", b)
	}
	if b := poll("@7"); !strings.Contains(b, `aria-label="Preview: 1 preview open, 2 servers to preview"`) {
		t.Errorf("poll @7: the button's name does not say the counts: %q", b)
	}
	lists := st.lists
	fm.setPreviews(map[string]mirror.WindowPreviews{"@4": {Offered: 1}})
	if b := poll("@7"); b != "" {
		t.Errorf("poll @7 with nothing to preview = %q, want empty", b)
	}
	if st.lists != lists {
		t.Errorf("a poll for a window with nothing to preview read the plugin store (%d reads)", st.lists-lists)
	}
	if b := poll("@99"); b != "" {
		t.Errorf("poll of an unlisted window = %q, want empty", b)
	}
	if b := poll("@4"); !strings.Contains(b, `data-preview-count="1"`) {
		t.Errorf("CONTROL: poll @4 = %q", b)
	}
	// The plugin revoked since the page loaded: gone at the next poll.
	at := time.Unix(1_900_000_000, 0)
	st.ps[0].RevokedAt = &at
	if b := poll("@4"); b != "" {
		t.Errorf("poll @4 with the only viewer plugin revoked = %q, want empty", b)
	}
}

// TestTheSidebarMarksAWindowWithSomethingToPreview (operator decision 3): a
// window whose host reports something to preview draws the preview mark on
// its sidebar row, its counts in words its name — for an owner with a live
// viewer plugin, the Preview button's own gate; a window with nothing — and
// every window of a host that never reports — draws none.
func TestTheSidebarMarksAWindowWithSomethingToPreview(t *testing.T) {
	_, get, _, fm := chipServerFm(t, chipZone, viewerRow(3, "Live", chipZone, "https://w.viewer.example.net/e"))
	fm.setPreviews(map[string]mirror.WindowPreviews{"@7": {Offered: 2, Open: 1}, "@4": {}})
	_, list := get(ui.NavAgentsPath)
	row7 := cardFor(t, list, ref("scratch2", "@7").ViewPath())
	if !strings.Contains(row7, `data-nav-preview="3"`) || !strings.Contains(row7, `aria-label="Preview: 1 preview open, 2 servers to preview"`) {
		t.Errorf("@7's row has no preview mark naming its counts:\n%s", row7)
	}
	for _, href := range []string{ref("scratch2", "@9").ViewPath(), ref("scratch2", "@4").TerminalPath()} {
		if row := cardFor(t, list, href); strings.Contains(row, "data-nav-preview") {
			t.Errorf("a window with nothing to preview has a preview mark:\n%s", row)
		}
	}
	fm.setPreviews(nil)
	_, list = get(ui.NavAgentsPath)
	if strings.Contains(list, "data-nav-preview") {
		t.Error("a host that reports no previews (an older muster-mirror) has a preview mark")
	}
}

// TestTheSidebarMarksNothingWithoutALiveViewerPlugin: the host reports
// something to preview, but the owner has no viewer plugin the Preview button
// would open — a revoked one, one without viewer, or MUSTER_PLUGIN_ORIGINS
// unset — so the views draw no button, and the sidebar must not point at one.
// The plugin store is read at most once per list, and not at all with the
// seam off.
func TestTheSidebarMarksNothingWithoutALiveViewerPlugin(t *testing.T) {
	revoked := viewerRow(9, "Revoked", chipZone, "https://w.viewer.example.net/e")
	at := time.Unix(1_900_000_000, 0)
	revoked.RevokedAt = &at
	for _, tc := range []struct {
		name    string
		origins string
		ps      []plugins.Plugin
		want    bool
		lists   int
	}{
		{"CONTROL: a live viewer plugin", chipZone, []plugins.Plugin{viewerRow(3, "Live", chipZone, "https://w.viewer.example.net/e")}, true, 1},
		{"a revoked viewer plugin", chipZone, []plugins.Plugin{revoked}, false, 1},
		{"a messages-only plugin", chipZone, []plugins.Plugin{viewerRow(8, "Messages", chipZone, "https://w.viewer.example.net/m", plugins.CapMessages)}, false, 1},
		{"MUSTER_PLUGIN_ORIGINS unset", "", []plugins.Plugin{viewerRow(3, "Live", chipZone, "https://w.viewer.example.net/e")}, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, get, st, fm := chipServerFm(t, tc.origins, tc.ps...)
			fm.setPreviews(map[string]mirror.WindowPreviews{"@7": {Offered: 2, Open: 1}, "@4": {Offered: 1}})
			before := st.lists
			_, list := get(ui.NavAgentsPath)
			if got := strings.Contains(list, "data-nav-preview"); got != tc.want {
				t.Errorf("preview mark drawn = %v, want %v", got, tc.want)
			}
			if n := st.lists - before; n > tc.lists {
				t.Errorf("the plugin store was read %d times for one list, want at most %d", n, tc.lists)
			}
		})
	}
}
