package api

import (
	"context"
	"errors"
	"log"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/attention"
	"github.com/ZacxDev/muster/internal/chiefnudge"
	"github.com/ZacxDev/muster/internal/ui"
)

// THE ASKS' ANSWER BOXES: the microphone on every one (ui.DictateControlHTML)
// and the "View in session" link (attentionSessionPath), including a chief
// nudge's ask, which names no session of its own (nudgeSessionPath).

// boxNudges is a chief-nudge store holding exactly byAsk; ByAsk is the one
// method the link reads (any other call panics on the nil Store). It answers
// WITHOUT row-level security — whatever row names the ask, any owner's — so
// the explicit owner check is what is under test.
type boxNudges struct {
	chiefnudge.Store
	byAsk map[int64]chiefnudge.Decision
	err   error
}

func (b boxNudges) ByAsk(_ context.Context, id int64) (chiefnudge.Decision, error) {
	if b.err != nil {
		return chiefnudge.Decision{}, b.err
	}
	d, ok := b.byAsk[id]
	if !ok {
		return chiefnudge.Decision{}, chiefnudge.ErrNotFound
	}
	return d, nil
}

// The nudge fixture's asks. 71..76 are nudge asks (their decisions below);
// 77 is a System ask no decision names.
const (
	boxNudgeMirror      = 71 // a mirrored session @2 runs: its window's view
	boxNudgeAgent       = 72 // the owner's agent 7, conversation 4242
	boxNudgeTheirs      = 73 // a decision of ANOTHER owner's naming this ask id
	boxNudgeNoWindow    = 74 // a mirrored session no window runs now
	boxNudgeTheirAgent  = 75 // a decision of the owner's naming another owner's agent
	boxNudgeOtherHost   = 76 // a mirrored session on a host the owner does not have
	boxSystemNoDecision = 77
)

// boxServer is pushTargetServer (host "ws", agents 7 and 8) with the chief
// nudge decisions above and, when dictate, dictation configured.
func boxServer(t *testing.T, dictate bool) *Server {
	t.Helper()
	s := pushTargetServer(t, nil, nil)
	s.ext.ChiefNudges = boxNudges{byAsk: map[int64]chiefnudge.Decision{
		boxNudgeMirror: {Owner: 1, AskID: boxNudgeMirror, Kind: chiefnudge.KindMirror, Key: ptOtherSID, Host: "ws",
			// The address the decision recorded is STALE (the window was @9 then):
			// the link follows the Claude session to the window running it now.
			TmuxSession: "old", Window: "@9"},
		boxNudgeAgent:      {Owner: 1, AskID: boxNudgeAgent, Kind: chiefnudge.KindAgent, Key: chiefnudge.AgentKey(7, 4242), AgentID: 7, ChatSessionID: 4242},
		boxNudgeTheirs:     {Owner: 2, AskID: boxNudgeTheirs, Kind: chiefnudge.KindAgent, Key: chiefnudge.AgentKey(7, 1), AgentID: 7, ChatSessionID: 1},
		boxNudgeNoWindow:   {Owner: 1, AskID: boxNudgeNoWindow, Kind: chiefnudge.KindMirror, Key: "1a2b3c4d-0000-4000-8000-000000000074", Host: "ws"},
		boxNudgeTheirAgent: {Owner: 1, AskID: boxNudgeTheirAgent, Kind: chiefnudge.KindAgent, Key: chiefnudge.AgentKey(8, 5), AgentID: 8, ChatSessionID: 5},
		boxNudgeOtherHost:  {Owner: 1, AskID: boxNudgeOtherHost, Kind: chiefnudge.KindMirror, Key: ptOtherSID, Host: "elsewhere"},
	}}
	if dictate {
		s.ext.Transcriber = stubTranscriber{}
	}
	return s
}

func systemAsk(id int64) attentionItem {
	return askItem(attention.Ask{ID: id, Owner: 1, Asker: attention.Asker{System: true, Label: chiefNudgeAskLabel}, Question: "q", CreatedAt: time.Now()}, nil)
}

