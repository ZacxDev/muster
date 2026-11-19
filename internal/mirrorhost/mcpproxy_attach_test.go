package mirrorhost

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ZacxDev/muster/internal/askattach"
)

// AN ASK'S FILES ARE UPLOADED FROM THIS HOST (mcpproxy_attach.go): the proxy
// takes attention_ask's attachments paths out of the call, resolves each
// against the calling session's working directory, sends it to muster's
// upload route, and forwards the call with attachment_ids. A refusal is the
// call's tool error, and the call is not forwarded.

var attachPNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89-pixels")

// attachPID is the calling Claude Code process in attachWindows.
const attachPID = 4321

// attachWindows is paneWindows with the Claude pane's process.
type attachWindows struct{}

func (attachWindows) Windows(context.Context) ([]WindowInfo, error) {
	return []WindowInfo{{Session: "work", ID: "@1", Pane: "%7", Claude: &ClaudePane{Pane: "%7", Gen: proxySID, PID: attachPID}}}, nil
}

// fakeMusterForAsk is muster: its upload route stores each file (answering an
// id per file), its MCP door records each forwarded call.
type fakeMusterForAsk struct {
	mu       sync.Mutex
	files    map[string][]byte // id → bytes
	sessions []string          // the Claude session each upload named
	agents   []string          // the agent session key (and marker) each upload named
	calls    []string          // forwarded tools/call bodies
	callAs   []string          // the agent session key (and marker) each forwarded call named
	revoked  []string          // ids revoked (DELETE MirrorPath/{id})
	revokeAs []string          // the session each take-back named: "<claude>|<agent key>|<marker>"
	// refuseAsk: the MCP door answers the forwarded call with a tool error.
	refuseAsk bool
}

func newFakeMusterForAsk(t *testing.T) (*fakeMusterForAsk, *httptest.Server) {
	t.Helper()
	f := &fakeMusterForAsk{files: map[string][]byte{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, askattach.MirrorPath+"/") {
			if r.Header.Get("Authorization") != "Bearer "+proxyToken {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			f.revoked = append(f.revoked, strings.TrimPrefix(r.URL.Path, askattach.MirrorPath+"/"))
			f.revokeAs = append(f.revokeAs, r.Header.Get(MCPSessionHeader)+"|"+agentOf(r))
			w.WriteHeader(http.StatusNoContent)
			return
		}
		switch r.URL.Path {
		case askattach.MirrorPath:
			if r.Header.Get("Authorization") != "Bearer "+proxyToken {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			id := strings.Repeat(string(rune('A'+len(f.files))), 22)
			f.files[id] = b
			f.sessions = append(f.sessions, r.Header.Get(MCPSessionHeader))
			f.agents = append(f.agents, agentOf(r))
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(askattach.Record{ID: id, Name: r.URL.Query().Get("filename"), Type: askattach.Sniff(b), Size: int64(len(b))})
		case MCPPath:
			f.calls = append(f.calls, string(b))
			f.callAs = append(f.callAs, agentOf(r))
			w.Header().Set("Content-Type", "application/json")
			if f.refuseAsk {
				_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"{\"error\":\"the question is empty\"}"}],"isError":true}}`)
				return
			}
			_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":7,"result":{"content":[{"type":"text","text":"{\"id\":1}"}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// agentOf is how r names an agent session: "<X-Muster-Agent-Session>|<X-Muster-Client>"
// ("|" for a request naming neither).
func agentOf(r *http.Request) string {
	return r.Header.Get(MCPAgentSessionHeader) + "|" + r.Header.Get(MCPClientHeader)
}

func attachProxy(base, cwd string) *MCPProxy {
	p := proxyFor(base)
	p.Windows = attachWindows{}
	p.Cwd = func(pid int) (string, bool) { return cwd, pid == attachPID }
	return p
}

func askCall(args map[string]any) string {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": map[string]any{"name": "attention_ask", "arguments": args}})
	return string(b)
}

var attachCaller = map[string]string{MCPPaneHeader: "%7", MCPTmuxHeader: proxyTMUX, "Authorization": "Bearer " + proxySecret}

func TestASessionsAttachmentsAreUploadedFromItsWorkingDirectoryAndTheCallCarriesTheirIDs(t *testing.T) {
	f, srv := newFakeMusterForAsk(t)
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "out"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "out", "shot.png"), attachPNG, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "notes.txt"), []byte("why it failed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := postBody(t, attachProxy(srv.URL, cwd), attachCaller, askCall(map[string]any{"question": "Which?", "attachments": []string{"out/shot.png", filepath.Join(cwd, "notes.txt")}}))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `\"id\":1`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(f.files) != 2 || string(f.files[strings.Repeat("A", 22)]) != string(attachPNG) || string(f.files[strings.Repeat("B", 22)]) != "why it failed\n" {
		t.Fatalf("muster's upload store holds %d file(s): %q", len(f.files), f.files)
	}
	if f.sessions[0] != proxySID || f.sessions[1] != proxySID {
		t.Fatalf("the uploads named sessions %v, want the caller's %s", f.sessions, proxySID)
	}
	if f.agents[0] != "|" || f.agents[1] != "|" {
		t.Fatalf("a Claude Code caller's uploads named an agent session: %v", f.agents)
	}
	if len(f.calls) != 1 {
		t.Fatalf("%d forwarded calls", len(f.calls))
	}
	var fwd struct {
		Params struct {
			Arguments map[string]json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(f.calls[0]), &fwd); err != nil {
		t.Fatal(err)
	}
	if _, still := fwd.Params.Arguments["attachments"]; still {
		t.Fatalf("the paths were forwarded: %s", f.calls[0])
	}
	if got := string(fwd.Params.Arguments["attachment_ids"]); got != `["AAAAAAAAAAAAAAAAAAAAAA","BBBBBBBBBBBBBBBBBBBBBB"]` {
		t.Fatalf("attachment_ids = %s", got)
	}
	if got := string(fwd.Params.Arguments["question"]); got != `"Which?"` {
		t.Fatalf("the question was not forwarded as it was: %s", got)
	}
}

func TestARefusedAttachmentFailsTheCallAndNothingIsForwarded(t *testing.T) {
	parent := t.TempDir()
	cwd := filepath.Join(parent, "repo")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "id_rsa.png")
	if err := os.WriteFile(outside, attachPNG, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(cwd, "link.png")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "ok.png"), attachPNG, 0o600); err != nil {
		t.Fatal(err)
	}
	five := []string{"ok.png", "ok.png", "ok.png", "ok.png", "ok.png"}
	cases := map[string]struct {
		args map[string]any
		want string
	}{
		"a path outside":   {map[string]any{"question": "q", "attachments": []string{"../id_rsa.png"}}, "outside your workspace"},
		"an absolute one":  {map[string]any{"question": "q", "attachments": []string{outside}}, "outside your workspace"},
		"a symlink out":    {map[string]any{"question": "q", "attachments": []string{"link.png"}}, "outside your workspace"},
		"a missing file":   {map[string]any{"question": "q", "attachments": []string{"gone.png"}}, "cannot read gone.png"},
		"too many":         {map[string]any{"question": "q", "attachments": five}, "at most 4 attachments (5 named)"},
		"too many with id": {map[string]any{"question": "q", "attachments": five[:3], "attachment_ids": []string{"x", "y"}}, "at most 4 attachments (5 named)"},
		"not a list":       {map[string]any{"question": "q", "attachments": "ok.png"}, "attachments must be a list of file paths"},
	}
	for name, c := range cases {
		f, srv := newFakeMusterForAsk(t)
		text := toolErrorText(t, postBody(t, attachProxy(srv.URL, cwd), attachCaller, askCall(c.args)))
		if !strings.Contains(text, c.want) {
			t.Errorf("%s: %q, want %q", name, text, c.want)
		}
		if len(f.calls) != 0 || len(f.files) != 0 {
			t.Errorf("%s: %d call(s) forwarded, %d file(s) uploaded", name, len(f.calls), len(f.files))
		}
	}
}

