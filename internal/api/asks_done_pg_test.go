package api

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// A DECIDED CARD STAYS PUT, SETTLED; THE DONE TAB HOLDS EVERYTHING CLOSED;
// THE REJECT REASON GROWS AND TAKES DICTATION (asks_done.go,
// draftReasonFieldHTML), over Postgres through the real routes.
//
// ⚠ THIS FILE NAMES ONLY WHAT origin/main ALREADY HAS (its routes, fixtures,
// helpers and the pages' literal markup), so it compiles there and each test
// is shown RED against the code before the change, not merely absent. The
// store's paging and owner scope are tested in internal/attention
// closed_pg_test.go.

// orderedIDs is the ids of the elements whose id is prefix<N> in s, in order.
func orderedIDs(s, prefix string) []int64 {
	var out []int64
	for _, m := range regexp.MustCompile(`<li id="`+regexp.QuoteMeta(prefix)+`(\d+)"`).FindAllStringSubmatch(s, -1) {
		id, _ := strconv.ParseInt(m[1], 10, 64)
		out = append(out, id)
	}
	return out
}

// doneSection is the Done tab's feed of page (its <ul>, up to the next tab's
// panel).
func doneSection(page string) string {
	return pageBetween(page, "attention-answered", "asks-tabs-panel-drafts")
}

// TestRejectingAStepUpCardsDraftLeavesItSettledInPlace is the operator's path:
// "Reject this draft?" on the Open feed, with a reason, posted as its form
// posts it. The browser lands on the Asks page with the card STILL THERE, in
// its old spot, settled: what was decided, the owner's reason (escaped, read
// from the draft), "View in session" (the drafting agent's page) and the way
// to the card's own page — not counted as open. On the next plain load it has
// left the Open feed and is on the Done tab, with the reason.
//
// 🔴 RED AT origin/main: the reject landed on the bare Asks page and the card
// was gone from it — nowhere to see it, nothing to click.
func TestRejectingAStepUpCardsDraftLeavesItSettledInPlace(t *testing.T) {
	f := newStepUpDraftFixture(t, "reject")
	cid, did := strconv.FormatInt(f.card, 10), strconv.FormatInt(f.draft.ID, 10)
	open := feedCard(f.page(t, attentionPath), f.card)
	// The reject form carries the card it is drawn on.
	if !strings.Contains(open, `<input type="hidden" name="card" value="`+cid+`">`) {
		t.Fatalf("the card's reject form does not carry the card:\n%s", open)
	}
	const reason = "Too <b>salesy</b> & too long"
	w := f.ownerForm(t, "/plugins/drafts/"+did+"/reject", url.Values{"return": {attentionPath}, "card": {cid}, "reason": {reason}}, sessionCookieFor(f.srv))
	landing := "/attention?settled=" + cid + "#ask-" + cid
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != landing {
		t.Fatalf("the reject: %d to %q, want 303 to %q (%s)", w.Code, w.Header().Get("Location"), landing, w.Body)
	}
	if r := f.row(t, f.draft.ID); r.status != "rejected" {
		t.Fatalf("the draft after the reject: %+v", r)
	}
	page := f.page(t, "/attention?settled="+cid)
	settled := feedCard(page, f.card)
	for _, want := range []string{
		" data-asks-settled",
		`data-status="answered"`,
		`data-asks-outcome-state data-attention-answer-text>Rejected in the web UI</p>`,
		`data-asks-reason>Your reason: Too &lt;b&gt;salesy&lt;/b&gt; &amp; too long</p>`,
		`<a href="` + html.EscapeString(ui.AgentSessionHref(f.chief.Name, 0)) + `" class="` + ui.AsksCardLinkClass + `" data-asks-view-session>View in session</a>`,
		`<a href="/attention/asks/` + cid + `" class="` + ui.AsksCardLinkClass + `" data-asks-ask-link>Open ask</a>`,
	} {
		if !strings.Contains(settled, want) {
			t.Errorf("the settled card lacks %q:\n%s", want, settled)
		}
	}
	if strings.Contains(settled, "<b>salesy") {
		t.Error("the owner's reason is drawn unescaped")
	}
	if strings.Contains(settled, "data-draft-reject") || strings.Contains(settled, "data-draft-approve") {
		t.Error("the settled card still offers the draft's decision")
	}
	// It is in the Open feed, before the feed's end, and not counted.
	feedStart, doneStart := strings.Index(page, `id="attention-items"`), strings.Index(page, `id="attention-answered"`)
	if at := strings.Index(page, `<li id="ask-`+cid+`"`); at < feedStart || at > doneStart {
		t.Errorf("the settled card is not on the Open feed (at %d, feed %d..%d)", at, feedStart, doneStart)
	}
	count := regexp.MustCompile(`data-asks-open-count[^>]*>(\d+) open<`)
	plain := f.page(t, attentionPath)
	if a, b := count.FindStringSubmatch(page), count.FindStringSubmatch(plain); a == nil || b == nil || a[1] != b[1] {
		t.Errorf("the settled card changes the open count: %v with it, %v without", a, b)
	}
	// The next plain load: it has left the Open feed and is on Done.
	if c := feedCard(plain, f.card); c != "" {
		t.Fatalf("after a reload the card is still on the Open feed: %s", c)
	}
	done := doneSection(plain)
	if !slices.Contains(orderedIDs(done, "answered-"), f.card) || !strings.Contains(done, `Your reason: Too &lt;b&gt;salesy&lt;/b&gt; &amp; too long`) {
		t.Errorf("the Done tab lacks the card or its reason:\n%s", done)
	}
}

