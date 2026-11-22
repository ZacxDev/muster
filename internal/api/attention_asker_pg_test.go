package api

import (
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// AN ASKER'S OPEN ASKS (attention_asker.go), on a real Postgres: a session
// page — an agent's, a mirrored window's rendered view — draws NO asks block,
// only a count badge in its header; the badge links to the Asks page filtered
// to that asker's asks (?from=), which lists them and no one else's, answers
// them, and ignores a ?from= the viewer cannot see.

// pageFollow GETs path as the operator, following one redirect (the agent
// page puts its active session in the URL).
func (f *attnFixture) pageFollow(t *testing.T, path string) string {
	t.Helper()
	w := f.call(t, http.MethodGet, path, nil, f.operator())
	if w.Code == http.StatusSeeOther || w.Code == http.StatusFound {
		path = w.Header().Get("Location")
		w = f.call(t, http.MethodGet, path, nil, f.operator())
	}
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
	}
	return w.Body.String()
}

// badgeRe is the asks badge's link (ui.AskerAsksBadgeHTML): its href and count.
var badgeRe = regexp.MustCompile(`<a href="([^"]*)" hx-boost="false" data-asker-asks-link data-count="(\d+)"`)

// badgeOf is the page's asks badge: its href (unescaped) and its count; "", 0
// when the page draws none. It fails the test on more than one.
func badgeOf(t *testing.T, page string) (string, int) {
	t.Helper()
	m := badgeRe.FindAllStringSubmatch(page, -1)
	switch len(m) {
	case 0:
		return "", 0
	case 1:
		n, _ := strconv.Atoi(m[0][2])
		return strings.ReplaceAll(m[0][1], "&amp;", "&"), n
	}
	t.Fatalf("%d asks badges on the page", len(m))
	return "", 0
}

// noAsksBlock: the page draws none of the removed block — its element, its
// chip, its hide — and no answer form for an ask.
func noAsksBlock(t *testing.T, name, page string) {
	t.Helper()
	for _, bad := range []string{`id="asker-asks"`, `id="asker-asks-chip"`, "data-asker-asks-hide", "Hide until next ask", `action="/attention/asks/`} {
		if strings.Contains(page, bad) {
			t.Errorf("%s draws %q: the asks block is gone", name, bad)
		}
	}
}

// asksListed is the ask ids the Asks page's (or its live list's) Open feed
// lists.
func asksListed(page string) map[int64]bool {
	out := map[int64]bool{}
	if i := strings.Index(page, `id="attention-items"`); i >= 0 {
		page = page[i:]
		if j := strings.Index(page, `id="attention-answered"`); j >= 0 {
			page = page[:j]
		}
	}
	for _, m := range regexp.MustCompile(`<li id="ask-(\d+)"`).FindAllStringSubmatch(page, -1) {
		id, _ := strconv.ParseInt(m[1], 10, 64)
		out[id] = true
	}
	return out
}

