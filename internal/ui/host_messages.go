package ui

import (
	g "maragu.dev/gomponents"
	. "maragu.dev/gomponents/html"

	"github.com/ZacxDev/muster/internal/mirror"
)

// PLAIN HOST MESSAGES (the operator rule: muster is for people who are not
// engineers, so its own chrome shows no flag, raw error string, id or
// host:port in its top layer). A session page says what a host can and cannot
// do in a plain sentence; the flag that changes it, or the exact message a
// host sent, is in the ⓘ details sheet (InfoSheet) beside it — and a SETUP
// detail (a flag, what to restart) is an admin's: anyone else's sheet says to
// ask their admin.

// MirrorReadOnlyNote is what a view-only host's composer says instead of
// accepting text: mirror.PlainViewOnly, the words its refusal says too.
const MirrorReadOnlyNote = mirror.PlainViewOnly

// MirrorReadOnlyPlaceholder is a view-only host's disabled message box.
const MirrorReadOnlyPlaceholder = "View only"

// HostSettingSheet is THE ⓘ of a host SETUP detail: the muster-mirror setting
// (setting, e.g. "-interactive") that would let a person do what the plain
// note beside it says they cannot. admin: the viewer may see setup details;
// anyone else's sheet is Withheld ("Ask your admin…") and carries no setting.
// id is the sheet's element id, unique in its document.
func HostSettingSheet(id, setting string, admin bool) InfoSheet {
	s := InfoSheet{
		ID: id, Label: "How to turn on typing", Heading: "Turn on typing",
		Lead:   "To allow it, restart this computer's muster-mirror with this setting:",
		Code:   setting,
		Inline: true,
	}
	if !admin {
		// Nothing of the setup, the lead included: the sheet says to ask.
		s.Lead, s.Withheld = "Typing from muster is turned off on this computer.", true
	}
	return s
}

// HostDetailSheet is THE ⓘ of a message a host or the hub sent about a
// session (why its view ended, why it has no chat): the exact text, which the
// plain sentence beside it never spells. Withheld unless admin, like every
// technical detail of a host's setup. code may be filled by the page's script
// (data-info-code, data-copy-text) when the message arrives over a socket.
func HostDetailSheet(id, code string, admin bool) InfoSheet {
	s := InfoSheet{ID: id, Label: "Details", Heading: "Details", Lead: "What muster was told, word for word:", Code: code, Inline: true}
	if !admin {
		s.Lead, s.Withheld = "", true
	}
	return s
}

// hostDetailSlot is a banner's ⓘ for a message that arrives over a socket
// (a view's or a terminal's error frame): HostDetailSheet with a placeholder
// code, inside an inline box drawn hidden. hostDetailJS's setDetail fills the code
// and its Copy text with the frame's detail and shows the span; a frame
// with no detail hides it again.
func hostDetailSlot(id string, admin bool) g.Node {
	return Div(ID(id), g.Attr("hidden", ""), g.Attr("data-host-detail", ""), Class("ml-1 inline-flex align-middle [&[hidden]]:hidden"),
		HostDetailSheet(id+"-sheet", " ", admin).Node())
}

// hostDetailJS defines setDetail(slot, detail) for a page's error banner
// (hostDetailSlot). Text only: textContent and an attribute, never markup.
const hostDetailJS = `
  function setDetail(slot, detail) {
    if (!slot) return;
    if (!detail) { slot.hidden = true; return; }
    var code = slot.querySelector('[data-info-code]');
    if (code) code.textContent = detail;
    var copy = slot.querySelector('[data-info-copy]');
    if (copy) copy.setAttribute('data-copy-text', detail);
    slot.hidden = false;
  }
`
