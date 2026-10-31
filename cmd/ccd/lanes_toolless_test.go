package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZacxDev/muster/internal/agents"
)

// TOOL-LESS CHECK LANES (X-Muster-CC-Toolless: 1, lanes.go "A CHECK LANE HAS NO
// TOOLS"): a lane muster marks tool-less starts its CLI with toollessArgs — no
// built-in tool, no MCP server — and no other CLI ever does.

// TestTheToollessHeaderIsMusters: the header ccd reads is the one muster's
// check route sends. A rename on either side would start every check's CLI
// with the chief's full tool set, silently.
func TestTheToollessHeaderIsMusters(t *testing.T) {
	if headerCCToolless != agents.HeaderCCToolless {
		t.Fatalf("ccd reads a tool-less lane as %q, muster marks it %q", headerCCToolless, agents.HeaderCCToolless)
	}
}

// The rollout check an operator does by hand after restarting a chief: ccd's
// /healthz says `"toolless_lanes": true` — read here as raw JSON from ccd's own
// handler, the way an operator's curl sees it — whenever it runs lanes, and
// says nothing when it runs none.
func TestHealthzSaysItHonoursToollessLanes(t *testing.T) {
	srv, _ := newScripted(t, cliScript{})
	url := gatewayServer(t, srv).URL
	read := func() (bool, bool) {
		t.Helper()
		_, body, _ := getHealth(t, url, "/healthz")
		var raw map[string]any
		if err := json.Unmarshal([]byte(body), &raw); err != nil {
			t.Fatalf("/healthz body: %v", err)
		}
		v, present := raw["toolless_lanes"]
		return v == true, present
	}
	if v, present := read(); v || present {
		t.Errorf("a ccd with no lanes reports toolless_lanes (%v, present=%v)", v, present)
	}
	srv.lanes = testLanes(t, 3)
	if v, _ := read(); !v {
		t.Error(`a ccd that runs lanes does not report "toolless_lanes": true — the operator's rollout check would read it as an old ccd`)
	}
}

// TestToollessArgsAreTheZeroToolFlags pins the flags themselves, literally:
// `--tools=` (the built-in allow-list, empty) and `--strict-mcp-config` (the
// MCP allow-list: only --mcp-config's servers, and none is passed). Measured on
// the pinned CLI (images/claude-code-agent, 2.1.296): each alone leaves the
// other half of the tools in place, so dropping either is a regression.
func TestToollessArgsAreTheZeroToolFlags(t *testing.T) {
	if want := []string{"--tools=", "--strict-mcp-config"}; !slices.Equal(toollessArgs, want) {
		t.Fatalf("toollessArgs %q, want %q", toollessArgs, want)
	}
}

