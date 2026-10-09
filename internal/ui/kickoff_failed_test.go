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
	return renderString(t, agentCard(AgentCardView{
		ID: 9, Name: "agent-9", DisplayName: "agent-9", Status: status,
		KickoffFailed: failed, KickoffFailure: msg,
	}))
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
		strings.TrimSpace(textOf(renderToString(t, remedies[0]))) != KickoffFailedRemedy {
		t.Errorf("the failure detail does not carry the remedy %q — nothing re-sends "+
			"automatically, so the card must say how to re-send by hand.\ndetail: %q",
			KickoffFailedRemedy, detail)
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
