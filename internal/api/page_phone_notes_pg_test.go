package api

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestPageCreateAndUpdateAnswerThePhoneCheck: both page writers answer the
// version they published with phone_notes (ui.PagePhoneNotes), naming the
// block, and still publish it; a version with nothing to say carries no
// phone_notes at all.
func TestPageCreateAndUpdateAnswerThePhoneCheck(t *testing.T) {
	f := newPageFixture(t)
	wide := "| a | b | c | d | e | f |\n|---|---|---|---|---|---|\n| 1 | 2 | 3 | 4 | 5 | 6 |"
	notes := func(out map[string]any) []string {
		var got []string
		raw, _ := json.Marshal(out["phone_notes"])
		_ = json.Unmarshal(raw, &got)
		return got
	}
	created := f.tool(t, f.agent, pageCreateTool, map[string]any{"title": "Phone check",
		"blocks": []map[string]any{{"type": "markdown", "text": "# Results"}, {"type": "markdown", "text": wide}}})
	id := pnum(created["page_id"])
	if id <= 0 || pnum(created["version"]) != 1 {
		t.Fatalf("page_create: %v", created)
	}
	if got := notes(created); len(got) != 1 || !strings.HasPrefix(got[0], "blocks[1] (markdown): a table with 6 columns is cramped on a phone") {
		t.Errorf("page_create phone_notes: %q", got)
	}

	updated := f.tool(t, f.agent, pageUpdateTool, map[string]any{"page_id": id,
		"blocks": []map[string]any{{"type": "markdown", "text": "![shot](https://example.com/a.png)"}}})
	if pnum(updated["version"]) != 2 {
		t.Fatalf("page_update: %v", updated)
	}
	want := []string{"blocks[0] (markdown): the image `![shot](https://example.com/a.png)` is shown as text, not drawn. " +
		"In markdown write ![what it shows](asset:<id>) (page_asset_upload) or a small data:image/png, jpeg, webp or gif base64 URI; " +
		"an http(s) address or an SVG is never drawn."}
	if got := notes(updated); !reflect.DeepEqual(got, want) {
		t.Errorf("page_update phone_notes:\n got %q\nwant %q", got, want)
	}

	clean := f.tool(t, f.agent, pageUpdateTool, map[string]any{"page_id": id,
		"blocks": []map[string]any{{"type": "markdown", "text": "![dot](data:image/png;base64,AAAA)"}}})
	if pnum(clean["version"]) != 3 {
		t.Fatalf("page_update: %v", clean)
	}
	if _, ok := clean["phone_notes"]; ok {
		t.Errorf("a version with nothing to say has phone_notes: %v", clean["phone_notes"])
	}
}
