package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/askq"
	"github.com/ZacxDev/muster/internal/mirror"
)

// AN OPENCODE WINDOW'S ASK IN ITS RENDERED VIEW (mirror_opencode_ask.go),
// through the real routes: the live region draws the whole question (or the
// permission request) as a card, and its submit relays exactly that ask's
// answer — options by index — or is refused with nothing relayed.
//
// 🔴 REGRESSION TEST. On origin/main the region draws nothing for an opencode
// window, and a submit for one 404s.

const ocAskSID = "ses_e5d55c179ffe6MEUqkUCZbHWA9"

// ocFormMirror is a host "ws" whose window @1 runs opencode waiting on the
// two-question form ocAskForm (or the permission request ocAskPerm when perm
// is set); it records each form read and each answer.
type ocFormMirror struct {
	*fakeMirror
	perm bool
	// custom, when set, is the form the host sends in place of form().
	custom  *mirror.OpenCodeForm
	forms   bool
	mu      sync.Mutex
	reads   []string
	answers []string
	picks   [][]mirror.OpenCodePick
	err     error
}

const (
	ocAskForm = "frm_1a2aa3ec2001NmTTM10yDwGseq"
	ocAskPerm = "per_1a2aa6345001HPvxtQP9FD941m"
)

func newOCFormMirror(perm bool) *ocFormMirror {
	p := &mirror.Pending{Kind: mirror.PendingQuestion, Tool: "question", Summary: "Which database should the fixture use? (and 1 more question)", PromptID: ocAskForm}
	if perm {
		p = &mirror.Pending{Kind: mirror.PendingPermission, Tool: "bash", Summary: "bash: rm -rf build", PromptID: ocAskPerm, Choices: "once reject"}
	}
	ws := []mirror.Window{{Session: "main", ID: "@1", Index: 0, Active: true, Command: "opencode", Runtime: mirror.RuntimeOpenCode,
		AgentSession: ocAskSID, Status: mirror.StatusWaiting, StatusSince: 1792000001111, Pending: p}}
	return &ocFormMirror{fakeMirror: &fakeMirror{online: true, interactive: true, statusReport: true, v2: &fakeMirrorV2{}, windows: ws}, perm: perm, forms: true}
}

func (m *ocFormMirror) Hosts(ctx context.Context) []mirror.HostView {
	hs := m.fakeMirror.Hosts(ctx)
	for i := range hs {
		hs[i].OpenCodeAnswer, hs[i].OpenCodeForms = true, m.forms
	}
	return hs
}

func (m *ocFormMirror) form() mirror.OpenCodeForm {
	return mirror.OpenCodeForm{PromptID: ocAskForm, Title: "Questions", Questions: []mirror.OpenCodeQuestion{
		{Key: "q0", Header: "Database", Question: "Which database should the fixture use?", Custom: true,
			Options: []mirror.OpenCodeOption{{Label: "Postgres (Recommended)", Description: "the default"}, {Label: "SQLite"}}},
		{Key: "q1", Header: "Extras", Question: "Which extras?", Multiple: true, Custom: true,
			Options: []mirror.OpenCodeOption{{Label: "cache"}, {Label: "metrics"}}},
	}}
}

func (m *ocFormMirror) OpenCodeForm(ctx context.Context, host, session, window, promptID string) (mirror.OpenCodeForm, error) {
	m.mu.Lock()
	m.reads = append(m.reads, strings.Join([]string{host, session, window, promptID}, " "))
	m.mu.Unlock()
	if promptID != ocAskForm {
		return mirror.OpenCodeForm{}, mirror.ErrOpenCodeAskGone
	}
	if m.custom != nil {
		return *m.custom, nil
	}
	return m.form(), nil
}

func (m *ocFormMirror) AnswerOpenCodeForm(ctx context.Context, host, session, window string, form mirror.OpenCodeForm, picks []mirror.OpenCodePick) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.answers = append(m.answers, strings.Join([]string{host, session, window, form.PromptID}, " "))
	m.picks = append(m.picks, picks)
	return m.err
}

