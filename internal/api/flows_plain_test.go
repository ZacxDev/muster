package api

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/authz"
	"github.com/ZacxDev/muster/internal/flows"
	"github.com/ZacxDev/muster/internal/notes"
	"github.com/ZacxDev/muster/internal/plainscan"
)

// The /flows page's plain top layer and its admin gate (the operator rule:
// setup is an admin's; a member is told to ask their admin). Every expected
// string here is a literal, never the constant the page draws, so a reworded
// page fails these tests until they are reworded too.

// plainFlows is a flows.Store for the page: fixed lists, real validation on
// the two writes the page's forms make, and a record of every call.
type plainFlows struct {
	flows.Store
	mu    sync.Mutex
	calls []string
}

func (p *plainFlows) note(m string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, m)
}

func (p *plainFlows) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

func (p *plainFlows) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = nil
}

func (p *plainFlows) List(context.Context, int) ([]flows.Job, error) {
	p.note("List")
	return nil, nil
}

func (p *plainFlows) ListRequests(context.Context, int) ([]flows.FlowRequest, error) {
	p.note("ListRequests")
	return []flows.FlowRequest{{ID: 31, AgentName: "acme-helper", Flow: "muse.poll", Host: "atlas", Instance: "personal",
		Params: map[string]string{"thread": "Weekly plan"}, Status: flows.RequestPending, ExpiresAt: time.Now().Add(time.Hour)}}, nil
}

func (p *plainFlows) ListRunnerTokens(context.Context) ([]flows.RunnerToken, error) {
	p.note("ListRunnerTokens")
	return []flows.RunnerToken{{ID: 5, Host: "atlas", CreatedAt: time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)}}, nil
}

func (p *plainFlows) CreateRunnerToken(_ context.Context, host string) (string, flows.RunnerToken, error) {
	p.note("CreateRunnerToken")
	if err := flows.ValidHost(host); err != nil {
		return "", flows.RunnerToken{}, err
	}
	return flows.RunnerTokenPrefix + strings.Repeat("C", 43), flows.RunnerToken{ID: 6, Host: host}, nil
}

func (p *plainFlows) RevokeRunnerToken(context.Context, int64) (bool, error) {
	p.note("RevokeRunnerToken")
	return true, nil
}

// Cancel always meets a running job: the refusal that carries detail
// (FlowNotCancellable) on the page.
func (p *plainFlows) Cancel(context.Context, int64) (flows.Job, error) {
	p.note("Cancel")
	return flows.Job{}, flows.ErrState
}

func (p *plainFlows) Enqueue(_ context.Context, r flows.Request, _, _ string) (flows.Job, error) {
	p.note("Enqueue")
	if _, err := flows.Validate(&r); err != nil {
		return flows.Job{}, err
	}
	return flows.Job{ID: 1}, nil
}

// pickerNotes serves the "Post the result on" picker's tasks.
type pickerNotes struct{ notes.Store }

func (pickerNotes) ListSummaries(context.Context) ([]notes.Note, error) {
	return []notes.Note{
		{ID: 41, Title: "Plan the acme launch", Status: notes.StatusOpen},
		{ID: 43, Title: `Fix <b>&"quotes"`, Status: notes.StatusInProgress},
		{ID: 42, Title: "Shipped already", Status: notes.StatusComplete},
	}, nil
}

const flowsPlainMember int64 = 7

type flowsPage struct {
	t     *testing.T
	store *plainFlows
	h     http.Handler
	s     *Server
}

func newFlowsPage(t *testing.T) flowsPage {
	t.Helper()
	store := &plainFlows{}
	s := New(nil, AuthConfig{HookToken: tokTestHook, UIPassword: "flows-plain-operator-password"}, log.New(io.Discard, "", 0))
	s.UseExtensions(Extensions{Users: stubUsers{}, Flows: store, Notes: pickerNotes{}})
	const userHeader = "X-Flows-Plain-User"
	s.sessionUserOverride = func(r *http.Request) int64 {
		v, _ := strconv.ParseInt(r.Header.Get(userHeader), 10, 64)
		return v
	}
	mux := http.NewServeMux()
	s.registerAll(mux)
	return flowsPage{t: t, store: store, h: mux, s: s}
}

