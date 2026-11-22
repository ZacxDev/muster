package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE ASKS PAGE'S FILTER CHIPS (asks_filter.go), on a real Postgres: the
// chips a page draws, what one tap keeps on each tab and in a search, that
// the address round-trips with ?q=, and that no other owner's officer is ever
// a chip or a result.
//
// 🔴 RED at the base: no chip row; ?type= and ?from=officer:… are ignored and
// the page lists everything.

// filtFix is the queue the tests read: the operator's marketing officer's
// question (one open, one answered), its outbound draft and the step-up card
// a session filed for that draft, another officer's question, an agent's
// question and a mirrored session's; and a MEMBER's own marketing officer
// (the same id) and sales officer, each with a question and the sales one a
// draft.
type filtFix struct {
	*attnFixture
	mktAsk, mktAnswered, mktCard, productAsk, agentAsk, sessionAsk int64
	mktDraft                                                       int64
	memberMkt, memberSales, memberDraft                            int64
}

const filtMarketing = "marketing:acme"

// filtMkt is the operator's marketing officer as a source key (its owner, 1,
// in it: asks_filter.go).
const filtMkt = "officer:1:" + filtMarketing

// mine is got restricted to the fixture's own asks: the package's database
// is shared with every other test in it, whose rows may still be there.
func (f *filtFix) mine(got map[int64]bool) map[int64]bool {
	out := map[int64]bool{}
	for _, id := range []int64{f.mktAsk, f.mktAnswered, f.mktCard, f.productAsk, f.agentAsk, f.sessionAsk, f.memberMkt, f.memberSales} {
		if got[id] {
			out[id] = true
		}
	}
	return out
}

