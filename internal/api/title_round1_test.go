package api

import (
	"bytes"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/push"
	"github.com/ZacxDev/muster/internal/textplain"
	"github.com/ZacxDev/muster/internal/ui"
)

// TestADispatchedAgentNamedAfterALongMarkdownTitleRendersPlain is the
// operator's case end to end: an agent dispatched from a task is named after
// the task's first line, and that line is a long markdown link. The name is
// stored UNCUT (to a generous storage bound) so the title rule sees the whole
// link; the card clamps AFTER plaining. Red before: the name was cut to 40 runes
// at dispatch, mid-link, and "[login page](https://example.com…" can never be
// plained — the card, the <h1>, the <title> and the recents strip all showed
// the broken markup.
func TestADispatchedAgentNamedAfterALongMarkdownTitleRendersPlain(t *testing.T) {
	body := "Fix the [login page](https://example.com/a/really/long/path/to/the/login) redirect\n\nDetails below."
	name := dispatchDisplayName(41, body)
	const plain = "#41 · Fix the login page redirect"
	if got := textplain.Title(name); got != plain {
		t.Fatalf("stored name %q reads %q, want %q", name, got, plain)
	}
	// The storage bound: a runaway first line is cut, generously.
	if n := len([]rune(dispatchDisplayName(42, strings.Repeat("word ", 200)))); n > 300+1+len([]rune("#42 · ")) || n < 250 { // 300 + the cut's ellipsis
		t.Errorf("a runaway first line is stored as %d runes; want it cut near 300", n)
	}

	a := ui.AgentCardView{ID: 41, Name: "agent-41", Status: "running", DisplayName: name}
	var b bytes.Buffer
	if err := ui.RenderAgentsList(&b, []ui.AgentCardView{a}, nil, []ui.RecentRef{{AgentID: 41}}, 0); err != nil {
		t.Fatal(err)
	}
	list := b.String()
	if n := strings.Count(list, ">"+plain+"<"); n < 2 {
		t.Errorf("the plain name is drawn %d time(s) on the agents list; want the card and the recents strip:\n%s", n, list)
	}
	// (The filter's data attributes keep the raw name on purpose; what is
	// DRAWN must not carry the markup.)
	if strings.Contains(list, ">#41 · Fix the [login") {
		t.Errorf("the agents list draws the link markup")
	}
	b.Reset()
	if err := ui.AgentDetailPage(ui.AgentDetailView{Name: "agent-41", Status: "running", DisplayName: name}, ui.Features{}).Render(&b); err != nil {
		t.Fatal(err)
	}
	page := b.String()
	for _, want := range []string{"<title>" + plain + " · muster</title>", ">" + plain + "</h1>"} {
		if !strings.Contains(page, want) {
			t.Errorf("the detail page lacks %q", want)
		}
	}
}

// TestAWaitingSessionPushTitleIsPlain: the attention push names the waiting
// window in its BODY and the asker in its TITLE, each by a title that is often
// LLM-written markdown; each reads as the plain title, as the attention page
// shows it — the body through attentionPushBody's own application of the rule,
// the title through the one title sink (goNotify).
func TestAWaitingSessionPushTitleIsPlain(t *testing.T) {
	store, pusher := &memPush{}, &payloadPusher{}
	claim(t, store, 1, ep("plain-title"))
	s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Users: stubUsers{}, Push: push.NewService(store, pusher, push.VAPIDConfig{PublicKey: "k"}, log.New(io.Discard, "", 0))})
	s.attentionNotify(1, attentionItem{Key: "session-0011223344556677", Kind: attentionKindSession, Source: "**Fix** [auth](https://example.com/a) for C# @ host-b"})
	s.attentionNotify(1, attentionItem{Key: "ask-9", Kind: attentionKindAsk, AskID: 9, Source: "`agent_7` **chief**", Text: "q"})
	s.WaitForPushes()
	got := map[string]string{}
	for _, p := range pusher.to(ep("plain-title")) {
		got[p.ID] = p.Title + " | " + p.Body
	}
	if got["session-0011223344556677"] != "A session is waiting on you | Waiting: Fix auth for C# @ host-b" || got["ask-9"] != "agent_7 chief is asking | q" {
		t.Errorf("pushes = %q", got)
	}
}
