package ui

import (
	"strings"
	"testing"
)

// THE QUEUED REGION IS REDRAWN WHOLE, AND ONLY WHEN THE LIST MOVED — but
// always the first time a socket hears one, so whatever an earlier socket or
// a snapshot drew is replaced (never left to linger).
func TestQueuedOpsRedrawTheRegionWhenTheListMoves(t *testing.T) {
	var tr MirrorTranscript
	first := tr.QueuedOps(nil)
	if len(first) != 1 || first[0].Op != "patch" || first[0].ID != ccQueuedID || !strings.Contains(first[0].HTML, `hidden`) {
		t.Fatalf("the first (empty) list drew %+v, want one patch hiding the region", first)
	}
	if ops := tr.QueuedOps(nil); len(ops) != 0 {
		t.Fatalf("the same empty list again drew %+v", ops)
	}
	ops := tr.QueuedOps([]string{"then say ok."})
	if len(ops) != 1 || ops[0].Op != "patch" || !strings.Contains(ops[0].HTML, "then say ok.") ||
		!strings.Contains(ops[0].HTML, "Queued in Claude Code") || strings.Contains(ops[0].HTML, " hidden") {
		t.Fatalf("one queued message drew %+v", ops)
	}
	if ops := tr.QueuedOps([]string{"then say ok."}); len(ops) != 0 {
		t.Fatalf("the same list again drew %+v", ops)
	}
	// Taken: the region empties at once.
	if ops := tr.QueuedOps(nil); len(ops) != 1 || strings.Contains(ops[0].HTML, "then say ok.") {
		t.Fatalf("the emptied list drew %+v", ops)
	}
	// A Reset starts the socket's drawing over: the next list is drawn even
	// when it equals the last.
	tr.Reset()
	if ops := tr.QueuedOps(nil); len(ops) != 1 {
		t.Fatalf("after a Reset the list drew %+v, want the region redrawn", ops)
	}
}

// A queued message is the transcript's text: drawn as text, escaped.
func TestQueuedMessagesAreEscapedAndCounted(t *testing.T) {
	var tr MirrorTranscript
	ops := tr.QueuedOps([]string{`<img src=x onerror=alert(1)>`, "second"})
	html := ops[0].HTML
	if strings.Contains(html, "<img") || !strings.Contains(html, "&lt;img") {
		t.Fatalf("the message was not escaped: %s", html)
	}
	if !strings.Contains(html, `data-cc-queued="2"`) || strings.Count(html, "data-cc-queued-item") != 2 || !strings.Contains(html, "Queued in Claude Code · 2") {
		t.Fatalf("two messages drew %s", html)
	}
}

// THE REGION IS OUTSIDE THE CONVERSATION: drawn after #mv-log closes, so its
// appearing never moves what is being read in the log.
func TestTheQueuedRegionIsOutsideTheConversation(t *testing.T) {
	page := renderString(t, MirrorViewPage(MirrorViewView{Host: "h", Session: "s", Interactive: true}))
	logAt := strings.Index(page, `id="mv-log"`)
	regionAt := strings.Index(page, `id="`+ccQueuedID+`"`)
	if logAt < 0 || regionAt < 0 {
		t.Fatalf("log at %d, region at %d", logAt, regionAt)
	}
	logEnd := logAt + strings.Index(page[logAt:], "</main>")
	if regionAt < logEnd {
		t.Fatalf("the region (at %d) is inside the conversation (ends at %d)", regionAt, logEnd)
	}
	if strings.Count(page, `id="`+ccQueuedID+`"`) != 1 {
		t.Fatal("the page draws the region more than once")
	}
}
