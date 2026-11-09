package api

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
)

// #546 ROUND 2: the kickoff is never in the card's attention ask, so no
// reader of the ask is handed it; the owner's own card draws it from the
// proposal row. Literal refs computed with Python's hashlib.

// r2Kickoff is r1SecretKick's block 0, as the owner's card must show it.
const r2Kickoff = "/resume — deploy with password=" + r1Secret + " now\nRun rank 3."

// ownerGet serves GET path to the owner's signed-in browser through h.
func (f *succFixture) ownerGet(t *testing.T, h http.HandlerFunc, path string, id int64) string {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if id > 0 {
		r.SetPathValue("id", strconv.FormatInt(id, 10))
	}
	r = r.WithContext(authz.WithPrincipal(context.Background(), f.ownerSession()))
	w := httptest.NewRecorder()
	h(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, w.Code, w.Body)
	}
	return w.Body.String()
}

// TestTheKickoffIsInNoReaderOfTheAsk (🔴): a secret in the kickoff is in
// NONE of the ask's stored question, attention_list, attention_wait on the
// card (before and after the tap), the text the delivery sweep would send,
// the chief's chat rows or any tool result. The positive control: the
// owner's own Attention view and ask page DO show the exact kickoff, from
// the proposal row; another owner's do not.
func TestTheKickoffIsInNoReaderOfTheAsk(t *testing.T) {
	f := newSuccFixture(t)
	f.addTail(r1SecretKick)
	f.viewLine(t, sxOpenCodeKey, "run its kickoff in a new session")
	text, id := f.propose(t, sxCall(sxOpenCodeKey, "spinoff", r1SecretRef, intp(0)))
	if id <= 0 {
		t.Fatalf("the proposal: %s", text)
	}
	x, _ := f.plugins.ProposalByID(f.asOwner, id)
	if x.Kickoff != r2Kickoff {
		t.Fatalf("instrument check: the proposal row's kickoff is %q; want %q", x.Kickoff, r2Kickoff)
	}
	leaks := func(where, s string) {
		t.Helper()
		for _, needle := range []string{r1Secret, "Run rank 3."} {
			if strings.Contains(s, needle) {
				t.Errorf("%s holds the kickoff's %q:\n%s", where, needle, s)
			}
		}
	}
	leaks("the proposal tool's answer", text)
	a, err := f.srv.ext.Attention.Get(f.asOwner, x.AskID)
	if err != nil {
		t.Fatal(err)
	}
	leaks("the card's stored question", a.Question)
	want := "Start a session that continues “main:3”?\nSpin off a new one · host/host-echo/opencode · " + sxModel +
		" · acme/widgets, from the repository's default branch\nRead the kickoff (" + strconv.Itoa(len(r2Kickoff)) +
		" bytes, copied exactly from the session) in muster: on the session's view, or on this card in the Attention view."
	if a.Question != want {
		t.Errorf("the card's question:\n%q\nwant\n%q", a.Question, want)
	}

	list := f.callRaw(f.chief, attentionListTool, map[string]any{})
	if !strings.Contains(list, `"ask_id":`+strconv.FormatInt(x.AskID, 10)) && !strings.Contains(list, "ask-"+strconv.FormatInt(x.AskID, 10)) {
		t.Fatalf("instrument check: attention_list does not list the card:\n%s", list)
	}
	leaks("attention_list", list)
	wait := map[string]any{"id": x.AskID, "wait_seconds": 0}
	pending := f.callRaw(f.chief, attentionWaitTool, wait)
	if !strings.Contains(pending, `"status":"pending"`) {
		t.Fatalf("instrument check: attention_wait on the card: %s", pending)
	}
	leaks("attention_wait before the tap", pending)

	// The positive control: the owner's own pages draw the kickoff whole.
	whole := `data-attention-proposal-kickoff>` + html.EscapeString(r2Kickoff) + `</pre>`
	if page := f.ownerGet(t, f.srv.handleAttentionItems, "/attention/items", 0); !strings.Contains(page, whole) {
		t.Errorf("the owner's Attention view does not show the kickoff whole:\n%s", page)
	}
	if page := f.ownerGet(t, f.srv.handleAskPage, askPagePath(x.AskID), x.AskID); !strings.Contains(page, whole) {
		t.Errorf("the owner's ask page does not show the kickoff whole:\n%s", page)
	}
	other := authz.WithPrincipal(context.Background(), authz.Principal{UserID: f.otherOwner(t), Role: authz.RoleAdmin, Via: authz.ViaSession})
	if b := f.srv.proposalKickoffBody(other, a); b != nil {
		t.Errorf("another owner is drawn the kickoff: %+v", b)
	}

	if w := f.tapAs(t, f.ownerSession(), id, "@3", "Start it"); w.Code != http.StatusNoContent {
		t.Fatalf("the tap: %d %s", w.Code, w.Body)
	}
	answered := f.callRaw(f.chief, attentionWaitTool, wait)
	if !strings.Contains(answered, `"status":"answered"`) {
		t.Fatalf("instrument check: attention_wait after the tap: %s", answered)
	}
	leaks("attention_wait after the tap", answered)
	a, _ = f.srv.ext.Attention.Get(f.asOwner, x.AskID)
	// What the sweep would deliver, were closing the delivery to fail.
	leaks("the delivery sweep's text", renderAttentionAnswer(a))
	time.Sleep(f.srv.deliveryGrace() + time.Second)
	f.srv.deliverDueAnswers(f.asOwner)
	if n := f.chatRowsHolding(t, r1Secret); n != 0 {
		t.Errorf("%d of the chief's chat rows hold the kickoff's secret", n)
	}
	if n := f.chatRowsHolding(t, "Run rank 3."); n != 0 {
		t.Errorf("%d of the chief's chat rows hold the kickoff", n)
	}
	// G1: the Answered list says muster recorded it, not that the chief read it.
	page := f.ownerGet(t, f.srv.handleAttentionItems, "/attention/items", 0)
	if !strings.Contains(page, html.EscapeString(proposalAnsweredText)) || strings.Contains(page, "read it while waiting") {
		t.Errorf("the Answered item of a proposal card:\n%s", page)
	}
}

