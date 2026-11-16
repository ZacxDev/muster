package api

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/pages"
	"github.com/ZacxDev/muster/internal/ui"
)

// TestAnAgentReusingTheOwnersLinkStillPushes: the owner shared the page
// first (the reusable link is theirs); then their opted-in agent's
// page_share_request returns that same link — and the owner is STILL pushed
// "Page shared" (an agent handed the link on, whoever made it), and the
// agent's result says the link was the owner's, not the agent's.
func TestAnAgentReusingTheOwnersLinkStillPushes(t *testing.T) {
	f := newPageFixture(t).keyed(t)
	id := f.create(t, f.agent, "Owner first", `[{"type":"markdown","text":"x"}]`)
	_, mine := f.share(t, "/pages/"+strconv.FormatInt(id, 10)+"/share")
	f.optIn(t, f.owner, true)
	req := f.tool(t, f.agent, pageShareRequestTool, map[string]any{"page_id": id})
	if req["status"] != "live" || req["url"] != mine["url"] {
		t.Fatalf("the agent's share: %v, want the owner's url %v", req, mine["url"])
	}
	if req["shared_by"] != "your owner" || !strings.Contains(req["note"].(string), "your owner already shared") {
		t.Errorf("the agent's result is not truthful about whose link it is: %v", req)
	}
	sent, to := f.pagePushesTagged("page-" + strconv.FormatInt(id, 10) + "-shared")
	if len(sent) != 1 || to[0] != f.owner || !strings.HasPrefix(sent[0].Title, "Page shared: Owner first") {
		t.Fatalf("no \"Page shared\" push for a reused link: %+v to %v", sent, to)
	}
}

// TestAPageBeingDeletedGetsNoNewLink: once a delete has closed a page's links
// (handlePageDelete's first step, under the page row's lock), an opted-in
// agent's page_share_request — and the owner's Share — make no new link on
// it: no link is live between the close and the delete.
func TestAPageBeingDeletedGetsNoNewLink(t *testing.T) {
	f := newPageFixture(t).keyed(t)
	f.optIn(t, f.owner, true)
	id := f.create(t, f.agent, "Going away", `[{"type":"markdown","text":"x"}]`)
	if _, err := f.pages.CloseLinks(f.as(f.owner, authz.ViaSession), id); err != nil {
		t.Fatal(err)
	}
	if req := f.tool(t, f.agent, pageShareRequestTool, map[string]any{"page_id": id}); req["url"] != nil || req["error"] != "page "+strconv.FormatInt(id, 10)+" is being deleted: it takes no new link" {
		t.Errorf("an agent shared a page being deleted: %v", req)
	}
	if code, out := f.share(t, "/pages/"+strconv.FormatInt(id, 10)+"/share"); code != http.StatusConflict || out["url"] != nil || !strings.Contains(out["error"].(string), "being deleted") {
		t.Errorf("the owner's Share made a link on a page being deleted: %d %v", code, out)
	}
	var live int
	if err := f.pool.QueryRow(authz.System(f.ctx), `SELECT count(*) FROM page_links WHERE page_id = $1 AND state IN ('live', 'pending')`, id).Scan(&live); err != nil || live != 0 {
		t.Fatalf("%d open link(s) on a page being deleted (%v)", live, err)
	}
}

// TestTheSharedCardNamesTheReusableLinksMaker: the card of a shared page names
// who made its REUSABLE link — not whoever made the newest live link of any
// kind (here an agent's older-style hash-only link, made after the owner's).
func TestTheSharedCardNamesTheReusableLinksMaker(t *testing.T) {
	f := newPageFixture(t).keyed(t)
	id := f.create(t, f.agent, "Two links", `[{"type":"markdown","text":"x"}]`)
	sid := strconv.FormatInt(id, 10)
	f.share(t, "/pages/"+sid+"/share")
	if _, err := f.pages.CreateLink(f.as(f.owner, authz.ViaSession), id, pages.LinkLive, "some-other-agent", true); err != nil {
		t.Fatal(err)
	}
	body := f.get(t, "/pages/"+sid+"/card?tool=page_share_request&status=live", f.ownerHdr()).Body.String()
	if strings.Contains(body, "some-other-agent") || !strings.Contains(body, "Shared by you") {
		t.Fatalf("the shared card names the wrong maker: %s", body)
	}
}

// TestTheStoredChatsCardIsThePagesOwn: the stored chat's cards are read from
// the pages under the viewer's principal — the title is the page's, whatever
// the tool result claims; another member's page id draws no card; another MCP
// server's same-named tool draws none.
func TestTheStoredChatsCardIsThePagesOwn(t *testing.T) {
	f := newPageFixture(t).keyed(t)
	mine := f.create(t, f.agent, "The real title", `[{"type":"markdown","text":"x"}]`)
	theirs := f.create(t, f.otherAgent, "Not yours", `[{"type":"markdown","text":"x"}]`)
	res := func(id int64) string {
		return `{"page_id":` + strconv.FormatInt(id, 10) + `,"version":1,"title":"FORGED-TITLE"}`
	}
	lines := []ui.ChatLine{
		{Kind: "tool_call", ToolID: "t1", ToolName: "page_create"},
		{Kind: "tool_result", ToolID: "t1", ToolOK: true, Content: res(mine)},
		{Kind: "tool_call", ToolID: "t2", ToolName: "mcp__muster__page_create"},
		{Kind: "tool_result", ToolID: "t2", ToolOK: true, Content: res(theirs)},
		{Kind: "tool_call", ToolID: "t3", ToolName: "mcp__evil__page_create"},
		{Kind: "tool_result", ToolID: "t3", ToolOK: true, Content: res(mine)},
	}
	cards := f.srv.pageCardsFor(f.as(f.owner, authz.ViaSession), lines)
	if c, ok := cards["t1"]; !ok || c.Title != "The real title" || c.ID != mine {
		t.Fatalf("the owner's own page's card: %+v %v", c, ok)
	}
	if _, ok := cards["t2"]; ok {
		t.Error("another member's page drew a card")
	}
	if _, ok := cards["t3"]; ok {
		t.Error("another MCP server's page_create drew a card")
	}
	if len(cards) != 1 {
		t.Fatalf("cards: %+v", cards)
	}
}
