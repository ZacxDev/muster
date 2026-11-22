package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
)

// THE PALETTE'S SEARCH OF ASKS AND PAGES (palette_find.go), on a real
// Postgres: GET /ui/palette/find?q= as the signed-in person.
//
// 🔴 EVERY EXPECTATION IS A LITERAL — the route, the JSON keys, the hrefs, the
// order — and this file names no symbol the search added, so it compiles on
// the commit before it and is RED there by its assertions (the route is a 404),
// not by a build error. Fixture words are pairwise distinct per test.

// paletteFind is GET /ui/palette/find?q=raw as f.session: the status and the
// answer as "ask:<id>" / "page:<id>" lists, plus the decoded answer.
func (f *pageFixture) paletteFind(t *testing.T, raw string) (int, []string, []string, map[string][]map[string]any) {
	t.Helper()
	w := f.call(t, http.MethodGet, "/ui/palette/find?q="+url.QueryEscape(raw), nil, f.operator())
	if w.Code != http.StatusOK {
		return w.Code, nil, nil, nil
	}
	var out map[string][]map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("?q=%q: not JSON: %v\n%s", raw, err, w.Body.String())
	}
	var asks, pages []string
	for _, a := range out["asks"] {
		asks = append(asks, "ask:"+strconv.FormatInt(pnum(a["id"]), 10))
	}
	for _, p := range out["pages"] {
		pages = append(pages, "page:"+strconv.FormatInt(pnum(p["id"]), 10))
	}
	return w.Code, asks, pages, out
}

