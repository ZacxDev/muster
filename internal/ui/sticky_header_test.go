package ui

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEveryStickyHeaderIsOneOpaqueClass: the shell's sticky header is OPAQUE
// (bg-bg, no backdrop blur): at bg-bg/60–80 the page scrolled under it
// ghosted through (the Asks page's Log tab, scrolled, at 390×844). And EVERY
// sticky top bar in the views — the shell's, an agent's, a task's — is that one
// class: a ledger of the files that draw one, failing if a bar is drawn
// anywhere else (a sixth copy) or one stops using it.
// RED at origin/main fcb27a6b: five hand-written copies, each translucent.
func TestEveryStickyHeaderIsOneOpaqueClass(t *testing.T) {
	var b strings.Builder
	if err := header(false, Brand{}).Render(&b); err != nil {
		t.Fatal(err)
	}
	cls := regexp.MustCompile(`^<header class="([^"]*)"`).FindStringSubmatch(b.String())
	if cls == nil {
		t.Fatalf("the shell header: %s", b.String())
	}
	tokens := strings.Fields(cls[1])
	opaque := false
	for _, tok := range tokens {
		if tok == "bg-bg" {
			opaque = true
		}
		if strings.HasPrefix(tok, "bg-bg/") || strings.Contains(tok, "backdrop") || strings.Contains(tok, ":bg-") {
			t.Errorf("the shell header is see-through: %q in %q", tok, cls[1])
		}
	}
	if !opaque {
		t.Errorf("the shell header has no solid background: %q", cls[1])
	}

	// The ledger: who draws a sticky top-0 z-20 bar, and with what.
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	literal := regexp.MustCompile(`"sticky top-0 z-20[^"]*"`)
	uses := map[string]int{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range literal.FindAllString(string(src), -1) {
			uses[f+": "+m]++
		}
		if n := strings.Count(string(src), "Class(StickyHeaderClass"); n > 0 {
			uses[f+": StickyHeaderClass"] += n
		}
	}
	want := map[string]int{
		`components.go: "sticky top-0 z-20 border-b border-line bg-bg"`: 1, // the constant itself
		"components.go: StickyHeaderClass":                              1,
		"agent_header_folded.go: StickyHeaderClass":                     1,
		"agents_detail.go: StickyHeaderClass":                           1,
		"task_detail.go: StickyHeaderClass":                             1,
		"task_not_found.go: StickyHeaderClass":                          1,
	}
	for k, n := range uses {
		if want[k] != n {
			t.Errorf("sticky bar %s ×%d, want ×%d", k, n, want[k])
		}
	}
	for k, n := range want {
		if uses[k] != n {
			t.Errorf("sticky bar %s ×%d, want ×%d", k, uses[k], n)
		}
	}
}
