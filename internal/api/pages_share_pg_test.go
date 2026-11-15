package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/push"
)

// INSTANT SHARE (docs/pages.md#sharing): MUSTER_PAGES_AGENT_SHARE=auto, the
// owner's one-tap Share and its reusable link, the publish push, the chat's
// page card route. Every name, title and address is invented.

// keyed gives the fixture's store a link key (reusable links) and returns f.
func (f *pageFixture) keyed(t *testing.T) *pageFixture {
	t.Helper()
	if _, err := f.pages.WithLinkKey(bytes.Repeat([]byte{9}, pages.LinkKeySize)); err != nil {
		t.Fatal(err)
	}
	return f
}

// asksOf counts owner's attention asks, read as the system.
func (f *pageFixture) asksOf(t *testing.T, owner int64) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(authz.System(f.ctx), `SELECT count(*) FROM attention_asks WHERE owner_id = $1`, owner).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// tokenOf is a share url's token.
func tokenOf(t *testing.T, u any) string {
	t.Helper()
	s, _ := u.(string)
	tok := strings.TrimPrefix(s, pagesPublicURL+pages.SharePrefix)
	if !pages.TokenShapeOK(tok) {
		t.Fatalf("not a share url: %v", u)
	}
	return tok
}

// pagePushesTagged is the pushes sent with tag, and to whom.
func (f *pageFixture) pagePushesTagged(tag string) ([]push.Payload, []int64) {
	f.srv.WaitForPushes()
	all, to := f.pushes.snapshot()
	var out []push.Payload
	var who []int64
	for i, p := range all {
		if p.Tag == tag {
			out, who = append(out, p), append(who, to[i])
		}
	}
	return out, who
}

// TestAutoModeSharesAtOnceWithNoCard: with MUSTER_PAGES_AGENT_SHARE=auto an
// agent's page_share_request makes the page's link live NOW — no attention
// row is filed — and returns its url, which a stranger opens; a second
// request returns the SAME url; the owner (only) is pushed once; the page's
// screen says the agent made it live without asking, with Revoke beside it.
func TestAutoModeSharesAtOnceWithNoCard(t *testing.T) {
	f := newPageFixture(t).keyed(t)
	f.srv.ext.PagesAgentShareAuto = true
	id := f.create(t, f.agent, "Auto shared", `[{"type":"markdown","text":"hello"}]`)
	before := f.asksOf(t, f.owner)

	req := f.tool(t, f.agent, pageShareRequestTool, map[string]any{"page_id": id, "reason": "for the client"})
	if req["status"] != "live" || req["ask_id"] != nil || req["reusable"] != true || req["shared_by"] != f.agent.Name {
		t.Fatalf("page_share_request in auto mode: %v", req)
	}
	tok := tokenOf(t, req["url"])
	if n := f.asksOf(t, f.owner); n != before {
		t.Fatalf("auto mode filed %d attention ask(s)", n-before)
	}
	if w := f.get(t, pages.LinkPath(tok), nil); w.Code != http.StatusOK {
		t.Fatalf("the auto-shared link does not serve a stranger: %d", w.Code)
	}
	again := f.tool(t, f.agent, pageShareRequestTool, map[string]any{"page_id": id})
	if again["url"] != req["url"] || again["status"] != "live" {
		t.Fatalf("a second request: %v, want the same url %v", again, req["url"])
	}
	ls, _ := f.pages.Links(f.as(f.owner, authz.ViaSession), id)
	if len(ls) != 1 || !ls[0].ByAgent || !ls[0].Reusable || ls[0].AskID != 0 || ls[0].RequestedBy != f.agent.Name {
		t.Fatalf("the link list: %+v", ls)
	}
	sent, to := f.pagePushesTagged("page-" + strconv.FormatInt(id, 10) + "-shared")
	if len(sent) != 1 || to[0] != f.owner || !strings.HasPrefix(sent[0].Title, "Page shared: Auto shared") ||
		sent[0].Data["url"] != "/pages/"+strconv.FormatInt(id, 10) || strings.Contains(sent[0].Title+sent[0].Body, tok) {
		t.Fatalf("the shared push: %+v to %v", sent, to)
	}
	detail := f.get(t, "/pages/"+strconv.FormatInt(id, 10), f.ownerHdr()).Body.String()
	if !strings.Contains(detail, "made live by "+f.agent.Name+" without asking you") || !strings.Contains(detail, "data-link-revoke") {
		t.Fatalf("the page's screen does not say the agent shared it: %s", detail)
	}
}