// do sends a document navigation as uid's browser session.
func (f flowsPage) do(method, path string, form url.Values, uid int64) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	r.Host = "muster.test"
	r.Header.Set("Cookie", sessionCookieFor(f.s))
	r.Header.Set("Origin", "http://muster.test")
	r.Header.Set("X-Flows-Plain-User", strconv.FormatInt(uid, 10))
	r.Header.Set("Sec-Fetch-Mode", "navigate")
	r.Header.Set("Sec-Fetch-Dest", "document")
	if method == http.MethodPost {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, r)
	return rec
}

// flowsSection is the page's own section (the shell's chrome is not this
// page's to pin).
func flowsSection(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, `data-testid="flows-page"`)
	if i < 0 {
		t.Fatalf("no flows page in the response: %.400s", body)
	}
	start := strings.LastIndex(body[:i], "<section")
	end := strings.Index(body[start:], "</section>")
	if end < 0 {
		t.Fatal("the flows page section is not closed")
	}
	return body[start : start+end+len("</section>")]
}

func topLayer(t *testing.T, markup string) []string {
	t.Helper()
	texts, err := plainscan.TopLayerTexts(markup)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range texts {
		if s = strings.Join(strings.Fields(s), " "); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// withoutRequestCards drops the agent-request cards: a card draws the exact
// flow and parameters an approval runs (flowRequestCardHTML), which is the
// control it exists for, so its words are not the page's chrome.
func withoutRequestCards(page string) string {
	for {
		i := strings.Index(page, `<li class="`+flowsCardClass+`" data-flow-request-id=`)
		if i < 0 {
			return page
		}
		j := strings.Index(page[i:], "</li>")
		page = page[:i] + page[i+j+len("</li>"):]
	}
}

func labelsOf(markup string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`(?s)<label[^>]*>(.*?)</label>`).FindAllStringSubmatch(markup, -1) {
		out = append(out, strings.Join(strings.Fields(m[1]), " "))
	}
	return out
}

// TestFlowsPageTopLayerIsPlainForAnAdmin (F071–F077): the admin's page leads
// with one plain sentence, names every field by its plain label, and keeps
// the queueing rules, the raw parameter descriptions and the runner-token
// detail behind ⓘ sheets — present in the page, absent from its top layer.
func TestFlowsPageTopLayerIsPlainForAnAdmin(t *testing.T) {
	f := newFlowsPage(t)
	rec := f.do("GET", "/flows", nil, authz.OperatorUserID)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /flows as the admin: %d", rec.Code)
	}
	page := flowsSection(t, rec.Body.String())
	if !strings.Contains(page, `data-flow-request-id="31"`) {
		t.Fatal("CONTROL: the fixture's pending request is not on the page")
	}
	top := topLayer(t, withoutRequestCards(page))
	joined := strings.Join(top, "\n")

	for _, want := range []string{
		"Browser tasks",
		"Run a saved browser task on one of your computers.",
		"Read Muse's latest reply",
		"Send a message to Muse",
		"Advanced",
		"Run task",
		"Connected browsers",
		"Connect a computer's browser so it can run these tasks.",
		"Connect a browser",
	} {
		found := false
		for _, s := range top {
			found = found || s == want
		}
		if !found {
			t.Errorf("the admin's top layer has no %q; it reads:\n%s", want, joined)
		}
	}
	wantLabels := []string{"Task", "Computer", "Browser", "Message", "Post the result on", "Time limit (minutes)",
		"Side chat", "Open a specific chat", "Completion code", "How long to wait for a reply (seconds)", "Computer name"}
	if got := labelsOf(page); strings.Join(got, "|") != strings.Join(wantLabels, "|") {
		t.Errorf("the form's labels are\n  %q\nwant\n  %q", got, wantLabels)
	}
	// The old top-layer words are gone from it (F071–F077)…
	for _, old := range []string{
		"Queue a named flow for one of your hosts' browsers.",
		"Muse thread id (uuid)",
		"dispatch id; the send completes only when",
		"seconds to await the reply",
		"muster task id the flow posts its result on",
		"Browser instance",
		"A runner token is for ONE host.",
		"Create runner token",
		"Queue flow",
		"muse.poll", "muse.send",
	} {
		if strings.Contains(joined, old) {
			t.Errorf("the admin's top layer still says %q", old)
		}
	}
	// …and every line of it passes the shared plain scan (the runner list and
	// the task picker included). The scan's control: it flags the old runner
	// sentence, so a clean pass is a fact about the page.
	if plainscan.Jargon("A runner token is for ONE host. It can claim and report that host's jobs and nothing else.") == "" {
		t.Fatal("CONTROL: the plain scan does not flag the old runner-token sentence")
	}
	for _, s := range top {
		if s == `Fix <b>&"quotes"` {
			continue // a task's own title is the user's words, not the page's (its escaping is pinned below)
		}
		if j := plainscan.Jargon(s); j != "" {
			t.Errorf("jargon %q in the admin's top layer: %q", j, s)
		}
	}
	// …while the detail moved behind the ⓘ, not out of the page.
	for _, detail := range []string{
		"Queue a named flow for one of your hosts&#39; browsers.",
		"Open a specific chat (session): Muse thread id (uuid) — opens that thread directly; exclusive with thread",
		"Completion code (marker): dispatch id; the send completes only when the reply&#39;s last line is DONE &lt;marker&gt;",
		"How long to wait for a reply (seconds) (wait): seconds to await the reply (1..3600; the runner&#39;s job timeout is wait+240s)",
		"A runner token (mfr_…) is for ONE host. It can claim and report that host&#39;s jobs and nothing else.",
	} {
		if !strings.Contains(page, detail) {
			t.Errorf("the ⓘ detail %q is missing from the page", detail)
		}
	}
	// The picker offers the admin's unfinished tasks, by title, and no
	// finished one.
	// The markup-shaped title is escaped, never drawn as markup.
	if !strings.Contains(page, `<option value="">Nowhere</option><option value="41">Plan the acme launch</option>`+
		`<option value="43">Fix &lt;b&gt;&amp;&#34;quotes&#34;</option></select>`) {
		t.Errorf("the task picker is not [Nowhere, Plan the acme launch, Fix <b>&\"quotes\"] escaped: %s",
			page[strings.Index(page, `name="param_task"`):][:300])
	}
}

