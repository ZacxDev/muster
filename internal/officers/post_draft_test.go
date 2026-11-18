package officers

import "testing"

// TestPostDraftIsNeverAnHQOfficersTool (security guard): officer_post_draft
// is offered to a builtin officer only — never to an HQ officer, whatever HQ
// advertises (HQ advertises every capability) — and is classified, so the
// listing, the allow-list and the dispatch all ask the same rule. The type
// grant is internal/api's (TestPostDraftIsABuiltinGrantedOfficersOnly).
func TestPostDraftIsNeverAnHQOfficersTool(t *testing.T) {
	hq := Officer{ID: "marketing:example"}
	builtin := Officer{ID: "marketing:example", Backend: BackendBuiltin}
	if ToolOffered(hq, ToolPostDraft) || ToolServed(hq, hqCapabilities, ToolPostDraft) {
		t.Fatal("an HQ officer is offered officer_post_draft")
	}
	if ToolServed(hq, DeclaredCapabilities(hq), ToolPostDraft) {
		t.Fatal("an HQ officer is served officer_post_draft through its declared capabilities")
	}
	if !ToolOffered(builtin, ToolPostDraft) || !ToolServed(builtin, BuiltinCapabilities, ToolPostDraft) {
		t.Fatal("control: a builtin officer is not offered officer_post_draft")
	}
	if _, known := ToolCapability(ToolPostDraft); !known {
		t.Fatal("officer_post_draft is not classified (ToolCapability)")
	}
	for _, c := range AllCapabilities() {
		if c == "outbound_drafts" {
			t.Fatal("outbound_drafts became a backend capability: HQ, which advertises every one, would serve it")
		}
	}
}
