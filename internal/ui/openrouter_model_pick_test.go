package ui

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestTheOpenRouterRuntimeIsPickedBySearch: where the opencode runtime is
// offered, Continue in a new session draws a form posting runtime=opencode with
// a model search field (it asks OpenRouterModelsOptionsPath as you type) —
// beside the Claude Code form and its <select>; where it is not, no such form.
// (New session's OpenRouter search is the "+" menu's:
// TestThePlusMenuOffersOpenRouterWhereOffered.)
func TestTheOpenRouterRuntimeIsPickedBySearch(t *testing.T) {
	both := []SessionRuntimeView{{Runtime: "claude-code", Label: "Claude Code"}, {Runtime: "opencode", Label: "OpenRouter"}}
	bar := &ChiefChatBar{AgentID: 7, SessionID: 9, Runtimes: both, NewChatModels: []string{"opus", "sonnet"}}
	html := renderString(t, chiefContinuePicker(bar, ChatSurfaceChiefPanel))
	oc := regexp.MustCompile(`(?s)<form [^>]*` + chiefContinueFormAttr + `="opencode".*?</form>`).FindString(html)
	if oc == "" {
		t.Fatalf("no opencode form:\n%s", html)
	}
	for _, want := range []string{`name="runtime" value="opencode"`, `name="model"`, openRouterSearchAttr,
		`hx-get="` + OpenRouterModelsOptionsPath + `"`, `hx-post="` + ChiefContinuePath(7, 9) + `?`} {
		if !strings.Contains(oc, want) {
			t.Errorf("the opencode form lacks %s", want)
		}
	}
	if strings.Contains(oc, "<select") {
		t.Error("the opencode form offers a fixed <select> of models")
	}
	if !strings.Contains(html, `value="claude-code"`) || !strings.Contains(html, `<option value="sonnet">`) {
		t.Error("the Claude Code form is gone")
	}
	bar.Runtimes = both[:1]
	only := renderString(t, chiefContinuePicker(bar, ChatSurfaceChiefPanel))
	if strings.Contains(only, `value="opencode"`) || strings.Contains(only, openRouterSearchAttr) {
		t.Error("an OpenRouter form is drawn where the runtime is not offered")
	}
}

// TestThePlusMenuOffersOpenRouterWhereOffered: the Sessions tab's "+" menu carries
// an OpenRouter form (runtime=opencode, the search field, Start) posting to
// the side chat route only when the bar says OpenRouter is offered.
func TestThePlusMenuOffersOpenRouterWhereOffered(t *testing.T) {
	b := &ChiefChatBar{AgentID: 7, AgentName: "chief", SessionID: 3, NewChatModels: []string{"opus"}, OpenRouter: true}
	html := renderString(t, chiefNewChatChip(b, ChatSurfacePage))
	form := regexp.MustCompile(`(?s)<form [^>]*` + chiefNewChatOpenRouterAttr + `.*?</form>`).FindString(html)
	if form == "" {
		t.Fatalf("no OpenRouter form in the \"+\" menu:\n%s", html)
	}
	for _, want := range []string{`value="opencode"`, openRouterSearchAttr, MenuKeepOpenAttr, ">Start<", ChiefSideChatPath(7)} {
		if !strings.Contains(form, want) {
			t.Errorf("the \"+\" OpenRouter form lacks %s", want)
		}
	}
	b.OpenRouter = false
	if html := renderString(t, chiefNewChatChip(b, ChatSurfacePage)); strings.Contains(html, chiefNewChatOpenRouterAttr) {
		t.Error("the \"+\" menu offers OpenRouter where it is not offered")
	}
	// OpenRouter alone still opens the menu (no Claude models offered).
	only := &ChiefChatBar{AgentID: 7, AgentName: "chief", SessionID: 3, OpenRouter: true}
	if html := renderString(t, chiefNewChatChip(only, ChatSurfacePage)); !strings.Contains(html, chiefNewChatOpenRouterAttr) {
		t.Error("with only OpenRouter offered the \"+\" menu is not drawn")
	}
}

// TestTheSearchDrawsTapTargets: each match is a button carrying the id it
// puts in the field, with its name, id and price; no match says so; an empty
// query draws nothing.
func TestTheSearchDrawsTapTargets(t *testing.T) {
	html := renderString(t, OpenRouterModelOptions([]OpenRouterModelHit{{ID: "openrouter/deepseek/deepseek-v4.1-flash",
		Name: "DeepSeek: V4.1 Flash", ContextLength: 128000, PromptUSD: "0.00000015", CompletionUSD: "0.0000006"}}, "deep"))
	for _, want := range []string{OpenRouterPickAttr + `="openrouter/deepseek/deepseek-v4.1-flash"`, "DeepSeek: V4.1 Flash",
		"deepseek/deepseek-v4.1-flash · $0.15 in, $0.60 out per M · 128k"} {
		if !strings.Contains(html, want) {
			t.Errorf("the match lacks %q:\n%s", want, html)
		}
	}
	if html := renderString(t, OpenRouterModelOptions(nil, "zzz")); !strings.Contains(html, "No OpenRouter model matches") {
		t.Errorf("no match: %s", html)
	}
	if html := renderString(t, OpenRouterModelOptions(nil, " ")); html != "" {
		t.Errorf("an empty query drew %q", html)
	}
}