func TestACallWithoutAttachmentsIsForwardedAsItWas(t *testing.T) {
	f, srv := newFakeMusterForAsk(t)
	body := askCall(map[string]any{"question": "q?", "context": "why"})
	if w := postBody(t, attachProxy(srv.URL, t.TempDir()), attachCaller, body); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(f.calls) != 1 || f.calls[0] != body || len(f.files) != 0 {
		t.Fatalf("forwarded %q, uploaded %d", f.calls, len(f.files))
	}
}

// TestFilesAlreadyUploadedAreRevokedWhenTheAskFails (round 1 G2): a call that
// fails after some of its files were uploaded — a later file refused here, or
// the forwarded call refused by muster — revokes, best effort, the uploads it
// made, so nothing is left in the store for an ask that was never filed.
func TestFilesAlreadyUploadedAreRevokedWhenTheAskFails(t *testing.T) {
	cwd := t.TempDir()
	for _, n := range []string{"a.png", "b.png"} {
		if err := os.WriteFile(filepath.Join(cwd, n), attachPNG, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A later file refused here (missing): the first upload is revoked.
	f, srv := newFakeMusterForAsk(t)
	text := toolErrorText(t, postBody(t, attachProxy(srv.URL, cwd), attachCaller, askCall(map[string]any{"question": "q", "attachments": []string{"a.png", "gone.png"}})))
	if !strings.Contains(text, "cannot read gone.png") || len(f.calls) != 0 {
		t.Fatalf("refusal %q, %d call(s) forwarded", text, len(f.calls))
	}
	if len(f.files) != 1 || len(f.revoked) != 1 || f.revoked[0] != strings.Repeat("A", 22) {
		t.Fatalf("uploaded %d, revoked %v; want the one upload revoked", len(f.files), f.revoked)
	}
	// The forwarded call refused by muster: every upload of the call is revoked,
	// and the refusal reaches the caller unchanged.
	f, srv = newFakeMusterForAsk(t)
	f.refuseAsk = true
	w := postBody(t, attachProxy(srv.URL, cwd), attachCaller, askCall(map[string]any{"question": "q", "attachments": []string{"a.png", "b.png"}}))
	if !strings.Contains(w.Body.String(), "the question is empty") || len(f.calls) != 1 {
		t.Fatalf("the refusal did not reach the caller: %s (%d calls)", w.Body, len(f.calls))
	}
	if strings.Join(f.revoked, ",") != strings.Repeat("A", 22)+","+strings.Repeat("B", 22) {
		t.Fatalf("revoked %v; want both uploads", f.revoked)
	}
	if strings.Join(f.revokeAs, ",") != proxySID+"||,"+proxySID+"||" {
		t.Fatalf("the take-backs named %v; want the Claude session alone", f.revokeAs)
	}
	// The control: an ask muster files keeps its uploads.
	f, srv = newFakeMusterForAsk(t)
	if w := postBody(t, attachProxy(srv.URL, cwd), attachCaller, askCall(map[string]any{"question": "q", "attachments": []string{"a.png"}})); w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(f.revoked) != 0 {
		t.Fatalf("a filed ask's uploads were revoked: %v", f.revoked)
	}
}