func newFiltFix(t *testing.T, more ...func(*Extensions, *pgxpool.Pool)) *filtFix {
	t.Helper()
	f := &filtFix{attnFixture: newAttnFixture(t, more...)}
	sys := authz.System(context.Background())
	mk := func(owner int64, n attention.New) int64 {
		t.Helper()
		a, err := f.attn.Create(asUser(owner), n)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = f.pool.Exec(sys, `DELETE FROM attention_asks WHERE id = $1`, a.ID) })
		return a.ID
	}
	officer := func(id string) attention.Asker { return attention.Asker{System: true, Label: officerAskLabel(id)} }
	draft := func(owner, agent int64, officer, body string) int64 {
		t.Helper()
		var plugin, id int64
		name := "filt-x-" + strconv.FormatInt(owner, 10) + "-" + strconv.FormatInt(agent, 10)
		if err := f.pool.QueryRow(sys, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope) VALUES ($1, $2, 'X', md5($2) || md5($2), $2) RETURNING id`,
			owner, name).Scan(&plugin); err != nil {
			t.Fatal(err)
		}
		if err := f.pool.QueryRow(sys, `INSERT INTO plugin_reply_drafts (owner_id, agent_id, agent_name, body, expires_at, plugin_id, venue, target_kind, officer)
			VALUES ($1, $2, 'a', $3, now() + interval '1 hour', $4, 'x', 'post', $5) RETURNING id`, owner, agent, body, plugin, officer).Scan(&id); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = f.pool.Exec(sys, `DELETE FROM plugin_reply_drafts WHERE id = $1`, id)
			_, _ = f.pool.Exec(sys, `DELETE FROM plugins WHERE id = $1`, plugin)
		})
		return id
	}
	f.mktAnswered = mk(1, attention.New{Asker: officer(filtMarketing), Question: "Use the blue banner for the launch post?", Options: []string{"Yes", "No"}})
	if _, err := f.attn.Answer(asUser(1), f.mktAnswered, "Yes"); err != nil {
		t.Fatal(err)
	}
	f.productAsk = mk(1, attention.New{Asker: officer("product:acme"), Question: "Ship the onboarding checklist this week?"})
	f.agentAsk = f.ask(t, f.opTok, "Which release should the changelog start from?")
	f.sessionAsk = f.mirrorAsk(t, "Publish the release notes now?").ID
	f.mktAsk = mk(1, attention.New{Asker: officer(filtMarketing), Question: "Approve the launch tagline?", Options: []string{"Approve", "Rewrite"}})
	f.mktDraft = draft(1, f.op.ID, filtMarketing, "We launched today.")
	f.mktCard = mk(1, attention.New{Asker: attention.Asker{System: true, Label: "step-up"}, Options: []string{"Confirm", "Refuse"},
		StepUp: &attention.StepUpCard{Tool: "plugin_draft_decide", Title: "Post to X? · Draft #" + strconv.FormatInt(f.mktDraft, 10), Why: "Launch announcement",
			Asker: "work:1", Preview: "We launched today.", Venue: "X", Platform: "x", Target: "post", DraftID: f.mktDraft, Details: "A session asks to approve the draft."}})
	f.memberMkt = mk(f.member, attention.New{Asker: officer(filtMarketing), Question: "MEMBER: approve their tagline?"})
	f.memberSales = mk(f.member, attention.New{Asker: officer("sales:zeta"), Question: "MEMBER: call the lead?"})
	f.memberDraft = draft(f.member, f.worker.ID, "sales:zeta", "MEMBER: the sales post.")
	return f
}

// pageAs is GET path as user (the handler, as requireSession would run it).
func (f *filtFix) pageAs(t *testing.T, user int64, path string) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil).WithContext(asUser(user))
	w := httptest.NewRecorder()
	f.srv.handleAttentionPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s as %d: %d %s", path, user, w.Code, w.Body)
	}
	return w.Body.String()
}

// pageBetween is page's part between the element with id from and the next one
// with id to ("" for the end).
func pageBetween(page, from, to string) string {
	i := strings.Index(page, `id="`+from+`"`)
	if i < 0 {
		return ""
	}
	page = page[i:]
	if to != "" {
		if j := strings.Index(page, `id="`+to+`"`); j >= 0 {
			page = page[:j]
		}
	}
	return page
}

// idsIn is the ids of the elements whose id is prefix<N> in s.
func idsIn(s, prefix string) map[int64]bool {
	out := map[int64]bool{}
	for _, m := range regexp.MustCompile(`id="`+regexp.QuoteMeta(prefix)+`(\d+)"`).FindAllStringSubmatch(s, -1) {
		id, _ := strconv.ParseInt(m[1], 10, 64)
		out[id] = true
	}
	return out
}

// chipRe is one chip link: its kind, key, active state and label.
var chipRe = regexp.MustCompile(`<a href="([^"]*)" class="[^"]*"( aria-current="true")? data-asks-chip="(type|from)" data-asks-chip-key="([^"]*)"><span class="[^"]*">([^<]*)</span>`)

type chipSeen struct {
	href, kind, key, label string
	active                 bool
}

func chipsOf(page string) []chipSeen {
	var out []chipSeen
	for _, m := range chipRe.FindAllStringSubmatch(page, -1) {
		out = append(out, chipSeen{href: strings.ReplaceAll(m[1], "&amp;", "&"), active: m[2] != "", kind: m[3], key: m[4], label: m[5]})
	}
	return out
}

func sourceChip(t *testing.T, page, key string) (chipSeen, bool) {
	t.Helper()
	for _, c := range chipsOf(page) {
		if c.kind == "from" && c.key == key {
			return c, true
		}
	}
	return chipSeen{}, false
}

// TestTheChipRowNamesTheTypesAndTheSources: the row is under the search box,
// with the four type chips in order and a chip per source, called by its
// name, busiest first — the marketing officer (its question, its answered
// question, its draft and the step-up card for it: 4) ahead of the rest.
func TestTheChipRowNamesTheTypesAndTheSources(t *testing.T) {
	f := newFiltFix(t)
	page := f.page(t, "/attention")
	row := regexp.MustCompile(`(?s)<nav class="[^"]*" aria-label="Filter asks" data-asks-chips>.*?</nav>`).FindString(page)
	if row == "" {
		t.Fatal("no chip row")
	}
	if strings.Index(page, row) < strings.Index(page, `data-asks-search>`) {
		t.Error("the chip row is above the search box")
	}
	var types []string
	for _, c := range chipsOf(row) {
		if c.kind == "type" {
			types = append(types, c.label)
		}
	}
	if strings.Join(types, ",") != "Questions,Confirmations,Drafts,Notices" {
		t.Errorf("type chips %v", types)
	}
	mkt, ok := sourceChip(t, row, filtMkt)
	if !ok || mkt.label != "Marketing — Acme" || mkt.href != "/attention?from=officer%3A1%3Amarketing%3Aacme" || mkt.active {
		t.Fatalf("the marketing chip: %+v %v", mkt, ok)
	}
	if !strings.Contains(row, `data-asks-chip-key="officer:1:marketing:acme"><span class="`+ui.AsksChipLabelClass+`">Marketing — Acme</span><span class="`+ui.AsksChipCountClass+`" data-asks-chip-count>4</span>`) {
		t.Errorf("the marketing chip does not count its 4 items: %s", row)
	}
	if first := chipsOf(row); len(first) < 5 || first[4].key != filtMkt {
		t.Errorf("the busiest source is not the first source chip: %+v", first)
	}
	for _, k := range []string{"officer:1:product:acme", "agentid:" + strconv.FormatInt(f.op.ID, 10)} {
		if _, ok := sourceChip(t, page, k); !ok {
			t.Errorf("no chip for %s", k)
		}
	}
	// No id or address on a chip's face.
	for _, c := range chipsOf(row) {
		if strings.Contains(c.label, ":") || strings.Contains(c.label, "@") {
			t.Errorf("a chip's face shows an id or an address: %q", c.label)
		}
	}
}

