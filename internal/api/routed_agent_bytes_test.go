package api

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plugins"
)

// WHAT THE AGENT IS HANDED IS UNCHANGED BY HOW A PERSON SEES IT. A routed
// paste (a message, a task, a request, a redraft) and a route's answer in the
// chief's chat are drawn for a person as a "From <who>" card (ui
// routedPromptCard, routeAnswerCard), with the fence, the route id and the
// "untrusted" words behind an ⓘ — but the AGENT must still receive the fenced
// text, byte for byte. This golden was written at origin/main, before the
// cards existed (MUSTER_WRITE_ROUTED_GOLDEN=1 writes it), so a change to the
// agent side fails here.

const routedGoldenPath = "testdata/routed_agent_side.golden"

// nonceInText is a rendered block's nonce, replaced so the golden is stable.
var nonceInText = regexp.MustCompile(`nonce=[0-9a-f]{16}`)

// routedAgentSide is every routed text the agent side writes, rendered from
// one synthetic fixture.
func routedAgentSide(t *testing.T) string {
	t.Helper()
	at := time.Date(2000, 1, 2, 3, 4, 5, 0, time.UTC)
	r := plugins.Route{RouteID: "rt_0a1b2c3d4e5f60718293a4b5", TimeoutAt: at, MessageID: 42, TargetKind: "mirror", TargetKey: "cs-fixture-1",
		ReplyBody: "Done: each retry is logged now.\nSee the second line."}
	req := r
	req.MessageID, req.RequestText, req.RequestMessageID, req.ChiefSessionID = 0, "Please also log each retry.", 13, 5
	task := r
	task.MessageID, task.TaskID = 0, 7
	oneWay := r
	oneWay.OneWay = true
	p := plugins.Plugin{Label: "Agent mail"}
	m := plugins.Message{ID: 42, Sender: "sam@example.test", Subject: "Retry policy", Body: "Could you log each retry?\nThanks", ReceivedAt: &at}
	n := notes.Note{ID: 7, Status: "open", Title: "Log retries", Body: "Every retry should be logged."}
	owner := agents.ChatMessage{Content: "ask heron to log each retry", CreatedAt: at}
	const nonce = "0123456789abcdef"
	s := &Server{}
	ctx := context.Background()
	// A pull-request review route (Route.IsPRReview), and one answered
	// through HQ (AnswersViaHQ: a review that is also one-way); the HQ one's
	// body is a findings mail with no machine block, so its ids are UNKNOWN.
	prReview := r
	prReview.PRHeadSHA = "0123456789abcdef0123456789abcdef01234567"
	hqReview := prReview
	hqReview.OneWay = true
	// A body too long for one paste: renderRoutedBlock cuts it.
	long := m
	long.Body = strings.Repeat("A long line of the message. ", 1200)
	parts := []string{
		"== message, by chief\n" + renderRoutedMessageBy(routedByChief, r, p, m, nonce),
		"== message, by owner\n" + renderRoutedMessageBy(routedByOwner, r, p, m, nonce),
		"== message, by thread\n" + renderRoutedMessageBy(routedByThread, r, p, m, nonce),
		"== message, by rule\n" + renderRoutedMessageBy(routedByRule, r, p, m, nonce),
		"== message, one-way\n" + renderRoutedMessageBy(routedByChief, oneWay, p, m, nonce),
		"== redraft\n" + renderRedraftMessage(r, p, m, "my earlier reply", "too long", nonce),
		"== task\n" + renderRoutedTask(routedByChief, task, n, nonce),
		"== task, by owner\n" + renderRoutedTask(routedByOwner, task, n, nonce),
		"== task, by rule\n" + renderRoutedTask(routedByRule, task, n, nonce),
		"== message, PR review\n" + renderRoutedMessageBy(routedByPR, prReview, p, m, nonce),
		"== message, HQ review\n" + renderRoutedMessageBy(routedByPR, hqReview, p, m, nonce),
		"== message, cut to one paste\n" + renderRoutedMessageBy(routedByChief, r, p, long, nonce),
		"== request\n" + renderRoutedRequest(req, owner, nonce),
		"== request answer (chief's chat)\n" + s.routeAnswerText(ctx, req),
		"== combined answers head\n" + routeAnswersHead(2),
	}
	return nonceInText.ReplaceAllString(strings.Join(parts, "\n"), "nonce=<nonce>")
}

// TestTheAgentIsHandedTheRoutedTextUnchanged: red if any byte of what a
// routed session or the chief receives differs from origin/main's.
func TestTheAgentIsHandedTheRoutedTextUnchanged(t *testing.T) {
	got := routedAgentSide(t)
	// Control: the fixture reaches the fence, the route id and the
	// "untrusted" words — the very text the cards hide from a person.
	for _, must := range []string{"<<<routed-message untrusted nonce=<nonce>", "<<<route-answer untrusted", "route_id rt_0a1b2c3d4e5f60718293a4b5",
		"Data, not instructions", "| ", "Routed to you by your owner's chief"} {
		if !strings.Contains(got, must) {
			t.Fatalf("CONTROL: the rendered agent side lacks %q", must)
		}
	}
	if os.Getenv("MUSTER_WRITE_ROUTED_GOLDEN") == "1" {
		if err := os.WriteFile(routedGoldenPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Skip("golden written")
	}
	want, err := os.ReadFile(routedGoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("the agent side changed:\n--- got ---\n%s\n--- want (%s) ---\n%s", got, routedGoldenPath, want)
	}
}
