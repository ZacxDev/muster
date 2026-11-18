package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ZacxDev/muster/internal/authz"
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
// flag ("-interactive", "-opencode-send": a dash starting a word, bare or
// inside backticks or quotes — "`-interactive`", "\"-s\""), the hub's
// raw "mirror:" prefix, the host binary's name, a quoted id the hub appends
// ("(host "atlas", …)"), a token counter ("~+140 ctx", "+3.3k ctx"), and the
// tool-call count's own words ("2 tool calls").
var hostJargon = regexp.MustCompile("(^|[\\s(`\"'“‘])--?[a-z][a-z-]*\\b|\\bmirror: |muster-mirror|\\(host \"|\\bctx\\b|\\btool calls?\\b|\\bdetails have more\\b")

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
		"<p>restart it with `-interactive`</p>",
		`<p>this opencode was started on one session (its "-s")</p>`,
		`<p>its ‘-opencode-send’ is off</p>`,
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
		`<span data-tool-ctx="split" data-tool-ctx-tokens="140" hidden=""></span><p>2 steps · up-to-date · re-run</p>`
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
		if !strings.Contains(body, html.EscapeString(ui.MirrorReadOnlyNote("ws"))) {
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
// refusal a Rendered or Terminal view's socket can end with — and an error
// it has no words for — is told to the page in plain words (the banner's
// line); the exact error is the detail (the banner's ⓘ) for an admin only,
// and a member's frame carries none.
//
// REGRESSION TEST for the jargon audit's F009: red at origin/main, whose
// banner said "mirror: the host is online but no longer lists this tmux
// session (host "atlas", session "nosuch")".
func TestASessionPagesSocketErrorsArePlainWithTheExactTextInTheDetail(t *testing.T) {
	gone := fmt.Errorf("%w (host %q, session %q)", mirror.ErrSessionGone, "atlas", "nosuch")
	other := errors.New("mirror: something odd (host \"atlas\", -interactive)")
	for _, err := range []error{
		gone,
		fmt.Errorf("%w (host %q, session %q, window %q)", mirror.ErrWindowGone, "atlas", "nosuch", "@9"),
		fmt.Errorf("%w (host %q)", mirror.ErrHostOffline, "atlas"),
		fmt.Errorf("%w (host %q)", mirror.ErrInteractiveRefused, "atlas"),
		fmt.Errorf("%w (host %q)", mirror.ErrTooManyStreams, "atlas"),
		fmt.Errorf("%w (host %q)", mirror.ErrTooManyWatches, "atlas"),
		fmt.Errorf("%w (host %q)", mirror.ErrNoRenderedView, "atlas"),
		fmt.Errorf("%w (host %q)", mirror.ErrScrollUnsupported, "atlas"),
		fmt.Errorf("%w (host %q, session %q)", mirror.ErrNotClaude, "atlas", "nosuch"),
		mirror.ErrHostDisconnected, mirror.ErrOverrun, mirror.ErrViewOverrun, mirror.ErrPasteTimeout,
		mirror.ErrScrollPaneInMode, mirror.ErrCatalogTimeout, mirror.ErrStepUpRequired,
		other, // no words for it: the terminal's and the view's own plain line
	} {
		for page, words := range map[string]func(error, string, bool) (string, string){
			"terminal": terminalErrorWords, "rendered view": viewErrorWords,
		} {
			msg, detail := words(err, "atlas", true)
			assertPlainLine(t, page+" of "+err.Error(), msg)
			if detail != err.Error() {
				t.Errorf("%s of %v: an admin's detail is %q, want the exact error", page, err, detail)
			}
			if mmsg, mdetail := words(err, "atlas", false); mmsg != msg || mdetail != "" {
				t.Errorf("%s of %v: a member's frame is %q / %q, want the same line and NO detail", page, err, mmsg, mdetail)
			}
		}
	}
	// Pinned literally.
	if msg, _ := terminalErrorWords(gone, "atlas", true); msg != "atlas no longer shows this session. It may have ended or been renamed." {
		t.Errorf("a gone session's line: %q", msg)
	}
	if msg, _ := viewErrorWords(other, "atlas", true); msg != MirrorViewEndedPlain {
		t.Errorf("an unknown view error: %q", msg)
	}
	if msg, _ := terminalErrorWords(other, "atlas", true); msg != MirrorTerminalEndedPlain {
		t.Errorf("an unknown terminal error of a mirrored session: %q", msg)
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
	for _, err := range []error{mirror.ErrInteractiveRefused, mirror.ErrWindowGone, mirror.ErrHostOffline} {
		assertPlainLine(t, "the queue's refusal "+err.Error(), notSent(err, "atlas"))
	}
	// A notice about the computer names it, when the page knows it.
	for code, want := range map[string]string{
		noticeGone:     "Not sent: atlas no longer shows this session. It may have ended or been renamed.",
		noticeReadOnly: "Not sent: atlas is view-only, so you cannot type into it from here.",
		noticeOffline:  "Not sent: atlas is not connected to muster right now.",
	} {
		if got := noticeText(code, "atlas"); got != want {
			t.Errorf("notice %s on atlas: %q, want %q", code, got, want)
		}
	}
	// The opencode idle gate: its own plain words (busy, waiting), never the
	// wrapped error's text.
	for _, st := range []string{mirror.StatusBusy, mirror.StatusWaiting, ""} {
		assertPlainLine(t, "the opencode idle gate ("+st+")", (&mirror.OpenCodeNotIdleError{Status: st}).Error())
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
	if strings.Contains(got, "data-tool-ctx-text") || strings.Contains(got, "data-tool-ctx-why") || strings.Contains(got, `title="Measured`) {
		t.Errorf("the rows still carry the count's words:\n%s", got)
	}
	if !strings.Contains(got, `>2 steps<`) {
		t.Errorf("the group does not count 2 steps:\n%s", got)
	}
}

// asRole is user 1 with role — the fake mirror's owner — as a browser session.
func asRole(role authz.Role) context.Context {
	return authz.WithPrincipal(context.Background(), authz.Principal{UserID: 1, Role: role, Via: authz.ViaSession})
}

// TestAMembersSessionPagesCarryNoSetup: a view-only computer's Rendered and
// Terminal views, rendered for a MEMBER, hold no muster-mirror setting —
// neither "-interactive" nor any ⓘ code at all — and say to ask the admin;
// the same pages for an ADMIN (the control) hold the setting in the ⓘ.
// Setup details are an admin's (ui.Features.Setup, authz.IsAdmin).
func TestAMembersSessionPagesCarryNoSetup(t *testing.T) {
	s, _ := viewServer(t, readOnlyHost(false))
	render := func(role authz.Role, path string, h func(http.ResponseWriter, *http.Request)) string {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest("GET", path, nil).WithContext(asRole(role)))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s as %s: %d", path, role, rec.Code)
		}
		return rec.Body.String()
	}
	for name, page := range map[string]struct {
		path string
		h    func(http.ResponseWriter, *http.Request)
	}{
		"Rendered view": {ref("main", "@1").ViewPath(), s.handleMirrorViewPage},
		"Terminal view": {ref("main", "@1").TerminalPath(), s.handleMirrorTerminalPage},
	} {
		// noteOf is the composer's note and its ⓘ, the region the member
		// check reads.
		noteOf := func(page string) string {
			n := page[strings.Index(page, `id="mv-readonly"`):]
			return n[:strings.Index(n, `</div></div></div>`)]
		}
		admin := render(authz.RoleAdmin, page.path, page.h)
		if !strings.Contains(noteOf(admin), `data-info-code="">-interactive</code>`) {
			t.Fatalf("CONTROL: %s: an admin's composer ⓘ lacks the setting (the region the member check reads)", name)
		}
		// What the page draws (its scripts' own text is code, not a detail),
		// and the composer note's ⓘ: no setting, and no code row at all.
		member := scriptBlock.ReplaceAllString(sessionBody(t, render(authz.RoleMember, page.path, page.h)), "")
		if i := strings.Index(member, "-interactive"); i >= 0 {
			t.Errorf("%s: a member's page carries the setting: …%s…", name, member[max(0, i-200):min(len(member), i+40)])
		}
		note := noteOf(member)
		if strings.Contains(note, "data-info-code") || strings.Contains(note, "data-info-copy") {
			t.Errorf("%s: a member's composer ⓘ carries a code:\n%s", name, note)
		}
		if !strings.Contains(member, `data-info-withheld="">`+ui.InfoSheetWithheldText) {
			t.Errorf("%s: a member's ⓘ does not say to ask the admin", name)
		}
	}
}

// errorFrameAs dials path on handler h as user 1 with role and returns the
// socket's first error frame.
func errorFrameAs(t *testing.T, h func(http.ResponseWriter, *http.Request), path string, role authz.Role) map[string]string {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h(w, r.WithContext(authz.WithPrincipal(r.Context(), authz.Principal{UserID: 1, Role: role, Via: authz.ViaSession})))
	}))
	defer ts.Close()
	hd := http.Header{}
	hd.Set("Origin", ts.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http")+path, &websocket.DialOptions{HTTPHeader: hd})
	if err != nil {
		t.Fatalf("%s %s: dial: %v", path, role, err)
	}
	defer c.CloseNow()
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("%s %s: no error frame: %v", path, role, err)
		}
		var m map[string]string
		if typ != websocket.MessageText || json.Unmarshal(data, &m) != nil || m["type"] != "error" {
			continue
		}
		return m
	}
}

