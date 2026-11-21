package api

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/fence"
	"github.com/ZacxDev/muster/internal/pages"
)

// THE OWNER'S OWN VIEW OF A PAGE (/pages/{id}/view; docs/pages.md "The
// owner's view"): what the signed-in owner sends from it is RECORDED, as
// theirs (by_owner, no link), through the same submit and upload code a
// link's viewer reaches; the word "preview" is gone from everything they see;
// its former address redirects; and only the owner's shell draws the version
// chip. Every name, title and address is invented.

// ownerSubmit posts {component, data} as the owner's shell does from their
// view of version v: their session, muster's own Origin, no token.
func (f *pageFixture) ownerSubmit(t *testing.T, id int64, v int, component, data string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"component":"` + component + `","data":` + data + `}`
	return f.call(t, http.MethodPost, "/pages/"+strconv.FormatInt(id, 10)+"/versions/"+strconv.Itoa(v)+"/submit", strings.NewReader(body),
		merge(hdr, map[string]string{"Content-Type": "application/json"}))
}

// ownerUpload posts one file as the owner's shell does: component, then file.
func (f *pageFixture) ownerUpload(t *testing.T, id int64, v int, component, name string, data []byte, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	_ = mw.WriteField("component", component)
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = fw.Write(data)
	_ = mw.Close()
	return f.call(t, http.MethodPost, "/pages/"+strconv.FormatInt(id, 10)+"/versions/"+strconv.Itoa(v)+"/upload", &b,
		merge(hdr, map[string]string{"Content-Type": mw.FormDataContentType()}))
}

// pageContentBlocks is a content document's page markup: the BLOCKS string
// its SDK puts in the page once it holds the shell's port, decoded.
func pageContentBlocks(t *testing.T, doc string) string {
	t.Helper()
	m := regexp.MustCompile(`BLOCKS=("(?:[^"\\]|\\.)*")`).FindStringSubmatch(doc)
	if m == nil {
		t.Fatalf("no BLOCKS in the content: %s", doc)
	}
	var out string
	if err := json.Unmarshal([]byte(m[1]), &out); err != nil {
		t.Fatalf("BLOCKS is not a JSON string: %v", err)
	}
	return out
}

// pageResponsesLines is page_responses' fenced lines, each decoded.
func pageResponsesLines(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	block, _ := out["responses"].(string)
	var rows []map[string]any
	for _, line := range strings.Split(block, "\n") {
		if line = strings.TrimPrefix(line, fence.ContentPrefix); !strings.HasPrefix(line, "{") {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("a fenced line is not JSON: %q", line)
		}
		rows = append(rows, m)
	}
	return rows
}

// TestTheOwnersSubmitAndUploadFromTheirViewAreRecordedAsTheirs: the owner's
// form submit and file upload from their own view are recorded, attributed to
// them — by_owner, no link — on the page's screen ("you") and in
// page_responses (by_owner: true, no link_id); they push nobody (the owner
// sent them). Before this change the view recorded nothing (its SDK answered
// {preview: true} and the shell relayed nothing), and the two routes did not
// exist.
func TestTheOwnersSubmitAndUploadFromTheirViewAreRecordedAsTheirs(t *testing.T) {
	f := newPageFixture(t)
	id := f.create(t, f.agent, "Owner answers", formBlocks)
	sid := strconv.FormatInt(id, 10)
	if w := f.ownerSubmit(t, id, 1, "f1", `{"fields":{"email":"me@example.com","seats":"3"}}`, f.ownerHdr()); w.Code != http.StatusOK {
		t.Fatalf("the owner's submit: %d %s", w.Code, w.Body.String())
	}
	if w := f.ownerUpload(t, id, 1, "u1", "mine.png", []byte("\x89PNG\r\n\x1a\nnot-really-a-png"), f.ownerHdr()); w.Code != http.StatusOK {
		t.Fatalf("the owner's upload: %d %s", w.Code, w.Body.String())
	}
	rs, err := f.pages.Responses(f.as(f.owner, authz.ViaSession), id, 0, 10)
	if err != nil || len(rs) != 2 {
		t.Fatalf("responses: %+v %v", rs, err)
	}
	for _, r := range rs {
		if !r.ByOwner || r.LinkID != 0 || r.Version != 1 {
			t.Errorf("not the owner's: %+v", r)
		}
	}
	if rs[0].Kind != pages.KindSubmit || !strings.Contains(string(rs[0].Data), `"me@example.com"`) || rs[1].Kind != pages.KindUpload || rs[1].UploadPublicID == "" {
		t.Fatalf("what was recorded: %+v", rs)
	}
	// The page's screen says they are the owner's.
	detail := f.get(t, "/pages/"+sid, f.ownerHdr()).Body.String()
	if n := strings.Count(detail, `data-response-by-owner>`); n != 2 || strings.Count(detail, `<span data-response-by="you"><time `) != 2 {
		t.Errorf("the page's screen marks %d response(s) as the owner's, want 2: %s", n, detail)
	}
	// page_responses says so, outside the data.
	rows := pageResponsesLines(t, f.tool(t, f.agent, pageResponsesTool, map[string]any{"page_id": id}))
	if len(rows) != 2 {
		t.Fatalf("page_responses: %v", rows)
	}
	for _, row := range rows {
		if row["by_owner"] != true {
			t.Errorf("page_responses: by_owner = %v, want true: %v", row["by_owner"], row)
		}
		if _, has := row["link_id"]; has {
			t.Errorf("page_responses names a link for the owner's own: %v", row)
		}
	}
	// No push: the owner sent them.
	if sent, _ := f.pagePushesTagged("page-" + sid + "-response"); len(sent) != 0 {
		t.Errorf("the owner's own response pushed them: %+v", sent)
	}
}

