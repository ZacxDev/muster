package main

import (
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/api"
	"github.com/ZacxDev/muster/internal/pages"
)

// TestThePagesBannerSaysWhetherLinksAreReusable: the `shareable pages: ON`
// line says whether each page keeps one reusable link (a link key) or every
// link is hash-only — and that agents ask unless their owner opted out, on
// Account (a per-owner setting, so nothing to name here but where it is).
func TestThePagesBannerSaysWhetherLinksAreReusable(t *testing.T) {
	line := func(out string) string {
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "shareable pages: ON") {
				return l
			}
		}
		t.Fatalf("no `shareable pages: ON` line:\n%s", out)
		return ""
	}
	plain := line(renderBanner(t, config{Pages: true}, api.Extensions{Pages: pages.NewPG(nil)}))
	if !strings.Contains(plain, "hash-only") || strings.Contains(plain, "one reusable link") || !strings.Contains(plain, "per-owner Account setting") {
		t.Fatalf("without a key: %s", plain)
	}
	keyed, err := pages.NewPG(nil).WithLinkKey(make([]byte, pages.LinkKeySize))
	if err != nil {
		t.Fatal(err)
	}
	if l := line(renderBanner(t, config{Pages: true}, api.Extensions{Pages: keyed})); !strings.Contains(l, "one reusable link") || strings.Contains(l, "hash-only") {
		t.Fatalf("with a key: %s", l)
	}
}
