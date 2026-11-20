package api

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
)

// A STEP-UP CARD'S FACE (host_stepup_card.go), without a database: what its
// push title and body say, what its page draws, and that a card filed before
// 0171 (no parts) still draws its question.

// stepUpFixtureAsk is a filed plugin_draft_decide card as the store returns
// it: its parts, and the question Clean builds from them.
func stepUpFixtureAsk(t *testing.T, body string) attention.Ask {
	t.Helper()
	n, err := attention.Clean(attention.New{
		Asker: attention.Asker{System: true, Label: hostDecisionLabel},
		StepUp: &attention.StepUpCard{Tool: pluginDraftDecideTool, Title: "Post to X?", Why: "Marketing: rebrand announcement",
			Asker: "main:1", Preview: body, Venue: "X", DraftID: 41,
			Media:   []attention.StepUpMedium{{N: 0, Ref: "launch/hero-1", Type: "image/webp", SHA256: strings.Repeat("a", 64)}},
			Details: "A session on host-a (main:1) asks to approve or reject a reply or outbound draft as you: plugin_draft_decide {\"id\":41} [0123456789ab]"},
		Options: []string{hostDecisionConfirm, hostDecisionRefuse},
	})
	if err != nil {
		t.Fatal(err)
	}
	return attention.Ask{ID: 7, Asker: n.Asker, Question: n.Question, Options: n.Options, StepUp: n.StepUp, Status: attention.Open}
}

// TestAStepUpCardsQuestionIsItsWhyAndPreview: Clean builds the question from
// the parts — title, why, the post, its media line — and nothing else.
func TestAStepUpCardsQuestionIsItsWhyAndPreview(t *testing.T) {
	a := stepUpFixtureAsk(t, "Big news: acme is now acme-two.\nRead more on the blog.")
	const want = "Post to X?\nMarketing: rebrand announcement\n\nBig news: acme is now acme-two.\nRead more on the blog.\n\nWith 1 image"
	if a.Question != want {
		t.Fatalf("the card's question:\n%q\nwant\n%q", a.Question, want)
	}
	// Clean REPLACES whatever question a caller passed.
	n, err := attention.Clean(attention.New{Asker: attention.Asker{System: true, Label: hostDecisionLabel}, Question: "something else",
		StepUp: &attention.StepUpCard{Tool: "t", Title: "T?", Why: "W", Details: "D"}})
	if err != nil || n.Question != "T?\nW" {
		t.Fatalf("a passed question beside the parts: %q, %v; want the parts' text", n.Question, err)
	}
	// Only muster's own ask carries parts.
	if _, err := attention.Clean(attention.New{Asker: attention.Asker{AgentID: 3, Label: "agent"},
		StepUp: &attention.StepUpCard{Tool: "t", Title: "T?", Why: "W", Details: "D"}}); err == nil {
		t.Fatal("an agent's ask was allowed step-up parts")
	}
}

// TestAStepUpCardsPushIsItsTitleWhyAndPost: the push title is the card's
// title; its body is the why and the start of the post — never the host, the
// security paragraph, the hash or the call's JSON.
func TestAStepUpCardsPushIsItsTitleWhyAndPost(t *testing.T) {
	a := stepUpFixtureAsk(t, "Big news: acme is now acme-two.\nRead more on the blog.")
	n := attentionNotification(askItem(a), false)
	if n.Title != "Post to X?" {
		t.Fatalf("the push title %q, want %q", n.Title, "Post to X?")
	}
	const body = "Marketing: rebrand announcement · Big news: acme is now acme-two. Read more on the blog."
	if n.Body != body {
		t.Fatalf("the push body:\n%q\nwant\n%q", n.Body, body)
	}
	for _, never := range []string{"host-a", "paired phone", "plugin_draft_decide", "{", "0123456789ab", "Options:"} {
		if strings.Contains(n.Body, never) || strings.Contains(n.Title, never) {
			t.Errorf("the push carries %q: %q / %q", never, n.Title, n.Body)
		}
	}
	if n.Ask == nil || strings.Join(n.Ask.Options, "|") != "Confirm|Refuse" {
		t.Fatalf("the push's buttons: %+v", n.Ask)
	}
}

