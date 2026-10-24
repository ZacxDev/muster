package api

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/sendqueue"
	"github.com/ZacxDev/muster/internal/ui"
)

// CHARACTERIZATION GOLDENS for the runtime-interface refactor, server UI and
// queue half: recorded BEFORE it against the per-runtime branches it
// replaces, and read the same after it. A golden that changes is a behaviour
// change. Every fixture is synthetic.

// TODO(runtime-interface): delete these goldens in the final fix pass after
// review round 1; until then they are the proof that before equals after.

var updateCharacterization = flag.Bool("update-characterization", false, "rewrite testdata/characterization/*.golden")

func characterizationGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "characterization", name+".golden")
	if *updateCharacterization {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the golden (record it with -update-characterization BEFORE a refactor, never after): %v", err)
	}
	if string(want) != got {
		t.Fatalf("%s differs from its golden %s.\n--- got\n%s\n--- want\n%s", name, path, got, want)
	}
}

const charSID = "local-0123456789abcdef0123456789abcdef0123456789abcdef0123456789ab"

func charAPIWindows() []mirror.Window {
	return []mirror.Window{
		{Session: "main", ID: "@1", Index: 1, Active: true, Command: "claude", Claude: true, ClaudeSession: "c-1", Status: mirror.StatusIdle, StatusSince: 1001},
		{Session: "main", ID: "@2", Index: 2, Command: "claude", Claude: true, ClaudeSession: "c-2", Status: mirror.StatusWaiting, StatusSince: 1002,
			Pending: &mirror.Pending{Summary: "Edit a file", Tool: "Edit", Last: "about to edit"}},
		{Session: "main", ID: "@3", Index: 3, Command: "python3.14", Runtime: mirror.RuntimeHermes, AgentSession: charSID, Status: mirror.StatusIdle, StatusSince: 1003},
		{Session: "main", ID: "@4", Index: 4, Command: "python3.14", Runtime: mirror.RuntimeHermes, AgentSession: charSID, Status: mirror.StatusWaiting, StatusSince: 1004,
			Pending: &mirror.Pending{Kind: mirror.PendingApproval, Summary: "terminal: ls", Tool: "terminal", Detail: "ls", PromptID: "p1", Choices: "once deny"}},
		{Session: "main", ID: "@5", Index: 5, Command: "python3.14", Runtime: mirror.RuntimeHermes},
		{Session: "main", ID: "@6", Index: 6, Command: "opencode"},
		{Session: "main", ID: "@7", Index: 7, Command: "zsh"},
		{Session: "main", ID: "@8", Index: 8, Command: "python3.14", Runtime: mirror.RuntimeHermes, AgentSession: charSID, Status: mirror.StatusWaiting, StatusSince: 1008},
	}
}

// TestCharacterizeTheSendRules: Send now's refusal and the queue's probe for
// every runtime and status, and whether a window can be queued for.
func TestCharacterizeTheSendRules(t *testing.T) {
	var b strings.Builder
	unknown := errors.New("mirror: the host cannot tell")
	for _, rt := range []string{"", mirror.RuntimeClaude, mirror.RuntimeHermes, "opencode"} {
		for _, status := range []string{mirror.StatusIdle, mirror.StatusBusy, mirror.StatusWaiting, ""} {
			for _, un := range []error{nil, unknown} {
				st := mirror.AgentState{Runtime: rt, Status: status, Since: 7, Unknown: un}
				msg, notice := sendNowRefusal(st)
				fmt.Fprintf(&b, "sendnow runtime=%q status=%q unknown=%v -> %q %q\n", rt, status, un != nil, msg, notice)
			}
		}
	}
	fm := queueMirror()
	s, _ := queueServer(t, fm, &memQueue{}, &turnGateway{})
	h := sendqueue.Head{Item: sendqueue.Item{Owner: 1, Target: sendqueue.Target{Host: "ws", Session: "main", Window: "@1"}}}
	for _, rt := range []string{"", mirror.RuntimeClaude, mirror.RuntimeHermes, "opencode"} {
		for _, status := range []string{mirror.StatusIdle, mirror.StatusBusy, mirror.StatusWaiting, ""} {
			fm.mu.Lock()
			fm.agentState = &mirror.AgentState{Runtime: rt, Status: status, Since: 7}
			fm.mu.Unlock()
			got := s.sendQueueProbe(asQueueUser(1, time.Now()), h)
			fmt.Fprintf(&b, "probe runtime=%q status=%q -> idle=%v since=%d why=%q\n", rt, status, got.Idle, got.Since, got.Why)
		}
	}
	for _, hermesSend := range []bool{false, true} {
		fm := &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}, statusReport: true, hermesSend: hermesSend, windows: charAPIWindows()}
		hs := fm.Hosts(hermesUser())
		for _, e := range mirror.Ordered(hs) {
			fmt.Fprintf(&b, "queueoff hermesSend=%v %s -> %q\n", hermesSend, e.Window, mirrorQueueOff(hs[0], e))
		}
	}
	characterizationGolden(t, "send-rules", b.String())
}

