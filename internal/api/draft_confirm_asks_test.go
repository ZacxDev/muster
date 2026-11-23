package api

import (
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/plugins"
)

// TestADraftRequestIsReadFromItsOpenStepUpCardOnly: an open plugin_draft_decide
// card that still matches its draft is its draft's request — who asked, and a
// rejection's reason; an answered one, a changed one, another tool's, or one
// muster did not file is not.
func TestADraftRequestIsReadFromItsOpenStepUpCardOnly(t *testing.T) {
	mk := func(mod func(*attention.Ask)) attention.Ask {
		a := attention.Ask{ID: 731, Status: attention.Open, Asker: attention.Asker{System: true, Label: hostDecisionLabel},
			StepUp: &attention.StepUpCard{Tool: pluginDraftDecideTool, Title: "Post to X? · Draft #52", Why: "W", Asker: "work:1", DraftID: 52}}
		if mod != nil {
			mod(&a)
		}
		return a
	}
	if r := draftRequestOf(mk(nil)); r == nil || *r != (plugins.DraftRequest{AskID: 731, Asker: "work:1"}) {
		t.Errorf("an approve request: %+v", r)
	}
	rej := mk(func(a *attention.Ask) {
		a.StepUp.Title, a.StepUp.Why = stepUpWithDraftID(stepUpRejectTitle, 52), stepUpRejectWhy+": too long"
	})
	if r := draftRequestOf(rej); r == nil || *r != (plugins.DraftRequest{AskID: 731, Asker: "work:1", Reject: true, Reason: "too long"}) {
		t.Errorf("a reject request: %+v", r)
	}
	bare := mk(func(a *attention.Ask) {
		a.StepUp.Title, a.StepUp.Why = stepUpWithDraftID(stepUpRejectTitle, 52), stepUpRejectWhy
	})
	if r := draftRequestOf(bare); r == nil || !r.Reject || r.Reason != "" {
		t.Errorf("a reject with no reason: %+v", r)
	}
	for name, a := range map[string]attention.Ask{
		"answered":        mk(func(a *attention.Ask) { a.Status = attention.Answered }),
		"changed":         mk(func(a *attention.Ask) { a.StepUp.Changed = true }),
		"another tool":    mk(func(a *attention.Ask) { a.StepUp.Tool = "connector_request_decide" }),
		"not muster's":    mk(func(a *attention.Ask) { a.Asker = attention.Asker{AgentID: 3, Label: hostDecisionLabel} }),
		"no draft":        mk(func(a *attention.Ask) { a.StepUp.DraftID = 0 }),
		"no step-up card": mk(func(a *attention.Ask) { a.StepUp = nil }),
	} {
		if r := draftRequestOf(a); r != nil {
			t.Errorf("%s: read as a request %+v", name, r)
		}
	}
}

// TestADraftCardDrawsItsRequestAndCappedAskChips: the card builder over a card
// the reader set — the request line (who, what, a rejection's reason, every
// part escaped and its hidden code points flagged), its chips in the order
// given, each a link to its ask's page, then "+N"; a request to reject leads
// with Reject. A card with neither draws none of it.
func TestADraftCardDrawsItsRequestAndCappedAskChips(t *testing.T) {
	now := time.Now()
	d := plugins.Draft{ID: 52, Status: plugins.DraftPending, Body: "Hello", CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)}
	plain := draftCardHTMLAt(plugins.DraftCard{Draft: d, PluginLabel: "Inbox"}, now)
	for _, no := range []string{"data-draft-asks", "data-draft-request", "data-id-link", "data-id-more"} {
		if strings.Contains(plain, no) {
			t.Errorf("a card with no confirm ask draws %s", no)
		}
	}
	c := plugins.DraftCard{Draft: d, PluginLabel: "Inbox", ConfirmAskIDs: []int64{9007, 9003, 9005}, ConfirmAsks: 7,
		Request: &plugins.DraftRequest{AskID: 9007, Asker: "work:<1>\u202e", Reject: true, Reason: "too <b>long</b>"}}
	got := draftCardHTMLAt(c, now)
	for _, want := range []string{
		`<a href="/attention/asks/9007" class="`, `data-id="9007" aria-label="Open Ask #9007">Ask #9007</a>`,
		`<a href="/attention/asks/9003" class="`, `data-id="9003" aria-label="Open Ask #9003">Ask #9003</a>`,
		`<a href="/attention/asks/9005" class="`, `data-id="9005" aria-label="Open Ask #9005">Ask #9005</a>`,
		`data-id-more="4" title="4 more">+4</span>`,
		`data-draft-request data-ask-id="9007">work:&lt;1&gt;`,
		` asked to reject this: too &lt;b&gt;long&lt;/b&gt;. Decide it here, or confirm on your phone.</p>`,
		`data-stepup-hidden="U+202E"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the card lacks %q:\n%s", want, got)
		}
	}
	if a, b, e := strings.Index(got, "Ask #9007"), strings.Index(got, "Ask #9003"), strings.Index(got, "Ask #9005"); !(a < b && b < e) {
		t.Errorf("the chips are not in the order given (%d, %d, %d)", a, b, e)
	}
	if strings.Contains(got, "<b>long") {
		t.Error("the reason is drawn unescaped")
	}
	if r, a := strings.Index(got, "data-draft-reject"), strings.Index(got, "data-draft-approve"); r < 0 || a < 0 || r > a {
		t.Errorf("a request to reject does not lead with Reject (reject at %d, approve at %d)", r, a)
	}
	// An approve request: who is unnamed, Approve leads.
	c.Request = &plugins.DraftRequest{AskID: 9007}
	got = draftCardHTMLAt(c, now)
	if !strings.Contains(got, `data-ask-id="9007">A session asked to approve and send this. Decide it here, or confirm on your phone.</p>`) {
		t.Errorf("an approve request's line:\n%s", got)
	}
	if r, a := strings.Index(got, "data-draft-reject"), strings.Index(got, "data-draft-approve"); a < 0 || r < a {
		t.Errorf("an approve request does not lead with Approve (reject at %d, approve at %d)", r, a)
	}
}