// TestOneTapOnASourceKeepsOnlyItsItemsOnEveryTab: ?from=officer:marketing
// keeps, on Open, its question and the step-up card a session filed for its
// draft (and nothing else); on Answered, its answered question; on Drafts,
// its draft. Its chip is active and links back to the unfiltered page; Clear
// does too. The live lists re-read with the filter.
func TestOneTapOnASourceKeepsOnlyItsItemsOnEveryTab(t *testing.T) {
	f := newFiltFix(t)
	all := f.page(t, "/attention")
	for _, id := range []int64{f.mktAsk, f.productAsk, f.agentAsk, f.sessionAsk, f.mktCard} {
		if !idsIn(pageBetween(all, "attention-items", "attention-answered"), "ask-")[id] {
			t.Fatalf("control: ask %d is not on the unfiltered Open tab", id)
		}
	}
	q := "?from=" + url.QueryEscape(filtMkt)
	page := f.page(t, "/attention"+q)
	open := f.mine(idsIn(pageBetween(page, "attention-items", "attention-answered"), "ask-"))
	if len(open) != 2 || !open[f.mktAsk] || !open[f.mktCard] {
		t.Errorf("Open under Marketing lists %v, want only %d (its question) and %d (the step-up card for its draft)", open, f.mktAsk, f.mktCard)
	}
	if got := f.mine(idsIn(pageBetween(page, "attention-answered", "asks-tabs-panel-drafts"), "answered-")); len(got) != 1 || !got[f.mktAnswered] {
		t.Errorf("Answered under Marketing lists %v, want only %d", got, f.mktAnswered)
	}
	if got := idsIn(pageBetween(page, "asks-drafts-pending", "asks-drafts-archived-toggle"), "draft-"); len(got) != 1 || !got[f.mktDraft] {
		t.Errorf("Drafts under Marketing lists %v, want only %d", got, f.mktDraft)
	}
	// THE ONE OPEN COUNT over what the filter keeps: its question, its
	// step-up card and its pending draft.
	if !strings.Contains(page, ui.AsksOpenCountHTML(3)) {
		t.Error("the open count is not the filtered list's 3")
	}
	chip, _ := sourceChip(t, page, filtMkt)
	if !chip.active || chip.href != "/attention" {
		t.Errorf("the active chip: %+v (a tap on it must clear it)", chip)
	}
	// The active chip leads the row (it scrolls sideways: after the types it
	// was off a phone's screen).
	if cs := chipsOf(page); len(cs) == 0 || cs[0].key != filtMkt {
		t.Errorf("the row does not start with the active chip: %+v", cs)
	}
	// The type chips count what the source keeps: Marketing's two questions
	// (open, answered), its step-up card, its draft.
	for typ, want := range map[string]int{askTypeQuestion: 2, askTypeConfirm: 1, askTypeDraft: 1} {
		m := regexp.MustCompile(`data-asks-chip-key="` + typ + `"><span class="[^"]*">[^<]*</span>(?:<span class="[^"]*" data-asks-chip-count>(\d+)</span>)?`).FindStringSubmatch(page)
		if m == nil || m[1] != strconv.Itoa(want) {
			t.Errorf("under Marketing the %s chip counts %v, want %d", typ, m, want)
		}
	}
	if !strings.Contains(page, `href="/attention" class="`+ui.AsksChipsClearClass+`" data-asks-chips-clear>Clear</a>`) {
		t.Error("no Clear")
	}
	if !strings.Contains(page, strconv.Quote("/attention/items?from=officer%3A1%3Amarketing%3Aacme")) {
		t.Error("the Open feed's live refresh does not keep the filter")
	}
	// The live lists themselves.
	if got := f.mine(idsIn(f.page(t, "/attention/items"+q), "ask-")); len(got) != 2 || !got[f.mktAsk] || !got[f.mktCard] {
		t.Errorf("the live Open list under Marketing lists %v", got)
	}
	if got := f.mine(idsIn(f.page(t, "/attention/items"+q+"&part=answered"), "answered-")); len(got) != 1 || !got[f.mktAnswered] {
		t.Errorf("the live Answered list under Marketing lists %v", got)
	}
	if body := f.page(t, "/attention/drafts"+q); !strings.Contains(body, `"pending_ids":"`+strconv.FormatInt(f.mktDraft, 10)+`"`) {
		t.Errorf("the live Drafts list under Marketing: %s", body)
	}
}

