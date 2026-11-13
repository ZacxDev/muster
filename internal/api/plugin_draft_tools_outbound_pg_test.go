package api

import (
	"strconv"
	"strings"
	"testing"

	"github.com/ZacxDev/muster/internal/dbtest"
	"github.com/ZacxDev/muster/internal/plugins"
)

// TestPluginDraftDecideBindsAnOutboundDraftsMedia (security guard, the MCP
// door): plugin_drafts_list shows an outbound draft's venue, target and media
// (ref, sha256, content_type) with the media_sha256 an approval must name;
// plugin_draft_decide refuses an approval without media_sha256 or with one
// that is not the uploaded media in order, and approves with the exact list.
func TestPluginDraftDecideBindsAnOutboundDraftsMedia(t *testing.T) {
	f := newDraftToolFixture(t)
	dsn := dbtest.DSN(t)
	// The fixture's plugin bound (behind the doors: this fixture wires no
	// officer store) to an officer, with draft_checks.
	dbtest.OwnerExec(t, dsn, `UPDATE plugins SET deliver_to = 'officer:example:main', capabilities = '{messages,draft_checks}' WHERE id = `+
		strconv.FormatInt(f.plugin.ID, 10))
	const body = "A new post with two media."
	d, err := f.plugins.FileOutbound(f.asOwner, f.chief.ID, f.owner, f.chief.Name, plugins.NewOutbound{Officer: "example:main",
		Plugin: f.plugin.Name, Venue: "example.forum", Target: plugins.OutboundTarget{Kind: plugins.TargetPost}, Body: body,
		Media: []string{"launch/hero-1", "launch/demo-2"}})
	if err != nil {
		t.Fatalf("file an outbound draft: %v", err)
	}
	asPlugin := f.asOwner
	for i, m := range []struct{ ref, ct string }{{"launch/hero-1", "image/webp"}, {"launch/demo-2", "video/mp4"}} {
		if _, _, err := f.plugins.UploadDraftMedia(asPlugin, f.plugin.ID, d.ID, plugins.DraftMedia{Ref: m.ref, SHA256: sha(m.ref),
			ContentType: m.ct, Size: int64(10 + i), Key: plugins.MediaKeyPrefix + "x/" + strconv.Itoa(i)}); err != nil {
			t.Fatalf("upload %s: %v", m.ref, err)
		}
	}
	if _, _, err := f.plugins.CheckDraft(asPlugin, f.plugin.ID, d.ID, plugins.DraftCheck{Sendable: true}); err != nil {
		t.Fatalf("check: %v", err)
	}

	got := listedByID(f.listed(t, f.tok, otherRouteSID), d.ID)
	if got == nil || got["kind"] != "outbound" || got["venue"] != "example.forum" || got["message_id"] != float64(0) {
		t.Fatalf("the listed outbound draft: %v", got)
	}
	if tg, _ := got["target"].(map[string]any); tg["kind"] != "post" {
		t.Fatalf("the listed target: %v", got["target"])
	}
	media, _ := got["media"].([]any)
	sums, _ := got["media_sha256"].([]any)
	if len(media) != 2 || len(sums) != 2 || sums[0] != sha("launch/hero-1") || sums[1] != sha("launch/demo-2") {
		t.Fatalf("the listed media: %v / %v", media, sums)
	}
	if m0, _ := media[0].(map[string]any); m0["ref"] != "launch/hero-1" || m0["sha256"] != sha("launch/hero-1") ||
		m0["content_type"] != "image/webp" || m0["uploaded"] != true {
		t.Fatalf("the first listed medium: %v", media[0])
	}

	decide := func(args map[string]any) (string, bool) {
		args["id"], args["decision"], args["body_sha256"] = d.ID, "approve", sha(body)
		return f.mcp(t, f.tok, otherRouteSID, "plugin_draft_decide", args)
	}
	if text, isErr := decide(map[string]any{}); !isErr || !strings.Contains(text, "names the body_sha256 and media_sha256") {
		t.Fatalf("approving without media_sha256: %s; want refused naming it", text)
	}
	if text, isErr := decide(map[string]any{"media_sha256": []string{sha("launch/demo-2"), sha("launch/hero-1")}}); !isErr ||
		!strings.Contains(text, "media_sha256 is not") {
		t.Fatalf("approving with the media out of order: %s; want refused", text)
	}
	if r := f.row(t, d.ID); r.status != "pending" {
		t.Fatalf("after the refusals the draft is %+v; want pending", r)
	}
	if text, isErr := decide(map[string]any{"media_sha256": []string{sha("launch/hero-1"), sha("launch/demo-2")}}); isErr ||
		!strings.Contains(text, `"status":"approved"`) {
		t.Fatalf("approving with the exact media: %s", text)
	}
	// A draft with NO media: an approval must still say so (media_sha256: []),
	// so an approval that never looked at the media is never taken for one
	// that did (nil and [] are otherwise equal).
	bare, err := f.plugins.FileOutbound(f.asOwner, f.chief.ID, f.owner, f.chief.Name, plugins.NewOutbound{Officer: "example:main",
		Plugin: f.plugin.Name, Venue: "example.forum", Target: plugins.OutboundTarget{Kind: plugins.TargetPost}, Body: "Text only."})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.plugins.CheckDraft(asPlugin, f.plugin.ID, bare.ID, plugins.DraftCheck{Sendable: true}); err != nil {
		t.Fatal(err)
	}
	if text, isErr := f.mcp(t, f.tok, otherRouteSID, "plugin_draft_decide", map[string]any{"id": bare.ID, "decision": "approve",
		"body_sha256": sha("Text only.")}); !isErr || !strings.Contains(text, "names the body_sha256 and media_sha256") {
		t.Fatalf("approving a media-less outbound draft without media_sha256: %s; want refused", text)
	}
	if text, isErr := f.mcp(t, f.tok, otherRouteSID, "plugin_draft_decide", map[string]any{"id": bare.ID, "decision": "approve",
		"body_sha256": sha("Text only."), "media_sha256": []string{}}); isErr || !strings.Contains(text, `"status":"approved"`) {
		t.Fatalf("control: approving it with media_sha256 []: %s", text)
	}
	stored, err := f.plugins.Draft(f.asOwner, d.ID)
	if err != nil || stored.ApprovedMediaSHA256 == nil || strings.Join(stored.ApprovedMediaSHA256, ",") != sha("launch/hero-1")+","+sha("launch/demo-2") {
		t.Fatalf("the approval's binding: %v (%v)", stored.ApprovedMediaSHA256, err)
	}
}
