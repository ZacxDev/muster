package ui

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// ---------------------------------------------------------------------------
// THE "KICKOFF FAILED" BADGE (PR #37 review round 0): a first turn that was handed
// to a gateway and did not complete. The stamp that makes delivery at-most-once
// also turns "kickoff owed" off, so this is the only thing on the card saying the
// agent was never told its task.
// ---------------------------------------------------------------------------

// failedFixtureError is the error text the card is handed.
//
// 🔴 PAIRWISE DISTINCT FROM EVERY CONSTANT THE ASSERTIONS NAME — the badge label,
// the remedy, the "Kickoff failed: " prefix — so finding it proves the VIEW's
// field was rendered, not that a fixed string happened to contain it.
const failedFixtureError = "responses HTTP 502: upstream reset by peer zr51"

func failedCardFor(t *testing.T, status string, failed bool, msg string) string {
	t.Helper()
	return failedCardWith(t, status, failed, false, msg)
}

func failedCardWith(t *testing.T, status string, failed, resendSafe bool, msg string) string {
	t.Helper()
	return renderString(t, agentCard(AgentCardView{
		ID: 9, Name: "agent-9", DisplayName: "agent-9", Namespace: "ns-q7-agent-9", Status: status,
		KickoffFailed: failed, KickoffFailure: msg, KickoffResendSafe: resendSafe,
	}))
}

// The two remedies, spelled out literally rather than read back from
// KickoffFailedRemedy, so a change to the text the operator acts on fails here.
const (
	wantRemedyCheckFirst = "Not retried automatically, so it is never paid twice. The agent may " +
		"still be working on this turn. Before re-sending, check its logs (kubectl -n ns-q7-agent-9 " +
		"logs deploy/agent-9 -c agent) or its chat transcript, and re-send through its chat only " +
		"if it is not working on the task."
	wantRemedyResendSafe = "Not retried automatically. Nothing reached the agent, so re-sending " +
		"cannot pay for the task twice: open this agent's chat and send the task again."
)

// TestTheRemedySaysCheckFirstUnlessNothingWasSent (PR #37 round 1, F3): the remedy
// used to tell the operator to re-send for EVERY cause. For a turn the runtime may
// still be running (shutdown, timeout, a transport error after the request was
// written, an empty reply) that pays for the task twice. Only a failure that proves
// nothing was sent may say re-sending is safe.
func TestTheRemedySaysCheckFirstUnlessNothingWasSent(t *testing.T) {
	for _, c := range []struct {
		safe bool
		want string
	}{
		{false, wantRemedyCheckFirst},
		{true, wantRemedyResendSafe},
	} {
		card := failedCardWith(t, "running", true, c.safe, failedFixtureError)
		remedies := nodesWithAttr(t, card, "data-kickoff-remedy")
		if len(remedies) != 1 {
			t.Fatalf("resendSafe=%t: %d remedy element(s), want 1.\ncard:\n%s", c.safe, len(remedies), card)
		}
		if got := strings.TrimSpace(textOf(renderToString(t, remedies[0]))); got != c.want {
			t.Errorf("resendSafe=%t: the remedy reads\n    %q\nwant\n    %q", c.safe, got, c.want)
		}
	}
}

