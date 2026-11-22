package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirrorhost"
	"github.com/spf13/pflag"
)

// `officer item enqueue` inside a mirrored Claude Code session goes through
// that session's own muster-mirror MCP door (officer_enqueue_session.go). The
// door is played by fakeDoor, the muster by the harness.

const (
	doorSecret = "door-secret-never-printed"
	doorPane   = "%17"
	doorTmux   = "/tmp/tmux-1000/default,4242,0"
	doorSID    = "5e55a0e0-0000-4000-8000-0000000000c3"
	enqRoute   = "POST /api/officers/defs/marketing:example/items"
)

type doorCall struct {
	path    string
	headers http.Header
	body    string
}

// fakeDoor is a muster-mirror MCP endpoint answering every call with answer
// (a tool result's text; isErr marks it a tool error).
type fakeDoor struct {
	srv    *httptest.Server
	mu     sync.Mutex
	calls  []doorCall
	answer string
	isErr  bool
}

func newFakeDoor(t *testing.T, answer string, isErr bool) *fakeDoor {
	t.Helper()
	d := &fakeDoor{answer: answer, isErr: isErr}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		d.calls = append(d.calls, doorCall{path: r.Method + " " + r.URL.Path, headers: r.Header.Clone(), body: string(b)})
		d.mu.Unlock()
		res, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{"content": []map[string]any{{"type": "text", "text": d.answer}}, "isError": d.isErr}})
		writeJSON(w, http.StatusOK, string(res))
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *fakeDoor) taken() []doorCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]doorCall(nil), d.calls...)
}

func (d *fakeDoor) listen() string { return strings.TrimPrefix(d.srv.URL, "http://") }

// sessionEnv is a Claude Code session's environment in a tmux pane.
func sessionEnv() map[string]string {
	return map[string]string{"CLAUDE_CODE_SESSION_ID": doorSID, "TMUX_PANE": doorPane, "TMUX": doorTmux, envMirrorMCPSecret: doorSecret}
}