// TestAutoModeWithoutAKeyMakesAHashOnlyLiveLink: no link key, auto mode: the
// link is still live at once (hash-only, shown in this result only).
func TestAutoModeWithoutAKeyMakesAHashOnlyLiveLink(t *testing.T) {
	f := newPageFixture(t)
	f.srv.ext.PagesAgentShareAuto = true
	id := f.create(t, f.agent, "No key", `[{"type":"markdown","text":"x"}]`)
	req := f.tool(t, f.agent, pageShareRequestTool, map[string]any{"page_id": id})
	if req["status"] != "live" || req["reusable"] != false {
		t.Fatalf("auto mode without a key: %v", req)
	}
	if w := f.get(t, pages.LinkPath(tokenOf(t, req["url"])), nil); w.Code != http.StatusOK {
		t.Fatalf("the link does not serve: %d", w.Code)
	}
}

// TestAnAgentCannotAutoShareAnotherOwnersPage: in auto mode, another owner's
// agent — and another agent of the same owner — get "no such page", and no
// link is made on the page. 🔴 THE GUARD THIS PINS is pageShareAuto's PageFor
// under the agent's owner's principal (the mutation check in the PR: read the
// page as the system instead, and this goes red).
func TestAnAgentCannotAutoShareAnotherOwnersPage(t *testing.T) {
	f := newPageFixture(t).keyed(t)
	f.srv.ext.PagesAgentShareAuto = true
	id := f.create(t, f.agent, "Mine", `[{"type":"markdown","text":"x"}]`)
	sid := strconv.FormatInt(id, 10)
	for _, a := range []struct {
		who  string
		call func() map[string]any
	}{
		{"another owner's agent", func() map[string]any {
			return f.tool(t, f.otherAgent, pageShareRequestTool, map[string]any{"page_id": id})
		}},
		{"another agent of the same owner", func() map[string]any {
			return f.tool(t, f.agent2, pageShareRequestTool, map[string]any{"page_id": id})
		}},
	} {
		if got := a.call(); got["error"] != "no such page: "+sid || got["url"] != nil {
			t.Errorf("%s: %v", a.who, got)
		}
	}
	var n int
	if err := f.pool.QueryRow(authz.System(f.ctx), `SELECT count(*) FROM page_links WHERE page_id = $1`, id).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the refused shares made %d link(s) (%v)", n, err)
	}
	// The control: the page's own agent can.
	if got := f.tool(t, f.agent, pageShareRequestTool, map[string]any{"page_id": id}); got["status"] != "live" {
		t.Fatalf("the page's own agent: %v", got)
	}
}

// TestAskModeIsTodaysFlow: the default (ask) files a pending link and an owner
// card, and returns status pending — what TestOnlyTheOwnersOwnDoorApprovesALink
// then decides.
func TestAskModeIsTodaysFlow(t *testing.T) {
	f := newPageFixture(t).keyed(t)
	if f.srv.ext.PagesAgentShareAuto {
		t.Fatal("the fixture is not in ask mode")
	}
	id := f.create(t, f.agent, "Ask first", `[{"type":"markdown","text":"x"}]`)
	before := f.asksOf(t, f.owner)
	req := f.tool(t, f.agent, pageShareRequestTool, map[string]any{"page_id": id})
	if req["status"] != "pending" || pnum(req["ask_id"]) <= 0 || f.asksOf(t, f.owner) != before+1 {
		t.Fatalf("ask mode: %v", req)
	}
	if w := f.get(t, pages.LinkPath(tokenOf(t, req["url"])), nil); w.Code != http.StatusNotFound {
		t.Fatalf("a pending link served in ask mode: %d", w.Code)
	}
}