// TestATypeChipAloneAndWithASource: ?type=confirmation keeps the step-up card
// only; ?type=question&from=marketing keeps the officer's question but not
// its step-up card; ?type=draft empties Open and Answered and keeps every
// draft.
func TestATypeChipAloneAndWithASource(t *testing.T) {
	f := newFiltFix(t)
	open := func(path string) map[int64]bool {
		return f.mine(idsIn(pageBetween(f.page(t, path), "attention-items", "attention-answered"), "ask-"))
	}
	if got := open("/attention?type=confirmation"); len(got) != 1 || !got[f.mktCard] {
		t.Errorf("Confirmations lists %v, want only %d", got, f.mktCard)
	}
	got := open("/attention?type=question&from=" + url.QueryEscape(filtMkt))
	if len(got) != 1 || !got[f.mktAsk] {
		t.Errorf("Questions from Marketing lists %v, want only %d", got, f.mktAsk)
	}
	page := f.page(t, "/attention?type=draft")
	if got := idsIn(pageBetween(page, "attention-items", "attention-answered"), "ask-"); len(got) != 0 || !strings.Contains(pageBetween(page, "attention-items", "attention-answered"), "data-asks-drafts-wait") {
		t.Errorf("Drafts leaves Open with %v and no pointer to the drafts", got)
	}
	if got := idsIn(pageBetween(page, "attention-answered", "asks-tabs-panel-drafts"), "answered-"); len(got) != 0 || !strings.Contains(pageBetween(page, "attention-answered", "asks-tabs-panel-drafts"), asksFilterEmptyLine) {
		t.Errorf("Drafts leaves Answered with %v (or without its empty line)", got)
	}
	if got := idsIn(pageBetween(page, "asks-drafts-pending", "asks-drafts-archived-toggle"), "draft-"); !got[f.mktDraft] {
		t.Errorf("Drafts keeps no draft: %v", got)
	}
	chip := ""
	for _, c := range chipsOf(page) {
		if c.active {
			chip = c.key
		}
	}
	if chip != askTypeDraft {
		t.Errorf("the active chip is %q", chip)
	}
	// An unknown type is no filter.
	if got := open("/attention?type=nonsense"); !got[f.agentAsk] || !got[f.mktAsk] {
		t.Errorf("an unknown type filtered the page: %v", got)
	}
}

