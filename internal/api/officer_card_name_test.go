package api

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
	"github.com/ZacxDev/muster/internal/plugins"
)

// NO OFFICER ID ON A SHARED CARD (plugin_outbound.go officerWords): an
// officer's outbound draft and its outreach batch name the officer by its
// display name in the top layer — the officer's id (here one shaped like
// the e2e officers', "e2e-mkt-<run>:main") is in none of the visible text.

const cardOfficerID = "e2e-mkt-7f3a2c:main"

// cardDraft is a pending outbound draft of cardOfficerID of kind on X.
func cardDraft(id int64, kind string) plugins.Draft {
	officer, venue, plugin := cardOfficerID, plugins.VenueX, int64(9)
	ref := "2122495152350453825"
	return plugins.Draft{ID: id, OwnerID: 1, PluginID: &plugin, Officer: &officer, Venue: &venue, TargetKind: &kind, TargetRef: &ref,
		Status: plugins.DraftPending, CreatedAt: time.Date(2000, 3, 4, 10, 0, 0, 0, time.UTC), ExpiresAt: time.Date(2000, 3, 11, 0, 0, 0, 0, time.UTC),
		Body: "Hello."}
}

// TestAnOfficersCardsNameItNotItsID: with a display name, the draft's card
// line and the batch card say it; with none, they say "your officer"; the id
// is in the visible text of neither, either way.
func TestAnOfficersCardsNameItNotItsID(t *testing.T) {
	for _, c := range []struct{ display, want string }{{"Marketing for Acme", "Marketing for Acme"}, {"", "your officer"}} {
		post := plugins.DraftCard{Draft: cardDraft(41, plugins.TargetPost), OfficerDisplay: c.display}
		line := outboundAttentionText(post)
		if !strings.Contains(line, " by "+c.want) || strings.Contains(line, cardOfficerID) || strings.Contains(line, "officer e2e") {
			t.Errorf("display %q: the draft's card line %q; want %q and no id", c.display, line, "by "+c.want)
		}
		follow := plugins.DraftCard{Draft: cardDraft(42, plugins.TargetFollow), OfficerDisplay: c.display}
		_, batches, _ := groupBatches([]plugins.DraftCard{follow})
		if len(batches) != 1 {
			t.Fatalf("display %q: %d batches, want 1", c.display, len(batches))
		}
		top := visibleText(outreachBatchCardHTML(batches[0]))
		if !strings.Contains(top, "Follows and likes by "+c.want) || strings.Contains(top, cardOfficerID) || strings.Contains(top, "e2e-mkt") {
			t.Errorf("display %q: the batch card's top layer names the id or not %q:\n%s", c.display, c.want, top)
		}
	}
}

// TestTheCardsTakeTheOfficersDeclaredName: the server resolves a card's
// display name from the officer's declared type name (what its page heads it
// with); an officer muster does not declare is left "" (so "your officer"),
// never its id; a reply draft (no officer) is untouched.
func TestTheCardsTakeTheOfficersDeclaredName(t *testing.T) {
	s := New(nil, AuthConfig{UIPassword: testUIPassword, HookToken: testHookToken}, log.New(&bytes.Buffer{}, "", 0))
	s.officerRT = &officerRuntime{reg: officers.NewRegistry(nil), loaded: true, declared: map[string]declaredState{
		declKey(1, cardOfficerID): {row: builtin.DeclaredRow{Owner: 1, DefInfo: builtin.DefInfo{Officer: cardOfficerID}, TypeName: "Marketing for Acme"}},
	}}
	other, unknown := cardDraft(43, plugins.TargetPost), "e2e-mkt-unknown:main"
	other.Officer = &unknown
	cards := []plugins.DraftCard{{Draft: cardDraft(41, plugins.TargetPost)}, {Draft: other}, {Draft: plugins.Draft{ID: 44, OwnerID: 1}}}
	s.withDraftOfficerNames(cards)
	if cards[0].OfficerDisplay != "Marketing for Acme" || cards[1].OfficerDisplay != "" || cards[2].OfficerDisplay != "" {
		t.Fatalf("display names: %q, %q, %q; want the declared name, then none, none", cards[0].OfficerDisplay, cards[1].OfficerDisplay, cards[2].OfficerDisplay)
	}
	if line := outboundAttentionText(cards[1]); strings.Contains(line, unknown) {
		t.Fatalf("an undeclared officer's card line names its id: %q", line)
	}
}