// paletteAskAs files an open ask for owner, asked by agent agentID.
func (f *pageFixture) paletteAskAs(t *testing.T, owner, agentID int64, question string, options ...string) attention.Ask {
	t.Helper()
	a, err := attention.NewPG(f.pool).Create(f.as(owner, authz.ViaSession), attention.New{
		Asker: attention.Asker{AgentID: agentID, Label: "palette-asker"}, Question: question, Options: options})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func kindIDs(prefix string, xs ...int64) []string {
	out := []string{}
	for _, x := range xs {
		out = append(out, prefix+":"+strconv.FormatInt(x, 10))
	}
	return out
}

func joined(xs []string) string { return strings.Join(xs, ",") }

// TestThePaletteFindsOnlyTheCallersAsksAndPages: the same words in another
// owner's ask and page find only the caller's own, by text AND by id; another
// owner's id reads exactly as an id nobody has. Each hit links to the page
// that opens it.
func TestThePaletteFindsOnlyTheCallersAsksAndPages(t *testing.T) {
	f := newPageFixture(t)
	mine := f.paletteAskAs(t, f.owner, f.agent.ID, "Rotate the kestrel keys?")
	theirs := f.paletteAskAs(t, f.other, f.otherAgent.ID, "Rotate the kestrel keys?")
	myPage := f.create(t, f.agent, "Kestrel launch plan", `[{"type":"markdown","text":"mine"}]`)
	theirPage := f.create(t, f.otherAgent, "Kestrel launch plan", `[{"type":"markdown","text":"theirs"}]`)

	code, asks, pages, out := f.paletteFind(t, "kestrel")
	if code != http.StatusOK {
		t.Fatalf("?q=kestrel: %d", code)
	}
	if got, want := joined(asks), joined(kindIDs("ask", mine.ID)); got != want {
		t.Errorf("asks %s, want only the caller's %s (another owner's is %d)", got, want, theirs.ID)
	}
	if got, want := joined(pages), joined(kindIDs("page", myPage)); got != want {
		t.Errorf("pages %s, want only the caller's %s (another owner's is %d)", got, want, theirPage)
	}
	if len(out["asks"]) == 1 {
		a := out["asks"][0]
		if a["href"] != "/attention/asks/"+strconv.FormatInt(mine.ID, 10) || a["title"] != "Rotate the kestrel keys?" || a["state"] != "Open" || a["open"] != true || a["byId"] != false {
			t.Errorf("the ask hit: %v", a)
		}
	}
	if len(out["pages"]) == 1 {
		p := out["pages"][0]
		if p["href"] != "/pages/"+strconv.FormatInt(myPage, 10) || p["title"] != "Kestrel launch plan" || p["byId"] != false {
			t.Errorf("the page hit: %v", p)
		}
	}

	// By id: the caller's own (the control) — and another owner's, which
	// finds nothing, exactly as an id nobody has.
	if _, asks, _, _ := f.paletteFind(t, "ask "+strconv.FormatInt(mine.ID, 10)); joined(asks) != joined(kindIDs("ask", mine.ID)) {
		t.Errorf("control: the caller's own ask by id: %v", asks)
	}
	if _, _, pages, _ := f.paletteFind(t, "page "+strconv.FormatInt(myPage, 10)); joined(pages) != joined(kindIDs("page", myPage)) {
		t.Errorf("control: the caller's own page by id: %v", pages)
	}
	for _, q := range []string{"#" + strconv.FormatInt(theirs.ID, 10), "ask " + strconv.FormatInt(theirs.ID, 10), "page " + strconv.FormatInt(theirPage, 10), strconv.FormatInt(theirPage, 10)} {
		_, asks, pages, _ := f.paletteFind(t, q)
		if len(asks) != 0 || len(pages) != 0 {
			t.Errorf("?q=%q (another owner's): asks %v pages %v, want nothing", q, asks, pages)
		}
	}
	w1 := f.call(t, http.MethodGet, "/ui/palette/find?q="+url.QueryEscape("#"+strconv.FormatInt(theirs.ID, 10)), nil, f.operator())
	w2 := f.call(t, http.MethodGet, "/ui/palette/find?q="+url.QueryEscape("#"+strconv.FormatInt(theirs.ID+1_000_000, 10)), nil, f.operator())
	if w1.Code != w2.Code || w1.Body.String() != w2.Body.String() {
		t.Errorf("another owner's ask id reads differently from a missing one: %d %s / %d %s", w1.Code, w1.Body, w2.Code, w2.Body)
	}

	// The other owner, signed in, finds theirs and not the caller's.
	f.session = f.other
	if _, asks, pages, _ := f.paletteFind(t, "kestrel"); joined(asks) != joined(kindIDs("ask", theirs.ID)) || joined(pages) != joined(kindIDs("page", theirPage)) {
		t.Errorf("as the other owner: asks %v pages %v", asks, pages)
	}
}

// TestThePaletteFindsATeamsPageButNeverATeamsAsk: row-level security lets a
// team's member READ the team's asks and pages (the control: the member's own
// read of the team ask succeeds). The palette lists the team's page — the
// member's Pages screen lists it too — but never the team's ask: an ask is the
// caller's OWN (attention.Search names the owner; askHasPage). An outsider
// finds neither.
func TestThePaletteFindsATeamsPageButNeverATeamsAsk(t *testing.T) {
	f := newTeamPageFixture(t)
	page := f.teamPage(t, "Curlew roadmap")
	attn := attention.NewPG(f.pool)
	ask, err := attn.Create(f.asTeam(f.teamA), attention.New{Asker: attention.Asker{AgentID: f.teamAgent.ID, Label: "team agent"}, Question: "Curlew budget sign-off?"})
	if err != nil {
		t.Fatal(err)
	}
	// The member's principal as a request door builds it: with the team
	// memberships row-level security reads.
	acct, err := f.users.Account(authz.System(context.Background()), f.creator)
	if err != nil {
		t.Fatal(err)
	}
	member := authz.WithPrincipal(context.Background(), authz.Principal{UserID: f.creator, Role: acct.Role, Via: authz.ViaSession, Teams: acct.Teams})
	if got, err := attn.Get(member, ask.ID); err != nil || got.ID != ask.ID {
		t.Fatalf("control: the member cannot read the team's ask %d owned by %d (team %d) (%v): the owner rule below would be untested", ask.ID, ask.Owner, f.teamA, err)
	}
	n := strconv.FormatInt(ask.ID, 10)
	f.session = f.creator
	_, asks, pages, _ := f.paletteFind(t, "curlew")
	if len(asks) != 0 {
		t.Errorf("the member finds the team's ask by its words: %v", asks)
	}
	if joined(pages) != joined(kindIDs("page", page)) {
		t.Errorf("the member's pages %v, want the team's page %d", pages, page)
	}
	for _, q := range []string{n, "#" + n, "ask " + n} {
		if _, asks, _, _ := f.paletteFind(t, q); len(asks) != 0 {
			t.Errorf("the member's ?q=%q finds the team's ask: %v", q, asks)
		}
	}
	f.session = f.outsider
	if _, asks, pages, _ := f.paletteFind(t, "curlew"); len(asks) != 0 || len(pages) != 0 {
		t.Errorf("an outsider finds asks %v pages %v", asks, pages)
	}
	if _, _, pages, _ := f.paletteFind(t, "page "+strconv.FormatInt(page, 10)); len(pages) != 0 {
		t.Errorf("an outsider finds the team's page by id: %v", pages)
	}
}

// TestThePaletteRanksOpenAsksFirstAndAnIdAlone: open asks above answered and
// dismissed ones, and within each those whose QUESTION holds the words above
// those matched elsewhere (an option), newest first among equals. An id is
// looked up by id ONLY — never as text, so a question holding the number does
// not appear — whatever its length.
func TestThePaletteRanksOpenAsksFirstAndAnIdAlone(t *testing.T) {
	f := newPageFixture(t)
	attn := attention.NewPG(f.pool)
	answeredQ := f.paletteAskAs(t, f.owner, f.agent.ID, "Ship the plover build?")
	// The question match is OLDER than the option match, so newest-first
	// alone would put the option first: the question rank must win.
	openQ := f.paletteAskAs(t, f.owner, f.agent.ID, "Retire the plover queue?")
	openOpt := f.paletteAskAs(t, f.owner, f.agent.ID, "Which branch?", "plover", "main")
	dismissedQ := f.paletteAskAs(t, f.owner, f.agent.ID, "Rename plover?")
	if _, err := attn.Answer(f.as(f.owner, authz.ViaSession), answeredQ.ID, "yes"); err != nil {
		t.Fatal(err)
	}
	if _, err := attn.Cancel(f.as(f.owner, authz.ViaSession), dismissedQ.ID); err != nil {
		t.Fatal(err)
	}
	// Newest first among equals: dismissedQ is newer than answeredQ.
	_, asks, _, out := f.paletteFind(t, "PLOVER")
	if got, want := joined(asks), joined(kindIDs("ask", openQ.ID, openOpt.ID, dismissedQ.ID, answeredQ.ID)); got != want {
		t.Fatalf("asks %s, want %s (open: question, then option; then closed newest first)", got, want)
	}
	states := []string{}
	for _, a := range out["asks"] {
		states = append(states, a["state"].(string))
	}
	if got := strings.Join(states, "|"); got != "Open|Open|Dismissed|Answered" {
		t.Errorf("states %s", got)
	}

	// An id, bare or marked, finds that ask alone, marked as found by id —
	// though another ask's question holds the same digits.
	n := strconv.FormatInt(answeredQ.ID, 10)
	holder := f.paletteAskAs(t, f.owner, f.agent.ID, "Revert build "+n+"?")
	for _, q := range []string{n, "#" + n, "ask " + n, "Ask #" + n} {
		_, asks, _, out := f.paletteFind(t, q)
		if joined(asks) != joined(kindIDs("ask", answeredQ.ID)) || out["asks"][0]["byId"] != true {
			t.Errorf("?q=%q: %v, want only ask %s by id (not %d, whose question holds it)", q, asks, n, holder.ID)
		}
	}
}

// TestThePaletteFindHoldsItsLengthGuards: text below three runes or above two
// hundred finds nothing, though the words are there to find; an id passes
// whatever its length.
func TestThePaletteFindHoldsItsLengthGuards(t *testing.T) {
	f := newPageFixture(t)
	long := strings.Repeat("dunlin ", 40) // 280 runes
	a := f.paletteAskAs(t, f.owner, f.agent.ID, long)
	for q, want := range map[string]string{
		"du":                       "",
		"dun":                      joined(kindIDs("ask", a.ID)),
		string([]rune(long)[:200]): joined(kindIDs("ask", a.ID)),
		string([]rune(long)[:201]): "",
	} {
		if _, asks, _, _ := f.paletteFind(t, q); joined(asks) != want {
			t.Errorf("?q=(%d runes) %.12q…: asks %v, want [%s]", len([]rune(q)), q, asks, want)
		}
	}
	// An id shorter than the text floor is still an id (parsePaletteFind's
	// own table, palette_find_test.go, pins one-digit ids).
	p := f.create(t, f.agent, "Sanderling notes", `[{"type":"markdown","text":"x"}]`)
	if _, _, pages, _ := f.paletteFind(t, "#"+strconv.FormatInt(p, 10)); joined(pages) != joined(kindIDs("page", p)) {
		t.Errorf("?q=#%d: pages %v", p, pages)
	}
}

// TestThePaletteFindCutsEachKindAtFive: seven asks and seven pages hold the
// words; five of each are answered.
func TestThePaletteFindCutsEachKindAtFive(t *testing.T) {
	f := newPageFixture(t)
	for i := 0; i < 7; i++ {
		f.paletteAskAs(t, f.owner, f.agent.ID, "Tune godwit cache "+strconv.Itoa(i)+"?")
		f.create(t, f.agent, "Godwit report "+strconv.Itoa(i), `[{"type":"markdown","text":"x"}]`)
	}
	if _, asks, pages, _ := f.paletteFind(t, "godwit"); len(asks) != 5 || len(pages) != 5 {
		t.Errorf("asks %d pages %d, want 5 and 5", len(asks), len(pages))
	}
}