// TestAnotherOwnersOfficerIsNeverAChipOrAResult: OWNER SCOPING. The member
// has a "sales:zeta" officer and their own "marketing:acme"; the operator's
// page draws no Sales chip, its Marketing chip counts the operator's four
// items only, and ?from=officer:sales:zeta lists none of the member's items —
// while the member's own page draws Sales (the control: the chip exists for
// its owner) and their Marketing lists only their own question.
func TestAnotherOwnersOfficerIsNeverAChipOrAResult(t *testing.T) {
	f := newFiltFix(t)
	page := f.page(t, "/attention")
	memberSales := "officer:" + strconv.FormatInt(f.member, 10) + ":sales:zeta"
	if _, ok := sourceChip(t, page, memberSales); ok || strings.Contains(page, "Sales") || strings.Contains(page, "MEMBER:") {
		t.Error("OWNER SCOPING: the operator's page names the member's officer or their items")
	}
	sales := f.page(t, "/attention?from="+url.QueryEscape(memberSales))
	if got := idsIn(sales, "ask-"); got[f.memberSales] || got[f.memberMkt] || strings.Contains(sales, "MEMBER:") {
		t.Errorf("OWNER SCOPING: the operator's Sales filter lists the member's items: %v", got)
	}
	mkt := f.page(t, "/attention?from="+url.QueryEscape(filtMkt))
	if got := idsIn(mkt, "ask-"); got[f.memberMkt] {
		t.Errorf("OWNER SCOPING: the operator's Marketing lists the member's marketing question %d", f.memberMkt)
	}
	// The control: the member sees their own officers.
	theirs := f.pageAs(t, f.member, "/attention")
	if c, ok := sourceChip(t, theirs, memberSales); !ok || c.label != "Sales — Zeta" {
		t.Fatalf("control: the member's page has no Sales chip: %+v %v", c, ok)
	}
	theirMkt := f.pageAs(t, f.member, "/attention?from="+url.QueryEscape("officer:"+strconv.FormatInt(f.member, 10)+":"+filtMarketing))
	if got := f.mine(idsIn(pageBetween(theirMkt, "attention-items", "attention-answered"), "ask-")); len(got) != 1 || !got[f.memberMkt] {
		t.Errorf("the member's Marketing lists %v, want only their %d", got, f.memberMkt)
	}
}

