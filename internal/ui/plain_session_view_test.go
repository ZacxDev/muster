package ui

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/plainscan"
)

// THE SESSION VIEW IN PLAIN WORDS (jargon audit, "settings" theme: F020,
// F027, F044, F045). Every expectation is a literal: a test that read the
// constants it pins would pass whatever they said.

// TestAnOpenedToolRowSaysInputAndOutput (F020): an opened tool row labels its
// two blocks "Input" and "Output" — the persisted row, a subagent card's
// result and the live stream's pre() alike — and never "args" or "result".
func TestAnOpenedToolRowSaysInputAndOutput(t *testing.T) {
	call := ChatLine{Role: "assistant", Kind: "tool_call", ToolName: "Bash", ToolID: "tool-1", Content: `{"command":"ls atlas"}`}
	res := &ChatLine{Role: "assistant", Kind: "tool_result", ToolName: "Bash", ToolID: "tool-1", Content: "acme/api", ToolOK: true}
	row := renderString(t, chatToolChip(call, res))
	for _, want := range []string{
		`<div class="text-[10px] uppercase tracking-wide text-muted">Input</div>`,
		`<div class="text-[10px] uppercase tracking-wide text-muted">Output</div>`,
	} {
		if !strings.Contains(row, want) {
			t.Errorf("the opened tool row has no %s in:\n%s", want, row)
		}
	}
	for _, gone := range []string{">args</div>", ">result</div>"} {
		if strings.Contains(row, gone) {
			t.Errorf("the opened tool row still labels a block %s", gone)
		}
	}
	// CONTROL: the blocks themselves are drawn (a row with no body would pass
	// the "gone" checks above vacuously).
	if !strings.Contains(row, "acme/api") || !strings.Contains(row, "ls atlas") {
		t.Fatalf("CONTROL: the row does not draw its input and output:\n%s", row)
	}

	js := renderString(t, agentChatScript("atlas"))
	for _, want := range []string{"pre('Input', clip(msg.args))", "pre('Output', clip(msg.output))"} {
		if !strings.Contains(js, want) {
			t.Errorf("the live stream does not draw %s", want)
		}
	}
	for _, gone := range []string{"pre('args'", "pre('result'"} {
		if strings.Contains(js, gone) {
			t.Errorf("the live stream still labels a block with %s", gone)
		}
	}
}

// TestTheModelPickerSaysChooseAModel (F044): the model box's placeholder and
// accessible name are "Choose a model…" / "Choose a model".
func TestTheModelPickerSaysChooseAModel(t *testing.T) {
	// A selected model fills the box; the placeholder is what an empty one says.
	box := renderString(t, modelFieldFor("", ""))
	for _, want := range []string{`placeholder="Choose a model…"`, `aria-label="Choose a model"`} {
		if !strings.Contains(box, want) {
			t.Errorf("the model box has no %s in:\n%.2000s", want, box)
		}
	}
	for _, gone := range []string{"quick-pick", "filter/search"} {
		if strings.Contains(box, gone) {
			t.Errorf("the model box still says %q", gone)
		}
	}
}

// TestTheChiefPanelSaysWhatTheChiefDoes (F045): the line under "Chief" is
// "keeps an eye on all your agents", not "the fleet's orchestrator".
func TestTheChiefPanelSaysWhatTheChiefDoes(t *testing.T) {
	panel := renderString(t, ChiefPanel(false, false))
	if !strings.Contains(panel, `<span class="text-[10px] text-muted">keeps an eye on all your agents</span>`) {
		t.Errorf("the chief panel's subtitle is not the plain one:\n%.3000s", panel)
	}
	if strings.Contains(panel, "orchestrator") {
		t.Error("the chief panel still calls the chief an orchestrator")
	}
}

// TestTheMutedMicAndPaperclipSayWhoTurnsThemOn (F027): with dictation and
// attachments off, the composer's muted mic and paperclip say the feature is
// not turned on and that the admin turns it on — whole strings, in the
// tooltip and the accessible name — and nothing the plain scans flag.
func TestTheMutedMicAndPaperclipSayWhoTurnsThemOn(t *testing.T) {
	off := renderString(t, mirrorComposer(ComposerView{SendPath: "/mirror/view/send", Aria: "Message", DictateOff: true, UploadOff: true}))
	for _, want := range []string{
		`aria-label="Voice typing isn&#39;t turned on. An admin turns it on under Features."`,
		`title="Voice typing isn&#39;t turned on. An admin turns it on under Features."`,
		`aria-label="File attachments aren&#39;t turned on. An admin turns them on under Features."`,
		`title="File attachments aren&#39;t turned on. An admin turns them on under Features."`,
	} {
		if !strings.Contains(off, want) {
			t.Errorf("the muted controls have no %s", want)
		}
	}
	if strings.Contains(off, "on this server") || strings.Contains(off, "how to turn") {
		t.Error("a muted control still says \"on this server\" / \"how to turn … on\"")
	}
	texts, err := plainscan.TopLayerTexts(off)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, s := range texts {
		if strings.Contains(s, "turned on") {
			seen++
		}
		if m := plainscan.Jargon(s); m != "" {
			t.Errorf("the composer's top layer says %q (%q)", s, m)
		}
	}
	if seen != 4 {
		t.Errorf("CONTROL: the scan read %d of the 4 muted-control labels", seen)
	}
}

// TestASetupSheetWithheldCarriesNoSetup (audit round 1): a SetupSheet for
// anyone but an admin carries neither its code nor its lead — the lead
// describes the setup too — only "Ask your admin…"; the admin's carries both.
func TestASetupSheetWithheldCarriesNoSetup(t *testing.T) {
	const lead, code = "Set this in the server's settings:", "MUSTER_EXAMPLE_KEY"
	admin := InfoSheetHTML(SetupSheet("s", "How", lead, code, true))
	member := InfoSheetHTML(SetupSheet("s", "How", lead, code, false))
	for _, n := range []string{"Set this in the server", code, "data-info-code"} {
		if !strings.Contains(admin, n) {
			t.Errorf("CONTROL: the admin's sheet has no %q", n)
		}
		if strings.Contains(member, n) {
			t.Errorf("a withheld sheet carries %q", n)
		}
	}
	if !strings.Contains(member, ">Ask your admin for these details.</p>") {
		t.Error("a withheld sheet does not say to ask the admin")
	}
}
