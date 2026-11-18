package api

import (
	"errors"
	"fmt"
	"html"
	"regexp"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/cctranscript"
	"github.com/ZacxDev/muster/internal/mirror"
	"github.com/ZacxDev/muster/internal/ui"
)

// PLAIN HOST MESSAGES (operator rule: muster is for people who are not
// engineers, so its own chrome shows no flag, raw error string or token
// counter in its top layer). A session page says what a computer can and
// cannot do in a plain sentence; the flag that changes it, or the exact
// message a host sent, is in the ⓘ details sheet (ui.InfoSheet) beside it.

// hostJargon is what a session page's top layer must not hold: a command-line
// flag ("-interactive", "-opencode-send": a dash starting a word), the hub's
// raw "mirror:" prefix, the host binary's name, a quoted id the hub appends
// ("(host "atlas", …)"), a token counter ("~+140 ctx", "+3.3k ctx"), and the
// tool-call count's own words ("2 tool calls").
var hostJargon = regexp.MustCompile(`(^|[\s(])--?[a-z][a-z-]*\b|\bmirror: |muster-mirror|\(host "|\bctx\b|\btool calls?\b`)

// assertNoHostJargon fails for every top-layer text of markup (topLayerTexts:
// text, title, aria-label and placeholder, never an ⓘ sheet, a script or a
// data-* attribute) that hostJargon matches.
func assertNoHostJargon(t *testing.T, what, markup string) {
	t.Helper()
	texts := topLayerTexts(t, markup)
	if len(texts) == 0 {
		t.Fatalf("%s: CONTROL: no top-layer text at all — the scan sees nothing", what)
	}
	for _, s := range texts {
		if hostJargon.MatchString(s) {
			t.Errorf("%s: the top layer says %q", what, s)
		}
	}
}

// assertPlainLine fails when a line a script puts on the page (a socket's
// error, a send's refusal) holds host jargon or #806's address jargon.
func assertPlainLine(t *testing.T, what, line string) {
	t.Helper()
	if line == "" {
		t.Errorf("%s: CONTROL: an empty line says nothing", what)
	}
	if hostJargon.MatchString(line) || jargonInTopLayer.MatchString(line) {
		t.Errorf("%s: the page says %q", what, line)
	}
}

// TestTheHostJargonScanSeesAFlagAMirrorErrorAndACounter: the scan's own
// controls — it finds a flag, a raw "mirror:" error, the binary's name, a
// quoted host id, a token counter and "tool calls" in a text, a tooltip and
// an aria-label (POSITIVE), and ignores them inside an ⓘ sheet, a script and
// a data attribute, and passes plain words that hold a hyphen (NEGATIVE).
func TestTheHostJargonScanSeesAFlagAMirrorErrorAndACounter(t *testing.T) {
	for _, bad := range []string{
		`<p>This host is read-only — its muster was not started with -interactive.</p>`,
		`<p>it needs -interactive and -opencode-send</p>`,
		`<div role="alert"><span>mirror: the host is online but no longer lists this tmux session</span></div>`,
		`<p>ended (host "atlas", session "nosuch")</p>`,
		`<span title="Restart muster-mirror">x</span>`,
		`<span aria-label="~+140 ctx">x</span>`,
		`<span>+3.3k ctx</span>`,
		`<summary><span>2 tool calls</span></summary>`,
		`<input placeholder="run it with --server">`,
	} {
		hit := false
		for _, s := range topLayerTexts(t, bad) {
			hit = hit || hostJargon.MatchString(s)
		}
		if !hit {
			t.Errorf("the scan missed the jargon in %s", bad)
		}
	}
	ok := `<p>This computer is view-only, so you cannot type into it from here.</p>` +
		`<div data-info-sheet=""><p>Restart its muster-mirror with this setting:</p><code>-interactive -opencode-send</code></div>` +
		`<script>var e="mirror: x (host \"a\")"</script>` +
		`<span data-tool-ctx-text="~+140 ctx" hidden=""></span><p>2 steps · up-to-date · re-run</p>`
	for _, s := range topLayerTexts(t, ok) {
		if hostJargon.MatchString(s) {
			t.Errorf("the scan flagged %q in %s", s, ok)
		}
	}
}

// readOnlyHost is a host that did not advertise -interactive, listing one
// Claude Code window and one opencode window.
func readOnlyHost(interactive bool) *fakeMirror {
	return &fakeMirror{online: true, interactive: interactive, v2: &fakeMirrorV2{}, statusReport: true, windows: []mirror.Window{
		{Session: "main", ID: "@1", Index: 1, Active: true, Claude: true, Command: "claude",
			ClaudeSession: "5a1b2c3d-4444-4444-8444-0000000000c1", Status: mirror.StatusIdle, StatusSince: 7},
		{Session: "main", ID: "@2", Index: 2, Command: "opencode", Runtime: mirror.RuntimeOpenCode, AgentSession: "ses_0fixture1a2b",
			Status: mirror.StatusIdle, StatusSince: 7},
	}}
}

