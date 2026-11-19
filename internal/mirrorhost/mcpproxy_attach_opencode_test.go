package mirrorhost

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/mirror"
)

// AN OPENCODE SESSION'S ASK CARRIES FILES TOO (mcpproxy_attach.go, "THE
// WORKING DIRECTORY IS THE BOUND SESSION'S"): its call is bound by the session
// id it carries (openCodeBinding), and its paths are resolved against the
// directory the host's opencode server records for THAT bound session —
// never the TUI's cwd, never anything else the call names — confined there
// after symlinks, exactly as a Claude Code session's are against its process's
// cwd.

// ocAttachDirs is a parent directory holding two sessions' directories, alpha
// and beta, each with a file of its own, and a file in neither.
type ocAttachDirs struct {
	parent, alpha, beta string
}

func newOCAttachDirs(t *testing.T) ocAttachDirs {
	t.Helper()
	parent := t.TempDir()
	d := ocAttachDirs{parent: parent, alpha: filepath.Join(parent, "alpha"), beta: filepath.Join(parent, "beta")}
	for _, dir := range []string{filepath.Join(d.alpha, "out"), d.beta} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, data := range map[string][]byte{
		filepath.Join(d.alpha, "out", "shot.png"): attachPNG,
		filepath.Join(d.alpha, "notes.txt"):       []byte("alpha's notes\n"),
		filepath.Join(d.beta, "secret.png"):       attachPNG,
		filepath.Join(parent, "outside.png"):      attachPNG,
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(parent, "outside.png"), filepath.Join(d.alpha, "link.png")); err != nil {
		t.Fatal(err)
	}
	return d
}

// ocAttachWindows: @1 Claude Code (pane %7, the attach PID); @3 opencode
// ses_alpha, launcher-bound; @4 opencode ses_beta, a verified title binding;
// @5 opencode ses_unverified, a title match muster does not send to.
type ocAttachWindows struct{}

func (ocAttachWindows) Windows(context.Context) ([]WindowInfo, error) {
	oc := func(pane, gen string, send bool, bind string) *AgentPane {
		// PID is the TUI's: its cwd is NOT where its session runs (ocAttachProxy's
		// Cwd answers the parent for it), so a root read from it is caught.
		return &AgentPane{Runtime: mirror.RuntimeOpenCode, Pane: pane, Gen: gen, Send: send, Bind: bind, PID: ocTUIPID}
	}
	return []WindowInfo{
		{Session: "work", ID: "@1", Pane: "%7", Claude: &ClaudePane{Pane: "%7", Gen: proxySID, PID: attachPID}},
		{Session: "work", ID: "@3", Pane: "%11", PanePID: ocTUIPID, Agent: oc("%11", "ses_alpha", true, "")},
		{Session: "work", ID: "@4", Pane: "%12", PanePID: ocTUIPID, Agent: oc("%12", "ses_beta", false, mirror.AgentBindVerified)},
		{Session: "work", ID: "@5", Pane: "%13", PanePID: ocTUIPID, Agent: oc("%13", "ses_unverified", false, mirror.AgentBindStream)},
	}, nil
}

// ocTUIPID is every opencode window's TUI process in ocAttachWindows.
const ocTUIPID = 5555

// ocAttachProxy is a proxy whose opencode server records d.alpha for
// ses_alpha, d.beta for ses_beta, and d.parent — WIDER than its root's — for
// ses_child, a subagent's session under ses_alpha. The Claude Code process's
// cwd is d.alpha too; the TUI's is d.parent.
func ocAttachProxy(base string, d ocAttachDirs) *MCPProxy {
	p := proxyFor(base)
	p.Windows = ocAttachWindows{}
	p.OpenCodeAncestors = func(id string) []string {
		return map[string][]string{"ses_child": {"ses_alpha"}}[id]
	}
	dirs := map[string]string{"ses_alpha": d.alpha, "ses_beta": d.beta, "ses_child": d.parent, "ses_unverified": d.parent}
	p.OpenCodeDirectory = func(id string) (string, bool) {
		dir, ok := dirs[id]
		return dir, ok
	}
	p.Cwd = func(pid int) (string, bool) {
		switch pid {
		case attachPID:
			return d.alpha, true
		case ocTUIPID:
			return d.parent, true
		}
		return "", false
	}
	return p
}

