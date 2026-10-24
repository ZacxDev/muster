package mirror_test

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/mirror"
)

// CHARACTERIZATION GOLDENS for the runtime-interface refactor (Claude Code,
// Hermes and the opencode stub behind one runtime interface). They were
// written and recorded BEFORE that refactor, against the per-runtime branches
// it replaces, and must read the same after it: every decision below is a
// place that branched on a runtime's name. They pin behaviour, not intent —
// a golden that changes is a behaviour change, to be justified or reverted,
// never re-recorded to make the suite green.
//
// Every fixture is synthetic.

// TODO(runtime-interface): delete these goldens in the final fix pass after
// review round 1; until then they are the proof that before equals after.

var updateCharacterization = flag.Bool("update-characterization", false, "rewrite testdata/characterization/*.golden")

// golden compares got to testdata/characterization/<name>.golden.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "characterization", name+".golden")
	if *updateCharacterization {
		must(t, os.MkdirAll(filepath.Dir(path), 0o755))
		must(t, os.WriteFile(path, []byte(got), 0o644))
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

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

const charHermesSID = "local-0123456789abcdef0123456789abcdef0123456789abcdef0123456789ab"

// charWindows is one window of every shape a runtime decision branches on.
func charWindows() []mirror.Window {
	approval := &mirror.Pending{Summary: "run ls", Tool: "terminal", Kind: mirror.PendingApproval, Detail: "ls -la",
		PromptID: "p-1", Choices: "once,session,always,deny"}
	claudeAsk := &mirror.Pending{Summary: "Edit a file", Tool: "Edit", Last: "about to edit", Kind: mirror.PendingApproval,
		PromptID: "p-9", Choices: "once"}
	return []mirror.Window{
		{Session: "work", ID: "@1", Index: 1, Active: true, Command: "claude", Claude: true, ClaudeSession: "c-1", Status: mirror.StatusIdle, StatusSince: 1001},
		{Session: "work", ID: "@2", Index: 2, Command: "claude", Claude: true, ClaudeSession: "c-2", Status: mirror.StatusBusy, StatusSince: 1002},
		{Session: "work", ID: "@3", Index: 3, Command: "claude", Claude: true, ClaudeSession: "c-3", Status: mirror.StatusWaiting, StatusSince: 1003, Pending: claudeAsk},
		{Session: "work", ID: "@4", Index: 4, Command: "claude", Claude: true, ClaudeSession: "c-4"},
		{Session: "work", ID: "@5", Index: 5, Command: "python3.14", Runtime: mirror.RuntimeHermes, AgentSession: charHermesSID, Status: mirror.StatusIdle, StatusSince: 1005},
		{Session: "work", ID: "@6", Index: 6, Command: "python3.14", Runtime: mirror.RuntimeHermes, AgentSession: charHermesSID, Status: mirror.StatusBusy, StatusSince: 1006},
		{Session: "work", ID: "@7", Index: 7, Command: "python3.14", Runtime: mirror.RuntimeHermes, AgentSession: charHermesSID, Status: mirror.StatusWaiting, StatusSince: 1007, Pending: approval},
		{Session: "work", ID: "@8", Index: 8, Command: "python3.14", Runtime: mirror.RuntimeHermes},
		{Session: "work", ID: "@9", Index: 9, Command: "python3.14", Claude: true, ClaudeSession: "c-9", Runtime: mirror.RuntimeHermes, AgentSession: charHermesSID, Status: mirror.StatusIdle, StatusSince: 1009},
		{Session: "work", ID: "@10", Index: 10, Command: "opencode", Runtime: "opencode", AgentSession: "ses_abc", Status: mirror.StatusIdle, StatusSince: 1010},
		{Session: "work", ID: "@11", Index: 11, Command: "zz", Runtime: "bogus", AgentSession: "x", Status: mirror.StatusIdle, StatusSince: 1011},
		{Session: "work", ID: "@12", Index: 12, Command: "zsh", Status: mirror.StatusIdle, StatusSince: 1012},
		{Session: "work", ID: "@13", Index: 13, Command: "opencode"},
		{Session: "work", ID: "@14", Index: 14, Command: "python3.14", Runtime: mirror.RuntimeHermes, AgentSession: charHermesSID, Status: mirror.StatusWaiting, StatusSince: 1014,
			Pending: &mirror.Pending{Summary: "no kind"}},
		{Session: "work", ID: "@15", Index: 15, Command: "python3.14", Runtime: mirror.RuntimeHermes, AgentSession: strings.Repeat("a", 129), Status: mirror.StatusIdle, StatusSince: 1015},
		{Session: "work", ID: "@16", Index: 16, Command: "python3.14", Runtime: mirror.RuntimeHermes, AgentSession: charHermesSID, Status: mirror.StatusIdle},
		{Session: "work", ID: "@17", Index: 17, Command: "claude", Claude: true, ClaudeSession: "c-17", Status: mirror.StatusIdle},
	}
}

func jsonOf(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "!" + err.Error()
	}
	return string(b)
}

