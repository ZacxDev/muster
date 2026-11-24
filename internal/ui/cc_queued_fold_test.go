package ui

import (
	"regexp"
	"strings"
	"testing"
)

// CLAUDE CODE'S QUEUE IS BOUNDED AND FOLDS, like the send queue
// (send_queue_fold_test.go): the region holds a <details>, drawn open, whose
// <summary> is the count line, with every message inside the bounded,
// scrolling <ul>. The fold's memory and the kept scroll are the view script's
// (ccQueuedKeepJS), driven in e2e/tests/send-queue.spec.ts ("Claude Code's
// own long queue …").

var ccqSummary = regexp.MustCompile(`(?s)<summary[^>]*>.*?<span data-cc-queued-head="">([^<]*)</span></summary>`)

func TestClaudeCodesQueueIsADetailsOpenUnderItsCountWithEveryMessageInsideTheBoundedList(t *testing.T) {
	html := renderString(t, CCQueued([]string{"first", "second", "third"}))

	if !strings.Contains(html, `<details id="mv-ccq-fold" open="" class="group/ccq">`) {
		t.Fatalf("not a <details> drawn open: %s", html)
	}
	if m := ccqSummary.FindStringSubmatch(html); m == nil || m[1] != "Queued in Claude Code · 3" {
		t.Fatalf("count line %v: %s", m, html)
	}
	ul := `<ul id="mv-ccq-rows" class="flex flex-col gap-1.5 pb-2 max-h-[min(8.5rem,20dvh)] overflow-y-auto overscroll-contain">`
	open, end := strings.Index(html, ul), strings.LastIndex(html, "</ul></details></section>")
	if open < 0 || end < 0 {
		t.Fatalf("the messages are not the bounded list closing the details: %s", html)
	}
	for _, s := range []string{"first", "second", "third"} {
		if at := strings.Index(html, ">"+s+"<"); at < open || at > end {
			t.Errorf("%q is not inside the bounded list (at %d, list %d–%d)", s, at, open, end)
		}
	}
	// The region itself no longer scrolls: its rows do.
	if strings.Contains(html, "max-h-40") {
		t.Errorf("the region still carries its old bound: %s", html)
	}

	// One message: the count line without a number; none: hidden, no fold.
	if m := ccqSummary.FindStringSubmatch(renderString(t, CCQueued([]string{"only"}))); m == nil || m[1] != "Queued in Claude Code" {
		t.Errorf("one message: count line %v", m)
	}
	if empty := renderString(t, CCQueued(nil)); empty != `<div id="mv-ccq" data-cc-queued="0" hidden=""></div>` {
		t.Errorf("an empty list draws %s", empty)
	}
}

// The view's patch of the region goes through ccqKeep, and the fold is kept
// under a key of its own, per window — not the send queue's.
func TestTheViewKeepsClaudeCodesQueueFoldAndScrollAcrossAPatch(t *testing.T) {
	js := renderString(t, mirrorViewScript())
	for _, want := range []string{
		`var kept = op.id === 'mv-ccq' ? ccqKeep() : null;
          pt.replaceWith(frag(op.html));
          if (kept) kept();`,
		`return 'muster.ccqueue.closed:' + (l ? l.getAttribute('data-ws') || '' : '');`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("the view script lacks:\n%s", want)
		}
	}
	if CCQueuedClosedKeyPrefix == SendQueueClosedKeyPrefix {
		t.Error("the two lists fold under the same key")
	}
}
