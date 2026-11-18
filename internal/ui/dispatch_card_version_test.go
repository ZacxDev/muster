package ui

import (
	"strings"
	"testing"
)

// TestADispatchCardCarriesItsVersionAndThePageItsGuard: a card with a version
// draws it (data-dispatch-version), one without draws none, and the guard that
// discards an older card response is a script that compares exactly that
// attribute on the card wrapper's swaps, installed once per page.
func TestADispatchCardCarriesItsVersionAndThePageItsGuard(t *testing.T) {
	with := renderString(t, DispatchAgentCard(DispatchCard{AgentID: 7, Name: "brisk-otter", State: DispatchWaiting, Version: 1793999254123456}))
	if !strings.Contains(with, `data-dispatch-version="1793999254123456"`) {
		t.Fatalf("a versioned card draws no version: %s", with)
	}
	without := renderString(t, DispatchAgentCard(DispatchCard{AgentID: 7, Name: "brisk-otter", State: DispatchWaiting}))
	if strings.Contains(without, DispatchVersionAttr) {
		t.Fatalf("an unversioned card draws a version: %s", without)
	}
	js := renderString(t, DispatchCardGuard())
	for _, must := range []string{"window.musterDispatchCardGuard = function", DispatchVersionAttr, DispatchLiveAttr, "shouldSwap = false"} {
		if !strings.Contains(js, must) {
			t.Errorf("the guard script lacks %q", must)
		}
	}
}

// TestEveryLiveCardRunsTheGuardOnItself: the wrapper that refreshes a card calls
// the guard from its OWN before-swap (a detached wrapper's swap must still be
// recorded), and a card's version does not change the #chat-log version (a
// re-render that changes nothing but card versions answers 204, not a swap).
func TestEveryLiveCardRunsTheGuardOnItself(t *testing.T) {
	html := renderString(t, DispatchAgentCard(DispatchCard{AgentID: 7, Name: "brisk-otter", State: DispatchWaiting, Version: 5}))
	if !strings.Contains(html, `hx-on::before-swap="window.musterDispatchCardGuard&amp;&amp;window.musterDispatchCardGuard(event)"`) {
		t.Fatalf("the live card's wrapper does not run the guard on itself: %s", html)
	}
	if got, want := chatLogVersionOf([]byte(`<div data-dispatch-card data-dispatch-version="111">x</div>`)),
		chatLogVersionOf([]byte(`<div data-dispatch-card data-dispatch-version="222">x</div>`)); got != want {
		t.Fatalf("two renders differing only in a card version have log versions %s and %s; want equal", got, want)
	}
	if chatLogVersionOf([]byte(`<div data-dispatch-state="a">`)) == chatLogVersionOf([]byte(`<div data-dispatch-state="b">`)) {
		t.Fatal("control: a real change did not change the log version")
	}
}

// TestTheChatLogSettlesInTheSwap: #chat-log swaps with settle:0ms, so a card it
// draws is listening in the same task it is inserted (the measured gap is in the
// attribute's comment). htmx 2 settles synchronously only when the delay is 0.
func TestTheChatLogSettlesInTheSwap(t *testing.T) {
	html := renderString(t, agentChatPane(AgentDetailView{Name: "brisk-otter", Status: "running", ActiveSessionID: 3}))
	if !strings.Contains(html, `id="chat-log"`) {
		t.Fatalf("control: no #chat-log drawn: %.400s", html)
	}
	if !strings.Contains(html, `hx-swap="innerHTML settle:0ms"`) {
		t.Fatalf("#chat-log does not settle in its swap: %.2000s", html)
	}
}