// TestAMemberGetsAskYourAdminOnFlows: a member's /flows says to ask their
// admin where the hand-run form was, and draws no form, no intro and none of
// the form's ⓘs (absent, not hidden). It still shows their agents' requests to
// decide, and their own connected browsers with Connect, Disconnect and that
// section's ⓘ (operator decision 2026-11-18: connecting your own browser is
// personal setup). The hand-run write refuses a member before the store; the
// connect and disconnect writes reach it. The admin reaches all three (the
// control).
func TestAMemberGetsAskYourAdminOnFlows(t *testing.T) {
	f := newFlowsPage(t)
	rec := f.do("GET", "/flows", nil, flowsPlainMember)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /flows as a member: %d", rec.Code)
	}
	page := flowsSection(t, rec.Body.String())
	if !strings.Contains(page, `<p class="text-sm text-fg" data-flows-ask-admin>Ask your admin to run a browser task by hand.</p>`) {
		t.Errorf("the member's page does not say to ask their admin:\n%s", page)
	}
	for _, absent := range []string{`data-testid="flow-enqueue"`, `data-flows-intro`, "Run task", `id="flows-about"`, `id="flows-fields"`,
		`data-flow-advanced`} {
		if strings.Contains(page, absent) {
			t.Errorf("the member's page carries %q", absent)
		}
	}
	for _, present := range []string{`data-testid="runner-token-create"`, `data-testid="runner-tokens"`, `data-runner-token-id="5"`,
		`/flows/runner-tokens/5/revoke`, `id="flows-runner"`, ">Connected browsers</h2>", ">Computer name</label>", ">Connect a browser</button>",
		">Disconnect</button>", "A runner token (mfr_…) is for ONE host."} {
		if !strings.Contains(page, present) {
			t.Errorf("the member's page lacks %q", present)
		}
	}
	if got := labelsOf(page); strings.Join(got, "|") != "Computer name" {
		t.Errorf("the member's form labels are %q, want only [Computer name]", got)
	}
	if !strings.Contains(page, `data-flow-request-id="31"`) || !strings.Contains(page, `/flows/requests/31/approve`) {
		t.Error("the member's page does not show their agent's pending request to decide")
	}

	writes := []struct {
		path       string
		form       url.Values
		wantStore  string
		memberCode int // 403 = refused before the store
		adminCode  int
	}{
		{"/flows/jobs", url.Values{"flow": {"muse.poll"}, "host": {"atlas"}, "instance": {"personal"}}, "Enqueue", http.StatusForbidden, http.StatusSeeOther},
		{"/flows/runner-tokens", url.Values{"host": {"atlas"}}, "CreateRunnerToken", http.StatusOK, http.StatusOK},
		{"/flows/runner-tokens/5/revoke", url.Values{}, "RevokeRunnerToken", http.StatusSeeOther, http.StatusSeeOther},
	}
	reached := func(want string) bool {
		for _, c := range f.store.snapshot() {
			if c == want {
				return true
			}
		}
		return false
	}
	for _, w := range writes {
		f.store.reset()
		rec := f.do("POST", w.path, w.form, flowsPlainMember)
		if rec.Code != w.memberCode {
			t.Errorf("a member's POST %s: %d, want %d", w.path, rec.Code, w.memberCode)
		}
		if got := reached(w.wantStore); got != (w.memberCode != http.StatusForbidden) {
			t.Errorf("a member's POST %s reached %s: %v", w.path, w.wantStore, got)
		}
		if w.path == "/flows/runner-tokens" && !strings.Contains(rec.Body.String(), `id="minted-runner-token" readonly value="mfr_`) {
			t.Error("a member's new connection code is not shown")
		}
		f.store.reset()
		if rec := f.do("POST", w.path, w.form, authz.OperatorUserID); rec.Code != w.adminCode {
			t.Errorf("CONTROL: the admin's POST %s: %d, want %d", w.path, rec.Code, w.adminCode)
		}
		if !reached(w.wantStore) {
			t.Errorf("CONTROL: the admin's POST %s never reached %s", w.path, w.wantStore)
		}
	}
}

