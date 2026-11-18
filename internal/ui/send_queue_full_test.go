package ui

import (
	"bytes"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// THE WHOLE MESSAGE (sendQueuePreview): a row's preview is cut (the server's
// 200 bytes) and clamped (two lines), so a tap on it opens the whole text in a
// popover — a native popovertarget button, so it works without script.

// queueDoc renders v's list and parses it.
func queueDoc(t *testing.T, v SendQueueListView) *html.Node {
	t.Helper()
	var b bytes.Buffer
	if err := SendQueueList(v).Render(&b); err != nil {
		t.Fatal(err)
	}
	doc, err := html.Parse(&b)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func sqAttr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

// findNode is the first node under n for which ok holds, or nil.
func sqFind(n *html.Node, ok func(*html.Node) bool) *html.Node {
	if ok(n) {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if f := sqFind(c, ok); f != nil {
			return f
		}
	}
	return nil
}

func sqText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// TestATapOnAPreviewOpensTheWholeMessage: a queued, a sending and a failed
// row whose text is longer than its preview each draw the preview as a button
// (popovertarget, aria-label, 44px) opening a popover that holds the WHOLE
// text — its tail, which the preview does not — with a Close. A row with no
// whole text draws the preview alone, with nothing to open.
func TestATapOnAPreviewOpensTheWholeMessage(t *testing.T) {
	const tail = "kilo-tango-tail-0731"
	for i, state := range []string{"queued", "sending", "failed"} {
		id := int64(41 + i)
		body := strings.Repeat(state+" words ", 40) + tail
		preview := body[:200] + "…"
		it := SendQueueItem{ID: id, State: state, Preview: preview, Body: body, Cancellable: state == "queued",
			Retryable: state == "failed", Deletable: state == "failed"}
		doc := queueDoc(t, SendQueueListView{Items: []SendQueueItem{it}})
		full := SendQueueFullID(id)
		pop := sqFind(doc, func(n *html.Node) bool { v, _ := sqAttr(n, "id"); return n.Type == html.ElementNode && v == full })
		if pop == nil {
			t.Fatalf("%s row: no #%s", state, full)
		}
		if _, ok := sqAttr(pop, "popover"); !ok {
			t.Errorf("%s row: #%s is not a popover", state, full)
		}
		if got := sqText(sqFind(pop, func(n *html.Node) bool { _, ok := sqAttr(n, "data-queue-full-text"); return ok })); got != body {
			t.Errorf("%s row: the popover's text is %q; want the whole message", state, got)
		}
		if closer := sqFind(pop, func(n *html.Node) bool {
			a, _ := sqAttr(n, "popovertargetaction")
			v, _ := sqAttr(n, "popovertarget")
			return n.Data == "button" && a == "hide" && v == full
		}); closer == nil {
			t.Errorf("%s row: the popover has no Close", state)
		}
		open := sqFind(doc, func(n *html.Node) bool {
			v, _ := sqAttr(n, "popovertarget")
			_, hide := sqAttr(n, "popovertargetaction")
			return n.Data == "button" && v == full && !hide
		})
		if open == nil {
			t.Fatalf("%s row: no button opens #%s", state, full)
		}
		label, _ := sqAttr(open, "aria-label")
		cls, _ := sqAttr(open, "class")
		typ, _ := sqAttr(open, "type")
		if !strings.HasPrefix(label, SendQueueFullLabel) || !strings.Contains(cls, "min-h-[44px]") || typ != "button" {
			t.Errorf("%s row: the opener is %q class %q type %q", state, label, cls, typ)
		}
		if strings.Contains(sqText(open), tail) || !strings.Contains(sqText(open), preview) {
			t.Errorf("%s row: the opener shows %q; want the preview, not the tail", state, sqText(open))
		}
	}
	// No whole text: the preview alone.
	var b bytes.Buffer
	if err := SendQueueList(SendQueueListView{Items: []SendQueueItem{{ID: 7, State: "failed", Preview: "short"}}}).Render(&b); err != nil {
		t.Fatal(err)
	}
	if p := b.String(); strings.Contains(p, "popovertarget") || strings.Contains(p, SendQueueFullID(7)) || !strings.Contains(p, "short") {
		t.Fatalf("a row with no whole text: %s", p)
	}
}

// TestThePollDoesNotRedrawUnderAnOpenWholeMessage: the list's script skips a
// redraw while ANY popover in the list is open — a row's whole message
// included, not only the ⋯ menus and the waiting line.
func TestThePollDoesNotRedrawUnderAnOpenWholeMessage(t *testing.T) {
	var b bytes.Buffer
	if err := sendQueueScript().Render(&b); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "var ms = box.querySelectorAll('[popover]');") {
		t.Fatalf("menuOpen does not count every popover of the list:\n%s", b.String())
	}
}
