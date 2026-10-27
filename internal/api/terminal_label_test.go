package api

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/ui"
)

// THE TERMINAL PAGE NEVER CALLS ITSELF "READ-ONLY" WHERE IT CAN SEND.
//
// The switch used to read "Read-only" and the status "Connected · read-only"
// whenever live keys were off, while the composer under the terminal sends a
// message without the switch: a label telling a person they cannot send, on a
// page that can. The switch now names what it gates (Live keys off / on), and a
// host that genuinely cannot be sent to says so once, in its composer's
// disabled note.
//
// Red at origin/main 5252925 (the switch's label and both status strings carry
// "read-only"); green at HEAD.
//
// LIVE KEYS ARE NOW ALWAYS ON and the switch is gone (ui.AgentTerminalPage):
// the status line names the state instead — Connected, "confirm it's you to
// type", "this host takes no keys" — and none of it says read-only either.

var (
	scriptBlock = regexp.MustCompile(`(?s)<script\b[^>]*>.*?</script>`)
	jsLiteral   = regexp.MustCompile(`'[^'\n]*'|"[^"\n]*"`)
	anyTag      = regexp.MustCompile(`(?s)<[^>]*>`)
	readOnlyRe  = regexp.MustCompile(`(?i)read[- ]only`)
)

// readOnlyWording is every place page SHOWS "read-only" to a person: its text
// outside scripts, its attributes a person reads (placeholder, title,
// aria-label), and the string literals its scripts put on screen. Script
// COMMENTS are not shown and are not counted.
func readOnlyWording(page string) []string {
	var out []string
	for _, s := range scriptBlock.FindAllString(page, -1) {
		for _, lit := range jsLiteral.FindAllString(s, -1) {
			if readOnlyRe.MatchString(lit) {
				out = append(out, "script literal "+lit)
			}
		}
	}
	rest := scriptBlock.ReplaceAllString(page, "")
	for _, attr := range regexp.MustCompile(`(?:placeholder|title|aria-label)="([^"]*)"`).FindAllStringSubmatch(rest, -1) {
		if readOnlyRe.MatchString(attr[1]) {
			out = append(out, "attribute "+attr[0])
		}
	}
	text := anyTag.ReplaceAllString(rest, " ")
	for _, m := range readOnlyRe.FindAllStringIndex(text, -1) {
		lo, hi := max(0, m[0]-40), min(len(text), m[1]+40)
		out = append(out, "text …"+strings.Join(strings.Fields(text[lo:hi]), " ")+"…")
	}
	return out
}

// theStatusSaysLiveKeys asserts the page has no Live keys switch and its
// script draws the status line from the exported words.
func theStatusSaysLiveKeys(t *testing.T, what, page string) {
	t.Helper()
	for _, id := range []string{`id="term-mode"`, `id="term-mode-label"`} {
		if strings.Contains(page, id) {
			t.Errorf("%s: the page still draws the Live keys switch (%s)", what, id)
		}
	}
	for _, lit := range []string{ui.StatusConnected, ui.StatusConfirmToType, ui.StatusNoKeys, ui.StatusReconnecting} {
		if !strings.Contains(page, strconv.Quote(lit)) {
			t.Errorf("%s: the page's script does not use %q", what, lit)
		}
	}
}

// TestAnInteractiveHostsTerminalPageSaysNothingIsReadOnly: the composer can
// send, so nothing on the page may read "read-only".
func TestAnInteractiveHostsTerminalPageSaysNothingIsReadOnly(t *testing.T) {
	s, ts := viewServer(t, &fakeMirror{online: true, interactive: true, v2: &fakeMirrorV2{}})
	for _, path := range []string{chiefTermPage, mainPage} {
		_, page := mirrorGet(t, s, ts, path)
		if tagOf(page, "mv-input") == "" || disabledAttr.MatchString(tagOf(page, "mv-input")) {
			t.Fatalf("%s: fixture: the composer is not there and enabled", path)
		}
		if got := readOnlyWording(page); len(got) != 0 {
			t.Errorf("%s: a page whose composer can send says read-only:\n  %s", path, strings.Join(got, "\n  "))
		}
		theStatusSaysLiveKeys(t, path, page)
	}
}

// TestAReadOnlyHostsOnlyExplanationIsItsComposersNote: on a host that genuinely
// cannot be sent to, the composer's disabled note (and its placeholder) is the
// ONE place the page says so — no switch, no status wording.
func TestAReadOnlyHostsOnlyExplanationIsItsComposersNote(t *testing.T) {
	s, ts := viewServer(t, &fakeMirror{online: true, interactive: false, v2: &fakeMirrorV2{}})
	_, page := mirrorGet(t, s, ts, chiefTermPage)
	if !strings.Contains(page, `<p id="mv-readonly"`) || !strings.Contains(page, ui.MirrorReadOnlyNote) {
		t.Fatal("a read-only host's page does not carry its composer's note")
	}
	if term := tagOf(page, "term"); !strings.Contains(term, ` data-keys-off`) {
		t.Errorf("a read-only host's terminal does not turn its keys off: %s", term)
	}
	theStatusSaysLiveKeys(t, "a read-only host", page)
	// Remove the composer's own words (its note, which it also carries as a
	// tooltip, and its placeholder): nothing else may say it.
	if got := readOnlyWording(page); len(got) == 0 {
		t.Fatal("control: readOnlyWording does not see the composer's own note")
	}
	rest := strings.ReplaceAll(page, ui.MirrorReadOnlyNote, "")
	rest = strings.Replace(rest, `placeholder="Read-only host"`, "", 1)
	if got := readOnlyWording(rest); len(got) != 0 {
		t.Errorf("a read-only host's page says read-only outside its composer's note:\n  %s", strings.Join(got, "\n  "))
	}
}

// TestAnAgentsTerminalPageSaysNothingIsReadOnly: an agent pod's terminal page
// has the same status line and a composer that sends.
func TestAnAgentsTerminalPageSaysNothingIsReadOnly(t *testing.T) {
	var b strings.Builder
	c := ui.ComposerView{SendPath: ui.AgentSendPath("keen-wren"), Placeholder: "Message…", Aria: "Send a message"}
	if err := ui.AgentTerminalPage(ui.AgentTerminalView{Name: "keen-wren", Composer: &c}).Render(&b); err != nil {
		t.Fatal(err)
	}
	if got := readOnlyWording(b.String()); len(got) != 0 {
		t.Errorf("an agent's terminal page says read-only:\n  %s", strings.Join(got, "\n  "))
	}
	theStatusSaysLiveKeys(t, "an agent's terminal", b.String())
}