// TestALinksResponseIsStillTheLinks (regression): a viewer's response through
// a link is recorded as the link's — its link_id, not by_owner — beside the
// owner's own, on the page's screen and in page_responses, and still pushes
// the owner.
func TestALinksResponseIsStillTheLinks(t *testing.T) {
	f := newPageFixture(t)
	id := f.create(t, f.agent, "Both", formBlocks)
	sid := strconv.FormatInt(id, 10)
	l := f.liveLink(t, f.owner, id)
	if w := f.submit(t, l.Token, "q1", `{"answer":"a stranger's"}`); w.Code != http.StatusOK {
		t.Fatalf("the link's submit: %d %s", w.Code, w.Body.String())
	}
	if w := f.ownerSubmit(t, id, 1, "q1", `{"answer":"mine"}`, f.ownerHdr()); w.Code != http.StatusOK {
		t.Fatalf("the owner's submit: %d %s", w.Code, w.Body.String())
	}
	rs, _ := f.pages.Responses(f.as(f.owner, authz.ViaSession), id, 0, 10)
	if len(rs) != 2 || rs[0].ByOwner || rs[0].LinkID != l.ID || !rs[1].ByOwner || rs[1].LinkID != 0 {
		t.Fatalf("responses: %+v", rs)
	}
	detail := f.get(t, "/pages/"+sid, f.ownerHdr()).Body.String()
	// The viewer's is "Sent …", the owner's "Sent by you …"; which link a
	// viewer's came through is the admin's raw ⓘ, never the top layer.
	if strings.Contains(detail, ` · link `+strconv.FormatInt(l.ID, 10)+` · `) || strings.Count(detail, `data-response-by-owner>`) != 1 ||
		strings.Count(detail, `data-plain-when-prefix="Sent "`) != 1 || strings.Count(detail, `data-plain-when-prefix="Sent by you "`) != 1 {
		t.Errorf("the page's screen: %s", detail)
	}
	// An admin's "Show raw" ⓘ still names the link the viewer's came through,
	// and says the owner sent theirs.
	aw := httptest.NewRecorder()
	f.srv.renderPageDetail(aw, httptest.NewRequest("GET", "/pages/"+sid, nil).WithContext(f.pageAs(authz.RoleAdmin)), id, 200, "", "", "")
	raw := regexp.MustCompile(`data-info-code="">part q1 · version 1 · ([^·]+) · `).FindAllStringSubmatch(aw.Body.String(), -1)
	if len(raw) != 2 || raw[0][1] != "sent by the owner" || raw[1][1] != "link "+strconv.FormatInt(l.ID, 10) {
		t.Errorf("an admin's raw ⓘ: %v, want the owner's then link %d", raw, l.ID)
	}
	rows := pageResponsesLines(t, f.tool(t, f.agent, pageResponsesTool, map[string]any{"page_id": id}))
	if len(rows) != 2 || rows[0]["by_owner"] != false || pnum(rows[0]["link_id"]) != l.ID || rows[1]["by_owner"] != true {
		t.Fatalf("page_responses: %v", rows)
	}
	if sent, _ := f.pagePushesTagged("page-" + sid + "-response"); len(sent) != 1 {
		t.Errorf("the link's response pushed the owner %d time(s), want 1", len(sent))
	}
}