// runEnqueue runs `officer item enqueue` against h with env, in a home whose
// muster-mirror settings file holds mirrorEnv ("" = no file).
func runEnqueue(t *testing.T, h *harness, env map[string]string, mirrorEnv string, extra ...string) result {
	t.Helper()
	home := t.TempDir()
	if mirrorEnv != "" {
		p := filepath.Join(home, ".config", "muster-mirror", "env")
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(mirrorEnv), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	args := append([]string{"--api-url", h.srv.URL, "--token", testToken, "--env-file", filepath.Join(t.TempDir(), "absent.env"),
		"officer", "item", "enqueue", "marketing:example", "--kind", "owner.plan", "--subject", "plan-cli",
		"--payload", `{"goal":"launch"}`}, extra...)
	var out, errOut strings.Builder
	getenv := func(k string) string { return env[k] }
	code := run(context.Background(), testProgName, args, strings.NewReader(""), &out, &errOut, getenv, home)
	return result{code: code, stdout: out.String(), stderr: errOut.String()}
}

func mirrorSettings(d *fakeDoor, musterURL string) string {
	return envMirrorMCPListen + "=" + d.listen() + "\n" + envMirrorURL + "=" + musterURL + "\n"
}

// adminEnqueues counts the admin API's enqueue requests h took.
func adminEnqueues(h *harness) int {
	n := 0
	for _, r := range h.nonHealthRequests() {
		if r.method+" "+strings.SplitN(r.uri, "?", 2)[0] == enqRoute {
			n++
		}
	}
	return n
}

// TestEnqueueInsideASessionGoesThroughItsMirrorDoor: the command makes the
// call the session's MCP client makes — officer_item_enqueue on this machine's
// muster-mirror endpoint, with its secret, pane and tmux server — and names no
// session itself; the admin API is not called.
func TestEnqueueInsideASessionGoesThroughItsMirrorDoor(t *testing.T) {
	h := newHarness(t)
	h.json(enqRoute, http.StatusCreated, `{"id":5,"created":true}`)
	d := newFakeDoor(t, `{"id":42,"created":true,"requested_by_this_session":true}`, false)
	got := runEnqueue(t, h, sessionEnv(), mirrorSettings(d, h.srv.URL), "--priority", "high", "--not-before", "2030-01-02T03:04:05Z")
	if got.code != exitOK {
		t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
	}
	calls := d.taken()
	if len(calls) != 1 {
		t.Fatalf("the door took %d calls; want 1", len(calls))
	}
	c := calls[0]
	if c.path != "POST /mcp" || c.headers.Get("Authorization") != "Bearer "+doorSecret || c.headers.Get("X-Muster-Pane") != doorPane ||
		c.headers.Get("X-Muster-Tmux") != doorTmux {
		t.Fatalf("the door call: %s %v", c.path, c.headers)
	}
	var rpc struct {
		Method string `json:"method"`
		Params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(c.body), &rpc); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(rpc.Params.Arguments))
	for k := range rpc.Params.Arguments {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if rpc.Method != "tools/call" || rpc.Params.Name != "officer_item_enqueue" ||
		strings.Join(keys, ",") != "expect_session,kind,not_before,officer,payload,priority,subject_key" {
		t.Fatalf("the call: %s %s %v", rpc.Method, rpc.Params.Name, keys)
	}
	a := rpc.Params.Arguments
	if a["officer"] != "marketing:example" || a["kind"] != "owner.plan" || a["subject_key"] != "plan-cli" || a["priority"] != "high" ||
		a["not_before"] != "2030-01-02T03:04:05Z" || a["expect_session"] != doorSID {
		t.Fatalf("the arguments: %v", a)
	}
	if p, _ := a["payload"].(map[string]any); p["goal"] != "launch" {
		t.Fatalf("the payload: %v", a["payload"])
	}
	// The session id is in the body ONCE, as expect_session (which can only
	// refuse a call bound elsewhere); the pane and the secret not at all.
	if n := strings.Count(c.body, doorSID); n != 1 {
		t.Fatalf("the call's body names the session %d times; want once, as expect_session: %s", n, c.body)
	}
	for _, s := range []string{doorPane, doorSecret} {
		if strings.Contains(c.body, s) {
			t.Fatalf("the call's body names %q; the session is muster-mirror's to bind: %s", s, c.body)
		}
	}
	if n := adminEnqueues(h); n != 0 {
		t.Fatalf("the admin API was called %d times as well", n)
	}
	if !strings.Contains(got.stdout, `"requested_by_this_session": true`) && !strings.Contains(got.stdout, `"requested_by_this_session":true`) {
		t.Fatalf("stdout: %q", got.stdout)
	}
	if got.stderr != "" {
		t.Fatalf("stderr: %q", got.stderr)
	}
}

// TestEnqueueWithNoSessionUsesTheAdminAPIAndSaysWhy: whenever the session
// cannot be resolved, the item is enqueued as before, no session is recorded,
// and stderr says why — and the door is never called with the secret.
func TestEnqueueWithNoSessionUsesTheAdminAPIAndSaysWhy(t *testing.T) {
	without := func(k string) map[string]string { e := sessionEnv(); delete(e, k); return e }
	cases := []struct {
		name   string
		env    map[string]string
		mirror func(d *fakeDoor, url string) string
		why    string
	}{
		{"not a Claude Code session", without("CLAUDE_CODE_SESSION_ID"), mirrorSettings, "not running inside a Claude Code session"},
		{"no tmux pane", without("TMUX_PANE"), mirrorSettings, "not running inside a tmux pane"},
		{"no muster-mirror settings", sessionEnv(), func(*fakeDoor, string) string { return "" }, "no muster-mirror settings"},
		{"no MCP endpoint", sessionEnv(), func(_ *fakeDoor, url string) string { return envMirrorURL + "=" + url + "\n" }, "serves no MCP endpoint"},
		{"not loopback", sessionEnv(), func(_ *fakeDoor, url string) string {
			return envMirrorMCPListen + "=192.0.2.7:47823\n" + envMirrorURL + "=" + url + "\n"
		}, "is not a loopback address"},
		{"a name, not a loopback literal", sessionEnv(), func(_ *fakeDoor, url string) string {
			return envMirrorMCPListen + "=localhost:47823\n" + envMirrorURL + "=" + url + "\n"
		}, "MUSTER_MIRROR_MCP_LISTEN=localhost:47823 is not a loopback address"},
		{"mirrored to another muster", sessionEnv(), func(d *fakeDoor, _ string) string {
			return mirrorSettings(d, "https://other-muster.example")
		}, "mirrored to https://other-muster.example, and this command talks to"},
		{"no secret", without(envMirrorMCPSecret), func(d *fakeDoor, url string) string {
			return mirrorSettings(d, url) + envMirrorMCPSecretFile + "=/nonexistent/mcp-secret\n"
		}, "MCP secret could not be read (/nonexistent/mcp-secret)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.json(enqRoute, http.StatusCreated, `{"id":5,"created":true}`)
			d := newFakeDoor(t, `{"id":42,"created":true,"requested_by_this_session":true}`, false)
			got := runEnqueue(t, h, c.env, c.mirror(d, h.srv.URL))
			if got.code != exitOK {
				t.Fatalf("exit = %d, stderr=%q", got.code, got.stderr)
			}
			if n := len(d.taken()); n != 0 {
				t.Fatalf("the door took %d calls", n)
			}
			if n := adminEnqueues(h); n != 1 {
				t.Fatalf("the admin API took %d enqueues; want 1", n)
			}
			if !strings.HasPrefix(got.stderr, "note: no session recorded on this item — ") || !strings.Contains(got.stderr, c.why) {
				t.Fatalf("stderr %q does not say why (%q)", got.stderr, c.why)
			}
		})
	}
}

