//go:build tmuxit

package mirrorhost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/opencodehost"
	"github.com/ZacxDev/muster/internal/opencodehost/opencodetest"
)

// THE HOST SPAWN FOR opencode (agentspawn_opencode.go) against REAL tmux and
// REAL git, a FAKE opencode server (opencodetest, whose create, model and
// delete bodies are a real server's) and a stub launcher that does what
// `muster-mirror opencode -s <id>` does: records its argv, writes the launcher
// record naming its own pid and start time, and execs a stub `opencode` (which
// keeps that pid) — so the host's listing binds the pane through the REAL
// resolver (OpenCodeSource.bindLaunch), not a test seam.

const ocModel = "openrouter/qwen/qwen3-coder"

type ocRig struct {
	*spawnRig
	srv    *opencodetest.Server
	src    *OpenCodeSource
	socket string
	bin    string
}

func newOCRig(t *testing.T) *ocRig {
	t.Helper()
	r := &ocRig{spawnRig: newSpawnRig(t)}
	r.srv = opencodetest.New(ocFixturePassword)
	t.Cleanup(r.srv.Close)
	r.srv.SetModels(opencodetest.Model{ProviderID: "openrouter", ID: "qwen/qwen3-coder"}, opencodetest.Model{ProviderID: "openrouter", ID: "z-ai/glm-4.6"},
		opencodetest.Model{ProviderID: "opencode", ID: "big-pickle"})
	r.src = r.source(t, r.srv.URL, r.srv.Issue())
	r.socket = filepath.Join(r.tmp, "x", "tmux-"+strconv.Itoa(os.Getuid()), "default")
	r.bin = filepath.Join(r.tmp, "bin")
	launch := filepath.Join(r.tmp, "launch")
	r.src.SetLaunches(launch, r.socket)
	r.tm = Tmux{OpenCode: r.src}
	r.c.cfg.Backend = r.tm
	r.c.cfg.OpenCodeSend = true
	r.c.cfg.AgentSpawn.Rate = "100/10m"
	r.c.cfg.AgentSpawn.OpenCodeBin = filepath.Join(r.bin, "opencode")
	r.c.cfg.AgentSpawn.MirrorBin = filepath.Join(r.bin, "mm")
	r.c.cfg.AgentSpawn.BindWait = 15 * time.Second
	// The stub opencode: its version for --version (the fake's), else it stays
	// — as a process named opencode on the pane's terminal.
	writeExec(t, filepath.Join(r.bin, "opencode"), "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'opencode v2.0.0-fixture'; exit 0; fi\n"+
		"while :; do sleep 1; done\n")
	// The stub launcher: `<mm> opencode -s <id>`.
	writeExec(t, filepath.Join(r.bin, "mm"), "#!/bin/sh\nd=$(basename \"$PWD\")\n"+
		"printf '%s\\0' \"$@\" > "+r.recs+"/oc-$d.tmp && mv "+r.recs+"/oc-$d.tmp "+r.recs+"/oc-$d\n"+
		"sock=${TMUX%%,*}\nkey=$(printf %s \"$sock\" | sha256sum | cut -c1-16)\nmkdir -p "+launch+"/$key\n"+
		"start=$(cut -d' ' -f22 /proc/$$/stat)\n"+
		"printf '{\"v\":1,\"socket\":\"%s\",\"pane\":\"%s\",\"pid\":%d,\"pidStart\":\"%s\",\"session\":\"%s\",\"server\":\"%s\",\"directory\":\"%s\",\"created\":1}\\n' "+
		"\"$sock\" \"$TMUX_PANE\" $$ \"$start\" \"$3\" '"+r.srv.URL+"' \"$PWD\" > "+launch+"/$key/${TMUX_PANE#%}.json\n"+
		"date +%s%N > "+r.recs+"/bind-$d\n"+
		"exec "+filepath.Join(r.bin, "opencode")+"\n")
	// A launcher that binds nothing (no record): the bind never comes.
	writeExec(t, filepath.Join(r.bin, "mm-nobind"), "#!/bin/sh\nexec "+filepath.Join(r.bin, "opencode")+"\n")
	// The stream open, so a created session is seen as idle at once.
	_ = r.src.state()
	waitFor(t, "the opencode event stream", func() bool { s, _ := r.src.status("x"); return s != "" })
	return r
}

