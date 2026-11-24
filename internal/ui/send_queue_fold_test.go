package ui

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

// THE LIST FOLDS AND ITS ROWS ARE BOUNDED (SendQueueList): every non-empty
// list is a <details>, drawn open, whose <summary> is its count line, with
// every row inside it in the bounded, scrolling <ul>. The fold's memory and
// the kept scroll are the script's, driven in e2e/tests/send-queue.spec.ts ("a
// long queue …").

func renderList(t *testing.T, v SendQueueListView) string {
	t.Helper()
	var b bytes.Buffer
	if err := SendQueueList(v).Render(&b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

var summaryText = regexp.MustCompile(`(?s)<summary id="mv-queue-head"[^>]*>.*?<span data-queue-count="(\d+)">([^<]*)</span></summary>`)

func TestTheQueueIsADetailsOpenUnderItsCountWithEveryRowInsideTheBoundedList(t *testing.T) {
	items := []SendQueueItem{
		{ID: 11, State: "queued", Preview: "first", Cancellable: true},
		{ID: 12, State: "queued", Preview: "second", Cancellable: true},
		{ID: 13, State: "sending", Preview: "third"},
		{ID: 14, State: "failed", Preview: "fourth", Retryable: true, Deletable: true},
	}
	html := renderList(t, SendQueueListView{Items: items, Waiting: "Claude Code is working."})

	if !strings.HasPrefix(html, `<details id="mv-queue-items" data-active="" open="" class="group/sq px-3 pt-1">`) {
		t.Fatalf("not a <details> drawn open: %.200s", html)
	}
	m := summaryText.FindStringSubmatch(html)
	if m == nil {
		t.Fatalf("no count line as the summary: %s", html)
	}
	if m[1] != "4" || m[2] != "3 messages waiting — they send one at a time, each when Claude is free." {
		t.Fatalf("count line = (%s, %q)", m[1], m[2])
	}
	ul := `<ul id="mv-queue-rows" class="max-h-[min(8.5rem,20dvh)] overflow-y-auto overscroll-contain">`
	open, end := strings.Index(html, ul), strings.LastIndex(html, "</ul></details>")
	if open < 0 || end < 0 {
		t.Fatalf("the rows are not the bounded list closing the details: %s", html)
	}
	for _, id := range []string{"11", "12", "13", "14"} {
		at := strings.Index(html, `data-queue-id="`+id+`"`)
		if at < open || at > end {
			t.Errorf("row %s is not inside the bounded list (at %d, list %d–%d)", id, at, open, end)
		}
	}
	// Each row keeps its actions: the queued ones' ⋯ menu, the failed one's
	// Retry and Delete.
	for _, want := range []string{`id="mv-qm-11-btn"`, `id="mv-qm-12-btn"`, `action="/send-queue/14/retry"`, `action="/send-queue/14/delete"`} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %s", want)
		}
	}
}

// With nothing open, the list holds only failures not yet handled; it still
// has its count line to fold under (on origin/main it had none).
func TestTheCountLineOfAListOfFailuresOnly(t *testing.T) {
	for _, c := range []struct {
		items []SendQueueItem
		want  string
	}{
		{[]SendQueueItem{{ID: 1, State: "failed", Preview: "a", Retryable: true}}, "1 message not sent"},
		{[]SendQueueItem{{ID: 1, State: "failed", Preview: "a"}, {ID: 2, State: "failed", Preview: "b"}}, "2 messages not sent"},
		{[]SendQueueItem{{ID: 1, State: "queued", Preview: "a"}}, "1 message waiting — it sends when Claude is free."},
	} {
		m := summaryText.FindStringSubmatch(renderList(t, SendQueueListView{Items: c.items}))
		if m == nil || m[2] != c.want {
			t.Errorf("%d items: count line %v; want %q", len(c.items), m, c.want)
		}
	}
	if html := renderList(t, SendQueueListView{}); html != "" {
		t.Errorf("an empty queue draws %q; want nothing", html)
	}
}

// The script keeps the fold under the key the docs name, per list URL.
func TestTheFoldIsKeptPerWindowUnderItsKey(t *testing.T) {
	var b bytes.Buffer
	if err := sendQueueScript().Render(&b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), `var FOLD = 'muster.sendqueue.closed:' + src;`) {
		t.Fatal("the fold is not keyed by the list's URL under muster.sendqueue.closed:")
	}
}