// sessionBody is a session page's <body>: everything a person can read.
func sessionBody(t *testing.T, page string) string {
	t.Helper()
	i, j := strings.Index(page, "<body"), strings.LastIndex(page, "</body>")
	if i < 0 || j < i {
		t.Fatalf("no <body> in:\n%s", page)
	}
	return page[i : j+len("</body>")]
}

// TestNoTopLayerOfASessionPageSpellsAHostMessage: the session pages of a
// view-only computer — its Claude Code window's Rendered view and Terminal
// view, and its opencode window's Rendered view on a computer that does not
// send to opencode — say so in plain words, every banner, status row,
// composer note, placeholder and tooltip of their <body>: no flag, no
// "mirror:", no binary name. The flag is in the composer's ⓘ.
//
// REGRESSION TEST for the jargon audit's F006/F012/F014/F015/F016/U-08/U-09/
// U-10/U-11: red at origin/main ("This host is read-only — its muster-mirror
// was not started with -interactive…", "Not sent: this host's muster-mirror
// does not send to opencode (it needs -interactive and -opencode-send)…",
// "Live keys type into a tmux client…", "Read-only host").
func TestNoTopLayerOfASessionPageSpellsAHostMessage(t *testing.T) {
	s, ts := viewServer(t, readOnlyHost(false))
	for name, path := range map[string]string{
		"the Rendered view of a view-only computer": ref("main", "@1").ViewPath(),
		"the Terminal view of a view-only computer": ref("main", "@1").TerminalPath(),
	} {
		resp, page := mirrorGet(t, s, ts, path)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", name, resp.StatusCode)
		}
		body := sessionBody(t, page)
		assertNoHostJargon(t, name, body)
		// It says why, in words, and the ⓘ beside it holds the setting: the
		// viewer is the password operator, an admin (a member's is withheld:
		// ui's TestAViewOnlyNotesInfoShowsAnAdminTheSettingAndOthersAskYourAdmin).
		if !strings.Contains(body, html.EscapeString(ui.MirrorReadOnlyNote)) {
			t.Errorf("%s: the composer does not say the computer is view-only", name)
		}
		note := body[strings.Index(body, `id="mv-readonly"`):]
		if !strings.Contains(note, `id="mv-readonly-info-open"`) || !strings.Contains(note, `data-info-code="">-interactive<`) {
			t.Errorf("%s: no ⓘ holding the setting beside the view-only note:\n%.2000s", name, note)
		}
	}
	// An opencode window on an -interactive computer that does not send to
	// opencode: its composer is disabled saying so plainly.
	s2, ts2 := viewServer(t, readOnlyHost(true))
	resp, page := mirrorGet(t, s2, ts2, ref("main", "@2").ViewPath())
	if resp.StatusCode != 200 {
		t.Fatalf("the opencode view: %d", resp.StatusCode)
	}
	body := sessionBody(t, page)
	assertNoHostJargon(t, "the opencode Rendered view", body)
	if !strings.Contains(body, html.EscapeString(mirror.PasteRefusedOpenCodeOffText)) || !strings.Contains(body, `id="mv-readonly-info-open"`) {
		t.Errorf("the opencode view does not say, with an ⓘ, that replying is not turned on")
	}
}

// TestASessionPagesSocketErrorsArePlainWithTheExactTextInTheDetail: every
// refusal a Rendered or Terminal view's socket can end with is told to the
// page in plain words (the banner's line) and the exact error as its detail
// (the banner's ⓘ) — never the error itself on the line.
//
// REGRESSION TEST for the jargon audit's F009: red at origin/main, whose
// banner said "mirror: the host is online but no longer lists this tmux
// session (host "atlas", session "nosuch")".
func TestASessionPagesSocketErrorsArePlainWithTheExactTextInTheDetail(t *testing.T) {
	gone := fmt.Errorf("%w (host %q, session %q)", mirror.ErrSessionGone, "atlas", "nosuch")
	for _, err := range []error{
		gone,
		fmt.Errorf("%w (host %q, session %q, window %q)", mirror.ErrWindowGone, "atlas", "nosuch", "@9"),
		fmt.Errorf("%w (host %q)", mirror.ErrHostOffline, "atlas"),
		fmt.Errorf("%w (host %q)", mirror.ErrInteractiveRefused, "atlas"),
		fmt.Errorf("%w (host %q)", mirror.ErrTooManyStreams, "atlas"),
		fmt.Errorf("%w (host %q)", mirror.ErrTooManyWatches, "atlas"),
		fmt.Errorf("%w (host %q)", mirror.ErrNoRenderedView, "atlas"),
		fmt.Errorf("%w (host %q, session %q)", mirror.ErrNotClaude, "atlas", "nosuch"),
		mirror.ErrHostDisconnected, mirror.ErrOverrun, mirror.ErrViewOverrun, mirror.ErrPasteTimeout,
		mirror.ErrScrollPaneInMode, mirror.ErrCatalogTimeout, mirror.ErrStepUpRequired,
	} {
		for page, words := range map[string]func(error, string) (string, string){
			"terminal": terminalErrorWords, "rendered view": viewErrorWords,
		} {
			msg, detail := words(err, "atlas")
			assertPlainLine(t, page+" of "+err.Error(), msg)
			if detail != err.Error() {
				t.Errorf("%s of %v: the detail is %q, want the exact error", page, err, detail)
			}
		}
	}
	// Pinned literally: the audit's own example.
	if msg, _ := terminalErrorWords(gone, "atlas"); msg != "This session has ended on atlas." {
		t.Errorf("a gone session's line: %q", msg)
	}
	// An error the view has no words for is plain too, its text the detail.
	other := errors.New("mirror: something odd (host \"atlas\")")
	if msg, detail := viewErrorWords(other, "atlas"); msg != MirrorViewEndedPlain || detail != other.Error() {
		t.Errorf("an unknown view error: %q / %q", msg, detail)
	}
}