// TestAMembersRefusalCarriesNoRawDetail: a refusal with technical detail (a
// cancel of a running job: FlowNotCancellable) reads plainly to a member, with
// no ⓘ and none of the detail in the page; the admin's same refusal carries
// the ⓘ with it (the control).
func TestAMembersRefusalCarriesNoRawDetail(t *testing.T) {
	f := newFlowsPage(t)
	const plain = "That task is already running or finished, so it cannot be cancelled."
	const raw = "a running job is the runner&#39;s until it reports"
	alert := regexp.MustCompile(`<p role="alert"[^>]*>([^<]*)</p>`)
	for _, c := range []struct {
		who       string
		uid       int64
		wantSheet bool
	}{{"member", flowsPlainMember, false}, {"admin", authz.OperatorUserID, true}} {
		rec := f.do("POST", "/flows/jobs/9/cancel", url.Values{}, c.uid)
		if rec.Code != http.StatusConflict {
			t.Fatalf("%s's cancel of a running job: %d, want 409", c.who, rec.Code)
		}
		page := flowsSection(t, rec.Body.String())
		if m := alert.FindStringSubmatch(page); m == nil || m[1] != plain {
			t.Errorf("%s's alert reads %q, want %q", c.who, m, plain)
		}
		if got := strings.Contains(page, `id="flows-problem"`); got != c.wantSheet {
			t.Errorf("%s's page has the refusal ⓘ: %v, want %v", c.who, got, c.wantSheet)
		}
		if got := strings.Contains(page, raw); got != c.wantSheet {
			t.Errorf("%s's page carries the raw detail: %v, want %v", c.who, got, c.wantSheet)
		}
	}
}

