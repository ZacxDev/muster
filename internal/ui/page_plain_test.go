package ui

import (
	"strings"
	"testing"
	"time"
)

// TestThePageLinkSheetIsDrawnForTheScriptToFill: with no link yet, the page's
// link ⓘ is drawn hidden WITH its code row and Copy (empty), so the page
// script can fill and show it on the first Share and hide it on Revoke; with
// a link, it is shown holding it. Copy link is drawn hidden either way (the
// script shows it only where a share sheet exists).
func TestThePageLinkSheetIsDrawnForTheScriptToFill(t *testing.T) {
	var b strings.Builder
	_ = PageLinkSheet(7, "").Render(&b)
	empty := b.String()
	if !strings.HasPrefix(empty, `<span data-page-link-sheet="" data-page-id="7" hidden="">`) ||
		!strings.Contains(empty, `data-info-code=""></code>`) || !strings.Contains(empty, `data-copy-text=""`) {
		t.Errorf("no link yet: %s", empty)
	}
	b.Reset()
	_ = PageLinkSheet(7, "https://muster.example.test/s/abc").Render(&b)
	full := b.String()
	if strings.Contains(full[:strings.Index(full, ">")], "hidden") || !strings.Contains(full, `data-info-code="">https://muster.example.test/s/abc</code>`) {
		t.Errorf("with a link: %s", full)
	}
	b.Reset()
	_ = PageCopyButton(7, "T", "", true).Render(&b)
	if !strings.Contains(b.String(), `data-page-copy="" hidden=""`) {
		t.Errorf("Copy link is not drawn hidden: %s", b.String())
	}
	for _, js := range []string{"s.hidden = !url;", "if (!navigator.share) return;"} {
		if !strings.Contains(pageShareJS, js) {
			t.Errorf("pageShareJS lacks %q", js)
		}
	}
}

// TestPlainWhenSaysTheDayAndNoClock: the server's words name the day only
// (the script adds the reader's own time); never a clock or "UTC".
func TestPlainWhenSaysTheDayAndNoClock(t *testing.T) {
	now := time.Date(2030, 3, 9, 1, 30, 0, 0, time.UTC)
	for _, c := range []struct {
		t    time.Time
		want string
	}{
		{now.Add(-time.Hour), "today"},
		{now.Add(-24 * time.Hour), "yesterday"},
		{time.Date(2030, 1, 4, 23, 0, 0, 0, time.UTC), "on Jan 4"},
		{time.Date(2029, 12, 31, 8, 0, 0, 0, time.UTC), "on Dec 31, 2029"},
	} {
		if got := PlainWhen(c.t, now); got != c.want {
			t.Errorf("PlainWhen(%v) = %q, want %q", c.t, got, c.want)
		}
	}
	if h := PlainWhenHTML("Sent ", now, now); !strings.HasSuffix(h, `>Sent today</time>`) || strings.Contains(h, "UTC<") {
		t.Errorf("PlainWhenHTML: %s", h)
	}
}
