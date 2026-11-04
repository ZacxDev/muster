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

func TestTheNavShowsConnectorsAsOffAndLinksPluginsAndFlowsWhenOn(t *testing.T) {
	doc := func(f Features) string {
		return sidebarPanel(t, documentSource(t, NavDocument(NavDocumentView{Title: "x", Entry: NavAccount, Feat: f, Main: "<h1>x</h1>"})))
	}
	off := doc(Features{})
	if !strings.Contains(off, `id="sidebar-connectors-off" href="/account/features#connectors"`) {
		t.Error("connectors off: no muted Connectors entry linking to its Features row")
	}
	if strings.Contains(off, `id="sidebar-plugins"`) || strings.Contains(off, `id="sidebar-flows"`) {
		t.Error("plugins/flows off: their links are drawn")
	}
	if !strings.Contains(off, `id="sidebar-features" href="/account/features"`) {
		t.Error("no Features link")
	}
	on := doc(Features{Connectors: true, Plugins: true, Flows: true})
	if strings.Contains(on, "sidebar-connectors-off") {
		t.Error("connectors on: the off entry is drawn")
	}
	if !strings.Contains(on, `id="sidebar-plugins" href="/plugins"`) || !strings.Contains(on, `id="sidebar-flows" href="/flows"`) {
		t.Error("plugins/flows on: no link")
	}
}