// TestFlowsPageSaysARefusalPlainly (F078 and the enqueue form's refusals):
// the page leads with a plain sentence; the machine-facing message, which the
// API answers unchanged, is only in the ⓘ beside it.
func TestFlowsPageSaysARefusalPlainly(t *testing.T) {
	f := newFlowsPage(t)
	cases := []struct {
		name, path string
		form       url.Values
		plain, raw string
	}{
		{"no computer name", "/flows/runner-tokens", url.Values{"host": {""}},
			"Enter the computer's name first.",
			`invalid flow request: host "" is not a host label (lowercase letters, digits and '-')`},
		{"a computer name with capitals", "/flows/runner-tokens", url.Values{"host": {"Atlas"}},
			"A computer's name can use only lowercase letters, numbers and dashes.",
			`invalid flow request: host "Atlas" is not a host label (lowercase letters, digits and '-')`},
		{"no message", "/flows/jobs", url.Values{"flow": {"muse.send"}, "host": {"atlas"}, "instance": {"personal"}},
			"Fill in “Message” first.",
			`invalid flow request: flow muse.send needs parameter "text"`},
		{"side chat and a specific chat", "/flows/jobs", url.Values{"flow": {"muse.send"}, "host": {"atlas"}, "instance": {"personal"},
			"param_text": {"hello"}, "param_thread": {"Weekly plan"}, "param_session": {"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"}},
			"Use “Side chat” or “Open a specific chat”, not both.",
			`invalid flow request: parameters "session" and "thread" are exclusive; name the thread by one of them`},
		{"a wait over the limit", "/flows/jobs", url.Values{"flow": {"muse.send"}, "host": {"atlas"}, "instance": {"personal"},
			"param_text": {"hello"}, "param_session": {"0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"}, "param_wait": {"3601"}},
			"“How long to wait for a reply (seconds)” has a value this task does not accept.",
			`invalid flow request: parameter "wait" is 3601; the limit is 3600`},
		{"a time limit over a day", "/flows/jobs", url.Values{"flow": {"muse.poll"}, "host": {"atlas"}, "instance": {"personal"},
			"ttl_minutes": {"1441"}},
			"The time limit must be between 1 and 1440 minutes.",
			`invalid flow request: ttl 24h1m0s is outside 1m0s..24h0m0s`},
	}
	alert := regexp.MustCompile(`<p role="alert"[^>]*>([^<]*)</p>`)
	for _, c := range cases {
		rec := f.do("POST", c.path, c.form, authz.OperatorUserID)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", c.name, rec.Code)
			continue
		}
		page := flowsSection(t, rec.Body.String())
		m := alert.FindStringSubmatch(page)
		if m == nil || strings.ReplaceAll(m[1], "&#39;", "'") != c.plain {
			t.Errorf("%s: the alert reads %q, want %q", c.name, m, c.plain)
		}
		joined := strings.Join(topLayer(t, withoutRequestCards(page)), "\n")
		if strings.Contains(joined, "invalid flow request") || strings.Contains(joined, "host label") || strings.Contains(joined, "parameter") {
			t.Errorf("%s: the raw message is in the top layer:\n%s", c.name, joined)
		}
		at := strings.Index(page, `id="flows-problem"`)
		if at < 0 {
			t.Errorf("%s: no ⓘ beside the problem", c.name)
			continue
		}
		sheet := page[at:]
		if raw := strings.NewReplacer("&#34;", `"`, "&#39;", "'").Replace(sheet); !strings.Contains(raw, c.raw) {
			t.Errorf("%s: the ⓘ does not carry the exact message %q", c.name, c.raw)
		}
	}
}

// TestFlowProblemCoversEveryCatalogueProblem: every Problem constant
// internal/flows declares (read from its source, so a new one is in the list
// the day it is added) has its own plain, distinct sentence — never the
// generic fallback.
func TestFlowProblemCoversEveryCatalogueProblem(t *testing.T) {
	const generic = "That could not be done. Check what you entered and try again."
	src, err := os.ReadFile(filepath.Join("..", "flows", "catalogue.go"))
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "catalogue.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var problems []flows.Problem
	for _, d := range file.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, spec := range g.Specs {
			vs := spec.(*ast.ValueSpec)
			if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Problem" {
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok {
					t.Fatalf("a Problem constant is not a string literal: %T", v)
				}
				val, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				problems = append(problems, flows.Problem(val))
			}
		}
	}
	// The enumeration's control: it found the ones this test knows by name.
	if len(problems) < 9 || !slices.Contains(problems, flows.ProblemHost) || !slices.Contains(problems, flows.ProblemTask) {
		t.Fatalf("the source scan found %v", problems)
	}
	seen := map[string]flows.Problem{}
	for _, p := range problems {
		e := flows.Invalid(p, "x")
		e.Param = "text"
		v := flowProblem(e)
		if v.problem == generic || v.problem == "" {
			t.Errorf("Problem %q has no plain sentence", p)
		}
		if prev, dup := seen[v.problem]; dup {
			t.Errorf("Problems %q and %q say the same thing: %q", prev, p, v.problem)
		}
		seen[v.problem] = p
		if j := plainscan.Jargon(v.problem); j != "" {
			t.Errorf("Problem %q's sentence carries jargon %q: %q", p, j, v.problem)
		}
	}
	// The control: an error with no Problem gets the generic sentence.
	if v := flowProblem(flows.ErrInvalid); v.problem != generic {
		t.Errorf("CONTROL: a bare ErrInvalid reads %q", v.problem)
	}
}