// nodesWithAttr returns EVERY element carrying attr — every one, not the first,
// for the reason badgeNodes records (an additive mutant hides behind a sibling).
func nodesWithAttr(t *testing.T, markup, attr string) []*html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(markup))
	if err != nil {
		t.Fatalf("parse card: %v", err)
	}
	var found []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			for _, a := range n.Attr {
				if a.Key == attr {
					found = append(found, n)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return found
}

// TestAFailedKickoffIsBadgedWithItsErrorAndTheRemedy is the render half of the
// visibility fix, on a `running` card — the status a failed-but-stamped agent's
// live instance reads, and the one that looked healthy.
func TestAFailedKickoffIsBadgedWithItsErrorAndTheRemedy(t *testing.T) {
	card := failedCardFor(t, "running", true, failedFixtureError)

	badges := nodesWithAttr(t, card, "data-kickoff-failed")
	if len(badges) != 1 {
		t.Fatalf("a card with KickoffFailed=true renders %d \"kickoff failed\" badge(s), want 1.\n"+
			"    Without it a turn that failed AFTER the stamp leaves a card that reads healthy: "+
			"the stamp cleared \"kickoff owed\".\ncard:\n%s", len(badges), card)
	}
	if got := strings.TrimSpace(textOf(renderToString(t, badges[0]))); got != "kickoff failed" {
		t.Errorf("the badge reads %q, want %q", got, "kickoff failed")
	}
	var aria string
	for _, a := range badges[0].Attr {
		if a.Key == "aria-label" {
			aria = a.Val
		}
	}
	if !strings.HasPrefix(aria, "Kickoff failed:") {
		t.Errorf("the failed badge's aria-label is %q; its only other affordance is colour", aria)
	}

	details := nodesWithAttr(t, card, "data-kickoff-failure")
	if len(details) != 1 {
		t.Fatalf("the card renders %d failure detail(s), want 1.\ncard:\n%s", len(details), card)
	}
	detail := textOf(renderToString(t, details[0]))
	if !strings.Contains(detail, failedFixtureError) {
		t.Errorf("the failure detail does not show the recorded error %q — the operator sees "+
			"THAT it failed and not WHY.\ndetail: %q", failedFixtureError, detail)
	}
	if remedies := nodesWithAttr(t, card, "data-kickoff-remedy"); len(remedies) != 1 ||
		strings.TrimSpace(textOf(renderToString(t, remedies[0]))) != wantRemedyCheckFirst {
		t.Errorf("the failure detail does not carry the remedy %q — nothing re-sends "+
			"automatically, so the card must say what to do by hand.\ndetail: %q",
			wantRemedyCheckFirst, detail)
	}
	if !strings.Contains(card, `aria-label="Status: running"`) {
		t.Errorf("the status indicator is gone from a card carrying the failed badge; the "+
			"badge is ADDITIVE.\ncard:\n%s", card)
	}
	if len(badgeNodes(t, card)) != 0 {
		t.Errorf("a FAILED kickoff also renders the OWED badge")
	}
}

// TestACardWithNoFailedKickoffRendersNoFailureBadge is the control: every status,
// KickoffFailed=false — and a stray KickoffFailure string must not render on its
// own (the predicate decides, not the presence of text).
func TestACardWithNoFailedKickoffRendersNoFailureBadge(t *testing.T) {
	for _, st := range []string{"pending", "provisioning", "running", "stopped", "error"} {
		card := failedCardFor(t, st, false, failedFixtureError)
		if n := len(nodesWithAttr(t, card, "data-kickoff-failed")); n != 0 {
			t.Errorf("status %q with KickoffFailed=false renders %d failed badge(s)", st, n)
		}
		if strings.Contains(card, failedFixtureError) {
			t.Errorf("status %q with KickoffFailed=false still renders the error text", st)
		}
		if !strings.Contains(card, `aria-label="Status: `+st+`"`) {
			t.Errorf("status %q lost its status indicator", st)
		}
	}
}

// TestTheKickoffBadgesAreNotInsideThePollingStatusSpan extends the owed badge's
// placement guard to the failed badge and its detail: cardStatusIcon's span swaps
// its own innerHTML every 10s, so anything nested in it renders once and vanishes,
// which would read as "the failure resolved itself".
//
// ⚠ INVARIANT GUARD for placement; neither element was ever inside that span.
func TestTheKickoffBadgesAreNotInsideThePollingStatusSpan(t *testing.T) {
	card := failedCardFor(t, "running", true, failedFixtureError)
	var checked int
	for _, attr := range []string{"data-kickoff-failed", "data-kickoff-failure"} {
		for _, n := range nodesWithAttr(t, card, attr) {
			checked++
			for p := n.Parent; p != nil; p = p.Parent {
				for _, a := range p.Attr {
					if a.Key == "hx-swap" && strings.Contains(a.Val, "innerHTML") {
						t.Errorf("%s is nested inside an element whose hx-swap is %q, so the "+
							"first status poll erases it", attr, a.Val)
					}
				}
			}
		}
	}
	if checked != 2 {
		t.Fatalf("placement-checked %d element(s), want 2 (badge + detail) — fewer means the "+
			"guard is checking nothing", checked)
	}
}

// TestTheFailureTextIsEscapedNotRendered: the text can carry runtime-authored bytes
// (a quoted response body). It must render as text.
func TestTheFailureTextIsEscapedNotRendered(t *testing.T) {
	card := failedCardFor(t, "running", true, `responses HTTP 500: <script>alert("x")</script>`)
	if strings.Contains(card, "<script>") {
		t.Errorf("runtime-authored failure text was rendered as markup:\n%s", card)
	}
}