// TestAnOpenRouterChipNamesItsModel: a row's model tag is the model's own
// name (the catalogue id's last part), its title the catalogue id; a Claude
// model's tag is unchanged.
func TestAnOpenRouterChipNamesItsModel(t *testing.T) {
	sess := SessionView{ID: 3, Runtime: "opencode", RuntimeLabel: "OpenRouter", Model: "openrouter/deepseek/deepseek-v4.1-flash"}
	tag := renderString(t, chiefChipModelTag(sess))
	if !strings.Contains(tag, ">deepseek-v4.1-flash<") || !strings.Contains(tag, `title="Model: deepseek/deepseek-v4.1-flash"`) {
		t.Errorf("the chip tag: %s", tag)
	}
	if tag := renderString(t, chiefChipModelTag(SessionView{Model: "sonnet"})); !strings.Contains(tag, ">sonnet<") {
		t.Errorf("a Claude tag: %s", tag)
	}
}

// TestTheBudgetCardSaysWhatAndWhenAndOffersContinue: spent, cap, the 00:00
// UTC reset with the time left, and — on a side chat — Continue in a new
// session onto Claude Code only (never OpenRouter again: the cap is the
// owner's day).
func TestTheBudgetCardSaysWhatAndWhenAndOffersContinue(t *testing.T) {
	old := spendCapNow
	spendCapNow = func() time.Time { return time.Date(2026, 11, 23, 20, 48, 0, 0, time.UTC) }
	t.Cleanup(func() { spendCapNow = old })
	content := `{"day_spent_usd":20.0123,"day_reserved_usd":0,"daily_cap_usd":20,"session_spent_usd":3.5,"session_cap_usd":0,` +
		`"resets_at":"2026-11-24T00:00:00Z","refusal":"x"}`
	b := &ChiefChatBar{AgentID: 7, AgentName: "chief", SessionID: 9}
	html := renderString(t, spendCapCard(content, b, []string{"opus", "sonnet"}))
	for _, want := range []string{"OpenRouter budget reached", "spent $20.01 of the $20.00 daily cap", "resets at 00:00 UTC (Tue 24 Nov), in 3h 12m",
		chiefContinueAttr, chiefContinueFormAttr + `="claude-code"`, ChiefContinuePath(7, 9)} {
		if !strings.Contains(html, want) {
			t.Errorf("the card lacks %q:\n%s", want, html)
		}
	}
	if strings.Contains(html, `="opencode"`) {
		t.Error("the card offers to continue on OpenRouter")
	}
	if html := renderString(t, spendCapCard(content, nil, nil)); strings.Contains(html, chiefContinueAttr) || !strings.Contains(html, "daily cap") {
		t.Errorf("off a chief chat: %s", html)
	}
}

// TestTheEmptyFieldAsksForTheRecentModels: the model field asks the options
// route when it is focused and when the "+" menu it sits in opens (a toggle of
// its closest popover), not only as it is typed in — so an empty field shows
// the recent models before anything is typed — on every picker that draws it
// (Continue on either surface, the "+" menu). The recent list is the search's own
// buttons under "Recent", and nothing when there are none. RED at main: the
// field asked only on input.
func TestTheEmptyFieldAsksForTheRecentModels(t *testing.T) {
	both := []SessionRuntimeView{{Runtime: "claude-code", Label: "Claude Code"}, {Runtime: "opencode", Label: "OpenRouter"}}
	for name, html := range map[string]string{
		"continue page":  renderString(t, chiefContinuePicker(&ChiefChatBar{AgentID: 7, SessionID: 9, Runtimes: both, NewChatModels: []string{"opus"}}, ChatSurfacePage)),
		"continue panel": renderString(t, chiefContinuePicker(&ChiefChatBar{AgentID: 7, SessionID: 9, Runtimes: both, NewChatModels: []string{"opus"}}, ChatSurfaceChiefPanel)),
		"plus menu":      renderString(t, chiefNewChatChip(&ChiefChatBar{AgentID: 7, SessionID: 9, OpenRouter: true}, ChatSurfacePage)),
	} {
		field := regexp.MustCompile(`<input [^>]*` + openRouterSearchAttr + `[^>]*>`).FindString(html)
		if field == "" {
			t.Fatalf("%s: no model field", name)
		}
		if !strings.Contains(field, `hx-trigger="focus, input changed delay:250ms, toggle from:closest [popover]"`) {
			t.Errorf("%s: the field does not ask when focused or when its menu opens: %s", name, field)
		}
	}
	hits := []OpenRouterModelHit{{ID: "openrouter/vendor-n/model-n6", Name: "Vendor N: Six", ContextLength: 48000, PromptUSD: "0.0000004", CompletionUSD: "0.0000009"}}
	recent := renderString(t, OpenRouterRecentModels(hits))
	if !strings.Contains(recent, `role="group" aria-label="Recent models"`) || !strings.Contains(recent, `>Recent</span>`) {
		t.Errorf("the recent list has no heading of its own:\n%s", recent)
	}
	if !strings.Contains(recent, renderString(t, OpenRouterModelOptions(hits, "six"))) {
		t.Errorf("a recent model is not drawn as a search hit is:\n%s", recent)
	}
	if got := renderString(t, OpenRouterRecentModels(nil)); got != "" {
		t.Errorf("no recent model drew %q", got)
	}
}
