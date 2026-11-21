package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/pages"
)

// PLAIN PAGES SCREENS (the operator rule: muster is for people who are not
// engineers, so its own chrome shows no address, id, hash, timestamp, raw JSON
// or setting name in its top layer). A share link's ask card names the page
// by its title and opens it from a button; the page's screen says "Copy link"
// / "Share" with the address in an ⓘ, plain dates, responses as labelled
// fields from the page's own form, and "Sharing isn't set up on this server"
// with the setting in an admin-only ⓘ.

// pageJargon is what the top layer of a Pages surface must not hold: an
// address (a scheme, a link's /s/ path, a page's /pages/<id> path), a
// timestamp (an ISO date, "UTC"), a hash fragment (8+ lower-case hex digits
// holding a digit), raw JSON (a brace or bracket opening a quoted key), a
// setting's name (MUSTER_…), and an id by number ("link 7", "page 12").
var pageJargon = regexp.MustCompile(`https?://|/s/[A-Za-z0-9_-]{8,}|/pages/[0-9]+|\b[0-9]{4}-[0-9]{2}-[0-9]{2}\b|\bUTC\b|` +
	`\b[0-9a-f]*[0-9][0-9a-f]*[a-f][0-9a-f]*\b|[{\[]\s*"|\bMUSTER_[A-Z_]+|\b(link|page|version) id\b|\blink [0-9]+\b|\bpage [0-9]+\b`)

// pageJargonIn is pageJargon's first match in s, "" for none; a hex run is a
// hash only at 8+ characters (RE2 has no lookahead).
func pageJargonIn(s string) string {
	for _, m := range pageJargon.FindAllString(s, -1) {
		if regexp.MustCompile(`^[0-9a-f]+$`).MatchString(m) && len(m) < 8 {
			continue
		}
		return m
	}
	return ""
}

// pageTopLayerTexts is topLayerTexts plus what an <input> SHOWS (its value,
// unless it is hidden): the old Share box drew the address that way, so a
// scan of text nodes alone could not see it.
func pageTopLayerTexts(t *testing.T, markup string) []string {
	t.Helper()
	out := topLayerTexts(t, markup)
	z := html.NewTokenizer(strings.NewReader(markup))
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			if z.Err() != io.EOF {
				t.Fatalf("parse: %v", z.Err())
			}
			return out
		}
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			continue
		}
		tok := z.Token()
		if tok.Data != "input" {
			continue
		}
		hidden, value := false, ""
		for _, a := range tok.Attr {
			switch a.Key {
			case "type":
				hidden = a.Val == "hidden"
			case "value":
				value = a.Val
			}
		}
		if !hidden && strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
}

// assertPagePlain fails for every top-layer text of markup that pageJargon
// matches.
func assertPagePlain(t *testing.T, what, markup string) {
	t.Helper()
	texts := pageTopLayerTexts(t, markup)
	if len(texts) == 0 {
		t.Fatalf("%s: CONTROL: no top-layer text at all — the scan sees nothing", what)
	}
	for _, s := range texts {
		if m := pageJargonIn(s); m != "" {
			t.Errorf("%s: the top layer says %q (%q)", what, s, m)
		}
		// A mirrored window's tmux address (#806's scan) is jargon here too.
		if jargonInTopLayer.MatchString(s) {
			t.Errorf("%s: the top layer spells a tmux address: %q", what, s)
		}
	}
}