func (m *ocFormMirror) AnswerOpenCodeAsk(ctx context.Context, host, session, window, promptID, choice string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.answers = append(m.answers, strings.Join([]string{host, session, window, promptID, choice}, " "))
	return m.err
}

func (m *ocFormMirror) got() ([]string, [][]mirror.OpenCodePick) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.answers...), append([][]mirror.OpenCodePick(nil), m.picks...)
}

// TestTheViewDrawsAnOpenCodeQuestionWhole: every question, every option (with
// its description), a multi-select's checkboxes, an "Other" box, one Submit —
// named for opencode, keyed by the ask's id.
func TestTheViewDrawsAnOpenCodeQuestionWhole(t *testing.T) {
	m := newOCFormMirror(false)
	s, ts := viewServer(t, m.fakeMirror)
	s.UseExtensions(Extensions{Mirror: m, Notes: stubNotesStore{}, Agents: mirrorAgents{}, SessionLiveness: stubLiveness{}, Provisioner: newFakeTerminal()})
	_, card := mirrorGet(t, s, ts, asksPath)
	for _, want := range []string{`data-ask-tool="` + ocAskForm + `"`, "opencode is asking", "Which database should the fixture use?",
		"Postgres (Recommended)", "the default", "SQLite", "Which extras?", `type="checkbox"`, `name="a1" value="1"`, "Type your own answer", "data-ask-submit"} {
		if !strings.Contains(card, want) {
			t.Errorf("the card lacks %q:\n%s", want, card)
		}
	}
	if strings.Contains(card, "Claude is asking") {
		t.Error("the card calls opencode Claude")
	}
	if r := m.reads; len(r) != 1 || r[0] != "ws main @1 "+ocAskForm {
		t.Fatalf("form reads %v", r)
	}
}

// TestAnOpenCodeQuestionsSubmitRelaysThatAskByIndex: the answers become one
// pick list per question — option INDEXES, an answer of one's own — for
// exactly the window and ask the card showed.
func TestAnOpenCodeQuestionsSubmitRelaysThatAskByIndex(t *testing.T) {
	m := newOCFormMirror(false)
	s, ts := viewServer(t, m.fakeMirror)
	s.UseExtensions(Extensions{Mirror: m, Notes: stubNotesStore{}, Agents: mirrorAgents{}, SessionLiveness: stubLiveness{}, Provisioner: newFakeTerminal()})
	resp, body := answerReq(t, s, ts, answerForm(ocAskForm, "a0", "1", "a1", "1", "a1", "__other__", "o1", "tracing"), "", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Answered here — opencode has it.") {
		t.Fatalf("answer = %d:\n%s", resp.StatusCode, body)
	}
	calls, picks := m.got()
	want := []mirror.OpenCodePick{{Key: "q0", Picks: []int{1}}, {Key: "q1", Picks: []int{1}, Other: "tracing"}}
	if len(calls) != 1 || calls[0] != "ws main @1 "+ocAskForm || !reflect.DeepEqual(picks[0], want) {
		t.Fatalf("relayed %v %+v", calls, picks)
	}
}

// TestAnOpenCodePermissionCardRelaysOnceOrReject: Allow once / Reject, no
// answer of one's own, never "always".
func TestAnOpenCodePermissionCardRelaysOnceOrReject(t *testing.T) {
	m := newOCFormMirror(true)
	s, ts := viewServer(t, m.fakeMirror)
	s.UseExtensions(Extensions{Mirror: m, Notes: stubNotesStore{}, Agents: mirrorAgents{}, SessionLiveness: stubLiveness{}, Provisioner: newFakeTerminal()})
	_, card := mirrorGet(t, s, ts, asksPath)
	if !strings.Contains(card, "bash: rm -rf build") || !strings.Contains(card, "Allow once") || !strings.Contains(card, `name="a0" value="1"`) ||
		strings.Contains(card, "Type your own answer") || strings.Contains(card, "always") {
		t.Fatalf("the permission card:\n%s", card)
	}
	if resp, body := answerReq(t, s, ts, answerForm(ocAskPerm, "a0", "1"), "", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("answer = %d:\n%s", resp.StatusCode, body)
	}
	if calls, _ := m.got(); len(calls) != 1 || calls[0] != "ws main @1 "+ocAskPerm+" reject" {
		t.Fatalf("relayed %v", calls)
	}
}

