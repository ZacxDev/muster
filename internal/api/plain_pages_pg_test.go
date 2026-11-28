package api

import (
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/plainscan"
)

// THE PAGES SCREENS ARE PLAIN (operator rule: only what the owner acts on in
// the top layer; every explanation behind an ⓘ). The list, a page's own
// screen and a link's view say no jargon (internal/plainscan's scans), no
// "(s)" plural and no zero said out loud (plainscan.Counts); a row's badges
// are drawn only when they say something; a page's screen draws its Links,
// Versions and the ⋯ beside Share only when there is something in them.
//
// Every title, name and address below is invented.

// pagesTopLayer fails for every top-layer text of markup (what a person reads
// without opening an ⓘ) that spells jargon or a clumsy count. CONTROL: it
// fails when there is no text at all.
func pagesTopLayer(t *testing.T, what, markup string) {
	t.Helper()
	texts := topLayerTexts(t, markup)
	if len(texts) == 0 {
		t.Fatalf("%s: CONTROL: no top-layer text at all — the scan sees nothing", what)
	}
	for _, s := range texts {
		if j := plainscan.Jargon(s); j != "" {
			t.Errorf("%s: the top layer says %q (jargon %q)", what, s, j)
		}
		if c := plainscan.Counts.FindString(s); c != "" {
			t.Errorf("%s: the top layer says %q (a clumsy count %q)", what, s, c)
		}
		// The Pages surfaces' own scan (pages_plain_pg_test.go): no address,
		// timestamp, hash, raw JSON, setting or id by number.
		if j := pageJargonIn(s); j != "" {
			t.Errorf("%s: the top layer says %q (%q)", what, s, j)
		}
	}
}

// TestTheCountsScanSeesAClumsyCount: the scan's own controls — it finds a
// "(s)" plural and a zero said out loud, in text and in a tooltip, and passes
// the counts a plain screen draws.
func TestTheCountsScanSeesAClumsyCount(t *testing.T) {
	for _, bad := range []string{
		`<span>by Release helper · version 1 of 1 · 0 live link(s), 0 waiting for you · 0 response(s)</span>`,
		`<span>3 response(s)</span>`,
		`<span>0 live link</span>`,
		`<span>0 responses</span>`,
		`<span>2 links, 0 waiting</span>`,
		`<a title="0 replies">x</a>`,
	} {
		hit := false
		for _, s := range topLayerTexts(t, bad) {
			hit = hit || plainscan.Counts.MatchString(s)
		}
		if !hit {
			t.Errorf("the scan missed the clumsy count in %s", bad)
		}
	}
	for _, ok := range []string{"v4", "Live", "2 waiting", "1 reply", "3 replies", "10 replies", "20 waiting", "2h ago", "17 Nov 2026, 20:03 UTC", "1.0 links"} {
		if c := plainscan.Counts.FindString(ok); c != "" {
			t.Errorf("the scan flagged %q in the plain %q", c, ok)
		}
	}
}

// pagesListRows is each row of the list's markup: its page id → its badges'
// texts, in order.
func pagesListRows(t *testing.T, list string) map[int64][]string {
	t.Helper()
	out := map[int64][]string{}
	rows := regexp.MustCompile(`(?s)<li data-page-id="(\d+)".*?</li>`).FindAllStringSubmatch(list, -1)
	for _, r := range rows {
		id, _ := strconv.ParseInt(r[1], 10, 64)
		badges := []string{}
		for _, b := range regexp.MustCompile(`data-page-badge="[^"]*"[^>]*>([^<]*)<`).FindAllStringSubmatch(r[0], -1) {
			badges = append(badges, b[1])
		}
		out[id] = badges
	}
	return out
}

// pagesSection is doc from the element carrying marker to the end of its first
// </section>.
func pagesSection(t *testing.T, doc, marker string) string {
	t.Helper()
	i := strings.Index(doc, marker)
	if i < 0 {
		t.Fatalf("no %s in %s", marker, doc)
	}
	i = strings.LastIndex(doc[:i], "<")
	j := strings.LastIndex(doc, "</section>")
	if j < i {
		t.Fatalf("no </section> after %s", marker)
	}
	return doc[i : j+len("</section>")]
}