// TestEveryComposerRefusalAboutAComputerIsPlain: the composer's status line —
// a refusal the script shows inline, and the notice a page posted without
// script comes back with — says what the computer cannot do without a flag,
// the binary's name or a raw "mirror:" error.
//
// REGRESSION TEST for the jargon audit's F016/U-08 (and the hub's refusals the
// same line drew raw): red at origin/main.
func TestEveryComposerRefusalAboutAComputerIsPlain(t *testing.T) {
	for _, code := range []string{noticeOffline, noticeGone, noticeWindowGone, noticeReadOnly, noticeNotClaude,
		noticeUpgrade, noticeOpenCodeOff} {
		n, ok := composerNotices[code]
		if !ok {
			t.Fatalf("CONTROL: no notice %q", code)
		}
		assertPlainLine(t, "notice "+code, n.text)
	}
	assertPlainLine(t, "the queue's view-only refusal", notSent(mirror.ErrInteractiveRefused))
	if got := composerNotices[noticeGone].text; got != "Not sent: this session has ended on this computer." {
		t.Errorf("the gone notice: %q", got)
	}
}

// TestAToolRowDrawsNoTokenCounterAndAGroupCountsSteps: a tool row whose
// context cost is known draws no "+3.3k ctx" — no text, no tooltip — keeping
// the measurement only as data; a run of calls is "N steps", not "N tool
// calls".
//
// REGRESSION TEST for the jargon audit's F019/U-05/U-06: red at origin/main
// ("+3.3k ctx" with a "Measured: the context grew…" tooltip; "2 tool calls").
func TestAToolRowDrawsNoTokenCounterAndAGroupCountsSteps(t *testing.T) {
	var tr ui.MirrorTranscript
	var b strings.Builder
	for _, evs := range [][]cctranscript.Event{
		{{Kind: cctranscript.KindToolCall, ToolID: "toolu_a", Tool: "Bash", Target: "ls", Input: `{"command":"ls"}`, Bytes: 16,
			Usage: &cctranscript.Usage{Tokens: 25731}}},
		{{Kind: cctranscript.KindToolCall, ToolID: "toolu_b", Tool: "Bash", Target: "pwd", Input: `{"command":"pwd"}`, Bytes: 17,
			Usage: &cctranscript.Usage{Tokens: 25731}}},
		{{Kind: cctranscript.KindToolResult, ToolID: "toolu_a", OK: true, Output: "out", Bytes: 2600},
			{Kind: cctranscript.KindToolResult, ToolID: "toolu_b", OK: true, Output: "out", Bytes: 900}},
		{{Kind: cctranscript.KindText, Text: "Done.", Usage: &cctranscript.Usage{Tokens: 29030}}},
	} {
		for _, op := range tr.Ops(evs) {
			b.WriteString(op.HTML)
		}
	}
	got := b.String()
	// CONTROL: the measurement was made (it is in the data), so a missing
	// counter is not a missing measurement.
	if !strings.Contains(got, `data-tool-ctx="split"`) || !strings.Contains(got, `data-tool-ctx-tokens="`) {
		t.Fatalf("CONTROL: no measured cost in the rows:\n%s", got)
	}
	assertNoHostJargon(t, "the tool rows", got)
	if !strings.Contains(got, `data-tool-ctx-text="~+`) {
		t.Errorf("the rows lost the count as data:\n%s", got)
	}
	if !strings.Contains(got, `>2 steps<`) {
		t.Errorf("the group does not count 2 steps:\n%s", got)
	}
}