// TestAnotherOwnerCannotSubmitToOrReadThePage: another member's session can
// neither read the owner's page through its view nor record a response or a
// file on it — every answer is the one 404, and nothing is stored; the
// owner's own submit with a foreign Origin, or with no session (a link's
// token is no credential on these routes), records nothing either.
func TestAnotherOwnerCannotSubmitToOrReadThePage(t *testing.T) {
	f := newPageFixture(t)
	id := f.create(t, f.agent, "Not yours", `[{"type":"question","id":"q1","label":"canary-question"},{"type":"upload","id":"u1","label":"f"}]`)
	sid := strconv.FormatInt(id, 10)
	l := f.liveLink(t, f.owner, id)
	objects := len(f.objects.Keys())
	f.session = f.other
	for _, w := range []*httptest.ResponseRecorder{
		f.get(t, "/pages/"+sid+"/view", f.ownerHdr()),
		f.get(t, "/pages/"+sid+"/view?v=1", f.ownerHdr()),
		f.ownerSubmit(t, id, 1, "q1", `{"answer":"hijack"}`, f.ownerHdr()),
		f.ownerUpload(t, id, 1, "u1", "x.txt", []byte("hijack"), f.ownerHdr()),
	} {
		if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "canary-question") {
			t.Errorf("another member: %d %s", w.Code, w.Body.String())
		}
	}
	f.session = f.owner
	// The owner's session, but a cross-site Origin: refused.
	if w := f.ownerSubmit(t, id, 1, "q1", `{"answer":"x"}`, map[string]string{"Cookie": sessionCookieFor(f.srv), "Origin": "null"}); w.Code != http.StatusForbidden {
		t.Errorf("a cross-origin owner submit: %d", w.Code)
	}
	// No session, the link's token in hand: these routes read no token.
	if w := f.ownerSubmit(t, id, 1, "q1", `{"answer":"x"}`, map[string]string{"Origin": "null", "Authorization": "Bearer " + l.Token}); w.Code == http.StatusOK {
		t.Errorf("a submit with the link's token and no session: %d", w.Code)
	}
	if rs, _ := f.pages.Responses(f.as(f.owner, authz.ViaSession), id, 0, 10); len(rs) != 0 {
		t.Fatalf("recorded: %+v", rs)
	}
	if n := len(f.objects.Keys()); n != objects {
		t.Fatalf("another member's upload stored %d object(s)", n-objects)
	}
	// The control: the owner's own submit on the same route is recorded.
	if w := f.ownerSubmit(t, id, 1, "q1", `{"answer":"mine"}`, f.ownerHdr()); w.Code != http.StatusOK {
		t.Fatalf("control: the owner's own submit: %d %s", w.Code, w.Body.String())
	}
}

// TestTheOwnersResponsesHaveALinksLimits: the owner's own submits are
// rate-limited exactly as one link's are (pageSubmitLimit per window), and
// a submit's data is checked against the version's component as a link's is.
func TestTheOwnersResponsesHaveALinksLimits(t *testing.T) {
	f := newPageFixture(t)
	defer func(s int) { pageSubmitLimit = s }(pageSubmitLimit)
	pageSubmitLimit = 2
	id := f.create(t, f.agent, "Limits", formBlocks)
	for i := 0; i < 2; i++ {
		if w := f.ownerSubmit(t, id, 1, "q1", `{"answer":"a"}`, f.ownerHdr()); w.Code != http.StatusOK {
			t.Fatalf("submit %d: %d", i, w.Code)
		}
	}
	if w := f.ownerSubmit(t, id, 1, "q1", `{"answer":"a"}`, f.ownerHdr()); w.Code != http.StatusTooManyRequests {
		t.Fatalf("a third owner submit within the minute: %d", w.Code)
	}
	// A link of the same page has its own window, untouched by the owner's.
	l := f.liveLink(t, f.owner, id)
	if w := f.submit(t, l.Token, "q1", `{"answer":"a"}`); w.Code != http.StatusOK {
		t.Fatalf("the link's first submit: %d", w.Code)
	}
}

