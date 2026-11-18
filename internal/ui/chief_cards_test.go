package ui

import (
	"github.com/ZacxDev/muster/internal/sessionname"

	"bytes"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// --- small HTML helpers for these tests --------------------------------------

func ccParse(t *testing.T, markup string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader("<div id=root>" + markup + "</div>"))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func ccAttr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

// withAttr is every element under n carrying key (and, when val is not "",
// equal to val).
func ccWithAttr(n *html.Node, key, val string) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(c *html.Node) {
		if c.Type == html.ElementNode {
			if v, ok := ccAttr(c, key); ok && (val == "" || v == val) {
				out = append(out, c)
			}
		}
		for k := c.FirstChild; k != nil; k = k.NextSibling {
			walk(k)
		}
	}
	walk(n)
	return out
}

func ccText(n *html.Node) string {
	var b bytes.Buffer
	var walk func(*html.Node)
	walk = func(c *html.Node) {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
		for k := c.FirstChild; k != nil; k = k.NextSibling {
			walk(k)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

func ccRenderLog(t *testing.T, v AgentDetailView) *html.Node {
	t.Helper()
	body, _, err := ChatLogBody(v)
	if err != nil {
		t.Fatal(err)
	}
	return ccParse(t, string(body))
}

const askArgs = `{"question":"Where should I run this agent?","options":[` +
	`{"label":"Claude","description":"Claude Code, on your Claude account"},` +
	`{"label":"deepseek/deepseek-v3"},{"label":"qwen/qwen3-coder"}]}`

func askRows(name string, extra ...ChatLine) []ChatLine {
	return append([]ChatLine{
		{Role: "user", Content: "dispatch an agent for the flaky test", Typed: true, ID: 1},
		{Role: "assistant", Kind: "tool_call", ToolID: "call_ask_1", ToolName: name, Content: askArgs, ID: 2},
		{Role: "assistant", Kind: "tool_result", ToolID: "call_ask_1", ToolName: name, ToolOK: true, Content: `{"asked":true}`, ID: 3},
	}, extra...)
}

// --- ask_owner ----------------------------------------------------------------

// TestAnOwnerAskIsDrawnAsAQuestionCard: a stored ask_owner call (as a gateway
// chief names it, and as a claude-code chief does over MCP) is a CARD, not a
// tool row: the question, one real button per option carrying the label it
// sends, and an "Other" box with a label; labelled for a screen reader by its
// question. While nothing typed follows it, it is waiting.
func TestAnOwnerAskIsDrawnAsAQuestionCard(t *testing.T) {
	for _, name := range []string{"ask_owner", "mcp__muster__ask_owner"} {
		doc := ccRenderLog(t, AgentDetailView{Name: "chief", Messages: askRows(name)})
		cards := ccWithAttr(doc, "data-owner-ask", "")
		if len(cards) != 1 {
			t.Fatalf("%s: %d question cards, want 1", name, len(cards))
		}
		card := cards[0]
		if st, _ := ccAttr(card, "data-owner-ask-state"); st != OwnerAskWaiting {
			t.Errorf("%s: state %q, want %q", name, st, OwnerAskWaiting)
		}
		if n := len(ccWithAttr(doc, "data-tool-row", "")); n != 0 {
			t.Errorf("%s: the call is ALSO drawn as %d tool row(s)", name, n)
		}
		q := ccWithAttr(card, "data-owner-ask-question", "")
		if len(q) != 1 || ccText(q[0]) != "Where should I run this agent?" {
			t.Fatalf("%s: question %v", name, q)
		}
		qid, _ := ccAttr(q[0], "id")
		if by, _ := ccAttr(card, "aria-labelledby"); by == "" || by != qid {
			t.Errorf("%s: the card is labelled by %q, its question's id is %q", name, by, qid)
		}
		var labels []string
		for _, b := range ccWithAttr(card, "data-owner-ask-pick", "") {
			if b.Data != "button" {
				t.Errorf("%s: an option is a <%s>, not a button", name, b.Data)
			}
			if ty, _ := ccAttr(b, "type"); ty != "button" {
				t.Errorf("%s: an option button is type %q: it would submit a form", name, ty)
			}
			v, _ := ccAttr(b, "data-owner-ask-pick")
			labels = append(labels, v)
		}
		if got := strings.Join(labels, "|"); got != "Claude|deepseek/deepseek-v3|qwen/qwen3-coder" {
			t.Errorf("%s: option buttons send %q", name, got)
		}
		other := ccWithAttr(card, "data-owner-ask-other", "")
		if len(other) != 1 {
			t.Fatalf("%s: %d Other boxes, want 1", name, len(other))
		}
		oid, _ := ccAttr(other[0], "id")
		found := false
		for _, l := range ccWithAttr(card, "for", oid) {
			found = found || ccText(l) == "Other"
		}
		if !found {
			t.Errorf("%s: the Other box has no <label for> reading Other", name)
		}
		forms := ccWithAttr(card, "data-owner-ask-form", "")
		if len(forms) != 1 {
			t.Fatalf("%s: %d forms", name, len(forms))
		}
		if b, _ := ccAttr(forms[0], "hx-boost"); b != "false" {
			t.Errorf("%s: the card's form is boostable (hx-boost=%q): htmx would send it as a page request", name, b)
		}
	}
}

// TestAnOwnerAskIsAnsweredByTheNextTypedLine: the card's state is read from the
// transcript. A line a person TYPED after it answers it — the card is drawn
// answered, with that line as the answer and the matching option marked, and
// no controls. A line nobody typed (a plugin message, a hand-back) does not.
func TestAnOwnerAskIsAnsweredByTheNextTypedLine(t *testing.T) {
	untyped := askRows("ask_owner", ChatLine{Role: "user", Content: "plugin: a message from outside", ID: 4})
	doc := ccRenderLog(t, AgentDetailView{Name: "chief", Messages: untyped})
	if st, _ := ccAttr(ccWithAttr(doc, "data-owner-ask", "")[0], "data-owner-ask-state"); st != OwnerAskWaiting {
		t.Fatalf("an untyped line answered the card (state %q)", st)
	}

	typed := askRows("ask_owner", ChatLine{Role: "user", Content: "deepseek/deepseek-v3", Typed: true, ID: 4},
		ChatLine{Role: "assistant", Content: "Starting it on deepseek.", ID: 5})
	doc = ccRenderLog(t, AgentDetailView{Name: "chief", Messages: typed})
	card := ccWithAttr(doc, "data-owner-ask", "")[0]
	if st, _ := ccAttr(card, "data-owner-ask-state"); st != OwnerAskAnswered {
		t.Fatalf("state %q, want %q", st, OwnerAskAnswered)
	}
	ans := ccWithAttr(card, "data-owner-ask-answer", "")
	if len(ans) != 1 || ccText(ans[0]) != "Your answer: deepseek/deepseek-v3" {
		t.Errorf("answer line %v", ans)
	}
	chosen := ccWithAttr(card, "data-owner-ask-chosen", "")
	if len(chosen) != 1 || !strings.HasPrefix(ccText(chosen[0]), "deepseek/deepseek-v3") {
		t.Errorf("chosen %v, want exactly the deepseek option", chosen)
	}
	for _, k := range []string{"data-owner-ask-pick", "data-owner-ask-form", "data-owner-ask-other"} {
		if n := len(ccWithAttr(card, k, "")); n != 0 {
			t.Errorf("an answered card still has %d %s control(s)", n, k)
		}
	}

	// An "Other" answer: answered, nothing marked as chosen.
	free := askRows("ask_owner", ChatLine{Role: "user", Content: "use whatever is cheapest", Typed: true, ID: 4})
	card = ccWithAttr(ccRenderLog(t, AgentDetailView{Name: "chief", Messages: free}), "data-owner-ask", "")[0]
	if st, _ := ccAttr(card, "data-owner-ask-state"); st != OwnerAskAnswered || len(ccWithAttr(card, "data-owner-ask-chosen", "")) != 0 {
		t.Errorf("a free answer: state %q, %d chosen", st, len(ccWithAttr(card, "data-owner-ask-chosen", "")))
	}
}

// TestAMultiSelectOwnerAskIsCheckboxesAndOneSend: multi draws checkboxes (no
// one-tap buttons) and one Send; its answer marks every label it lists.
func TestAMultiSelectOwnerAskIsCheckboxesAndOneSend(t *testing.T) {
	args := `{"question":"Which repos?","multi":true,"options":[{"label":"api"},{"label":"web"},{"label":"docs"}]}`
	rows := []ChatLine{
		{Role: "assistant", Kind: "tool_call", ToolID: "m1", ToolName: "ask_owner", Content: args},
		{Role: "assistant", Kind: "tool_result", ToolID: "m1", ToolName: "ask_owner", ToolOK: true, Content: `{"asked":true}`},
	}
	card := ccWithAttr(ccRenderLog(t, AgentDetailView{Name: "chief", Messages: rows}), "data-owner-ask", "")[0]
	if n := len(ccWithAttr(card, "data-owner-ask-pick", "")); n != 0 {
		t.Errorf("a multi-select card has %d one-tap buttons", n)
	}
	if n := len(ccWithAttr(card, "data-owner-ask-check", "")); n != 3 {
		t.Errorf("%d checkboxes, want 3", n)
	}
	rows = append(rows, ChatLine{Role: "user", Content: "api" + OwnerAskMultiSep + "docs", Typed: true})
	card = ccWithAttr(ccRenderLog(t, AgentDetailView{Name: "chief", Messages: rows}), "data-owner-ask", "")[0]
	var got []string
	for _, c := range ccWithAttr(card, "data-owner-ask-chosen", "") {
		got = append(got, ccText(c))
	}
	if strings.Join(got, ",") != "api,docs" {
		t.Errorf("chosen %v, want [api docs]", got)
	}
}

// TestAnUnreadableOwnerAskStaysAToolRow: a call whose arguments the tool
// refused (or a failed result) is drawn as the plain row, never a broken card.
func TestAnUnreadableOwnerAskStaysAToolRow(t *testing.T) {
	for _, rows := range [][]ChatLine{
		{{Role: "assistant", Kind: "tool_call", ToolID: "b1", ToolName: "ask_owner", Content: `{"question":""}`}},
		{
			{Role: "assistant", Kind: "tool_call", ToolID: "b2", ToolName: "ask_owner", Content: askArgs},
			{Role: "assistant", Kind: "tool_result", ToolID: "b2", ToolName: "ask_owner", ToolOK: false, Content: `{"error":"x"}`},
		},
	} {
		doc := ccRenderLog(t, AgentDetailView{Name: "chief", Messages: rows})
		if len(ccWithAttr(doc, "data-owner-ask", "")) != 0 || len(ccWithAttr(doc, "data-tool-row", "")) != 1 {
			t.Errorf("rows %+v: want one plain tool row and no card", rows)
		}
	}
}

// TestDecodeOwnerAskRefusesWhatTheCardCannotDraw: the one reading of the
// arguments refuses what would not lay out, each with its own reason.
func TestDecodeOwnerAskRefusesWhatTheCardCannotDraw(t *testing.T) {
	long := strings.Repeat("x", OwnerAskLabelMax+1)
	for args, want := range map[string]string{
		`{}`:                              "question is required",
		`{"question":"q"}`:                "options is required",
		`{"question":"q","options":[]}`:   "options is required",
		`{"question":"q","options":[{}]}`: "every option needs a label",
		`{"question":"q","options":[{"label":"a"},{"label":"A"}]}`: "two options have the label",
		`{"question":"q","options":[{"label":"` + long + `"}]}`:    "label is over",
		`{"question":"q","options":[{"label":"a"}],"kind":"x"}`:    "arguments are",
		`not json`: "arguments are",
	} {
		_, err := DecodeOwnerAsk(args)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error %v, want one containing %q", args, err, want)
		}
	}
	if a, err := DecodeOwnerAsk(askArgs); err != nil || len(a.Options) != 3 || a.Options[0].Description == "" {
		t.Errorf("a valid ask: %+v %v", a, err)
	}
}

// --- the working row -----------------------------------------------------------

// TestTheWorkingRowIsDrawnFromTheServersMark: #chat-working is drawn SHOWING
// exactly when the view says a turn is in flight, with the script's own class.
func TestTheWorkingRowIsDrawnFromTheServersMark(t *testing.T) {
	for _, inFlight := range []bool{false, true} {
		doc := ccRenderLog(t, AgentDetailView{Name: "chief", TurnInFlight: inFlight})
		w := ccWithAttr(doc, "id", "chat-working")
		if len(w) != 1 {
			t.Fatalf("%d working rows", len(w))
		}
		cls, _ := ccAttr(w[0], "class")
		_, marked := ccAttr(w[0], "data-turn-in-flight")
		shown := strings.HasPrefix(cls, "flex ")
		hidden := strings.HasPrefix(cls, "hidden ")
		if shown != inFlight || hidden == inFlight || marked != inFlight {
			t.Errorf("in flight %v: class %q, marked %v", inFlight, cls, marked)
		}
	}
}

// --- agent_dispatch -------------------------------------------------------------

func dispatchRows(result string) []ChatLine {
	return []ChatLine{
		{Role: "assistant", Kind: "tool_call", ToolID: "d1", ToolName: "agent_dispatch", Content: `{"target":"pod/gateway"}`},
		{Role: "assistant", Kind: "tool_result", ToolID: "d1", ToolName: "agent_dispatch", ToolOK: !strings.Contains(result, `"error"`), Content: result},
	}
}

// TestADispatchedAgentIsACardThatOpensIt: a start with a card composed for it
// draws the card — name, where it runs, repo, task link, its state — and the
// card's link is the agent's page href it was given; the task link sits beside
// it, never inside it. It refreshes itself on the server's events for THAT agent.
func TestADispatchedAgentIsACardThatOpensIt(t *testing.T) {
	href := AgentCardView{Name: "brisk-otter", SessionID: 41}.PageHref()
	for _, state := range []string{DispatchStarting, DispatchWorking, DispatchWaiting, DispatchDone, DispatchError, DispatchFailed, DispatchStuck} {
		c := DispatchCard{AgentID: 7, Name: "brisk-otter", SessionName: sessionname.Name{Text: "Fix the flaky test", Source: sessionname.SourceDispatch}, Href: href,
			Runtime: "Gateway · deepseek/deepseek-v3", Repo: "example-org/widgets", TaskID: 12, State: state}
		doc := ccRenderLog(t, AgentDetailView{Name: "chief", Messages: dispatchRows(`{"agent_id":7,"agent":"brisk-otter"}`),
			DispatchCards: map[string]DispatchCard{"d1": c}})
		cards := ccWithAttr(doc, "data-dispatch-card", "")
		if len(cards) != 1 {
			t.Fatalf("%s: %d cards", state, len(cards))
		}
		card := cards[0]
		if st, _ := ccAttr(card, "data-dispatch-state"); st != state {
			t.Errorf("state %q, want %q", st, state)
		}
		if p := ccWithAttr(card, "data-dispatch-status", state); len(p) != 1 || ccText(p[0]) != dispatchStateLabel(state) {
			t.Errorf("%s: status pill %v", state, p)
		}
		open := ccWithAttr(card, "data-dispatch-open", "")
		if len(open) != 1 || open[0].Data != "a" {
			t.Fatalf("%s: %d open links", state, len(open))
		}
		if h, _ := ccAttr(open[0], "href"); h != href {
			t.Errorf("%s: the card opens %q, want the agent's page %q", state, h, href)
		}
		for k, want := range map[string]string{"data-dispatch-name": "Fix the flaky test", "data-dispatch-runtime": "Gateway · deepseek/deepseek-v3",
			"data-dispatch-repo": "example-org/widgets"} {
			if n := ccWithAttr(card, k, ""); len(n) != 1 || ccText(n[0]) != want {
				t.Errorf("%s: %s = %v, want %q", state, k, n, want)
			}
		}
		task := ccWithAttr(card, "data-dispatch-task", "12")
		if len(task) != 1 {
			t.Fatalf("%s: no task link", state)
		}
		if h, _ := ccAttr(task[0], "href"); h != "/tasks/12" {
			t.Errorf("task link %q", h)
		}
		for p := task[0].Parent; p != nil; p = p.Parent {
			if p == open[0] {
				t.Errorf("the task link is inside the card's link")
			}
		}
		live := card.Parent
		if _, ok := ccAttr(live, DispatchLiveAttr); !ok {
			t.Fatalf("%s: the card is not inside its refreshing wrapper", state)
		}
		if g, _ := ccAttr(live, "hx-get"); g != DispatchCardSrc(7) {
			t.Errorf("the card refreshes from %q", g)
		}
		if tr, _ := ccAttr(live, "hx-trigger"); !strings.Contains(tr, `id==="brisk-otter"`) || !strings.Contains(tr, "sse:agent.changed") {
			t.Errorf("the card's trigger %q is not its own agent's events", tr)
		}
	}
}

// TestARefusedDispatchIsOneCompactLine: a refusal draws its sentence, with no
// link and no refresh — from the result alone, whether or not a card was
// composed for it.
func TestARefusedDispatchIsOneCompactLine(t *testing.T) {
	const why = "target is required and has no default, and your owner named no model"
	for _, cards := range []map[string]DispatchCard{nil, {"d1": {Refused: why}}} {
		doc := ccRenderLog(t, AgentDetailView{Name: "chief", Messages: dispatchRows(`{"error":"` + why + `","targets":{}}`), DispatchCards: cards})
		c := ccWithAttr(doc, "data-dispatch-card", "")
		if len(c) != 1 {
			t.Fatalf("%d cards", len(c))
		}
		if st, _ := ccAttr(c[0], "data-dispatch-state"); st != "refused" {
			t.Errorf("state %q", st)
		}
		if r := ccWithAttr(c[0], "data-dispatch-refused", ""); len(r) != 1 || ccText(r[0]) != why {
			t.Errorf("refusal text %v", r)
		}
		if len(ccWithAttr(c[0], "href", "")) != 0 || len(ccWithAttr(c[0], "hx-get", "")) != 0 {
			t.Errorf("a refusal carries a link or a refresh")
		}
	}
}

// TestARemovedDispatchedAgentLinksNowhere: an agent that no longer exists is
// drawn as removed, with no link and no refresh.
func TestARemovedDispatchedAgentLinksNowhere(t *testing.T) {
	doc := ccRenderLog(t, AgentDetailView{Name: "chief", Messages: dispatchRows(`{"agent_id":9}`),
		DispatchCards: map[string]DispatchCard{"d1": {AgentID: 9, Name: "Agent #9", Gone: true}}})
	c := ccWithAttr(doc, "data-dispatch-card", "")[0]
	if len(ccWithAttr(c, "href", "")) != 0 || len(ccWithAttr(c, "hx-get", "")) != 0 {
		t.Error("a removed agent's card links or refreshes")
	}
	if p := ccWithAttr(c, "data-dispatch-status", ""); len(p) != 1 || ccText(p[0]) != "Removed" {
		t.Errorf("status %v", p)
	}
}

// TestReadDispatchResult: the one reading of agent_dispatch's result.
func TestReadDispatchResult(t *testing.T) {
	for out, want := range map[string]DispatchResult{
		`{"agent_id":5,"agent":"x","budget":{}}`:  {AgentID: 5},
		`{"agent_id":5,"error":"recorded badly"}`: {AgentID: 5},
		`{"error":"no target"}`:                   {Refused: "no target"},
	} {
		if got, ok := ReadDispatchResult(out); !ok || got != want {
			t.Errorf("%s: %+v %v, want %+v", out, got, ok, want)
		}
	}
	for _, out := range []string{`{"replayed":true,"status":"starting"}`, `not json`, `{"already_dispatched":true}`} {
		if _, ok := ReadDispatchResult(out); ok {
			t.Errorf("%s read as a card", out)
		}
	}
}

// TestTwoAsksBeforeOneReplyDrawOneCard (#496 round 1): one typed reply answers
// every question asked before it, so a second ask_owner since the owner last
// typed is drawn as its plain row — never a second card the same reply answers.
// After a typed reply, the next ask is a card again.
func TestTwoAsksBeforeOneReplyDrawOneCard(t *testing.T) {
	call := func(id string) []ChatLine {
		return []ChatLine{
			{Role: "assistant", Kind: "tool_call", ToolID: id, ToolName: "ask_owner", Content: askArgs},
			{Role: "assistant", Kind: "tool_result", ToolID: id, ToolName: "ask_owner", ToolOK: true, Content: `{"asked":true}`},
		}
	}
	rows := append(append([]ChatLine{{Role: "user", Content: "go", Typed: true}}, call("a1")...), call("a2")...)
	doc := ccRenderLog(t, AgentDetailView{Name: "chief", Messages: rows})
	if cards := ccWithAttr(doc, "data-owner-ask", ""); len(cards) != 1 {
		t.Fatalf("%d cards for two asks before one reply, want 1", len(cards))
	} else if id, _ := ccAttr(cards[0], "data-tool-id"); id != "a1" {
		t.Errorf("the card is %q's, want the first ask's", id)
	}
	if n := len(ccWithAttr(doc, "data-tool-row", "")); n != 1 {
		t.Errorf("the second ask is drawn as %d tool rows, want 1", n)
	}
	rows = append(append(rows, ChatLine{Role: "user", Content: "Claude", Typed: true}), call("a3")...)
	if cards := ccWithAttr(ccRenderLog(t, AgentDetailView{Name: "chief", Messages: rows}), "data-owner-ask", ""); len(cards) != 2 {
		t.Errorf("after a typed reply the next ask is not a card again: %d cards, want 2", len(cards))
	}
}

// TestAnUnreadableDispatchedAgentStaysRefreshable (#496 round 1): a card whose
// agent could not be READ (not a missing row) does not say "Removed": it says
// so, links nowhere, and keeps a refresh on the app's own events.
func TestAnUnreadableDispatchedAgentStaysRefreshable(t *testing.T) {
	doc := ccRenderLog(t, AgentDetailView{Name: "chief", Messages: dispatchRows(`{"agent_id":9}`),
		DispatchCards: map[string]DispatchCard{"d1": {AgentID: 9, Name: "Agent #9", Unreadable: true}}})
	c := ccWithAttr(doc, "data-dispatch-card", "")[0]
	if st, _ := ccAttr(c, "data-dispatch-state"); st != "unreadable" {
		t.Errorf("state %q, want unreadable", st)
	}
	if strings.Contains(ccText(c), "Removed") || len(ccWithAttr(c, "href", "")) != 0 {
		t.Errorf("an unreadable agent is drawn removed or linked: %s", ccText(c))
	}
	if g, _ := ccAttr(c.Parent, "hx-get"); g != DispatchCardSrc(9) {
		t.Errorf("an unreadable card does not refresh itself (hx-get %q on its wrapper)", g)
	}
}

// TestADispatchCardsNewestRefreshWins: every self-refreshing dispatch card is
// refreshed by a wrapper that never replaces itself — it swaps the answer's
// card INTO itself (hx-select, innerHTML) and queues the newest request
// behind one in flight. The card used to refresh itself with outerHTML, and a
// request queued behind the first fired from the REMOVED card: the newer state
// was never drawn (CI: a card stuck on "Starting"). The browser case "a
// dispatched agent" plays that race.
func TestADispatchCardsNewestRefreshWins(t *testing.T) {
	for _, c := range []DispatchCard{
		{AgentID: 7, Name: "brisk-otter", Href: "/agents/brisk-otter", State: DispatchWorking},
		{AgentID: 9, Name: "Agent #9", Unreadable: true},
	} {
		doc := ccRenderLog(t, AgentDetailView{Name: "chief", Messages: dispatchRows(`{"agent_id":1}`),
			DispatchCards: map[string]DispatchCard{"d1": c}})
		live := ccWithAttr(doc, DispatchLiveAttr, "")
		if len(live) != 1 {
			t.Fatalf("card %+v: %d refreshing wrappers, want 1", c, len(live))
		}
		for k, want := range map[string]string{"hx-swap": "innerHTML", "hx-select": "[data-dispatch-card]", "hx-target": "this", "hx-sync": "this:queue last"} {
			if v, _ := ccAttr(live[0], k); v != want {
				t.Errorf("card %+v: wrapper %s %q, want %q", c, k, v, want)
			}
		}
		cards := ccWithAttr(live[0], "data-dispatch-card", "")
		if len(cards) != 1 {
			t.Fatalf("card %+v: the wrapper holds %d cards", c, len(cards))
		}
		if _, ok := ccAttr(cards[0], "hx-get"); ok {
			t.Errorf("card %+v: the card refreshes itself (it would replace the element that queued its refresh)", c)
		}
	}
}