// TestThePagesScreensArePlain: four pages whose counts are pairwise distinct
// (4 versions and nothing else; a live link and 3 replies; 2 links waiting;
// 2 versions, a live link, 1 waiting and 1 reply). The list draws exactly
// the badges each one has, and the list, every page's screen and a link's
// view say no jargon and no clumsy count.
//
// REGRESSION TEST: red at origin/main — the list said "not shared · 0
// responses" under a row with none, drew no badges, and its rows opened the
// page's screen, not its view. The page screens and the link's view already
// passed the plain scans there: those halves are invariant guards.
func TestThePagesScreensArePlain(t *testing.T) {
	f := newPageFixture(t)
	own := f.as(f.owner, authz.ViaSession)
	const qBlocks = `[{"type":"markdown","text":"# Pick a date"},{"type":"question","id":"q1","label":"Which day suits you?"}]`
	update := func(id int64, text string) {
		f.tool(t, f.agent, pageUpdateTool, map[string]any{"page_id": id, "blocks": json.RawMessage(`[{"type":"markdown","text":"` + text + `"},{"type":"question","id":"q1","label":"Which day suits you?"}]`)})
	}
	reply := func(token, answer string) {
		if w := f.submit(t, token, "q1", `{"answer":"`+answer+`"}`); w.Code != 200 {
			t.Fatalf("a reply through the link: %d %s", w.Code, w.Body.String())
		}
	}
	pending := func(id int64) {
		if _, err := f.pages.CreateLink(own, id, pages.LinkPending, "Release helper", true); err != nil {
			t.Fatal(err)
		}
	}
	plan := f.create(t, f.agent, "Quarter plan", qBlocks)
	for _, v := range []string{"two", "three", "four"} {
		update(plan, v)
	}
	notes := f.create(t, f.agent, "Launch notes", qBlocks)
	nl := f.liveLink(t, f.owner, notes)
	for _, a := range []string{"Monday", "Tuesday", "Friday"} {
		reply(nl.Token, a)
	}
	survey := f.create(t, f.agent, "Venue survey", qBlocks)
	pending(survey)
	pending(survey)
	budget := f.create(t, f.agent, "Budget review", qBlocks)
	update(budget, "two")
	bl := f.liveLink(t, f.owner, budget)
	reply(bl.Token, "Thursday")
	pending(budget)

	list := f.get(t, "/pages", f.ownerHdr()).Body.String()
	pagesTopLayer(t, "the Pages list", pagesSection(t, list, `data-testid="pages-page"`))
	got := pagesListRows(t, list)
	for id, want := range map[int64][]string{
		plan:   {"v4"},
		notes:  {"Live", "3 replies"},
		survey: {"2 waiting"},
		budget: {"v2", "Live", "1 waiting", "1 reply"},
	} {
		if !slices.Equal(got[id], want) {
			t.Errorf("page %d's badges: %q, want %q", id, got[id], want)
		}
	}
	// 🔴 THE ONE ID THE TOP LAYER DRAWS, ON PURPOSE (operator decision: "see
	// the page ID on the page card … and search by ID"): each row's number,
	// "#<id>", exactly. The scans above READ it — each number is one of the
	// top-layer texts they passed — and accept it because it is "#" and
	// digits; an id said any other way ("page 12") is still theirs to refuse
	// (pageJargon). So the acceptance is this shape on a row, not a wider scan.
	texts := topLayerTexts(t, pagesSection(t, list, `data-testid="pages-page"`))
	for _, id := range []int64{plan, notes, survey, budget} {
		num := "#" + strconv.FormatInt(id, 10)
		if !slices.Contains(texts, num) {
			t.Errorf("page %d's number %q is not in the list's top layer (%q)", id, num, texts)
		}
		if !regexp.MustCompile(`(?s)<li data-page-id="` + strconv.FormatInt(id, 10) + `".*?<span data-page-number="" class="[^"]*">` + num + `</span>`).MatchString(list) {
			t.Errorf("page %d's row does not draw its number as %q", id, num)
		}
	}
	if pageJargonIn("page 12") == "" {
		t.Errorf("CONTROL: the scan no longer refuses an id said as %q", "page 12")
	}
	for _, id := range []int64{plan, notes, survey, budget} {
		sid := strconv.FormatInt(id, 10)
		// The whole row opens the page's own view.
		if !strings.Contains(list, `<li data-page-id="`+sid+`"`) || !strings.Contains(list, `<a href="/pages/`+sid+`/view" hx-boost="false" data-page-row-open=""`) {
			t.Errorf("page %d's row does not open its view", id)
		}
		detail := f.get(t, "/pages/"+sid, f.ownerHdr()).Body.String()
		pagesTopLayer(t, "page "+sid+"'s screen", pagesSection(t, detail, `data-testid="page-detail"`))
	}
	shell := f.get(t, pages.LinkPath(nl.Token), nil).Body.String()
	pagesTopLayer(t, "a link's view", shell)
}

