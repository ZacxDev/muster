package ui

import "testing"

// TestMarkdownHTMLDrawsDocumentHeadings: a shareable page's markdown (the
// document renderer) draws an ATX heading as a real heading, escaped; the chat
// renderer keeps its styled div.
func TestMarkdownHTMLDrawsDocumentHeadings(t *testing.T) {
	for src, want := range map[string]string{
		"# Launch <b>":   `<h1>Launch &lt;b&gt;</h1>`,
		"### **Bold** x": `<h3><strong>Bold</strong> x</h3>`,
	} {
		if got := MarkdownHTML(src); got != want {
			t.Errorf("MarkdownHTML(%q) = %q, want %q", src, got, want)
		}
	}
	if got, want := markdownHTML("# Launch"), `<div class="mt-1 font-semibold text-fg text-base">Launch</div>`; got != want {
		t.Errorf("markdownHTML(# Launch) = %q, want the chat's %q", got, want)
	}
}