// TestTheFilterRoundTripsWithASearch: ?q= and ?from= together — the results
// are the search's narrowed to the source, every chip link keeps ?q=, Clear
// keeps it too, the search form carries the filter (a submit keeps it), and
// the box's live request answers the narrowed results with an address that
// keeps both.
func TestTheFilterRoundTripsWithASearch(t *testing.T) {
	f := newFiltFix(t)
	plain := f.page(t, "/attention?q=launch")
	res := pageBetween(plain, ui.AsksSearchRegionID, "")
	if got := idsIn(res, "search-ask-"); !got[f.mktAsk] || !got[f.mktCard] || !got[f.mktAnswered] {
		t.Fatalf("control: the search for launch finds %v", got)
	}
	if got := idsIn(res, "search-draft-"); !got[f.mktDraft] {
		t.Fatalf("control: the search for launch finds no draft %d", f.mktDraft)
	}
	page := f.page(t, "/attention?q=launch&type=question&from="+url.QueryEscape(filtMkt))
	res = pageBetween(page, ui.AsksSearchRegionID, "")
	if got := f.mine(idsIn(res, "search-ask-")); len(got) != 2 || !got[f.mktAsk] || !got[f.mktAnswered] {
		t.Errorf("the filtered search lists asks %v, want %d and %d", got, f.mktAsk, f.mktAnswered)
	}
	if got := idsIn(res, "search-draft-"); len(got) != 0 {
		t.Errorf("the Questions search lists drafts %v", got)
	}
	for _, c := range chipsOf(page) {
		u, err := url.Parse(c.href)
		if err != nil || u.Query().Get("q") != "launch" {
			t.Errorf("chip %s does not keep q: %s", c.key, c.href)
		}
	}
	if !strings.Contains(page, `href="/attention?q=launch" class="`+ui.AsksChipsClearClass+`"`) {
		t.Error("Clear does not keep the search")
	}
	for _, want := range []string{`<input type="hidden" name="type" value="question" data-asks-search-keep>`,
		`<input type="hidden" name="from" value="officer:1:marketing:acme" data-asks-search-keep>`, `hx-include="closest form"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the search form lacks %q", want)
		}
	}
	// The box's live request.
	r := httptest.NewRequest(http.MethodGet, "/attention?q=launch&type=question&from="+url.QueryEscape(filtMkt), nil).WithContext(asUser(1))
	r.Header.Set("HX-Request", "true")
	r.Header.Set("HX-Target", ui.AsksSearchRegionID)
	w := httptest.NewRecorder()
	f.srv.handleAttentionPage(w, r)
	if got := f.mine(idsIn(w.Body.String(), "search-ask-")); len(got) != 2 || !got[f.mktAsk] {
		t.Errorf("the live search lists %v", got)
	}
	if to := w.Header().Get("HX-Replace-Url"); to != "/attention?from=officer%3A1%3Amarketing%3Aacme&q=launch&type=question" {
		t.Errorf("the live search's address: %q", to)
	}
	// An id search jumps only to what the filter keeps: the product ask's id
	// under Marketing lists nothing instead of opening it.
	pq := "/attention?q=ask+" + strconv.FormatInt(f.productAsk, 10) + "&from=" + url.QueryEscape(filtMkt)
	if w := f.call(t, http.MethodGet, pq, nil, f.operator()); w.Code != http.StatusOK || strings.Contains(w.Body.String(), `id="search-ask-`) {
		t.Errorf("an id search under a filter that does not keep it: %d", w.Code)
	}
}

// countingDrafts counts the draft reads a render makes (round 1, Y2).
type countingDrafts struct {
	plugins.Store
	mu  sync.Mutex
	n   map[string]int
	ids [][]int64
}

func (c *countingDrafts) note(k string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == nil {
		c.n = map[string]int{}
	}
	c.n[k]++
}

func (c *countingDrafts) counts() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]int{}
	for k, v := range c.n {
		out[k] = v
	}
	return out
}

func (c *countingDrafts) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n, c.ids = nil, nil
}

func (c *countingDrafts) DraftCard(ctx context.Context, id int64) (plugins.DraftCard, error) {
	c.note("DraftCard")
	return c.Store.DraftCard(ctx, id)
}

func (c *countingDrafts) DraftCards(ctx context.Context, limit int) ([]plugins.DraftCard, error) {
	c.note("DraftCards")
	return c.Store.DraftCards(ctx, limit)
}

func (c *countingDrafts) DraftSources(ctx context.Context, ids []int64) ([]plugins.DraftSource, error) {
	c.note("DraftSources")
	c.mu.Lock()
	c.ids = append(c.ids, append([]int64(nil), ids...))
	c.mu.Unlock()
	return c.Store.DraftSources(ctx, ids)
}

// stepUpFor files, as owner, a step-up card bound to draft id.
func (f *filtFix) stepUpFor(t *testing.T, owner, draft int64, why string) int64 {
	t.Helper()
	a, err := f.attn.Create(asUser(owner), attention.New{Asker: attention.Asker{System: true, Label: "step-up"}, Options: []string{"Confirm", "Refuse"},
		StepUp: &attention.StepUpCard{Tool: "plugin_draft_decide", Title: "Post to X? · Draft #" + strconv.FormatInt(draft, 10), Why: why,
			Asker: "work:1", Preview: why, Venue: "X", Platform: "x", Target: "post", DraftID: draft, Details: "A session asks to approve the draft."}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(authz.System(context.Background()), `DELETE FROM attention_asks WHERE id = $1`, a.ID)
	})
	return a.ID
}