// TestAPagesScreenDrawsOnlyWhatItHas: a page with one version and no link has
// no Links section, no Versions section and no ⋯ beside Share (nothing for it
// to hold); once it has a live link and four versions, its Links section is
// drawn — the live link with Copy (its address) and Revoke — the ⋯ holds
// Revoke link and New link, and the versions are listed newest first, each
// row opening that version, an older one's ⋯ making it current.
//
// REGRESSION TEST: red at origin/main, which drew "Links", "No links." and a
// ⋯ with Revoke link and New link on a page with no link at all.
func TestAPagesScreenDrawsOnlyWhatItHas(t *testing.T) {
	f := newPageFixture(t).keyed(t)
	own := f.as(f.owner, authz.ViaSession)
	id := f.create(t, f.agent, "Seating chart", `[{"type":"markdown","text":"one"}]`)
	sid := strconv.FormatInt(id, 10)
	bare := pagesSection(t, f.get(t, "/pages/"+sid, f.ownerHdr()).Body.String(), `data-testid="page-detail"`)
	for _, banned := range []string{`data-testid="page-links"`, `data-testid="page-versions"`, `data-page-share-action="revoke"`, `data-page-share-action="rotate"`, `id="page-link-menu-`} {
		if strings.Contains(bare, banned) {
			t.Errorf("a page with no link and one version draws %s", banned)
		}
	}
	// CONTROL: the screen is drawn, with its Open and Share.
	if !strings.Contains(bare, `href="/pages/`+sid+`/view" hx-boost="false" data-page-view=""`) || !strings.Contains(bare, `data-page-share=""`) {
		t.Fatalf("the screen lacks Open or Share: %s", bare)
	}

	l, _, err := f.pages.ShareLink(own, id, "you", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"two", "three", "four"} {
		f.tool(t, f.agent, pageUpdateTool, map[string]any{"page_id": id, "blocks": json.RawMessage(`[{"type":"markdown","text":"` + v + `"}]`)})
	}
	url := pagesPublicURL + pages.SharePrefix + l.Token
	full := pagesSection(t, f.get(t, "/pages/"+sid, f.ownerHdr()).Body.String(), `data-testid="page-detail"`)
	for _, want := range []string{
		`data-testid="page-links"`,
		`data-link-state="live"`,
		`data-copy-text="` + url + `" data-link-copy`,
		`data-link-revoke>Revoke</button>`,
		`id="page-link-menu-` + sid + `"`,
		`data-page-share-action="revoke"`,
		`data-page-share-action="rotate"`,
	} {
		if !strings.Contains(full, want) {
			t.Errorf("a page with a live link lacks %q", want)
		}
	}
	var order []string
	for _, m := range regexp.MustCompile(`data-version="(\d+)">\s*<a href="/pages/`+sid+`/view\?v=(\d+)"[^>]*data-version-view>v(\d+)`).FindAllStringSubmatch(full, -1) {
		if m[1] != m[2] || m[1] != m[3] {
			t.Errorf("version row %s opens v=%s and says v%s", m[1], m[2], m[3])
		}
		order = append(order, m[1])
	}
	if !slices.Equal(order, []string{"4", "3", "2", "1"}) {
		t.Errorf("the versions, in order: %q, want 4, 3, 2, 1", order)
	}
	// No ⓘ on a link or a version row, and none holding the address: a
	// version's exact time and size are its time's tooltip, a link's address
	// is its Copy. RED at 35b55f6f3 (each row had an ⓘ, the address one too).
	rows := regexp.MustCompile(`(?s)<li [^>]*data-(link-id|version)="[^"]*".*?</li>`).FindAllString(full, -1)
	if len(rows) != 5 {
		t.Fatalf("CONTROL: %d link and version rows, want 5 (1 link, 4 versions)", len(rows))
	}
	for _, r := range rows {
		if strings.Contains(r, `data-info-open`) {
			t.Errorf("a row has an ⓘ: %s", r)
		}
	}
	if strings.Contains(full, `data-info-code="">`+url) || strings.Contains(full, `data-page-copy`) {
		t.Errorf("the address is in an ⓘ, or Copy link is still drawn")
	}
	if !regexp.MustCompile(`data-version="1">\s*<a [^>]*data-version-view>v1<span class="text-fg2"> · <time title="[0-9.]+ [kKMG]?B" data-plain-when-title=`).MatchString(full) {
		t.Errorf("version 1's time has no tooltip with its size: %s", full)
	}
	// The current version has no ⋯; each older one's makes it current.
	if strings.Contains(full, `id="version-menu-4"`) {
		t.Errorf("the current version has a ⋯")
	}
	for _, v := range []string{"3", "2", "1"} {
		if !regexp.MustCompile(`(?s)id="version-menu-` + v + `".*?<input type="hidden" name="version" value="` + v + `"><button type="submit" [^>]*data-version-current>Make current</button>`).MatchString(full) {
			t.Errorf("version %s's ⋯ does not make it current", v)
		}
	}
}