// TestAChiefNudgesAskLinksTheSessionItIsAbout: a nudge's ask is muster's own
// (Asker.System), so it names no session; its link is the session its
// decision is about — a mirrored session's CURRENT window (not the address
// recorded), an agent's page at the conversation asked about. Never another
// owner's decision, never another owner's agent, nothing for a System ask no
// decision names, a session no window runs or a host the owner lacks. RED at
// origin/main: every nudge ask had no link.
func TestAChiefNudgesAskLinksTheSessionItIsAbout(t *testing.T) {
	s := boxServer(t, false)
	ctx := s.attentionPrincipal(context.Background(), 1)
	for _, c := range []struct {
		name string
		id   int64
		want string
	}{
		{"a nudge about a mirrored session", boxNudgeMirror, "/mirror/view?host=ws&session=main&window=%402"},
		{"a nudge about an agent's conversation", boxNudgeAgent, "/agents/ptworker-7?session=4242"},
		{"another owner's decision naming this ask", boxNudgeTheirs, ""},
		{"a nudge about a session no window runs", boxNudgeNoWindow, ""},
		{"a nudge naming another owner's agent", boxNudgeTheirAgent, ""},
		{"a nudge about a session on a host the owner lacks", boxNudgeOtherHost, ""},
		{"a System ask no decision names", boxSystemNoDecision, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			it := systemAsk(c.id)
			if got := s.attentionSessionPath(ctx, 1, it); got != c.want {
				t.Fatalf("its link is %q, want %q", got, c.want)
			}
			// 🔴 THE TAP IS UNCHANGED: an open ask's notification lands on its
			// own page whatever its link.
			if got := s.pushTargetFor(ctx, 1, it); got != askPagePath(c.id) {
				t.Fatalf("its tap opens %q, want its ask page", got)
			}
		})
	}
	// Without the nudge store (chief nudges off) nothing is read.
	s.ext.ChiefNudges = nil
	if got := s.attentionSessionPath(ctx, 1, systemAsk(boxNudgeMirror)); got != "" {
		t.Fatalf("with chief nudges off the link is %q", got)
	}
}

// TestANudgeStoreFailureLinksNothingAndSaysWhy: a read failure (not a missing
// row) gives no link and is logged; a missing row is not.
func TestANudgeStoreFailureLinksNothingAndSaysWhy(t *testing.T) {
	s := boxServer(t, false)
	var logs strings.Builder
	s.logger = log.New(&syncWriter{w: &logs}, "", 0)
	ctx := s.attentionPrincipal(context.Background(), 1)
	if got := s.attentionSessionPath(ctx, 1, systemAsk(boxSystemNoDecision)); got != "" || logs.Len() != 0 {
		t.Fatalf("a missing decision: link %q, log %q", got, logs.String())
	}
	s.ext.ChiefNudges = boxNudges{err: errors.New("connection reset")}
	if got := s.attentionSessionPath(ctx, 1, systemAsk(boxNudgeMirror)); got != "" {
		t.Fatalf("its link is %q, want none", got)
	}
	if !strings.Contains(logs.String(), "the session of ask 71's chief nudge: connection reset") {
		t.Fatalf("log %q", logs.String())
	}
}

// dictateTag is one mic button as ui.DictateControlHTML draws it.
var dictateTag = regexp.MustCompile(`<button id="([^"]+)" type="button" data-dictate="/transcribe" data-dictate-input="([^"]+)" data-dictate-note="([^"]+)"`)

// boxListItems is one ask of each kind the link follows, plus a waiting
// session (which has no answer box), all as the owner's queue would hold them.
func boxListItems(t *testing.T, s *Server) []attentionItem {
	now := time.Now()
	return []attentionItem{
		askItem(attention.Ask{ID: 81, Owner: 1, Asker: attention.Asker{MirrorHost: "ws", MirrorSession: ptOtherSID, Label: "s"}, Question: "mirror?", CreatedAt: now}, nil),
		askItem(attention.Ask{ID: 82, Owner: 1, Asker: attention.Asker{AgentID: 7, Label: "Worker Seven"}, Question: "agent?", CreatedAt: now}, nil),
		systemAsk(boxNudgeMirror),
		systemAsk(boxNudgeAgent),
		systemAsk(boxSystemNoDecision),
		waitingItem(t, s, "@1"),
	}
}

