package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE ASKS BADGE'S SCOPE (attention_asker.go), on a real Postgres: whose asks
// it counts, and whose window a ?from= may name.

// TestATeamViewersBadgeLeavesOutTheTeamsAsks: a VIEWER of a team reads the
// team chief's page and the Asks page filtered to it. The team's two open
// asks are listed (watch-only), but they are not waiting on the viewer, so
// the badge counts none (it is not drawn) and the filtered page's server
// count is 0 — the rule every other count uses (waitingOnReader). An
// OPERATOR of the team (the control) gets 2 on both.
//
// 🔴 RED before the fix: the viewer's badge said 2 "questions waiting on your
// answer", and the filtered page's server count said 2 (its script then
// rewrote it to 0).
func TestATeamViewersBadgeLeavesOutTheTeamsAsks(t *testing.T) {
	td := newTeamDispatchWorld(t)
	for _, q := range []string{"Team ask one?", "Team ask two?"} {
		if _, err := td.w.srv.ext.Attention.Create(td.teamCtx, attention.New{Asker: attention.Asker{AgentID: td.chief.ID}, Question: q}); err != nil {
			t.Fatal(err)
		}
	}
	ref := askerRef{agent: td.chief.Name}
	filtered := func(ctx context.Context) string {
		r := httptest.NewRequest(http.MethodGet, ref.asksHref(), nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		td.w.srv.handleAttentionPage(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("the filtered Asks page: %d", rec.Code)
		}
		return rec.Body.String()
	}
	for _, c := range []struct {
		who   string
		ctx   context.Context
		count int
	}{
		{"V (viewer)", td.personCtx(td.v.uid, authz.ViaSession, true), 0},
		{"B (operator, the control)", td.personCtx(td.w.b.uid, authz.ViaSession, true), 2},
	} {
		badge, _ := td.w.srv.askerAsksBadge(c.ctx, ref)
		if _, n := badgeOf(t, badge); n != c.count {
			t.Errorf("%s: the team chief's badge counts %d, want %d:\n%s", c.who, n, c.count, badge)
		}
		page := filtered(c.ctx)
		if got := len(asksListed(page)); got != 2 {
			t.Errorf("%s: the filtered page lists %d of the team's asks, want both (listed, counted or not)", c.who, got)
		}
		if want := ">" + strconv.Itoa(c.count) + " open</span>"; !strings.Contains(page, want) {
			t.Errorf("%s: the filtered page's server count is not %q", c.who, want)
		}
	}
}

// TestAWindowKeyNamingAWindowNotTheViewersOwnIsIgnored: OWNER SCOPING for a
// window. A window: key is resolved over the viewer's OWN hosts
// (Mirror.Hosts), never the hosts the viewer can merely see (a team's,
// Mirror.VisibleHosts): naming a team host's window, or another person's,
// gets the page as without the filter, names nothing, and an empty badge.
// The control: the viewer's own window resolves.
func TestAWindowKeyNamingAWindowNotTheViewersOwnIsIgnored(t *testing.T) {
	f := newAttnFixture(t)
	f.mirror.mu.Lock()
	f.mirror.teamHosts = []mirror.HostView{{Host: "teamhost", Online: true, Interactive: true, StatusReport: true, Rendered: true,
		Windows: mirror.NormalizeWindows([]mirror.Window{{Session: "shared", ID: "@1", Index: 1, Active: true, Claude: true, ClaudeSession: laptopSID,
			Status: mirror.StatusIdle, StatusSince: attnSince, Activity: 1_700_000_000}}, 0)}}
	f.mirror.mu.Unlock()
	mine := f.ask(t, f.opTok, "My own question?")
	team := url.Values{askerFromParam: {"window:teamhost:@1:shared"}}
	page := f.page(t, "/attention?"+team.Encode())
	if strings.Contains(page, `data-testid="asks-filter"`) {
		t.Error("OWNER SCOPING: a team host's window filtered the operator's Asks page")
	}
	if got := asksListed(page); !got[mine] {
		t.Errorf("OWNER SCOPING: with a team host's window in ?from= the page lists %v, want their own %d", got, mine)
	}
	if _, ok := f.srv.asksFilter(asUser(1), team, f.srv.ext.Mirror.Hosts(asUser(1))); ok {
		t.Error("OWNER SCOPING: a team host's window resolved for the operator")
	}
	if frag := f.page(t, "/attention/asker?"+team.Encode()); frag != "" {
		t.Errorf("OWNER SCOPING: the badge fragment for a team host's window: %q", frag)
	}
	// Another person's window: the operator's own ws window, named by a member.
	own := url.Values{askerFromParam: {"window:ws:@1:main"}}
	if _, ok := f.srv.asksFilter(asUser(f.member), own, f.srv.ext.Mirror.Hosts(asUser(f.member))); ok {
		t.Error("OWNER SCOPING: another person's window resolved for a member")
	}
	// The control: the operator's own window resolves.
	if k, ok := f.srv.asksFilter(asUser(1), own, f.srv.ext.Mirror.Hosts(asUser(1))); !ok || k.ref.mirror.Host != "ws" {
		t.Fatalf("control: the operator's own window does not resolve: %+v %v", k, ok)
	}
}

// TestAQuestionToTheChiefKeepsTheSuggestionsAway: an ask a window's session
// addressed to the chief (to_chief) is not one waiting on the owner, so the
// badge does not count it — but the server withholds suggested replies while
// ANY ask from the session is open (suggest.go), and the view's script hides
// its chips by the same rule (suggest_row.go blocked()), which reads the
// badge slot's marker (data-asker-asks-open). With only that ask open the
// slot carries the marker and no count; with none open, neither.
func TestAQuestionToTheChiefKeepsTheSuggestionsAway(t *testing.T) {
	f := newAttnFixture(t)
	view := "/mirror/view?host=ws&session=main&window=%401"
	if p := f.pageFollow(t, view); strings.Contains(p, "data-asker-asks-open") {
		t.Fatal("control: the view marks an open ask with none open")
	}
	a, err := f.attn.Create(asUser(1), attention.New{Asker: attention.Asker{MirrorHost: "ws", MirrorSession: focusSID, Label: "main:1 @ ws"},
		Question: "Chief, which branch?", ToChief: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM attention_asks WHERE id = $1`, a.ID)
	})
	p := f.pageFollow(t, view)
	if _, n := badgeOf(t, p); n != 0 {
		t.Errorf("the badge counts the question to the chief: %d", n)
	}
	if !strings.Contains(p, "<span hidden data-asker-asks-open></span>") {
		t.Error("the view's badge slot does not mark the session's open question to the chief (the chips' block)")
	}
	if frag := f.page(t, "/attention/asker?"+askerRef{mirror: ui.MirrorRef{Host: "ws", Session: "main", Window: "@1"}}.query()); !strings.Contains(frag, "data-asker-asks-open") {
		t.Errorf("the live refresh drops the mark: %q", frag)
	}
}