// TestPastLinksAreFoldedUnderTheirCount: a page with one live link, two
// revoked and one not approved lists the live one under Links, with Revoke,
// and the three past ones under a collapsed "Past links (3)", their rows
// inside it with no Revoke. muster has no expired state: a link is live,
// waiting, not approved or revoked.
//
// REGRESSION TEST: red at 35b55f6f3, which dropped the past links from the
// screen altogether.
func TestPastLinksAreFoldedUnderTheirCount(t *testing.T) {
	f := newPageFixture(t)
	own := f.as(f.owner, authz.ViaSession)
	id := f.create(t, f.agent, "Guest list", `[{"type":"markdown","text":"one"}]`)
	live := f.liveLink(t, f.owner, id)
	for range 2 {
		l := f.liveLink(t, f.owner, id)
		if _, err := f.pages.RevokeLink(own, id, l.ID); err != nil {
			t.Fatal(err)
		}
	}
	asked, err := f.pages.CreateLink(own, id, pages.LinkPending, "Release helper", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pages.DecideLink(own, asked.ID, false); err != nil {
		t.Fatal(err)
	}
	full := pagesSection(t, f.get(t, "/pages/"+strconv.FormatInt(id, 10), f.ownerHdr()).Body.String(), `data-testid="page-detail"`)
	openList := regexp.MustCompile(`(?s)data-testid="page-links">(.*?)</ul>`).FindStringSubmatch(full)
	pastBox := regexp.MustCompile(`(?s)<details data-page-links-past><summary [^>]*>([^<]*)</summary>(.*?)</details>`).FindStringSubmatch(full)
	if openList == nil || pastBox == nil {
		t.Fatalf("no open list or no folded past links: %s", full)
	}
	if n := strings.Count(openList[1], `<li `); n != 1 || !strings.Contains(openList[1], `data-link-id="`+strconv.FormatInt(live.ID, 10)+`" data-link-state="live"`) ||
		!strings.Contains(openList[1], `data-link-revoke`) {
		t.Errorf("the open links: %d rows, want the 1 live one with Revoke: %s", n, openList[1])
	}
	if pastBox[1] != "Past links (3)" {
		t.Errorf("the fold says %q, want %q", pastBox[1], "Past links (3)")
	}
	if strings.Count(pastBox[2], `data-link-state="revoked"`) != 2 || strings.Count(pastBox[2], `data-link-state="denied"`) != 1 ||
		strings.Contains(pastBox[2], `data-link-revoke`) {
		t.Errorf("the past links: %s", pastBox[2])
	}
}

// TestARowsVersionBadgeIsTheVersionShown: the list's vN badge is the page's
// CURRENT version (what its links show), not how many it has — after a
// rollback to version 1 of 3 the row says v1, as the page's screen does.
//
// REGRESSION TEST: red at 366348af4, whose row said v3 there.
func TestARowsVersionBadgeIsTheVersionShown(t *testing.T) {
	f := newPageFixture(t)
	own := f.as(f.owner, authz.ViaSession)
	id := f.create(t, f.agent, "Rolled back", `[{"type":"markdown","text":"one"}]`)
	for _, v := range []string{"two", "three"} {
		f.tool(t, f.agent, pageUpdateTool, map[string]any{"page_id": id, "blocks": json.RawMessage(`[{"type":"markdown","text":"` + v + `"}]`)})
	}
	if _, err := f.pages.SetCurrent(own, id, 1); err != nil {
		t.Fatal(err)
	}
	got := pagesListRows(t, f.get(t, "/pages", f.ownerHdr()).Body.String())[id]
	if !slices.Equal(got, []string{"v1"}) {
		t.Errorf("the rolled-back page's badges: %q, want [v1]", got)
	}
	if detail := f.get(t, "/pages/"+strconv.FormatInt(id, 10), f.ownerHdr()).Body.String(); !strings.Contains(detail, " · v1</p>") {
		t.Errorf("CONTROL: the page's screen does not say v1")
	}
}
