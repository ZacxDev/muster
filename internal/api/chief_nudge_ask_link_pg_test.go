package api

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/ui"
)

// A CHIEF NUDGE'S ASK LINKS THE SESSION IT IS ABOUT, on a real Postgres under
// row-level security (nudgeSessionPath): the ask is muster's own
// (Asker.System), and its session is found through the decision that raised
// it. Both pages draw it — the ask page's "View in session" and the Asks
// list's card — and both answer boxes carry the mic when dictation is on.

// askContextLink is the ask page's "View in session" href, "" for none.
var askContextLink = regexp.MustCompile(`<a href="([^"]+)" class="[^"]*" data-ask-context>View in session</a>`)

// attentionCardLink is ask id's card's "View in session" href on the Asks
// list, "" for none (or no card).
func attentionCardLink(page string, id int64) string {
	k := `<li id="ask-` + strconv.FormatInt(id, 10) + `"`
	i := strings.Index(page, k)
	if i < 0 {
		return ""
	}
	li := page[i:]
	if j := strings.Index(li, "</li>"); j >= 0 {
		li = li[:j]
	}
	m := regexp.MustCompile(`<a href="([^"]+)"[^>]*data-attention-session>View in session</a>`).FindStringSubmatch(li)
	if m == nil {
		return ""
	}
	return m[1]
}

func askPageLink(page string) string {
	if m := askContextLink.FindStringSubmatch(page); m != nil {
		return m[1]
	}
	return ""
}

// TestALiveNudgeAskAboutAMirroredSessionLinksItsWindow: the sweep's live ask
// about the idle window @1; its ask page and its card link that window's
// rendered view, and each answer box has its mic. RED at origin/main: the
// nudge's ask had no session link at all (Asker.System), and no mic.
func TestALiveNudgeAskAboutAMirroredSessionLinksItsWindow(t *testing.T) {
	f := newNudgeFixture(t, true)
	f.srv.ext.Transcriber = stubTranscriber{}
	f.gw.set(nudgeAskReply)
	if n := f.pass(t); n != 1 {
		t.Fatalf("decided %d", n)
	}
	d := f.one(t)
	if d.Outcome != chiefnudge.OutcomeAsked || d.Kind != chiefnudge.KindMirror || d.AskID == 0 {
		t.Fatalf("live ask %+v", d)
	}
	const want = "/mirror/view?host=ws&amp;session=main&amp;window=%401"
	page := f.page(t, askPagePath(d.AskID))
	if got := askPageLink(page); got != want {
		t.Errorf("the ask page links %q, want %q", got, want)
	}
	if !strings.Contains(page, `id="ask-dictate"`) || !strings.Contains(page, `data-dictate-input="ask-input"`) {
		t.Error("the ask page's answer box has no mic")
	}
	list := f.page(t, "/attention")
	if got := attentionCardLink(list, d.AskID); got != want {
		t.Errorf("the Asks list's card links %q, want %q", got, want)
	}
	id := strconv.FormatInt(d.AskID, 10)
	if !strings.Contains(list, `id="answer-`+id+`-dictate"`) || strings.Count(list, "window.__musterDictate = true") != 1 {
		t.Error("the card has no mic, or the list does not carry the mic's script exactly once")
	}
	// The live refresh's fragment draws the same card.
	if got := attentionCardLink(f.page(t, "/attention/items"), d.AskID); got != want {
		t.Errorf("the list's live refresh links %q, want %q", got, want)
	}
}

// TestALiveNudgeAskAboutAnAgentLinksTheConversationAskedAbout: an agent with
// two conversations, the OLDER stalled; the link opens that one (its answer is
// relayed there), not the agent's newest. RED at origin/main: no link.
func TestALiveNudgeAskAboutAnAgentLinksTheConversationAskedAbout(t *testing.T) {
	f := newNudgeFixture(t, true)
	d, stalled, other := liveAgentAskOpen(t, f)
	if stalled.ID == other.ID {
		t.Fatal("fixture: one conversation")
	}
	a, err := f.srv.ext.Agents.Get(asUser(authz.OperatorUserID), d.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	want := ui.AgentSessionHref(a.Name, stalled.ID)
	if got := askPageLink(f.page(t, askPagePath(d.AskID))); got != want {
		t.Errorf("the ask page links %q, want %q", got, want)
	}
	if got := attentionCardLink(f.page(t, "/attention"), d.AskID); got != want {
		t.Errorf("the Asks list's card links %q, want %q", got, want)
	}
}

// TestASystemAskLinksOnlyItsOwnersDecision: a System ask of the operator's
// that no decision names links nothing; ANOTHER owner's decision naming the
// same ask id (written as that owner, live, about a session the operator's
// host does run) still links nothing — row-level security keeps it from the
// operator's read; and the operator's OWN decision naming it — the positive
// control, proving the read can see a row — links its session.
func TestASystemAskLinksOnlyItsOwnersDecision(t *testing.T) {
	f := newNudgeFixture(t, true)
	a, err := f.attn.Create(asUser(authz.OperatorUserID), attention.New{
		Asker: attention.Asker{System: true, Label: chiefNudgeAskLabel}, Question: "A System ask?"})
	if err != nil {
		t.Fatal(err)
	}
	links := func() (string, string) {
		return askPageLink(f.page(t, askPagePath(a.ID))), attentionCardLink(f.page(t, "/attention"), a.ID)
	}
	if p, c := links(); p != "" || c != "" {
		t.Fatalf("with no decision: page %q, card %q", p, c)
	}
	decision := func(owner int64, streak string) {
		t.Helper()
		if _, ok, err := f.nudges.Insert(asUser(owner), chiefnudge.Decision{Kind: chiefnudge.KindMirror, Key: nudgeSID, Host: "ws",
			TmuxSession: "main", Window: "@1", StreakKey: streak, Decision: chiefnudge.DecisionAsk, Question: "q",
			Mode: chiefnudge.ModeLive, Outcome: chiefnudge.OutcomeAsked, AskID: a.ID}, ""); err != nil || !ok {
			t.Fatalf("insert as %d: %v %v", owner, ok, err)
		}
	}
	decision(f.member, "since:theirs")
	if p, c := links(); p != "" || c != "" {
		t.Fatalf("another owner's decision is linked: page %q, card %q", p, c)
	}
	decision(authz.OperatorUserID, "since:mine")
	const want = "/mirror/view?host=ws&amp;session=main&amp;window=%401"
	if p, c := links(); p != want || c != want {
		t.Fatalf("the owner's own decision: page %q, card %q, want %q", p, c, want)
	}
}