// 🔴 ONLY A TOOL-LESS LANE'S CLI STARTS WITH THE FLAGS, on every start. Through
// acquireIn (which sets the lane's mode) the WHOLE argv is compared, so the
// flags cannot be dropped, moved behind --session-id/--resume, or duplicated
// unnoticed: an operator lane has none; a tool-less check lane has both, on its
// first start and when it resumes its transcript; a tool-less lane that is not
// ephemeral has them too (the two properties are independent); and a job lane
// on a bypass pod keeps its permission flag beside them.
func TestOnlyAToollessLaneStartsWithNoTools(t *testing.T) {
	const wt = "/data/workspace/.muster/jobs/41"
	for _, c := range []struct {
		name, key, model, workdir string
		mode                      laneMode
		bypass, resumed           bool
		want                      string
	}{
		{name: "operator lane", key: "sess-a", model: "opus",
			want: "env CCD_LANE=%id claude --model opus --session-id %uuid"},
		{name: "check lane, fresh", key: "agent:chief:webchat:nudge:0123456789abcdef", mode: laneMode{ephemeral: true, toolless: true},
			want: "env CCD_LANE=%id claude --tools= --strict-mcp-config --session-id %uuid"},
		{name: "check lane, resumed", key: "agent:chief:webchat:nudge:0123456789abcdef", mode: laneMode{ephemeral: true, toolless: true}, resumed: true,
			want: "env CCD_LANE=%id claude --tools= --strict-mcp-config --resume %uuid"},
		{name: "ephemeral only", key: "agent:chief:webchat:nudge:1111111111111111", mode: laneMode{ephemeral: true},
			want: "env CCD_LANE=%id claude --session-id %uuid"},
		{name: "toolless only", key: "sess-t", model: "haiku", mode: laneMode{toolless: true},
			want: "env CCD_LANE=%id claude --model haiku --tools= --strict-mcp-config --session-id %uuid"},
		{name: "job lane on a bypass pod, toolless", key: "job-41", workdir: wt, bypass: true, mode: laneMode{toolless: true},
			want: "env CCD_LANE=%id claude --permission-mode bypassPermissions --tools= --strict-mcp-config --session-id %uuid"},
		{name: "job lane on a bypass pod", key: "job-42", workdir: wt, bypass: true,
			want: "env CCD_LANE=%id claude --permission-mode bypassPermissions --session-id %uuid"},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := testLanes(t, 3)
			l.jobBypass = c.bypass
			if c.resumed {
				dir := projectDir(l.configDir, l.workspace)
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, laneUUID(c.key)+".jsonl"), []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ln, f := l.acquireIn(context.Background(), c.key, c.model, c.workdir, c.mode)
			if f != nil {
				t.Fatalf("acquire: %v", f)
			}
			defer l.release(ln)
			argv, _ := ln.sup.argv()
			want := strings.NewReplacer("%id", laneID(c.key), "%uuid", laneUUID(c.key)).Replace(c.want)
			if got := strings.Join(argv, " "); got != want {
				t.Errorf("argv\n got  %s\n want %s", got, want)
			}
			var h laneHealth
			for _, x := range l.health() {
				if x.ID == laneID(c.key) {
					h = x
				}
			}
			if h.ID == "" || h.Toolless != c.mode.toolless {
				t.Errorf("/healthz lane %+v; want toolless=%v", h, c.mode.toolless)
			}
		})
	}
}

// 🔴 A LANE'S TOOL-LESSNESS IS FIXED FOR ITS LIFE. A turn whose headers
// disagree with the running lane is refused (400) and never pasted into it —
// above all a tool-less turn must never reach a CLI that has tools. Both
// directions, with the lane's ephemerality held equal so this refusal is the
// one that fires.
func TestALanesToollessnessIsFixed(t *testing.T) {
	for _, c := range []struct {
		name          string
		running, turn laneMode
	}{
		{"a tool-less turn for a CLI with tools", laneMode{ephemeral: true}, laneMode{ephemeral: true, toolless: true}},
		{"a turn with tools for a tool-less CLI", laneMode{ephemeral: true, toolless: true}, laneMode{ephemeral: true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := testLanes(t, 3)
			key := "agent:chief:webchat:nudge:2222222222222222"
			ln, f := l.acquireIn(context.Background(), key, "", "", c.running)
			if f != nil {
				t.Fatalf("precondition: %v", f)
			}
			defer l.release(ln)
			got, f := l.acquireIn(context.Background(), key, "", "", c.turn)
			if f == nil || f.Status != http.StatusBadRequest || !strings.Contains(f.Message, "toolless") {
				t.Fatalf("got lane %v, failure %v; want a 400 naming toolless", got, f)
			}
			if r := reservedOf(l, ln.id); r != 1 {
				t.Errorf("the refused turn left the lane reserved %d times, want 1 (the first turn's)", r)
			}
		})
	}
}

// argvPane is a stillPane that records the argv its CLI was started with.
type argvPane struct {
	stillPane
	mu   sync.Mutex
	argv []string
}

func (p *argvPane) Start(_ context.Context, _ string, argv []string) (paneRef, error) {
	p.mu.Lock()
	p.argv = append([]string(nil), argv...)
	p.mu.Unlock()
	return fakeRef, nil
}

func (p *argvPane) started() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.argv
}

