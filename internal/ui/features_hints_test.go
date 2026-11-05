package ui

import (
	"strings"
	"testing"
)

// The hints that replace a control hidden because its feature is off
// (features.go's FeatureHref): a muted mic and paperclip in the composer, a
// muted Connectors entry in the nav — each linking to its row on the Features
// page — and the real control, with no hint, when the feature is on.

func TestTheComposerShowsMutedHintsWhenDictationAndAttachmentsAreOff(t *testing.T) {
	off := renderString(t, mirrorComposer(ComposerView{SendPath: "/mirror/view/send", Aria: "Message", DictateOff: true, UploadOff: true}))
	for _, want := range []string{
		`id="mv-dictate-off" href="/account/features#dictation"`,
		`id="mv-attach-off" href="/account/features#attachments"`,
		`aria-label="` + DictateOffLabel + `"`,
		`aria-label="` + AttachOffLabel + `"`,
	} {
		if !strings.Contains(off, want) {
			t.Errorf("feature off: no %s", want)
		}
	}
	if strings.Contains(off, `id="mv-dictate"`) || strings.Contains(off, `id="mv-attach"`) {
		t.Error("feature off: the real mic or paperclip is drawn")
	}

	on := renderString(t, mirrorComposer(ComposerView{SendPath: "/mirror/view/send", Aria: "Message",
		Dictate: TranscribePath, Upload: UploadPath}))
	if !strings.Contains(on, `id="mv-dictate"`) || !strings.Contains(on, `id="mv-attach"`) {
		t.Error("feature on: the real mic or paperclip is missing")
	}
	if strings.Contains(on, "mv-dictate-off") || strings.Contains(on, "mv-attach-off") {
		t.Error("feature on: a hint is drawn beside the real control")
	}

	// A read-only (disabled) composer draws neither the controls nor the hints:
	// there is nothing its text could be sent to.
	ro := renderString(t, mirrorComposer(ComposerView{SendPath: "/mirror/view/send", Aria: "Message", Disabled: "Read-only host",
		DictateOff: true, UploadOff: true}))
	if strings.Contains(ro, "mv-dictate-off") || strings.Contains(ro, "mv-attach-off") {
		t.Error("a disabled composer draws the hints")
	}
	// The zero value draws neither: a caller that does not set the flags gets
	// the composer as it was.
	zero := renderString(t, mirrorComposer(ComposerView{SendPath: "/mirror/view/send", Aria: "Message"}))
	if strings.Contains(zero, "mv-dictate-off") || strings.Contains(zero, "mv-attach-off") {
		t.Error("the zero value draws a hint")
	}
}

func TestTheNavShowsConnectorsAsOff(t *testing.T) {
	doc := func(f Features) string {
		return sidebarPanel(t, documentSource(t, NavDocument(NavDocumentView{Title: "x", Entry: NavAccount, Feat: f, Main: "<h1>x</h1>"})))
	}
	off := doc(Features{})
	if !strings.Contains(off, `id="sidebar-connectors-off" href="/account/features#connectors"`) {
		t.Error("connectors off: no muted Connectors entry linking to its Features row")
	}
	if !strings.Contains(off, `id="sidebar-features" href="/account/features"`) {
		t.Error("no Features link")
	}
	// The /plugins and /flows links were dropped (#441 round 0, D6).
	if strings.Contains(off, `href="/plugins"`) || strings.Contains(off, `href="/flows"`) {
		t.Error("the nav links to /plugins or /flows")
	}
	if on := doc(Features{Connectors: true}); strings.Contains(on, "sidebar-connectors-off") {
		t.Error("connectors on: the off entry is drawn")
	}
}

// TestTheAgentChatShowsTheMutedMicWhenDictationIsOff: the agent chat form (and
// the chief panel, which draws the same pane) draws the muted mic when the
// server has no dictation, and the real one when it does.
func TestTheAgentChatShowsTheMutedMicWhenDictationIsOff(t *testing.T) {
	off := renderString(t, agentChatPane(AgentDetailView{Name: "a", Status: "running", ActiveSessionID: 1, DictateOff: true}))
	if !strings.Contains(off, `id="chat-dictate-off" href="/account/features#dictation"`) || strings.Contains(off, `id="chat-dictate"`) {
		t.Error("dictation off: no muted mic in the agent chat")
	}
	on := renderString(t, agentChatPane(AgentDetailView{Name: "a", Status: "running", ActiveSessionID: 1, Dictate: TranscribePath}))
	if !strings.Contains(on, `id="chat-dictate"`) || strings.Contains(on, "chat-dictate-off") {
		t.Error("dictation on: the real mic is missing or the hint is drawn")
	}
	if zero := renderString(t, agentChatPane(AgentDetailView{Name: "a", Status: "running", ActiveSessionID: 1})); strings.Contains(zero, "chat-dictate-off") {
		t.Error("the zero value draws the hint")
	}
}