// TestThePageJargonScanSeesEachKind: the scan's own controls. POSITIVE: each
// kind of jargon, in a text, a tooltip, an aria-label and an input's value, is
// found. NEGATIVE: plain words with numbers, an ⓘ sheet holding all of it, a
// script, a data attribute and a hidden input pass.
func TestThePageJargonScanSeesEachKind(t *testing.T) {
	for _, bad := range []string{
		`<p>See the page as it is now: https://muster.example.test/pages/12/preview.</p>`,
		`<input type="text" readonly value="https://muster.example.test/s/AbCdEfGhIjKlMnOpQrStUvWxYz0123456789abcdefg">`,
		`<span>created 2026-11-11 14:02 UTC · 3fa9c2e1</span>`,
		`<span>3fa9c2e1</span>`,
		`<pre>{
  "fields": {"email": "a@example.com"}
}</pre>`,
		`<p>This server keeps no reusable links (MUSTER_CONNECTOR_ENCRYPTION_KEY is unset)</p>`,
		`<span>contact · version 1 · link 7 · today</span>`,
		`<a title="Open /pages/3">x</a>`,
		`<button aria-label="Copy https://x.example">x</button>`,
	} {
		hit := false
		for _, s := range pageTopLayerTexts(t, bad) {
			hit = hit || pageJargonIn(s) != ""
		}
		if !hit {
			t.Errorf("the scan missed the jargon in %s", bad)
		}
	}
	ok := `<p>Share the page “Pick a subject line”? Version 2 of 3 · 3 responses · Updated today at 9:14 · 1 waiting for you · added 12 faces</p>` +
		`<div data-info-sheet=""><code>https://muster.example.test/s/abc · created 2026-11-11 14:02 UTC · 3fa9c2e1 · MUSTER_X · {"a":1}</code></div>` +
		`<script>var u="https://x/s/abcdefghij"</script><span data-share-url="https://x/s/abcdefghij">Share</span>` +
		`<input type="hidden" name="version" value="2026-11-11"><time datetime="2026-11-18T09:14:00Z">Sent today at 9:14</time>`
	for _, s := range pageTopLayerTexts(t, ok) {
		if m := pageJargonIn(s); m != "" {
			t.Errorf("the scan flagged %q (%q) in plain markup", s, m)
		}
	}
}

// pagePlainBlocks is a page whose form, question, choice and approval each
// have labels: what the owner's screen must name its responses by.
const pagePlainBlocks = `[
 {"type":"markdown","text":"# Pick a subject line"},
 {"type":"question","id":"note","label":"Anything else?"},
 {"type":"choice","id":"pick","label":"Which subject line?","options":["Spring sale","Last call"]},
 {"type":"form","id":"contact","label":"Your details","fields":[
   {"name":"email","label":"Email","type":"email","required":true},
   {"name":"seats","label":"Seats","type":"number"},
   {"name":"agree","label":"Send me news","type":"checkbox"}]},
 {"type":"approval","id":"ok","label":"Go ahead?","comment":true}
]`

// pagePlainSurfaces publishes a page, has its agent ask to share it, answers
// every part through a live link, and returns the page and its ask.
func pagePlainSurfaces(t *testing.T, f *pageFixture) (int64, int64, pages.Link) {
	t.Helper()
	id := f.create(t, f.agent, "Pick a subject line", pagePlainBlocks)
	req := f.tool(t, f.agent, pageShareRequestTool, map[string]any{"page_id": id, "reason": "to pick the launch email"})
	askID := pnum(req["ask_id"])
	if askID <= 0 {
		t.Fatalf("page_share_request: %v", req)
	}
	l := f.liveLink(t, f.owner, id)
	for comp, data := range map[string]string{
		"note":    `{"answer":"Ship it on Friday"}`,
		"pick":    `{"choices":["Last call"]}`,
		"contact": `{"fields":{"email":"client@example.com","seats":12,"agree":true}}`,
		"ok":      `{"decision":"approve","comment":"Looks right"}`,
	} {
		if w := f.submit(t, l.Token, comp, data); w.Code != 200 {
			t.Fatalf("submit %s: %d %s", comp, w.Code, w.Body.String())
		}
	}
	return id, askID, l
}

// pagePlainMirrored publishes a page from a mirrored session whose window had
// no human name — stored, as mirrorLabel stores it, by its code and host
// ("work:1 @ atlas") — and has it ask to share the page; the page and its ask.
func pagePlainMirrored(t *testing.T, f *pageFixture) (int64, int64) {
	t.Helper()
	ctx := f.as(f.owner, authz.ViaMirrorHost)
	c := toolCaller{asker: attention.Asker{MirrorHost: "atlas", MirrorSession: "0d1e2f3a-aaaa-4bbb-8ccc-0000000000f1", Label: "work:1 @ atlas"}}
	out := f.toolAs(t, ctx, c, pageCreateTool, map[string]any{"title": "Notes from a session", "blocks": json.RawMessage(`[{"type":"markdown","text":"hi"}]`)})
	id := pnum(out["page_id"])
	req := f.toolAs(t, ctx, c, pageShareRequestTool, map[string]any{"page_id": id})
	askID := pnum(req["ask_id"])
	if id <= 0 || askID <= 0 {
		t.Fatalf("a mirrored session's page and ask: %v %v", out, req)
	}
	return id, askID
}