func (r *ocRig) source(t *testing.T, url, token string) *OpenCodeSource {
	t.Helper()
	creds, err := opencodehost.EnvCredentials(url, token)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return NewOpenCodeSource(ctx, creds, "", log.New(io.Discard, "", 0))
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func ocWidgets(prompt string) mirror.AgentSpawnReq {
	return mirror.AgentSpawnReq{Runtime: mirror.SpawnRuntimeOpenCode, Repo: "acme/widgets", Prompt: prompt, Name: "#9 Fix the typo!", Model: ocModel}
}

// promptsTo is every prompt the fake admitted for session sid.
func (r *ocRig) promptsTo(sid string) []opencodetest.Prompt {
	var out []opencodetest.Prompt
	for _, p := range r.srv.Prompts() {
		if p.Session == sid {
			out = append(out, p)
		}
	}
	return out
}

// at is when the fake logged op for sid (the first such), zero for never.
func (r *ocRig) at(op, sid string) time.Time {
	for _, e := range r.srv.Log() {
		if e.Op == op && e.Session == sid {
			return e.At
		}
	}
	return time.Time{}
}

func (r *ocRig) windows(t *testing.T) string {
	t.Helper()
	return r.tmux(t, "list-windows", "-a", "-F", "#{window_id}")
}

// TestTmuxASpawnedOpenCodeRunsItsModelAndGetsThePromptAfterTheBind: for every
// prompt shape phase 3 lists, the session is created IN THE WORKTREE on exactly
// {providerID: openrouter, id: qwen/qwen3-coder}; the launcher's argv is
// EXACTLY [opencode -s <session>] (no prompt word); the window is oc-<d>-<slug>,
// tagged; the prompt arrives ONCE, byte-identical, delivery queue, under the
// id derived from the dispatch; and the fake saw create < bind < prompt.
func TestTmuxASpawnedOpenCodeRunsItsModelAndGetsThePromptAfterTheBind(t *testing.T) {
	r := newOCRig(t)
	prompts := []string{" a;b", " tail;", ` \;`, " $(id) `id`", ` 'single' "double"`, "\n-leading dash\n", "\nlead and trail\n\n",
		" " + strings.Repeat("x", 8<<10-len(mirror.SpawnPreface)-1)}
	for i, tail := range prompts {
		d := int64(200 + i)
		prompt := mirror.SpawnPreface + tail
		w, e := r.spawn(t, d, ocWidgets(prompt))
		if e != nil {
			t.Fatalf("prompt %q: refused %+v", tail, e)
		}
		sid := w.AgentSession
		if !opencodehost.ValidSessionID(sid) || !strings.HasPrefix(sid, "ses_") {
			t.Fatalf("the session %q is not an opencode session id", sid)
		}
		if m := r.srv.SessionModel(sid); m["providerID"] != "openrouter" || m["id"] != "qwen/qwen3-coder" || len(m) != 2 {
			t.Fatalf("the session's model: %v", m)
		}
		cl, _ := r.src.client()
		if s, err := cl.GetSession(context.Background(), sid); err != nil || s.Location.Directory != w.Worktree {
			t.Fatalf("the session's directory %q, want the worktree %q (%v)", s.Location.Directory, w.Worktree, err)
		}
		argv := r.recOf(t, "oc-"+strconv.FormatInt(d, 10))
		if !slices.Equal(argv, []string{"opencode", "-s", sid}) {
			t.Fatalf("prompt %q: the launcher saw %q; want [opencode -s %s]", tail, argv, sid)
		}
		wantName := "oc-" + strconv.FormatInt(d, 10) + "-9-fix-the-typo"
		if got := r.tmux(t, "display-message", "-p", "-t", w.Window, "#{session_name} #{window_name}"); got != mirror.AgentSpawnSession+" "+wantName {
			t.Fatalf("the window is %q; want agents %s", got, wantName)
		}
		if tag, err := r.tm.dispatchTagOf(context.Background(), w.Window); err != nil || tag != d {
			t.Fatalf("the window's own tag: %d %v", tag, err)
		}
		ps := r.promptsTo(sid)
		if len(ps) != 1 || ps[0].Text != prompt || ps[0].Delivery != "queue" || ps[0].ID != spawnMessageID(d, sid) {
			t.Fatalf("prompt %q: the fake admitted %+v; want it once, byte-identical, queued, under %s", tail, ps, spawnMessageID(d, sid))
		}
		bindNs, err := strconv.ParseInt(strings.TrimSpace(string(r.readRec(t, "bind-"+strconv.FormatInt(d, 10)))), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		created, bound, posted := r.at("create", sid), time.Unix(0, bindNs), r.at("prompt", sid)
		if created.IsZero() || posted.IsZero() || !created.Before(bound) || !bound.Before(posted) {
			t.Fatalf("prompt %q: create %v, bind %v, prompt %v; want create < bind < prompt", tail, created, bound, posted)
		}
	}
	if got := r.srv.Creates(); got != len(prompts) {
		t.Fatalf("%d sessions created for %d spawns", got, len(prompts))
	}
	lines := r.auditLines(t)
	if len(lines) != 3*len(prompts) || lines[0]["runtime"] != "opencode" || lines[0]["model"] != ocModel || lines[2]["result"] != "spawned" {
		t.Fatalf("audit: %d lines (%v)", len(lines), lines[:3])
	}
}

func (r *ocRig) readRec(t *testing.T, name string) []byte {
	t.Helper()
	p := filepath.Join(r.recs, name)
	waitFor(t, "the stub to record "+name, func() bool { _, err := os.Stat(p); return err == nil })
	b, _ := os.ReadFile(p)
	return b
}

func (r *ocRig) recOf(t *testing.T, name string) []string {
	t.Helper()
	return strings.Split(strings.TrimSuffix(string(r.readRec(t, name)), "\x00"), "\x00")
}

// TestTmuxAnOpenCodeSpawnIsRefusedBeforeAnySession: each of opencode's own
// refusals comes with its code and detail, BEFORE any git call, any session and
// any window (the fake's create count stays 0, counted) — and the same rig then
// spawns with a listed model (the positive control: 1 create).
func TestTmuxAnOpenCodeSpawnIsRefusedBeforeAnySession(t *testing.T) {
	r := newOCRig(t)
	before := r.windows(t)
	for _, c := range []struct {
		name, code, detail string
		req                mirror.AgentSpawnReq
		prep, undo         func()
	}{
		{"a model the host's opencode does not list", mirror.SpawnBadModel, `this host's opencode lists no OpenRouter model "nope/none"`,
			func() mirror.AgentSpawnReq { q := ocWidgets(testPrompt); q.Model = "openrouter/nope/none"; return q }(), nil, nil},
		{"a model of another provider", mirror.SpawnBadModel, `"opencode/big-pickle" is not an OpenRouter model (openrouter/<vendor>/<model>)`,
			func() mirror.AgentSpawnReq { q := ocWidgets(testPrompt); q.Model = "opencode/big-pickle"; return q }(), nil, nil},
		{"no OpenRouter model listed at all", mirror.SpawnUnavailable, "opencode's model list is not ready (it lists no OpenRouter model)",
			ocWidgets(testPrompt), func() { r.srv.SetModels(opencodetest.Model{ProviderID: "opencode", ID: "big-pickle"}) },
			func() {
				r.srv.SetModels(opencodetest.Model{ProviderID: "openrouter", ID: "qwen/qwen3-coder"})
			}},
		{"the TUI's version is not the service's", mirror.SpawnUnavailable, "opencode TUI and service versions differ",
			ocWidgets(testPrompt), func() { r.srv.SetVersion("2.0.1-other") }, func() { r.srv.SetVersion("2.0.0-fixture") }},
		{"the service unreachable", mirror.SpawnUnavailable, "the opencode service cannot be reached", ocWidgets(testPrompt),
			func() { r.tm.OpenCode = r.source(t, "http://127.0.0.1:1", r.srv.Issue()); r.c.cfg.Backend = r.tm },
			func() { r.tm.OpenCode = r.src; r.c.cfg.Backend = r.tm }},
		{"no token", mirror.SpawnUnavailable, "no opencode token on this host (muster-mirror opencode-pair)", ocWidgets(testPrompt),
			func() { r.tm.OpenCode = NewOpenCodeSource(context.Background(), nil, "no token", log.New(io.Discard, "", 0)); r.c.cfg.Backend = r.tm },
			func() { r.tm.OpenCode = r.src; r.c.cfg.Backend = r.tm }},
		{"-opencode-send off", mirror.SpawnUnavailable, "this host does not send to opencode (-opencode-send is off)", ocWidgets(testPrompt),
			func() { r.c.cfg.OpenCodeSend = false }, func() { r.c.cfg.OpenCodeSend = true }},
	} {
		if c.prep != nil {
			c.prep()
		}
		gitBefore := r.gitCalls()
		if _, e := r.spawn(t, 300, c.req); e == nil || e.code != c.code || e.detail != c.detail {
			t.Fatalf("%s: %+v; want %s %q", c.name, e, c.code, c.detail)
		}
		if c.undo != nil {
			c.undo()
		}
		if n := r.srv.Creates(); n != 0 {
			t.Fatalf("%s: %d sessions created", c.name, n)
		}
		if n := r.gitCalls() - gitBefore; n != 0 {
			t.Fatalf("%s: git ran %d times", c.name, n)
		}
		if after := r.windows(t); after != before {
			t.Fatalf("%s: a window opened", c.name)
		}
	}
	if _, e := r.spawn(t, 301, ocWidgets(testPrompt)); e != nil || r.srv.Creates() != 1 {
		t.Fatalf("the positive control: %+v, %d creates", e, r.srv.Creates())
	}
}

// TestTmuxALostPromptAnswerIsRetriedOnceUnderTheSameID: the first post's answer
// never comes in time (the prompt WAS admitted); the host posts again with the
// SAME id, and the session holds exactly one user message.
func TestTmuxALostPromptAnswerIsRetriedOnceUnderTheSameID(t *testing.T) {
	r := newOCRig(t)
	old := openCodeSendTimeout
	openCodeSendTimeout = 700 * time.Millisecond
	t.Cleanup(func() { openCodeSendTimeout = old })
	r.srv.DelayFirstPromptAnswer(3 * time.Second)
	w, e := r.spawn(t, 310, ocWidgets(testPrompt))
	if e != nil {
		t.Fatalf("refused: %+v", e)
	}
	posts := 0
	for _, l := range r.srv.Log() {
		if l.Op == "prompt" && l.Session == w.AgentSession {
			posts++
		}
	}
	if ps := r.promptsTo(w.AgentSession); posts != 2 || len(ps) != 1 || ps[0].ID != spawnMessageID(310, w.AgentSession) {
		t.Fatalf("%d posts, admitted %+v; want 2 posts of one id, one message", posts, ps)
	}
}

// TestTmuxAnOpenCodeSpawnThatFailsBeforeThePostLeavesNothing: the launcher never
// binds the pane; the spawn is refused failed, the window is killed, the
// session it created is DELETED (the fake's sessions back to their start), the
// worktree is removed, and nothing was posted.
func TestTmuxAnOpenCodeSpawnThatFailsBeforeThePostLeavesNothing(t *testing.T) {
	r := newOCRig(t)
	r.c.cfg.AgentSpawn.MirrorBin = filepath.Join(r.bin, "mm-nobind")
	r.c.cfg.AgentSpawn.BindWait = 1500 * time.Millisecond
	sessionsBefore, windowsBefore := len(r.srv.Sessions()), r.windows(t)
	if _, e := r.spawn(t, 320, ocWidgets(testPrompt)); e == nil || e.code != mirror.SpawnFailed || e.detail != "the window did not bind to its opencode session" {
		t.Fatalf("refusal %+v", e)
	}
	if r.srv.Creates() != 1 || len(r.srv.Sessions()) != sessionsBefore {
		t.Fatalf("created %d, sessions %d (were %d): the session was not deleted", r.srv.Creates(), len(r.srv.Sessions()), sessionsBefore)
	}
	if len(r.srv.Prompts()) != 0 || r.srv.PromptPosts() != 0 {
		t.Fatal("a prompt was posted")
	}
	if r.windows(t) != windowsBefore {
		t.Fatal("the window was not killed")
	}
	if _, err := os.Stat(filepath.Join(r.dir, "acme__widgets", "320")); !os.IsNotExist(err) {
		t.Fatalf("the worktree is still there: %v", err)
	}
}

// TestTmuxAPromptNotAcceptedKeepsTheWindow: the post is refused (400); the spawn
// is refused failed "prompt not accepted" — and the window, its session and its
// worktree are KEPT (the owner may be typing in it).
func TestTmuxAPromptNotAcceptedKeepsTheWindow(t *testing.T) {
	r := newOCRig(t)
	r.srv.RefusePrompts(true)
	sessionsBefore := len(r.srv.Sessions())
	w, e := r.spawn(t, 330, ocWidgets(testPrompt))
	if e == nil || e.code != mirror.SpawnFailed || e.detail != "prompt not accepted" {
		t.Fatalf("refusal %+v", e)
	}
	if tag, err := r.tm.dispatchTagOf(context.Background(), w.Window); err != nil || tag != 330 {
		t.Fatalf("the window %q is gone or untagged: %d %v", w.Window, tag, err)
	}
	if len(r.srv.Sessions()) != sessionsBefore+1 {
		t.Fatal("the session was deleted")
	}
	if _, err := os.Stat(w.Worktree); err != nil {
		t.Fatalf("the worktree is gone: %v", err)
	}
}

// driven runs the rig's host against a driven muster (the wire), with the rig's
// spawn configuration and the gated send on.
func (r *ocRig) driven(t *testing.T, openCodeSend bool) *drivenMuster {
	t.Helper()
	d := newDrivenMuster(t)
	c, err := New(Config{URL: d.srv.URL, Token: mirror.TokenPrefix + "t", Host: "laptop", Backend: r.tm, Interactive: true,
		OpenCodeSend: openCodeSend, AgentSpawn: r.c.cfg.AgentSpawn, AllowHTTP: true, Refresh: 100 * time.Millisecond, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = c.RunOnce(ctx) }()
	waitFor(t, "the hello", func() bool { return len(d.of(mirror.TypeHello)) == 1 })
	return d
}

// TestTmuxTheHelloOffersOpenCodeOnlyBesideTheSend: with -opencode-send (and a
// token) the hello offers claude and opencode; without it, claude alone.
func TestTmuxTheHelloOffersOpenCodeOnlyBesideTheSend(t *testing.T) {
	r := newOCRig(t)
	for _, c := range []struct {
		send bool
		want []string
	}{{true, []string{"claude", "opencode"}}, {false, []string{"claude"}}} {
		d := r.driven(t, c.send)
		if got := d.of(mirror.TypeHello)[0].AgentSpawn; !slices.Equal(got, c.want) {
			t.Fatalf("-opencode-send %v: the hello offers %q; want %q", c.send, got, c.want)
		}
	}
}

// TestTmuxAForwardReachesASpawnedOpenCodeWindowOnlyThroughItsTag: over the wire,
// a spawn answers agent-spawned with the window (after a listing carried it);
// a paste naming ANOTHER dispatch into that window is refused not_muster_window
// BEFORE the gated send runs (the fake's prompt posts do not move); a paste
// naming its own dispatch is posted once through opencode's API; while the
// session is busy the gated send holds it (refused, nothing posted).
func TestTmuxAForwardReachesASpawnedOpenCodeWindowOnlyThroughItsTag(t *testing.T) {
	r := newOCRig(t)
	d := r.driven(t, true)
	req := ocWidgets(testPrompt)
	d.send(t, mirror.Msg{Type: mirror.TypeAgentSpawn, ID: 900, Dispatch: 340, Spawn: &req})
	var sp mirror.Msg
	waitForLong(t, "agent-spawned", func() bool {
		for _, m := range d.of(mirror.TypeAgentSpawned) {
			if m.ID == 900 {
				sp = m
				return true
			}
		}
		return false
	})
	if sp.Spawned == nil || sp.Refusal != "" {
		t.Fatalf("the spawn: %+v", sp)
	}
	win, sid := sp.Spawned.Window, sp.Spawned.AgentSession
	var listed mirror.Window
	waitFor(t, "the listing of the bound, idle window", func() bool {
		ms := append(d.of(mirror.TypeHello), d.of(mirror.TypeSessions)...)
		for i := len(ms) - 1; i >= 0; i-- {
			for _, w := range ms[i].Windows {
				if w.ID == win && w.Dispatch == 340 && w.AgentSend && w.AgentSession == sid && w.StatusSince > 0 && w.Status == mirror.StatusIdle {
					listed = w
					return true
				}
			}
		}
		return false
	})
	posts := r.srv.PromptPosts()
	paste := func(id uint32, dispatch int64, text string) mirror.Msg {
		d.send(t, mirror.Msg{Type: mirror.TypePaste, ID: id, Session: mirror.AgentSpawnSession, Window: win, Text: text, Runtime: mirror.RuntimeOpenCode,
			Since: listed.StatusSince, MessageID: opencodehost.NewMessageID(time.Now()), Dispatch: dispatch})
		var out mirror.Msg
		waitFor(t, "the pasted answer", func() bool {
			for _, m := range d.of(mirror.TypePasted) {
				if m.ID == id {
					out = m
					return true
				}
			}
			return false
		})
		return out
	}
	if m := paste(901, 341, "muster: task #9 changed"); m.Refusal != mirror.PasteRefusalNotMusterWindow || r.srv.PromptPosts() != posts {
		t.Fatalf("another dispatch's forward: %+v, posts %d → %d", m, posts, r.srv.PromptPosts())
	}
	if m := paste(902, 340, "muster: task #9 changed"); m.Error != "" {
		t.Fatalf("its own dispatch's forward: %+v", m)
	}
	if ps := r.promptsTo(sid); len(ps) != 2 || ps[1].Text != "muster: task #9 changed" {
		t.Fatalf("admitted %+v; want the first prompt, then the forward once", ps)
	}
	r.srv.SetStatus(sid, "busy")
	waitFor(t, "busy", func() bool { s, _ := r.src.status(sid); return s == mirror.StatusBusy })
	posts = r.srv.PromptPosts()
	if m := paste(903, 340, "muster: again"); m.Refusal != mirror.PasteRefusalOpenCodeNotIdle || r.srv.PromptPosts() != posts {
		t.Fatalf("a forward while busy: %+v", m)
	}
}

func waitForLong(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// launchKey is the record directory's key for socket (opencodehost.LaunchPath),
// spelled out so the stub launcher's shell can be checked against it.
func launchKey(socket string) string {
	s := sha256.Sum256([]byte(socket))
	return hex.EncodeToString(s[:8])
}

// TestTheStubLauncherWritesWhereTheResolverReads: the rig's shell spells the
// record's directory as opencodehost.LaunchPath does (an instrument check: a
// stub writing elsewhere would bind nothing and every case above would fail
// for the wrong reason).
func TestTheStubLauncherWritesWhereTheResolverReads(t *testing.T) {
	p, err := opencodehost.LaunchPath("/l", "/s/tmux-1/default", "%3")
	if err != nil || p != filepath.Join("/l", launchKey("/s/tmux-1/default"), "3.json") {
		t.Fatalf("LaunchPath %q %v", p, err)
	}
}
