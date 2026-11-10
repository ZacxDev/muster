package api

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"

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
	perm    bool
	// custom, when set, is the form the host sends in place of form().
	custom *mirror.OpenCodeForm
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