// TestARenderReadsTheCardsDraftsOnceAndSweepsNothing (round 1, Y2): the step-up
// cards whose drafts are not among the Drafts tab's are named by ONE
// reader-scoped DraftSources read per render — never a DraftCard per draft,
// which sweeps (a write transaction). Measured on an unfiltered GET /attention
// with two such cards: at 39928e4c4 it was 2 DraftCard calls (2 sweeps) per
// render; here 0 DraftCard and 1 DraftSources naming both drafts.
func TestARenderReadsTheCardsDraftsOnceAndSweepsNothing(t *testing.T) {
	c := &countingDrafts{}
	f := newFiltFix(t, func(ext *Extensions, _ *pgxpool.Pool) { c.Store = ext.Plugins; ext.Plugins = c })
	// The control: the render's draft reads with no card the hop must name.
	c.reset()
	f.page(t, "/attention")
	base := c.counts()
	gone := int64(1 << 40)
	f.stepUpFor(t, 1, f.memberDraft, "a card on a draft the reader cannot read")
	f.stepUpFor(t, 1, gone, "a card on a draft that is gone")
	c.reset()
	f.page(t, "/attention")
	n := c.counts()
	t.Logf("draft reads per GET /attention: %v, DraftSources ids %v", n, c.ids)
	if n["DraftCard"] != 0 {
		t.Errorf("the render read %d drafts one by one (DraftCard sweeps: a write each)", n["DraftCard"])
	}
	if n["DraftSources"] != 1 || len(c.ids) != 1 || len(c.ids[0]) != 2 {
		t.Errorf("the render's DraftSources reads: %d %v, want one naming both cards' drafts", n["DraftSources"], c.ids)
	}
	if n["DraftCards"] != base["DraftCards"] {
		t.Errorf("the page's own draft list reads went %d → %d with the cards", base["DraftCards"], n["DraftCards"])
	}
}

// TestAStepUpCardOnAnotherOwnersDraftIsNotTheirOfficers (round 1, Y3): the
// step-up → draft → officer hop for a draft NOT among the reader's Drafts tab
// (so the hop's own read runs), whose draft is ANOTHER owner's (the member's
// sales officer's): row-level security hides it, so the card is named by its
// head — never as that owner's officer, no chip, and that officer's filter
// lists nothing. A mutation that reads the hop as authz.System fails here.
func TestAStepUpCardOnAnotherOwnersDraftIsNotTheirOfficers(t *testing.T) {
	f := newFiltFix(t)
	card := f.stepUpFor(t, 1, f.memberDraft, "a card on the member's draft")
	page := f.page(t, "/attention")
	if !idsIn(pageBetween(page, "attention-items", "attention-answered"), "ask-")[card] {
		t.Fatalf("control: the card %d is not on the operator's page", card)
	}
	sales := "officer:" + strconv.FormatInt(f.member, 10) + ":sales:zeta"
	if _, ok := sourceChip(t, page, sales); ok || strings.Contains(page, "Sales") {
		t.Errorf("OWNER SCOPING: the card on the member's draft made their officer a chip")
	}
	if got := idsIn(f.page(t, "/attention?from="+url.QueryEscape(sales)), "ask-"); got[card] {
		t.Errorf("OWNER SCOPING: the member's officer's filter lists the operator's card %d", card)
	}
}

