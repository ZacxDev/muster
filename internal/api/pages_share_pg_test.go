package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/users"
)

// INSTANT SHARE (docs/pages.md#sharing): an owner's opt-in to their agents sharing without asking, the
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

// optIn flips uid's own "Let my agents share pages without asking" through
// the Account route, signed in as uid, and puts the session back.
func (f *pageFixture) optIn(t *testing.T, uid int64, on bool) {
	t.Helper()
	prev := f.session
	f.session = uid
	defer func() { f.session = prev }()
	v := "0"
	if on {
		v = "1"
	}
	body, hdr := form(url.Values{"on": {v}})
	w := f.call(t, http.MethodPost, accountPagesAgentSharePath, body, merge(f.ownerHdr(), hdr))
	if w.Code != http.StatusSeeOther || !strings.HasSuffix(w.Header().Get("Location"), "#"+pagesAgentShareID) {
		t.Fatalf("the opt-in as user %d: %d %v %s", uid, w.Code, w.Header(), w.Body.String())
	}
}

// optedIn reads uid's opt-in as uid.
func (f *pageFixture) optedIn(uid int64) bool {
	return f.srv.pageAgentShareOn(f.as(uid, authz.ViaSession), uid)
}

// TestAnOptedInOwnersAgentSharesAtOnce: for an owner who turned on "Let my
// agents share pages without asking", their agent's page_share_request makes
// the page's link live NOW — no attention row is filed — and returns its url,
// which a stranger opens; a second request returns the SAME url; the owner
// (only) is pushed for EACH share, on the page_shared toggle; the page's screen says
// the agent made it live without asking, with Revoke beside it.
func TestAnOptedInOwnersAgentSharesAtOnce(t *testing.T) {
	f := newPageFixture(t).keyed(t)
	f.optIn(t, f.owner, true)
	id := f.create(t, f.agent, "Auto shared", `[{"type":"markdown","text":"hello"}]`)
	before := f.asksOf(t, f.owner)

	req := f.tool(t, f.agent, pageShareRequestTool, map[string]any{"page_id": id, "reason": "for the client"})
	if req["status"] != "live" || req["ask_id"] != nil || req["reusable"] != true || req["shared_by"] != f.agent.Name {
		t.Fatalf("page_share_request for an opted-in owner: %v", req)
	}
	tok := tokenOf(t, req["url"])
	if n := f.asksOf(t, f.owner); n != before {
		t.Fatalf("an opted-in owner's agent filed %d attention ask(s)", n-before)
	}
	if w := f.get(t, pages.LinkPath(tok), nil); w.Code != http.StatusOK {
		t.Fatalf("the shared link does not serve a stranger: %d", w.Code)
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
	// One push per share — the second hands on the page's existing link, and
	// says so.
	if len(sent) != 2 || to[0] != f.owner || to[1] != f.owner || !strings.HasPrefix(sent[0].Title, "Page shared: Auto shared") ||
		sent[0].Data["url"] != "/pages/"+strconv.FormatInt(id, 10) || strings.Contains(sent[0].Title+sent[0].Body+sent[1].Body, tok) ||
		strings.Contains(sent[0].Body, "existing link") || !strings.Contains(sent[1].Body, "with the page's existing link") {
		t.Fatalf("the shared pushes: %+v to %v", sent, to)
	}
	detail := f.get(t, "/pages/"+strconv.FormatInt(id, 10), f.ownerHdr()).Body.String()
	if !strings.Contains(detail, "made live by "+f.agent.Name+" without asking you") || !strings.Contains(detail, "data-link-revoke") {
		t.Fatalf("the page's screen does not say the agent shared it: %s", detail)
	}
}

// TestAnOwnerWhoDidNotOptInIsStillAsked: owner A opted in, owner B turned it
// off. B's agent still files the pending link and the owner card; A's shares
// at once — on the same server, at the same time.
func TestAnOwnerWhoDidNotOptInIsStillAsked(t *testing.T) {
	f := newPageFixture(t).keyed(t)
	f.optIn(t, f.owner, true)
	f.optIn(t, f.other, false)
	if f.optedIn(f.other) {
		t.Fatal("B is still opted in")
	}
	a := f.create(t, f.agent, "A's page", `[{"type":"markdown","text":"a"}]`)
	b := f.create(t, f.otherAgent, "B's page", `[{"type":"markdown","text":"b"}]`)
	beforeB := f.asksOf(t, f.other)
	rb := f.tool(t, f.otherAgent, pageShareRequestTool, map[string]any{"page_id": b})
	if rb["status"] != "pending" || pnum(rb["ask_id"]) <= 0 || f.asksOf(t, f.other) != beforeB+1 {
		t.Fatalf("B (not opted in): %v", rb)
	}
	if w := f.get(t, pages.LinkPath(tokenOf(t, rb["url"])), nil); w.Code != http.StatusNotFound {
		t.Fatalf("B's pending link serves: %d", w.Code)
	}
	if ra := f.tool(t, f.agent, pageShareRequestTool, map[string]any{"page_id": a}); ra["status"] != "live" {
		t.Fatalf("A (opted in): %v", ra)
	}
}

// TestOnlyTheOwnerFlipsTheirOwnOptIn: a member's POST sets their own switch
// and nobody else's; it is ON for everyone until then (migration 0160), and
// the Account page draws it on for a member who never chose; the route takes
// no agent's, anonymous or cross-origin request; and the store refuses a read
// of another user's row.
func TestOnlyTheOwnerFlipsTheirOwnOptIn(t *testing.T) {
	f := newPageFixture(t)
	if !f.optedIn(f.owner) || !f.optedIn(f.other) {
		t.Fatal("the switch is not on by default")
	}
	// A new member's Account page: the switch on, its button "Turn off".
	acct := f.get(t, accountPath, f.ownerHdr()).Body.String()
	if !strings.Contains(acct, `id="`+pagesAgentShareID+`" data-testid="`+pagesAgentShareID+`" data-state="on"`) {
		t.Fatalf("a new member's Account page lacks the switch, on")
	}
	if i := strings.Index(acct, `id="`+pagesAgentShareID+`-toggle"`); i < 0 || !strings.Contains(acct[i:min(len(acct), i+600)], "Turn off") {
		t.Fatalf("a new member's Account page: the switch's button does not say Turn off")
	}
	f.optIn(t, f.owner, false)
	if f.optedIn(f.owner) || !f.optedIn(f.other) {
		t.Fatalf("A's POST: A %v, B %v; want A off and B untouched", f.optedIn(f.owner), f.optedIn(f.other))
	}
	body := url.Values{"on": {"1"}}.Encode()
	ct := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	for name, h := range map[string]map[string]string{
		"no credential":        merge(ct, map[string]string{"Origin": "http://example.com"}),
		"an agent's token":     merge(ct, map[string]string{"Origin": "http://example.com", "Authorization": "Bearer " + f.agent.HooksToken}),
		"a session, no Origin": merge(ct, map[string]string{"Cookie": sessionCookieFor(f.srv), "Origin": "null"}),
	} {
		if w := f.call(t, http.MethodPost, accountPagesAgentSharePath, strings.NewReader(body), h); w.Code == http.StatusSeeOther {
			t.Errorf("%s flipped the opt-in: %d", name, w.Code)
		}
	}
	if f.optedIn(f.owner) {
		t.Fatal("a refused request turned A's switch back on")
	}
	// The store: B's principal cannot read A's row, nor write it.
	if _, err := f.srv.ext.NotifyPrefs.PagesAgentShare(f.as(f.other, authz.ViaSession), f.owner); err == nil {
		t.Fatal("B read A's opt-in")
	}
	_ = f.srv.ext.NotifyPrefs.SetPagesAgentShare(f.as(f.other, authz.ViaSession), f.owner, true)
	if f.optedIn(f.owner) {
		t.Fatal("B wrote A's switch")
	}
	// The Account page draws the switch in the caller's own state.
	f.session = f.other
	acct = f.get(t, accountPath, f.ownerHdr()).Body.String()
	if !strings.Contains(acct, `id="`+pagesAgentShareID+`" data-testid="`+pagesAgentShareID+`" data-state="on"`) || !strings.Contains(acct, pagesAgentShareLabel) {
		t.Fatalf("B's Account page lacks the switch, on")
	}
	f.session = f.owner
	acct = f.get(t, accountPath, f.ownerHdr()).Body.String()
	if !strings.Contains(acct, `id="`+pagesAgentShareID+`" data-testid="`+pagesAgentShareID+`" data-state="off"`) {
		t.Fatalf("A's Account page lacks the switch, off")
	}
}

// TestTheOptInIsThePageOwners: page_share_request reads the PAGE OWNER's
// switch — not the operator's, not anybody else's. The operator (user 1) is
// on and the page's owner turned it off: asked. Then the owner turns it on
// and the operator off: live. (The mutation check in the PR: read
// authz.OperatorUserID's instead of p.OwnerID's, and this goes red.)
func TestTheOptInIsThePageOwners(t *testing.T) {
	f := newPageFixture(t).keyed(t).askFlow(t)
	op := f.as(authz.OperatorUserID, authz.ViaSession)
	if err := f.srv.ext.NotifyPrefs.SetPagesAgentShare(op, authz.OperatorUserID, true); err != nil {
		t.Fatal(err)
	}
	// Back to the operator's default (no row) after.
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM notify_preferences WHERE owner_id = $1`, authz.OperatorUserID)
	})
	id := f.create(t, f.agent, "Whose setting", `[{"type":"markdown","text":"x"}]`)
	if r := f.tool(t, f.agent, pageShareRequestTool, map[string]any{"page_id": id}); r["status"] != "pending" {
		t.Fatalf("the operator opted in, the owner did not: %v (want pending)", r)
	}
	if err := f.srv.ext.NotifyPrefs.SetPagesAgentShare(op, authz.OperatorUserID, false); err != nil {
		t.Fatal(err)
	}
	f.optIn(t, f.owner, true)
	if r := f.tool(t, f.agent, pageShareRequestTool, map[string]any{"page_id": id}); r["status"] != "live" {
		t.Fatalf("the owner opted in, the operator did not: %v (want live)", r)
	}
}

// TestTheToolsReadRightInBothStates: page_share_request's description names
// both outcomes and whose setting decides; page_create's next hint says what a
// share will do for THIS owner — asked, or live at once — and flips when the
// owner flips the opt-in.
func TestTheToolsReadRightInBothStates(t *testing.T) {
	var desc string
	for _, d := range pageToolDefs() {
		if d.Name == pageShareRequestTool {
			desc = d.Description
		}
	}
	for _, want := range []string{`status "live"`, `status "pending"`, "your owner's own setting", "attention_wait"} {
		if !strings.Contains(desc, want) {
			t.Errorf("page_share_request's description lacks %q: %s", want, desc)
		}
	}
	f := newPageFixture(t)
	next := func() string {
		out := f.tool(t, f.agent, pageCreateTool, map[string]any{"title": "Hint", "blocks": json.RawMessage(`[{"type":"markdown","text":"x"}]`)})
		n, _ := out["next"].(string)
		return n
	}
	if n := next(); !strings.Contains(n, "its link is live at once, and your owner is told") || strings.Contains(n, "approves") {
		t.Fatalf("next, an owner who never chose (on): %s", n)
	}
	f.optIn(t, f.owner, false)
	if n := next(); !strings.Contains(n, "your owner is asked, and approves the link") || strings.Contains(n, "live at once") {
		t.Fatalf("next, turned off: %s", n)
	}
	f.optIn(t, f.owner, true)
	if n := next(); !strings.Contains(n, "its link is live at once, and your owner is told") || strings.Contains(n, "approves") {
		t.Fatalf("next, turned on again: %s", n)
	}
}

// TestAnOwnerWhoNeverChoseHasTheirAgentShareAtOnce: an owner with NO
// notify_preferences row at all — they never touched any switch — gets the
// default, on (migration 0160): their agent's page_share_request through the
// real tool path makes the link live, files no attention ask, and a stranger
// opens it.
func TestAnOwnerWhoNeverChoseHasTheirAgentShareAtOnce(t *testing.T) {
	f := newPageFixture(t)
	var rows int
	if err := f.pool.QueryRow(authz.System(f.ctx), `SELECT count(*) FROM notify_preferences WHERE owner_id = $1`, f.owner).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("fixture: the owner has %d preference row(s) (%v), want none", rows, err)
	}
	id := f.create(t, f.agent, "Never chose", `[{"type":"markdown","text":"x"}]`)
	before := f.asksOf(t, f.owner)
	req := f.tool(t, f.agent, pageShareRequestTool, map[string]any{"page_id": id})
	if req["status"] != "live" || req["ask_id"] != nil {
		t.Fatalf("page_share_request for an owner who never chose: %v, want live with no ask", req)
	}
	if n := f.asksOf(t, f.owner); n != before {
		t.Fatalf("it filed %d attention ask(s)", n-before)
	}
	if w := f.get(t, pages.LinkPath(tokenOf(t, req["url"])), nil); w.Code != http.StatusOK {
		t.Fatalf("the link does not serve a stranger: %d", w.Code)
	}
	// Another owner whose row was CREATED by another switch (Show tmux
	// sessions, saved first): the row takes the column's default, on.
	if err := f.srv.ext.NotifyPrefs.SetTmuxAddress(f.as(f.other, authz.ViaSession), f.other, true); err != nil {
		t.Fatal(err)
	}
	var share bool
	if err := f.pool.QueryRow(authz.System(f.ctx), `SELECT pages_agent_share FROM notify_preferences WHERE owner_id = $1`, f.other).Scan(&share); err != nil || !share {
		t.Fatalf("a row the tmux switch created: pages_agent_share=%v (%v), want true", share, err)
	}
	ob := f.create(t, f.otherAgent, "Row from tmux", `[{"type":"markdown","text":"x"}]`)
	if r := f.tool(t, f.otherAgent, pageShareRequestTool, map[string]any{"page_id": ob}); r["status"] != "live" {
		t.Fatalf("an owner whose row another switch created: %v, want live", r)
	}
}

// TestAnOptedInShareWithoutAKeyMakesAHashOnlyLiveLink: no link key, opted in:
// the link is still live at once (hash-only, shown in this result only).
func TestAnOptedInShareWithoutAKeyMakesAHashOnlyLiveLink(t *testing.T) {
	f := newPageFixture(t)
	f.optIn(t, f.owner, true)
	id := f.create(t, f.agent, "No key", `[{"type":"markdown","text":"x"}]`)
	req := f.tool(t, f.agent, pageShareRequestTool, map[string]any{"page_id": id})
	if req["status"] != "live" || req["reusable"] != false {
		t.Fatalf("opted in without a key: %v", req)
	}
	if w := f.get(t, pages.LinkPath(tokenOf(t, req["url"])), nil); w.Code != http.StatusOK {
		t.Fatalf("the link does not serve: %d", w.Code)
	}
}

// TestAnAgentCannotAutoShareAnotherOwnersPage: with both owners opted in,
// another owner's agent — and another agent of the same owner — get "no such
// page", and no link is made on the page. 🔴 THE GUARD THIS PINS is the
// PageFor read under the agent's owner's principal before any share (the
// mutation check in the PR).
func TestAnAgentCannotAutoShareAnotherOwnersPage(t *testing.T) {
	f := newPageFixture(t).keyed(t)
	f.optIn(t, f.owner, true)
	f.optIn(t, f.other, true)
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
	// The page's screen holds its link in the ⓘ beside Share / Copy link —
	// never drawn in its top layer.
	if d := f.get(t, base, f.ownerHdr()).Body.String(); !strings.Contains(d, `data-info-code="">`+first["url"].(string)+`</code>`) ||
		strings.Contains(d, `value="`+first["url"].(string)+`"`) || !strings.Contains(d, "data-page-share") || !strings.Contains(d, "data-page-copy") {
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
// ready: <title>", opening the owner's view; page_update sends no second
// "ready" (its own "Page updated" push is
// TestEveryPageVersionPushesItsOwnerUnderTheSendersCap's).
func TestPublishPushesTheOwnerOnce(t *testing.T) {
	f := newPageFixture(t)
	id := f.create(t, f.agent, "Fresh page", `[{"type":"markdown","text":"v1"}]`)
	f.tool(t, f.agent, pageUpdateTool, map[string]any{"page_id": id, "blocks": json.RawMessage(`[{"type":"markdown","text":"v2"}]`)})
	sent, to := f.pagePushesTagged("page-" + strconv.FormatInt(id, 10) + "-ready")
	if len(sent) != 1 || to[0] != f.owner || sent[0].Title != "Page ready: Fresh page" ||
		sent[0].Data["url"] != "/pages/"+strconv.FormatInt(id, 10)+"/view" {
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
		!strings.Contains(w.Body.String(), `href="/pages/`+strconv.FormatInt(id, 10)+`/view"`) {
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

// TestATeamOwnedAgentsShareIsAlwaysAsked: a TEAM-owned agent's
// page_share_request, through the real tool path as the team acting
// principal, stays PENDING — an attention ask is filed and no link serves —
// with the team's preferences row absent AND with it forced to true (a team
// is never auto-shared, whatever is stored: nobody signs in as a team). The
// control: a person beside it, with no row, shares at once.
func TestATeamOwnedAgentsShareIsAlwaysAsked(t *testing.T) {
	f := newPageFixture(t).keyed(t)
	sys := authz.System(context.Background())
	sfx := strconv.FormatInt(time.Now().UnixNano(), 36)
	team, err := users.NewPG(f.pool).CreateTeam(sys, users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}, "pages team "+sfx, f.owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM pages WHERE owner_id = $1`, `DELETE FROM attention_asks WHERE owner_id = $1`,
			`DELETE FROM agents WHERE owner_id = $1`, `DELETE FROM notify_preferences WHERE owner_id = $1`,
			`DELETE FROM team_members WHERE team_id = $1`, `DELETE FROM audit_log WHERE team_id = $1::bigint`, `DELETE FROM users WHERE id = $1`} {
			_, _ = f.pool.Exec(sys, q, team.ID)
		}
	})
	teamCtx := authz.WithPrincipal(context.Background(), authz.Principal{UserID: team.ID, Team: true, Role: authz.RoleMember, Via: authz.ViaAgent})
	ta, err := agents.NewPG(f.pool).Create(teamCtx, agents.Agent{Name: "pgt-" + sfx, Namespace: "devpod-pgt-" + sfx,
		Status: agents.StatusRunning, HooksToken: "pgt-" + sfx + "-token"})
	if err != nil || ta.OwnerID != team.ID {
		t.Fatalf("the team's agent: owner %d (want %d), %v", ta.OwnerID, team.ID, err)
	}
	call := func(name string, args map[string]any) map[string]any {
		t.Helper()
		return f.toolAs(t, teamCtx, callerOfAgent(ta), name, args)
	}
	out := call(pageCreateTool, map[string]any{"title": "Team page", "blocks": json.RawMessage(`[{"type":"markdown","text":"x"}]`)})
	id := pnum(out["page_id"])
	if id <= 0 {
		t.Fatalf("page_create as the team's agent: %v", out)
	}
	for _, state := range []string{"no row", "row forced true"} {
		if state == "row forced true" {
			if _, err := f.pool.Exec(sys, `INSERT INTO notify_preferences (owner_id, pages_agent_share) VALUES ($1, true)
				ON CONFLICT (owner_id) DO UPDATE SET pages_agent_share = true`, team.ID); err != nil {
				t.Fatal(err)
			}
		}
		before := f.asksOf(t, team.ID)
		req := call(pageShareRequestTool, map[string]any{"page_id": id})
		if req["status"] != "pending" || pnum(req["ask_id"]) <= 0 {
			t.Fatalf("%s: a team agent's page_share_request: %v, want pending with an ask", state, req)
		}
		if n := f.asksOf(t, team.ID); n != before+1 {
			t.Fatalf("%s: %d ask(s) filed, want 1", state, n-before)
		}
		if w := f.get(t, pages.LinkPath(tokenOf(t, req["url"])), nil); w.Code != http.StatusNotFound {
			t.Fatalf("%s: the team's pending link serves: %d", state, w.Code)
		}
	}
	// The control: a person with no row shares at once.
	pid := f.create(t, f.agent, "Person page", `[{"type":"markdown","text":"x"}]`)
	if r := f.tool(t, f.agent, pageShareRequestTool, map[string]any{"page_id": pid}); r["status"] != "live" {
		t.Fatalf("control: a person with no row: %v, want live", r)
	}
}

