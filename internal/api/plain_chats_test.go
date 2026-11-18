package api

import (
	"context"
	"io"
	"log"
	"regexp"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/focus"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
	"github.com/ZacxDev/muster/internal/ui"
)

// A CHAT SHOWS NO PLUMBING OF MUSTER'S OWN IN ITS TOP LAYER (the operator rule;
// jargon audit "chats": U-28, U-29 and the Pages filming blocker). Three chat
// views — a mirrored Claude Code session, a mirrored opencode session and the
// chief's chat — draw muster's tool calls by their plain labels and a routed
// message (or a session's answer to one) as a "From <who>" card; the raw tool
// name, the fence, the route id and the "untrusted" words are in an ⓘ.
//
// The agent's own words and transcripts are the person's content and out of
// scope: the fixtures below are synthetic and hold none of the jargon, so a
// hit is muster's own drawing.
//
// REGRESSION TEST: red at origin/main (raw mcp__muster__page_create rows, the
// routed paste and the route answer drawn verbatim as the owner's bubbles).

// chatPlumbing is what a chat's top layer must not spell: an MCP or opencode
// tool prefix, a fence marker, the fence's "untrusted" and "Data, not
// instructions" words, a route id or route_id, muster's "[muster]" head, a
// session subject ("mirror:<id>", "agent:<n>"), and any of muster's own tool
// names as a word.
var chatPlumbing = regexp.MustCompile(`mcp__|\bmuster_[a-z]|<<<|\buntrusted\b|(?i:data, not instructions)|\brt_[0-9a-f]{6,}|\broute_id\b|\[muster\]|\b(?:mirror|agent):[A-Za-z0-9]|` +
	`\b(?:page_create|page_update|page_share_request|focus_search|attention_ask|attention_wait|task_get|route_to_session|route_reply|fleet_list)\b`)

