package api

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE COMMAND PALETTE'S PREVIOUS FOCUS TITLES on a real Postgres: the entries
// the sidebar and the session switcher draw (agentsListEntries) carry each
// session's previous focus titles — its current one left out — and a viewer
// is never handed another owner's, not even for a mirrored session under the
// same Claude session key. Synthetic fixtures throughout.

// paletteFixture writes, for the operator (user 1) and a member, an agent's
// focus history and the shared mirrored session's (focusSID, which the fake
// mirror lists to both), each an earlier title then the current one.
func paletteFixture(t *testing.T) *attnFixture {
	t.Helper()
	f := newAttnFixture(t)
	store := focus.NewPG(f.pool)
	// focusSID is the package's shared Claude session id, and other tests
	// leave focus under it: cleared before (so the exact-title assertions see
	// only this fixture's rows) and after.
	clear := func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agent_focus_history WHERE subject_kind = 'mirror' AND subject_key = $1`, focusSID)
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM agent_focus WHERE subject_kind = 'mirror' AND subject_key = $1`, focusSID)
	}
	clear()
	t.Cleanup(clear)
	for _, w := range []struct {
		uid    int64
		sub    focus.Subject
		titles []string
	}{
		{1, focus.AgentSubject(f.op.ID), []string{"Drafting the operator launch plan", "Reviewing the operator budget"}},
		{f.member, focus.AgentSubject(f.worker.ID), []string{"Auditing the member invoices", "Filing the member expenses"}},
		{1, focus.MirrorSubject(focusSID), []string{"Operator window earlier work", "Operator window current work"}},
		{f.member, focus.MirrorSubject(focusSID), []string{"Member window earlier work", "Member window current work"}},
	} {
		for _, ti := range w.titles {
			if _, err := store.Upsert(asUser(w.uid), w.sub, focus.Write{Title: ti, Description: "d", Source: focus.SourceSummarizer}); err != nil {
				t.Fatalf("upsert %q for %d: %v", ti, w.uid, err)
			}
		}
	}
	return f
}

// prevByEntry is each entry's previous titles, by "agent:<name>" or
// "mirror:<window>".
func prevByEntry(cards []ui.AgentCardView, mirrors []ui.MirrorCardView) map[string]string {
	out := map[string]string{}
	for _, c := range cards {
		out["agent:"+c.Name] = strings.Join(c.PrevFocus, "|")
	}
	for _, m := range mirrors {
		out["mirror:"+m.Window] = strings.Join(m.PrevFocus, "|")
	}
	return out
}

// TestTheEntriesCarryTheirPreviousFocusTitles: an agent's and a mirrored
// session's earlier focus title reach their entry, the current one does not
// (it is the title already), and the sidebar's rows carry them as
// data-switcher-prev for the palette. Red at origin/main: no PrevFocus, no
// attribute.
func TestTheEntriesCarryTheirPreviousFocusTitles(t *testing.T) {
	f := paletteFixture(t)
	cards, mirrors, err := f.srv.agentsListEntries(asUser(1))
	if err != nil {
		t.Fatal(err)
	}
	got := prevByEntry(cards, mirrors)
	if g := got["agent:"+f.op.Name]; g != "Drafting the operator launch plan" {
		t.Errorf("the operator's agent's previous titles = %q", g)
	}
	if g := got["mirror:@1"]; g != "Operator window earlier work" {
		t.Errorf("the mirrored session's previous titles = %q", g)
	}
	if g := got["mirror:@2"]; g != "" {
		t.Errorf("the window that reports no Claude session has previous titles %q", g)
	}
	rows := navRowTags(f.page(t, ui.NavAgentsPath))
	if tag := rows[agentNavID(f.op.ID)]; !strings.Contains(tag, `data-switcher-prev="Drafting the operator launch plan"`) ||
		!strings.Contains(tag, `data-switcher-title="Reviewing the operator budget"`) {
		t.Errorf("the agent's sidebar row does not carry its previous title for the palette: %s", tag)
	}
}

// 🔴 TestNoViewerIsHandedAnotherOwnersFocusTitles: the member's entries carry
// the member's titles and NONE of the operator's — not its agent's (whose id
// the member's list never names) and not the operator's history of the
// mirrored session the member's list DOES name, under the same key. CONTROL:
// the operator's own entries carry the operator's titles (so the strings the
// member must not see are really there to be leaked).
func TestNoViewerIsHandedAnotherOwnersFocusTitles(t *testing.T) {
	f := paletteFixture(t)
	opCards, opMirrors, err := f.srv.agentsListEntries(asUser(1))
	if err != nil {
		t.Fatal(err)
	}
	if op := fmt.Sprint(prevByEntry(opCards, opMirrors)); !strings.Contains(op, "Operator window earlier work") || !strings.Contains(op, "Drafting the operator launch plan") {
		t.Fatalf("CONTROL: the operator's own entries lack its titles: %s", op)
	}
	cards, mirrors, err := f.srv.agentsListEntries(asUser(f.member))
	if err != nil {
		t.Fatal(err)
	}
	got := prevByEntry(cards, mirrors)
	if g := got["agent:"+f.worker.Name]; g != "Auditing the member invoices" {
		t.Errorf("the member's agent's previous titles = %q (instrument: the member's read works)", g)
	}
	// The fake mirror lists the host's windows to their token's owner only, so
	// the member's list names no mirrored session; the shared key is handed
	// to the server's read directly — as an entry the member's list WOULD
	// draw if its own host reported that Claude session — and the operator's
	// whole agent list too, ids the member's list never names.
	shared := []ui.MirrorCardView{{Host: "h", Session: "main", Window: "@9", FocusKey: focusSID,
		Focus: &ui.FocusView{Title: "Member window current work"}}}
	hostile := append([]ui.AgentCardView{}, opCards...)
	for i := range hostile {
		hostile[i].PrevFocus, hostile[i].Focus = nil, nil
	}
	f.srv.withPreviousTitles(asUser(f.member), hostile, shared)
	if g := strings.Join(shared[0].PrevFocus, "|"); g != "Member window earlier work" {
		t.Errorf("the member's read of the shared-key session = %q, want only the member's own earlier title", g)
	}
	for _, c := range hostile {
		if len(c.PrevFocus) != 0 {
			t.Errorf("the member read the operator's agent %s's titles: %q", c.Name, c.PrevFocus)
		}
	}
	all := fmt.Sprintf("%+v %+v %+v", cards, mirrors, shared)
	for _, leak := range []string{"operator", "Operator"} {
		if strings.Contains(all, leak) {
			t.Errorf("the member's entries carry %q — another owner's focus title:\n%s", leak, all)
		}
	}
}