// TestCharacterizeTheWireRuntimeRules: NormalizeWindows over every shape, and
// what each normalised window answers for its runtime (AgentRuntime,
// SessionKey), plus which stored keys are valid.
func TestCharacterizeTheWireRuntimeRules(t *testing.T) {
	var b strings.Builder
	for _, w := range mirror.NormalizeWindows(charWindows(), 2000) {
		fmt.Fprintf(&b, "%s\n  runtime=%q key=%q valid=%v\n", jsonOf(w), w.AgentRuntime(), w.SessionKey(), mirror.ValidSessionKey(w.SessionKey()))
	}
	for _, k := range []string{"c-1", strings.Repeat("c", 64), strings.Repeat("c", 65), "hermes:" + charHermesSID, "hermes:" + strings.Repeat("a", 128),
		"hermes:" + strings.Repeat("a", 129), "hermes:", "opencode:ses_abc", "claude:c-1", "bogus:x", charHermesSID, ""} {
		fmt.Fprintf(&b, "key %q valid=%v\n", k, mirror.ValidSessionKey(k))
	}
	fmt.Fprintf(&b, "SessionKeyPattern %s\nHermesKeyPrefix %q\n", mirror.SessionKeyPattern, mirror.HermesKeyPrefix)
	golden(t, "wire", b.String())
}

// charHello is a v2 hello listing charWindows, with extra flags.
func charHello(host, extra string) string {
	return `{"type":"hello","version":2,"host":"` + host + `","interactive":true,"panePaste":true,"statusReport":true` + extra +
		`,"windows":` + jsonOf(charWindows()) + `}`
}

func errText(err error) string {
	if err == nil {
		return "nil"
	}
	var pr *mirror.PasteRefusedError
	kind := ""
	if errors.As(err, &pr) {
		kind = " refusal=" + pr.Refusal
	}
	flags := []string{}
	for _, e := range []struct {
		name string
		e    error
	}{{"refused", mirror.ErrPasteRefused}, {"unconfirmed", mirror.ErrPasteUnconfirmed}, {"superseded", mirror.ErrSuperseded},
		{"notclaude", mirror.ErrNotClaude}, {"norendered", mirror.ErrNoRenderedView}, {"hermesnotidle", mirror.ErrHermesNotIdle},
		{"hermessendoff", mirror.ErrHermesSendOff}} {
		if errors.Is(err, e.e) {
			flags = append(flags, e.name)
		}
	}
	return fmt.Sprintf("%q%s is=%v", err.Error(), kind, flags)
}