// pageAs is the owner's request context in role.
func (f *pageFixture) pageAs(role authz.Role) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: f.owner, Role: role, Via: authz.ViaSession})
}

// pageSection is markup's <section …data-testid="page-detail"…>…</section>
// (the page screen's own content, not the nav around it).
func pageSection(t *testing.T, body, testid string) string {
	t.Helper()
	i := strings.Index(body, `data-testid="`+testid+`"`)
	if i < 0 {
		t.Fatalf("no %s in:\n%.3000s", testid, body)
	}
	i = strings.LastIndex(body[:i], "<section")
	j := strings.LastIndex(body, "</section>")
	return body[i : j+len("</section>")]
}

// TestNoTopLayerOfThePageScreensSpellsALinkAnIdOrASetting: a share link's ask
// card (the Asks feed and the ask's own page), the Pages list and the page's
// screen — with a live reusable link, with none and no link key, and right
// after a one-off link is made — for an ADMIN and for a MEMBER owner: no
// address, id, hash, timestamp, JSON or setting name in the top layer, and
// what is needed is one tap away: the card's "Open the page", Copy link and
// Share holding the exact link, the link in an ⓘ, the setting in an ⓘ only an
// admin is shown.
//
// REGRESSION TEST: red at origin/main, whose card said "See the page as it is
// now: https://…/pages/<id>/view", whose page screen drew the link in an
// input, "created 2026-11-11 14:02 UTC · 3fa9c2e1" per link, raw JSON with
// "contact · version 1 · link 7" per response, and "(MUSTER_CONNECTOR_
// ENCRYPTION_KEY is unset)".
func TestNoTopLayerOfThePageScreensSpellsALinkAnIdOrASetting(t *testing.T) {
	for _, role := range []authz.Role{authz.RoleAdmin, authz.RoleMember} {
		t.Run(string(role), func(t *testing.T) {
			f := newPageFixture(t).askFlow(t)
			id, askID, oneOff := pagePlainSurfaces(t, f)
			mid, mask := pagePlainMirrored(t, f)
			ctx := f.pageAs(role)
			sid := strconv.FormatInt(id, 10)

			// The ask card: the feed's and the ask's own page.
			a, err := f.srv.ext.Attention.Get(ctx, askID)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(a.Question, "Share the page “Pick a subject line”? ") {
				t.Errorf("the question does not name the page by its title first: %q", a.Question)
			}
			rec := httptest.NewRecorder()
			f.srv.handleAttentionItems(rec, httptest.NewRequest("GET", "/attention/items", nil).WithContext(ctx))
			feed := rec.Body.String()
			at := strings.Index(feed, `<li id="ask-`+strconv.FormatInt(askID, 10)+`"`)
			if at < 0 {
				t.Fatalf("CONTROL: the feed has no card for ask %d:\n%.3000s", askID, feed)
			}
			card := feed[at:]
			card = card[:strings.Index(card, "</li>\n")+len("</li>")]
			assertPagePlain(t, "the Asks feed's share card", card)
			// A mirrored session's: named in words ("work on atlas").
			ma, err := f.srv.ext.Attention.Get(ctx, mask)
			if err != nil || !strings.HasPrefix(ma.Question, "Share the page “Notes from a session”? work on atlas asks") {
				t.Errorf("a mirrored session's share question: %q %v", ma.Question, err)
			}
			rec = httptest.NewRecorder()
			f.srv.renderAskPage(rec, httptest.NewRequest("GET", askPagePath(mask), nil).WithContext(ctx), ma)
			mpage := rec.Body.String()
			// Its question, which this PR words. ⚠ Not its head: an ask whose
			// session no window shows any more is still headed by its stored
			// label ("work:1 @ atlas (not shown …)"), the Asks surface's own
			// open case (TestNoTopLayerOfTheAsksSurfacesSpellsATmuxAddress's
			// "Not covered").
			q := regexp.MustCompile(`(?s)<div [^>]*data-ask-question>.*?</div>`).FindString(mpage)
			if q == "" {
				t.Fatalf("CONTROL: no question on the mirrored session's ask page")
			}
			assertPagePlain(t, "a mirrored session's share question", q)
			rec = httptest.NewRecorder()
			f.srv.renderAskPage(rec, httptest.NewRequest("GET", askPagePath(askID), nil).WithContext(ctx), a)
			body := rec.Body.String()
			page := body[strings.Index(body, `<article`):]
			page = page[:strings.Index(page, `</article>`)+len("</article>")]
			assertPagePlain(t, "the share ask's page", page)
			open := regexp.MustCompile(`<a href="/pages/` + sid + `/view" class="[^"]*" data-ask-page-open>Open the page</a>`)
			for what, m := range map[string]string{"the feed's card": card, "the ask's page": page} {
				if !open.MatchString(m) {
					t.Errorf("%s has no “Open the page” to the owner's view:\n%s", what, m)
				}
			}

			// The page screen, no link key: one-off links, and the setting.
			get := func(path string, render func(w *httptest.ResponseRecorder, r *http.Request)) string {
				w := httptest.NewRecorder()
				render(w, httptest.NewRequest("GET", path, nil).WithContext(ctx))
				return w.Body.String()
			}
			detail := pageSection(t, get("/pages/"+sid, func(w *httptest.ResponseRecorder, r *http.Request) {
				f.srv.renderPageDetail(w, r, id, 200, "", "", "")
			}), "page-detail")
			assertPagePlain(t, "the page screen with no link key", detail)
			// Why Share makes one-off links here is the ⓘ beside Share.
			if !regexp.MustCompile(`id="page-share-setup"[^>]*data-info-sheet[\s\S]*Sharing isn(&#39;|')t set up on this server`).MatchString(detail) {
				t.Errorf("the page screen's ⓘ does not say sharing is not set up:\n%s", detail)
			}
			setting := `data-info-code="">MUSTER_CONNECTOR_ENCRYPTION_KEY</code>`
			if role == authz.RoleAdmin && !strings.Contains(detail, setting) {
				t.Errorf("an admin's page screen has no ⓘ holding the setting")
			}
			if role == authz.RoleMember && (strings.Contains(detail, "MUSTER_CONNECTOR_ENCRYPTION_KEY") || !strings.Contains(detail, `data-info-withheld`)) {
				t.Errorf("a member's page screen names the setting, or does not say to ask their admin")
			}
			// Each link is told apart by when it was made — never by a hash,
			// in the top layer or an ⓘ (the row has none). A one-off link's
			// address is not kept, so its row has no Copy.
			oneRow := regexp.MustCompile(`(?s)<li [^>]*data-link-id="` + strconv.FormatInt(oneOff.ID, 10) + `".*?</li>`).FindString(detail)
			if !strings.Contains(oneRow, `data-link-made><time `) || strings.Contains(detail, oneOff.Label()) ||
				strings.Contains(oneRow, `data-info-open`) || strings.Contains(oneRow, `data-link-copy`) {
				t.Errorf("the one-off link's row: %s", oneRow)
			}
			// The responses, labelled from the form: every value, by its label.
			for _, want := range []string{
				`Your details</span>`, `<dt class="text-xs text-fg2">Email</dt><dd class="whitespace-pre-wrap break-words text-sm text-fg" data-response-value>client@example.com</dd>`,
				`<dt class="text-xs text-fg2">Seats</dt><dd class="whitespace-pre-wrap break-words text-sm text-fg" data-response-value>12</dd>`,
				`<dt class="text-xs text-fg2">Send me news</dt><dd class="whitespace-pre-wrap break-words text-sm text-fg" data-response-value>Yes</dd>`,
				`Which subject line?</span>`, `data-response-value>Last call</dd>`,
				`Anything else?</span>`, `data-response-value>Ship it on Friday</dd>`,
				`Go ahead?</span>`, `data-response-value>Approved</dd>`, `data-response-value>Looks right</dd>`,
			} {
				if !strings.Contains(detail, want) {
					t.Errorf("the responses lack %q", want)
				}
			}
			// The stored form: an admin's "Show raw" ⓘ only.
			raw := strings.Count(detail, `aria-label="Show raw"`)
			if role == authz.RoleAdmin && (raw != 4 || !strings.Contains(detail, `· {&#34;fields&#34;:`)) {
				t.Errorf("an admin's responses: %d “Show raw” ⓘ, want 4 holding the stored data", raw)
			}
			if role == authz.RoleMember && (raw != 0 || strings.Contains(detail, "&#34;fields&#34;")) {
				t.Errorf("a member's responses carry the raw data (%d ⓘ)", raw)
			}

			// Right after a one-off link is made (Share without a link key):
			// the address only on Copy link and in its ⓘ.
			shown := f.srv.pageLinkURL(oneOff)
			once := pageSection(t, get("/pages/"+sid, func(w *httptest.ResponseRecorder, r *http.Request) {
				f.srv.renderPageDetail(w, r, id, 200, "", "", shown)
			}), "page-detail")
			assertPagePlain(t, "the page screen showing a new one-off link", once)
			if !strings.Contains(once, `data-copy-text="`+shown+`"`) || !strings.Contains(once, `data-info-code="">`+shown+`</code>`) {
				t.Errorf("the new one-off link is not on Copy link and in its ⓘ")
			}

			// With a link key: the page's reusable link.
			f.keyed(t)
			l, _, err := f.pages.ShareLink(ctx, id, "you", false)
			if err != nil {
				t.Fatal(err)
			}
			link := f.srv.pageLinkURL(l)
			keyed := pageSection(t, get("/pages/"+sid, func(w *httptest.ResponseRecorder, r *http.Request) {
				f.srv.renderPageDetail(w, r, id, 200, "", "", "")
			}), "page-detail")
			assertPagePlain(t, "the page screen with a reusable link", keyed)
			for _, want := range []string{
				`data-page-share="" data-page-id="` + sid + `" data-share-title="Pick a subject line" data-share-url="` + link + `"`,
				// Its row under Links copies it; the address is on the button
				// only, never drawn and never in an ⓘ.
				`data-copy-text="` + link + `" data-link-copy`,
			} {
				if !strings.Contains(keyed, want) {
					t.Errorf("the keyed page screen lacks %q", want)
				}
			}
			if strings.Contains(keyed, "Sharing isn't set up") {
				t.Errorf("a keyed server's page screen says sharing is not set up")
			}

			// A mirrored session's page screen: "Made by work on atlas", its pending
			// link "asked for by work on atlas".
			msid := strconv.FormatInt(mid, 10)
			mdetail := pageSection(t, get("/pages/"+msid, func(w *httptest.ResponseRecorder, r *http.Request) {
				f.srv.renderPageDetail(w, r, mid, 200, "", "", "")
			}), "page-detail")
			assertPagePlain(t, "a mirrored session's page screen", mdetail)
			if !strings.Contains(mdetail, "by work on atlas · v1") || !strings.Contains(mdetail, "asked for by work on atlas") {
				t.Errorf("a mirrored session's page screen does not name it in words:\n%s", mdetail)
			}

			// The Pages list.
			list := pageSection(t, get("/pages", func(w *httptest.ResponseRecorder, r *http.Request) {
				f.srv.renderPagesList(w, r, 200, "")
			}), "pages-page")
			assertPagePlain(t, "the Pages list", list)
			if !strings.Contains(list, ">4 replies</span>") {
				t.Errorf("the list row does not count its responses in words:\n%s", list)
			}
		})
	}
}

