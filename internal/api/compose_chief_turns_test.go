package api

import (
	"context"
	"maps"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/plugins"
)

// composeChiefTurnTexts is every turn text muster can send a chief that names
// a tool, rendered for tools (plugins.ChiefTools): the plugin turn in each
// shape (routing on and off, no_reply, retried, with session hints and with an
// author block), and a route's hand-back of each kind. A task event is not
// here: MUSTER_TASK_ROUTING is refused beside the Compose chief at boot
// (cmd/muster-server TestTheComposeChiefRefusesTaskRoutingBesideIt), and a
// session's question is never put to a chief without answer_session_question
// (TestASessionQuestionIsNotPutToTheComposeChief). A delivered answer to the
// chief's own attention ask (attention_delivery.go) is not either: the Compose
// chief is not offered attention_ask, so it has none to be answered.
func composeChiefTurnTexts(tools plugins.ChiefTools) map[string]string {
	p := plugins.Plugin{Label: "fixture-mail", DataScope: "fixture-scope"}
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	m := plugins.Message{ID: 41, Sender: "sender@example.test", Subject: "s", Body: "b", CreatedAt: now.Add(-time.Hour)}
	noReply := m
	noReply.NoReply = true
	hints := plugins.Hints{Window: 6 * time.Hour, Sessions: []plugins.SessionHint{{ID: "mirror:fixture-sid", Name: "fixture window",
		Titles: []plugins.HintTitle{{Title: "fixture focus", At: now}}}},
		Author: &plugins.AuthorHint{ID: "mirror:fixture-author", Kind: "gh-pr-create", Repo: "example/repo", PR: 7, LastSeen: now}}
	retry := plugins.Retry{Count: 1, Now: now}
	out := map[string]string{}
	for _, routing := range []bool{true, false} {
		r := map[bool]string{true: "routing on", false: "routing off"}[routing]
		out["plugin turn, "+r] = plugins.RenderMessage(p, m, routing, hints, retry, tools)
		out["no_reply plugin turn, "+r] = plugins.RenderMessage(p, noReply, routing, hints, retry, tools)
	}
	out["message hand-back"] = handBackChoices(handBackMessage, tools)
	out["request hand-back"] = handBackChoices(handBackRequest, tools)
	out["task hand-back"] = handBackChoices(handBackTask, tools)
	out["one-way hand-back"] = handBackChoices(handBackOneWay, tools)
	out["one-way route answer"] = oneWayAnswerPreface(tools)
	return out
}

// actionPhrases are the ways a turn names an action without its tool's name,
// each with the tool that performs it.
var actionPhrases = map[string]string{
	"reply drafts":           "reply_draft",
	"draft a reply":          "reply_draft",
	"file a task":            "task_create_gated",
	"in front of your owner": "task_route_attention",
	"file the work":          "task_create_gated",
}