// TestCharacterizeTheHubsRuntimeDecisions: for every window shape, on a host
// with and without hermesSend / hermesAnswer, what the hub decides — the
// Hosts() view (and the SessionView answers built on it), AgentState, a
// paste by each target (and a queued one), a rendered view's start, and an
// approval answer — and exactly what reached the host.
func TestCharacterizeTheHubsRuntimeDecisions(t *testing.T) {
	var b strings.Builder
	for _, hc := range []struct{ host, extra string }{
		{"plain", ""},
		{"gated", `,"hermesSend":true,"hermesAnswer":true`},
	} {
		r := newRig(t)
		tok, _ := r.store.mint(t, 1, hc.host)
		h := connectScripted(t, r, tok, charHello(hc.host, hc.extra), okReply)
		r.online(t, 1, hc.host, "work")
		fmt.Fprintf(&b, "== host %s%s\n", hc.host, hc.extra)
		hosts := r.svc.Hosts(asUser(1))
		for _, hv := range hosts {
			fmt.Fprintf(&b, "hostview %s online=%v interactive=%v rendered=%v hermesSend=%v hermesAnswer=%v panePaste=%v claude=%q\n",
				hv.Host, hv.Online, hv.Interactive, hv.Rendered, hv.HermesSend, hv.HermesAnswer, hv.PanePaste, hv.Claude)
			for _, w := range hv.Windows {
				fmt.Fprintf(&b, "  listed %s\n", jsonOf(w))
			}
		}
		for _, e := range mirror.Ordered(hosts) {
			fmt.Fprintf(&b, "view %s kind=%q hermesView=%v key=%q canPaste=%v runtime=%q agentSession=%q claude=%v\n",
				e.Window, e.Kind(), e.HermesView(), e.SessionKey(), e.CanPaste(), e.Runtime, e.AgentSession, e.Claude)
		}
		ids, since := []string{}, map[string]int64{}
		for _, w := range charWindows() {
			ids = append(ids, w.ID)
			since[w.ID] = w.StatusSince
		}
		for _, id := range ids {
			st := r.svc.AgentState(asUser(1), hc.host, "work", id)
			un := "nil"
			if st.Unknown != nil {
				un = st.Unknown.Error()
			}
			fmt.Fprintf(&b, "agentstate %s runtime=%q status=%q since=%d unknown=%q\n", id, st.Runtime, st.Status, st.Since, un)
		}
		for _, id := range ids {
			for _, c := range []struct {
				name   string
				target mirror.PasteTarget
				opts   []mirror.PasteOption
			}{
				{"claude", mirror.PasteClaude, nil},
				{"anypane", mirror.PasteActivePane, nil},
				{"queued", mirror.PasteClaude, []mirror.PasteOption{mirror.ExpectTurn(mirror.Turn{Since: since[id]})}},
			} {
				ctx, cancel := context.WithTimeout(asUser(1), 3*time.Second)
				before := len(h.got())
				err := r.svc.PasteWindow(ctx, hc.host, "work", id, "hi "+id, c.target, c.opts...)
				cancel()
				got := h.got()[before:]
				reached := "nothing"
				if len(got) > 0 {
					m := got[0]
					reached = fmt.Sprintf("window=%s runtime=%q since=%d anyPane=%v", m.Window, m.Runtime, m.Since, m.AnyPane)
				}
				fmt.Fprintf(&b, "paste %s %s err=%s host<-%s\n", id, c.name, errText(err), reached)
			}
		}
		for _, id := range ids {
			ctx, cancel := context.WithTimeout(asUser(1), 150*time.Millisecond)
			err := r.svc.WatchWindow(ctx, hc.host, "work", id, "", 0, func(mirror.Msg) error { return nil })
			cancel()
			started := "refused " + errText(err)
			if err == nil || errors.Is(err, context.DeadlineExceeded) {
				started = "started"
			}
			fmt.Fprintf(&b, "watch %s %s\n", id, started)
		}
		for _, id := range ids {
			ctx, cancel := context.WithTimeout(asUser(1), 300*time.Millisecond)
			err := r.svc.AnswerHermesApproval(ctx, hc.host, "work", id, "p-1", "once")
			cancel()
			fmt.Fprintf(&b, "answer %s err=%s\n", id, errText(err))
		}
	}
	golden(t, "hub", b.String())
}

// sortedKeys is m's keys, sorted.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