// TestAnOldVersionsSubmitAnswersThatVersion: the owner viewing ?v=1 of a page
// whose current version is 2 sends against version 1 — checked against ITS
// components and recorded with its number (the old-version rule: record, as
// a link's response records the version it answered). A component that is
// only in version 2 is refused at version 1, and the other way round.
func TestAnOldVersionsSubmitAnswersThatVersion(t *testing.T) {
	f := newPageFixture(t)
	id := f.create(t, f.agent, "Two versions", `[{"type":"question","id":"old","label":"v1 only"}]`)
	f.tool(t, f.agent, pageUpdateTool, map[string]any{"page_id": id, "blocks": json.RawMessage(`[{"type":"question","id":"new","label":"v2 only"}]`)})
	if w := f.ownerSubmit(t, id, 1, "old", `{"answer":"to v1"}`, f.ownerHdr()); w.Code != http.StatusOK {
		t.Fatalf("a submit at v1: %d %s", w.Code, w.Body.String())
	}
	if w := f.ownerSubmit(t, id, 1, "new", `{"answer":"x"}`, f.ownerHdr()); w.Code != http.StatusBadRequest {
		t.Errorf("v2's component at v1: %d", w.Code)
	}
	if w := f.ownerSubmit(t, id, 2, "old", `{"answer":"x"}`, f.ownerHdr()); w.Code != http.StatusBadRequest {
		t.Errorf("v1's component at v2: %d", w.Code)
	}
	if w := f.ownerSubmit(t, id, 3, "new", `{"answer":"x"}`, f.ownerHdr()); w.Code != http.StatusNotFound {
		t.Errorf("a version that does not exist: %d", w.Code)
	}
	rs, _ := f.pages.Responses(f.as(f.owner, authz.ViaSession), id, 0, 10)
	if len(rs) != 1 || rs[0].Version != 1 || rs[0].Component != "old" || !rs[0].ByOwner {
		t.Fatalf("responses: %+v", rs)
	}
}

// TestThePreviewAddressRedirectsToTheView: /pages/{id}/preview, which older
// notifications, cards and Asks name, answers 302 to /pages/{id}/view, its ?v
// kept.
func TestThePreviewAddressRedirectsToTheView(t *testing.T) {
	f := newPageFixture(t)
	id := f.create(t, f.agent, "Moved", `[{"type":"markdown","text":"x"}]`)
	sid := strconv.FormatInt(id, 10)
	for path, want := range map[string]string{
		"/pages/" + sid + "/preview":     "/pages/" + sid + "/view",
		"/pages/" + sid + "/preview?v=1": "/pages/" + sid + "/view?v=1",
	} {
		w := f.get(t, path, f.ownerHdr())
		if w.Code != http.StatusFound || w.Header().Get("Location") != want {
			t.Errorf("%s: %d → %q, want 302 → %q", path, w.Code, w.Header().Get("Location"), want)
		}
	}
	if w := f.get(t, "/pages/"+sid+"/view", f.ownerHdr()); w.Code != http.StatusOK {
		t.Fatalf("the view itself: %d", w.Code)
	}
}

// previewHits is every place s says "preview", case-insensitively.
func previewHits(s string) []string {
	return regexp.MustCompile(`(?i).{0,30}preview.{0,30}`).FindAllString(s, -1)
}

// pageDetailSection is the page's own section of /pages/{id}: what the page
// screen draws, without the app's nav and script around it (whose session
// previews are another feature).
func pageDetailSection(t *testing.T, doc string) string {
	t.Helper()
	i := strings.Index(doc, `data-testid="page-detail"`)
	j := strings.Index(doc, `data-page-delete-box>`)
	if i < 0 || j < i {
		t.Fatalf("no page-detail section in %s", doc)
	}
	return doc[i:j]
}

