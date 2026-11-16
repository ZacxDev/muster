package ui

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The terminal page's inline script, rendered, for e2e/terminal-gate.test.mjs:
// a Node test that runs it against a stub DOM and a WebSocket whose outcome it
// controls (the terminal's reconnect and step-up rules are JS; this package's
// tests cannot run them). The fixture is GENERATED from AgentTerminalPage and
// this test fails when it no longer matches, so the Node test never runs an
// old script:
//
//	MUSTER_UPDATE_GOLDEN=1 go test ./internal/ui -run TestTheTerminalScriptFixtureIsCurrent
//
// Two pages: a TEAM agent's (StepUp + StepUpAttach, the gated attach) and the
// same page with live keys only behind the step-up (the mirrored shape: the
// control).

var (
	termTagRE    = regexp.MustCompile(`<div id="term"[^>]*>`)
	inlineScript = regexp.MustCompile(`<script>([\s\S]*?)</script>`)
)

// terminalFixture is page's #term tag and its terminal script, as the fixture
// stores them.
func terminalFixture(t *testing.T, v AgentTerminalView) string {
	t.Helper()
	var b bytes.Buffer
	if err := AgentTerminalPage(v).Render(&b); err != nil {
		t.Fatal(err)
	}
	page := b.String()
	tag := termTagRE.FindString(page)
	if tag == "" {
		t.Fatal("no #term element on the terminal page")
	}
	for _, m := range inlineScript.FindAllStringSubmatch(page, -1) {
		if strings.Contains(m[1], "reconnectBtn") {
			return tag + "\n<script>" + m[1] + "</script>\n"
		}
	}
	t.Fatal("no terminal script on the terminal page")
	return ""
}

func TestTheTerminalScriptFixtureIsCurrent(t *testing.T) {
	su := func() *StepUpView {
		return &StepUpView{ConfirmPath: "/auth/confirm", FreshFor: 3_600_000, Window: "12 hours", Why: "Opening a team agent's terminal needs a sign-in from the last 12 hours."}
	}
	for file, v := range map[string]AgentTerminalView{
		"terminal-gated.html":   {Name: "team-chief", StepUp: su(), StepUpAttach: true},
		"terminal-control.html": {Name: "team-chief", StepUp: su()},
	} {
		got := terminalFixture(t, v)
		path := filepath.Join("..", "..", "e2e", "testdata", file)
		if os.Getenv("MUSTER_UPDATE_GOLDEN") == "1" {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil || string(want) != got {
			t.Errorf("%s is not the current terminal script (%v): MUSTER_UPDATE_GOLDEN=1 go test ./internal/ui -run TestTheTerminalScriptFixtureIsCurrent", path, err)
		}
	}
	if fx := terminalFixture(t, AgentTerminalView{Name: "x", StepUp: su(), StepUpAttach: true}); !strings.Contains(fx, `data-step-up-attach=""`) {
		t.Errorf("CONTROL: the gated page's #term carries no data-step-up-attach")
	}
}
