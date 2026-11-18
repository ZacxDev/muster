package api

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/officers"
	"github.com/ZacxDev/muster/internal/officers/builtin"
)

// OFFICERS WORK ON COMPOSE (officers O-B), the parts that need the real
// stores: a host instance's claim through the mirror door marks its spawn
// claimed (what clears the unclaimed notice), and the /plugins form.

// TestAHostInstancesClaimThroughTheMirrorDoorMarksItsSpawn: the instance's
// real officer_claim, through the mirror door's binding, records its spawn's
// first claim — the input the unclaimed-instance notice clears on — once.
// The control: before the claim the spawn reads unclaimed.
func TestAHostInstancesClaimThroughTheMirrorDoorMarksItsSpawn(t *testing.T) {
	w := newOutboundWorldOn(t, true, true)
	st := w.srv.officerHostSpawns()
	if row, _ := st.HostSpawnByID(w.ctx, w.spawn.ID); row.ClaimedAt != nil {
		t.Fatalf("control: the spawn reads claimed before any claim: %+v", row)
	}
	if _, _, err := w.store.Enqueue(w.ctx, 1, builtin.NewItem{Officer: builtinOfficerID, Kind: "triage.look", SubjectKey: "claim:1"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if out := w.call(officers.ToolClaim, `{}`); !strings.Contains(out, `"claim:1"`) {
		t.Fatalf("the instance's claim: %s", out)
	}
	row, err := st.HostSpawnByID(w.ctx, w.spawn.ID)
	if err != nil || row.ClaimedAt == nil {
		t.Fatalf("the spawn after the instance's claim: %+v %v, want it marked claimed", row, err)
	}
	first := *row.ClaimedAt
	if _, _, err := w.store.Enqueue(w.ctx, 1, builtin.NewItem{Officer: builtinOfficerID, Kind: "triage.look", SubjectKey: "claim:2"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	w.call(officers.ToolClaim, `{}`)
	if row, _ := st.HostSpawnByID(w.ctx, w.spawn.ID); row.ClaimedAt == nil || !row.ClaimedAt.Equal(first) {
		t.Fatalf("a second claim moved the first claim's time: %v → %v", first, row.ClaimedAt)
	}
}

var mintedCodeRE = regexp.MustCompile(`<code id="minted-plugin-token"[^>]*>(mpl_[A-Za-z0-9_-]{43})</code>`)

// TestThePluginsFormDefaultsTheScopeShowsTheTokenAndTheBinding (the blind
// probe's /plugins friction): a plugin minted with no data scope takes its
// name, as the docs say (it was refused); the one-time token is TEXT the page
// carries — a <code>, with a Copy button — never a form field's value, which
// is not the page's text; and each card SAYS where its messages go — the
// bound officer, or your chief — in text, not only in the binding field's
// value (it read as unbound after a save). REGRESSION: red at origin/main on
// all three.
func TestThePluginsFormDefaultsTheScopeShowsTheTokenAndTheBinding(t *testing.T) {
	w := newOutboundWorld(t, true)
	rec := w.do("POST", "/plugins", url.Values{"name": {"example-mint"}, "label": {"Mint"}, "data_scope": {""}}.Encode(), w.operatorForm)
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("mint with no data scope: %d\n%s", rec.Code, body)
	}
	m := mintedCodeRE.FindStringSubmatch(body)
	if m == nil || strings.Contains(body, `<input id="minted-plugin-token"`) || !strings.Contains(body, "data-copy-plugin-token") {
		t.Fatalf("the minted token is not shown as text with a Copy button:\n%s", body)
	}
	var scope string
	if err := w.f.pool.QueryRow(authz.System(w.ctx), `SELECT data_scope FROM plugins WHERE name = 'example-mint' AND owner_id = 1`).Scan(&scope); err != nil || scope != "example-mint" {
		t.Fatalf("the minted plugin's data scope %q (%v), want its name", scope, err)
	}
	// The page: each card says where its messages go.
	page := w.do("GET", "/plugins", "", func(r *http.Request) {
		r.Header.Set("Cookie", sessionCookieFor(w.srv))
	}).Body.String()
	bound := regexp.MustCompile(`data-plugin-delivers-to>Delivers to <span[^>]*>officer:` + regexp.QuoteMeta(builtinOfficerID) + `</span>`)
	chief := regexp.MustCompile(`data-plugin-delivers-to>Delivers to <span[^>]*>your chief</span>`)
	if !bound.MatchString(page) || !chief.MatchString(page) {
		t.Fatalf("the cards do not say where messages go (bound officer and your chief):\n%s", page)
	}
	if strings.Contains(page, m[1]) {
		t.Fatal("the minted token is rendered again on a later page load")
	}
}

// TestTheWakeLoopsKickoffListsAGrantedPostDraft: the wake loop's kickoff
// (officers.Config.Kickoff = officerKickoff) reads a builtin officer's type
// grants when it starts an instance: officer_post_draft is in the first
// message's tool list exactly when the type grants outbound_drafts. The
// blind probe's marketing instance was told only the six queue tools.
func TestTheWakeLoopsKickoffListsAGrantedPostDraft(t *testing.T) {
	for _, grant := range []bool{true, false} {
		w := newOutboundWorldOn(t, grant, true)
		o, err := w.srv.officerRT.reg.Get(builtinOfficerID)
		if err != nil {
			t.Fatal(err)
		}
		k := w.srv.officerKickoff(w.ctx, o, 1)
		if listed := strings.Contains(k, "officer_release, "+officers.ToolPostDraft+"."); listed != grant {
			t.Fatalf("grant %v: officer_post_draft listed %v in\n%s", grant, listed, k)
		}
	}
}