// ocAskCall is attention_ask from opencode session sid (params._meta), with
// meta's extra keys in _meta too.
func ocAskCall(sid string, args map[string]any, meta map[string]any) string {
	m := map[string]any{mirror.OpenCodeSessionMeta: sid}
	for k, v := range meta {
		m[k] = v
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{"name": "attention_ask", "arguments": args, "_meta": m}})
	return string(b)
}

// forwardedArgs is the arguments of the forwarded call body.
func forwardedArgs(t *testing.T, body string) map[string]json.RawMessage {
	t.Helper()
	var fwd struct {
		Params struct {
			Arguments map[string]json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(body), &fwd); err != nil {
		t.Fatal(err)
	}
	return fwd.Params.Arguments
}

// TestAnOpenCodeSessionsAttachmentsAreUploadedFromItsSessionsDirectory: a
// relative and an absolute path inside the bound session's directory are each
// uploaded — the uploads name the session's KEY and the opencode marker, never
// a Claude session — and the call is forwarded, as that session, with their
// ids in place of the paths. A subagent's child session, whose own recorded
// directory is wider, uploads from its ROOT's directory.
//
// 🔴 REGRESSION TEST: at the base every opencode caller's attachments were
// refused ("not supported yet") and nothing was uploaded.
func TestAnOpenCodeSessionsAttachmentsAreUploadedFromItsSessionsDirectory(t *testing.T) {
	d := newOCAttachDirs(t)
	f, srv := newFakeMusterForAsk(t)
	w := postBody(t, ocAttachProxy(srv.URL, d), ocHeaders(), ocAskCall("ses_alpha", map[string]any{"question": "Which?", "context": "picking the header",
		"attachments": []string{"out/shot.png", filepath.Join(d.alpha, "notes.txt")}}, nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `\"id\":1`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(f.files) != 2 || string(f.files[strings.Repeat("A", 22)]) != string(attachPNG) || string(f.files[strings.Repeat("B", 22)]) != "alpha's notes\n" {
		t.Fatalf("muster's upload store holds %d file(s): %q", len(f.files), f.files)
	}
	if strings.Join(f.sessions, ",") != "," || strings.Join(f.agents, ",") != "opencode:ses_alpha|opencode,opencode:ses_alpha|opencode" {
		t.Fatalf("the uploads named Claude sessions %q and agent sessions %q; want opencode:ses_alpha|opencode twice and no Claude session", f.sessions, f.agents)
	}
	if len(f.calls) != 1 || f.callAs[0] != "opencode:ses_alpha|opencode" {
		t.Fatalf("forwarded %d call(s) as %v", len(f.calls), f.callAs)
	}
	args := forwardedArgs(t, f.calls[0])
	if _, still := args["attachments"]; still {
		t.Fatalf("the paths were forwarded: %s", f.calls[0])
	}
	if got := string(args["attachment_ids"]); got != `["AAAAAAAAAAAAAAAAAAAAAA","BBBBBBBBBBBBBBBBBBBBBB"]` {
		t.Fatalf("attachment_ids = %s", got)
	}
	if string(args["question"]) != `"Which?"` || string(args["context"]) != `"picking the header"` {
		t.Fatalf("the question and its context were not forwarded as they were: %s", f.calls[0])
	}

	// A subagent's child session: bound to ses_alpha's window, so ses_alpha's
	// directory — "out/shot.png" exists there and not in the child's own
	// (wider) recorded directory — and the call and upload are ses_alpha's.
	f, srv = newFakeMusterForAsk(t)
	if w := postBody(t, ocAttachProxy(srv.URL, d), ocHeaders(), ocAskCall("ses_child", map[string]any{"question": "q", "attachments": []string{"out/shot.png"}}, nil)); w.Code != 200 {
		t.Fatalf("the child's ask: %d %s", w.Code, w.Body)
	}
	if len(f.files) != 1 || strings.Join(f.agents, ",") != "opencode:ses_alpha|opencode" {
		t.Fatalf("the child's ask uploaded %d file(s) as %v", len(f.files), f.agents)
	}
}

// TestAnOpenCodeSessionsAttachmentOutsideItsDirectoryIsRefused: `..`, an
// absolute path elsewhere, a symlink that leads out, and a path inside ANOTHER
// session's directory — relative and absolute — are each refused with the
// confinement's own sentence ("outside your workspace": the session's binding
// and directory read already passed, which the uploads above prove for the
// same caller), nothing is uploaded and no ask is filed. Which of
// workspacefile's two checks refuses each: the symlink only the one after
// symlinks are resolved; `..` to no file only the lexical one; the others
// both (each stays refused with either removed).
func TestAnOpenCodeSessionsAttachmentOutsideItsDirectoryIsRefused(t *testing.T) {
	d := newOCAttachDirs(t)
	for name, path := range map[string]string{
		"..": "../outside.png",
		// Nothing there: refused by the lexical check alone, before any
		// symlink is resolved (without it: "cannot read").
		".., to no file":             "../absent.png",
		"an absolute path elsewhere": filepath.Join(d.parent, "outside.png"),
		"a symlink out":              "link.png",
		"another session's, by ..":   "../beta/secret.png",
		"another session's, by abs":  filepath.Join(d.beta, "secret.png"),
	} {
		f, srv := newFakeMusterForAsk(t)
		text := toolErrorSentence(t, postBody(t, ocAttachProxy(srv.URL, d), ocHeaders(), ocAskCall("ses_alpha", map[string]any{"question": "q", "attachments": []string{path}}, nil)))
		if want := outsideRefusal(path); text != want {
			t.Errorf("%s: %q, want %q", name, text, want)
		}
		if len(f.calls) != 0 || len(f.files) != 0 {
			t.Errorf("%s: %d call(s) forwarded, %d file(s) uploaded", name, len(f.calls), len(f.files))
		}
	}
}

// TestAnOpenCodeCallerCannotWidenItsDirectoryByWhatItClaims: the call's only
// input to the directory is the session it is BOUND by. A directory claimed in
// _meta or in the arguments is not read; claiming ANOTHER session's id binds
// the call wholly to that session — its directory, its key on the upload and
// the call — so the claimant's own files are outside it; a child session never
// gets its own (wider) recorded directory; and a session no window binds, or
// one whose directory the server does not record as an absolute path,
// uploads nothing.
func TestAnOpenCodeCallerCannotWidenItsDirectoryByWhatItClaims(t *testing.T) {
	d := newOCAttachDirs(t)
	cases := []struct {
		name, sid string
		args      map[string]any
		meta      map[string]any
		want      string
	}{
		{"a directory in _meta", "ses_alpha", map[string]any{"question": "q", "attachments": []string{filepath.Join(d.beta, "secret.png")}},
			map[string]any{"ai.opencode/directory": d.parent, "directory": d.beta, "cwd": d.beta}, ""},
		{"a directory in the arguments", "ses_alpha", map[string]any{"question": "q", "directory": d.parent, "cwd": d.parent, "attachments": []string{"../outside.png"}}, nil, ""},
		{"another session's id, for the claimant's file", "ses_beta", map[string]any{"question": "q", "attachments": []string{filepath.Join(d.alpha, "notes.txt")}}, nil, ""},
		{"a child, for a file its own record would allow", "ses_child", map[string]any{"question": "q", "attachments": []string{filepath.Join(d.parent, "outside.png")}}, nil, ""},
		{"a session no window binds", "ses_unverified", map[string]any{"question": "q", "attachments": []string{filepath.Join(d.parent, "outside.png")}}, nil,
			`window @5 shows opencode session "ses_unverified", but its binding is not verified (muster does not send to it either)`},
		{"a session the server records no directory for", "ses_alpha", map[string]any{"question": "q", "attachments": []string{"out/shot.png"}}, nil,
			`cannot read the working directory of opencode session "ses_alpha" from this host's opencode server, so its attachments cannot be found; ask without them`},
		{"a relative recorded directory", "ses_alpha", map[string]any{"question": "q", "attachments": []string{"out/shot.png"}}, nil,
			`cannot read the working directory of opencode session "ses_alpha" from this host's opencode server, so its attachments cannot be found; ask without them`},
	}
	for _, c := range cases {
		f, srv := newFakeMusterForAsk(t)
		p := ocAttachProxy(srv.URL, d)
		switch c.name {
		case "a session the server records no directory for":
			p.OpenCodeDirectory = func(string) (string, bool) { return "", false }
		case "a relative recorded directory":
			p.OpenCodeDirectory = func(string) (string, bool) { return "alpha", true }
		}
		want := c.want
		if want == "" {
			want = outsideRefusal(c.args["attachments"].([]string)[0])
		}
		text := toolErrorSentence(t, postBody(t, p, ocHeaders(), ocAskCall(c.sid, c.args, c.meta)))
		if text != want {
			t.Errorf("%s: %q\nwant %q", c.name, text, want)
		}
		if len(f.calls) != 0 || len(f.files) != 0 {
			t.Errorf("%s: %d call(s) forwarded, %d file(s) uploaded", c.name, len(f.calls), len(f.files))
		}
	}
	// The control for "another session's id": the same claim, for a file IN
	// that session's directory, is uploaded and forwarded — as ses_beta.
	f, srv := newFakeMusterForAsk(t)
	if w := postBody(t, ocAttachProxy(srv.URL, d), ocHeaders(), ocAskCall("ses_beta", map[string]any{"question": "q", "attachments": []string{"secret.png"}}, nil)); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if strings.Join(f.agents, ",") != "opencode:ses_beta|opencode" || strings.Join(f.callAs, ",") != "opencode:ses_beta|opencode" {
		t.Fatalf("the claim of ses_beta uploaded as %v and called as %v; want ses_beta for both", f.agents, f.callAs)
	}
}

// toolErrorSentence is a tool error's sentence: its text is {"error": "<sentence>"}.
func toolErrorSentence(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	text := toolErrorText(t, w)
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(text), &e) != nil || e.Error == "" {
		t.Fatalf("a tool error that is not {\"error\": …}: %s", text)
	}
	return e.Error
}

// outsideRefusal is the call's refusal of path by the confinement, pinned as
// a literal: "the path is outside your workspace" is workspacefile.ErrOutside.
func outsideRefusal(path string) string {
	return "attachment " + quoteShort(path) + ": the path is outside your workspace"
}

// TestAnOpenCodeSessionsUploadsAreRevokedWhenItsAskFails: as a Claude Code
// session's — a later file refused here, or muster refusing the forwarded
// call — every upload the call made is taken back, naming the opencode
// session's key (and the marker), never a Claude session; an ask muster files
// keeps them.
func TestAnOpenCodeSessionsUploadsAreRevokedWhenItsAskFails(t *testing.T) {
	d := newOCAttachDirs(t)
	f, srv := newFakeMusterForAsk(t)
	text := toolErrorSentence(t, postBody(t, ocAttachProxy(srv.URL, d), ocHeaders(), ocAskCall("ses_alpha", map[string]any{"question": "q", "attachments": []string{"out/shot.png", "gone.png"}}, nil)))
	if !strings.Contains(text, "cannot read gone.png") || len(f.calls) != 0 {
		t.Fatalf("refusal %q, %d call(s) forwarded", text, len(f.calls))
	}
	if len(f.files) != 1 || strings.Join(f.revoked, ",") != strings.Repeat("A", 22) || strings.Join(f.revokeAs, ",") != "|opencode:ses_alpha|opencode" {
		t.Fatalf("uploaded %d, revoked %v as %v; want the one upload revoked as opencode:ses_alpha", len(f.files), f.revoked, f.revokeAs)
	}
	f, srv = newFakeMusterForAsk(t)
	f.refuseAsk = true
	w := postBody(t, ocAttachProxy(srv.URL, d), ocHeaders(), ocAskCall("ses_alpha", map[string]any{"question": "q", "attachments": []string{"out/shot.png", "notes.txt"}}, nil))
	if !strings.Contains(w.Body.String(), "the question is empty") || len(f.calls) != 1 {
		t.Fatalf("the refusal did not reach the caller: %s (%d calls)", w.Body, len(f.calls))
	}
	if strings.Join(f.revoked, ",") != strings.Repeat("A", 22)+","+strings.Repeat("B", 22) ||
		strings.Join(f.revokeAs, ",") != "|opencode:ses_alpha|opencode,|opencode:ses_alpha|opencode" {
		t.Fatalf("revoked %v as %v; want both uploads, as opencode:ses_alpha", f.revoked, f.revokeAs)
	}
	f, srv = newFakeMusterForAsk(t)
	if w := postBody(t, ocAttachProxy(srv.URL, d), ocHeaders(), ocAskCall("ses_alpha", map[string]any{"question": "q", "attachments": []string{"out/shot.png"}}, nil)); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(f.revoked) != 0 {
		t.Fatalf("a filed ask's uploads were revoked: %v", f.revoked)
	}
}

// TestAnOpenCodeSessionsDirectoryIsWhatItsServerRecords: OpenCodeSource's
// Directory is the location.directory the host's opencode server lists for
// the session, read with the host's token — a child's its own record (the
// proxy asks for the BOUND session's, never the child's) — and nothing for a
// session the server does not list, one it records no directory for, a
// relative one, or a host with no source. Then the seam: the proxy over that
// real source uploads a child session's file from its ROOT's recorded
// directory, and refuses one only the child's own (wider) record would allow.
func TestAnOpenCodeSessionsDirectoryIsWhatItsServerRecords(t *testing.T) {
	d := newOCAttachDirs(t)
	srv, src := ocServer(t, nil)
	srv.PutSessionIn("ses_alpha", "Alpha", "", d.alpha)
	srv.PutSessionIn("ses_beta", "Beta", "", d.beta)
	srv.PutSessionIn("ses_child", "Child", "ses_alpha", d.parent)
	srv.PutSessionIn("ses_rel", "Rel", "", "alpha")
	srv.PutSessionIn("ses_none", "None", "", "")
	for id, want := range map[string]string{"ses_alpha": d.alpha, "ses_beta": d.beta, "ses_child": d.parent, "ses_rel": "", "ses_none": "", "ses_gone": "", "": ""} {
		got, ok := src.Directory(id)
		if ok != (want != "") || (ok && got != want) {
			t.Errorf("Directory(%q) = %q, %v; want %q", id, got, ok, want)
		}
	}
	if got, ok := (*OpenCodeSource)(nil).Directory("ses_alpha"); ok || got != "" {
		t.Errorf("a host with no opencode source: %q, %v", got, ok)
	}

	f, musterSrv := newFakeMusterForAsk(t)
	p := ocAttachProxy(musterSrv.URL, d)
	p.OpenCodeDirectory = src.Directory
	p.OpenCodeAncestors = src.Ancestors
	if w := postBody(t, p, ocHeaders(), ocAskCall("ses_child", map[string]any{"question": "q", "attachments": []string{"out/shot.png"}}, nil)); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if len(f.files) != 1 || strings.Join(f.agents, ",") != "opencode:ses_alpha|opencode" {
		t.Fatalf("over the real source: %d file(s) uploaded as %v", len(f.files), f.agents)
	}
	text := toolErrorSentence(t, postBody(t, p, ocHeaders(), ocAskCall("ses_child", map[string]any{"question": "q", "attachments": []string{filepath.Join(d.parent, "outside.png")}}, nil)))
	if want := outsideRefusal(filepath.Join(d.parent, "outside.png")); text != want {
		t.Fatalf("over the real source, the child's own (wider) directory: %q, want %q", text, want)
	}
}