// TestAnOpenCodeSubmitForAnotherAskIsRefused: a submit naming an ask the
// window does not wait on NOW (an older card, another id), or answers the
// form cannot take, relays nothing.
func TestAnOpenCodeSubmitForAnotherAskIsRefused(t *testing.T) {
	m := newOCFormMirror(false)
	s, ts := viewServer(t, m.fakeMirror)
	s.UseExtensions(Extensions{Mirror: m, Notes: stubNotesStore{}, Agents: mirrorAgents{}, SessionLiveness: stubLiveness{}, Provisioner: newFakeTerminal()})
	for name, c := range map[string]struct {
		form url.Values
		code int
	}{
		"another ask's id":          {answerForm("frm_1a2aaa3d1001lEte0g1lyG1pFi", "a0", "1", "a1", "0"), http.StatusConflict},
		"no id":                     {answerForm("", "a0", "1", "a1", "0"), http.StatusConflict},
		"a label, not an index":     {answerForm(ocAskForm, "a0", "SQLite", "a1", "0"), http.StatusBadRequest},
		"an index past the options": {answerForm(ocAskForm, "a0", "2", "a1", "0"), http.StatusBadRequest},
		"a question unanswered":     {answerForm(ocAskForm, "a0", "1"), http.StatusBadRequest},
	} {
		if resp, body := answerReq(t, s, ts, c.form, "", ""); resp.StatusCode != c.code {
			t.Errorf("%s: %d, want %d:\n%s", name, resp.StatusCode, c.code, body)
		}
	}
	if calls, _ := m.got(); len(calls) != 0 {
		t.Fatalf("relayed %v", calls)
	}
}

// TestAnOlderHostsOpenCodeQuestionSaysWhy: a host that sends no forms gets a
// card that shows the question and says it cannot be answered here.
func TestAnOlderHostsOpenCodeQuestionSaysWhy(t *testing.T) {
	m := newOCFormMirror(false)
	m.forms = false
	s, ts := viewServer(t, m.fakeMirror)
	s.UseExtensions(Extensions{Mirror: m, Notes: stubNotesStore{}, Agents: mirrorAgents{}, SessionLiveness: stubLiveness{}, Provisioner: newFakeTerminal()})
	_, card := mirrorGet(t, s, ts, asksPath)
	if !strings.Contains(card, html_(openCodeCardNoForms)) || strings.Contains(card, "data-ask-submit") {
		t.Fatalf("the card:\n%s", card)
	}
}

// TestEveryQuestionsFullBoxOfCJKTextFits: a form of mirror.MaxFormQuestions
// free-text questions, each answered with a full box (askq.MaxFreeText
// characters) of three-byte text — 9 bytes each url-encoded — is taken and
// relayed whole, not refused as too large.
//
// 🔴 REGRESSION TEST. At 5c4c3e4e the answer form's bound was a fixed 64 KiB:
// four full boxes of CJK text came to about 73.8 KB and got a 413.
func TestEveryQuestionsFullBoxOfCJKTextFits(t *testing.T) {
	m := newOCFormMirror(false)
	f := mirror.OpenCodeForm{PromptID: ocAskForm}
	full := strings.Repeat("字", askq.MaxFreeText)
	args := []string{}
	var want []mirror.OpenCodePick
	for i := range mirror.MaxFormQuestions {
		key, idx := fmt.Sprintf("q%d", i), strconv.Itoa(i)
		f.Questions = append(f.Questions, mirror.OpenCodeQuestion{Key: key, Header: "Why " + idx, Question: "Say why, part " + idx, Custom: true})
		args = append(args, "a"+idx, askq.OtherValue, "o"+idx, full)
		want = append(want, mirror.OpenCodePick{Key: key, Other: full})
	}
	m.custom = &f
	s, ts := viewServer(t, m.fakeMirror)
	s.UseExtensions(Extensions{Mirror: m, Notes: stubNotesStore{}, Agents: mirrorAgents{}, SessionLiveness: stubLiveness{}, Provisioner: newFakeTerminal()})
	form := answerForm(ocAskForm, args...)
	if n := len(form.Encode()); n <= 64<<10*2 {
		t.Fatalf("the fixture's body is %d bytes: it must overshoot twice the old 64 KiB bound to test anything", n)
	}
	resp, body := answerReq(t, s, ts, form, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%d full boxes of CJK text: %d %.200q", mirror.MaxFormQuestions, resp.StatusCode, body)
	}
	if _, picks := m.got(); len(picks) != 1 || !reflect.DeepEqual(picks[0], want) {
		t.Fatalf("relayed %d pick lists; want the %d answers whole", len(picks), len(want))
	}
}