// TestACutKickoffIsCutOnTheAttentionCard: the Attention view's card draws a
// kickoff over its room cut, from the row, with the note that the whole is
// on the session's view; its question holds none of it.
func TestACutKickoffIsCutOnTheAttentionCard(t *testing.T) {
	f := newSuccFixture(t)
	f.addTail("Kickoff:\n```\n" + strings.Repeat("0123456789abcdef\n", 240) + "```\n")
	f.viewLine(t, sxOpenCodeKey, "run its long kickoff in a new session")
	_, id := f.propose(t, sxCall(sxOpenCodeKey, "spinoff", "m:d31fa7eb0e12", intp(0)))
	x, _ := f.plugins.ProposalByID(f.asOwner, id)
	a, _ := f.srv.ext.Attention.Get(f.asOwner, x.AskID)
	if strings.Contains(a.Question, "0123456789abcdef") || strings.Join(a.Options, "|") != "Review on the session's view" {
		t.Fatalf("the cut card's ask: %q %v", a.Question, a.Options)
	}
	page := f.ownerGet(t, f.srv.handleAttentionItems, "/attention/items", 0)
	if !strings.Contains(page, "data-attention-proposal-cut>"+html.EscapeString(successionCutNote)) ||
		!strings.Contains(page, `data-attention-proposal-kickoff>`+strings.Repeat("0123456789abcdef\n", 100)) ||
		strings.Contains(page, strings.Repeat("0123456789abcdef\n", 239)) {
		t.Errorf("the Attention view's cut card:\n%s", page)
	}
}

// TestOnlyHidingCharactersAreRefused (🟡): the bidi embeddings, overrides
// and isolates, the zero-width space, word joiner and BOM, and the tag
// characters are refused; an emoji ZWJ sequence, Persian with a ZWNJ,
// right-to-left text with a right-to-left mark and a soft hyphen are
// ordinary text.
func TestOnlyHidingCharactersAreRefused(t *testing.T) {
	for _, r := range []rune{0x202A, 0x202B, 0x202C, 0x202D, 0x202E, 0x2066, 0x2067, 0x2068, 0x2069, 0x200B, 0x2060, 0xFEFF, 0xE0000, 0xE0041, 0xE007F} {
		if kickoffSendable("run " + string(r) + " this") {
			t.Errorf("U+%04X is sent", r)
		}
	}
	for _, s := range []string{
		"ship it \U0001F469\u200d\U0001F4BB now",                    // an emoji ZWJ sequence
		"\u0645\u06cc\u200c\u062e\u0648\u0627\u0647\u0645 continue", // Persian with a ZWNJ
		"\u05e9\u05dc\u05d5\u05dd\u200f run step 2",                 // Hebrew, then a right-to-left mark
		"left\u200e to right",                                       // a left-to-right mark
		"hy\u00adphen",                                              // a soft hyphen
		"\u0928\u092e\u0938\u094d\u200d\u0924\u0947 and \u0c95\u0ca8\u0ccd\u200c\u0ca8\u0ca1", // Indic with a ZWJ and a ZWNJ
	} {
		if !kickoffSendable(s) {
			t.Errorf("%q is refused", s)
		}
	}
}
