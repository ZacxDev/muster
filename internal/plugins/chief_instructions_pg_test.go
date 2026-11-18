package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/dbtest"
)

// A PLUGIN'S CHIEF INSTRUCTIONS (migration 0141) against a real Postgres:
// what is stored, what an approval names, and exactly which sections a chief
// owned by an owner renders (ApprovedChiefSections). Every text below is
// distinct from every other and from any muster constant.

// hexSHA is the sha256 of text, computed HERE (not by the code under test).
func hexSHA(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

func mustGuide(t *testing.T, s *PGStore, uid int64, name string) Plugin {
	t.Helper()
	_, p, err := s.CreatePlugin(session(uid), NewPlugin{Name: name, Label: "Example guide", Capabilities: []Capability{CapChiefExtension}})
	if err != nil {
		t.Fatalf("create %s for %d: %v", name, uid, err)
	}
	return p
}

// rendered is owner's rendered sections as "plugin: text" lines.
func rendered(t *testing.T, s *PGStore, owner int64) []string {
	t.Helper()
	secs, err := s.ApprovedChiefSections(context.Background(), owner)
	if err != nil {
		t.Fatalf("ApprovedChiefSections(%d): %v", owner, err)
	}
	var out []string
	for _, sec := range secs {
		if sec.ApprovedAt.IsZero() {
			t.Errorf("a rendered section of %s has no approval time", sec.Plugin)
		}
		out = append(out, sec.Plugin+": "+sec.Text)
	}
	return out
}

func put(t *testing.T, s *PGStore, uid, pluginID int64, text string) (ChiefInstructions, bool) {
	t.Helper()
	c, changed, err := s.PutChiefInstructions(plugin(uid), pluginID, text)
	if err != nil {
		t.Fatalf("put %q: %v", text, err)
	}
	if c.SHA != hexSHA(text) || c.Text != text {
		t.Fatalf("put %q stored sha %s text %q", text, c.SHA, c.Text)
	}
	return c, changed
}

// TestAChiefSectionRendersOnlyOnceItsExactTextIsApproved: stored text is
// pending and not rendered; approving its sha renders it; the SAME text again
// keeps it; CHANGED text is pending again and not rendered; approving the OLD
// sha then is ErrStaleApproval and approves nothing; and returning to the old
// text (A → B → A) does not bring the old approval back.
func TestAChiefSectionRendersOnlyOnceItsExactTextIsApproved(t *testing.T) {
	s, _, _ := pgStore(t)
	p := mustGuide(t, s, 2, "example-quill")
	const textA, textB = "Quill ledger lives at the quill board.", "Quill ledger moved to the heron board."

	c, changed := put(t, s, 2, p.ID, textA)
	if !changed || c.Approved() || c.ApprovedSHA != nil {
		t.Fatalf("new text: changed %v approved %v; want stored and pending", changed, c.Approved())
	}
	if got := rendered(t, s, 2); len(got) != 0 {
		t.Fatalf("pending text rendered: %v", got)
	}
	if _, err := s.ApproveChiefInstructions(session(2), p.ID, hexSHA(textA)); err != nil {
		t.Fatalf("approve A: %v", err)
	}
	if got := rendered(t, s, 2); strings.Join(got, "|") != "example-quill: "+textA {
		t.Fatalf("approved text renders %v; want A alone", got)
	}
	if c, changed := put(t, s, 2, p.ID, textA); changed || !c.Approved() {
		t.Fatalf("the same text again: changed %v approved %v; want unchanged and still approved", changed, c.Approved())
	}
	if c, changed := put(t, s, 2, p.ID, textB); !changed || c.Approved() || c.ApprovedSHA != nil || c.ApprovedAt != nil {
		t.Fatalf("changed text: changed %v approved %v approved_sha %v; want pending with no approval", changed, c.Approved(), c.ApprovedSHA)
	}
	if got := rendered(t, s, 2); len(got) != 0 {
		t.Fatalf("changed (pending) text rendered: %v", got)
	}
	if _, err := s.ApproveChiefInstructions(session(2), p.ID, hexSHA(textA)); !errors.Is(err, ErrStaleApproval) {
		t.Fatalf("approving the old sha after a change: %v; want ErrStaleApproval", err)
	}
	if got := rendered(t, s, 2); len(got) != 0 {
		t.Fatalf("a stale approval rendered something: %v", got)
	}
	if c, changed := put(t, s, 2, p.ID, textA); !changed || c.Approved() {
		t.Fatalf("back to A: changed %v approved %v; want pending — the old approval does not come back", changed, c.Approved())
	}
	if got := rendered(t, s, 2); len(got) != 0 {
		t.Fatalf("A → B → A rendered without a new approval: %v", got)
	}
}

// TestARevokedOrSwitchedOffPluginsSectionIsGone: an approved section is gone
// when its plugin is revoked (the row deleted) or chief_extension is turned
// off (the row deleted; on again brings nothing back). And the render filter
// holds on its own: a row left behind on a plugin revoked, or stripped of the
// capability, behind the store's back renders nothing.
func TestARevokedOrSwitchedOffPluginsSectionIsGone(t *testing.T) {
	s, pool, dsn := pgStore(t)
	approved := func(name, text string) Plugin {
		t.Helper()
		p := mustGuide(t, s, 2, name)
		put(t, s, 2, p.ID, text)
		if _, err := s.ApproveChiefInstructions(session(2), p.ID, hexSHA(text)); err != nil {
			t.Fatal(err)
		}
		return p
	}
	rows := func(id int64) int {
		var n int
		if err := pool.QueryRow(authz.System(context.Background()), `SELECT count(*) FROM plugin_chief_instructions WHERE plugin_id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	revoked := approved("example-heron", "Heron notices go to the heron queue.")
	// Beside messages, so turning chief_extension off leaves a capability.
	_, off, err := s.CreatePlugin(session(2), NewPlugin{Name: "example-ibis", Label: "Example ibis", DataScope: "example-ibis",
		Capabilities: []Capability{CapMessages, CapChiefExtension}})
	if err != nil {
		t.Fatal(err)
	}
	put(t, s, 2, off.ID, "Ibis reviews are read on the ibis page.")
	if _, err := s.ApproveChiefInstructions(session(2), off.ID, hexSHA("Ibis reviews are read on the ibis page.")); err != nil {
		t.Fatal(err)
	}
	if got := rendered(t, s, 2); len(got) != 2 {
		t.Fatalf("control: two approved sections render %v", got)
	}
	if found, err := s.Revoke(session(2), revoked.ID); err != nil || !found {
		t.Fatal(found, err)
	}
	if rows(revoked.ID) != 0 {
		t.Errorf("a revoked plugin's section row survived the revoke")
	}
	if _, _, err := s.SetChiefExtension(session(2), off.ID, false); err != nil {
		t.Fatal(err)
	}
	if rows(off.ID) != 0 {
		t.Errorf("turning chief_extension off left the section row")
	}
	if got := rendered(t, s, 2); len(got) != 0 {
		t.Fatalf("after revoke and switch-off: %v; want none", got)
	}
	if _, _, err := s.SetChiefExtension(session(2), off.ID, true); err != nil {
		t.Fatal(err)
	}
	if got := rendered(t, s, 2); len(got) != 0 {
		t.Fatalf("turning it on again brought a section back: %v", got)
	}

	// Behind the store's back: the filter alone.
	byRevoke := approved("example-jay", "Jay alerts page the jay rota.")
	byCap := approved("example-kite", "Kite tasks are filed on the kite tracker.")
	dbtest.OwnerExec(t, dsn, `UPDATE plugins SET revoked_at = now() WHERE id = `+strconv.FormatInt(byRevoke.ID, 10))
	dbtest.OwnerExec(t, dsn, `UPDATE plugins SET capabilities = '{messages}', data_scope = 'example-kite' WHERE id = `+strconv.FormatInt(byCap.ID, 10))
	if rows(byRevoke.ID) != 1 || rows(byCap.ID) != 1 {
		t.Fatal("control: the rows the filter must hide are not there")
	}
	if got := rendered(t, s, 2); len(got) != 0 {
		t.Fatalf("a revoked plugin's or a capability-less plugin's row rendered: %v", got)
	}
}

// TestAnotherOwnersChiefSectionNeverRenders: user 2 and user 3 each have an
// approved section. Each owner's chief renders its own alone; user 3 cannot
// read, approve or overwrite user 2's (ErrNotFound / none), and row-level
// security shows user 3's principal no row of user 2's.
func TestAnotherOwnersChiefSectionNeverRenders(t *testing.T) {
	s, pool, _ := pgStore(t)
	const two, three = "Lark escalations go to the lark desk.", "Moth reports are kept in the moth archive."
	p2, p3 := mustGuide(t, s, 2, "example-lark"), mustGuide(t, s, 3, "example-moth")
	put(t, s, 2, p2.ID, two)
	put(t, s, 3, p3.ID, three)
	for uid, p := range map[int64]Plugin{2: p2, 3: p3} {
		c, _, err := s.ChiefInstructionsOf(session(uid), p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ApproveChiefInstructions(session(uid), p.ID, c.SHA); err != nil {
			t.Fatal(err)
		}
	}
	if got := rendered(t, s, 2); strings.Join(got, "|") != "example-lark: "+two {
		t.Errorf("user 2's chief renders %v; want user 2's own alone", got)
	}
	if got := rendered(t, s, 3); strings.Join(got, "|") != "example-moth: "+three {
		t.Errorf("user 3's chief renders %v; want user 3's own alone", got)
	}
	if _, ok, err := s.ChiefInstructionsOf(session(3), p2.ID); ok || err != nil {
		t.Errorf("user 3 read user 2's section: ok %v err %v", ok, err)
	}
	if _, err := s.ApproveChiefInstructions(session(3), p2.ID, hexSHA(two)); !errors.Is(err, ErrNotFound) {
		t.Errorf("user 3 approving user 2's section: %v; want ErrNotFound", err)
	}
	if _, _, err := s.PutChiefInstructions(plugin(3), p2.ID, "Nightjar text from the wrong owner."); !errors.Is(err, ErrNotFound) {
		t.Errorf("user 3's token writing user 2's plugin's section: %v; want ErrNotFound", err)
	}
	var n int
	if err := pool.QueryRow(session(3), `SELECT count(*) FROM plugin_chief_instructions WHERE plugin_id = $1`, p2.ID).Scan(&n); err != nil || n != 0 {
		t.Errorf("row-level security: user 3 sees %d of user 2's rows (%v); want 0", n, err)
	}
	if err := pool.QueryRow(session(2), `SELECT count(*) FROM plugin_chief_instructions WHERE plugin_id = $1`, p2.ID).Scan(&n); err != nil || n != 1 {
		t.Errorf("CONTROL: user 2 sees %d of its own rows (%v); want 1", n, err)
	}
}

// TestATeamChiefRendersTheTeamsSectionsNotAMembersPersonal: team 9141 owns a
// plugin with an approved section; user 2, a MANAGER of the team, has a
// personal one. The team's chief (owner 9141) renders the team's alone, and
// the member's chief the member's alone; the manager's own session does not
// even read the team's row (the table is personal: owner_isolation only).
func TestATeamChiefRendersTheTeamsSectionsNotAMembersPersonal(t *testing.T) {
	s, _, dsn := pgStore(t)
	const team = int64(9141)
	dbtest.OwnerExec(t, dsn, `INSERT INTO users (id, role, kind, display_name) OVERRIDING SYSTEM VALUE VALUES (9141, 'member', 'team', 'sections-team')
		ON CONFLICT (id) DO NOTHING`)
	dbtest.OwnerExec(t, dsn, `INSERT INTO team_members (team_id, user_id, role, added_by) VALUES (9141, 2, 'manager', 1) ON CONFLICT DO NOTHING`)
	t.Cleanup(func() {
		dbtest.OwnerExec(t, dsn, `DELETE FROM plugins WHERE owner_id = 9141`)
		dbtest.OwnerExec(t, dsn, `DELETE FROM team_members WHERE team_id = 9141`)
		dbtest.OwnerExec(t, dsn, `DELETE FROM audit_log WHERE user_id = 9141 OR team_id = 9141`)
		dbtest.OwnerExec(t, dsn, `DELETE FROM users WHERE id = 9141`)
	})
	teamAs := func(via authz.Via) context.Context {
		return authz.WithPrincipal(context.Background(), authz.Principal{UserID: team, Team: true, Role: authz.RoleMember, Via: via})
	}
	const teamText, personal = "Osprey incidents are filed with the osprey tools.", "Pelican drafts wait in the pelican tray."
	_, tp, err := s.CreatePlugin(teamAs(authz.ViaTeamAdmin), NewPlugin{Name: "example-osprey", Label: "Example team guide", Capabilities: []Capability{CapChiefExtension}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PutChiefInstructions(teamAs(authz.ViaPlugin), tp.ID, teamText); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApproveChiefInstructions(teamAs(authz.ViaTeamAdmin), tp.ID, hexSHA(teamText)); err != nil {
		t.Fatal(err)
	}
	mp := mustGuide(t, s, 2, "example-pelican")
	put(t, s, 2, mp.ID, personal)
	if _, err := s.ApproveChiefInstructions(session(2), mp.ID, hexSHA(personal)); err != nil {
		t.Fatal(err)
	}
	if got := rendered(t, s, team); strings.Join(got, "|") != "example-osprey: "+teamText {
		t.Errorf("the team's chief renders %v; want the team's own section alone", got)
	}
	if got := rendered(t, s, 2); strings.Join(got, "|") != "example-pelican: "+personal {
		t.Errorf("the member's chief renders %v; want the member's own alone", got)
	}
	ms, err := authz.NewMemberships([]authz.Membership{{Team: team, Role: authz.TeamManager}})
	if err != nil {
		t.Fatal(err)
	}
	mgr := authz.WithPrincipal(context.Background(), authz.Principal{UserID: 2, Role: authz.RoleMember, Via: authz.ViaSession, Teams: ms})
	if _, ok, err := s.ChiefInstructionsOf(mgr, tp.ID); ok || err != nil {
		t.Errorf("the manager's own session read the team's section: ok %v err %v", ok, err)
	}
	if _, err := s.ApproveChiefInstructions(mgr, tp.ID, hexSHA(teamText)); !errors.Is(err, ErrNotFound) {
		t.Errorf("the manager's own session approving the team's section: %v; want ErrNotFound", err)
	}
}

// TestTheChiefSectionCapIsFourKiB: 4096 bytes are stored and 4097 refused —
// counted in BYTES, so 4095 ASCII bytes and one two-byte character are
// refused too. Blank text, a control character and invalid UTF-8 are
// refused; a tab and a line feed are not. A plugin without chief_extension
// stores nothing.
func TestTheChiefSectionCapIsFourKiB(t *testing.T) {
	s, _, _ := pgStore(t)
	p := mustGuide(t, s, 2, "example-rook")
	if _, _, err := s.PutChiefInstructions(plugin(2), p.ID, strings.Repeat("r", 4096)); err != nil {
		t.Fatalf("4096 bytes: %v", err)
	}
	for _, bad := range []struct{ why, text string }{
		{"4097 bytes", strings.Repeat("s", 4097)},
		{"4095 bytes and a two-byte character", strings.Repeat("t", 4095) + "é"},
		{"blank", " \n\t "},
		{"a control character", "Rook text\x07 with a bell"},
		{"invalid UTF-8", "Rook text \xff"},
	} {
		if _, _, err := s.PutChiefInstructions(plugin(2), p.ID, bad.text); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v; want ErrInvalid", bad.why, err)
		}
	}
	if _, _, err := s.PutChiefInstructions(plugin(2), p.ID, "Rook steps:\n\t1. read the rook page"); err != nil {
		t.Errorf("a tab and a line feed: %v", err)
	}
	_, msgs := mustCreate(t, s, 2, "example-sparrow")
	if _, _, err := s.PutChiefInstructions(plugin(2), msgs.ID, "Sparrow text with no capability."); !errors.Is(err, ErrNotFound) {
		t.Errorf("a plugin without chief_extension: %v; want ErrNotFound", err)
	}
}