// TestCharacterizeTheAttentionItems: which windows are attention items, and
// what each says and offers, on a host that answers approvals and one that
// does not.
func TestCharacterizeTheAttentionItems(t *testing.T) {
	var b strings.Builder
	for _, answers := range []bool{false, true} {
		m := &hermesAskMirror{fakeMirror: &fakeMirror{online: true, interactive: true, statusReport: true, windows: charAPIWindows()}, answers: answers}
		s := New(nil, AuthConfig{}, log.New(io.Discard, "", 0))
		s.UseExtensions(Extensions{Mirror: m})
		for _, it := range s.waitingSessions(hermesUser()) {
			hermes := "nil"
			if it.hermes != nil {
				hermes = fmt.Sprintf("%+v", *it.hermes)
			}
			fmt.Fprintf(&b, "answers=%v item window=%s kind=%q source=%q text=%q detail=%q options=%q link=%q offline=%v tool=%q claudeSession=%q subject=%+v hermes=%s\n",
				answers, it.card.ref.Window, it.Kind, it.Source, it.Text, it.Detail, it.Options, it.Link, it.Offline, it.tool, it.claudeSession, it.subject, hermes)
		}
	}
	characterizationGolden(t, "attention", b.String())
}

var (
	charInput = regexp.MustCompile(`<textarea[^>]*id="mv-input"[^>]*>`)
	charCards = regexp.MustCompile(`href="(/mirror/(?:view|terminal)[^"]*)"|data-f-kind="([^"]*)"|data-mirror-kind="([^"]*)"`)
)

// TestCharacterizeTheRenderedPages: the Agents list's cards and each
// window's rendered-view page — its status, redirect, composer and the
// runtime features it draws — on a host with and without hermesSend. No
// whole-page hash: any UI change would trip it, whatever the runtime.
func TestCharacterizeTheRenderedPages(t *testing.T) {
	var b strings.Builder
	for _, hermesSend := range []bool{false, true} {
		fm := &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}, statusReport: true, hermesSend: hermesSend, windows: charAPIWindows()}
		s, ts := viewServer(t, fm)
		_, list := mirrorGet(t, s, ts, "/ui/agents")
		for _, m := range charCards.FindAllStringSubmatch(list, -1) {
			fmt.Fprintf(&b, "hermesSend=%v list %q\n", hermesSend, m[0])
		}
		for _, w := range charAPIWindows() {
			ref := ui.MirrorRef{Host: "ws", Session: "main", Window: w.ID}
			resp, page := mirrorGet(t, s, ts, ref.ViewPath())
			input := charInput.FindString(page)
			features := []string{}
			for _, f := range []string{ui.SendQueuePath, "Claude is working", "Hermes is working…", charHTML(ui.MirrorHermesNoSendNote), "Message Hermes…",
				`id="mv-log"`, "data-ws=", "Sending to Hermes is not available"} {
				if strings.Contains(page, f) {
					features = append(features, f)
				}
			}
			fmt.Fprintf(&b, "hermesSend=%v view %s status=%d location=%q input=%s features=%q\n", hermesSend, w.ID,
				resp.StatusCode, resp.Header.Get("Location"), input, features)
		}
	}
	characterizationGolden(t, "pages", b.String())
}

func charHTML(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;").Replace(s)
}
