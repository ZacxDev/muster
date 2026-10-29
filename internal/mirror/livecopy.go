package mirror

// A LIVE COPY (Window.LiveCopy): the conversation in a mirrored Claude Code
// window has a second transcript, under another session id, that is still
// running somewhere else.
//
// How one happens (an external report, confirmed against transcripts on a
// real host): `claude --resume <id>` of a conversation that is STILL OPEN in
// another client (the VS Code extension, another terminal) does not attach to
// it. It copies the conversation into a NEW session — every row of the old
// transcript kept with its own uuid and timestamp, only sessionId rewritten —
// and carries on there. muster mirrors the copy correctly, so the phone shows a
// working session whose messages never reach the other client, and nothing on
// the card says so. The host finds the copy (mirrorhost's livecopy.go); this
// file is the bound on the wire and the words.
//
// ⚠ THE WORDS DO NOT SAY WHICH ONE IS THE COPY. A transcript names no parent
// session (no field for it, measured on Claude Code 2.1.x): the two files share
// their first rows and nothing says which was written first. So both cards say
// the same, true thing — the conversation runs in two places, and they do not
// see each other's messages.

// MaxLiveCopyWindows bounds how many windows of ONE list carry a LiveCopy
// (the frame budget: TestTheLargestWindowListFitsOneFrame).
const MaxLiveCopyWindows = 8

// LiveCopyShortID is how many leading characters of the other session's id
// the warning shows: enough to tell it from the window's own, and to find it
// with `claude --resume`'s picker.
const LiveCopyShortID = 8

// boundLiveCopy keeps a LiveCopy on the first MaxLiveCopyWindows windows of a
// sorted list that carry one.
func boundLiveCopy(out []Window) {
	n := 0
	for i := range out {
		if out[i].LiveCopy == "" {
			continue
		}
		if n++; n > MaxLiveCopyWindows {
			out[i].LiveCopy = ""
		}
	}
}

// LiveCopyWarning is the one line a card and the views show about a window
// whose conversation also runs elsewhere (SessionView.LiveCopy), "" for none.
//
// 🔴 ONE RULE, ONE PLACE: the card, the Rendered view and the Terminal view
// all take it from here.
func (s SessionView) LiveCopyWarning() string {
	if s.LiveCopy == "" || !s.Claude {
		return ""
	}
	id := s.LiveCopy
	if len(id) > LiveCopyShortID {
		id = id[:LiveCopyShortID]
	}
	return "This conversation is also open as session " + id + ", which is still running elsewhere. " +
		"Messages sent here won't appear there, and its messages won't appear here."
}