// TestEnqueueTheDoorRefusesOrMissesFallsBack: a tool error from the door (here:
// the pane runs no session muster-mirror can bind), or no door listening, is
// the admin API's enqueue, the reason on stderr.
func TestEnqueueTheDoorRefusesOrMissesFallsBack(t *testing.T) {
	t.Run("refused", func(t *testing.T) {
		h := newHarness(t)
		h.json(enqRoute, http.StatusCreated, `{"id":5,"created":true}`)
		const refusal = "officer_item_enqueue is available only to your owner's own mirrored Claude Code sessions"
		d := newFakeDoor(t, refusal, true)
		got := runEnqueue(t, h, sessionEnv(), mirrorSettings(d, h.srv.URL))
		if got.code != exitOK || len(d.taken()) != 1 || adminEnqueues(h) != 1 || !strings.Contains(got.stderr, "refused it from this session: "+refusal) {
			t.Fatalf("exit %d, door %d, admin %d, stderr %q", got.code, len(d.taken()), adminEnqueues(h), got.stderr)
		}
	})
	t.Run("not listening", func(t *testing.T) {
		h := newHarness(t)
		h.json(enqRoute, http.StatusCreated, `{"id":5,"created":true}`)
		d := newFakeDoor(t, "", false)
		d.srv.Close()
		got := runEnqueue(t, h, sessionEnv(), mirrorSettings(d, h.srv.URL))
		if got.code != exitOK || adminEnqueues(h) != 1 || !strings.Contains(got.stderr, "MCP endpoint did not answer") {
			t.Fatalf("exit %d, admin %d, stderr %q", got.code, adminEnqueues(h), got.stderr)
		}
	})
}