// TestASettledLinkDrawsOnlyTheReadersOwnClosedAsk: ?settled= names an ask;
// the page draws it settled only when it is the reader's own and closed — an
// open ask's id draws its open card once, another owner's draws nothing.
func TestASettledLinkDrawsOnlyTheReadersOwnClosedAsk(t *testing.T) {
	f := newAttnFixture(t)
	open := f.ask(t, f.opTok, "Still open, settled link?")
	theirs, err := f.attn.Create(asUser(f.member), attention.New{Asker: attention.Asker{AgentID: f.worker.ID, Label: f.worker.Name}, Question: "Another owner's, settled link?"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.attn.Cancel(asUser(f.member), theirs.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{open, theirs.ID} {
		page := f.page(t, "/attention?settled="+strconv.FormatInt(id, 10))
		if c := feedCard(page, id); strings.Contains(c, " data-asks-settled") || (id == theirs.ID && c != "") {
			t.Errorf("?settled=%d draws a settled card: %s", id, c)
		}
	}
	// Control: the reader's own closed ask IS drawn settled.
	mine := f.ask(t, f.opTok, "Mine, closed, settled link?")
	if _, err := f.attn.Cancel(asUser(1), mine); err != nil {
		t.Fatal(err)
	}
	if c := feedCard(f.page(t, "/attention?settled="+strconv.FormatInt(mine, 10)), mine); !strings.Contains(c, " data-asks-settled") {
		t.Errorf("control: the reader's own closed ask is not drawn settled: %s", c)
	}
	if n := strings.Count(f.page(t, "/attention?settled="+strconv.FormatInt(open, 10)), `<li id="ask-`+strconv.FormatInt(open, 10)+`"`); n != 1 {
		t.Errorf("the open ask is drawn %d times", n)
	}
}

// TestTheDoneTabListsEverythingClosedPagedFilteredAndAliased: 29 of the
// owner's closed asks — answered, dismissed, expired and answered in the
// session, every one closed MORE THAN A DAY ago (the old tab's window), and
// more than its 20 — are all on the Done tab, newest closed first, 25 a page:
// "Show older" opens the 4 older, nothing repeats, and at exactly one page
// there is no "Show older". Another owner's closed ask with the same text is
// on neither page. The type chips filter it; ?tab=answered (the old name, in
// links already on phones) opens it, and the old live part still answers.
//
// 🔴 RED AT origin/main: the Answered tab drew only the answers of the last
// day, at most 20 — none of these.
func TestTheDoneTabListsEverythingClosedPagedFilteredAndAliased(t *testing.T) {
	f := newAttnFixture(t)
	sys := authz.System(context.Background())
	// Only this test's closed asks are the owner's (other tests' leftovers
	// would interleave by time).
	if _, err := f.pool.Exec(sys, `DELETE FROM attention_asks WHERE owner_id = 1 AND (status <> 'open' OR expires_at <= now())`); err != nil {
		t.Fatal(err)
	}
	const marker = "the shared done-tab word"
	now := time.Now().UTC().Truncate(time.Second)
	closeRow := func(id int64, status string, at time.Time) {
		t.Helper()
		if _, err := f.pool.Exec(sys, `UPDATE attention_asks SET status = $2, resolved_at = $3,
			answer = CASE WHEN $2 IN ('answered', 'answered_in_session') THEN 'done' END,
			answered_by = CASE WHEN $2 IN ('answered', 'answered_in_session') THEN 'operator' END,
			delivery = CASE WHEN $2 = 'answered' AND agent_id IS NOT NULL THEN 'delivered' END WHERE id = $1`, id, status, at); err != nil {
			t.Fatal(err)
		}
	}
	statuses := []string{"answered", "cancelled", "expired", "answered_in_session"}
	var mine []int64 // newest closed first
	// Two step-up cards (the "Confirmations" chip), the newest.
	var confirms []int64
	for i := 0; i < 2; i++ {
		a, err := f.attn.Create(asUser(1), attention.New{Asker: attention.Asker{System: true, Label: hostDecisionLabel},
			StepUp: &attention.StepUpCard{Tool: pluginDraftDecideTool, Title: "Post to example.forum? · Draft #999990" + strconv.Itoa(i),
				Why: "A done card", Preview: marker, DraftID: 9999990 + int64(i), Asker: "work:1", Details: "A session asks."},
			Options: []string{hostDecisionConfirm, hostDecisionRefuse}})
		if err != nil {
			t.Fatal(err)
		}
		closeRow(a.ID, "answered", now.Add(-25*time.Hour-time.Duration(i)*time.Minute))
		confirms = append(confirms, a.ID)
		mine = append(mine, a.ID)
	}
	for i := 0; i < 27; i++ {
		a, err := f.attn.Create(asUser(1), attention.New{Asker: attention.Asker{AgentID: f.op.ID, Label: f.op.Name}, Question: marker + " " + strconv.Itoa(i)})
		if err != nil {
			t.Fatal(err)
		}
		closeRow(a.ID, statuses[i%len(statuses)], now.Add(-48*time.Hour-time.Duration(i)*time.Minute))
		mine = append(mine, a.ID)
	}
	t.Cleanup(func() {
		for _, id := range mine {
			_, _ = f.pool.Exec(sys, `DELETE FROM attention_asks WHERE id = $1`, id)
		}
	})
	theirs, err := f.attn.Create(asUser(f.member), attention.New{Asker: attention.Asker{AgentID: f.worker.ID, Label: f.worker.Name}, Question: marker + " 3"})
	if err != nil {
		t.Fatal(err)
	}
	closeRow(theirs.ID, "cancelled", now.Add(-48*time.Hour-90*time.Second))

	page := f.page(t, "/attention?tab=done")
	first := doneSection(page)
	got1 := orderedIDs(first, "answered-")
	if !slices.Equal(got1, mine[:25]) {
		t.Fatalf("Done page 1: %v, want the 25 newest %v", got1, mine[:25])
	}
	for _, st := range statuses {
		if !strings.Contains(first, `data-status="`+st+`"`) {
			t.Errorf("Done page 1 has no %s card", st)
		}
	}
	older := regexp.MustCompile(`<a href="([^"]+)" class="[^"]*" data-asks-done-older>Show older</a>`).FindStringSubmatch(first)
	if older == nil {
		t.Fatalf("Done page 1 has no Show older:\n%s", first[max(0, len(first)-1500):])
	}
	second := doneSection(f.page(t, html.UnescapeString(older[1])))
	got2 := orderedIDs(second, "answered-")
	if !slices.Equal(got2, mine[25:]) {
		t.Fatalf("Done page 2: %v, want the 4 older %v", got2, mine[25:])
	}
	if strings.Contains(second, "data-asks-done-older") || !strings.Contains(second, "data-asks-done-newest") {
		t.Errorf("Done page 2 (the last) offers Show older, or no way back to the newest")
	}
	for _, p := range [][]int64{got1, got2} {
		if slices.Contains(p, theirs.ID) {
			t.Errorf("another owner's closed ask %d is on the owner's Done tab", theirs.ID)
		}
	}
	// The chips: Confirmations alone, then Questions alone.
	if got := orderedIDs(doneSection(f.page(t, "/attention?tab=done&type=confirmation")), "answered-"); !slices.Equal(got, confirms) {
		t.Errorf("Done under Confirmations: %v, want %v", got, confirms)
	}
	if got := orderedIDs(doneSection(f.page(t, "/attention?tab=done&type=question")), "answered-"); len(got) != 25 || slices.Contains(got, confirms[0]) || got[0] != mine[2] {
		t.Errorf("Done under Questions: %v, want 25 starting at %d, no confirmation", got, mine[2])
	}
	// The old name opens Done; the old live part answers with its feed.
	alias := f.page(t, "/attention?tab=answered")
	if !regexp.MustCompile(`id="asks-tabs-tab-done"[^>]*aria-selected="true"|aria-selected="true"[^>]*id="asks-tabs-tab-done"`).MatchString(alias) {
		t.Errorf("?tab=answered does not open the Done tab")
	}
	if got := orderedIDs(f.page(t, "/attention/items?part=answered"), "answered-"); !slices.Equal(got, mine[:25]) {
		t.Errorf("?part=answered: %v, want the Done feed's first page", got)
	}
	// EXACTLY ONE PAGE: no Show older.
	for _, id := range mine[25:] {
		if _, err := f.pool.Exec(sys, `DELETE FROM attention_asks WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	exact := doneSection(f.page(t, "/attention?tab=done"))
	if got := orderedIDs(exact, "answered-"); len(got) != 25 || strings.Contains(exact, "data-asks-done-older") {
		t.Errorf("exactly 25 closed: %d cards, Show older drawn %v", len(got), strings.Contains(exact, "data-asks-done-older"))
	}
}

// TestTheRejectReasonIsAGrowingTextareaThatTakesDictation: every face that
// draws a draft's reason — a step-up card on the Open feed, the same draft on
// the Drafts tab, the card's page — draws a TEXTAREA (one line, growing to the
// answer box's cap: ui.AskFormFitClass), still name="reason" and bounded at
// plugins.MaxDecisionReasonRunes, each with an id of its own. With dictation
// on, each has the mic for ITS box and the page has the dictation script; with
// it off there is no live mic (the muted one, as every answer box draws).
//
// 🔴 RED AT origin/main: a one-line <input>, no mic, one id for both faces.
func TestTheRejectReasonIsAGrowingTextareaThatTakesDictation(t *testing.T) {
	f := newStepUpDraftFixture(t, "reject")
	cid, did := strconv.FormatInt(f.card, 10), strconv.FormatInt(f.draft.ID, 10)
	cardBox, tabBox := "draft-reason-"+did+"-ask-"+cid, "draft-reason-"+did
	box := func(id string) string {
		return `<textarea id="` + id + `" name="reason" rows="1" maxlength="` + strconv.Itoa(plugins.MaxDecisionReasonRunes) +
			`" placeholder="Reason (optional)" autocomplete="off" class="` + ui.AskFormInputClass + `" data-draft-reason-input></textarea>`
	}
	if !strings.Contains(ui.AskFormInputClass, ui.AskFormFitClass) {
		t.Fatal("instrument: the answer box's class has no fit class")
	}
	faces := func() map[string][2]string {
		asks := f.page(t, attentionPath)
		return map[string][2]string{
			"the Open feed's card": {feedCard(asks, f.card), cardBox},
			"the Drafts tab":       {pageBetween(asks, "asks-tabs-panel-drafts", ""), tabBox},
			"the card's page":      {f.page(t, askPagePath(f.card)), cardBox},
		}
	}
	// Dictation off (no speech-to-text service): the box, no live mic.
	for face, c := range faces() {
		if !strings.Contains(c[0], box(c[1])) {
			t.Errorf("dictation off, %s: no reason textarea %s", face, c[1])
		}
		if strings.Contains(c[0], `data-dictate-input="`+c[1]+`"`) {
			t.Errorf("dictation off, %s: a live mic", face)
		}
	}
	// Dictation on.
	f.srv.ext.Transcriber = stubTranscriber{}
	asks := f.page(t, attentionPath)
	if !strings.Contains(asks, "window.__musterDictate") {
		t.Error("the Asks page has no dictation script")
	}
	for face, c := range faces() {
		if !strings.Contains(c[0], box(c[1])) {
			t.Errorf("dictation on, %s: no reason textarea %s", face, c[1])
		}
		if !strings.Contains(c[0], `data-dictate-input="`+c[1]+`"`) || !strings.Contains(c[0], `data-dictate-note="`+c[1]+`-dictate-note"`) {
			t.Errorf("dictation on, %s: no mic for %s", face, c[1])
		}
	}
	if page := f.page(t, askPagePath(f.card)); !strings.Contains(page, "window.__musterDictate") {
		t.Error("the card's page has no dictation script")
	}
	// One id per box on the page: the two faces of one draft never share one.
	if n := strings.Count(asks, `id="`+cardBox+`"`); n != 1 {
		t.Errorf("the card's box id is on the page %d times", n)
	}
	if n := strings.Count(asks, `id="`+tabBox+`"`); n != 1 {
		t.Errorf("the Drafts tab's box id is on the page %d times", n)
	}
}

// TestTheOfficerPagesRejectReasonTakesDictation: the officer page's Needs you
// draws the same reason textarea and its mic, and the page carries the
// dictation script.
//
// 🔴 RED AT origin/main: a one-line <input>, no mic, no script on the page.
func TestTheOfficerPagesRejectReasonTakesDictation(t *testing.T) {
	w := newOutboundWorld(t, true)
	id := strconv.FormatInt(w.readyDraft(), 10)
	w.srv.ext.Transcriber = stubTranscriber{}
	page := w.officerPage()
	nd := needsYou(page)
	if !strings.Contains(nd, `<textarea id="draft-reason-`+id+`" name="reason" rows="1"`) {
		t.Errorf("Needs you has no reason textarea for draft %s", id)
	}
	if !strings.Contains(nd, `data-dictate-input="draft-reason-`+id+`"`) {
		t.Errorf("Needs you has no mic for draft %s's reason", id)
	}
	if !strings.Contains(page, "window.__musterDictate") {
		t.Error("the officer's page has no dictation script")
	}
}