// mixedForm: q0 takes only its options; q1 has none and takes only an answer
// of one's own (free text).
func mixedForm() mirror.OpenCodeForm {
	return mirror.OpenCodeForm{PromptID: ocAskForm, Questions: []mirror.OpenCodeQuestion{
		{Key: "q0", Header: "Branch", Question: "Which branch?", Options: []mirror.OpenCodeOption{{Label: "main"}, {Label: "next"}}},
		{Key: "q1", Header: "Why", Question: "Say why, in your words", Custom: true},
	}}
}

// TestAMixedFormKeepsEachQuestionsOwnAnswerBox: a question that takes only
// its options has no "Other"; the free-text-only question beside it keeps
// its box — so the form can be answered — and its answer goes as typed,
// newlines included (an API answer, not a terminal's one line).
//
// 🔴 REGRESSION TEST. At 2afa5ab8 one question without "custom" took the box
// off EVERY question of the card: the free-text question could not be
// answered at all.
func TestAMixedFormKeepsEachQuestionsOwnAnswerBox(t *testing.T) {
	m := newOCFormMirror(false)
	f := mixedForm()
	m.custom = &f
	s, ts := viewServer(t, m.fakeMirror)
	s.UseExtensions(Extensions{Mirror: m, Notes: stubNotesStore{}, Agents: mirrorAgents{}, SessionLiveness: stubLiveness{}, Provisioner: newFakeTerminal()})
	_, card := mirrorGet(t, s, ts, asksPath)
	if strings.Contains(card, `name="o0"`) || !strings.Contains(card, `name="o1"`) {
		t.Fatalf("the card's answer boxes: want none on q0, one on q1:\n%s", card)
	}
	resp, body := answerReq(t, s, ts, answerForm(ocAskForm, "a0", "1", "a1", "__other__", "o1", "it builds\nand it ships"), "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("answer = %d:\n%s", resp.StatusCode, body)
	}
	_, picks := m.got()
	want := []mirror.OpenCodePick{{Key: "q0", Picks: []int{1}}, {Key: "q1", Other: "it builds\nand it ships"}}
	if len(picks) != 1 || !reflect.DeepEqual(picks[0], want) {
		t.Fatalf("relayed %+v; want %+v", picks, want)
	}
	// An answer of one's own on the question that takes none: refused, saying so.
	resp, body = answerReq(t, s, ts, answerForm(ocAskForm, "a0", "__other__", "o0", "main-ish", "a1", "__other__", "o1", "x"), "", "")
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "takes only its own options") {
		t.Fatalf("one's own answer where none is taken: %d %q", resp.StatusCode, body)
	}
}

// takenFormMirror is an ocFormMirror whose host, once an answer is relayed,
// has it: opencode no longer holds the form, so a read of it is refused as
// gone — while the window's listing (the hub's copy of the host's last one)
// still says it waits on that ask, until the host's next listing.
type takenFormMirror struct {
	*ocFormMirror
	taken bool
}

func (m *takenFormMirror) OpenCodeForm(ctx context.Context, host, session, window, promptID string) (mirror.OpenCodeForm, error) {
	m.mu.Lock()
	taken := m.taken
	m.mu.Unlock()
	if taken && promptID == ocAskForm {
		m.mu.Lock()
		m.reads = append(m.reads, strings.Join([]string{host, session, window, promptID}, " "))
		m.mu.Unlock()
		return mirror.OpenCodeForm{}, fmt.Errorf("%w: %w (no form)", mirror.ErrAskNotSent, mirror.ErrOpenCodeAskGone)
	}
	if m.custom != nil && promptID == m.custom.PromptID {
		return *m.custom, nil
	}
	return m.ocFormMirror.OpenCodeForm(ctx, host, session, window, promptID)
}