// TestEnqueueAStepUpCardIsNotRetriedElsewhere: on a machine the step-up gate
// covers, the door answers with a card on the owner's phone. That is the
// answer — the command never enqueues around it through the admin API — and,
// nothing being queued yet, it exits 13, not 0, the card still on stdout.
func TestEnqueueAStepUpCardIsNotRetriedElsewhere(t *testing.T) {
	h := newHarness(t)
	h.json(enqRoute, http.StatusCreated, `{"id":5,"created":true}`)
	d := newFakeDoor(t, `{"status":"awaiting_owner_confirmation","card":91}`, false)
	got := runEnqueue(t, h, sessionEnv(), mirrorSettings(d, h.srv.URL))
	if got.code != 13 || adminEnqueues(h) != 0 || !strings.Contains(got.stdout, `"card": 91`) && !strings.Contains(got.stdout, `"card":91`) ||
		!strings.Contains(got.stdout, "awaiting_owner_confirmation") ||
		!strings.Contains(got.stderr, "nothing is queued yet") || !strings.Contains(got.stderr, "card #91 on your owner's phone") {
		t.Fatalf("exit %d (want 13), admin %d, stdout %q, stderr %q", got.code, adminEnqueues(h), got.stdout, got.stderr)
	}
}

// TestEnqueueHelpDocumentsTheStepUpExit: `officer item enqueue --help` names
// exit 13 and what it means.
func TestEnqueueHelpDocumentsTheStepUpExit(t *testing.T) {
	root := newRootCmd(&app{stdout: io.Discard, stderr: io.Discard, getenv: func(string) string { return "" }})
	cmd, _, err := root.Find([]string{"officer", "item", "enqueue"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd.Long, "Exit 13: on a machine the step-up gate covers") || !strings.Contains(cmd.Long, "NOTHING IS QUEUED until it is confirmed") {
		t.Fatalf("enqueue's help does not document exit 13:\n%s", cmd.Long)
	}
}

// TestLoopbackListenTakesOnlyLoopbackLiterals: the secret goes only to a
// loopback IP literal — never to a name, however loopback it sounds.
func TestLoopbackListenTakesOnlyLoopbackLiterals(t *testing.T) {
	for listen, want := range map[string]bool{"127.0.0.1:47823": true, "127.8.9.10:1": true, "[::1]:47823": true,
		"localhost:47823": false, "LOCALHOST:47823": false, "localhost.:47823": false, "127.0.0.1.nip.io:47823": false,
		"192.0.2.7:47823": false, "[::]:47823": false, "0.0.0.0:47823": false, "127.0.0.1": false} {
		if got := loopbackListen(listen); got != want {
			t.Errorf("loopbackListen(%q) = %v; want %v", listen, got, want)
		}
	}
}

// TestEnqueueHasNoFlagThatNamesASession: the requester is muster-mirror's
// binding, so the command takes no session from its caller. Its flags are
// exactly these.
func TestEnqueueHasNoFlagThatNamesASession(t *testing.T) {
	root := newRootCmd(&app{stdout: io.Discard, stderr: io.Discard, getenv: func(string) string { return "" }})
	cmd, _, err := root.Find([]string{"officer", "item", "enqueue"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	cmd.LocalFlags().VisitAll(func(f *pflag.Flag) { names = append(names, f.Name) })
	sort.Strings(names)
	if got := strings.Join(names, ","); got != "kind,not-before,payload,priority,subject,tier" {
		t.Fatalf("enqueue's flags are %s", got)
	}
}

// TestTheSessionDoorSpellsMirrorHostsNames pins the header names and the
// secret's variable against literals and against muster-mirror's own
// spellings: a misspelt header is a door that binds no session, with every
// fake-door test still green.
func TestTheSessionDoorSpellsMirrorHostsNames(t *testing.T) {
	for _, c := range []struct{ got, literal, theirs string }{
		{mirrorPaneHeader, "X-Muster-Pane", mirrorhost.MCPPaneHeader},
		{mirrorTmuxHeader, "X-Muster-Tmux", mirrorhost.MCPTmuxHeader},
		{envMirrorMCPSecret, "MUSTER_MIRROR_MCP_SECRET", "MUSTER_MIRROR_MCP_SECRET"},
		{envMirrorURL, "MUSTER_MIRROR_URL", "MUSTER_MIRROR_URL"},
	} {
		if c.got != c.literal || c.got != c.theirs {
			t.Errorf("%q: want %q (muster-mirror's %q)", c.got, c.literal, c.theirs)
		}
	}
	if !strings.Contains(mcpAddCommand("127.0.0.1:1"), "${"+envMirrorMCPSecret+":-}") {
		t.Errorf("the secret's variable %q is not the one the MCP registration reads", envMirrorMCPSecret)
	}
}

// seamWindows is this host's tmux listing: pane doorPane runs Claude Code
// session doorSID; pane %18 is a shell.
type seamWindows struct{}

func (seamWindows) Windows(context.Context) ([]mirrorhost.WindowInfo, error) {
	return []mirrorhost.WindowInfo{
		{Session: "work", ID: "@4", Pane: doorPane, Claude: &mirrorhost.ClaudePane{Pane: doorPane, Gen: doorSID}},
		{Session: "work", ID: "@5", Pane: "%18"},
	}, nil
}

type seamSocket struct{}

func (seamSocket) ServerSocket(context.Context) (string, error) { return "/tmp/tmux-1000/default", nil }

// TestEnqueueIsBoundByTheRealMirrorProxy: the SEAM. The command's request
// through muster-mirror's own MCP proxy (internal/mirrorhost, with its tool
// watch on, as it runs by default) reaches muster's mirror door bound to the
// session the pane runs — the header muster binds the requester from — and
// from a pane that runs no Claude Code session it is refused there, so the
// command falls back and records nothing.
func TestEnqueueIsBoundByTheRealMirrorProxy(t *testing.T) {
	var mu sync.Mutex
	var forwarded []doorCall
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		forwarded = append(forwarded, doorCall{path: r.Method + " " + r.URL.Path, headers: r.Header.Clone(), body: string(b)})
		mu.Unlock()
		writeJSON(w, http.StatusOK, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"{\"id\":42,\"created\":true,\"requested_by_this_session\":true}"}]}}`)
	}))
	t.Cleanup(primary.Close)
	p := &mirrorhost.MCPProxy{URL: primary.URL, Token: "mmt_seam", AllowHTTP: true, Secret: doorSecret, Windows: seamWindows{},
		Socket: seamSocket{}, ToolsRefresh: time.Hour}
	proxy := httptest.NewServer(p)
	t.Cleanup(proxy.Close)
	settings := envMirrorMCPListen + "=" + strings.TrimPrefix(proxy.URL, "http://") + "\n"

	h := newHarness(t)
	h.json(enqRoute, http.StatusCreated, `{"id":5,"created":true}`)
	got := runEnqueue(t, h, sessionEnv(), settings+envMirrorURL+"="+h.srv.URL+"\n")
	mu.Lock()
	calls := append([]doorCall(nil), forwarded...)
	mu.Unlock()
	if got.code != exitOK || adminEnqueues(h) != 0 || len(calls) != 1 {
		t.Fatalf("exit %d, admin %d, forwarded %d, stderr %q", got.code, adminEnqueues(h), len(calls), got.stderr)
	}
	c := calls[0]
	if c.path != "POST "+mirrorhost.MCPPath || c.headers.Get(mirrorhost.MCPSessionHeader) != doorSID || c.headers.Get("Authorization") != "Bearer mmt_seam" ||
		!strings.Contains(c.body, `"name":"officer_item_enqueue"`) {
		t.Fatalf("muster was sent %s, session %q, auth %q: %s", c.path, c.headers.Get(mirrorhost.MCPSessionHeader), c.headers.Get("Authorization"), c.body)
	}

	// The control: the same command from the shell pane binds no session.
	env := sessionEnv()
	env["TMUX_PANE"] = "%18"
	h2 := newHarness(t)
	h2.json(enqRoute, http.StatusCreated, `{"id":5,"created":true}`)
	got = runEnqueue(t, h2, env, settings+envMirrorURL+"="+h2.srv.URL+"\n")
	mu.Lock()
	n := len(forwarded)
	mu.Unlock()
	if got.code != exitOK || n != 1 || adminEnqueues(h2) != 1 || !strings.Contains(got.stderr, "note: no session recorded on this item") {
		t.Fatalf("from a shell pane: exit %d, forwarded %d, admin %d, stderr %q", got.code, n, adminEnqueues(h2), got.stderr)
	}
}