// TestEveryAskCardCarriesItsOwnMicAndItsSessionLink: on the Asks list, each
// ask's answer box has a mic addressing THAT box, with its own button and note
// ids (the list draws many); a waiting session's card has none. Each ask card
// links its session — a mirrored session's window, an agent's page, a chief
// nudge's session — and a System ask with no session links nothing. RED at
// origin/main: no card had a mic or a "View in session" link.
func TestEveryAskCardCarriesItsOwnMicAndItsSessionLink(t *testing.T) {
	s := boxServer(t, true)
	ctx := s.attentionPrincipal(context.Background(), 1)
	items := boxListItems(t, s)
	s.withAskBoxes(ctx, items, true)
	page := attentionItemsHTML(items, time.Now())

	tags := dictateTag.FindAllStringSubmatch(page, -1)
	if len(tags) != 5 {
		t.Fatalf("%d mics on the list, want one per ask (5):\n%s", len(tags), page)
	}
	seen := map[string]bool{}
	for _, m := range tags {
		button, input, note := m[1], m[2], m[3]
		for _, id := range []string{button, input, note} {
			if seen[id] {
				t.Fatalf("id %q is used by two mics", id)
			}
			seen[id] = true
		}
		if !strings.Contains(page, `<textarea id="`+input+`" name="answer"`) {
			t.Errorf("mic %s addresses #%s, which is no answer box on the page", button, input)
		}
		if strings.Count(page, `id="`+note+`"`) != 1 {
			t.Errorf("mic %s's note #%s is not on the page exactly once", button, note)
		}
	}
	for _, id := range []string{"81", "82", strconv.Itoa(boxNudgeMirror), strconv.Itoa(boxNudgeAgent), strconv.Itoa(boxSystemNoDecision)} {
		if !seen["answer-"+id] {
			t.Errorf("ask %s's box has no mic", id)
		}
	}
	// The mic sits in its own card's form, beside that card's Answer.
	for _, li := range strings.Split(page, "</li>") {
		if !strings.Contains(li, `data-kind="ask"`) {
			if strings.Contains(li, "data-dictate") {
				t.Errorf("a card that is not an ask carries a mic:\n%s", li)
			}
			continue
		}
		m := dictateTag.FindStringSubmatch(li)
		if m == nil || !strings.Contains(li, `id="`+m[2]+`"`) {
			t.Errorf("an ask card's mic is not its own box's:\n%s", li)
		}
	}

	links := regexp.MustCompile(`<li id="ask-(\d+)"[\s\S]*?(?:</li>)`).FindAllStringSubmatch(page, -1)
	got := map[string]string{}
	for _, l := range links {
		if h := regexp.MustCompile(`<a href="([^"]+)"[^>]*data-attention-session>View in session</a>`).FindStringSubmatch(l[0]); h != nil {
			got[l[1]] = h[1]
		}
	}
	want := map[string]string{
		"81":                         "/mirror/view?host=ws&amp;session=main&amp;window=%402",
		"82":                         "/agents/ptworker-7",
		strconv.Itoa(boxNudgeMirror): "/mirror/view?host=ws&amp;session=main&amp;window=%402",
		strconv.Itoa(boxNudgeAgent):  "/agents/ptworker-7?session=4242",
	}
	if len(got) != len(want) {
		t.Errorf("links %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("ask %s links %q, want %q", k, got[k], v)
		}
	}
}

// TestNoServiceNoMicOnAnyAnswerBox: with dictation off (dictatePath "") no
// answer box draws a mic, a note or the script — the list, a session page's
// block and the ask page's composer alike. The links do not depend on it.
func TestNoServiceNoMicOnAnyAnswerBox(t *testing.T) {
	s := boxServer(t, false)
	ctx := s.attentionPrincipal(context.Background(), 1)
	items := boxListItems(t, s)
	s.withAskBoxes(ctx, items, true)
	r := httptest.NewRequest("GET", askPagePath(boxNudgeMirror), nil)
	for name, html := range map[string]string{
		"the list":      attentionItemsHTML(items, time.Now()),
		"a block":       askerAsksHTML(items, "/agents/ptworker-7", time.Now()),
		"the composer":  askComposerHTML(r, attention.Ask{ID: boxNudgeMirror}, s.dictatePath()),
		"the page's js": ui.DictateScriptHTML(s.dictatePath()),
	} {
		if strings.Contains(html, "data-dictate") || strings.Contains(html, "__musterDictate") || strings.Contains(html, "-dictate-note") {
			t.Errorf("%s draws dictation with no service:\n%s", name, html)
		}
	}
	if !strings.Contains(attentionItemsHTML(items, time.Now()), `data-attention-session>View in session</a>`) {
		t.Fatal("control: the links went with the mic")
	}
}

// TestTheAskComposerCarriesTheMicBesideAnswer: the ask page's pinned answer
// box has the mic addressing #ask-input, between the box and Answer, its note
// on the composer, and the script AFTER both (its first run reveals only the
// mics already parsed). RED at origin/main: no mic.
func TestTheAskComposerCarriesTheMicBesideAnswer(t *testing.T) {
	r := httptest.NewRequest("GET", askPagePath(9), nil)
	html := askComposerHTML(r, attention.Ask{ID: 9}, ui.TranscribePath)
	m := dictateTag.FindStringSubmatch(html)
	if m == nil || m[1] != "ask-dictate" || m[2] != "ask-input" || m[3] != "ask-dictate-note" {
		t.Fatalf("the composer's mic: %v\n%s", m, html)
	}
	box, mic, send := strings.Index(html, `<textarea id="ask-input"`), strings.Index(html, `id="ask-dictate"`), strings.Index(html, `data-ask-send`)
	note, script := strings.Index(html, `id="ask-dictate-note"`), strings.Index(html, "window.__musterDictate")
	if box < 0 || !(box < mic && mic < send) {
		t.Errorf("the mic is not between the box and Answer (box %d, mic %d, Answer %d)", box, mic, send)
	}
	if note < 0 || script < send || script < note {
		t.Errorf("the note (%d) or the script (%d) is missing or before the form", note, script)
	}
	if strings.Count(html, "window.__musterDictate = true") != 1 {
		t.Error("the composer carries the script other than once")
	}
}
