package api

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
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
	if !strings.Contains(page, `<option value="">Nowhere</option><option value="41">Plan the acme launch</option></select>`) {
		t.Errorf("the task picker is not [Nowhere, Plan the acme launch]: %s", page[strings.Index(page, `name="param_task"`):][:200])
	}
}

// TestAMemberGetsAskYourAdminOnFlows: a member's /flows says to ask their
// admin where the setup was, draws no setup form and no ⓘ at all (absent, not
// hidden), never reads the runner list — and still shows their agents'
// requests to decide. The setup writes refuse a member before the store; the
// admin's same writes reach it (the control).
func TestAMemberGetsAskYourAdminOnFlows(t *testing.T) {
	f := newFlowsPage(t)
	rec := f.do("GET", "/flows", nil, flowsPlainMember)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /flows as a member: %d", rec.Code)
	}
	page := flowsSection(t, rec.Body.String())
	if !strings.Contains(page, `<p class="text-sm text-fg" data-flows-ask-admin>Ask your admin to set up browser tasks.</p>`) {
		t.Errorf("the member's page does not say to ask their admin:\n%s", page)
	}
	for _, absent := range []string{`data-testid="flow-enqueue"`, `data-testid="runner-token-create"`, `data-testid="runner-tokens"`,
		`data-info-sheet`, `data-flows-intro`, "runner", "Connect a browser", "Run task"} {
		if strings.Contains(page, absent) {
			t.Errorf("the member's page carries %q", absent)
		}
	}
	if !strings.Contains(page, `data-flow-request-id="31"`) || !strings.Contains(page, `/flows/requests/31/approve`) {
		t.Error("the member's page does not show their agent's pending request to decide")
	}
	for _, c := range f.store.snapshot() {
		if c == "ListRunnerTokens" {
			t.Error("the member's page read the runner list")
		}
	}

	writes := []struct {
		path      string
		form      url.Values
		wantStore string
		adminCode int
	}{
		{"/flows/jobs", url.Values{"flow": {"muse.poll"}, "host": {"atlas"}, "instance": {"personal"}}, "Enqueue", http.StatusSeeOther},
		{"/flows/runner-tokens", url.Values{"host": {"atlas"}}, "CreateRunnerToken", http.StatusOK},
		{"/flows/runner-tokens/5/revoke", url.Values{}, "RevokeRunnerToken", http.StatusSeeOther},
	}
	for _, w := range writes {
		f.store.reset()
		if rec := f.do("POST", w.path, w.form, flowsPlainMember); rec.Code != http.StatusForbidden {
			t.Errorf("a member's POST %s: %d, want 403", w.path, rec.Code)
		}
		for _, c := range f.store.snapshot() {
			if c == w.wantStore {
				t.Errorf("a member's POST %s reached %s", w.path, c)
			}
		}
		f.store.reset()
		if rec := f.do("POST", w.path, w.form, authz.OperatorUserID); rec.Code != w.adminCode {
			t.Errorf("CONTROL: the admin's POST %s: %d, want %d", w.path, rec.Code, w.adminCode)
		}
		reached := false
		for _, c := range f.store.snapshot() {
			reached = reached || c == w.wantStore
		}
		if !reached {
			t.Errorf("CONTROL: the admin's POST %s never reached %s", w.path, w.wantStore)
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

// TestFlowProblemCoversEveryCatalogueProblem: every Problem the catalogue
// names has its own plain sentence, never the generic fallback, so a new
// Problem added without words shows up here.
func TestFlowProblemCoversEveryCatalogueProblem(t *testing.T) {
	const generic = "That could not be done. Check what you entered and try again."
	problems := []flows.Problem{flows.ProblemFlow, flows.ProblemHost, flows.ProblemInstance, flows.ProblemTTL, flows.ProblemMissing,
		flows.ProblemValue, flows.ProblemThreadAndSession, flows.ProblemMarkerNeedsSession, flows.ProblemTask}
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
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) != len(problems) {
		t.Errorf("%d sentences for %d problems", len(keys), len(problems))
	}
}