// 🔴 THE HEADER IS WHAT MAKES A LANE TOOL-LESS, through ccd's own handler: a
// turn carrying X-Muster-CC-Toolless: 1 starts its CLI with toollessArgs; one
// without it, or with any other value, starts a CLI with the usual tools. The
// argv is the one tmux was handed (the terminal's Start), not a re-derivation.
// No pane here sends SessionStart, so every turn ends 503 not_ready — after its
// CLI was started, which is all this asserts.
func TestTheToollessHeaderStartsAToollessCLI(t *testing.T) {
	r := newLaneRig(t, 3)
	panes := map[string]*argvPane{}
	var mu sync.Mutex
	r.l.newTerm = func(target string) laneTerminal {
		p := &argvPane{}
		mu.Lock()
		panes[target] = p
		mu.Unlock()
		return p
	}
	turn := func(key, toolless string) []string {
		t.Helper()
		req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"input":"is this session stalled?","stream":false}`))
		req.Header.Set("Authorization", "Bearer b")
		req.Header.Set(headerCCSession, key)
		if toolless != "" {
			req.Header.Set(headerCCToolless, toolless)
		}
		w := httptest.NewRecorder()
		r.s.gatewayHandler().ServeHTTP(w, req)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("precondition: %s's turn answered %d %s, want 503 not_ready", key, w.Code, w.Body.String())
		}
		mu.Lock()
		p := panes["cc-"+laneID(key)]
		mu.Unlock()
		if p == nil {
			t.Fatalf("precondition: %s's turn started no lane", key)
		}
		deadline := time.Now().Add(10 * time.Second)
		for p.started() == nil {
			if time.Now().After(deadline) {
				t.Fatalf("precondition: %s's lane never started its CLI", key)
			}
			time.Sleep(time.Millisecond)
		}
		return p.started()
	}
	hasFlags := func(argv []string) bool {
		return slices.Contains(argv, "--tools=") || slices.Contains(argv, "--strict-mcp-config")
	}
	if argv := turn("check-1", "1"); len(argv) < 4 || !slices.Equal(argv[len(argv)-4:len(argv)-2], toollessArgs) {
		t.Errorf("X-Muster-CC-Toolless: 1 started %q; want %q before the session flag", argv, toollessArgs)
	}
	for _, c := range []struct{ key, v string }{{"operator-1", ""}, {"operator-2", "true"}, {"operator-3", "0"}} {
		if argv := turn(c.key, c.v); hasFlags(argv) {
			t.Errorf("X-Muster-CC-Toolless %q started a TOOL-LESS CLI %q; only \"1\" may", c.v, argv)
		}
	}
}

// 🔴 A RESTARTED TOOL-LESS CLI IS STILL TOOL-LESS (lanes.go: "Read on every
// start, so a restart or a resume is tool-less too"). A lane's CLI killed with
// no SessionEnd (SIGKILL, OOM) is respawned by its supervisor; the command line
// the terminal is handed for that RESPAWN — not a re-derivation — must carry
// the flags exactly as the first start did, and an operator lane's respawn must
// still carry none. The whole argv of both starts is compared literally.
func TestARespawnedToollessLaneIsStillToolless(t *testing.T) {
	for _, c := range []struct {
		name, key string
		mode      laneMode
		want      string
	}{
		{name: "check lane", key: "agent:chief:webchat:nudge:3333333333333333", mode: laneMode{ephemeral: true, toolless: true},
			want: "env CCD_LANE=%id claude --tools= --strict-mcp-config --session-id %uuid"},
		{name: "operator lane", key: "sess-r",
			want: "env CCD_LANE=%id claude --session-id %uuid"},
	} {
		t.Run(c.name, func(t *testing.T) {
			pane := newKillablePane()
			l := testLanes(t, 1)
			l.newTerm = func(string) laneTerminal { return pane }
			l.tweak = fastRestarts
			ln, f := l.acquireIn(context.Background(), c.key, "", "", c.mode)
			if f != nil {
				t.Fatalf("acquire: %v", f)
			}
			t.Cleanup(ln.stop)
			waitUntil(t, "the first start", func() bool { _, ok := ln.sup.inputPane(); return ok })
			killAndRespawn(t, pane, ln.sup)
			l.release(ln)

			want := strings.NewReplacer("%id", laneID(c.key), "%uuid", laneUUID(c.key)).Replace(c.want)
			pane.mu.Lock()
			argvs := slices.Clone(pane.argvs)
			pane.mu.Unlock()
			if len(argvs) != 2 {
				t.Fatalf("precondition: %d starts recorded (%q), want the first start and one respawn", len(argvs), argvs)
			}
			for i, what := range []string{"first start", "respawn"} {
				if got := strings.Join(argvs[i], " "); got != want {
					t.Errorf("%s argv\n got  %s\n want %s", what, got, want)
				}
			}
		})
	}
}