// TestAStepUpCardsPageShowsThePostAndFoldsTheDetails: the page draws the
// title, the why, who asked, the venue, the post and its medium; the
// boilerplate is inside a <details> that is closed; the face carries no JSON.
func TestAStepUpCardsPageShowsThePostAndFoldsTheDetails(t *testing.T) {
	a := stepUpFixtureAsk(t, "Big news: acme is now acme-two.")
	page := askQuestionHTML(a)
	for _, want := range []string{
		`<h2 class="break-words text-xl font-semibold leading-snug text-fg" data-stepup-title>Post to X?</h2>`,
		`data-stepup-why>Marketing: rebrand announcement</p>`,
		`data-stepup-asker>Asked by main:1</p>`,
		`data-stepup-venue>X</span>`,
		`data-stepup-preview-text>Big news: acme is now acme-two.</p>`,
		`src="/plugins/drafts/41/media/0" data-stepup-image>`,
		`<details class="rounded-lg bg-s1 px-3 py-2 ring-1 ring-inset ring-line" data-stepup-details>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %s:\n%s", want, page)
		}
	}
	open := strings.Index(page, "<details")
	end := open + strings.Index(page[open:], "</details>")
	if open < 0 || end < open {
		t.Fatalf("no Details:\n%s", page)
	}
	details, face := page[open:end], page[:open]+page[end:]
	if strings.Contains(page[open:strings.Index(page[open:], ">")+open], " open") {
		t.Fatalf("Details is open by default:\n%s", details)
	}
	for _, in := range []string{"host-a", "main:1", "[0123456789ab]", "plugin_draft_decide {&#34;id&#34;:41}"} {
		if !strings.Contains(details, in) {
			t.Errorf("Details lacks %q:\n%s", in, details)
		}
	}
	for _, never := range []string{"host-a", "{", "0123456789ab", "paired phone"} {
		if strings.Contains(face, never) {
			t.Errorf("the card's face carries %q:\n%s", never, face)
		}
	}
	if strings.Contains(page, "data-ask-question") {
		t.Errorf("a card with parts also draws the raw question:\n%s", page)
	}
}

// TestAStepUpCardEscapesTheOfficersText: a post holding markup is drawn as
// text, never HTML — in the preview and in every other part.
func TestAStepUpCardEscapesTheOfficersText(t *testing.T) {
	a := stepUpFixtureAsk(t, `<script>alert(1)</script> & <img src=x onerror=alert(2)>`)
	a.StepUp.Why = `Marketing: <b>launch</b>`
	page := askQuestionHTML(a)
	if strings.Contains(page, "<script>") || strings.Contains(page, "<img src=x") || strings.Contains(page, "<b>launch") {
		t.Fatalf("officer text was drawn as HTML:\n%s", page)
	}
	if !strings.Contains(page, `&lt;script&gt;alert(1)&lt;/script&gt; &amp; &lt;img src=x onerror=alert(2)&gt;`) ||
		!strings.Contains(page, `Marketing: &lt;b&gt;launch&lt;/b&gt;`) {
		t.Fatalf("the escaped text is not shown:\n%s", page)
	}
}

// TestAChangedDraftsCardShowsNoPreview: a card whose draft no longer matched
// shows the refuse state, and nothing of the draft — even if its parts named
// one (CleanStepUp drops it).
func TestAChangedDraftsCardShowsNoPreview(t *testing.T) {
	n, err := attention.Clean(attention.New{Asker: attention.Asker{System: true, Label: hostDecisionLabel},
		StepUp: &attention.StepUpCard{Tool: pluginDraftDecideTool, Title: stepUpChangedTitle, Why: stepUpChangedWhy, Changed: true,
			Preview: "STALE TEXT", Venue: "X", DraftID: 41, Media: []attention.StepUpMedium{{Ref: "r", Type: "image/png", SHA256: "s"}}, Details: "D"}})
	if err != nil {
		t.Fatal(err)
	}
	const want = "This draft changed since it was asked\nRefuse: the draft no longer matches what the session asked you to decide."
	if n.Question != want {
		t.Fatalf("a changed card's question %q, want %q", n.Question, want)
	}
	page := askQuestionHTML(attention.Ask{Question: n.Question, StepUp: n.StepUp})
	if !strings.Contains(page, `role="alert"`) || !strings.Contains(page, "data-stepup-changed") ||
		strings.Contains(page, "STALE TEXT") || strings.Contains(page, "data-stepup-preview") || strings.Contains(page, "/media/") {
		t.Fatalf("a changed card's page:\n%s", page)
	}
}

// TestAnOldStepUpCardStillRenders (invariant guard: a card filed before 0171
// has no parts, and must keep drawing its question as filed). It cannot be red
// at the base, which drew every question this way.
func TestAnOldStepUpCardStillRenders(t *testing.T) {
	old := attention.Ask{ID: 3, Asker: attention.Asker{System: true, Label: hostDecisionLabel}, Options: []string{hostDecisionConfirm, hostDecisionRefuse},
		Question: "A session on host-a (main:1) asks to destroy one of your agents: agent_destroy {\"id\":5}\n\nThis machine runs host officer acme:main [0011223344ff]"}
	page := askQuestionHTML(old)
	if !strings.Contains(page, `data-ask-question>A session on host-a (main:1) asks to destroy one of your agents: agent_destroy {&#34;id&#34;:5}`) ||
		strings.Contains(page, "data-stepup-card") {
		t.Fatalf("an old card's page:\n%s", page)
	}
	if n := attentionNotification(askItem(old), false); n.Title != "step-up is asking" {
		t.Fatalf("an old card's push title %q", n.Title)
	}
}

// TestEveryOtherGatedToolGetsAHumanSummary: no tool's card face is JSON.
func TestEveryOtherGatedToolGetsAHumanSummary(t *testing.T) {
	cases := []struct{ tool, args, title, preview string }{
		{routeReplyToolName, `{"route_id":"r1","body":"See you Friday."}`, "Send this answer to a routed message?", "See you Friday."},
		{attentionAnswerTool, `{"id":12,"answer":"yes"}`, "Answer an ask as you?", "Answer ask #12 with: yes"},
		{connectorRequestDecideTool, `{"id":4,"decision":"approve"}`, "Approve a connector request?", "Approve connector request #4."},
		{connectorRequestDecideTool, `{"id":4,"decision":"deny"}`, "Deny a connector request?", "Deny connector request #4."},
		{agentDestroyTool, `{"name":"helper"}`, "Destroy an agent?", "Destroy agent helper."},
		{pluginMessageRouteTool, `{"message_id":9,"subject":"billing"}`, "Route a plugin message?", "Route plugin message #9 as \u201cbilling\u201d."},
		{pluginRequestDecideTool, `{"id":2,"decision":"approve","code":"x"}`, "Approve a plugin's pairing?", "Approve pairing request #2 (it mints the plugin a token)."},
		{pluginOutboundBatchDecideTool, `{"batch_id":"b","batch_sha256":"s","approve_ids":[1,2]}`, "Approve part of an outreach batch?", "Send 2 of the batch's follows and likes; reject the rest."},
		{dispatchApproveTool, `{"hold_id":"held:1"}`, "Start this agent?", "The held start: Start a new agent on pod/gateway"},
	}
	for _, c := range cases {
		title, preview := stepUpToolSummary(c.tool, c.args, hostStepUpTools[c.tool], "Start a new agent on pod/gateway")
		if title != c.title || preview != c.preview {
			t.Errorf("%s: %q / %q, want %q / %q", c.tool, title, preview, c.title, c.preview)
		}
		if strings.Contains(title+preview, "{") {
			t.Errorf("%s: JSON on the card's face: %q %q", c.tool, title, preview)
		}
	}
}