// share is the owner's Share tap as the page script makes it.
func (f *pageFixture) share(t *testing.T, path string) (int, map[string]any) {
	t.Helper()
	w := f.call(t, http.MethodPost, path, nil, merge(f.ownerHdr(), map[string]string{"Accept": "application/json"}))
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s answered %d %q", path, w.Code, w.Body.String())
	}
	return w.Code, out
}

// TestTheOwnersShareButtonGivesOneReusableLink: Share makes the page's link
// and a second Share answers the same url; the page's screen shows it; New
// link rotates it (the old url 404s, the new serves); Revoke link takes it
// away; a write without muster's Origin is refused; a legacy hash-only link
// beside it keeps serving.
func TestTheOwnersShareButtonGivesOneReusableLink(t *testing.T) {
	f := newPageFixture(t).keyed(t)
	id := f.create(t, f.agent, "Share me", `[{"type":"markdown","text":"x"}]`)
	base := "/pages/" + strconv.FormatInt(id, 10)
	legacy := f.liveLink(t, f.owner, id)

	code, first := f.share(t, base+"/share")
	if code != http.StatusOK || first["reusable"] != true || first["created"] != true {
		t.Fatalf("Share: %d %v", code, first)
	}
	tok := tokenOf(t, first["url"])
	_, second := f.share(t, base+"/share")
	if second["url"] != first["url"] || second["created"] != false {
		t.Fatalf("a second Share: %v, want %v again", second, first["url"])
	}
	if w := f.get(t, pages.LinkPath(tok), nil); w.Code != http.StatusOK {
		t.Fatalf("the shared link does not serve: %d", w.Code)
	}
	if d := f.get(t, base, f.ownerHdr()).Body.String(); !strings.Contains(d, `value="`+first["url"].(string)+`"`) || !strings.Contains(d, "data-page-share") {
		t.Fatalf("the page's screen does not show its link: %s", d)
	}
	if l := f.get(t, "/pages", f.ownerHdr()).Body.String(); !strings.Contains(l, `data-share-url="`+first["url"].(string)+`"`) {
		t.Fatalf("the list's Share button does not carry the link")
	}
	// Without muster's Origin: refused.
	if w := f.call(t, http.MethodPost, base+"/share", nil, map[string]string{"Cookie": sessionCookieFor(f.srv), "Origin": "null", "Accept": "application/json"}); w.Code != http.StatusForbidden {
		t.Fatalf("a cross-origin Share: %d", w.Code)
	}

	// New link: the old url 404s, the new one serves and is what Share answers.
	_, rot := f.share(t, base+"/share/rotate")
	if rot["url"] == first["url"] || pnum(rot["revoked"]) != 1 {
		t.Fatalf("New link: %v", rot)
	}
	newTok := tokenOf(t, rot["url"])
	if w := f.get(t, pages.LinkPath(tok), nil); w.Code != http.StatusNotFound {
		t.Fatalf("the rotated-away url still serves: %d", w.Code)
	}
	if w := f.get(t, pages.LinkPath(newTok), nil); w.Code != http.StatusOK {
		t.Fatalf("the new url does not serve: %d", w.Code)
	}
	if _, again := f.share(t, base+"/share"); again["url"] != rot["url"] {
		t.Fatalf("Share after New link: %v, want %v", again, rot["url"])
	}

	// Revoke link.
	if _, rv := f.share(t, base+"/share/revoke"); rv["revoked"] != true {
		t.Fatalf("Revoke link: %v", rv)
	}
	if w := f.get(t, pages.LinkPath(newTok), nil); w.Code != http.StatusNotFound {
		t.Fatalf("a revoked url still serves: %d", w.Code)
	}
	if _, rv := f.share(t, base+"/share/revoke"); rv["revoked"] != false {
		t.Fatalf("a second Revoke link: %v", rv)
	}

	// The legacy link, beside all of it, still serves.
	if w := f.get(t, pages.LinkPath(legacy.Token), nil); w.Code != http.StatusOK {
		t.Fatalf("the legacy link stopped serving: %d", w.Code)
	}
	// Another member's Share of this page: no such page.
	f.session = f.other
	if code, out := f.share(t, base+"/share"); code != http.StatusNotFound || out["url"] != nil {
		t.Fatalf("another member's Share: %d %v", code, out)
	}
}

