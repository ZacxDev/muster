package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/users"
)

// TestADemotedCreatorsRevokeAndDeleteLeaveNothingLive (round-1 audit): the
// page's creator, demoted to VIEWER (a creator keeps the page's management
// whatever their role, per the spec), revokes the team agent's pending link —
// and its card is WITHDRAWN, though 0127 lets a viewer write no
// attention_asks row through the ordinary cancel (0162's
// muster_cancel_team_page_card). Then they delete the page — and its uploads
// rows, the team's, under the page's prefix, are REVOKED, though 0127 lets a
// viewer revoke none (muster_revoke_team_page_uploads). CONTROLS: the
// ordinary cancel, as the demoted creator, withdraws nothing; the team's
// upload row outside the page's prefix and team B's under a look-alike prefix
// are left alone; a member who may not manage the page revokes nothing
// through the door.
func TestADemotedCreatorsRevokeAndDeleteLeaveNothingLive(t *testing.T) {
	f := newTeamPageFixture(t)
	id := f.teamPage(t, "Budget review")
	sid := strconv.FormatInt(id, 10)
	sys := authz.System(context.Background())
	if err := f.users.SetTeamMember(sys, users.Actor{UserID: authz.OperatorUserID, Via: authz.ViaSession}, f.teamA, f.creator, authz.TeamViewer); err != nil {
		t.Fatal(err)
	}
	out := f.teamTool(t, pageShareRequestTool, map[string]any{"page_id": id})
	ask := pnum(out["ask_id"])
	if ask <= 0 {
		t.Fatalf("share request: %v", out)
	}
	status := func() string {
		t.Helper()
		var st string
		if err := f.pool.QueryRow(sys, `SELECT status FROM attention_asks WHERE id = $1`, ask).Scan(&st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	session := func(uid int64) context.Context {
		return authz.WithPrincipal(context.Background(), f.srv.principalFor(context.Background(), uid, authz.ViaSession))
	}
	// CONTROL: the ordinary cancel, as the demoted creator, withdraws nothing.
	if _, err := attention.NewPG(f.pool).Cancel(session(f.creator), ask); err == nil || status() != "open" {
		t.Fatalf("control: the ordinary cancel as a viewer-role creator: %v, the card %s; want it refused and open", err, status())
	}
	// The narrow door withdraws a card only once its link is REVOKED: while
	// the link is still pending, a member's call withdraws nothing.
	if _, err := attention.NewPG(f.pool).CancelTeamPageCard(session(f.creator), ask); err == nil || status() != "open" {
		t.Fatalf("the narrow door on a card whose link is still pending: %v, the card %s; want it refused and open", err, status())
	}
	l, err := f.pages.LinkByAsk(f.asTeam(f.teamA), ask)
	if err != nil {
		t.Fatal(err)
	}
	if code, body := f.req(t, f.creator, http.MethodPost, "/pages/"+sid+"/links/"+strconv.FormatInt(l.ID, 10)+"/revoke", "", formHdr); code != http.StatusSeeOther {
		t.Fatalf("the demoted creator's revoke: %d %s", code, body)
	}
	if got := status(); got != "cancelled" {
		t.Errorf("after the demoted creator revoked the link its card is %q; want cancelled", got)
	}

	// The page's files: two of the team's under its prefix, one of the
	// team's elsewhere, and under a look-alike prefix (page <id>0's) one of
	// team B's and one of the team's own.
	type row struct {
		owner int64
		key   string
	}
	pid := func(c string) string { return strings.Repeat(c, 22) }
	rows := map[string]row{
		pid("A"): {f.teamA, "pages/" + sid + "/" + pid("A")},
		pid("B"): {f.teamA, "pages/" + sid + "/" + pid("B")},
		pid("C"): {f.teamA, "uploads/" + pid("C")},
		pid("D"): {f.teamB, "pages/" + sid + "0/" + pid("D")},
		pid("E"): {f.teamA, "pages/" + sid + "0/" + pid("E")},
	}
	for p, r := range rows {
		if _, err := f.pool.Exec(sys, `INSERT INTO uploads (owner_id, public_id, object_key, filename, content_type, size_bytes, expires_at)
			VALUES ($1, $2, $3, 'f.txt', 'text/plain', 3, now() + interval '1 day')`, r.owner, p, r.key); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for p := range rows {
			_, _ = f.pool.Exec(sys, `DELETE FROM uploads WHERE public_id = $1`, p)
		}
	})
	// The door refuses a member who may not manage the page.
	if n, err := f.pages.RevokeTeamPageUploads(session(f.both), id); err == nil {
		t.Errorf("an operator (not the creator) revoked %d of the page's files", n)
	}
	if code, body := f.req(t, f.creator, http.MethodPost, "/pages/"+sid+"/delete", "", formHdr); code != http.StatusSeeOther {
		t.Fatalf("the demoted creator's delete: %d %s", code, body)
	}
	for p, want := range map[string]bool{pid("A"): true, pid("B"): true, pid("C"): false, pid("D"): false, pid("E"): false} {
		var revoked bool
		if err := f.pool.QueryRow(sys, `SELECT revoked_at IS NOT NULL FROM uploads WHERE public_id = $1`, p).Scan(&revoked); err != nil {
			t.Fatal(err)
		}
		if revoked != want {
			t.Errorf("upload %s (%s) revoked = %v after the delete; want %v", p, rows[p].key, revoked, want)
		}
	}
}