// TestAResponsesMarkupIsDrawnAsText: a stranger's answer, a page script's key
// and value, and the agent's part label that hold markup — a script tag, an
// attribute break-out into an <img onerror> — are drawn on the owner's page
// screen as text, escaped, never as elements.
func TestAResponsesMarkupIsDrawnAsText(t *testing.T) {
	f := newPageFixture(t)
	const evil = `<script>alert(1)</script>"><img src=x onerror=alert(2)>`
	id := f.create(t, f.agent, "Markup", `[
 {"type":"question","id":"q","label":"<b onmouseover=alert(3)>Ask</b>"},
 {"type":"html","html":"<p>x</p>"}
]`)
	l := f.liveLink(t, f.owner, id)
	for comp, data := range map[string]string{
		"q":      `{"answer":` + strconv.Quote(evil) + `}`,
		"custom": `{` + strconv.Quote("<i>"+evil) + `:` + strconv.Quote(evil) + `}`,
	} {
		if w := f.submit(t, l.Token, comp, data); w.Code != 200 {
			t.Fatalf("submit %s: %d %s", comp, w.Code, w.Body.String())
		}
	}
	for _, role := range []authz.Role{authz.RoleAdmin, authz.RoleMember} {
		w := httptest.NewRecorder()
		f.srv.renderPageDetail(w, httptest.NewRequest("GET", "/pages/"+strconv.FormatInt(id, 10), nil).WithContext(f.pageAs(role)), id, 200, "", "", "")
		detail := pageSection(t, w.Body.String(), "page-detail")
		for _, raw := range []string{"<script>alert(1)", "<img src=x", "<b onmouseover", "<i>"} {
			if strings.Contains(detail, raw) {
				t.Errorf("%s: the page screen draws %q as markup", role, raw)
			}
		}
		esc := `&lt;script&gt;alert(1)&lt;/script&gt;&#34;&gt;&lt;img src=x onerror=alert(2)&gt;`
		if n := strings.Count(detail, `data-response-value>`+esc+`</dd>`); n != 2 {
			t.Errorf("%s: %d escaped values, want the answer's and the script's", role, n)
		}
		if !strings.Contains(detail, `data-response-heading>&lt;b onmouseover=alert(3)&gt;Ask&lt;/b&gt;</span>`) ||
			!strings.Contains(detail, `<dt class="text-xs text-fg2">&lt;i&gt;`+esc+`</dt>`) {
			t.Errorf("%s: the heading or the script's key is not drawn escaped", role)
		}
	}
}

