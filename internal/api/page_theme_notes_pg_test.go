package api

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestPageCreateAndUpdateAnswerTheThemeCheck: both page writers answer the
// version they published with theme_notes (pages.ThemeNotes) when an html
// block paints a literal light background or dark text — naming the block,
// the declaration and the token — and still publish it; a version styled
// with the tokens carries no theme_notes at all.
func TestPageCreateAndUpdateAnswerTheThemeCheck(t *testing.T) {
	f := newPageFixture(t)
	notes := func(out map[string]any) []string {
		var got []string
		raw, _ := json.Marshal(out["theme_notes"])
		_ = json.Unmarshal(raw, &got)
		return got
	}
	created := f.tool(t, f.agent, pageCreateTool, map[string]any{"title": "Theme check",
		"blocks": []map[string]any{{"type": "markdown", "text": "# Results"},
			{"type": "html", "html": `<style>body{background:#fff}</style><p style="color:#111">hi</p>`}}})
	id := pnum(created["page_id"])
	if id <= 0 || pnum(created["version"]) != 1 {
		t.Fatalf("page_create: %v", created)
	}
	want := []string{
		`blocks[1] (html): "background:#fff" in body paints a light background, but the page is drawn in the server's dark theme. ` +
			"Use var(--bg) for the page, var(--s1) for a card or panel, var(--line) for a stripe or divider, var(--accent) for emphasis.",
		`blocks[1] (html): "color:#111" in a <p> style attribute is dark text on the page's dark background. ` +
			"Use var(--fg) for text, var(--fg2) for secondary text.",
	}
	if got := notes(created); !reflect.DeepEqual(got, want) {
		t.Errorf("page_create theme_notes:\n got %q\nwant %q", got, want)
	}

	updated := f.tool(t, f.agent, pageUpdateTool, map[string]any{"page_id": id,
		"blocks": []map[string]any{{"type": "html", "html": `<div style="background:whitesmoke">x</div>`}}})
	if pnum(updated["version"]) != 2 {
		t.Fatalf("page_update: %v", updated)
	}
	if got := notes(updated); len(got) != 1 || got[0][:len(`blocks[0] (html): "background:whitesmoke"`)] != `blocks[0] (html): "background:whitesmoke"` {
		t.Errorf("page_update theme_notes: %q", got)
	}

	clean := f.tool(t, f.agent, pageUpdateTool, map[string]any{"page_id": id,
		"blocks": []map[string]any{{"type": "html", "html": `<style>body{background:var(--bg);color:var(--fg)}</style>`}}})
	if pnum(clean["version"]) != 3 {
		t.Fatalf("page_update: %v", clean)
	}
	if _, ok := clean["theme_notes"]; ok {
		t.Errorf("a version styled with the tokens has theme_notes: %v", clean["theme_notes"])
	}
}