// TestNothingTheOwnerSeesSaysPreview: the owner's view (its shell, at the
// current and an old version), the content it frames, the page's screen,
// the chat's page card and page_create's next step never say "preview". The
// positive control: the old banner, put through the same check, is caught.
func TestNothingTheOwnerSeesSaysPreview(t *testing.T) {
	if hits := previewHits("Preview of version 1. Only you can see this; nothing sent from it is recorded."); len(hits) != 1 {
		t.Fatalf("control: the check does not catch the old banner: %v", hits)
	}
	if hits := previewHits(`<p class="mp-preview" data-muster-preview>…</p>`); len(hits) == 0 {
		t.Fatal("control: the check does not catch the old content line")
	}
	f := newPageFixture(t)
	created := f.tool(t, f.agent, pageCreateTool, map[string]any{"title": "Wording", "blocks": json.RawMessage(formBlocks)})
	id := pnum(created["page_id"])
	sid := strconv.FormatInt(id, 10)
	// One name for the owner's view (round 0's D2): preview_url, no url.
	if created["preview_url"] != pagesPublicURL+"/pages/"+sid+"/view" || created["url"] != nil {
		t.Errorf("page_create's view address: preview_url %v, url %v", created["preview_url"], created["url"])
	}
	f.tool(t, f.agent, pageUpdateTool, map[string]any{"page_id": id, "blocks": json.RawMessage(formBlocks)})
	for name, body := range map[string]string{
		"the view":           f.get(t, "/pages/"+sid+"/view", f.ownerHdr()).Body.String(),
		"the view at v1":     f.get(t, "/pages/"+sid+"/view?v=1", f.ownerHdr()).Body.String(),
		"the content":        f.get(t, "/pages/"+sid+"/versions/1/content", f.ownerHdr()).Body.String(),
		"the page's screen":  pageDetailSection(t, f.get(t, "/pages/"+sid, f.ownerHdr()).Body.String()),
		"the page card":      f.get(t, "/pages/"+sid+"/card?tool=page_create", f.ownerHdr()).Body.String(),
		"page_create's next": created["next"].(string),
	} {
		if len(body) < 40 {
			t.Errorf("%s: drew nothing: %q", name, body)
		}
		if hits := previewHits(body); len(hits) != 0 {
			t.Errorf("%s says preview: %q", name, hits)
		}
	}
}

// TestTheOwnersHeaderHasCrumbsTheTitlePopoverAndShare: the owner's view of
// their own page draws "Pages ›" (a link to /pages) before the title, the
// title as its popover's button — the whole title, "Page #<id> · v<n> ·
// updated …" and Copy ID — and Share at the header's end; a link's viewer
// gets the brand and the title, none of them.
func TestTheOwnersHeaderHasCrumbsTheTitlePopoverAndShare(t *testing.T) {
	f := newPageFixture(t)
	const title = "Spring launch checklist for the whole crew, every step"
	id := f.create(t, f.agent, title, `[{"type":"markdown","text":"one"}]`)
	sid := strconv.FormatInt(id, 10)
	view := f.get(t, "/pages/"+sid+"/view", f.ownerHdr()).Body.String()
	for _, want := range []string{
		`<nav class="crumbs" aria-label="Breadcrumb" data-page-crumbs><a class="cr" href="/pages" data-page-crumb>Pages</a><span class="sep" aria-hidden="true">›</span></nav>` +
			`<button type="button" class="t" data-page-title title="` + title + `" aria-haspopup="dialog" aria-expanded="false" aria-controls="page-about">` + title + `</button>`,
		`<p class="ft" data-page-about-title>` + title + `</p><p class="meta" data-page-about-meta>Page #` + sid + ` · v1 · updated <time data-plain-when="`,
		`<button type="button" data-copy-page-id="` + sid + `">Copy ID</button>`,
		`<form method="POST" action="/pages/` + sid + `/share" class="contents"><button type="submit" data-page-share="" data-page-id="` + sid + `"`,
	} {
		if !strings.Contains(view, want) {
			t.Errorf("the owner's view lacks %q", want)
		}
	}
	if !regexp.MustCompile(`data-page-share="" data-page-id="` + sid + `"[^>]*>.*?</button></form></header>`).MatchString(view) {
		t.Error("Share does not end the owner's header")
	}
	l := f.liveLink(t, f.owner, id)
	shell := f.get(t, pages.LinkPath(l.Token), nil).Body.String()
	markup := shell[:strings.Index(shell, "<script nonce=")]
	for _, banned := range []string{"data-page-crumb", "Breadcrumb", "data-page-about", "data-copy-page-id", "data-page-share", `href="/pages`} {
		if strings.Contains(markup, banned) {
			t.Errorf("a link's shell draws the owner's %s", banned)
		}
	}
}