// TestAResponsesPushNamesThePartByItsLabel: the "got a response" push names
// the part a viewer answered by the page's own label — a form's, an upload's
// — and the page's own script as such; never the part's id, never what the
// viewer sent.
func TestAResponsesPushNamesThePartByItsLabel(t *testing.T) {
	f := newPageFixture(t)
	blocks := `[{"type":"form","id":"contact","label":"Your details","fields":[{"name":"email","type":"email"}]},
 {"type":"upload","id":"logo","label":"Your logo"},{"type":"html","html":"<p>x</p>"}]`
	want := map[string]string{}
	for comp, w := range map[string]string{
		"contact": "Through “Your details”. Open the page in muster to see it.",
		"logo":    "Through “Your logo”. Open the page in muster to see it.",
		"custom":  "From the page's own script. Open the page in muster to see it.",
	} {
		// One page per part: the push is at most one a minute per page.
		id := f.create(t, f.agent, "Push "+comp, blocks)
		l := f.liveLink(t, f.owner, id)
		var rec *httptest.ResponseRecorder
		switch comp {
		case "logo":
			rec = f.upload(t, l.Token, comp, "logo.txt", []byte("client-secret-words"))
		case "custom":
			rec = f.submit(t, l.Token, comp, `{"secret":"client-secret-words"}`)
		default:
			rec = f.submit(t, l.Token, comp, `{"fields":{"email":"client-secret-words@example.com"}}`)
		}
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", comp, rec.Code, rec.Body.String())
		}
		want["page-"+strconv.FormatInt(id, 10)+"-response"] = w
	}
	for tag, body := range want {
		sent, _ := f.pagePushesTagged(tag)
		if len(sent) != 1 || sent[0].Body != body || strings.Contains(sent[0].Body, "client-secret") {
			t.Errorf("%s: %+v, want one push saying %q", tag, sent, body)
		}
	}
}
