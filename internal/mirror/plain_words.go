package mirror

import (
	"errors"
	"strings"
)

// PLAIN WORDS FOR A HOST'S REFUSALS (the operator rule: muster is for people
// who are not engineers, so its own chrome shows no raw error string, flag or
// id in its top layer). The errors of this package keep their exact text —
// logs, agents and the ⓘ details sheet read it — and a page shows a person
// PlainWords instead, with the exact text one tap down in the ⓘ.

// PlainWords is what a page tells a person about err, a refusal or the end of
// a terminal or rendered view of a window on host ("" = "this computer"): a
// plain sentence with no flag, binary name, quoted id or "mirror:" prefix.
// An error it does not know reads as PlainUnknown, never as its own text.
func PlainWords(err error, host string) string {
	where, lead := host, host
	if host == "" {
		where, lead = "this computer", "This computer"
	}
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrStepUpRequired):
		// The sign-in rule's own words, which already say what to do.
		return strings.TrimPrefix(err.Error(), "mirror: ")
	case errors.Is(err, ErrHostOffline):
		return lead + " is not connected to muster right now."
	case errors.Is(err, ErrSessionGone):
		return "This session has ended on " + where + "."
	case errors.Is(err, ErrWindowGone):
		return "This window has closed on " + where + "."
	case errors.Is(err, ErrHostDisconnected):
		return "The connection to " + where + " dropped. Reconnect to carry on."
	case errors.Is(err, ErrOverrun), errors.Is(err, ErrViewOverrun):
		return "This page fell behind the session. Reconnect to carry on."
	case errors.Is(err, ErrInteractiveRefused):
		return PlainViewOnly
	case errors.Is(err, ErrTooManyStreams), errors.Is(err, ErrTooManyWatches):
		return "Too many views of " + where + " are open. Close one (it may be in another tab) and try again."
	case errors.Is(err, ErrNoRenderedView), errors.Is(err, ErrPanePasteUnsupported),
		errors.Is(err, ErrScrollUnsupported), errors.Is(err, ErrCatalogUnsupported):
		return lead + " needs a muster update for this. Use the terminal for now."
	case errors.Is(err, ErrNoStatusReport):
		return lead + " needs a muster update before messages can wait for it. Send them yourself for now."
	case errors.Is(err, ErrNotClaude):
		return "There is no Claude Code conversation in this window."
	}
	return PlainUnknown
}

// PlainUnknown is PlainWords of an error it has no words for.
const PlainUnknown = "Something went wrong with this session. The details have more."

// PlainViewOnly is what a page says of a host that takes no typing (it did
// not advertise -interactive): the sentence the composer and the terminal's
// refusal share. The setting that changes it is the ⓘ's (ViewOnlySetting).
const PlainViewOnly = "This computer is view-only, so you cannot type into it from here."

// ViewOnlySetting is the muster-mirror setting a view-only host lacks: what
// its ⓘ shows an admin, never the top layer.
const ViewOnlySetting = "-interactive"

// OpenCodeSendSetting is the muster-mirror setting a host needs before muster
// sends to its opencode sessions: what the ⓘ shows an admin.
const OpenCodeSendSetting = "-interactive -opencode-send"

// SendOffSetting is the setting e's host lacks for muster to send to it
// through its runtime's gated send (OpenCodeSendSetting for opencode), "" when
// that is not why the send is blocked (SendBlocked): an unbound pane, a
// runtime with no gated send, or one the host sends to.
func SendOffSetting(h HostView, e SessionView) string {
	r, ok := e.Spec()
	if !ok || !r.GatedSend || h.SendsOn(r) {
		return ""
	}
	return OpenCodeSendSetting
}
