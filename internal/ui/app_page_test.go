package ui

import (
	"regexp"
	"strings"
	"testing"
)

// hostileAppLabel is a label no validator admits (1a's allows only letters,
// digits, spaces and - . & '): it closes an attribute, opens a tag, ends a
// single-quoted script string and carries an entity, so each context's
// escaping is exercised whatever the label rule says.
const hostileAppLabel = `Tom's & "Jerry"></h1><script>alert('x')</script>&amp;`

func sampleAppPageView(nav NavChrome) AppPageView {
	return AppPageView{Nav: nav, ID: 7, Label: "Team calls", Origin: "https://w.meet.example.test", Ref: "abc", MintPath: "/plugin-handoffs"}
}

var inlineScriptRE = regexp.MustCompile(`(?s)<script(\s[^>]*)?>(.*?)</script>`)

// TestTheAppPageEscapesAHostileLabelInEveryContext: the label rides the
// <title>, the header, the sidebar entry, the frame's title attribute and the
// Android card — each escaped for its context, never raw — and no script on
// the page carries any part of it (the page's script is a static file that
// reads no label).
func TestTheAppPageEscapesAHostileLabelInEveryContext(t *testing.T) {
	feat := featuresAllOn
	feat.Apps = []AppTab{{ID: 7, Label: hostileAppLabel, Icon: "📹"}}
	v := sampleAppPageView(NavChrome{Feat: feat})
	v.Label = hostileAppLabel
	page := renderString(t, AppPage(v))
	for _, raw := range []string{hostileAppLabel, `<script>alert`, `"Jerry">`, "Tom's"} {
		if strings.Contains(page, raw) {
			t.Errorf("the page carries %q raw", raw)
		}
	}
	esc := `Tom&#39;s &amp; &#34;Jerry&#34;&gt;&lt;/h1&gt;&lt;script&gt;alert(&#39;x&#39;)&lt;/script&gt;&amp;amp;`
	for name, want := range map[string]string{
		"<title>":                    "<title>" + esc + " · muster</title>",
		"the header":                 ">" + esc + "</h1>",
		"the sidebar entry":          `<span class="min-w-0 truncate">` + esc + `</span>`,
		"the frame's title":          `title="` + esc + `"`,
		"the card":                   ">" + esc + " opens in your browser</p>",
		"the sidebar entry's anchor": `id="sidebar-app-7" href="/apps/7"`,
	} {
		if n := strings.Count(page, want); n != 1 {
			t.Errorf("%s: %d of %q; want exactly 1", name, n, want)
		}
	}
	for _, m := range inlineScriptRE.FindAllStringSubmatch(page, -1) {
		if strings.Contains(m[2], "Jerry") || strings.Contains(m[2], "alert('x')") || strings.Contains(m[2], `alert(\'x\')`) {
			t.Errorf("a script carries the label: <script%s>%.300s", m[1], m[2])
		}
	}
}

// TestTheAppTabsAreTheLastSectionLinks: each of Features.Apps is a sidebar
// entry linking its page, after the built-in section links (after Officers)
// and before the account links, in the given order; the entry of the page
// being drawn carries aria-current and no other does. No Apps draws none.
func TestTheAppTabsAreTheLastSectionLinks(t *testing.T) {
	feat := featuresAllOn
	feat.Officers = true
	feat.Apps = []AppTab{{ID: 3, Label: "Calls", Icon: "📹"}, {ID: 9, Label: "Notes", Icon: "🗒️"}}
	v := sampleAppPageView(NavChrome{Feat: feat})
	v.ID = 9
	page := renderString(t, AppPage(v))
	off := strings.Index(page, `id="sidebar-officers"`)
	a, b := strings.Index(page, `id="sidebar-app-3"`), strings.Index(page, `id="sidebar-app-9"`)
	acct := strings.Index(page, `id="sidebar-prefs"`)
	if off < 0 || a < 0 || b < 0 || acct < 0 || !(off < a && a < b && b < acct) {
		t.Fatalf("order: officers %d, app 3 %d, app 9 %d, prefs %d; want officers < 3 < 9 < prefs", off, a, b, acct)
	}
	current := regexp.MustCompile(`<a id="sidebar-app-(\d+)"[^>]*aria-current="page"`).FindAllStringSubmatch(page, -1)
	if len(current) != 1 || current[0][1] != "9" {
		t.Errorf("aria-current on app entries %v; want app 9's alone", current)
	}
	none := renderString(t, AppPage(sampleAppPageView(NavChrome{Feat: featuresAllOn})))
	if strings.Contains(none, "data-app-tab") {
		t.Errorf("with no Apps the sidebar draws an app entry")
	}
}

// TestTheAppFrameIsTheDesignsExactly: one frame, sandboxed as the preview
// panel's (no allow-top-navigation), allow delegating exactly six features
// to exactly the entry origin, no-referrer, no src (the script swaps in the
// minted one), and the Android card hidden beside it.
func TestTheAppFrameIsTheDesignsExactly(t *testing.T) {
	page := renderString(t, AppPage(sampleAppPageView(NavChrome{Feat: featuresAllOn})))
	frames := regexp.MustCompile(`<iframe[^>]*>`).FindAllString(page, -1)
	if len(frames) != 1 {
		t.Fatalf("%d frames; want 1", len(frames))
	}
	want := `<iframe data-app-frame="" sandbox="allow-scripts allow-same-origin allow-forms allow-popups allow-downloads allow-modals" ` +
		`allow="camera https://w.meet.example.test; microphone https://w.meet.example.test; display-capture https://w.meet.example.test; ` +
		`fullscreen https://w.meet.example.test; autoplay https://w.meet.example.test; clipboard-write https://w.meet.example.test" ` +
		`referrerpolicy="no-referrer" title="Team calls" class="min-h-0 w-full flex-1 border-0 bg-bg">`
	if frames[0] != want {
		t.Errorf("the frame\n got %s\nwant %s", frames[0], want)
	}
	if !strings.Contains(page, `<div data-app-card="" hidden=""`) || !strings.Contains(page, `>Team calls opens in your browser</p>`) ||
		!strings.Contains(page, `data-app-open`) {
		t.Errorf("the Android card (hidden, saying the app opens in the browser, with Open) is not drawn")
	}
	for _, attr := range []string{`data-plugin="7"`, `data-mint="/plugin-handoffs"`, `data-alive="/ui/apps/7/alive"`, `data-poll="30"`, `data-ref="abc"`, `data-removed="This app was removed."`} {
		if !strings.Contains(page, attr) {
			t.Errorf("#app-main lacks %s", attr)
		}
	}
}