// TestTheChipIsTheOwnersAndListsTheVersions: the owner's view draws the
// version chip — v<shown> — and a popover listing every version, newest
// first, marking the page's current one and the one shown, each opening
// ?v=N; a link's viewer gets the same header without any of it.
func TestTheChipIsTheOwnersAndListsTheVersions(t *testing.T) {
	f := newPageFixture(t)
	id := f.create(t, f.agent, "Chip", `[{"type":"markdown","text":"one"}]`)
	sid := strconv.FormatInt(id, 10)
	f.tool(t, f.agent, pageUpdateTool, map[string]any{"page_id": id, "blocks": json.RawMessage(`[{"type":"markdown","text":"two"}]`)})
	vs, _ := f.pages.Versions(f.as(f.owner, authz.ViaSession), id)
	if len(vs) != 2 {
		t.Fatalf("versions: %+v", vs)
	}
	// Each version's time is in words, as on the Pages screens: the day,
	// which the shell's script redraws in the reader's zone with its time
	// (data-plain-when). RED before: "current · 2026-11-17 23:37 UTC".
	made := map[int]string{
		vs[0].Version: `<time data-plain-when="` + strconv.FormatInt(vs[0].CreatedAt.UnixMilli(), 10) + `" data-plain-when-prefix="">today</time>`,
		vs[1].Version: `<time data-plain-when="` + strconv.FormatInt(vs[1].CreatedAt.UnixMilli(), 10) + `" data-plain-when-prefix="">today</time>`,
	}

	cur := f.get(t, "/pages/"+sid+"/view", f.ownerHdr()).Body.String()
	if strings.Contains(cur, " UTC<") {
		t.Errorf("the version popover spells a UTC clock")
	}
	for _, want := range []string{
		`aria-label="Version 2: show versions">v2 <span aria-hidden="true">▾</span></button>`,
		`<li><a href="/pages/` + sid + `/view?v=2" data-version-link="2" aria-current="page">Version 2<span class="w">current · ` + made[2] + `</span></a></li>` +
			`<li><a href="/pages/` + sid + `/view?v=1" data-version-link="1">Version 1<span class="w">` + made[1] + `</span></a></li></ul>`,
		`<a href="/pages/` + sid + `" data-page-details>Page details</a>`,
		`action="/pages/` + sid + `/share"`, `data-page-share=""`,
		`src="/pages/` + sid + `/versions/2/content"`, `base="/pages/` + sid + `/versions/2"`,
	} {
		if !strings.Contains(cur, want) {
			t.Errorf("the owner's view lacks %q", want)
		}
	}
	old := f.get(t, "/pages/"+sid+"/view?v=1", f.ownerHdr()).Body.String()
	for _, want := range []string{
		`aria-label="Version 1: show versions">v1 <span`,
		`<li><a href="/pages/` + sid + `/view?v=2" data-version-link="2">Version 2<span class="w">current · `,
		`<li><a href="/pages/` + sid + `/view?v=1" data-version-link="1" aria-current="page">Version 1<span`,
		`src="/pages/` + sid + `/versions/1/content"`, `base="/pages/` + sid + `/versions/1"`,
	} {
		if !strings.Contains(old, want) {
			t.Errorf("the owner's view of v1 lacks %q", want)
		}
	}
	l := f.liveLink(t, f.owner, id)
	shell := f.get(t, pages.LinkPath(l.Token), nil).Body.String()
	markup := shell[:strings.Index(shell, "<script nonce=")]
	if !strings.Contains(markup, `data-page-title title="Chip">Chip</span></header>`) {
		t.Errorf("a link's shell lacks the slim header: %s", markup)
	}
	for _, banned := range []string{"data-version-chip", "data-versions", "data-page-share", "data-page-details"} {
		if strings.Contains(markup, banned) {
			t.Errorf("a link's shell draws the owner's %s", banned)
		}
	}
}