// TestAMembersSocketErrorCarriesNoDetail: a Rendered view's and a Terminal
// view's socket that ends on a refusal tell a MEMBER the plain line only
// (detail ""), and an ADMIN (the control) the plain line with the exact
// refusal as its detail — through the real handlers, so the role is read
// where each frame is written.
func TestAMembersSocketErrorCarriesNoDetail(t *testing.T) {
	notClaude := fmt.Errorf("%w (host %q, session %q)", mirror.ErrNotClaude, "ws", "main")
	sv, _ := viewServer(t, &fakeMirror{online: true, v2: &fakeMirrorV2{watchErr: notClaude}})
	offline := fmt.Errorf("%w (host %q)", mirror.ErrHostOffline, "ws")
	st, _ := mirrorServer(t, &fakeMirror{online: false, refuse: offline}, newFakeTerminal())
	for name, c := range map[string]struct {
		h       func(http.ResponseWriter, *http.Request)
		path    string
		refusal error
	}{
		"rendered view": {sv.handleMirrorViewWS, ui.MirrorViewWSPath("ws", "main"), notClaude},
		"terminal":      {st.handleMirrorTerminalWS, ui.MirrorTerminalWSPath("ws", "main"), offline},
	} {
		admin := errorFrameAs(t, c.h, c.path, authz.RoleAdmin)
		if admin["detail"] != c.refusal.Error() {
			t.Fatalf("CONTROL: %s: an admin's frame: %v", name, admin)
		}
		member := errorFrameAs(t, c.h, c.path, authz.RoleMember)
		if member["message"] != admin["message"] || member["detail"] != "" {
			t.Errorf("%s: a member's frame: %v, want the same line and an empty detail", name, member)
		}
	}
}