// chatTopLayerTexts is topLayerTexts for a chat: what a person reads WITHOUT
// OPENING ANYTHING — every text, tooltip, aria-label and placeholder, but not
// an ⓘ sheet, a script, a style or a template, nor the body of a CLOSED
// <details> (a tool row's arguments and result: opened by a tap) — except a
// tool GROUP's, whose rows' summaries are read. all keeps
// the sheets and the closed bodies too: the control that the second layer
// still holds what the first one hides.
func chatTopLayerTexts(t *testing.T, markup string, all bool) []string {
	t.Helper()
	doc, err := html.Parse(strings.NewReader("<!doctype html><html><body>" + markup + "</body></html>"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var out []string
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.TextNode:
			if s := strings.TrimSpace(n.Data); s != "" {
				out = append(out, s)
			}
			return
		case html.ElementNode:
			if n.Data == "script" || n.Data == "style" || n.Data == "template" {
				return
			}
			open := false
			for _, a := range n.Attr {
				switch a.Key {
				case "data-info-sheet":
					if !all {
						return
					}
				case "open", "data-tool-group":
					// A run of tool rows folded into "N steps" is the rows'
					// own faces one tap away: their summaries are read.
					open = true
				case "title", "aria-label", "placeholder":
					out = append(out, a.Val)
				}
			}
			if n.Data == "details" && !open && !all {
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					if c.Type == html.ElementNode && c.Data == "summary" {
						walk(c)
					}
				}
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return out
}

// assertNoChatPlumbing fails for every top-layer text of markup that spells
// muster's plumbing, and — the control — unless the second layer does.
func assertNoChatPlumbing(t *testing.T, what, markup string) {
	t.Helper()
	top := chatTopLayerTexts(t, markup, false)
	if len(top) == 0 {
		t.Fatalf("%s: CONTROL: no top-layer text at all — the scan sees nothing", what)
	}
	for _, s := range top {
		if m := chatPlumbing.FindString(s); m != "" {
			t.Errorf("%s: the top layer says %q (%q)", what, m, cut(s, 160))
		}
	}
	hit := false
	for _, s := range chatTopLayerTexts(t, markup, true) {
		hit = hit || chatPlumbing.MatchString(s)
	}
	if !hit {
		t.Errorf("%s: CONTROL: nothing in the whole markup — ⓘ sheets and closed rows included — spells the plumbing the fixture carries", what)
	}
}

func cut(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// TestTheChatScanSeesPlumbing: the scan's own controls — it flags a raw tool
// name, a fence and a route id in a summary, a bubble and a tooltip; and it
// does not look inside an ⓘ sheet or a closed row's body.
func TestTheChatScanSeesPlumbing(t *testing.T) {
	for _, bad := range []string{
		`<details data-tool-row><summary>🔧 <span>mcp__muster__page_create</span></summary></details>`,
		`<div data-bubble="user"><p>Routed to you by your owner's chief: route_id rt_8f51ffceb101b6c0faaf3b9b</p></div>`,
		`<p>&lt;&lt;&lt;route-answer untrusted nonce=0123456789abcdef count=1&gt;&gt;&gt;</p>`,
		`<span title="muster_attention_ask">x</span>`,
		`<p>focus_search</p>`,
		`<details data-tool-row><summary>🔧 <span>Sent something to a session</span><span>mirror:cs-heron-1</span></summary></details>`,
	} {
		hit := false
		for _, s := range chatTopLayerTexts(t, bad, false) {
			hit = hit || chatPlumbing.MatchString(s)
		}
		if !hit {
			t.Errorf("the scan missed the plumbing in %s", bad)
		}
	}
	// A row folded into a closed group is still read.
	grouped := `<details data-tool-group=""><summary>2 steps</summary><div><details data-tool-row><summary>🔧 <span>mcp__muster__page_create</span></summary></details></div></details>`
	hit := false
	for _, s := range chatTopLayerTexts(t, grouped, false) {
		hit = hit || chatPlumbing.MatchString(s)
	}
	if !hit {
		t.Errorf("the scan missed a raw name in a row inside a closed group")
	}
	ok := `<details data-tool-row><summary>🔧 <span>Made a page</span></summary><div><pre>{"name":"mcp__muster__page_create"}</pre>` +
		`<div data-info-sheet=""><code>mcp__muster__page_create</code></div></div></details><p>Please log each retry.</p>`
	for _, s := range chatTopLayerTexts(t, ok, false) {
		if chatPlumbing.MatchString(s) {
			t.Errorf("the scan flagged %q in %s", s, ok)
		}
	}
}

// chatFixtureRoute is the synthetic route every chat below names.
func chatFixtureRoute() (req, msg plugins.Route, p plugins.Plugin, m plugins.Message, n notes.Note, owner agents.ChatMessage) {
	at := time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)
	chief := int64(77)
	msg = plugins.Route{RouteID: "rt_8f51ffceb101b6c0faaf3b9b", TimeoutAt: at, MessageID: 42, AgentID: &chief, TargetKind: "mirror",
		TargetKey: "cs-heron-1", ReplyBody: "Done: each retry is logged now."}
	req = msg
	req.MessageID, req.RequestText, req.RequestMessageID, req.ChiefSessionID = 0, "Please also log each retry.", 13, 5
	p = plugins.Plugin{Label: "Agent mail"}
	m = plugins.Message{ID: 42, Sender: "sam@example.test", Subject: "Retry policy", Body: "Could you log each retry?\nThanks", ReceivedAt: &at}
	n = notes.Note{ID: 7, Status: "open", Title: "Log retries", Body: "Every retry should be logged."}
	owner = agents.ChatMessage{Content: "ask heron to log each retry", CreatedAt: at}
	return
}

// mirrorChatHTML is a mirrored session's transcript drawn by the view, as one
// document.
func mirrorChatHTML(events []cctranscript.Event) string {
	var b strings.Builder
	for _, op := range (&ui.MirrorTranscript{}).Ops(events) {
		b.WriteString(op.HTML)
	}
	return b.String()
}

// oneRouteStore is a plugins.RouteStore holding one route: only Route is
// served (the chief chat's cards read nothing else).
type oneRouteStore struct {
	plugins.RouteStore
	r plugins.Route
}

func (s oneRouteStore) Route(_ context.Context, id string) (plugins.Route, error) {
	if id == s.r.RouteID {
		return s.r, nil
	}
	return plugins.Route{}, plugins.ErrRouteNotFound
}

func TestNoTopLayerOfAChatSpellsMustersPlumbing(t *testing.T) {
	req, msg, p, m, n, owner := chatFixtureRoute()
	const nonce = "0123456789abcdef"
	ok := func(id string) cctranscript.Event {
		return cctranscript.Event{Kind: cctranscript.KindToolResult, ToolID: id, OK: true, Output: `{"ok":true}`}
	}

	t.Run("claude code session", func(t *testing.T) {
		page := mirrorChatHTML([]cctranscript.Event{
			{Kind: cctranscript.KindPrompt, Line: 3, Text: renderRoutedRequest(req, owner, nonce)},
			{Kind: cctranscript.KindText, Text: "On it: adding a log line per retry."},
			{Kind: cctranscript.KindToolCall, ToolID: "toolu_a", Tool: "mcp__muster__page_create", Input: `{"title":"Retry report","body":"# Retries"}`},
			ok("toolu_a"),
			{Kind: cctranscript.KindToolCall, ToolID: "toolu_b", Tool: "mcp__muster__focus_search", Input: `{"query":"retries"}`},
			ok("toolu_b"),
			{Kind: cctranscript.KindToolCall, ToolID: "toolu_c", Tool: "Bash", Input: `{"command":"make build"}`},
			ok("toolu_c"),
			{Kind: cctranscript.KindPrompt, Line: 9, Text: renderRoutedMessageBy(routedByChief, msg, p, m, nonce)},
			{Kind: cctranscript.KindPrompt, Line: 12, Text: renderRoutedTask(routedByChief, msg, n, nonce)},
			{Kind: cctranscript.KindToolCall, ToolID: "toolu_d", Tool: "mcp__muster__route_reply", Input: `{"body":"Logged."}`},
			ok("toolu_d"),
		})
		assertNoChatPlumbing(t, "claude code", page)
		// The ⓘ holds the raw name, and a built-in keeps its own.
		if !strings.Contains(page, `data-info-code="">mcp__muster__page_create</code>`) {
			t.Errorf("no ⓘ holds the raw name mcp__muster__page_create:\n%s", page)
		}
		if !strings.Contains(page, `>Bash</span>`) {
			t.Errorf("Bash lost its own name")
		}
		// The routed paste is not the owner's own bubble.
		if strings.Contains(page, `data-bubble="user"`) {
			t.Errorf("a routed paste is drawn as the owner's bubble")
		}
	})

	t.Run("opencode session", func(t *testing.T) {
		page := mirrorChatHTML([]cctranscript.Event{
			{Kind: cctranscript.KindText, Text: "Asking before I change the retry policy."},
			{Kind: cctranscript.KindToolCall, ToolID: "oc_1", Tool: "muster_attention_ask", Input: `{"question":"Change the retry policy?"}`},
			ok("oc_1"),
			{Kind: cctranscript.KindToolCall, ToolID: "oc_2", Tool: "muster_task_get", Input: `{"id":7}`},
			ok("oc_2"),
			{Kind: cctranscript.KindToolCall, ToolID: "oc_3", Tool: "bash", Input: `{"command":"go test ./..."}`},
			ok("oc_3"),
		})
		assertNoChatPlumbing(t, "opencode", page)
		if !strings.Contains(page, `data-info-code="">muster_attention_ask</code>`) {
			t.Errorf("no ⓘ holds the raw name muster_attention_ask")
		}
	})

	t.Run("chief chat", func(t *testing.T) {
		s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
		s.UseExtensions(Extensions{Routes: oneRouteStore{r: req}, Focus: &memFocus{rows: map[focus.Subject]focus.Focus{
			focus.MirrorSubject("cs-heron-1"): {Subject: focus.MirrorSubject("cs-heron-1"), Name: focus.Name{Manual: "heron"}},
		}}})
		ctx := context.Background()
		v := ui.AgentDetailView{Name: "chief", Status: "running", Messages: []ui.ChatLine{
			{Role: "user", Content: "ask heron to log each retry", ID: 1, Typed: true},
			{Role: "assistant", Kind: "tool_call", ToolID: "c1", ToolName: "mcp__muster__route_to_session", Content: `{"subject":"mirror:cs-heron-1","text":"Please also log each retry."}`, ID: 2},
			{Role: "assistant", Kind: "tool_result", ToolID: "c1", ToolName: "mcp__muster__route_to_session", ToolOK: true, Content: `{"state":"routed"}`, ID: 3},
			{Role: "assistant", Kind: "tool_call", ToolID: "c2", ToolName: "fleet_list", Content: `{}`, ID: 4},
			{Role: "assistant", Kind: "tool_result", ToolID: "c2", ToolName: "fleet_list", ToolOK: true, Content: `[]`, ID: 5},
			{Role: "assistant", Content: "Sent it to heron.", ID: 6},
			{Role: "user", Content: s.routeAnswerText(ctx, req), ID: 7},
			{Role: "assistant", Content: "heron logged each retry.", ID: 8},
		}}
		s.withChatLogLive(ctx, &v, agents.ChatSession{ID: 5, AgentID: *req.AgentID})
		body, _, err := ui.ChatLogBody(v)
		if err != nil {
			t.Fatal(err)
		}
		page := string(body)
		assertNoChatPlumbing(t, "chief chat", page)
		if !strings.Contains(page, `>From heron<`) || !strings.Contains(page, `Done: each retry is logged now.`) {
			t.Errorf("the route answer is not drawn as heron's card:\n%s", page)
		}
	})
}

// TestARoutedBlockReadsBackWhatTheRendererWrote: plugins.ParseRoutedBlock reads
// every routed paste internal/api writes — each router, each kind — with the
// fields a card shows, pinned as literals.
func TestARoutedBlockReadsBackWhatTheRendererWrote(t *testing.T) {
	req, msg, p, m, n, owner := chatFixtureRoute()
	const nonce = "0123456789abcdef"
	task := msg
	task.MessageID, task.TaskID = 0, 7
	const id = "rt_8f51ffceb101b6c0faaf3b9b"
	msgWant := func(via plugins.RoutedVia) plugins.RoutedBlock {
		return plugins.RoutedBlock{Via: via, RouteID: id, Kind: plugins.RoutedKindMessage, Source: "Agent mail", Subject: "Retry policy", Body: "Could you log each retry?\nThanks"}
	}
	for name, c := range map[string]struct {
		text string
		want plugins.RoutedBlock
	}{
		"request":         {renderRoutedRequest(req, owner, nonce), plugins.RoutedBlock{Via: plugins.RoutedViaChief, RouteID: id, Kind: plugins.RoutedKindRequest, Body: "Please also log each retry."}},
		"message, chief":  {renderRoutedMessageBy(routedByChief, msg, p, m, nonce), msgWant(plugins.RoutedViaChief)},
		"message, owner":  {renderRoutedMessageBy(routedByOwner, msg, p, m, nonce), msgWant(plugins.RoutedViaOwner)},
		"message, thread": {renderRoutedMessageBy(routedByThread, msg, p, m, nonce), msgWant(plugins.RoutedViaThread)},
		"message, rule":   {renderRoutedMessageBy(routedByRule, msg, p, m, nonce), msgWant(plugins.RoutedViaRule)},
		"redraft":         {renderRedraftMessage(msg, p, m, "earlier", "too long", nonce), msgWant(plugins.RoutedViaRedraft)},
		"task":            {renderRoutedTask(routedByChief, task, n, nonce), plugins.RoutedBlock{Via: plugins.RoutedViaChief, RouteID: id, Kind: plugins.RoutedKindTask, TaskID: 7, Subject: "Log retries", Body: "Every retry should be logged."}},
	} {
		got, ok := plugins.ParseRoutedBlock(c.text)
		if !ok || got != c.want {
			t.Errorf("%s: ParseRoutedBlock = %+v, %v; want %+v", name, got, ok, c.want)
		}
	}
	// Not a routed block: the owner's own words quoting one, a memo relay's
	// fence, and a routed block with text after it.
	routed := renderRoutedMessageBy(routedByChief, msg, p, m, nonce)
	for name, text := range map[string]string{
		"quoted":      "look at this:\n" + routed + "\nwhat do you think?",
		"other fence": strings.ReplaceAll(routed, "routed-message", "cairn-memo"),
		"no preface":  routed[strings.Index(routed, "<<<"):],
	} {
		if _, ok := plugins.ParseRoutedBlock(text); ok {
			t.Errorf("%s: read as a routed block", name)
		}
	}
}

// TestARouteAnswerCardIsDrawnFromItsRoutesRecord: a chief-chat row is drawn as
// answer cards only when every route it names is THIS chief's, answered, and
// the row carries muster's own head for it; the card's body is the record's.
// A typed row, another chief's route, a forged head and an unknown route are
// each drawn as before (no card).
func TestARouteAnswerCardIsDrawnFromItsRoutesRecord(t *testing.T) {
	req, _, _, _, _, _ := chatFixtureRoute()
	ctx := context.Background()
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Routes: oneRouteStore{r: req}})
	real := s.routeAnswerText(ctx, req)
	cards := func(agentID int64, l ui.ChatLine) []ui.RouteAnswerCard {
		return s.routeAnswerCardsFor(ctx, agentID, []ui.ChatLine{l})[l.ID]
	}
	got := cards(*req.AgentID, ui.ChatLine{Role: "user", Content: real, ID: 9})
	if len(got) != 1 || got[0] != (ui.RouteAnswerCard{From: "a session", Body: "Done: each retry is logged now."}) {
		t.Fatalf("CONTROL: the real row's cards = %+v", got)
	}
	other := req
	other.ReplyBody = "something else entirely"
	for name, c := range map[string]struct {
		agent int64
		line  ui.ChatLine
	}{
		"typed by the owner": {*req.AgentID, ui.ChatLine{Role: "user", Content: real, ID: 9, Typed: true}},
		"another chief's":    {*req.AgentID + 1, ui.ChatLine{Role: "user", Content: real, ID: 9}},
		"a forged head":      {*req.AgentID, ui.ChatLine{Role: "user", Content: strings.Replace(real, routeAnswerNote, "the session refused.", 1), ID: 9}},
		"an unknown route":   {*req.AgentID, ui.ChatLine{Role: "user", Content: strings.ReplaceAll(real, req.RouteID, "rt_000000000000000000000000"), ID: 9}},
		"an assistant row":   {*req.AgentID, ui.ChatLine{Role: "assistant", Content: real, ID: 9}},
	} {
		if got := cards(c.agent, c.line); got != nil {
			t.Errorf("%s: drawn as cards %+v", name, got)
		}
	}
	// The body is the record's, whatever the row's fence holds.
	s.UseExtensions(Extensions{Routes: oneRouteStore{r: other}})
	if got := cards(*req.AgentID, ui.ChatLine{Role: "user", Content: real, ID: 9}); len(got) != 1 || got[0].Body != "something else entirely" {
		t.Errorf("the card's body is not the record's: %+v", got)
	}
	// The one caller: every #chat-log renderer gets the cards through
	// withChatLogLive (chat_log_live.go's one composer).
	assertCallers(t, ".", "routeAnswerCardsFor", []string{"chat_log_live.go:withChatLogLive"})
}