func (m *takenFormMirror) AnswerOpenCodeForm(ctx context.Context, host, session, window string, form mirror.OpenCodeForm, picks []mirror.OpenCodePick) error {
	err := m.ocFormMirror.AnswerOpenCodeForm(ctx, host, session, window, form, picks)
	if err == nil {
		m.mu.Lock()
		m.taken = true
		m.mu.Unlock()
	}
	return err
}

// TestAnAnsweredOpenCodeQuestionStaysAnsweredWhileTheListingCatchesUp: the
// card's submit succeeds (200, "Answered here"), and the region's next poll —
// while the window's listing still names the ask, before the host's next
// listing — keeps saying it was answered here, with the answers. It never
// redraws it as a card saying nothing was answered, and a second submit of the
// same card relays nothing more. Once the listing drops the ask the card goes;
// a NEW ask in the window gets its own card.
//
// 🔴 REGRESSION TEST (the operator's report: "the answer had been submitted,
// but the card thought that it failed"). Before the fix the poll read the
// form again, the host said opencode no longer holds it, and the card became
// "opencode is no longer waiting on that … Nothing was answered here."
func TestAnAnsweredOpenCodeQuestionStaysAnsweredWhileTheListingCatchesUp(t *testing.T) {
	m := &takenFormMirror{ocFormMirror: newOCFormMirror(false)}
	s, ts := viewServer(t, m.fakeMirror)
	s.UseExtensions(Extensions{Mirror: m, Notes: stubNotesStore{}, Agents: mirrorAgents{}, SessionLiveness: stubLiveness{}, Provisioner: newFakeTerminal()})
	submit := answerForm(ocAskForm, "a0", "1", "a1", "1")
	resp, body := answerReq(t, s, ts, submit, "", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Answered here — opencode has it.") {
		t.Fatalf("answer = %d:\n%s", resp.StatusCode, body)
	}
	// The listing still says the window waits on the ask (it is unchanged).
	_, card := mirrorGet(t, s, ts, asksPath)
	for _, want := range []string{`data-ask-tool="` + ocAskForm + `"`, `data-ask-state="answered"`, "Answered here — opencode has it.", "SQLite", "metrics"} {
		if !strings.Contains(card, want) {
			t.Errorf("the poll after a successful answer lacks %q:\n%s", want, card)
		}
	}
	for _, bad := range []string{html_(openCodeCardGone), "Nothing was answered here", "data-ask-submit"} {
		if strings.Contains(card, bad) {
			t.Errorf("the poll after a successful answer shows %q:\n%s", bad, card)
		}
	}
	// The same card submitted again: answered, and nothing more relayed.
	resp, body = answerReq(t, s, ts, submit, "", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Answered here — opencode has it.") {
		t.Errorf("a second submit = %d:\n%s", resp.StatusCode, body)
	}
	if calls, _ := m.got(); len(calls) != 1 {
		t.Errorf("relayed %d answers, want 1: %v", len(calls), calls)
	}
	// The host's next listing: a new ask in the window. Its own card, a form.
	const next = "frm_1a2aaa3d1001lEte0g1lyG1pFi"
	m.fakeMirror.mu.Lock()
	m.fakeMirror.windows[0].Pending = &mirror.Pending{Kind: mirror.PendingQuestion, Tool: "question", Summary: "Another?", PromptID: next}
	m.fakeMirror.mu.Unlock()
	nf := m.form()
	nf.PromptID = next
	m.custom = &nf
	_, card = mirrorGet(t, s, ts, asksPath)
	if strings.Contains(card, `data-ask-tool="`+ocAskForm+`"`) || !strings.Contains(card, `data-ask-tool="`+next+`"`) ||
		!strings.Contains(card, `data-ask-state="pending"`) || !strings.Contains(card, "data-ask-submit") {
		t.Errorf("after the listing moved on, want only the new ask's form:\n%s", card)
	}
}