// TestAnAgentPageCountsItsOwnOpenAsksInItsHeader: the agent page draws no
// asks block — only a badge counting that agent's OPEN asks (not another
// agent's, not an answered one), linking to the Asks page filtered to them —
// and its live refresh answers the same badge.
//
// 🔴 RED at origin/main: the page drew the block (with its answer forms) and
// no linked badge.
func TestAnAgentPageCountsItsOwnOpenAsksInItsHeader(t *testing.T) {
	f := newAttnFixture(t)
	f.ask(t, f.opTok, "Ship the migration tonight?", "ship it", "wait for Monday")
	f.ask(t, f.opTok, "And tag it?")
	f.ask(t, f.op2Tok, "Rotate the other key?")
	answered := f.ask(t, f.opTok, "Already answered?")
	if _, err := f.srv.answerAsk(asUser(1), answered, "yes"); err != nil {
		t.Fatal(err)
	}
	page := f.pageFollow(t, "/agents/"+f.op.Name)
	noAsksBlock(t, "the agent page", page)
	href, n := badgeOf(t, page)
	if want := "/attention?from=" + url.QueryEscape("agent:"+f.op.Name); href != want || n != 2 {
		t.Errorf("the badge: %q counting %d, want %q counting 2", href, n, want)
	}
	// The badge is in the header, before the chat pane.
	if b, h, pane := strings.Index(page, "data-asker-asks-link"), strings.Index(page, "</header>"), strings.Index(page, `id="chat-log"`); !(b < h && h < pane) {
		t.Errorf("the badge (%d) is not in the header (ends %d, pane %d)", b, h, pane)
	}
	// Live: re-read on the queue's own SSE event, from inside the pane's SSE
	// section, into the header's slot.
	for _, want := range []string{`hx-get="/attention/asker?from=` + url.QueryEscape("agent:"+f.op.Name) + `"`,
		`hx-trigger="sse:` + EventAttentionChanged + ` delay:50ms, every 60s" hx-target="#` + ui.AskerAsksBadgeID + `"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %q", want)
		}
	}
	// The other agent's page counts ITS one.
	if _, n := badgeOf(t, f.pageFollow(t, "/agents/"+f.op2.Name)); n != 1 {
		t.Errorf("the other agent's badge counts %d, want 1", n)
	}
	// The live refresh answers the same badge.
	if _, n := badgeOf(t, f.page(t, "/attention/asker?from="+url.QueryEscape("agent:"+f.op.Name))); n != 2 {
		t.Errorf("the fragment counts %d, want 2", n)
	}
}

// TestAnAgentWithNoOpenAsksDrawsNoBadge: none open, no badge — the slot is
// empty (and empty:hidden takes no room in the header).
func TestAnAgentWithNoOpenAsksDrawsNoBadge(t *testing.T) {
	f := newAttnFixture(t)
	page := f.pageFollow(t, "/agents/"+f.op.Name)
	if href, n := badgeOf(t, page); href != "" || n != 0 {
		t.Errorf("a badge with nothing open: %q %d", href, n)
	}
	if !strings.Contains(page, `<span id="`+ui.AskerAsksBadgeID+`" class="`+html.EscapeString(ui.AskerAsksSlotClass)+`" aria-live="polite"></span>`) {
		t.Error("the badge's empty slot is not drawn (the live refresh fills it)")
	}
}

// TestAWindowViewCountsItsSessionsOpenAsksOnly: the rendered view of ws's @1
// (Claude session focusSID) counts that session's open asks, and not an
// agent's ask, nor a session's that this window does not run. Windows that
// share one part of @1's address each count their own.
func TestAWindowViewCountsItsSessionsOpenAsksOnly(t *testing.T) {
	f := newAttnFixture(t)
	const sideSID = "2d4e6f80-1a2b-4c3d-8e4f-5a6b7c8d9e0f"
	f.mirror.mu.Lock()
	f.mirror.windows[1].ClaudeSession = secondSID
	f.mirror.windows = append(f.mirror.windows, mirror.Window{Session: "side", ID: "@1", Index: 1, Active: true, Claude: true,
		ClaudeSession: sideSID, Status: mirror.StatusIdle, StatusSince: attnSince, Activity: 1_700_000_000})
	f.mirror.others = []mirror.HostView{{Host: "laptop", Online: true, Interactive: true, StatusReport: true, Rendered: true,
		Windows: mirror.NormalizeWindows([]mirror.Window{{Session: "main", ID: "@1", Index: 1, Active: true, Claude: true, ClaudeSession: laptopSID,
			Status: mirror.StatusIdle, StatusSince: attnSince, Activity: 1_700_000_000}}, 0)}}
	f.mirror.mu.Unlock()
	f.mirrorAsk(t, "Merge the schema change?")
	f.mirrorAsk(t, "And the changelog?")
	f.mirrorAsk(t, "And the release notes?")
	f.askAs(t, attention.Asker{MirrorHost: "ws", MirrorSession: secondSID, Label: "main:2 @ ws"}, "From the other window?")
	f.askAs(t, attention.Asker{MirrorHost: "ws", MirrorSession: sideSID, Label: "side:1 @ ws"}, "From another session's @1?")
	f.askAs(t, attention.Asker{MirrorHost: "laptop", MirrorSession: laptopSID, Label: "main:1 @ laptop"}, "From another host's main @1?")
	f.askAs(t, attention.Asker{MirrorHost: "ws", MirrorSession: "5f5f5f5f-0000-4000-8000-00000000abcd", Label: "gone @ ws"}, "From a session no window runs?")
	f.ask(t, f.opTok, "An agent's question?")
	view := "/mirror/view?host=ws&session=main&window=%401"
	page := f.pageFollow(t, view)
	noAsksBlock(t, "the view", page)
	href, n := badgeOf(t, page)
	if want := "/attention?from=" + url.QueryEscape("window:ws:@1:main"); href != want || n != 3 {
		t.Errorf("@1's badge: %q counting %d, want %q counting 3", href, n, want)
	}
	// In the header, kept live by the Asks page's own script on its slot.
	if b, h := strings.Index(page, "data-asker-asks-link"), strings.Index(page, "</header>"); b < 0 || b > h {
		t.Errorf("the badge (%d) is not in the header (ends %d)", b, h)
	}
	if !strings.Contains(page, `document.getElementById("`+ui.AskerAsksBadgeID+`")`) {
		t.Error("the view does not refresh its asks badge")
	}
	for view, want := range map[string]int{
		"/mirror/view?host=ws&session=main&window=%402":     1,
		"/mirror/view?host=ws&session=side&window=%401":     1,
		"/mirror/view?host=laptop&session=main&window=%401": 1,
	} {
		if _, n := badgeOf(t, f.pageFollow(t, view)); n != want {
			t.Errorf("%s's badge counts %d, want %d", view, n, want)
		}
	}
}

// TestTheAsksPageFilterListsOnlyTheNamedSessionsAsks: /attention?from=<key>
// — where the badge goes — lists that asker's open asks and no one else's,
// says so with a "Showing asks from <name> · Show all" line, and its live list
// (/attention/items?from=) keeps the filter. Without ?from= the page lists
// them all (the control that the others are open and listable).
//
// 🔴 RED at origin/main: ?from= was ignored — every ask listed, no line.
func TestTheAsksPageFilterListsOnlyTheNamedSessionsAsks(t *testing.T) {
	f := newAttnFixture(t)
	a1 := f.ask(t, f.opTok, "Ship the migration tonight?", "ship it", "wait")
	a2 := f.ask(t, f.opTok, "And tag it?")
	b1 := f.ask(t, f.op2Tok, "Rotate the other key?")
	w1 := f.mirrorAsk(t, "Merge the schema change?").ID
	all := asksListed(f.page(t, "/attention"))
	for _, id := range []int64{a1, a2, b1, w1} {
		if !all[id] {
			t.Fatalf("control: ask %d is not on the unfiltered page: %v", id, all)
		}
	}
	for _, c := range []struct {
		key   string
		label string
		want  []int64
	}{
		{"agent:" + f.op.Name, f.op.Name, []int64{a1, a2}},
		{"agent:" + f.op2.Name, f.op2.Name, []int64{b1}},
		{"window:ws:@1:main", "", []int64{w1}},
	} {
		q := "?from=" + url.QueryEscape(c.key)
		page := f.page(t, "/attention"+q)
		for name, got := range map[string]map[int64]bool{"the page": asksListed(page), "the live list": asksListed(f.page(t, "/attention/items"+q))} {
			if len(got) != len(c.want) {
				t.Errorf("%s %s lists %v, want %v", c.key, name, got, c.want)
			}
			for _, id := range c.want {
				if !got[id] {
					t.Errorf("%s %s does not list ask %d: %v", c.key, name, id, got)
				}
			}
		}
		// WHOSE ASKS THESE ARE (asks_filter.go): an agent is a source chip,
		// drawn active, named, with Clear; a window — which no chip names —
		// keeps the line saying so, with Show all.
		if strings.HasPrefix(c.key, askerKeyAgent) {
			chip := activeSourceChip(page)
			if !strings.Contains(chip, `data-asks-chip-key="`+c.key+`"`) || !strings.Contains(chip, ">"+c.label+"</span>") || !strings.Contains(page, `href="/attention" class="`+ui.AsksChipsClearClass+`" data-asks-chips-clear>Clear</a>`) {
				t.Errorf("%s: the active source chip: %q", c.key, chip)
			}
		} else {
			line := regexp.MustCompile(`(?s)<p [^>]*data-testid="asks-filter">.*?</p>`).FindString(page)
			if !strings.Contains(line, "Showing asks from <strong") || !strings.Contains(line, `href="/attention" data-asks-filter-all>Show all</a>`) {
				t.Errorf("%s: the filter line: %q", c.key, line)
			}
			if c.label != "" && !strings.Contains(line, ">"+c.label+"</strong>") {
				t.Errorf("%s: the filter line does not name %q: %q", c.key, c.label, line)
			}
		}
		// The page's live list re-reads the filtered list.
		if !strings.Contains(page, strconv.Quote("/attention/items"+q)) {
			t.Errorf("%s: the live list does not keep the filter", c.key)
		}
		// The open count is the filtered list's.
		if !strings.Contains(page, ">"+strconv.Itoa(len(c.want))+" open</span>") {
			t.Errorf("%s: the open count is not %d", c.key, len(c.want))
		}
	}
	// A malformed key is no filter.
	if p := f.page(t, "/attention?from=nonsense"); strings.Contains(p, `data-testid="asks-filter"`) || len(asksListed(p)) != len(all) {
		t.Errorf("a malformed ?from= filtered the page")
	}
}

// TestTheAsksFilterIgnoresASessionTheViewerDoesNotOwn: OWNER SCOPING
// (resolveAsker). The operator naming ANOTHER person's agent in ?from= gets
// the page as without it — every ask of their own listed, no filter line, the
// other person's agent named nowhere — and its badge fragment is empty; that
// person, naming their own agent, gets the filter (the control that the key
// resolves for its owner).
func TestTheAsksFilterIgnoresASessionTheViewerDoesNotOwn(t *testing.T) {
	f := newAttnFixture(t)
	mine := f.ask(t, f.opTok, "My own question?")
	theirs := f.ask(t, f.workerTok, "The member's worker asks?")
	q := url.Values{askerFromParam: {"agent:" + f.worker.Name}}
	page := f.page(t, "/attention?"+q.Encode())
	if strings.Contains(page, `data-testid="asks-filter"`) || strings.Contains(page, f.worker.Name) {
		t.Errorf("OWNER SCOPING: the operator's page filtered to, or named, another person's agent %q", f.worker.Name)
	}
	if got := asksListed(page); !got[mine] || got[theirs] {
		t.Errorf("OWNER SCOPING: the operator's page with another person's ?from= lists %v, want their own %d and not %d", got, mine, theirs)
	}
	if frag := f.page(t, "/attention/asker?"+q.Encode()); frag != "" {
		t.Errorf("OWNER SCOPING: the badge fragment for another person's agent: %q", frag)
	}
	if _, ok := f.srv.asksFilter(asUser(1), q, nil); ok {
		t.Error("OWNER SCOPING: another person's agent resolved for the operator")
	}
	// The control: its owner gets the filter, listing its ask.
	k, ok := f.srv.asksFilter(asUser(f.member), q, nil)
	if !ok || k.agentID != f.worker.ID || k.label != f.worker.Name {
		t.Fatalf("control: the member's own agent does not resolve for them: %+v %v", k, ok)
	}
	r := httptest.NewRequest(http.MethodGet, "/attention?"+q.Encode(), nil).WithContext(asUser(f.member))
	w := httptest.NewRecorder()
	f.srv.handleAttentionPage(w, r)
	if body := w.Body.String(); w.Code != http.StatusOK || !strings.Contains(activeSourceChip(body), f.worker.Name) || !asksListed(body)[theirs] {
		t.Errorf("control: the member's filtered page: %d, active chip %q, lists %v", w.Code, activeSourceChip(body), asksListed(body))
	}
}

// activeSourceChip is the page's active source chip (asks_filter.go), "" for
// none.
func activeSourceChip(page string) string {
	return regexp.MustCompile(`<a [^>]*aria-current="true" data-asks-chip="from"[^>]*>.*?</a>`).FindString(page)
}

// TestAnAnswerFromTheFilteredAsksPageIsRecorded: the filtered page's card
// answers as every Asks card does (its htmx post: 204, nothing swapped), the
// ask is answered with that answer, and it leaves the filtered list and the
// badge's count.
func TestAnAnswerFromTheFilteredAsksPageIsRecorded(t *testing.T) {
	f := newAttnFixture(t)
	id := f.ask(t, f.opTok, "Ship the migration tonight?", "ship it", "wait")
	other := f.ask(t, f.opTok, "And tag it?")
	q := "?from=" + url.QueryEscape("agent:"+f.op.Name)
	page := f.page(t, "/attention"+q)
	sid := strconv.FormatInt(id, 10)
	if !strings.Contains(page, `hx-post="/attention/asks/`+sid+`/answer"`) {
		t.Fatalf("the filtered page draws no answer form for ask %d", id)
	}
	body, hdr := form(url.Values{"option": {"ship it"}})
	w := f.call(t, http.MethodPost, "/attention/asks/"+sid+"/answer", body, merge(merge(f.operator(), hdr), map[string]string{asksCardHeader: "1"}))
	if w.Code != http.StatusNoContent {
		t.Fatalf("the card's answer: %d %s", w.Code, w.Body)
	}
	if a := f.askStatus(t, id); a.Status != attention.Answered || a.Answer != "ship it" {
		t.Errorf("the ask is %s %q, want answered \"ship it\"", a.Status, a.Answer)
	}
	if got := asksListed(f.page(t, "/attention/items"+q)); got[id] || !got[other] {
		t.Errorf("after the answer the filtered list holds %v, want %d alone", got, other)
	}
	if _, n := badgeOf(t, f.page(t, "/attention/asker"+q)); n != 1 {
		t.Errorf("after the answer the badge counts %d, want 1", n)
	}
}

// TestAReturnToASessionPageLandsOnTheQueue: a session page draws no answer
// form any more, so a `return` naming one (an agent's page, a window's view)
// is not followed — the answer is recorded and the browser lands on the queue,
// as a form on /attention does — and neither is any other page that draws no
// form; an ask's own page still is.
func TestAReturnToASessionPageLandsOnTheQueue(t *testing.T) {
	f := newAttnFixture(t)
	post := func(id int64, verb string, v url.Values) string {
		t.Helper()
		body, hdr := form(v)
		w := f.call(t, http.MethodPost, "/attention/asks/"+strconv.FormatInt(id, 10)+"/"+verb, body, merge(f.operator(), hdr))
		if w.Code != http.StatusSeeOther {
			t.Fatalf("%s %d: %d %s", verb, id, w.Code, w.Body)
		}
		return w.Header().Get("Location")
	}
	for _, c := range []struct {
		verb, ret, want string
	}{
		{"answer", "/agents/" + f.op.Name, "/attention"},
		{"cancel", "/agents/" + f.op.Name, "/attention"},
		{"answer", "/mirror/view?host=ws&session=main&window=%401", "/attention"},
		{"answer", "", "/attention"},
		{"answer", "https://evil.example.test/agents/x", "/attention"},
		{"answer", "/tasks", "/attention"},
	} {
		id := f.ask(t, f.opTok, "Where do I land? "+c.verb+" "+c.ret)
		v := url.Values{"answer": {"here"}}
		if c.ret != "" {
			v.Set("return", c.ret)
		}
		if got := post(id, c.verb, v); got != c.want {
			t.Errorf("%s with return %q lands on %q, want %q", c.verb, c.ret, got, c.want)
		}
		if a := f.askStatus(t, id); a.Status == attention.Open {
			t.Errorf("%s with return %q left the ask open", c.verb, c.ret)
		}
	}
	// The control: an ask's own page is still a page the form returns to.
	id := f.ask(t, f.opTok, "Back to my own page?")
	if got := post(id, "answer", url.Values{"answer": {"yes"}, "return": {askPagePath(id)}}); got != askPagePath(id) {
		t.Errorf("an answer from the ask's page lands on %q, want %q", got, askPagePath(id))
	}
}

// TestTheChiefsTaskAskOffersItsActionsOnItsFilteredAsksPage: the chief's
// task_route_attention ask is counted by the chief page's badge, and the Asks
// page filtered to the chief (where the badge goes) offers the task actions as
// its buttons (task_dispatch here: the owner opted in and nobody works the
// task).
func TestTheChiefsTaskAskOffersItsActionsOnItsFilteredAsksPage(t *testing.T) {
	f := newTaskRouteFixture(t, true, true)
	n := f.task(t, "who takes it")
	res := f.call(f.chief, taskRouteAttentionTool, map[string]any{"task_id": n.ID, "note": "Who should take it?"})
	var out struct {
		ID      int64    `json:"id"`
		Offered []string `json:"actions_offered"`
	}
	if err := json.Unmarshal([]byte(res), &out); err != nil || out.ID <= 0 || !containsStr(out.Offered, agentDispatchTool) {
		t.Fatalf("task_route_attention answered %s", res)
	}
	// The chief's page, as its owner (a member: the handler itself, under the
	// owner's principal, as requireSession would call it).
	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil).WithContext(f.asOwner)
		r.SetPathValue("name", f.chief.Name)
		w := httptest.NewRecorder()
		f.srv.handleAgentDetail(w, r)
		return w
	}
	w := get("/agents/" + f.chief.Name)
	if w.Code == http.StatusSeeOther {
		w = get(w.Header().Get("Location"))
	}
	if w.Code != http.StatusOK {
		t.Fatalf("the chief's page: %d", w.Code)
	}
	href, count := badgeOf(t, w.Body.String())
	if count != 1 || href != "/attention?from="+url.QueryEscape("agent:"+f.chief.Name) {
		t.Fatalf("the chief's badge: %q counting %d", href, count)
	}
	r := httptest.NewRequest(http.MethodGet, href, nil).WithContext(f.asOwner)
	pw := httptest.NewRecorder()
	f.srv.handleAttentionPage(pw, r)
	page := pw.Body.String()
	if !asksListed(page)[out.ID] {
		t.Fatalf("the chief's filtered Asks page does not list its ask %d", out.ID)
	}
	for _, want := range []string{`data-attention-option>agent_dispatch on pod/`, `hx-post="/attention/asks/` + strconv.FormatInt(out.ID, 10) + `/answer"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the chief's filtered Asks page lacks %q", want)
		}
	}
}

// TestTheAskerKeyRoundTrips: a session page's key (the ?from= value) parses
// back to the page's own reference — a tmux session name with a colon kept
// whole, the window's id kept — and a malformed key names nothing.
func TestTheAskerKeyRoundTrips(t *testing.T) {
	for _, r := range []askerRef{
		{agent: "keen-wren"},
		{mirror: ui.MirrorRef{Host: "ws", Session: "main", Window: "@1"}},
		{mirror: ui.MirrorRef{Host: "laptop", Session: "a:b:c", Window: "@12"}},
		{mirror: ui.MirrorRef{Host: "ws", Session: "per-session"}},
	} {
		got, ok := parseAskerKey(r.key())
		if !ok || got != r {
			t.Errorf("%q parses to %+v %v, want %+v", r.key(), got, ok, r)
		}
	}
	for _, k := range []string{"", "agent:", "window:", "window:ws:@1", "window::@1:main", "window:ws:@1:", "keen-wren", "Agent:x"} {
		if r, ok := parseAskerKey(k); ok {
			t.Errorf("%q names %+v", k, r)
		}
	}
}