// TestTheAgentBadgeCountsWhatItsFilterLists (round 1): ONE predicate
// (agentAskOf) for a session page's asks badge and the agent's filter. The
// agent has two asks and a reply draft with a step-up card on it (muster's
// card, not the agent's ask); the badge says 2 and the page its tap opens
// lists exactly 2 cards. At 39928e4c4 the filter listed the step-up card too
// (3 cards under a badge of 2).
func TestTheAgentBadgeCountsWhatItsFilterLists(t *testing.T) {
	f := newFiltFix(t)
	f.ask(t, f.op2Tok, "First?")
	f.ask(t, f.op2Tok, "Second?")
	sys := authz.System(context.Background())
	var plugin, msg, draft int64
	if err := f.pool.QueryRow(sys, `INSERT INTO plugins (owner_id, name, label, token_hash, data_scope) VALUES (1, 'filt-badge', 'Inbox', md5('filt-badge') || md5('b'), 'filt-badge') RETURNING id`).Scan(&plugin); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(sys, `INSERT INTO plugin_messages (owner_id, plugin_id, external_id, sender, body, status, delivered_at, agent_id)
		VALUES (1, $1, 'filt-b-1', 'someone', 'Noon?', 'delivered', now(), $2) RETURNING id`, plugin, f.op2.ID).Scan(&msg); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(sys, `INSERT INTO plugin_reply_drafts (owner_id, message_id, agent_id, agent_name, body, expires_at)
		VALUES (1, $1, $2, $3, 'Yes.', now() + interval '1 hour') RETURNING id`, msg, f.op2.ID, f.op2.Name).Scan(&draft); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(sys, `DELETE FROM plugin_reply_drafts WHERE id = $1`, draft)
		_, _ = f.pool.Exec(sys, `DELETE FROM plugin_messages WHERE id = $1`, msg)
		_, _ = f.pool.Exec(sys, `DELETE FROM plugins WHERE id = $1`, plugin)
	})
	card := f.stepUpFor(t, 1, draft, "approve the agent's reply")
	href, n := badgeOf(t, f.page(t, "/attention/asker?from="+url.QueryEscape("agent:"+f.op2.Name)))
	page := f.page(t, href)
	listed := idsIn(pageBetween(page, "attention-items", "attention-answered"), "ask-")
	if n != 2 || len(listed) != n || listed[card] {
		t.Errorf("the badge says %d, its page lists %v (want 2 cards, not the step-up card %d)", n, listed, card)
	}
}

// TestAWindowFilterStaysOnEveryLiveList (round 1, Y1): a session page's window
// key (?from=window:…) is read with the reader's windows by the live
// endpoints too — the Answered refresh, the Drafts refresh and the search box
// — so none drops it; and the search's address keeps it. At 39928e4c4 each
// parsed it with no windows and listed everything.
func TestAWindowFilterStaysOnEveryLiveList(t *testing.T) {
	f := newFiltFix(t)
	w := f.mirrorAsk(t, "Merge the launch schema change?")
	if _, err := f.attn.Answer(asUser(1), w.ID, "yes"); err != nil {
		t.Fatal(err)
	}
	q := "from=" + url.QueryEscape("window:ws:@1:main")
	ans := idsIn(f.page(t, "/attention/items?part=answered&"+q), "answered-")
	if !ans[w.ID] || ans[f.mktAnswered] {
		t.Errorf("the live Answered list under the window lists %v: want %d and not %d", ans, w.ID, f.mktAnswered)
	}
	var drafts struct {
		PendingIDs string `json:"pending_ids"`
	}
	if err := json.Unmarshal([]byte(f.page(t, "/attention/drafts?"+q)), &drafts); err != nil || drafts.PendingIDs != "" {
		t.Errorf("the live Drafts list under the window: %q %v (a window has no drafts)", drafts.PendingIDs, err)
	}
	r := httptest.NewRequest(http.MethodGet, "/attention?q=launch&"+q, nil).WithContext(asUser(1))
	r.Header.Set("HX-Request", "true")
	r.Header.Set("HX-Target", ui.AsksSearchRegionID)
	rec := httptest.NewRecorder()
	f.srv.handleAttentionPage(rec, r)
	got := f.mine(idsIn(rec.Body.String(), "search-ask-"))
	if len(got) != 0 || !idsIn(rec.Body.String(), "search-ask-")[w.ID] || strings.Contains(rec.Body.String(), "search-draft-") {
		t.Errorf("the live search under the window lists %v (want only %d)", idsIn(rec.Body.String(), "search-ask-"), w.ID)
	}
	if to := rec.Header().Get("HX-Replace-Url"); to != "/attention?from=window%3Aws%3A%401%3Amain&q=launch" {
		t.Errorf("the live search's address drops the window: %q", to)
	}
}