// namedTools is every registry tool (and muster_help) that text names.
func namedTools(s *Server, text string) []string {
	var out []string
	for _, n := range append(toolNames(toolDefsOf(s.agentTools())), mcpHelpTool) {
		if regexp.MustCompile(`\b`+n+`\b`).MatchString(text) && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

func toolDefsOf(ts []agentTool) []agents.ToolDef {
	var out []agents.ToolDef
	for _, t := range ts {
		out = append(out, t.def)
	}
	return out
}

// TestNoComposeChiefTurnTextNamesAToolItIsNotOffered: every turn text the
// Compose chief can be sent names only tools in the set it is offered
// (ChiefTurnTools, read from its own listing). Positive control: the same
// texts rendered for an unrestricted chief DO name reply_draft,
// task_route_attention and attention_ask, so the scan can see a tool. Control:
// the Kubernetes chief's ChiefTurnTools is nil (unrestricted).
func TestNoComposeChiefTurnTextNamesAToolItIsNotOffered(t *testing.T) {
	s, chief, _ := chiefAndWorkerServer(t)
	ctx := context.Background()
	if got := s.ChiefTurnTools(ctx, chief); got != nil {
		t.Fatalf("control: the Kubernetes chief's turn tools are restricted: %v", got)
	}
	s.ext.ComposeChief = &ComposeChief{}
	tools := s.ChiefTurnTools(ctx, chief)
	var held []string
	for n := range tools {
		held = append(held, n)
	}
	if slices.Sort(held); !slices.Equal(held, composeChiefToolLedger) {
		t.Fatalf("the Compose chief's turn tools = %v, want its offered set %v", held, composeChiefToolLedger)
	}
	withoutRoute := plugins.ChiefTools{}
	for n := range tools {
		if n != routeToSessionToolName {
			withoutRoute[n] = true
		}
	}
	for label, set := range map[string]plugins.ChiefTools{"routing on": tools, "routing off": withoutRoute} {
		for name, text := range composeChiefTurnTexts(set) {
			for _, n := range namedTools(s, text) {
				if !set[n] {
					t.Errorf("%s, %s: the Compose chief's turn names %s, which it is not offered:\n%s", label, name, n, text)
				}
			}
			// An action is named without its tool's name, too.
			lower := strings.ToLower(text)
			for phrase, tool := range actionPhrases {
				if !set[tool] && strings.Contains(lower, phrase) {
					t.Errorf("%s, %s: the Compose chief's turn says %q, an action of %s, which it is not offered:\n%s",
						label, name, phrase, tool, text)
				}
			}
		}
	}
	unrestricted := strings.ToLower(strings.Join(slices.Collect(maps.Values(composeChiefTurnTexts(nil))), "\n"))
	for phrase := range actionPhrases {
		if !strings.Contains(unrestricted, phrase) {
			t.Errorf("positive control: no unrestricted turn text says %q, so the phrase scan could not see it", phrase)
		}
	}
	var seen []string
	for _, text := range composeChiefTurnTexts(nil) {
		seen = append(seen, namedTools(s, text)...)
	}
	for _, n := range []string{"reply_draft", "task_route_attention", "attention_ask"} {
		if !slices.Contains(seen, n) {
			t.Errorf("positive control: no unrestricted turn text names %s, so the scan could not see it", n)
		}
	}
}

// TestASessionQuestionIsNotPutToTheComposeChief: a mirrored session's
// chief_ask is refused, and never filed or put as a turn, when the chief is
// the Compose chief: it is not offered answer_session_question, so the
// question would only expire. Control: the Kubernetes chief's goes on to be
// filed (the stub store's Create is reached).
func TestASessionQuestionIsNotPutToTheComposeChief(t *testing.T) {
	w := newChiefWorld(t, true, true)
	w.prefs.choice = map[int64]bool{authz.OperatorUserID: true}
	w.s.ext.Attention = stubAttention{}
	reached := func() (got string, filed bool) {
		defer func() {
			if recover() != nil {
				filed = true
			}
		}()
		return w.askChief(), false
	}
	if _, filed := reached(); !filed {
		t.Fatal("control: the Kubernetes chief's session question was not filed")
	}
	w.s.ext.ComposeChief = &ComposeChief{}
	got, filed := reached()
	if filed {
		t.Fatal("the Compose chief's session question was filed")
	}
	if !strings.Contains(got, "cannot answer session questions on this install") || !strings.Contains(got, "attention_ask") {
		t.Errorf("chief_ask with the Compose chief: %s", got)
	}
}

// TestARoutesTextIsRenderedForItsChiefsTools is the seam between a route and
// its chief's tools: the hand-back and the relayed answer muster sends a
// route's chief (handBackText, routeAnswerPreface) name only tools that chief
// is offered, for a route of every kind. Control: the same routes' texts for
// the Kubernetes chief name reply_draft, task_route_attention and
// plugin_message_settle, so the scan sees them.
func TestARoutesTextIsRenderedForItsChiefsTools(t *testing.T) {
	s, chief, _ := chiefAndWorkerServer(t)
	ctx := context.Background()
	id := chief.ID
	routes := map[string]plugins.Route{
		"message": {RouteID: "r-msg", AgentID: &id, MessageID: 41},
		"one-way": {RouteID: "r-oneway", AgentID: &id, MessageID: 42, OneWay: true},
		"request": {RouteID: "r-req", AgentID: &id, RequestMessageID: 43},
		"task":    {RouteID: "r-task", AgentID: &id, TaskID: 44},
	}
	texts := func() string {
		var all []string
		for _, r := range routes {
			all = append(all, s.handBackText(ctx, r, "it timed out"), s.routeAnswerPreface(ctx, r))
		}
		return strings.Join(all, "\n")
	}
	k8s := namedTools(s, texts())
	for _, n := range []string{"reply_draft", "task_route_attention", "plugin_message_settle"} {
		if !slices.Contains(k8s, n) {
			t.Errorf("control: the Kubernetes chief's route texts do not name %s", n)
		}
	}
	s.ext.ComposeChief = &ComposeChief{}
	all := texts()
	for _, n := range namedTools(s, all) {
		if !slices.Contains(composeChiefToolLedger, n) {
			t.Errorf("a route's text names %s, which the Compose chief is not offered:\n%s", n, all)
		}
	}
	for phrase, tool := range actionPhrases {
		if !slices.Contains(composeChiefToolLedger, tool) && strings.Contains(strings.ToLower(all), phrase) {
			t.Errorf("a route's text says %q (%s), which the Compose chief is not offered:\n%s", phrase, tool, all)
		}
	}
	// The chief cannot be read: the text is rendered for the Compose chief's
	// pre-allowed set, never unrestricted.
	s.ext.Agents = failingAgents{}
	for _, n := range namedTools(s, texts()) {
		if !slices.Contains(composeChiefToolLedger, n) {
			t.Errorf("with the chief unreadable, a route's text names %s", n)
		}
	}
}

// TestThePluginDelivererIsGivenTheChiefsTurnTools is the seam between the
// renderer and the offered set: the deliverer UseExtensions builds renders
// every plugin turn for the chief's ChiefTurnTools — the Compose chief's
// offered set, and nil (unrestricted) for the Kubernetes chief.
func TestThePluginDelivererIsGivenTheChiefsTurnTools(t *testing.T) {
	s, chief, _ := chiefAndWorkerServer(t)
	ctx := context.Background()
	ext := s.ext
	ext.PluginGateway = struct{ plugins.Gateway }{}
	ext.Gateway = struct{ Gateway }{}
	for _, compose := range []bool{false, true} {
		ext.ComposeChief = nil
		if compose {
			ext.ComposeChief = &ComposeChief{}
		}
		s.UseExtensions(ext)
		if s.pluginDeliverer == nil {
			t.Fatal("no plugin deliverer was built")
		}
		got := s.pluginDeliverer.ToolsFor(ctx, chief)
		if !compose {
			if got != nil {
				t.Errorf("the Kubernetes chief's plugin turns are restricted to %v", got)
			}
			continue
		}
		var held []string
		for n := range got {
			held = append(held, n)
		}
		if slices.Sort(held); !slices.Equal(held, composeChiefToolLedger) {
			t.Errorf("the Compose chief's plugin turns are rendered for %v, want its offered set %v", held, composeChiefToolLedger)
		}
	}
}