// TestAStepUpCoveredCallerIsAlwaysAsked: with the owner's switch ON
// (explicitly), a mirrored session the step-up gate covers — a host whose
// token is marked as having started agents for the chief, and a remote caller
// (a session mirrored to another muster) — gets status "pending" and files
// exactly one ask; page_create's hint says it will be asked. The control: a
// mirrored session on an UNCOVERED host of the same owner, same switch, goes
// live. (operator decision, #847 round 1; pageShareGoesLive.)
func TestAStepUpCoveredCallerIsAlwaysAsked(t *testing.T) {
	f := newPageFixture(t).keyed(t)
	f.optIn(t, f.owner, true)
	base := f.as(f.owner, authz.ViaMirrorHost)
	spawned := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name string
		ctx  context.Context
		c    toolCaller
	}{
		{"a host that started agents for the chief",
			context.WithValue(base, mirrorHostCtxKey{}, mirror.Token{ID: 9, OwnerID: f.owner, Host: "host-spawner", SpawnedAt: &spawned}),
			mirrorCaller("host-spawner", "0d1e2f3a-aaaa-4bbb-8ccc-000000000101")},
		{"a remote caller", base, func() toolCaller {
			c := mirrorCaller("host-elsewhere", "0d1e2f3a-aaaa-4bbb-8ccc-000000000102")
			c.remote = true
			return c
		}()},
	} {
		out := f.toolAs(t, tc.ctx, tc.c, pageCreateTool, map[string]any{"title": "Covered " + tc.name, "blocks": json.RawMessage(`[{"type":"markdown","text":"x"}]`)})
		id := pnum(out["page_id"])
		if n, _ := out["next"].(string); id <= 0 || !strings.Contains(n, "your owner is asked") {
			t.Fatalf("%s: page_create: %v (want its hint to say the owner is asked)", tc.name, out)
		}
		before := f.asksOf(t, f.owner)
		req := f.toolAs(t, tc.ctx, tc.c, pageShareRequestTool, map[string]any{"page_id": id})
		if req["status"] != "pending" || pnum(req["ask_id"]) <= 0 {
			t.Fatalf("%s, switch on: page_share_request %v, want pending with an ask", tc.name, req)
		}
		if n := f.asksOf(t, f.owner); n != before+1 {
			t.Fatalf("%s: %d ask(s) filed, want 1", tc.name, n-before)
		}
		if w := f.get(t, pages.LinkPath(tokenOf(t, req["url"])), nil); w.Code != http.StatusNotFound {
			t.Fatalf("%s: the pending link serves: %d", tc.name, w.Code)
		}
	}
	// The control: an uncovered host of the same owner, the same switch.
	ctx := context.WithValue(base, mirrorHostCtxKey{}, mirror.Token{ID: 10, OwnerID: f.owner, Host: "host-plain"})
	c := mirrorCaller("host-plain", "0d1e2f3a-aaaa-4bbb-8ccc-000000000103")
	out := f.toolAs(t, ctx, c, pageCreateTool, map[string]any{"title": "Uncovered", "blocks": json.RawMessage(`[{"type":"markdown","text":"x"}]`)})
	if r := f.toolAs(t, ctx, c, pageShareRequestTool, map[string]any{"page_id": pnum(out["page_id"])}); r["status"] != "live" {
		t.Fatalf("control: an uncovered host, switch on: %v, want live", r)
	}
}