// TestShareWithoutAKeyMakesANewLinkEachTime: no link key — each Share is a new
// hash-only live link (it serves), never the same address twice.
func TestShareWithoutAKeyMakesANewLinkEachTime(t *testing.T) {
	f := newPageFixture(t)
	id := f.create(t, f.agent, "No key share", `[{"type":"markdown","text":"x"}]`)
	base := "/pages/" + strconv.FormatInt(id, 10)
	_, a := f.share(t, base+"/share")
	_, b := f.share(t, base+"/share")
	if a["reusable"] != false || a["url"] == b["url"] {
		t.Fatalf("Share without a key: %v then %v", a, b)
	}
	if w := f.get(t, pages.LinkPath(tokenOf(t, a["url"])), nil); w.Code != http.StatusOK {
		t.Fatalf("the link does not serve: %d", w.Code)
	}
}

// TestPublishPushesTheOwnerOnce: page_create pushes its OWNER (only) "Page
// ready: <title>", opening the preview; page_update pushes nothing more.
func TestPublishPushesTheOwnerOnce(t *testing.T) {
	f := newPageFixture(t)
	id := f.create(t, f.agent, "Fresh page", `[{"type":"markdown","text":"v1"}]`)
	f.tool(t, f.agent, pageUpdateTool, map[string]any{"page_id": id, "blocks": json.RawMessage(`[{"type":"markdown","text":"v2"}]`)})
	sent, to := f.pagePushesTagged("page-" + strconv.FormatInt(id, 10) + "-ready")
	if len(sent) != 1 || to[0] != f.owner || sent[0].Title != "Page ready: Fresh page" ||
		sent[0].Data["url"] != "/pages/"+strconv.FormatInt(id, 10)+"/preview" {
		t.Fatalf("the publish push: %+v to %v", sent, to)
	}
	_, all := f.pushes.snapshot()
	for _, who := range all {
		if who != f.owner {
			t.Fatalf("a push went to user %d", who)
		}
	}
}

// TestThePageCardRoute: GET /pages/{id}/card draws the owner's page card; for
// another member it is 404.
func TestThePageCardRoute(t *testing.T) {
	f := newPageFixture(t)
	id := f.create(t, f.agent, "Carded", `[{"type":"markdown","text":"x"}]`)
	path := "/pages/" + strconv.FormatInt(id, 10) + "/card?tool=page_create"
	w := f.get(t, path, f.ownerHdr())
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `data-page-card=""`) || !strings.Contains(w.Body.String(), "Carded") ||
		!strings.Contains(w.Body.String(), `href="/pages/`+strconv.FormatInt(id, 10)+`/preview"`) {
		t.Fatalf("the card: %d %s", w.Code, w.Body.String())
	}
	f.session = f.other
	if w := f.get(t, path, f.ownerHdr()); w.Code != http.StatusNotFound {
		t.Fatalf("another member's card: %d", w.Code)
	}
}

// TestTheNavHasPages: a page's nav carries the sidebar's Pages entry while
// pages are served, marked current on the Pages screens.
func TestTheNavHasPages(t *testing.T) {
	f := newPageFixture(t)
	body := f.get(t, "/pages", f.ownerHdr()).Body.String()
	if !strings.Contains(body, `id="sidebar-pages" href="/pages"`) || !strings.Contains(body, `href="/pages" hx-boost="false" aria-current="page"`) {
		t.Fatalf("the Pages screen's nav has no current Pages entry")
	}
}
